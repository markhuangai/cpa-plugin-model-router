package main

import (
	"strings"
	"sync"
	"time"
)

type routeRuntime struct {
	mu     sync.Mutex
	now    func() time.Time
	states map[string]*routeState
}

type routeState struct {
	signature string
	cursor    int64
	cooldowns map[string]time.Time
}

type routeSelection struct {
	model      string
	allCooling bool
	retryAfter time.Duration
}

func newRouteRuntime(now func() time.Time) *routeRuntime {
	if now == nil {
		now = time.Now
	}
	return &routeRuntime{now: now, states: make(map[string]*routeState)}
}

func (runtime *routeRuntime) Sync(routes []modelRoute) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	next := make(map[string]*routeState, len(routes))
	for _, route := range routes {
		key := routeKey(route.Alias)
		signature := routeSignature(route)
		state := runtime.states[key]
		if state == nil || state.signature != signature {
			state = &routeState{signature: signature, cooldowns: make(map[string]time.Time)}
		}
		next[key] = state
	}
	runtime.states = next
}

func (runtime *routeRuntime) Clone(routes []modelRoute) *routeRuntime {
	if runtime == nil {
		cloned := newRouteRuntime(nil)
		cloned.Sync(routes)
		return cloned
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	cloned := newRouteRuntime(runtime.now)
	for _, route := range routes {
		key := routeKey(route.Alias)
		signature := routeSignature(route)
		old := runtime.states[key]
		state := &routeState{signature: signature, cooldowns: make(map[string]time.Time)}
		if old != nil && old.signature == signature {
			state.cursor = old.cursor
			for model, until := range old.cooldowns {
				state.cooldowns[model] = until
			}
		}
		cloned.states[key] = state
	}
	return cloned
}

func (runtime *routeRuntime) Select(route modelRoute) routeSelection {
	return runtime.SelectExcluding(route, nil)
}

func (runtime *routeRuntime) SelectExcluding(route modelRoute, excluded map[string]struct{}) routeSelection {
	if runtime == nil || len(route.Targets) == 0 {
		return routeSelection{}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := runtime.stateLocked(route)
	if state == nil {
		return routeSelection{}
	}
	now := runtime.now()
	var earliest time.Time
	if route.Strategy != routeStrategyRoundRobin {
		for _, target := range route.Targets {
			model := strings.TrimSpace(target.Model)
			if _, skip := excluded[routeKey(model)]; skip {
				continue
			}
			until := state.cooldowns[routeKey(model)]
			if until.IsZero() || !now.Before(until) {
				delete(state.cooldowns, routeKey(model))
				return routeSelection{model: model}
			}
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
		}
	} else {
		totalWeight := routeTargetWeightTotal(route.Targets)
		if totalWeight <= 0 {
			return routeSelection{}
		}
		start := state.cursor % totalWeight
		if start < 0 {
			start += totalWeight
		}
		startIndex, startOffset := weightedTargetAt(route.Targets, start)
		slot := start
		for offset := 0; offset < len(route.Targets); offset++ {
			index := (startIndex + offset) % len(route.Targets)
			target := route.Targets[index]
			model := strings.TrimSpace(target.Model)
			if _, skip := excluded[routeKey(model)]; !skip {
				until := state.cooldowns[routeKey(model)]
				if until.IsZero() || !now.Before(until) {
					delete(state.cooldowns, routeKey(model))
					state.cursor = (slot + 1) % totalWeight
					return routeSelection{model: model}
				}
				if earliest.IsZero() || until.Before(earliest) {
					earliest = until
				}
			}
			weight := int64(target.Weight)
			if offset == 0 {
				weight -= startOffset
			}
			if weight > 0 {
				slot = (slot + weight) % totalWeight
			}
		}
	}
	if earliest.IsZero() {
		return routeSelection{}
	}
	return routeSelection{allCooling: true, retryAfter: earliest.Sub(now)}
}

func (runtime *routeRuntime) MarkFailure(route modelRoute, model string) {
	if runtime == nil || strings.TrimSpace(model) == "" {
		return
	}
	cooldown := time.Duration(route.CooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = defaultCooldownSeconds * time.Second
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := runtime.stateLocked(route)
	if state != nil {
		state.cooldowns[routeKey(model)] = runtime.now().Add(cooldown)
	}
}

func (runtime *routeRuntime) stateLocked(route modelRoute) *routeState {
	key := routeKey(route.Alias)
	if key == "" {
		return nil
	}
	if runtime.states == nil {
		runtime.states = make(map[string]*routeState)
	}
	signature := routeSignature(route)
	state := runtime.states[key]
	if state == nil || state.signature != signature {
		state = &routeState{signature: signature, cooldowns: make(map[string]time.Time)}
		runtime.states[key] = state
	}
	return state
}

func routeTargetWeightTotal(targets []modelTarget) int64 {
	var total int64
	for _, target := range targets {
		weight := int64(target.Weight)
		if weight < 1 || total > (1<<63-1)-weight {
			return 0
		}
		total += weight
	}
	return total
}

func weightedTargetAt(targets []modelTarget, slot int64) (int, int64) {
	var offset int64
	for index, target := range targets {
		weight := int64(target.Weight)
		if weight < 1 {
			continue
		}
		if slot < offset+weight {
			return index, slot - offset
		}
		offset += weight
	}
	return 0, 0
}
