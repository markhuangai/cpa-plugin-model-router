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
