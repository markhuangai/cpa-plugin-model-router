package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAttributionTrackerRoutedDirectAndConflict(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	headers := http.Header{"Authorization": {"Bearer sk-client-ab"}}

	tracker.MarkRouted("auto", "openai/gpt-5.4(high)", headers, 1, "")
	routed := tracker.Match(pluginapi.UsageRecord{Model: "openai/gpt-5.4", Alias: "gpt-5.4(high)", APIKey: "sk-client-ab", RequestedAt: now})
	if routed.Kind != attributionRouted || routed.RouterModel != "auto" {
		t.Fatalf("routed attribution = %#v", routed)
	}

	now = now.Add(time.Millisecond)
	tracker.MarkDirect(pluginapi.ModelRouteRequest{RequestedModel: "gpt-5.4", Headers: headers})
	direct := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "sk-client-ab", RequestedAt: now})
	if direct.Kind != attributionDirect || direct.RouterModel != "" {
		t.Fatalf("direct attribution = %#v", direct)
	}

	tracker.MarkRouted("auto", "gpt-5.4", headers, 1, "")
	tracker.MarkDirect(pluginapi.ModelRouteRequest{RequestedModel: "gpt-5.4", Headers: headers})
	conflict := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "sk-client-ab", RequestedAt: now})
	if conflict.Kind != attributionUnresolved {
		t.Fatalf("conflicting attribution = %#v", conflict)
	}
}

func TestAttributionTrackerRequiresCredentialAndUnanimousRoute(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	tracker.MarkRouted("auto", "gpt-5.4", http.Header{"X-Api-Key": {"key-one"}}, 1, "")
	if got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "key-two", RequestedAt: now}); got.Kind != attributionUnresolved {
		t.Fatalf("credential mismatch = %#v", got)
	}
	tracker.MarkRouted("auto", "gpt-5.4", nil, 1, "")
	tracker.MarkRouted("cheap", "gpt-5.4", nil, 1, "")
	if got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", RequestedAt: now}); got.Kind != attributionUnresolved {
		t.Fatalf("route disagreement = %#v", got)
	}
}

func TestAttributionTrackerMatchesTimestampLessUsageAfterWindow(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	tracker.MarkRouted("auto", "gpt-5.4", http.Header{"X-Api-Key": {"key-one"}}, 1, "")
	now = now.Add(attributionWindow + time.Second)
	got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "key-one"})
	if got.Kind != attributionRouted || got.RouterModel != "auto" {
		t.Fatalf("timestamp-less attribution = %#v", got)
	}
}

func TestAttributionTrackerLeavesAmbiguousTimestampLessUsageUnattributed(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	headers := http.Header{"X-Api-Key": {"key-one"}}
	tracker.MarkRouted("first", "gpt-5.4", headers, 1, "")
	now = now.Add(time.Second)
	tracker.MarkRouted("second", "gpt-5.4", headers, 1, "")
	if got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "key-one"}); got.Kind != attributionUnresolved {
		t.Fatalf("ambiguous timestamp-less attribution = %#v", got)
	}
	if len(tracker.markers) != 2 {
		t.Fatalf("active markers after ambiguous match = %#v", tracker.markers)
	}
}

func TestAttributionTrackerExpiresAndCapsMarkers(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	tracker.MarkRouted("old", "model", nil, 1, "")
	now = now.Add(attributionRetention + time.Second)
	newID := tracker.MarkRouted("new", "model", nil, 1, "")
	if marker := tracker.markers[newID]; len(tracker.markers) != 1 || marker == nil || marker.routerModel != "new" {
		t.Fatalf("expired markers = %#v", tracker.markers)
	}
	for index := 0; index < maxAttributionMarks+10; index++ {
		tracker.MarkDirect(pluginapi.ModelRouteRequest{RequestedModel: "model"})
	}
	if len(tracker.markers) != maxAttributionMarks {
		t.Fatalf("marker count = %d", len(tracker.markers))
	}
}

func TestAttributionDispatchIntervals(t *testing.T) {
	now := time.Now().UTC()
	tracker := newAttributionTracker(func() time.Time { return now })
	headers := http.Header{"X-Api-Key": {"client-test"}}
	old := tracker.MarkRouted("earlier", "west/chat", headers, 2, "earlier")
	now = now.Add(time.Millisecond)
	tracker.FinishRouted(old)

	now = now.Add(time.Millisecond)
	current := tracker.MarkRouted("auto", "west/chat", headers, 1, "fresh")
	now = now.Add(2 * attributionWindow)
	record := pluginapi.UsageRecord{Model: "upstream/gpt", Alias: "west/chat", APIKey: "client-test", RequestedAt: now}
	now = now.Add(time.Millisecond)
	tracker.FinishRouted(current)
	now = now.Add(time.Minute)
	match := tracker.Match(record)
	if match.RouterModel != "auto" || match.RouteAttempt != 1 || match.Operation != "fresh" || match.Target != "west/chat" {
		t.Fatalf("delayed usage matched the wrong dispatch: %#v", match)
	}
	record.RequestedAt = record.RequestedAt.Add(-time.Millisecond)
	record.Model = "upstream/another-model"
	if match := tracker.Match(record); match.RouterModel != "auto" || match.RouteAttempt != 1 || match.Operation != "fresh" {
		t.Fatalf("second official record from the dispatch = %#v", match)
	}

	tracker.MarkRouted("auto", "west/chat", headers, 2, "fallback")
	now = now.Add(time.Millisecond)
	tracker.MarkRouted("auto", "west/chat", headers, 1, "another request")
	record.RequestedAt = now.Add(time.Millisecond)
	if match := tracker.Match(record); match.Kind != attributionUnresolved || match.RouteAttempt != 0 || match.Reason != "conflicting_markers" {
		t.Fatalf("overlapping first attempt and retry matched: %#v", match)
	}
}

func TestAttributionDirectKeepsRequestedNamespace(t *testing.T) {
	now := time.Now().UTC()
	tracker := newAttributionTracker(func() time.Time { return now })
	request := pluginapi.RequestInterceptRequest{RequestID: "test", RequestedModel: "west/chat", Model: "west/chat"}
	tracker.MarkDirectRequest(request)
	request.Model = "gpt"
	tracker.updateDirectRequest(request)
	match := tracker.Match(pluginapi.UsageRecord{Model: "gpt", RequestedAt: now})
	if match.Kind != attributionDirect || match.Target != "west/chat" || match.RouteAttempt != 0 {
		t.Fatalf("direct attribution lost requested spelling: %#v", match)
	}
}

func TestMaskAPIKey(t *testing.T) {
	for input, want := range map[string]string{"sk-client-ab": "sk******ab", "abcde": "******", "abcd": "******", "": ""} {
		if got := maskAPIKey(input); got != want {
			t.Fatalf("maskAPIKey(%q) = %q, want %q", input, got, want)
		}
	}
}
