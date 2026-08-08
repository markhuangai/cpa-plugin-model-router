package main

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExecuteStreamFailsOverBeforePayload(t *testing.T) {
	plugin := testRouterPlugin(modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"provider-a", "provider-b"}})
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
	plugin := testRouterPlugin(modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"provider-a", "provider-b"}})
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

func TestStreamModelRewriterSeparatesGluedSSEEvents(t *testing.T) {
	rewriter := newStreamModelRewriter("smart")
	payload := rewriter.Rewrite([]byte(`data: {"model":"one"}data: {"model":"two"}`))
	if strings.Count(string(payload), `"model":"smart"`) != 2 || strings.Contains(string(payload), `}data:`) {
		t.Fatalf("Rewrite() = %q", payload)
	}
}
