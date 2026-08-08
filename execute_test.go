package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeModelHost struct {
	execute      func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error)
	start        func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error)
	reads        map[string][]pluginapi.HostModelStreamReadResponse
	executeCalls []pluginapi.HostModelExecutionRequest
	startCalls   []pluginapi.HostModelExecutionRequest
	emitted      [][]byte
	closed       []string
}

func (host *fakeModelHost) Execute(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	host.executeCalls = append(host.executeCalls, request)
	if host.execute == nil {
		return pluginapi.HostModelExecutionResponse{}, errors.New("unexpected Execute call")
	}
	return host.execute(request)
}

func (host *fakeModelHost) StartStream(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
	host.startCalls = append(host.startCalls, request)
	if host.start == nil {
		return pluginapi.HostModelStreamResponse{}, errors.New("unexpected StartStream call")
	}
	return host.start(request)
}

func (host *fakeModelHost) ReadStream(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
	responses := host.reads[streamID]
	if len(responses) == 0 {
		return pluginapi.HostModelStreamReadResponse{}, errors.New("unexpected ReadStream call")
	}
	response := responses[0]
	host.reads[streamID] = responses[1:]
	return response, nil
}

func (host *fakeModelHost) CloseStream(streamID string) error {
	host.closed = append(host.closed, streamID)
	return nil
}

func (host *fakeModelHost) Emit(_ string, payload []byte) error {
	host.emitted = append(host.emitted, append([]byte(nil), payload...))
	return nil
}

func (*fakeModelHost) ClosePluginStream(string, string) {}

func TestExecuteWithHostFailsOverAndRewritesAlias(t *testing.T) {
	plugin := testRouterPlugin(modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"provider-a", "provider-b"}})
	host := &fakeModelHost{}
	host.execute = func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		if request.Model == "provider-a(high)" {
			return pluginapi.HostModelExecutionResponse{StatusCode: 429, Body: []byte(`{"error":"rate limit"}`)}, nil
		}
		return pluginapi.HostModelExecutionResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"X-Upstream": []string{"provider-b"}},
			Body:       []byte(`{"id":"result","model":"provider-b(high)"}`),
		}, nil
	}
	request := pluginapi.ExecutorRequest{
		Model:           "smart(high)",
		SourceFormat:    "openai",
		OriginalRequest: []byte(`{"model":"smart(high)","messages":[]}`),
		Headers:         http.Header{"Authorization": []string{"Bearer client-secret"}, "X-Trace": []string{"trace-id"}},
		Query:           url.Values{"alt": []string{"json"}},
	}
	response, err := plugin.executeWithHost(request, host)
	if err != nil {
		t.Fatalf("executeWithHost() error = %v", err)
	}
	if len(host.executeCalls) != 2 {
		t.Fatalf("Execute calls = %d, want 2", len(host.executeCalls))
	}
	for index, call := range host.executeCalls {
		if call.Headers.Get("Authorization") != "" || call.Headers.Get("X-Trace") != "trace-id" {
			t.Fatalf("call %d headers = %#v", index, call.Headers)
		}
		var body map[string]any
		if err := json.Unmarshal(call.Body, &body); err != nil {
			t.Fatalf("call %d body error = %v", index, err)
		}
		if body["model"] != call.Model {
			t.Fatalf("call %d body model = %v, request model = %q", index, body["model"], call.Model)
		}
	}
	if model := jsonModel(t, response.Payload); model != "smart(high)" {
		t.Fatalf("response model = %q", model)
	}
	if response.Headers.Get("X-Upstream") != "provider-b" || response.Metadata["selected_model"] != "provider-b(high)" {
		t.Fatalf("response = %#v", response)
	}
}

func TestExecuteWithHostDoesNotFailOverTerminalError(t *testing.T) {
	plugin := testRouterPlugin(modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"a", "b"}})
	host := &fakeModelHost{execute: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: 400, Body: []byte(`{"error":"invalid request"}`)}, nil
	}}
	_, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host)
	if statusFromError(err) != 400 || len(host.executeCalls) != 1 {
		t.Fatalf("executeWithHost() error = %v, calls = %d", err, len(host.executeCalls))
	}
}

func TestExecuteWithHostReturnsUnavailableAfterCandidatesFail(t *testing.T) {
	plugin := testRouterPlugin(modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"a", "b"}})
	host := &fakeModelHost{execute: func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, statusError{status: 503, message: "provider unavailable"}
	}}
	_, err := plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host)
	if statusFromError(err) != 503 || codeFromError(err, "") != "model_route_unavailable" || len(host.executeCalls) != 2 {
		t.Fatalf("executeWithHost() error = %v, calls = %d", err, len(host.executeCalls))
	}
	_, err = plugin.executeWithHost(pluginapi.ExecutorRequest{Model: "smart"}, host)
	if statusFromError(err) != 429 || codeFromError(err, "") != "model_route_cooldown" {
		t.Fatalf("cooldown error = %v", err)
	}
}

func TestCountTokensIsExplicitlyUnsupported(t *testing.T) {
	plugin := testRouterPlugin(modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"a"}})
	_, err := plugin.CountTokens(t.Context(), pluginapi.ExecutorRequest{Model: "smart"})
	if statusFromError(err) != 501 || codeFromError(err, "") != "model_route_count_tokens_unsupported" {
		t.Fatalf("CountTokens() error = %v", err)
	}
}

func TestSanitizeNestedHeaders(t *testing.T) {
	headers := sanitizeNestedHeaders(http.Header{
		"Authorization":    []string{"secret"},
		"X-Api-Key":        []string{"secret"},
		"X-Custom-Token":   []string{"secret"},
		"Proxy-Connection": []string{"keep-alive"},
		"X-Trace":          []string{"trace-id"},
	})
	if headers.Get("Authorization") != "" || headers.Get("X-Api-Key") != "" || headers.Get("X-Custom-Token") != "" {
		t.Fatalf("sensitive headers survived: %#v", headers)
	}
	if headers.Get("X-Trace") != "trace-id" {
		t.Fatalf("benign header missing: %#v", headers)
	}
}

func jsonModel(t *testing.T, raw []byte) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode JSON: %v; body = %s", err, raw)
	}
	model, _ := body["model"].(string)
	return strings.TrimSpace(model)
}
