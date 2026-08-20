package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseUsagePayloadProtocols(t *testing.T) {
	tests := []struct {
		name string
		body string
		want pluginapi.UsageDetail
		tier string
	}{
		{
			name: "openai chat",
			body: `{"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}},"service_tier":"priority"}`,
			want: pluginapi.UsageDetail{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 2, CachedTokens: 3, CacheReadTokens: 3, TotalTokens: 18},
			tier: "priority",
		},
		{
			name: "openai responses",
			body: `{"response":{"usage":{"input_tokens":5,"output_tokens":4,"total_tokens":9,"input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"reasoning_tokens":2}}}}`,
			want: pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 4, ReasoningTokens: 2, CachedTokens: 1, CacheReadTokens: 1, TotalTokens: 9},
		},
		{
			name: "claude",
			body: `{"usage":{"input_tokens":5,"output_tokens":4,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`,
			want: pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 4, CachedTokens: 3, CacheReadTokens: 3, CacheCreationTokens: 2, TotalTokens: 14},
		},
		{
			name: "gemini",
			body: `{"usageMetadata":{"promptTokenCount":5,"toolUsePromptTokenCount":2,"candidatesTokenCount":4,"thoughtsTokenCount":3,"cachedContentTokenCount":1,"totalTokenCount":14}}`,
			want: pluginapi.UsageDetail{InputTokens: 7, OutputTokens: 4, ReasoningTokens: 3, CachedTokens: 1, CacheReadTokens: 1, TotalTokens: 14},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, tier, ok := parseUsagePayload([]byte(test.body))
			if !ok || got != test.want || tier != test.tier {
				t.Fatalf("parseUsagePayload() = %#v, %q, %v; want %#v, %q, true", got, tier, ok, test.want, test.tier)
			}
		})
	}
}

func TestUsageCaptureParsesSplitStream(t *testing.T) {
	start := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	capture := directUsageCapture{requestedAt: start, generate: true}
	capture.observeStream(pluginapi.StreamChunkInterceptRequest{ChunkIndex: 0, Body: []byte(`data: {"response":{"usage":{"input_tokens":8,`)}, start.Add(100*time.Millisecond))
	capture.observeStream(pluginapi.StreamChunkInterceptRequest{ChunkIndex: 1, Body: []byte(`"output_tokens":2,"total_tokens":10}}}` + "\n\n")}, start.Add(150*time.Millisecond))
	capture.complete(pluginapi.RequestCompletion{Outcome: pluginapi.RequestCompletionSucceeded, StartedAt: start, CompletedAt: start.Add(time.Second), StatusCode: http.StatusOK})
	if capture.detail.InputTokens != 8 || capture.detail.OutputTokens != 2 || capture.detail.TotalTokens != 10 {
		t.Fatalf("stream detail = %#v", capture.detail)
	}
	if got := durationBetween(capture.requestedAt, capture.firstTokenAt); got != 100*time.Millisecond {
		t.Fatalf("TTFT = %v", got)
	}
}

func TestDirectUsageFallbackSuppressesLateOfficialRecord(t *testing.T) {
	plugin := testUsageCapturePlugin(t)
	start := time.Now().UTC()
	before := pluginapi.RequestInterceptRequest{
		RequestID: "direct-1", SourceFormat: "openai", Model: "work/working-model", RequestedModel: "work/working-model",
		Headers:  http.Header{"Authorization": {"Bearer local-client-secret"}},
		Metadata: map[string]any{"service_tier": "priority", "generate": true},
	}
	if _, err := plugin.InterceptRequestBeforeAuth(t.Context(), before); err != nil {
		t.Fatal(err)
	}
	after := before
	after.Model = "working-model"
	after.ToFormat = "openai"
	if _, err := plugin.InterceptRequestAfterAuth(t.Context(), after); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InterceptResponse(t.Context(), pluginapi.ResponseInterceptRequest{
		RequestID: "direct-1", Model: "working-model", RequestedModel: "work/working-model", StatusCode: http.StatusOK,
		Body: []byte(`{"model":"working-model","usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := plugin.HandleRequestComplete(t.Context(), pluginapi.RequestCompletion{
		RequestID: "direct-1", Model: "working-model", RequestedModel: "work/working-model", Outcome: pluginapi.RequestCompletionSucceeded,
		StatusCode: http.StatusOK, StartedAt: start, CompletedAt: start.Add(200 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}

	plugin.HandleUsage(t.Context(), pluginapi.UsageRecord{
		Provider: "openai-compatibility", Model: "working-model", Alias: "work/working-model", APIKey: "local-client-secret", RequestedAt: start,
		Detail: pluginapi.UsageDetail{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}, Generate: true,
	})
	page := captureRequestPage(t, plugin)
	if page.Total != 1 || page.Items[0].Attribution != attributionDirect || page.Items[0].RouterModel != "" || page.Items[0].ProviderModel != "working-model" || page.Items[0].TotalTokens != 5 {
		t.Fatalf("direct fallback page = %#v", page)
	}
	if page.Items[0].MaskedAPIKey != "lo******et" || page.Items[0].Source != "work" {
		t.Fatalf("direct fallback sensitive dimensions = %#v", page.Items[0])
	}
}

func TestOfficialUsageSuppressesDirectFallback(t *testing.T) {
	plugin := testUsageCapturePlugin(t)
	start := time.Now().UTC()
	request := pluginapi.RequestInterceptRequest{
		RequestID: "direct-official", Model: "work/working-model", RequestedModel: "work/working-model",
		Headers: http.Header{"Authorization": {"Bearer local-client-secret"}},
	}
	_, _ = plugin.InterceptRequestBeforeAuth(t.Context(), request)
	plugin.HandleUsage(t.Context(), pluginapi.UsageRecord{
		Provider: "official-provider", Model: "work/working-model", APIKey: "local-client-secret", RequestedAt: start,
		Detail: pluginapi.UsageDetail{InputTokens: 9, TotalTokens: 9}, Generate: true,
	})
	_, _ = plugin.InterceptResponse(t.Context(), pluginapi.ResponseInterceptRequest{RequestID: request.RequestID, StatusCode: http.StatusOK, Body: []byte(`{"usage":{"prompt_tokens":1,"total_tokens":1}}`)})
	_ = plugin.HandleRequestComplete(t.Context(), pluginapi.RequestCompletion{RequestID: request.RequestID, Outcome: pluginapi.RequestCompletionSucceeded, StartedAt: start, CompletedAt: start.Add(time.Second)})
	page := captureRequestPage(t, plugin)
	if page.Total != 1 || page.Items[0].Provider != "official-provider" || page.Items[0].TotalTokens != 9 {
		t.Fatalf("official usage page = %#v", page)
	}
}

func TestRoutedFallbackRecordsEachAttemptAndSuppressesLateUsage(t *testing.T) {
	plugin := testUsageCapturePlugin(t, modelRoute{Alias: "smart", Strategy: routeStrategyPriority, CooldownSeconds: 30, Models: []string{"fail/fail-model", "work/working-model"}})
	host := &fakeModelHost{execute: func(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		if request.Model == "fail/fail-model" {
			return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"error":"rate limit"}`)}, nil
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"model":"working-model","usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}}
	request := pluginapi.ExecutorRequest{Model: "smart", SourceFormat: "openai", Headers: http.Header{"Authorization": {"Bearer local-client-secret"}}}
	if _, err := plugin.executeWithHost(request, host); err != nil {
		t.Fatal(err)
	}
	plugin.HandleUsage(t.Context(), pluginapi.UsageRecord{Model: "fail-model", Alias: "fail/fail-model", APIKey: "local-client-secret", RequestedAt: time.Now().UTC(), Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}})
	plugin.HandleUsage(t.Context(), pluginapi.UsageRecord{Model: "working-model", Alias: "work/working-model", APIKey: "local-client-secret", RequestedAt: time.Now().UTC(), Detail: pluginapi.UsageDetail{TotalTokens: 5}})
	page := captureRequestPage(t, plugin)
	if page.Total != 2 {
		t.Fatalf("routed attempts = %#v", page)
	}
	failed, succeeded := false, false
	for _, item := range page.Items {
		if item.Attribution != attributionRouted || item.RouterModel != "smart" {
			t.Fatalf("routed attribution = %#v", item)
		}
		failed = failed || item.ProviderModel == "fail-model" && item.Failed && item.StatusCode == http.StatusTooManyRequests
		succeeded = succeeded || item.ProviderModel == "working-model" && !item.Failed && item.TotalTokens == 5
	}
	if !failed || !succeeded {
		t.Fatalf("routed attempts = %#v", page.Items)
	}
}

func testUsageCapturePlugin(t *testing.T, routes ...modelRoute) *modelRouterPlugin {
	t.Helper()
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime := newRouteRuntime(nil)
	runtime.Sync(routes)
	return &modelRouterPlugin{
		config: routerConfig{Enabled: true, Routes: routes}, runtime: runtime, store: store, attribution: newAttributionTracker(nil),
	}
}

func captureRequestPage(t *testing.T, plugin *modelRouterPlugin) usageRequestPage {
	t.Helper()
	page, err := plugin.store.Requests(usageFilter{From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC().Add(time.Hour)}, "time", "asc", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return page
}
