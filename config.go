package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	routeStrategyPriority   = "priority"
	routeStrategyRoundRobin = "round-robin"
	defaultCooldownSeconds  = 60
	defaultRetentionDays    = 365
	maxRetentionDays        = 3650
	defaultTargetWeight     = 1
	maxTargetWeight         = 1_000_000
)

type modelTarget struct {
	Model  string
	Weight int
}

type modelRoute struct {
	Alias           string
	Strategy        string
	CooldownSeconds int
	Targets         []modelTarget
}

type modelTargetYAML struct {
	Model  string `yaml:"model" json:"model"`
	Weight *int   `yaml:"weight,omitempty" json:"weight,omitempty"`
}

type modelRouteYAML struct {
	Alias                 string             `yaml:"alias" json:"alias"`
	Strategy              string             `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	CooldownSeconds       *int               `yaml:"cooldown_seconds,omitempty" json:"cooldown_seconds,omitempty"`
	LegacyCooldownSeconds *int               `yaml:"cooldown-seconds,omitempty" json:"cooldown-seconds,omitempty"`
	Models                *[]string          `yaml:"models,omitempty" json:"models,omitempty"`
	Targets               *[]modelTargetYAML `yaml:"targets,omitempty" json:"targets,omitempty"`
}

type routerConfig struct {
	Enabled       bool
	DataPath      string
	RetentionDays int
	Routes        []modelRoute
}

type routerConfigYAML struct {
	Enabled       *bool             `yaml:"enabled,omitempty"`
	Priority      int               `yaml:"priority,omitempty"`
	Store         yaml.Node         `yaml:"store,omitempty"`
	DataPath      string            `yaml:"data_path,omitempty"`
	RetentionDays *int              `yaml:"retention_days,omitempty"`
	Routes        *[]modelRouteYAML `yaml:"routes,omitempty"`
	LegacyRoutes  *[]modelRouteYAML `yaml:"model-routes,omitempty"`
}

func decodeRouterConfig(raw []byte) (routerConfig, error) {
	wire := routerConfigYAML{}
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(&wire); err != nil {
			return routerConfig{}, fmt.Errorf("decode %s config: %w", pluginID, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				return routerConfig{}, fmt.Errorf("decode %s config: multiple YAML documents are not supported", pluginID)
			}
			return routerConfig{}, fmt.Errorf("decode %s config: %w", pluginID, err)
		}
	}
	if wire.Routes != nil && wire.LegacyRoutes != nil {
		return routerConfig{}, errors.New("model-router config must not contain both routes and model-routes")
	}
	dataPath := strings.TrimSpace(wire.DataPath)
	if dataPath == "" {
		dataPath = defaultDataPathResolver()
	}
	absoluteDataPath, err := filepath.Abs(filepath.Clean(dataPath))
	if err != nil {
		return routerConfig{}, fmt.Errorf("resolve data_path: %w", err)
	}
	retentionDays := defaultRetentionDays
	if wire.RetentionDays != nil {
		retentionDays = *wire.RetentionDays
	}
	if retentionDays < 1 || retentionDays > maxRetentionDays {
		return routerConfig{}, fmt.Errorf("retention_days must be between 1 and %d", maxRetentionDays)
	}
	enabled := true
	if wire.Enabled != nil {
		enabled = *wire.Enabled
	}
	var routeWires []modelRouteYAML
	if wire.Routes != nil {
		routeWires = *wire.Routes
	} else if wire.LegacyRoutes != nil {
		routeWires = *wire.LegacyRoutes
	}
	routes := make([]modelRoute, 0, len(routeWires))
	aliases := make(map[string]int, len(routeWires))
	for index, item := range routeWires {
		if item.CooldownSeconds != nil && item.LegacyCooldownSeconds != nil {
			return routerConfig{}, fmt.Errorf("routes[%d] %q: must not contain both cooldown_seconds and cooldown-seconds", index, strings.TrimSpace(item.Alias))
		}
		cooldown := 0
		if item.CooldownSeconds != nil {
			cooldown = *item.CooldownSeconds
		} else if item.LegacyCooldownSeconds != nil {
			cooldown = *item.LegacyCooldownSeconds
		}
		if item.Models != nil && item.Targets != nil {
			return routerConfig{}, fmt.Errorf("routes[%d] %q: must not contain both models and targets", index, strings.TrimSpace(item.Alias))
		}
		targets := []modelTarget(nil)
		if item.Targets != nil {
			targets = normalizeTargetList(*item.Targets)
		} else if item.Models != nil {
			targets = normalizeLegacyTargetList(*item.Models)
		}
		route := modelRoute{
			Alias:           strings.TrimSpace(item.Alias),
			Strategy:        strings.ToLower(strings.TrimSpace(item.Strategy)),
			CooldownSeconds: cooldown,
			Targets:         targets,
		}
		if route.Strategy == "" {
			route.Strategy = routeStrategyPriority
		}
		if route.CooldownSeconds == 0 {
			route.CooldownSeconds = defaultCooldownSeconds
		}
		if err := validateRoute(route, index, aliases); err != nil {
			return routerConfig{}, err
		}
		aliases[routeKey(route.Alias)] = index
		routes = append(routes, route)
	}
	for routeIndex, route := range routes {
		for targetIndex, target := range route.Targets {
			base, _, _ := splitThinkingSuffix(target.Model)
			if aliasIndex, exists := aliases[routeKey(base)]; exists {
				return routerConfig{}, fmt.Errorf("routes[%d] %q targets[%d] %q: target must not reference route alias at index %d", routeIndex, route.Alias, targetIndex, target.Model, aliasIndex)
			}
		}
	}
	if !enabled {
		routes = nil
	}
	return routerConfig{Enabled: enabled, DataPath: absoluteDataPath, RetentionDays: retentionDays, Routes: routes}, nil
}

func validateRoute(route modelRoute, index int, aliases map[string]int) error {
	if route.Alias == "" {
		return fmt.Errorf("routes[%d]: alias is required", index)
	}
	if _, _, hasSuffix := splitThinkingSuffix(route.Alias); hasSuffix {
		return fmt.Errorf("routes[%d] %q: alias must not include a thinking suffix", index, route.Alias)
	}
	if previous, duplicate := aliases[routeKey(route.Alias)]; duplicate {
		return fmt.Errorf("routes[%d] %q: duplicate alias (also at index %d)", index, route.Alias, previous)
	}
	if route.Strategy != routeStrategyPriority && route.Strategy != routeStrategyRoundRobin {
		return fmt.Errorf("routes[%d] %q: strategy must be %q or %q", index, route.Alias, routeStrategyPriority, routeStrategyRoundRobin)
	}
	if route.CooldownSeconds < 0 {
		return fmt.Errorf("routes[%d] %q: cooldown_seconds must be >= 0", index, route.Alias)
	}
	if len(route.Targets) == 0 {
		return fmt.Errorf("routes[%d] %q: at least one model is required", index, route.Alias)
	}
	seen := make(map[string]int, len(route.Targets))
	for targetIndex, target := range route.Targets {
		model := strings.TrimSpace(target.Model)
		if model == "" {
			return fmt.Errorf("routes[%d] %q targets[%d]: model is required", index, route.Alias, targetIndex)
		}
		if target.Weight < 1 || target.Weight > maxTargetWeight {
			return fmt.Errorf("routes[%d] %q targets[%d] %q: weight must be between 1 and %d", index, route.Alias, targetIndex, model, maxTargetWeight)
		}
		key := routeKey(model)
		if previous, duplicate := seen[key]; duplicate {
			return fmt.Errorf("routes[%d] %q targets[%d] %q: duplicate model (also at index %d)", index, route.Alias, targetIndex, model, previous)
		}
		seen[key] = targetIndex
	}
	return nil
}

func normalizeModelList(models []string) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			out = append(out, model)
		}
	}
	return out
}

func normalizeLegacyTargetList(models []string) []modelTarget {
	normalized := normalizeModelList(models)
	targets := make([]modelTarget, 0, len(normalized))
	for _, model := range normalized {
		targets = append(targets, modelTarget{Model: model, Weight: defaultTargetWeight})
	}
	return targets
}

func normalizeTargetList(targets []modelTargetYAML) []modelTarget {
	out := make([]modelTarget, 0, len(targets))
	for _, target := range targets {
		weight := defaultTargetWeight
		if target.Weight != nil {
			weight = *target.Weight
		}
		out = append(out, modelTarget{Model: strings.TrimSpace(target.Model), Weight: weight})
	}
	return out
}

func routeKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func routeSignature(route modelRoute) string {
	var builder strings.Builder
	builder.WriteString(routeKey(route.Alias))
	builder.WriteByte('\n')
	builder.WriteString(route.Strategy)
	builder.WriteByte('\n')
	builder.WriteString(strconv.Itoa(route.CooldownSeconds))
	for _, target := range route.Targets {
		builder.WriteByte('\n')
		builder.WriteString(strings.TrimSpace(target.Model))
		builder.WriteByte('\n')
		builder.WriteString(strconv.Itoa(target.Weight))
	}
	return builder.String()
}

func splitThinkingSuffix(model string) (string, string, bool) {
	model = strings.TrimSpace(model)
	open := strings.LastIndex(model, "(")
	if open <= 0 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	base := strings.TrimSpace(model[:open])
	suffix := strings.TrimSpace(model[open+1 : len(model)-1])
	if base == "" || suffix == "" {
		return model, "", false
	}
	return base, suffix, true
}

func targetModel(requested, target string) string {
	target = strings.TrimSpace(target)
	if _, _, hasSuffix := splitThinkingSuffix(target); hasSuffix {
		return target
	}
	_, suffix, hasSuffix := splitThinkingSuffix(requested)
	if !hasSuffix {
		return target
	}
	return fmt.Sprintf("%s(%s)", target, suffix)
}
