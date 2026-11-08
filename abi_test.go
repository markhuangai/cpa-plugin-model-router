package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
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
	if !registrationEnvelope.OK || registration.SchemaVersion != registrationSchemaVersion || registration.Metadata.Name != pluginName || registration.Metadata.Version != "0.4.2" || !registration.Capabilities.ModelRegistrar || !registration.Capabilities.ModelRouter || !registration.Capabilities.Executor || !registration.Capabilities.RequestInterceptor || registration.Capabilities.RequestLifecycle || registration.Capabilities.ResponseInterceptor || registration.Capabilities.StreamInterceptor || !registration.Capabilities.UsagePlugin || !registration.Capabilities.ManagementAPI {
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
		Provider: "openai", Model: "provider-a", RequestedAt: time.Now().UTC(), Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 1, CacheCreationTokens: 2, TotalTokens: 11},
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
	if err != nil || page.Total != 1 || page.Items[0].TotalTokens != 11 || page.Items[0].CacheCreationTokens != 2 || page.Items[0].EffectiveCacheReadTokens != 0 {
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
	ModelRouterPluginShutdown()
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
		ModelRouterPluginShutdown()
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

func registeredStreamPlugin(t *testing.T) *modelRouterPlugin {
	t.Helper()
	resetModelRouterABIState(t)
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("routes:\n  - alias: smart\n    models: [provider-a, provider-b]\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registerModelRouter(raw); err != nil {
		t.Fatal(err)
	}
	modelRouterABIState.Lock()
	plugin := modelRouterABIState.plugin
	modelRouterABIState.Unlock()
	return plugin
}

func waitStreamSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("stream lifecycle did not advance")
	}
}

func assertStreamOwnership(t *testing.T, plugin *modelRouterPlugin) {
	t.Helper()
	modelRouterABIState.Lock()
	owned := modelRouterABIState.calls > 0 && len(modelRouterABIState.streams) == 1
	modelRouterABIState.Unlock()
	if !owned {
		t.Error("native callback outlived stream ownership")
	}
	if _, err := plugin.store.QueryPriceBook(); err != nil {
		t.Errorf("storage released during callback: %v", err)
	}
}

func TestShutdownCancelsBlockedStreamAndDrainsCleanup(t *testing.T) {
	for _, blocked := range []string{"read", "emit", "upstream cleanup", "client cleanup"} {
		t.Run(blocked, func(t *testing.T) {
			plugin := registeredStreamPlugin(t)
			blockedCall := make(chan struct{})
			cancelled := make(chan struct{})
			cleanupRelease := make(chan struct{})
			release := sync.OnceFunc(func() { close(cleanupRelease) })
			t.Cleanup(release)
			reads, clientCloses := 0, 0
			host := &fakeModelHost{
				start: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
					return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "upstream"}, nil
				},
				read: func(string) (pluginapi.HostModelStreamReadResponse, error) {
					reads++
					if reads == 1 {
						return pluginapi.HostModelStreamReadResponse{Payload: []byte("data: {\"model\":\"provider-a\"}\n\n")}, nil
					}
					if blocked != "emit" {
						close(blockedCall)
					}
					<-cancelled
					assertStreamOwnership(t, plugin)
					return pluginapi.HostModelStreamReadResponse{}, errors.New("status 429: cancelled upstream")
				},
				closeStream: func(string) error {
					assertStreamOwnership(t, plugin)
					if blocked != "emit" {
						close(cancelled)
					}
					if blocked == "upstream cleanup" {
						<-cleanupRelease
					}
					return nil
				},
				closePlugin: func(string, string) {
					clientCloses++
					assertStreamOwnership(t, plugin)
					if blocked == "emit" {
						close(cancelled)
					}
					if blocked == "client cleanup" {
						<-cleanupRelease
					}
				},
			}
			if blocked == "emit" {
				host.emit = func(string, []byte) error {
					close(blockedCall)
					<-cancelled
					assertStreamOwnership(t, plugin)
					return errors.New("status 429: client stream closed")
				}
			}
			if envelope := startTestStream(t, plugin, host); !envelope.OK {
				t.Fatalf("startup=%#v", envelope)
			}
			waitStreamSignal(t, blockedCall)
			finished := make(chan struct{})
			go func() { ModelRouterPluginShutdown(); close(finished) }()
			waitStreamSignal(t, cancelled)
			if strings.Contains(blocked, "cleanup") {
				select {
				case <-finished:
					t.Fatal("shutdown returned before cleanup callback")
				default:
				}
				assertStreamOwnership(t, plugin)
			}
			release()
			waitStreamSignal(t, finished)
			ModelRouterPluginShutdown()
			if len(host.closed) != 1 || clientCloses != 1 || len(host.startCalls) != 1 {
				t.Fatalf("cleanup upstream=%v client=%d starts=%d", host.closed, clientCloses, len(host.startCalls))
			}
			if _, err := plugin.store.QueryPriceBook(); err == nil {
				t.Fatal("shutdown left storage open")
			}
			if _, _, _, err := beginModelRouterCall(); err == nil {
				t.Fatal("shutdown accepted an ABI call")
			}
			if _, _, err := beginNativeStream(executorRPCRequest{}); err == nil {
				t.Fatal("shutdown accepted a stream")
			}
			if _, err := registerModelRouter(nil); err == nil {
				t.Fatal("registration reopened shutdown admission")
			}
			route, _ := plugin.matchingRoute("smart")
			if selected := plugin.runtime.SelectExcluding(route, nil); selected.model != "provider-a" {
				t.Fatalf("cancellation cooled target: %#v", selected)
			}
		})
	}
}

func TestShutdownWaitsForLateStreamStartup(t *testing.T) {
	plugin := registeredStreamPlugin(t)
	starting, releaseStart := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseStart) })
	t.Cleanup(release)
	clientClosed := make(chan struct{})
	host := &fakeModelHost{
		start: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
			close(starting)
			<-releaseStart
			assertStreamOwnership(t, plugin)
			return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "late-id"}, nil
		},
		closeStream: func(string) error { assertStreamOwnership(t, plugin); return nil },
		closePlugin: func(string, string) { assertStreamOwnership(t, plugin); close(clientClosed) },
	}
	startup := make(chan abiEnvelope, 1)
	go func() {
		raw, _ := plugin.startStream(executorRPCRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "smart"}, StreamID: "client"}, host)
		var envelope abiEnvelope
		_ = json.Unmarshal(raw, &envelope)
		startup <- envelope
	}()
	waitStreamSignal(t, starting)
	finished, second := make(chan struct{}), make(chan struct{})
	go func() { ModelRouterPluginShutdown(); close(finished) }()
	waitStreamSignal(t, clientClosed)
	go func() { ModelRouterPluginShutdown(); close(second) }()
	select {
	case <-finished:
		t.Fatal("shutdown abandoned startup callback")
	case <-second:
		t.Fatal("repeated shutdown returned before drain")
	default:
	}
	release()
	waitStreamSignal(t, finished)
	waitStreamSignal(t, second)
	envelope := <-startup
	if envelope.OK || envelope.Error == nil || !strings.Contains(envelope.Error.Message, "context canceled") || len(host.closed) != 1 || host.closed[0] != "late-id" || len(host.startCalls) != 1 {
		t.Fatalf("late startup=%#v closes=%v", envelope, host.closed)
	}
}

func TestReconfigureKeepsActiveStreamOwned(t *testing.T) {
	plugin := registeredStreamPlugin(t)
	blocked, cancelled := make(chan struct{}), make(chan struct{})
	reads := 0
	host := &fakeModelHost{
		start: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
			return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "active"}, nil
		},
		read: func(string) (pluginapi.HostModelStreamReadResponse, error) {
			reads++
			if reads == 1 {
				return pluginapi.HostModelStreamReadResponse{Payload: []byte("data: {}\n\n")}, nil
			}
			close(blocked)
			<-cancelled
			return pluginapi.HostModelStreamReadResponse{Done: true}, nil
		},
		closeStream: func(string) error { close(cancelled); return nil },
	}
	if envelope := startTestStream(t, plugin, host); !envelope.OK {
		t.Fatal(envelope)
	}
	waitStreamSignal(t, blocked)
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("routes: []\n")})
	if _, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodPluginReconfigure, raw); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
		t.Fatal("reconfiguration cancelled an active stream")
	default:
	}
	assertStreamOwnership(t, plugin)
	ModelRouterPluginShutdown()
	if len(host.closed) != 1 {
		t.Fatalf("active stream closes=%v", host.closed)
	}
}

func TestStreamReadPanicRetainsOwnershipThroughCleanup(t *testing.T) {
	plugin := registeredStreamPlugin(t)
	closed := make(chan string, 1)
	host := &fakeModelHost{
		start: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
			return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "panic-stream"}, nil
		},
		read:        func(string) (pluginapi.HostModelStreamReadResponse, error) { panic("read fixture panic") },
		closeStream: func(string) error { assertStreamOwnership(t, plugin); return nil },
		closePlugin: func(_, message string) { assertStreamOwnership(t, plugin); closed <- message },
	}
	envelope := startTestStream(t, plugin, host)
	if envelope.OK || envelope.Error == nil || !strings.Contains(envelope.Error.Message, "read fixture panic") {
		t.Fatalf("panic envelope=%#v", envelope)
	}
	if message := waitTestStreamClose(t, closed); message != envelope.Error.Message {
		t.Fatalf("panic closure=%q", message)
	}
	ModelRouterPluginShutdown()
	if len(host.closed) != 1 {
		t.Fatalf("panic cleanup=%v", host.closed)
	}
}

func TestShutdownDrainsRegistrationWithoutReopeningAdmission(t *testing.T) {
	resetModelRouterABIState(t)
	starting, released := make(chan struct{}), make(chan struct{})
	resolve := defaultDataPathResolver
	defaultDataPathResolver = func() string { close(starting); <-released; return resolve() }
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	result := make(chan error, 1)
	go func() { _, err := registerModelRouter(nil); result <- err }()
	waitStreamSignal(t, starting)
	finished := make(chan struct{})
	go func() { ModelRouterPluginShutdown(); close(finished) }()
	deadline := time.After(5 * time.Second)
	for {
		modelRouterABIState.Lock()
		shutting := modelRouterABIState.shuttingDown
		modelRouterABIState.Unlock()
		if shutting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("shutdown did not close admission")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-finished:
		t.Fatal("shutdown abandoned registration")
	default:
	}
	release()
	waitStreamSignal(t, finished)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("late registration error=%v", err)
	}
	modelRouterABIState.Lock()
	active := modelRouterABIState.plugin
	shutting := modelRouterABIState.shuttingDown
	modelRouterABIState.Unlock()
	if active != nil || !shutting {
		t.Fatal("late registration reopened admission")
	}
}
