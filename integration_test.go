//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
			Model string `json:"model"`
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
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
plugins:
  enabled: true
  dir: %q
  configs:
    model-router:
      enabled: true
      priority: 100
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
`, port, filepath.Join(workDir, "auth"), pluginsDir, provider.URL+"/v1", provider.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	logs := startSmokeCPA(t, cpaBinary, configPath, baseURL)
	if !waitForSmokeModel(t, baseURL, "smoke-router-alias", 10*time.Second) {
		t.Fatalf("model route alias smoke-router-alias was not registered\n%s", logs.String())
	}
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

func startSmokeCPA(t *testing.T, binary, configPath, baseURL string) *smokeSyncBuffer {
	t.Helper()
	logs := &smokeSyncBuffer{}
	command := exec.Command(binary, "--config", configPath)
	command.Stdout = logs
	command.Stderr = logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
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
				return logs
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("CLIProxyAPI did not become ready at %s\n%s", baseURL, logs.String())
	return logs
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
