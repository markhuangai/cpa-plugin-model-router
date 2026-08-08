package main

import (
	"testing"
	"time"
)

func TestRouteRuntimePriorityCooldown(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	runtime := newRouteRuntime(func() time.Time { return now })
	route := modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"provider-a", "provider-b"}}
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
	route := modelRoute{Alias: "smart", Strategy: routeStrategyRoundRobin, CooldownSeconds: 30, Models: []string{"a", "b"}}
	runtime.Sync([]modelRoute{route})
	want := []string{"a", "b", "a", "b"}
	for index, model := range want {
		if selected := runtime.Select(route); selected.model != model {
			t.Fatalf("selection %d = %#v, want %q", index, selected, model)
		}
	}
}

func TestRouteRuntimeClonePreservesOnlyUnchangedRoutes(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	runtime := newRouteRuntime(func() time.Time { return now })
	route := modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"a", "b"}}
	runtime.Sync([]modelRoute{route})
	runtime.MarkFailure(route, "a")

	unchanged := runtime.Clone([]modelRoute{route})
	if selected := unchanged.Select(route); selected.model != "b" {
		t.Fatalf("unchanged clone selection = %#v", selected)
	}

	changedRoute := route
	changedRoute.Models = []string{"a", "c"}
	changed := runtime.Clone([]modelRoute{changedRoute})
	if selected := changed.Select(changedRoute); selected.model != "a" {
		t.Fatalf("changed clone selection = %#v", selected)
	}
}
