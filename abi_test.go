package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestABIRegistrationRoutingAndCountTokens(t *testing.T) {
	resetModelRouterABIState(t)
	config := []byte(`routes:
  - alias: smart
    models: [provider-a]
`)
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: config})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodPluginRegister, request)
	if err != nil {
		t.Fatalf("plugin.register error = %v", err)
	}
	var registrationEnvelope abiEnvelope
	if err := json.Unmarshal(raw, &registrationEnvelope); err != nil {
		t.Fatalf("decode registration envelope: %v", err)
	}
	var registration abiRegistration
	if err := json.Unmarshal(registrationEnvelope.Result, &registration); err != nil {
		t.Fatalf("decode registration result: %v", err)
	}
	if !registrationEnvelope.OK || registration.SchemaVersion != registrationSchemaVersion || registration.Metadata.Name != pluginName || registration.Metadata.Version != "0.4.0" || !registration.Capabilities.ModelRegistrar || !registration.Capabilities.ModelRouter || !registration.Capabilities.Executor || !registration.Capabilities.RequestInterceptor || !registration.Capabilities.RequestLifecycle || !registration.Capabilities.ResponseInterceptor || !registration.Capabilities.StreamInterceptor || !registration.Capabilities.UsagePlugin || !registration.Capabilities.ManagementAPI {
		t.Fatalf("registration = %#v", registration)
	}

	routeRequest, err := json.Marshal(modelRouteRPCRequest{ModelRouteRequest: pluginapi.ModelRouteRequest{RequestedModel: "smart(high)"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = handleModelRouterABIMethod(t.Context(), pluginabi.MethodModelRoute, routeRequest)
	if err != nil {
		t.Fatalf("model.route error = %v", err)
	}
	var routeEnvelope abiEnvelope
	if err := json.Unmarshal(raw, &routeEnvelope); err != nil {
		t.Fatalf("decode route envelope: %v", err)
	}
	var route pluginapi.ModelRouteResponse
	if err := json.Unmarshal(routeEnvelope.Result, &route); err != nil {
		t.Fatalf("decode route result: %v", err)
	}
	if !route.Handled || route.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("route = %#v", route)
	}

	usageRequest, err := json.Marshal(pluginapi.UsageRecord{
		Provider: "openai", Model: "provider-a", RequestedAt: time.Now().UTC(), Detail: pluginapi.UsageDetail{InputTokens: 2, OutputTokens: 1, TotalTokens: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodUsageHandle, usageRequest); err != nil {
		t.Fatalf("usage.handle error = %v", err)
	}
	modelRouterABIState.Lock()
	activePlugin := modelRouterABIState.plugin
	modelRouterABIState.Unlock()
	page, err := activePlugin.store.Requests(usageFilter{From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC().Add(time.Hour)}, "time", "desc", 0, 10)
	if err != nil || page.Total != 1 || page.Items[0].TotalTokens != 3 {
		t.Fatalf("usage records = %#v, %v", page, err)
	}

	raw, err = handleModelRouterABIMethod(t.Context(), pluginabi.MethodExecutorCountTokens, nil)
	if err != nil {
		t.Fatalf("executor.count_tokens error = %v", err)
	}
	var countEnvelope abiEnvelope
	if err := json.Unmarshal(raw, &countEnvelope); err != nil {
		t.Fatalf("decode count envelope: %v", err)
	}
	if countEnvelope.OK || countEnvelope.Error == nil || countEnvelope.Error.HTTPStatus != 501 || countEnvelope.Error.Code != "model_route_count_tokens_unsupported" {
		t.Fatalf("count envelope = %#v", countEnvelope)
	}
}

func TestABIRegistrationUsesStreamBodyOmissionWhenHostSupportsIt(t *testing.T) {
	resetModelRouterABIState(t)
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("routes: []\n"), SchemaVersion: streamBodyOmissionSchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodPluginRegister, request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope abiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var registration abiRegistration
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatal(err)
	}
	if registration.SchemaVersion != streamBodyOmissionSchemaVersion {
		t.Fatalf("schema version = %d, want %d", registration.SchemaVersion, streamBodyOmissionSchemaVersion)
	}
}

func resetModelRouterABIState(t *testing.T) {
	t.Helper()
	previousResolver := defaultDataPathResolver
	testDataPath := filepath.Join(t.TempDir(), defaultDataFileName)
	defaultDataPathResolver = func() string { return testDataPath }
	modelRouterABIState.Lock()
	previousPlugin := modelRouterABIState.plugin
	modelRouterABIState.plugin = nil
	modelRouterABIState.metadata = pluginapi.Metadata{}
	modelRouterABIState.shuttingDown = false
	modelRouterABIState.Unlock()
	if previousPlugin != nil && previousPlugin.store != nil {
		_ = previousPlugin.store.Close()
	}
	t.Cleanup(func() {
		modelRouterABIState.Lock()
		plugin := modelRouterABIState.plugin
		modelRouterABIState.plugin = nil
		modelRouterABIState.metadata = pluginapi.Metadata{}
		modelRouterABIState.shuttingDown = false
		modelRouterABIState.Unlock()
		if plugin != nil && plugin.store != nil {
			_ = plugin.store.Close()
		}
		defaultDataPathResolver = previousResolver
	})
}
