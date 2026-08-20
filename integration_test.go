//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer provider.Close()

	workDir := t.TempDir()
	pluginsDir := filepath.Join(workDir, "plugins")
	usagePath := filepath.Join(workDir, "data", "model-router-usage.db")
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
      data_path: %q
      retention_days: 45
      routes:
        - alias: smoke-router-alias
          strategy: priority
          cooldown_seconds: 60
          models:
            - fail/fail-model
            - work/working-model
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
`, port, filepath.Join(workDir, "auth"), pluginsDir, usagePath, provider.URL+"/v1", provider.URL+"/v1")
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

	page := waitForSmokeUsage(t, baseURL, usageStart, 6, 10*time.Second)
	routedModel := ""
	foundDirect := false
	routedSuccesses, routedFailures, directSuccesses := 0, 0, 0
	totalTokens := uint64(0)
	for _, item := range page.Items {
		totalTokens += item.TotalTokens
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
	if page.Total != 6 || routedSuccesses != 3 || routedFailures != 1 || directSuccesses != 2 || totalTokens != 12 {
		t.Fatalf("usage records were missing or duplicated: total=%d routed_success=%d routed_failure=%d direct_success=%d tokens=%d items=%#v\n%s", page.Total, routedSuccesses, routedFailures, directSuccesses, totalTokens, page.Items, logs.String())
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
	savedPreferences := putSmokeManagementJSON[dashboardPreferences, dashboardPreferences](t, baseURL, modelRouterUsageBasePath+"/preferences", preferences)
	if savedPreferences.RequestPageSize != 25 {
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
	if restartedPreferences.RequestPageSize != 25 {
		t.Fatalf("preferences were lost across restart: %#v", restartedPreferences)
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
	for _, expected := range []string{"/v0/management/api-keys", "/v1/models", "<select data-target-field=\"model\""} {
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
	for _, expected := range []string{"fail/fail-model", "work/working-model"} {
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
		lastErr = requestSmokeManagementJSON(baseURL, modelRouterUsageBasePath+"/requests?"+query.Encode(), &lastPage)
		if lastErr == nil {
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

func startSmokeCPA(t *testing.T, binary, configPath, baseURL string) *smokeCPAProcess {
	t.Helper()
	logs := &smokeSyncBuffer{}
	command := exec.Command(binary, "--config", configPath)
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
