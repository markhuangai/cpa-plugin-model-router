//go:build integration

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelRouterWithCLIProxyAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the native host smoke test is currently exercised on Unix hosts")
	}
	cpaSource := os.Getenv("CPA_SOURCE")
	if cpaSource == "" {
		cpaSource = filepath.Join("..", "CLIProxyAPI")
	}
	cpaSource, err := filepath.Abs(cpaSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cpaSource, "go.mod")); err != nil {
		t.Fatalf("CPA source not found at %s; set CPA_SOURCE: %v", cpaSource, err)
	}

	var failedCalls atomic.Int32
	var workingCalls atomic.Int32
	var creationCalls atomic.Int32
	var observedModelsMu sync.Mutex
	var observedModels []string
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/chat/completions") {
			http.NotFound(response, request)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		observedModelsMu.Lock()
		observedModels = append(observedModels, body.Model)
		observedModelsMu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		if body.Model == "fail-model" {
			failedCalls.Add(1)
			response.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(response).Encode(map[string]any{"error": map[string]any{"code": "rate_limit", "message": "smoke-test rate limit"}})
			return
		}
		workingCalls.Add(1)
		if body.Stream {
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(response, "data: {\"id\":\"model-router-smoke-stream\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"upstream=%s\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n", body.Model, body.Model)
			_, _ = io.WriteString(response, "data: [DONE]\n\n")
			return
		}
		usage := map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		if strings.HasSuffix(body.Model, "working-model") && creationCalls.CompareAndSwap(0, 1) {
			usage = map[string]any{
				"prompt_tokens":         3,
				"completion_tokens":     1,
				"total_tokens":          4,
				"prompt_tokens_details": map[string]int{"cache_creation_tokens": 2},
			}
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"id":      "model-router-smoke",
			"object":  "chat.completion",
			"created": 1,
			"model":   body.Model,
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "upstream=" + body.Model,
				},
				"finish_reason": "stop",
			}},
			"usage": usage,
		})
	}))
	defer provider.Close()

	workDir := t.TempDir()
	pluginsDir := filepath.Join(workDir, "plugins")
	usagePath := filepath.Join(pluginsDir, defaultDataFileName)
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(pluginsDir, "model-router"+sharedLibraryExtension())
	runSmokeCommand(t, ".", "go", "build", "-buildmode=c-shared", "-o", pluginPath, ".")
	cpaBinary := filepath.Join(workDir, "cli-proxy-api")
	runSmokeCommand(t, cpaSource, "go", "build", "-o", cpaBinary, "./cmd/server")

	port := reserveLocalPort(t)
	configPath := filepath.Join(workDir, "config.yaml")
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
api-keys: ["local-test-key"]
request-retry: 0
usage-statistics-enabled: true
remote-management:
  allow-remote: false
  secret-key: "local-management-key"
  disable-control-panel: true
plugins:
  enabled: true
  dir: %q
  configs:
    model-router:
      enabled: true
      priority: 100
      retention_days: 45
      routes:
        - alias: smoke-router-alias
          strategy: priority
          cooldown_seconds: 60
          models:
            - fail/fail-model
            - work/working-model
        - alias: weighted-router-alias
          strategy: round-robin
          cooldown_seconds: 60
          targets:
            - model: work/working-model
              weight: 3
            - model: other/working-model-2
              weight: 1
openai-compatibility:
  - name: fail
    prefix: fail
    base-url: %q
    api-key-entries:
      - api-key: fail-provider-key
    models:
      - name: fail-model
  - name: work
    prefix: work
    base-url: %q
    api-key-entries:
      - api-key: work-provider-key
    models:
      - name: working-model
  - name: other
    prefix: other
    base-url: %q
    api-key-entries:
      - api-key: other-provider-key
    models:
      - name: working-model-2
`, port, filepath.Join(workDir, "auth"), pluginsDir, provider.URL+"/v1", provider.URL+"/v1", provider.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cpa := startSmokeCPA(t, cpaBinary, configPath, baseURL)
	logs := cpa.logs
	verifyModelRouterManagementUI(t, baseURL, logs)
	if !waitForSmokeModel(t, baseURL, "smoke-router-alias", 10*time.Second) {
		t.Fatalf("model route alias smoke-router-alias was not registered\n%s", logs.String())
	}
	if !waitForSmokeModel(t, baseURL, "weighted-router-alias", 10*time.Second) {
		t.Fatalf("weighted model route alias weighted-router-alias was not registered\n%s", logs.String())
	}
	if _, err := os.Stat(usagePath); err != nil {
		t.Fatalf("default usage database was not created at %q: %v", usagePath, err)
	}
	usageStart := time.Now().UTC().Add(-time.Second)
	for requestIndex := 0; requestIndex < 2; requestIndex++ {
		response := postSmokeChat(t, baseURL, "smoke-router-alias")
		if response["model"] != "smoke-router-alias" {
			t.Fatalf("response model = %v, want smoke-router-alias; response = %#v\n%s", response["model"], response, logs.String())
		}
		choices, ok := response["choices"].([]any)
		if !ok || len(choices) != 1 {
			t.Fatalf("unexpected response choices: %#v", response)
		}
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if message["content"] != "upstream=working-model" {
			t.Fatalf("assistant content = %v; response = %#v\n%s", message["content"], response, logs.String())
		}
	}
	if failedCalls.Load() != 1 || workingCalls.Load() != 2 {
		t.Fatalf("provider calls: failed=%d working=%d, want failed=1 working=2\n%s", failedCalls.Load(), workingCalls.Load(), logs.String())
	}
	directResponse := postSmokeChat(t, baseURL, "work/working-model")
	if directResponse["model"] != "working-model" && directResponse["model"] != "work/working-model" {
		t.Fatalf("direct response model = %v; response = %#v\n%s", directResponse["model"], directResponse, logs.String())
	}
	routedStream := postSmokeChatStream(t, baseURL, "smoke-router-alias")
	if !strings.Contains(routedStream, `"model":"smoke-router-alias"`) || !strings.Contains(routedStream, "upstream=working-model") {
		t.Fatalf("routed stream was not rewritten: %s\n%s", routedStream, logs.String())
	}
	directStream := postSmokeChatStream(t, baseURL, "work/working-model")
	if !strings.Contains(directStream, "upstream=working-model") {
		t.Fatalf("direct stream was not delivered: %s\n%s", directStream, logs.String())
	}
	if failedCalls.Load() != 1 || workingCalls.Load() != 5 {
		t.Fatalf("provider calls after direct and streaming requests: failed=%d working=%d, want failed=1 working=5\n%s", failedCalls.Load(), workingCalls.Load(), logs.String())
	}
	if creationCalls.Load() != 1 {
		t.Fatalf("cache-creation fixture was not served; calls=%d models=%v", creationCalls.Load(), observedModels)
	}

	page := waitForSmokeUsage(t, baseURL, usageStart, 6, 10*time.Second)
	var dashboard usageDashboard
	dashboardPath := modelRouterUsageBasePath + "/dashboard?" + url.Values{
		"from": {usageStart.Format(time.RFC3339Nano)}, "to": {time.Now().UTC().Format(time.RFC3339Nano)},
		"granularity": {"minute"}, "group_dimension": {"router_model"}, "request_limit": {"2"},
	}.Encode()
	if err := requestSmokeManagementJSON(baseURL, dashboardPath, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.Overview.Summary.Requests != 6 || dashboard.Requests.Total != 6 || len(dashboard.Requests.Items) != 2 || dashboard.Groups.Dimension != "router_model" || dashboard.GeneratedAt != dashboard.Requests.GeneratedAt {
		t.Fatalf("combined usage snapshot = %#v", dashboard)
	}
	unauthenticated, err := http.Get(baseURL + dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dashboard status = %d", unauthenticated.StatusCode)
	}
	routedModel := ""
	foundDirect := false
	routedSuccesses, routedFailures, directSuccesses := 0, 0, 0
	totalTokens := uint64(0)
	cacheCreationTokens := uint64(0)
	for _, item := range page.Items {
		totalTokens += item.TotalTokens
		cacheCreationTokens += item.CacheCreationTokens
		switch item.Attribution {
		case attributionRouted:
			if item.RouterModel == "smoke-router-alias" && item.ProviderModel != "" {
				routedModel = item.ProviderModel
			}
			if item.Failed {
				routedFailures++
			} else {
				routedSuccesses++
			}
		case attributionDirect:
			if item.RouterModel == "" && item.ProviderModel != "" {
				foundDirect = true
			}
			if !item.Failed {
				directSuccesses++
			}
		}
	}
	if routedModel == "" || !foundDirect {
		t.Fatalf("usage records do not distinguish routed and direct calls: %#v\n%s", page.Items, logs.String())
	}
	if page.Total != 6 || routedSuccesses != 3 || routedFailures != 1 || directSuccesses != 2 || totalTokens != 14 || cacheCreationTokens != 2 {
		t.Fatalf("usage records were missing, duplicated, or lost cache creation: total=%d routed_success=%d routed_failure=%d direct_success=%d tokens=%d cache_creation=%d items=%#v\n%s", page.Total, routedSuccesses, routedFailures, directSuccesses, totalTokens, cacheCreationTokens, page.Items, logs.String())
	}

	savedPrices := putSmokeManagementJSON[saveModelPricesRequest, modelPriceBook](t, baseURL, modelRouterUsageBasePath+"/prices", saveModelPricesRequest{
		Prices:       map[string]modelPrice{routedModel: {tokenRates: tokenRates{Input: 1.25, Output: 2.5}}},
		SyncSettings: defaultPriceSyncSettings(),
	})
	if savedPrices.Revision != 1 || savedPrices.Prices[routedModel].Input != 1.25 {
		t.Fatalf("saved prices = %#v", savedPrices)
	}
	preferences := defaultDashboardPreferences()
	preferences.RequestPageSize = 25
	preferences.HiddenGroupColumns = []string{"result", "service_tier", "source"}
	savedPreferences := putSmokeManagementJSON[dashboardPreferences, dashboardPreferences](t, baseURL, modelRouterUsageBasePath+"/preferences", preferences)
	if savedPreferences.RequestPageSize != 25 || !slices.Equal(savedPreferences.HiddenGroupColumns, preferences.HiddenGroupColumns) {
		t.Fatalf("saved preferences = %#v", savedPreferences)
	}

	cpa.Stop()
	cpa = startSmokeCPA(t, cpaBinary, configPath, baseURL)
	logs = cpa.logs
	if !waitForSmokeModel(t, baseURL, "smoke-router-alias", 10*time.Second) {
		t.Fatalf("model route alias was not restored after restart\n%s", logs.String())
	}
	restartedPage := waitForSmokeUsage(t, baseURL, usageStart, page.Total, 10*time.Second)
	if restartedPage.Total < page.Total {
		t.Fatalf("usage records were lost across restart: before=%d after=%d\n%s", page.Total, restartedPage.Total, logs.String())
	}
	restartedPrices := getSmokeManagementJSON[modelPriceBook](t, baseURL, modelRouterUsageBasePath+"/prices")
	if restartedPrices.Revision != savedPrices.Revision || restartedPrices.Prices[routedModel].Input != 1.25 {
		t.Fatalf("prices were lost across restart: %#v", restartedPrices)
	}
	restartedPreferences := getSmokeManagementJSON[dashboardPreferences](t, baseURL, modelRouterUsageBasePath+"/preferences")
	if restartedPreferences.RequestPageSize != 25 || !slices.Equal(restartedPreferences.HiddenGroupColumns, preferences.HiddenGroupColumns) {
		t.Fatalf("preferences were lost across restart: %#v", restartedPreferences)
	}

	observedModelsMu.Lock()
	weightedStart := len(observedModels)
	observedModelsMu.Unlock()
	for requestIndex := 0; requestIndex < 8; requestIndex++ {
		response := postSmokeChat(t, baseURL, "weighted-router-alias")
		if response["model"] != "weighted-router-alias" {
			t.Fatalf("weighted response model = %v; response = %#v\n%s", response["model"], response, logs.String())
		}
	}
	observedModelsMu.Lock()
	weightedModels := append([]string(nil), observedModels[weightedStart:]...)
	observedModelsMu.Unlock()
	wantWeightedModels := []string{"working-model", "working-model", "working-model", "working-model-2", "working-model", "working-model", "working-model", "working-model-2"}
	if !slices.Equal(weightedModels, wantWeightedModels) {
		t.Fatalf("weighted provider sequence = %#v, want %#v", weightedModels, wantWeightedModels)
	}
}

func TestNativeUnloadAndAccountingDiagnostics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native CPA integration runs on Unix hosts")
	}
	cpaSource := os.Getenv("CPA_SOURCE")
	if cpaSource == "" {
		cpaSource = filepath.Join("..", "CLIProxyAPI")
	}
	cpaSource, err := filepath.Abs(cpaSource)
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	library := filepath.Join(buildDir, "model-router"+sharedLibraryExtension())
	runSmokeCommand(t, ".", "go", "build", "-buildmode=c-shared", "-o", library, ".")
	cpaBinary := filepath.Join(buildDir, "cli-proxy-api")
	fixtureSource := filepath.Join(buildDir, "cpa-stall-fixture")
	copySmokeCPASource(t, cpaSource, fixtureSource)
	if err := os.WriteFile(filepath.Join(fixtureSource, "cmd", "server", "model_router_stall_fixture.go"), []byte(smokeStallHookSource), 0o600); err != nil {
		t.Fatal(err)
	}
	runSmokeCommand(t, fixtureSource, "go", "build", "-o", cpaBinary, "./cmd/server")
	for _, test := range []struct {
		phase string
		debug bool
		stall bool
	}{{"read_stream", true, false}, {"emit", false, false}, {"read_stream", false, true}} {
		name := test.phase
		if test.stall {
			name = "stalled_log_callback"
		}
		t.Run(name, func(t *testing.T) {
			var writes atomic.Int32
			cancelled := make(chan struct{})
			const promptSentinel = "PRIVATE_PROMPT_SENTINEL"
			const payloadSentinel = "PRIVATE_RESPONSE_SENTINEL"
			provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				defer close(cancelled)
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("provider decode: %v", err)
					return
				}
				response.Header().Set("Content-Type", "text/event-stream")
				payload := payloadSentinel
				if test.phase == "emit" {
					payload += strings.Repeat("x", 128<<10)
				}
				for {
					if _, err := fmt.Fprintf(response, "data: {\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", body.Model, payload); err != nil {
						return
					}
					response.(http.Flusher).Flush()
					writes.Add(1)
					if test.phase == "read_stream" {
						<-request.Context().Done()
						return
					}
					select {
					case <-request.Context().Done():
						return
					default:
					}
				}
			}))
			defer provider.Close()
			workDir := t.TempDir()
			pluginsDir := filepath.Join(workDir, "plugins")
			if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(library)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pluginsDir, "model-router"+sharedLibraryExtension()), raw, 0o700); err != nil {
				t.Fatal(err)
			}
			usagePath := filepath.Join(pluginsDir, defaultDataFileName)
			now := time.Now().UTC().Add(-time.Minute)
			const groups = 300
			store, err := openUsageStore(usagePath, 365)
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < groups; index++ {
				if err := store.Record(storedUsageRecord{RequestedAt: now, Provider: "openai", ExecutorType: "openai", ProviderModel: fmt.Sprintf("legacy-model-%03d", index), AccountingMode: accountingModeInputExcludesCache, InputTokens: 100, CacheReadTokens: 20, CacheCreationTokens: 5}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			port := reserveLocalPort(t)
			configPath := filepath.Join(workDir, "config.yaml")
			config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
api-keys: ["local-test-key"]
debug: %t
logging-to-file: false
logs-max-total-size-mb: 100
request-log: false
request-retry: 0
remote-management:
  allow-remote: false
  secret-key: "local-management-key"
  disable-control-panel: true
plugins:
  enabled: true
  dir: %q
  configs:
    model-router:
      enabled: true
      data_path: %q
      routes:
        - alias: unload-router
          models: [work/stream-model]
openai-compatibility:
  - name: work
    prefix: work
    base-url: %q
    api-key-entries:
      - api-key: sk-PRIVATE_PROVIDER_KEY_SENTINEL
    models:
      - name: stream-model
`, port, filepath.Join(workDir, "auth"), test.debug, pluginsDir, usagePath, provider.URL+"/v1")
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
			var extraArgs []string
			stallFile := filepath.Join(workDir, "release-log-callback")
			if test.stall {
				extraArgs = []string{"-model-router-stall-file", stallFile}
			}
			cpa := startSmokeCPA(t, cpaBinary, configPath, baseURL, extraArgs...)
			defer cpa.Stop()
			if !waitForSmokeModel(t, baseURL, "unload-router", 10*time.Second) {
				t.Fatalf("alias absent\n%s", cpa.logs.String())
			}
			query := url.Values{"from": {now.Add(-time.Minute).Format(time.RFC3339Nano)}, "to": {now.Add(time.Second).Format(time.RFC3339Nano)}}
			for stage := 0; stage < 2; stage++ {
				if stage == 1 {
					prices := make(map[string]modelPrice, groups)
					for index := 0; index < groups; index++ {
						prices[fmt.Sprintf("legacy-model-%03d", index)] = modelPrice{tokenRates: tokenRates{Input: 1}}
					}
					putSmokeManagementJSON[saveModelPricesRequest, modelPriceBook](t, baseURL, modelRouterUsageBasePath+"/prices", saveModelPricesRequest{Prices: prices})
				}
				overview := getSmokeManagementJSON[usageOverview](t, baseURL, modelRouterUsageBasePath+"/overview?"+query.Encode())
				var included uint64
				for _, point := range overview.Series {
					included += point.CacheReadIncludedTokens
				}
				if overview.Summary.Requests != groups || included != 0 {
					t.Fatalf("stage %d accounting changed: %#v", stage, overview)
				}
			}
			query.Set("source", "sk-PRIVATE_FILTER_KEY_SENTINEL")
			getSmokeManagementJSON[usageOverview](t, baseURL, modelRouterUsageBasePath+"/overview?"+query.Encode())
			query.Del("source")
			query.Set("provider_model", "sk-PRIVATE_FILTER_KEY_SENTINEL")
			getSmokeManagementJSON[usageOverview](t, baseURL, modelRouterUsageBasePath+"/overview?"+query.Encode())
			query.Set("provider_model", "ordinary/model")
			getSmokeManagementJSON[usageOverview](t, baseURL, modelRouterUsageBasePath+"/overview?"+query.Encode())

			connection, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			if err := connection.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatal(err)
			}
			body := `{"model":"unload-router","messages":[{"role":"user","content":"` + promptSentinel + `"}],"stream":true}`
			if _, err := fmt.Fprintf(connection, "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer local-test-key\r\nX-Private-Sentinel: PRIVATE_HEADER_SENTINEL\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(connection)
			status, err := reader.ReadString('\n')
			if err != nil || !strings.Contains(status, "200 OK") {
				t.Fatalf("stream status=%q error=%v\n%s", status, err, cpa.logs.String())
			}
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if line == "\r\n" {
					break
				}
			}
			if test.phase == "emit" {
				deadline := time.Now().Add(10 * time.Second)
				for {
					before := writes.Load()
					time.Sleep(200 * time.Millisecond)
					if before >= 8 && writes.Load() == before {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("provider did not encounter backpressure: writes=%d", writes.Load())
					}
				}
			}
			request, err := http.NewRequest(http.MethodDelete, baseURL+"/v0/management/plugins/model-router", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer local-management-key")
			type unloadResult struct {
				status int
				body   []byte
				err    error
			}
			unloaded := make(chan unloadResult, 1)
			go func() {
				response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
				result := unloadResult{err: err}
				if err == nil {
					result.status = response.StatusCode
					result.body, result.err = io.ReadAll(response.Body)
					response.Body.Close()
				}
				unloaded <- result
			}()
			if test.stall {
				deadline := time.Now().Add(10 * time.Second)
				for {
					_, entered := os.Stat(stallFile + ".entered")
					logs := cpa.logs.String()
					start := strings.Index(logs, "model_router.shutdown.stalled ")
					if entered == nil && start >= 0 && strings.Contains(logs[start:], "\n") {
						break
					}
					select {
					case result := <-unloaded:
						t.Fatalf("unload returned while log callback was held: %#v\n%s", result, cpa.logs.String())
					default:
					}
					if time.Now().After(deadline) {
						t.Fatalf("missing real WARN for stalled host callback\n%s", cpa.logs.String())
					}
					time.Sleep(25 * time.Millisecond)
				}
				var warning *smokeRouterEvent
				warnLevel := false
				for _, line := range strings.Split(cpa.logs.String(), "\n") {
					if strings.Contains(line, "model_router.shutdown.stalled ") && strings.Contains(line, "[warn ]") {
						warnLevel = true
					}
				}
				for _, event := range smokeModelRouterEvents(t, cpa.logs.String()) {
					if event.name == "model_router.shutdown.stalled" {
						warning = &event
					}
				}
				if warning == nil || !warnLevel || warning.fields["remaining_calls"].(float64) < 1 || warning.fields["elapsed_ms"].(float64) < 5000 {
					t.Fatalf("invalid stall warning: %v", warning)
				}
				streams := warning.fields["streams"].([]any)
				if len(streams) != 1 || streams[0].(map[string]any)["phase"] != "log_outcome" || streams[0].(map[string]any)["shutdown_operation_id"] != warning.fields["operation_id"] {
					t.Fatalf("warning lost outstanding phase: %v", warning)
				}
				if err := os.WriteFile(stallFile, []byte("release"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case result := <-unloaded:
				if result.err != nil || result.status != http.StatusOK {
					t.Fatalf("unload status=%d body=%s error=%v\n%s", result.status, result.body, result.err, cpa.logs.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("unload did not drain after callback release\n%s", cpa.logs.String())
			}
			waitStreamSignal(t, cancelled)
			plugins := getSmokeManagementJSON[struct {
				Plugins []struct {
					ID         string `json:"id"`
					Registered bool   `json:"registered"`
				} `json:"plugins"`
			}](t, baseURL, "/v0/management/plugins")
			for _, plugin := range plugins.Plugins {
				if plugin.ID == pluginID && plugin.Registered {
					t.Fatal("unload left the native plugin registered")
				}
			}
			if !smokeModelListed(t, baseURL, "work/stream-model") {
				t.Fatal("CPA did not survive native unload")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				logs := cpa.logs.String()
				start := strings.Index(logs, "model_router.shutdown.complete ")
				if start >= 0 && strings.Contains(logs[start:], "\n") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("missing shutdown completion log\n%s", logs)
				}
				time.Sleep(10 * time.Millisecond)
			}
			events := smokeModelRouterEvents(t, cpa.logs.String())
			starts, completes, cancellations := 0, 0, 0
			shutdownOperation := ""
			for _, event := range events {
				switch event.name {
				case "model_router.shutdown.start":
					starts++
					shutdownOperation = event.fields["operation_id"].(string)
					if event.fields["active_streams"] != float64(1) {
						t.Fatalf("shutdown had no active stream: %v", event.fields)
					}
				case "model_router.stream.complete":
					if completes > 0 {
						t.Fatal("stream callback completed after shutdown")
					}
					if event.fields["outcome"] == "cancelled_shutdown" {
						cancellations++
						if event.fields["phase"] != test.phase || event.fields["client_cleanup"] != "returned" || event.fields["upstream_cleanup"] != "returned" {
							t.Fatalf("cancellation diagnosis=%v", event.fields)
						}
					}
				case "model_router.shutdown.complete":
					completes++
					if event.fields["operation_id"] != shutdownOperation || event.fields["remaining_calls"] != float64(0) || cancellations != 1 {
						t.Fatalf("shutdown did not drain first: %v", event.fields)
					}
				}
			}
			if starts != 1 || completes != 1 || cancellations != 1 {
				t.Fatalf("missing native lifetime evidence: %v\n%s", events, cpa.logs.String())
			}
			verifySmokeAccountingDecisions(t, events, test.debug, groups)
			for _, line := range strings.Split(cpa.logs.String(), "\n") {
				if !strings.Contains(line, "model_router.") {
					continue
				}
				for _, secret := range []string{promptSentinel, payloadSentinel, "PRIVATE_HEADER_SENTINEL", "sk-PRIVATE_PROVIDER_KEY_SENTINEL", "sk-PRIVATE_FILTER_KEY_SENTINEL", "local-test-key", "local-management-key"} {
					if strings.Contains(line, secret) {
						t.Fatalf("diagnostic exposed %q", secret)
					}
				}
				t.Log(line)
			}
		})
	}
}

type smokeRouterEvent struct {
	name   string
	fields map[string]any
}

func smokeModelRouterEvents(t *testing.T, logs string) []smokeRouterEvent {
	t.Helper()
	var events []smokeRouterEvent
	lines := strings.Split(logs, "\n")
	for _, line := range lines[:len(lines)-1] {
		start := strings.Index(line, "model_router.")
		if start < 0 {
			continue
		}
		name, raw, ok := strings.Cut(line[start:], " ")
		if !ok {
			t.Fatalf("diagnostic has no fields: %s", line)
		}
		var fields map[string]any
		if err := json.NewDecoder(strings.NewReader(raw)).Decode(&fields); err != nil {
			t.Fatalf("diagnostic fields: %v: %s", err, line)
		}
		if fields["plugin_version"] != pluginVersion || fields["operation_id"] == "" {
			t.Fatalf("uncorrelated event: %v", fields)
		}
		events = append(events, smokeRouterEvent{name, fields})
	}
	return events
}

func verifySmokeAccountingDecisions(t *testing.T, events []smokeRouterEvent, debug bool, expected int) {
	t.Helper()
	chunks := map[string][]string{}
	outcomes := map[string]map[string]any{}
	for _, event := range events {
		if event.name == "model_router.accounting.decisions" {
			operation := event.fields["operation_id"].(string)
			index, total := int(event.fields["chunk"].(float64)), int(event.fields["chunks"].(float64))
			if chunks[operation] == nil {
				chunks[operation] = make([]string, total)
			}
			part := event.fields["decision_json"].(string)
			if len(part) > 32<<10 || index < 1 || index > total || chunks[operation][index-1] != "" {
				t.Fatal("unbounded or duplicate diagnostic batch")
			}
			chunks[operation][index-1] = part
		}
		if event.name == "model_router.query.complete" {
			outcomes[event.fields["operation_id"].(string)] = event.fields
		}
	}
	if !debug {
		if len(chunks) != 0 || len(outcomes) != 0 {
			t.Fatal("DEBUG accounting evidence survived INFO filtering")
		}
		return
	}
	if len(chunks) != 5 || len(outcomes) != 5 {
		t.Fatalf("query evidence count: batches=%d outcomes=%d", len(chunks), len(outcomes))
	}
	revisions := map[uint64]bool{}
	for operation, parts := range chunks {
		for _, part := range parts {
			if part == "" {
				t.Fatal("missing diagnostic batch")
			}
		}
		var report struct {
			Revision      uint64 `json:"price_book_revision"`
			Source        string `json:"source"`
			ProviderModel string `json:"provider_model"`
			Groups        []struct {
				accountingDiagnosticKey
				accountingDiagnosticTotals
			} `json:"groups"`
		}
		if err := json.Unmarshal([]byte(strings.Join(parts, "")), &report); err != nil {
			t.Fatal(err)
		}
		if outcomes[operation] == nil || outcomes[operation]["outcome"] != "complete" {
			t.Fatal("missing correlated query completion")
		}
		if report.Source != "" {
			if report.Source != "[omitted: credential-shaped source filter]" || len(report.Groups) != 0 {
				t.Fatalf("unsafe filter diagnosis: %#v", report)
			}
			continue
		}
		if report.ProviderModel != "" {
			if (report.ProviderModel != "[omitted: credential-shaped filter]" && report.ProviderModel != "ordinary/model") || len(report.Groups) != 0 {
				t.Fatalf("unsafe or unusable model filter: %#v", report)
			}
			continue
		}
		if len(parts) < 2 || len(report.Groups) != expected {
			t.Fatalf("lost decision groups: chunks=%d groups=%d", len(parts), len(report.Groups))
		}
		for _, group := range report.Groups {
			if group.Override != "" || group.Stored != accountingModeInputExcludesCache || group.Default != accountingModeInputIncludesCache || group.Effective != accountingModeInputExcludesCache || group.Requests != 1 || group.CacheRead != 20 || group.Included != 0 || group.Priced != (report.Revision == 1) {
				t.Fatalf("accounting decision=%#v", group)
			}
		}
		revisions[report.Revision] = true
	}
	if !revisions[0] || !revisions[1] {
		t.Fatal("missing before/after price revision evidence")
	}
}

func TestModelRouterFallbackStatusesWithCLIProxyAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the native host smoke test is currently exercised on Unix hosts")
	}
	cpaSource := os.Getenv("CPA_SOURCE")
	if cpaSource == "" {
		cpaSource = filepath.Join("..", "CLIProxyAPI")
	}
	cpaSource, err := filepath.Abs(cpaSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cpaSource, "go.mod")); err != nil {
		t.Fatalf("CPA source not found at %s; set CPA_SOURCE: %v", cpaSource, err)
	}

	type fallbackScenario struct {
		name           string
		status         int
		stream         bool
		fallback       string
		missingTargets int
		wantStatus     int
	}
	configuredFallback := "fallback_on_status: [400], no_fallback_on_status: [429]"
	scenarios := []fallbackScenario{
		{name: "configured_400_chat", status: 400, fallback: configuredFallback, wantStatus: 200},
		{name: "excluded_429_chat", status: 429, fallback: configuredFallback, wantStatus: 429},
		{name: "configured_400_stream", status: 400, stream: true, fallback: configuredFallback, wantStatus: 200},
		{name: "excluded_429_stream", status: 429, stream: true, fallback: configuredFallback, wantStatus: 429},
	}
	for _, stream := range []bool{false, true} {
		suffix := "_chat"
		if stream {
			suffix = "_stream"
		}
		scenarios = append(scenarios,
			fallbackScenario{name: "missing_provider" + suffix, stream: stream, missingTargets: 1, wantStatus: 200},
			fallbackScenario{name: "excluded_missing_provider" + suffix, stream: stream, missingTargets: 1, fallback: "no_fallback_on_status: [400]", wantStatus: 400},
			fallbackScenario{name: "all_providers_missing" + suffix, stream: stream, missingTargets: 2, wantStatus: 503},
		)
		for _, status := range []int{400, 401, 402, 422, 429} {
			wantStatus := 200
			if status == 400 || status == 422 {
				wantStatus = status
			}
			scenarios = append(scenarios, fallbackScenario{name: fmt.Sprintf("default_%d%s", status, suffix), status: status, stream: stream, wantStatus: wantStatus})
		}
	}
	type fallbackProvider struct {
		first  bool
		status int
	}
	providersByKey := make(map[string]fallbackProvider, len(scenarios)*2)
	var routes strings.Builder
	for _, scenario := range scenarios {
		fmt.Fprintf(&routes, `
        - alias: %s
          strategy: priority
          cooldown_seconds: 60
          targets:
`, scenario.name)
		for index, target := range []struct {
			name  string
			first bool
		}{{name: "first", first: true}, {name: "second"}} {
			providerName := "issue11-" + scenario.name + "-" + target.name
			fmt.Fprintf(&routes, "            - model: %s/issue11-model\n", providerName)
			if index < scenario.missingTargets {
				continue
			}
			providersByKey["issue11-"+scenario.name+"-"+target.name+"-key"] = fallbackProvider{
				first:  target.first,
				status: scenario.status,
			}
		}
	}

	var callsMu sync.Mutex
	calls := make(map[string]int, len(scenarios)*2)
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/chat/completions") {
			http.NotFound(response, request)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		fixture, ok := providersByKey[key]
		if !ok {
			http.Error(response, "unexpected fixture credential", http.StatusUnauthorized)
			return
		}
		callsMu.Lock()
		calls[key]++
		callsMu.Unlock()
		if fixture.first {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(fixture.status)
			message := "invalid prompt"
			code := "invalid_request_error"
			if fixture.status == http.StatusTooManyRequests {
				message = "rate limit exceeded"
				code = "rate_limit_error"
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"error": map[string]string{"message": message, "type": code}})
			return
		}
		if body.Stream {
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(response, "data: {\"id\":\"issue11-fallback\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"upstream=%s\"},\"finish_reason\":\"stop\"}]}\n\n", body.Model, body.Model)
			_, _ = io.WriteString(response, "data: [DONE]\n\n")
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"id": "issue11-fallback", "object": "chat.completion", "created": 1, "model": body.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": "upstream=" + body.Model},
				"finish_reason": "stop",
			}},
		})
	}))
	defer provider.Close()

	var providerConfig strings.Builder
	for key := range providersByKey {
		name := strings.TrimSuffix(key, "-key")
		prefix := "issue11-" + strings.TrimPrefix(name, "issue11-")
		fmt.Fprintf(&providerConfig, `
  - name: %s
    prefix: %s
    base-url: %q
    api-key-entries:
      - api-key: %s
    models:
      - name: issue11-model
`, name, prefix, provider.URL+"/v1", key)
	}
	workDir := t.TempDir()
	pluginArtifact := filepath.Join(workDir, "model-router"+sharedLibraryExtension())
	runSmokeCommand(t, ".", "go", "build", "-buildmode=c-shared", "-o", pluginArtifact, ".")
	cpaBinary := filepath.Join(workDir, "cli-proxy-api")
	runSmokeCommand(t, cpaSource, "go", "build", "-o", cpaBinary, "./cmd/server")
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			caseDir := filepath.Join(workDir, scenario.name)
			pluginsDir := filepath.Join(caseDir, "plugins")
			if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(pluginArtifact, filepath.Join(pluginsDir, "model-router"+sharedLibraryExtension())); err != nil {
				t.Fatalf("link plugin for %s: %v", scenario.name, err)
			}
			port := reserveLocalPort(t)
			configPath := filepath.Join(caseDir, "config.yaml")
			config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
api-keys: ["local-test-key"]
request-retry: 0
streaming-bootstrap-retries: 0
plugins:
  enabled: true
  dir: %q
  configs:
    model-router:
      enabled: true
      priority: 100
      fallback: {%s}
      routes:%s
openai-compatibility:%s`, port, filepath.Join(caseDir, "auth"), pluginsDir, scenario.fallback, routes.String(), providerConfig.String())
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
			cpa := startSmokeCPA(t, cpaBinary, configPath, baseURL)
			defer cpa.Stop()
			if !waitForSmokeModel(t, baseURL, scenario.name, 10*time.Second) {
				t.Fatalf("model route alias %s was not registered", scenario.name)
			}
			for attempt := range 2 {
				status, body := postSmokeChatResponse(t, baseURL, scenario.name, scenario.stream)
				wantStatus := scenario.wantStatus
				if attempt == 1 && wantStatus == 503 {
					wantStatus = 429
				}
				if status != wantStatus {
					t.Fatalf("request %d status = %d, want %d; body = %s", attempt+1, status, wantStatus, body)
				}
				if wantStatus == 200 && (!strings.Contains(body, "upstream=issue11-model") || !strings.Contains(body, scenario.name)) {
					t.Fatalf("request %d did not return the rewritten fallback response: %s", attempt+1, body)
				}
				if (wantStatus == 400 || wantStatus == 422) && scenario.missingTargets == 0 && !strings.Contains(body, "invalid prompt") {
					t.Fatalf("request %d lost upstream error details: %s", attempt+1, body)
				}
				// CPA cools the credential even when the router excludes 429 from failover.
				if attempt == 1 && scenario.wantStatus == 429 && !strings.Contains(body, `"code":"model_cooldown"`) {
					t.Fatalf("excluded 429 should retain CPA's credential cooldown: %s", body)
				}
			}
			firstKey := "issue11-" + scenario.name + "-first-key"
			secondKey := "issue11-" + scenario.name + "-second-key"
			callsMu.Lock()
			firstCalls, secondCalls := calls[firstKey], calls[secondKey]
			callsMu.Unlock()
			wantFirstCalls := 2
			if scenario.missingTargets > 0 {
				wantFirstCalls = 0
			} else if scenario.wantStatus == 200 || scenario.wantStatus == 429 {
				wantFirstCalls = 1
			}
			wantSecondCalls := 0
			if scenario.wantStatus == 200 {
				wantSecondCalls = 2
			}
			if firstCalls != wantFirstCalls || secondCalls != wantSecondCalls {
				t.Fatalf("target calls = (%d, %d), want (%d, %d)", firstCalls, secondCalls, wantFirstCalls, wantSecondCalls)
			}
		})
	}
}

func TestModelRouterResponsesSSEFramingWithCLIProxyAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the native host smoke test is currently exercised on Unix hosts")
	}
	cpaSource := os.Getenv("CPA_SOURCE")
	if cpaSource == "" {
		cpaSource = filepath.Join("..", "CLIProxyAPI")
	}
	cpaSource, err := filepath.Abs(cpaSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cpaSource, "go.mod")); err != nil {
		t.Fatalf("CPA source not found at %s; set CPA_SOURCE: %v", cpaSource, err)
	}

	var observedModelMu sync.Mutex
	observedModel := ""
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/responses") {
			http.NotFound(response, request)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		if !body.Stream {
			http.Error(response, "expected streaming request", http.StatusBadRequest)
			return
		}
		observedModelMu.Lock()
		observedModel = body.Model
		observedModelMu.Unlock()

		response.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := response.(http.Flusher)
		if !ok {
			http.Error(response, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		frames := []struct {
			event string
			data  string
		}{
			{
				event: "response.created",
				data:  fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":%q,"output":[]}}`, body.Model),
			},
			{
				event: "response.completed",
				data:  fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":%q,"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, body.Model),
			},
		}
		for _, frame := range frames {
			_, _ = fmt.Fprintf(response, "event: %s\n", frame.event)
			flusher.Flush()
			_, _ = fmt.Fprintf(response, "data: %s\n\n", frame.data)
			flusher.Flush()
		}
	}))
	defer provider.Close()

	workDir := t.TempDir()
	pluginsDir := filepath.Join(workDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(pluginsDir, "model-router"+sharedLibraryExtension())
	runSmokeCommand(t, ".", "go", "build", "-buildmode=c-shared", "-o", pluginPath, ".")
	cpaBinary := filepath.Join(workDir, "cli-proxy-api")
	runSmokeCommand(t, cpaSource, "go", "build", "-o", cpaBinary, "./cmd/server")

	port := reserveLocalPort(t)
	configPath := filepath.Join(workDir, "config.yaml")
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
api-keys: ["local-test-key"]
request-retry: 0
streaming-bootstrap-retries: 0
plugins:
  enabled: true
  dir: %q
  configs:
    model-router:
      enabled: true
      priority: 100
      routes:
        - alias: responses-router-alias
          strategy: priority
          cooldown_seconds: 60
          models:
            - responses-physical-model
codex-api-key:
  - api-key: "local-provider-key"
    base-url: %q
    models:
      - name: responses-physical-model
`, port, filepath.Join(workDir, "auth"), pluginsDir, provider.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cpa := startSmokeCPA(t, cpaBinary, configPath, baseURL)
	if !waitForSmokeModel(t, baseURL, "responses-router-alias", 10*time.Second) {
		t.Fatalf("responses-router-alias was not registered\n%s", cpa.logs.String())
	}
	requestBody := []byte(`{"model":"responses-router-alias","input":"Reply OK","stream":true}`)
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-test-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("responses stream returned %s: %s\n%s", response.Status, responseBody, cpa.logs.String())
	}
	stream := string(responseBody)
	if !strings.Contains(stream, "event: response.created") || !strings.Contains(stream, "event: response.completed") {
		t.Fatalf("responses stream lost event frames: %q", stream)
	}
	if strings.Count(stream, `"model":"responses-router-alias"`) != 2 || strings.Contains(stream, "responses-physical-model") {
		t.Fatalf("responses stream model was not rewritten: %q", stream)
	}
	observedModelMu.Lock()
	upstreamModel := observedModel
	observedModelMu.Unlock()
	if upstreamModel != "responses-physical-model" {
		t.Fatalf("provider model = %q, want responses-physical-model", upstreamModel)
	}
}

func verifyModelRouterManagementUI(t *testing.T, baseURL string, logs *smokeSyncBuffer) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v0/management/plugins", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-management-key")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("list plugins: %v\n%s", err, logs.String())
	}
	defer response.Body.Close()
	var plugins struct {
		Plugins []struct {
			ID    string `json:"id"`
			Menus []struct {
				Path string `json:"path"`
				Menu string `json:"menu"`
			} `json:"menus"`
		} `json:"plugins"`
	}
	if err := json.NewDecoder(response.Body).Decode(&plugins); err != nil {
		t.Fatalf("decode plugin list: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("plugin list status = %s\n%s", response.Status, logs.String())
	}
	resourcePath := ""
	for _, plugin := range plugins.Plugins {
		if plugin.ID != pluginID {
			continue
		}
		for _, menu := range plugin.Menus {
			if menu.Menu == pluginName {
				resourcePath = menu.Path
			}
		}
	}
	if resourcePath != modelRouterDashboardPath {
		t.Fatalf("dashboard path = %q, want %q; plugins=%#v\n%s", resourcePath, modelRouterDashboardPath, plugins, logs.String())
	}

	resourceResponse, err := client.Get(baseURL + resourcePath)
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resourceResponse.Body.Close()
	resourceBody, err := io.ReadAll(resourceResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resourceResponse.StatusCode != http.StatusOK || !strings.Contains(string(resourceBody), "<title>Model Router</title>") {
		t.Fatalf("dashboard status=%s body=%q", resourceResponse.Status, resourceBody)
	}
	for _, expected := range []string{"/v0/management/api-keys", "/v1/models", "<select data-target-field=\"model\"", "data-target-field=\"weight\"", "data-target-help"} {
		if !strings.Contains(string(resourceBody), expected) {
			t.Fatalf("dashboard missing %q", expected)
		}
	}

	apiKeysRequest, err := http.NewRequest(http.MethodGet, baseURL+"/v0/management/api-keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	apiKeysRequest.Header.Set("Authorization", "Bearer local-management-key")
	apiKeysResponse, err := client.Do(apiKeysRequest)
	if err != nil {
		t.Fatalf("get CPA API keys: %v\n%s", err, logs.String())
	}
	var apiKeys struct {
		Keys []string `json:"api-keys"`
	}
	if err := json.NewDecoder(apiKeysResponse.Body).Decode(&apiKeys); err != nil {
		apiKeysResponse.Body.Close()
		t.Fatalf("decode CPA API keys: %v", err)
	}
	apiKeysResponse.Body.Close()
	if apiKeysResponse.StatusCode != http.StatusOK || len(apiKeys.Keys) == 0 || apiKeys.Keys[0] != "local-test-key" {
		t.Fatalf("CPA API keys status=%s keys=%#v", apiKeysResponse.Status, apiKeys.Keys)
	}

	modelsRequest, err := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	modelsRequest.Header.Set("Authorization", "Bearer "+apiKeys.Keys[0])
	modelsResponse, err := client.Do(modelsRequest)
	if err != nil {
		t.Fatalf("get CPA models: %v\n%s", err, logs.String())
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(modelsResponse.Body).Decode(&models); err != nil {
		modelsResponse.Body.Close()
		t.Fatalf("decode CPA models: %v", err)
	}
	modelsResponse.Body.Close()
	if modelsResponse.StatusCode != http.StatusOK {
		t.Fatalf("CPA models status=%s", modelsResponse.Status)
	}
	modelIDs := make(map[string]bool, len(models.Data))
	for _, model := range models.Data {
		modelIDs[model.ID] = true
	}
	for _, expected := range []string{"fail/fail-model", "work/working-model", "other/working-model-2"} {
		if !modelIDs[expected] {
			t.Fatalf("CPA models missing %q: %#v", expected, modelIDs)
		}
	}

	validation := []byte(`{"enabled":true,"routes":[{"alias":"smoke","strategy":"priority","cooldown_seconds":60,"models":["provider/model"]}]}`)
	unauthenticatedRequest, err := http.NewRequest(http.MethodPost, baseURL+modelRouterValidationPath, bytes.NewReader(validation))
	if err != nil {
		t.Fatal(err)
	}
	unauthenticatedRequest.Header.Set("Content-Type", "application/json")
	unauthenticatedResponse, err := client.Do(unauthenticatedRequest)
	if err != nil {
		t.Fatalf("validate dashboard config without authentication: %v", err)
	}
	unauthenticatedResponse.Body.Close()
	if unauthenticatedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated validation status=%s, want 401", unauthenticatedResponse.Status)
	}

	validationRequest, err := http.NewRequest(http.MethodPost, baseURL+modelRouterValidationPath, bytes.NewReader(validation))
	if err != nil {
		t.Fatal(err)
	}
	validationRequest.Header.Set("Authorization", "Bearer local-management-key")
	validationRequest.Header.Set("Content-Type", "application/json")
	validationResponse, err := client.Do(validationRequest)
	if err != nil {
		t.Fatalf("validate dashboard config: %v", err)
	}
	defer validationResponse.Body.Close()
	var validationBody map[string]any
	if err := json.NewDecoder(validationResponse.Body).Decode(&validationBody); err != nil {
		t.Fatal(err)
	}
	if validationResponse.StatusCode != http.StatusOK || validationBody["valid"] != true {
		t.Fatalf("validation status=%s body=%#v", validationResponse.Status, validationBody)
	}
}

type smokeSyncBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (buffer *smokeSyncBuffer) Write(payload []byte) (int, error) {
	buffer.Lock()
	defer buffer.Unlock()
	return buffer.Buffer.Write(payload)
}

func (buffer *smokeSyncBuffer) String() string {
	buffer.Lock()
	defer buffer.Unlock()
	return buffer.Buffer.String()
}

func runSmokeCommand(t *testing.T, dir, name string, arguments ...string) {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(arguments, " "), err, output)
	}
}

func requestSmokeManagementJSON(baseURL, path string, destination any) error {
	request, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer local-management-key")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("GET %s returned %s: %s", path, response.Status, body)
	}
	return json.NewDecoder(response.Body).Decode(destination)
}

func getSmokeManagementJSON[T any](t *testing.T, baseURL, path string) T {
	t.Helper()
	var result T
	if err := requestSmokeManagementJSON(baseURL, path, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func putSmokeManagementJSON[Input, Output any](t *testing.T, baseURL, path string, input Input) Output {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-management-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("PUT %s returned %s: %s", path, response.Status, responseBody)
	}
	var result Output
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func waitForSmokeUsage(t *testing.T, baseURL string, from time.Time, minimumTotal int, timeout time.Duration) usageRequestPage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastPage usageRequestPage
	var lastErr error
	for time.Now().Before(deadline) {
		query := url.Values{
			"from":  {from.Format(time.RFC3339Nano)},
			"to":    {time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)},
			"limit": {"100"},
		}
		var page usageRequestPage
		lastErr = requestSmokeManagementJSON(baseURL, modelRouterUsageBasePath+"/requests?"+query.Encode(), &page)
		if lastErr == nil {
			lastPage = page
			foundRouted, foundDirect := false, false
			for _, item := range lastPage.Items {
				foundRouted = foundRouted || item.Attribution == attributionRouted && item.RouterModel == "smoke-router-alias"
				foundDirect = foundDirect || item.Attribution == attributionDirect && item.RouterModel == ""
			}
			if foundRouted && foundDirect && lastPage.Total >= minimumTotal {
				return lastPage
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("usage records did not contain routed and direct requests: page=%#v error=%v", lastPage, lastErr)
	return usageRequestPage{}
}

func TestWaitForSmokeUsageDoesNotReuseOmittedRouterModel(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		page := usageRequestPage{
			Total: 5,
			Items: []usageRequestDetail{
				{storedUsageRecord: storedUsageRecord{Attribution: attributionRouted, RouterModel: "smoke-router-alias"}},
				{storedUsageRecord: storedUsageRecord{Attribution: attributionRouted, RouterModel: "smoke-router-alias"}},
			},
		}
		if polls.Add(1) > 1 {
			page = usageRequestPage{
				Total: 6,
				Items: []usageRequestDetail{
					{storedUsageRecord: storedUsageRecord{Attribution: attributionDirect}},
					{storedUsageRecord: storedUsageRecord{Attribution: attributionRouted, RouterModel: "smoke-router-alias"}},
				},
			}
		}
		if err := json.NewEncoder(response).Encode(page); err != nil {
			t.Errorf("encode usage page: %v", err)
		}
	}))
	defer server.Close()

	page := waitForSmokeUsage(t, server.URL, time.Now().UTC().Add(-time.Second), 6, time.Second)
	if polls.Load() < 2 || page.Total != 6 || page.Items[0].Attribution != attributionDirect || page.Items[0].RouterModel != "" {
		t.Fatalf("direct usage row retained a prior router model: polls=%d page=%#v", polls.Load(), page)
	}
}

func postSmokeChatStream(t *testing.T, baseURL, model string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-test-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream chat returned %s: %s", response.Status, responseBody)
	}
	return string(responseBody)
}

func reserveLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

type smokeCPAProcess struct {
	command  *exec.Cmd
	done     chan error
	logs     *smokeSyncBuffer
	stopOnce sync.Once
}

func (process *smokeCPAProcess) Stop() {
	if process == nil {
		return
	}
	process.stopOnce.Do(func() {
		if process.command.Process != nil {
			_ = process.command.Process.Signal(os.Interrupt)
		}
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
			if process.command.Process != nil {
				_ = process.command.Process.Kill()
			}
			select {
			case <-process.done:
			case <-time.After(5 * time.Second):
			}
		}
	})
}

func startSmokeCPA(t *testing.T, binary, configPath, baseURL string, extraArgs ...string) *smokeCPAProcess {
	t.Helper()
	logs := &smokeSyncBuffer{}
	command := exec.Command(binary, append([]string{"--config", configPath}, extraArgs...)...)
	command.Stdout = logs
	command.Stderr = logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &smokeCPAProcess{command: command, done: make(chan error, 1), logs: logs}
	go func() { process.done <- command.Wait() }()
	t.Cleanup(process.Stop)

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-process.done:
			t.Fatalf("CLIProxyAPI exited before becoming ready: %v\n%s", err, logs.String())
		default:
		}
		request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer local-test-key")
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("CLIProxyAPI did not become ready at %s\n%s", baseURL, logs.String())
	return process
}

func copySmokeCPASource(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(destination, relative), 0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected CPA fixture file type: %s", relative)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(destination, relative), raw, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The disposable CPA build holds host.log open without altering the cached source or production host.
const smokeStallHookSource = `package main

import (
	"flag"
	"os"
	"strings"
	"time"
	log "github.com/sirupsen/logrus"
)

var modelRouterStallFile=flag.String("model-router-stall-file","","disposable native logging fixture")

type modelRouterStallHook struct{}

func (modelRouterStallHook) Levels() []log.Level {return []log.Level{log.InfoLevel}}

func (modelRouterStallHook) Fire(entry *log.Entry) error {
	path:=*modelRouterStallFile
	if path=="" || !strings.HasPrefix(entry.Message,"model_router.stream.complete ") {return nil}
	if err:=os.WriteFile(path+".entered",[]byte("host.log"),0600);err!=nil {return err}
	for {
		if _,err:=os.Stat(path);err==nil {return nil} else if !os.IsNotExist(err) {return err}
		time.Sleep(10*time.Millisecond)
	}
}

func init() {log.AddHook(modelRouterStallHook{})}
`

func smokeModelListed(t *testing.T, baseURL, model string) bool {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-test-key")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, item := range body.Data {
		if item.ID == model {
			return true
		}
	}
	return false
}

func waitForSmokeModel(t *testing.T, baseURL, model string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if smokeModelListed(t, baseURL, model) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func postSmokeChat(t *testing.T, baseURL, model string) map[string]any {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
		"stream": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-test-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode status %s response: %v", response.Status, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %s; body = %#v", response.Status, body)
	}
	return body
}

func postSmokeChatResponse(t *testing.T, baseURL, model string, stream bool) (int, string) {
	t.Helper()
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
		"stream": stream,
	}
	if stream {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-test-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("send %s chat request: %v", model, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s chat response: %v", model, err)
	}
	return response.StatusCode, string(responseBody)
}

func sharedLibraryExtension() string {
	switch runtime.GOOS {
	case "darwin":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}
