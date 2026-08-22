package main

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func testModelRoute(alias, strategy string, cooldown int, models ...string) modelRoute {
	targets := make([]modelTarget, 0, len(models))
	for _, model := range models {
		targets = append(targets, modelTarget{Model: model, Weight: defaultTargetWeight})
	}
	return modelRoute{Alias: alias, Strategy: strategy, CooldownSeconds: cooldown, Targets: targets}
}

func TestRouteModelMatchesAliasAndThinkingSuffix(t *testing.T) {
	plugin := testRouterPlugin(testModelRoute("smart", routeStrategyPriority, 30, "provider-a"))
	for _, model := range []string{"smart", "SMART", "smart(high)"} {
		response, err := plugin.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: model})
		if err != nil {
			t.Fatalf("RouteModel(%q) error = %v", model, err)
		}
		if !response.Handled || response.TargetKind != pluginapi.ModelRouteTargetSelf {
			t.Fatalf("RouteModel(%q) = %#v", model, response)
		}
	}
	response, err := plugin.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: "other"})
	if err != nil || response.Handled {
		t.Fatalf("RouteModel(other) = %#v, %v", response, err)
	}
}

func TestRegisterModelsAddsLogicalAliases(t *testing.T) {
	plugin := testRouterPlugin(
		testModelRoute("smart", routeStrategyPriority, 30, "a"),
		testModelRoute("fast", routeStrategyRoundRobin, 30, "b"),
	)
	response, err := plugin.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil {
		t.Fatalf("RegisterModels() error = %v", err)
	}
	if response.Provider != pluginID || len(response.Models) != 2 {
		t.Fatalf("RegisterModels() = %#v", response)
	}
	if response.Models[0].ID != "smart" || !response.Models[0].UserDefined || len(response.Models[0].SupportedGenerationMethods) != 0 {
		t.Fatalf("first model = %#v", response.Models[0])
	}
}

func TestTargetModelPreservesRequestedThinkingSuffix(t *testing.T) {
	if got := targetModel("smart(high)", "provider-a"); got != "provider-a(high)" {
		t.Fatalf("targetModel() = %q", got)
	}
	if got := targetModel("smart(high)", "provider-a(low)"); got != "provider-a(low)" {
		t.Fatalf("targetModel() explicit suffix = %q", got)
	}
}

func testRouterPlugin(routes ...modelRoute) *modelRouterPlugin {
	runtime := newRouteRuntime(nil)
	runtime.Sync(routes)
	return &modelRouterPlugin{config: routerConfig{Enabled: true, Routes: routes}, runtime: runtime}
}
