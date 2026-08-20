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

	tracker.MarkRouted("auto", "openai/gpt-5.4(high)", headers)
	routed := tracker.Match(pluginapi.UsageRecord{Model: "openai/gpt-5.4", Alias: "gpt-5.4(high)", APIKey: "sk-client-ab", RequestedAt: now})
	if routed.Kind != attributionRouted || routed.RouterModel != "auto" {
		t.Fatalf("routed attribution = %#v", routed)
	}

	tracker.MarkDirect(pluginapi.ModelRouteRequest{RequestedModel: "gpt-5.4", Headers: headers})
	direct := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "sk-client-ab", RequestedAt: now})
	if direct.Kind != attributionDirect || direct.RouterModel != "" {
		t.Fatalf("direct attribution = %#v", direct)
	}

	tracker.MarkRouted("auto", "gpt-5.4", headers)
	tracker.MarkDirect(pluginapi.ModelRouteRequest{RequestedModel: "gpt-5.4", Headers: headers})
	conflict := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "sk-client-ab", RequestedAt: now})
	if conflict.Kind != attributionUnresolved {
		t.Fatalf("conflicting attribution = %#v", conflict)
	}
}

func TestAttributionTrackerRequiresCredentialAndUnanimousRoute(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	tracker.MarkRouted("auto", "gpt-5.4", http.Header{"X-Api-Key": {"key-one"}})
	if got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "key-two", RequestedAt: now}); got.Kind != attributionUnresolved {
		t.Fatalf("credential mismatch = %#v", got)
	}
	tracker.MarkRouted("auto", "gpt-5.4", nil)
	tracker.MarkRouted("cheap", "gpt-5.4", nil)
	if got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", RequestedAt: now}); got.Kind != attributionUnresolved {
		t.Fatalf("route disagreement = %#v", got)
	}
}

func TestAttributionTrackerMatchesTimestampLessUsageAfterWindow(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	tracker.MarkRouted("auto", "gpt-5.4", http.Header{"X-Api-Key": {"key-one"}})
	now = now.Add(attributionWindow + time.Second)
	got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "key-one"})
	if got.Kind != attributionRouted || got.RouterModel != "auto" {
		t.Fatalf("timestamp-less attribution = %#v", got)
	}
}

func TestAttributionTrackerTimestampLessUsagePrefersFallbackTombstone(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	headers := http.Header{"X-Api-Key": {"key-one"}}
	first := tracker.MarkRouted("first", "gpt-5.4", headers)
	if _, claimed := tracker.claim(first); !claimed {
		t.Fatal("first marker was not converted to a fallback tombstone")
	}
	now = now.Add(2 * time.Second)
	tracker.MarkRouted("second", "gpt-5.4", headers)
	if got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "key-one"}); !got.Suppress {
		t.Fatalf("timestamp-less fallback match = %#v, want suppression", got)
	}
	if len(tracker.markers) != 1 || tracker.markers[0].fallback || tracker.markers[0].routerModel != "second" {
		t.Fatalf("active marker after fallback suppression = %#v", tracker.markers)
	}
	if got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "key-one"}); got.Kind != attributionRouted || got.RouterModel != "second" {
		t.Fatalf("active marker after timestamp-less fallback = %#v", got)
	}
}

func TestAttributionTrackerDoesNotGuessAmongActiveTimestampLessUsage(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	headers := http.Header{"X-Api-Key": {"key-one"}}
	tracker.MarkRouted("first", "gpt-5.4", headers)
	now = now.Add(time.Second)
	tracker.MarkRouted("second", "gpt-5.4", headers)
	if got := tracker.Match(pluginapi.UsageRecord{Model: "gpt-5.4", APIKey: "key-one"}); got.Kind != attributionUnresolved {
		t.Fatalf("ambiguous timestamp-less attribution = %#v", got)
	}
	if len(tracker.markers) != 2 || tracker.markers[0].fallback || tracker.markers[1].fallback {
		t.Fatalf("active markers after ambiguous match = %#v", tracker.markers)
	}
}

func TestAttributionTrackerExpiresAndCapsMarkers(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker := newAttributionTracker(func() time.Time { return now })
	tracker.MarkRouted("old", "model", nil)
	now = now.Add(attributionRetention + time.Second)
	tracker.MarkRouted("new", "model", nil)
	if len(tracker.markers) != 1 || tracker.markers[0].routerModel != "new" {
		t.Fatalf("expired markers = %#v", tracker.markers)
	}
	for index := 0; index < maxAttributionMarks+10; index++ {
		tracker.MarkDirect(pluginapi.ModelRouteRequest{RequestedModel: "model"})
	}
	if len(tracker.markers) != maxAttributionMarks {
		t.Fatalf("marker count = %d", len(tracker.markers))
	}
}

func TestMaskAPIKey(t *testing.T) {
	for input, want := range map[string]string{"sk-client-ab": "sk******ab", "abcde": "******", "abcd": "******", "": ""} {
		if got := maskAPIKey(input); got != want {
			t.Fatalf("maskAPIKey(%q) = %q, want %q", input, got, want)
		}
	}
}
