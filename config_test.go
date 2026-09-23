package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDecodeRouterConfigCanonicalAndLegacyKeys(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "canonical",
			raw: `routes:
  - alias: smart
    strategy: round-robin
    cooldown_seconds: 15
    targets:
      - model: provider-a
        weight: 3
      - model: provider-b
`,
		},
		{
			name: "legacy",
			raw: `model-routes:
  - alias: smart
    strategy: round-robin
    cooldown-seconds: 15
    models: [provider-a, provider-b]
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := decodeRouterConfig([]byte(test.raw))
			if err != nil {
				t.Fatalf("decodeRouterConfig() error = %v", err)
			}
			if !config.Enabled || len(config.Routes) != 1 {
				t.Fatalf("decodeRouterConfig() = %#v", config)
			}
			route := config.Routes[0]
			if route.Alias != "smart" || route.Strategy != routeStrategyRoundRobin || route.CooldownSeconds != 15 || len(route.Targets) != 2 {
				t.Fatalf("route = %#v", route)
			}
			wantTargets := []modelTarget{{Model: "provider-a", Weight: 1}, {Model: "provider-b", Weight: 1}}
			if test.name == "canonical" {
				wantTargets[0].Weight = 3
			}
			if !slices.Equal(route.Targets, wantTargets) {
				t.Fatalf("targets = %#v", route.Targets)
			}
		})
	}
}

func TestDecodeRouterConfigDefaults(t *testing.T) {
	previousResolver := defaultDataPathResolver
	wantPath := filepath.Join(t.TempDir(), "default.db")
	defaultDataPathResolver = func() string { return wantPath }
	t.Cleanup(func() { defaultDataPathResolver = previousResolver })
	config, err := decodeRouterConfig([]byte(`routes:
  - alias: smart
    models: [provider-a]
`))
	if err != nil {
		t.Fatalf("decodeRouterConfig() error = %v", err)
	}
	route := config.Routes[0]
	if route.Strategy != routeStrategyPriority || route.CooldownSeconds != defaultCooldownSeconds {
		t.Fatalf("route defaults = %#v", route)
	}
	if config.DataPath != wantPath || config.RetentionDays != defaultRetentionDays {
		t.Fatalf("storage defaults = %#v", config)
	}
}

func TestDecodeRouterConfigStorageOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "usage.db")
	config, err := decodeRouterConfig([]byte("data_path: " + filepath.ToSlash(path) + "\nretention_days: 45\nroutes: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.DataPath != path || config.RetentionDays != 45 {
		t.Fatalf("storage config = %#v", config)
	}
	for _, days := range []int{0, maxRetentionDays + 1} {
		if _, err := decodeRouterConfig([]byte("retention_days: " + itoa(days) + "\nroutes: []\n")); err == nil || !strings.Contains(err.Error(), "retention_days") {
			t.Fatalf("retention_days=%d error = %v", days, err)
		}
	}
}

func TestDecodeRouterConfigDisabledClearsRoutes(t *testing.T) {
	config, err := decodeRouterConfig([]byte(`enabled: false
routes:
  - alias: smart
    models: [provider-a]
`))
	if err != nil {
		t.Fatalf("decodeRouterConfig() error = %v", err)
	}
	if config.Enabled || config.Routes != nil {
		t.Fatalf("disabled config = %#v", config)
	}
}

func TestDecodeRouterConfigRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		message string
	}{
		{
			name: "both route keys",
			raw: `routes: []
model-routes: []
`,
			message: "both routes and model-routes",
		},
		{
			name: "both cooldown keys",
			raw: `routes:
  - alias: smart
    cooldown_seconds: 10
    cooldown-seconds: 20
    models: [provider-a]
`,
			message: "both cooldown_seconds and cooldown-seconds",
		},
		{
			name: "unknown field",
			raw: `routes: []
surprise: true
`,
			message: "field surprise not found",
		},
		{
			name: "duplicate alias",
			raw: `routes:
  - alias: Smart
    models: [provider-a]
  - alias: smart
    models: [provider-b]
`,
			message: "duplicate alias",
		},
		{
			name: "alias suffix",
			raw: `routes:
  - alias: smart(high)
    models: [provider-a]
`,
			message: "alias must not include a thinking suffix",
		},
		{
			name: "nested alias",
			raw: `routes:
  - alias: smart
    models: [backup]
  - alias: backup
    models: [provider-b]
`,
			message: "target must not reference route alias",
		},
		{
			name: "negative cooldown",
			raw: `routes:
  - alias: smart
    cooldown_seconds: -1
    models: [provider-a]
`,
			message: "cooldown_seconds must be >= 0",
		},
		{
			name: "both model schemas",
			raw: `routes:
  - alias: smart
    models: [provider-a]
    targets:
      - model: provider-b
`,
			message: "both models and targets",
		},
		{
			name: "zero weight",
			raw: `routes:
  - alias: smart
    targets:
      - model: provider-a
        weight: 0
`,
			message: "weight must be between",
		},
		{
			name: "weight too large",
			raw: `routes:
  - alias: smart
    targets:
      - model: provider-a
        weight: 1000001
`,
			message: "weight must be between",
		},
		{
			name: "empty canonical model",
			raw: `routes:
  - alias: smart
    targets:
      - model: ""
`,
			message: "model is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeRouterConfig([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("decodeRouterConfig() error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestRouterConfigFallbackPolicyDefaultsWhenEmpty(t *testing.T) {
	policy := routerConfig{}.fallbackPolicy()
	for _, status := range defaultFallbackOnStatus {
		if !policy.shouldFallback(status) {
			t.Fatalf("default policy must fall back on %d", status)
		}
	}
}

func TestDecodeRouterConfigFallback(t *testing.T) {
	raw := []byte(`
fallback:
  fallback_on_status: [400, 429]
  no_fallback_on_status: []
  stream_fallback_before_first_chunk_only: true
routes:
  - alias: demo
    targets:
      - model: target-a
`)
	cfg, err := decodeRouterConfig(raw)
	if err != nil {
		t.Fatalf("decodeRouterConfig() error = %v", err)
	}
	policy := cfg.fallbackPolicy()
	if !policy.shouldFallback(400) || !policy.shouldFallback(429) {
		t.Fatal("configured statuses must fall back")
	}
	if policy.shouldFallback(404) {
		t.Fatal("404 is not in the configured list and must not fall back")
	}
	if !policy.shouldFallback(503) {
		t.Fatal("503 must still fall back through the 5xx rule")
	}
}

func TestDecodeRouterConfigRejectsInvalidFallback(t *testing.T) {
	cases := map[string]string{
		"overlap": `
fallback:
  fallback_on_status: [429]
  no_fallback_on_status: [429]
routes:
  - alias: demo
    targets:
      - model: target-a
`,
		"out of range": `
fallback:
  fallback_on_status: [99]
routes:
  - alias: demo
    targets:
      - model: target-a
`,
		"stream splice": `
fallback:
  stream_fallback_before_first_chunk_only: false
routes:
  - alias: demo
    targets:
      - model: target-a
`,
		"unknown key": `
fallback:
  fallback_on_500: true
routes:
  - alias: demo
    targets:
      - model: target-a
`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRouterConfig([]byte(raw)); err == nil {
				t.Fatalf("decodeRouterConfig() accepted invalid fallback config for %q", name)
			}
		})
	}
}

func TestDecodeRouterConfigExclusionOverridesDefaults(t *testing.T) {
	raw := []byte(`
fallback:
  no_fallback_on_status: [400, 429, 503]
routes:
  - alias: demo
    targets:
      - model: target-a
`)
	cfg, err := decodeRouterConfig(raw)
	if err != nil {
		t.Fatalf("decodeRouterConfig() error = %v", err)
	}
	policy := cfg.fallbackPolicy()
	for _, status := range []int{400, 429, 503} {
		if policy.shouldFallback(status) {
			t.Fatalf("excluded status %d must not fall back", status)
		}
	}
	for _, status := range []int{402, 404, 504} {
		if !policy.shouldFallback(status) {
			t.Fatalf("status %d is not excluded and must keep the default", status)
		}
	}
}

func TestDecodeRouterConfigFallbackListFormsAgree(t *testing.T) {
	routes := `
routes:
  - alias: demo
    targets:
      - model: target-a
`
	cases := map[string]string{
		"omitted":        "fallback:\n  no_fallback_on_status: [400, 503]\n" + routes,
		"empty explicit": "fallback:\n  fallback_on_status: []\n  no_fallback_on_status: [400, 503]\n" + routes,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := decodeRouterConfig([]byte(raw))
			if err != nil {
				t.Fatalf("decodeRouterConfig() error = %v", err)
			}
			policy := cfg.fallbackPolicy()
			if policy.shouldFallback(400) || policy.shouldFallback(503) {
				t.Fatal("an exclusion must remove a status from the inherited default set")
			}
			if !policy.shouldFallback(404) || !policy.shouldFallback(500) {
				t.Fatal("an omitted or empty fallback_on_status must keep the default set")
			}
		})
	}
}
