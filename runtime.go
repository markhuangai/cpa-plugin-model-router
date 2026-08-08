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
	cursor    int
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
	if runtime == nil || len(route.Models) == 0 {
		return routeSelection{}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := runtime.stateLocked(route)
	if state == nil {
		return routeSelection{}
	}
	now := runtime.now()
	start := 0
	if route.Strategy == routeStrategyRoundRobin && state.cursor >= 0 && state.cursor < len(route.Models) {
		start = state.cursor
	}
	var earliest time.Time
	for offset := 0; offset < len(route.Models); offset++ {
		index := offset
		if route.Strategy == routeStrategyRoundRobin {
			index = (start + offset) % len(route.Models)
		}
		model := strings.TrimSpace(route.Models[index])
		until := state.cooldowns[routeKey(model)]
		if until.IsZero() || !now.Before(until) {
			delete(state.cooldowns, routeKey(model))
			if route.Strategy == routeStrategyRoundRobin {
				state.cursor = (index + 1) % len(route.Models)
			}
			return routeSelection{model: model}
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
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
