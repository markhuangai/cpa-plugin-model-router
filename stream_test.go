package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// StartStream may report the upstream status alongside the error. A 400 carried
// that way must move the request to the next target rather than be treated as an
// ambiguous failure.
func TestExecuteStreamFailsOverWhenStartReturnsStatusWithError(t *testing.T) {
	plugin := testRouterPlugin(testModelRoute("smart", routeStrategyPriority, 30, "provider-a", "provider-b"))
	host := &fakeModelHost{reads: map[string][]pluginapi.HostModelStreamReadResponse{
		"stream-b": {
			{Payload: []byte(`data: {"model":"provider-b"}` + "\n\n"), Done: true},
		},
	}}
	host.start = func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
		if request.Model == "provider-a" {
			return pluginapi.HostModelStreamResponse{StatusCode: 400}, errors.New(`{"error":{"message":"reasoning settings conflict","code":"convert_request_failed"}}`)
		}
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "stream-b"}, nil
	}
	err := plugin.executeStreamWithHost(context.Background(), pluginapi.ExecutorRequest{Model: "smart", SourceFormat: "openai"}, "plugin-stream", host)
	if err != nil {
		t.Fatalf("executeStreamWithHost() error = %v", err)
	}
	if len(host.startCalls) != 2 {
		t.Fatalf("a 400 reported with the error must fail over, start calls = %d", len(host.startCalls))
	}
}

func TestExecuteStreamFailsOverBeforePayload(t *testing.T) {
	plugin := testRouterPlugin(testModelRoute("smart", routeStrategyPriority, 30, "provider-a", "provider-b"))
	host := &fakeModelHost{reads: map[string][]pluginapi.HostModelStreamReadResponse{
		"stream-b": {
			{Payload: []byte(`data: {"model":"provider-b",`), Done: false},
			{Payload: []byte(`"choices":[]}` + "\n\n"), Done: true},
		},
	}}
	host.start = func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
		if request.Model == "provider-a" {
			return pluginapi.HostModelStreamResponse{StatusCode: 429}, nil
		}
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "stream-b"}, nil
	}
	err := plugin.executeStreamWithHost(context.Background(), pluginapi.ExecutorRequest{Model: "smart", SourceFormat: "openai"}, "plugin-stream", host)
	if err != nil {
		t.Fatalf("executeStreamWithHost() error = %v", err)
	}
	if len(host.startCalls) != 2 || len(host.emitted) != 1 {
		t.Fatalf("start calls = %d, emitted = %d", len(host.startCalls), len(host.emitted))
	}
	if payload := string(host.emitted[0]); !strings.Contains(payload, `"model":"smart"`) || strings.Contains(payload, "provider-b") {
		t.Fatalf("emitted payload = %q", payload)
	}
}

func TestExecuteStreamDoesNotRetryAfterPayload(t *testing.T) {
	plugin := testRouterPlugin(testModelRoute("smart", routeStrategyPriority, 30, "provider-a", "provider-b"))
	host := &fakeModelHost{reads: map[string][]pluginapi.HostModelStreamReadResponse{
		"stream-a": {
			{Payload: []byte(`data: {"model":"provider-a"}` + "\n\n")},
			{Error: "status 429: late rate limit", Done: true},
		},
	}}
	host.start = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
		return pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "stream-a"}, nil
	}
	err := plugin.executeStreamWithHost(context.Background(), pluginapi.ExecutorRequest{Model: "smart"}, "plugin-stream", host)
	if statusFromError(err) != 429 || len(host.startCalls) != 1 || len(host.emitted) != 1 {
		t.Fatalf("error = %v, start calls = %d, emitted = %d", err, len(host.startCalls), len(host.emitted))
	}
}

func TestExecuteStreamAttemptsWeightedTargetOncePerRequest(t *testing.T) {
	route := modelRoute{
		Alias:           "weighted",
		Strategy:        routeStrategyRoundRobin,
		CooldownSeconds: 30,
		Targets: []modelTarget{
			{Model: "a", Weight: 3},
			{Model: "b", Weight: 1},
		},
	}
	plugin := testRouterPlugin(route)
	host := &fakeModelHost{}
	host.start = func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
		return pluginapi.HostModelStreamResponse{StatusCode: 429}, statusError{status: 429, message: request.Model + " unavailable"}
	}
	err := plugin.executeStreamWithHost(context.Background(), pluginapi.ExecutorRequest{Model: "weighted"}, "plugin-stream", host)
	if statusFromError(err) != 503 || codeFromError(err, "") != "model_route_unavailable" {
		t.Fatalf("executeStreamWithHost() error = %v", err)
	}
	if len(host.startCalls) != 2 || host.startCalls[0].Model != "a" || host.startCalls[1].Model != "b" {
		t.Fatalf("weighted stream calls = %#v, want a then b once", host.startCalls)
	}
}

func TestStreamModelRewriterBuffersSplitSSE(t *testing.T) {
	rewriter := newStreamModelRewriter("smart")
	if first := rewriter.Rewrite([]byte(`data: {"model":"physical",`)); first != nil {
		t.Fatalf("first Rewrite() = %q", first)
	}
	second := rewriter.Rewrite([]byte(`"choices":[]}` + "\n\n"))
	if payload := string(second); !strings.Contains(payload, `"model":"smart"`) || strings.Contains(payload, "physical") {
		t.Fatalf("second Rewrite() = %q", payload)
	}
	if tail := rewriter.Finish(); tail != nil {
		t.Fatalf("Finish() = %q", tail)
	}
}

func TestStreamModelRewriterSeparatesResponsesEventAndDataChunks(t *testing.T) {
	rewriter := newStreamModelRewriter("responses-router-alias")
	if first := rewriter.Rewrite([]byte("event: response.created")); first != nil {
		t.Fatalf("first Rewrite() = %q", first)
	}
	second := rewriter.Rewrite([]byte(`data: {"type":"response.created","response":{"model":"physical-model"}}`))
	payload := string(second)
	if !strings.HasPrefix(payload, "event: response.created\ndata: ") {
		t.Fatalf("second Rewrite() did not preserve the SSE line boundary: %q", payload)
	}
	if !strings.Contains(payload, `"model":"responses-router-alias"`) || strings.Contains(payload, "physical-model") {
		t.Fatalf("second Rewrite() did not rewrite response.model: %q", payload)
	}
	if tail := rewriter.Finish(); tail != nil {
		t.Fatalf("Finish() = %q", tail)
	}
}

func TestStreamModelRewriterPreservesResponsesEventDataPairs(t *testing.T) {
	rewriter := newStreamModelRewriter("responses-router-alias")
	chunks := []string{
		"event: response.created",
		`data: {"type":"response.created","response":{"model":"physical-model"}}`,
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"OK"}`,
		"event: response.completed",
		`data: {"type":"response.completed","response":{"model":"physical-model"}}`,
	}
	var outputs []string
	for _, chunk := range chunks {
		if rewritten := rewriter.Rewrite([]byte(chunk)); len(rewritten) > 0 {
			outputs = append(outputs, string(rewritten))
		}
	}
	if tail := rewriter.Finish(); len(tail) > 0 {
		outputs = append(outputs, string(tail))
	}
	wantEvents := []string{"response.created", "response.output_text.delta", "response.completed"}
	if len(outputs) != len(wantEvents) {
		t.Fatalf("rewritten outputs = %#v, want %d event/data pairs", outputs, len(wantEvents))
	}
	for index, event := range wantEvents {
		if !strings.HasPrefix(outputs[index], "event: "+event+"\ndata: ") {
			t.Fatalf("rewritten pair %d = %q", index, outputs[index])
		}
	}
	combined := strings.Join(outputs, "\n\n")
	if !strings.Contains(combined, "response.completed") || strings.Count(combined, `"model":"responses-router-alias"`) != 2 || strings.Contains(combined, "physical-model") {
		t.Fatalf("rewritten stream = %q", combined)
	}
}

func TestStreamModelRewriterSeparatesGluedSSEEvents(t *testing.T) {
	rewriter := newStreamModelRewriter("smart")
	payload := rewriter.Rewrite([]byte(`data: {"model":"one"}data: {"model":"two"}`))
	if strings.Count(string(payload), `"model":"smart"`) != 2 || strings.Contains(string(payload), `}data:`) {
		t.Fatalf("Rewrite() = %q", payload)
	}
}
