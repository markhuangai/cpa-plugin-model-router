package main

import (
	"testing"
	"time"
)

func TestRouteRuntimePriorityCooldown(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	runtime := newRouteRuntime(func() time.Time { return now })
	route := testModelRoute("smart", routeStrategyPriority, 30, "provider-a", "provider-b")
	runtime.Sync([]modelRoute{route})

	if selected := runtime.Select(route); selected.model != "provider-a" {
		t.Fatalf("first selection = %#v", selected)
	}
	runtime.MarkFailure(route, "provider-a")
	if selected := runtime.Select(route); selected.model != "provider-b" {
		t.Fatalf("second selection = %#v", selected)
	}
	runtime.MarkFailure(route, "provider-b")
	if selected := runtime.Select(route); !selected.allCooling || selected.retryAfter != 30*time.Second {
		t.Fatalf("cooldown selection = %#v", selected)
	}

	now = now.Add(31 * time.Second)
	if selected := runtime.Select(route); selected.model != "provider-a" {
		t.Fatalf("selection after cooldown = %#v", selected)
	}
}

func TestRouteRuntimeRoundRobin(t *testing.T) {
	runtime := newRouteRuntime(nil)
	route := testModelRoute("smart", routeStrategyRoundRobin, 30, "a", "b")
	runtime.Sync([]modelRoute{route})
	want := []string{"a", "b", "a", "b"}
	for index, model := range want {
		if selected := runtime.Select(route); selected.model != model {
			t.Fatalf("selection %d = %#v, want %q", index, selected, model)
		}
	}
}

func TestRouteRuntimeWeightedRoundRobinUsesConsecutiveVirtualSlots(t *testing.T) {
	runtime := newRouteRuntime(nil)
	route := modelRoute{
		Alias:           "weighted",
		Strategy:        routeStrategyRoundRobin,
		CooldownSeconds: 30,
		Targets: []modelTarget{
			{Model: "mimo", Weight: 3},
			{Model: "deepseek", Weight: 1},
		},
	}
	runtime.Sync([]modelRoute{route})
	want := []string{"mimo", "mimo", "mimo", "deepseek", "mimo", "mimo", "mimo", "deepseek"}
	for index, model := range want {
		if selected := runtime.Select(route); selected.model != model {
			t.Fatalf("selection %d = %#v, want %q", index, selected, model)
		}
	}
}

func TestRouteRuntimeWeightedCooldownSkipsRemainingSlots(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	runtime := newRouteRuntime(func() time.Time { return now })
	route := modelRoute{
		Alias:           "weighted",
		Strategy:        routeStrategyRoundRobin,
		CooldownSeconds: 30,
		Targets: []modelTarget{
			{Model: "mimo", Weight: 3},
			{Model: "deepseek", Weight: 1},
		},
	}
	runtime.Sync([]modelRoute{route})
	if selected := runtime.Select(route); selected.model != "mimo" {
		t.Fatalf("initial selection = %#v", selected)
	}
	runtime.MarkFailure(route, "mimo")
	if selected := runtime.Select(route); selected.model != "deepseek" {
		t.Fatalf("selection after failed weighted target = %#v", selected)
	}
	runtime.MarkFailure(route, "deepseek")
	if selected := runtime.Select(route); !selected.allCooling || selected.retryAfter != 30*time.Second {
		t.Fatalf("all-cooling selection = %#v", selected)
	}
}

func TestRouteRuntimeExclusionsPreventRetryAfterCooldownExpires(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	runtime := newRouteRuntime(func() time.Time { return now })
	route := testModelRoute("weighted", routeStrategyRoundRobin, 1, "a", "b")
	runtime.Sync([]modelRoute{route})
	if selected := runtime.Select(route); selected.model != "a" {
		t.Fatalf("initial selection = %#v", selected)
	}
	runtime.MarkFailure(route, "a")
	now = now.Add(2 * time.Second)
	if selected := runtime.SelectExcluding(route, map[string]struct{}{"a": {}}); selected.model != "b" {
		t.Fatalf("selection after excluded cooldown expiry = %#v", selected)
	}
	if selected := runtime.Select(route); selected.model != "a" {
		t.Fatalf("unexcluded selection after cooldown expiry = %#v, want a", selected)
	}
}

func TestRouteRuntimePriorityIgnoresWeights(t *testing.T) {
	runtime := newRouteRuntime(nil)
	route := modelRoute{
		Alias:           "priority",
		Strategy:        routeStrategyPriority,
		CooldownSeconds: 30,
		Targets: []modelTarget{
			{Model: "first", Weight: 100},
			{Model: "second", Weight: 1},
		},
	}
	runtime.Sync([]modelRoute{route})
	for index := 0; index < 3; index++ {
		if selected := runtime.Select(route); selected.model != "first" {
			t.Fatalf("selection %d = %#v, want first", index, selected)
		}
	}
}

func TestRouteRuntimeCloneResetsCursorWhenWeightChanges(t *testing.T) {
	runtime := newRouteRuntime(nil)
	route := modelRoute{
		Alias:           "weighted",
		Strategy:        routeStrategyRoundRobin,
		CooldownSeconds: 30,
		Targets: []modelTarget{
			{Model: "first", Weight: 1},
			{Model: "second", Weight: 3},
		},
	}
	runtime.Sync([]modelRoute{route})
	if selected := runtime.Select(route); selected.model != "first" {
		t.Fatalf("initial selection = %#v", selected)
	}
	unchanged := runtime.Clone([]modelRoute{route})
	if selected := unchanged.Select(route); selected.model != "second" {
		t.Fatalf("unchanged clone selection = %#v", selected)
	}
	changed := route
	changed.Targets = []modelTarget{{Model: "first", Weight: 2}, {Model: "second", Weight: 3}}
	reset := runtime.Clone([]modelRoute{changed})
	if selected := reset.Select(changed); selected.model != "first" {
		t.Fatalf("changed clone selection = %#v, want first", selected)
	}
}

func TestRouteRuntimeClonePreservesOnlyUnchangedRoutes(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	runtime := newRouteRuntime(func() time.Time { return now })
	route := testModelRoute("smart", routeStrategyPriority, 30, "a", "b")
	runtime.Sync([]modelRoute{route})
	runtime.MarkFailure(route, "a")

	unchanged := runtime.Clone([]modelRoute{route})
	if selected := unchanged.Select(route); selected.model != "b" {
		t.Fatalf("unchanged clone selection = %#v", selected)
	}

	changedRoute := route
	changedRoute.Targets = []modelTarget{{Model: "a", Weight: defaultTargetWeight}, {Model: "c", Weight: defaultTargetWeight}}
	changed := runtime.Clone([]modelRoute{changedRoute})
	if selected := changed.Select(changedRoute); selected.model != "a" {
		t.Fatalf("changed clone selection = %#v", selected)
	}
}
