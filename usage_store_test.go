package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUsageStorePersistsAndResetPreservesSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := openUsageStore(path, 365)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	plugin := &modelRouterPlugin{config: routerConfig{Enabled: true}, store: store, attribution: newAttributionTracker(func() time.Time { return now })}
	plugin.attribution.MarkRouted("auto", "gpt-5.4", mapHeader("Authorization", "Bearer raw-client-secret"))
	plugin.HandleUsage(t.Context(), pluginapi.UsageRecord{
		Provider: "openai", ExecutorType: "openai", Model: "gpt-5.4", APIKey: "raw-client-secret", AuthType: "api-key", Source: "raw-provider-secret", RequestedAt: now,
		Latency: 2 * time.Second, TTFT: 500 * time.Millisecond, Detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 10, TotalTokens: 120},
	})
	book, err := store.SavePriceBook(saveModelPricesRequest{Prices: map[string]modelPrice{"gpt-5.4": {tokenRates: tokenRates{Input: 1, Output: 2}}}}, now)
	if err != nil || book.Revision != 1 {
		t.Fatalf("save prices = %#v, %v", book, err)
	}
	preferences := defaultDashboardPreferences()
	preferences.RequestPageSize = 50
	preferences.TimeRange = "custom"
	preferences.CustomFrom = "2026-08-18T10:00:00"
	preferences.CustomTo = "2026-08-19T10:00:00"
	preferences.HiddenGroupColumns = []string{"result", "service_tier", "source"}
	if _, err := store.SavePreferences(preferences); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("raw-client-secret")) || bytes.Contains(raw, []byte("raw-provider-secret")) {
		t.Fatal("database persisted a raw API key or provider credential")
	}
	if !bytes.Contains(raw, []byte("ra******et")) {
		t.Fatal("database did not persist the display-only API-key mask")
	}

	store, err = openUsageStore(path, 365)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	page, err := store.Requests(usageFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, "time", "desc", 0, 100)
	if err != nil || page.Total != 1 || page.Items[0].RouterModel != "auto" || page.Items[0].ProviderModel != "gpt-5.4" || page.Items[0].Source != "openai" {
		t.Fatalf("persisted requests = %#v, %v", page, err)
	}
	if loaded, err := store.QueryPriceBook(); err != nil || loaded.Revision != 1 || len(loaded.Prices) != 1 {
		t.Fatalf("persisted prices = %#v, %v", loaded, err)
	}
	if loaded, err := store.QueryPreferences(); err != nil || loaded.RequestPageSize != 50 || loaded.TimeRange != "custom" || loaded.CustomFrom != preferences.CustomFrom || loaded.CustomTo != preferences.CustomTo || !slices.Equal(loaded.HiddenGroupColumns, preferences.HiddenGroupColumns) {
		t.Fatalf("persisted preferences = %#v, %v", loaded, err)
	}
	if err := store.ResetUsage(); err != nil {
		t.Fatal(err)
	}
	page, err = store.Requests(usageFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, "time", "desc", 0, 100)
	if err != nil || page.Total != 0 {
		t.Fatalf("requests after reset = %#v, %v", page, err)
	}
	if loaded, _ := store.QueryPriceBook(); loaded.Revision != 1 || len(loaded.Prices) != 1 {
		t.Fatalf("prices after reset = %#v", loaded)
	}
	if loaded, _ := store.QueryPreferences(); loaded.RequestPageSize != 50 || !slices.Equal(loaded.HiddenGroupColumns, preferences.HiddenGroupColumns) {
		t.Fatalf("preferences after reset = %#v", loaded)
	}
}

func TestDashboardPreferenceColumnDefaults(t *testing.T) {
	want := []string{"provider", "result", "router_model", "service_tier", "source"}
	defaults := defaultDashboardPreferences()
	if defaults.RequestPageSize != 50 || defaults.GroupPageSize != 50 {
		t.Fatalf("default dashboard page sizes = %d/%d, want 50/50", defaults.RequestPageSize, defaults.GroupPageSize)
	}
	if !slices.Equal(defaults.HiddenGroupColumns, want) {
		t.Fatalf("default hidden group columns = %#v, want %#v", defaults.HiddenGroupColumns, want)
	}
	defaults.HiddenGroupColumns[0] = "changed"
	if next := defaultDashboardPreferences(); !slices.Equal(next.HiddenGroupColumns, want) {
		t.Fatalf("default hidden group columns shared mutable storage: %#v", next.HiddenGroupColumns)
	}

	normalized, err := normalizeDashboardPreferences(dashboardPreferences{})
	if err != nil || !slices.Equal(normalized.HiddenGroupColumns, want) {
		t.Fatalf("normalized omitted hidden columns = %#v, %v", normalized.HiddenGroupColumns, err)
	}
	explicit := defaultDashboardPreferences()
	explicit.HiddenGroupColumns = []string{}
	normalized, err = normalizeDashboardPreferences(explicit)
	if err != nil || len(normalized.HiddenGroupColumns) != 0 {
		t.Fatalf("normalized explicit visible columns = %#v, %v", normalized.HiddenGroupColumns, err)
	}
}

func TestSafeStoredUsageSourceRejectsCredentialShapedValues(t *testing.T) {
	for _, source := range []string{"abcdefgh12345678", "abcdefghijklmno", "123456789012345", "AbCd+1234==", "Ab/Cd+1234==", "client:secret-token", "client@secret-token", "client$secret"} {
		record := pluginapi.UsageRecord{Provider: "openai", ExecutorType: "openai", Source: source}
		if got := safeStoredUsageSource(record); got != "openai" {
			t.Errorf("safe source for %q = %q, want provider fallback", source, got)
		}
	}
}

func TestStoredRecordFromUsageSynthesizesMissingTotal(t *testing.T) {
	tests := []struct {
		name   string
		record pluginapi.UsageRecord
		want   uint64
	}{
		{
			name: "anthropic cache counters",
			record: pluginapi.UsageRecord{Provider: "anthropic", Detail: pluginapi.UsageDetail{
				InputTokens: 5, OutputTokens: 4, CacheReadTokens: 3, CacheCreationTokens: 2,
			}},
			want: 14,
		},
		{
			name: "google reasoning",
			record: pluginapi.UsageRecord{Provider: "google", Detail: pluginapi.UsageDetail{
				InputTokens: 7, OutputTokens: 4, ReasoningTokens: 3,
			}},
			want: 14,
		},
		{
			name: "provider default",
			record: pluginapi.UsageRecord{Provider: "openai", Detail: pluginapi.UsageDetail{
				InputTokens: 5, OutputTokens: 4, CachedTokens: 3,
			}},
			want: 9,
		},
		{
			name: "openai creation subset",
			record: pluginapi.UsageRecord{Provider: "openai", Detail: pluginapi.UsageDetail{
				InputTokens: 5, CacheCreationTokens: 2,
			}},
			want: 5,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stored := storedRecordFromUsage(test.record, attributionResult{Kind: attributionDirect})
			if stored.TotalTokens != test.want {
				t.Fatalf("stored total = %d, want %d; record=%#v", stored.TotalTokens, test.want, stored)
			}
		})
	}
}

func TestHandleUsagePersistsAuthoritativeCPARecord(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	plugin := &modelRouterPlugin{
		config:      routerConfig{Enabled: true},
		store:       store,
		attribution: newAttributionTracker(func() time.Time { return now }),
	}
	plugin.attribution.MarkRouted("smart", "provider/model", mapHeader("Authorization", "Bearer client-secret"))
	plugin.HandleUsage(t.Context(), pluginapi.UsageRecord{
		Provider: "openai-compatibility", ExecutorType: "OpenAICompatExecutor", Model: "provider/model", APIKey: "client-secret", RequestedAt: now,
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 1, CachedTokens: 4, CacheCreationTokens: 4, TotalTokens: 11}, Generate: true,
	})
	page, err := store.Requests(usageFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute)}, "time", "asc", 0, 10)
	if err != nil || page.Total != 1 {
		t.Fatalf("authoritative usage page = %#v, %v", page, err)
	}
	item := page.Items[0]
	if item.Attribution != attributionRouted || item.RouterModel != "smart" || item.CacheCreationTokens != 4 || item.EffectiveCacheReadTokens != 0 || item.CacheHit {
		t.Fatalf("authoritative usage item = %#v", item)
	}
}

func TestHandleUsageStoresAmbiguousRecordAsUnattributed(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	tracker := newAttributionTracker(func() time.Time { return now })
	tracker.MarkRouted("first", "provider/model", mapHeader("X-Api-Key", "client-secret"))
	tracker.MarkRouted("second", "provider/model", mapHeader("X-Api-Key", "client-secret"))
	plugin := &modelRouterPlugin{config: routerConfig{Enabled: true}, store: store, attribution: tracker}
	plugin.HandleUsage(t.Context(), pluginapi.UsageRecord{
		Provider: "provider", Model: "provider/model", APIKey: "client-secret", Detail: pluginapi.UsageDetail{TotalTokens: 9}, Generate: true,
	})
	page, err := store.Requests(usageFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute)}, "time", "asc", 0, 10)
	if err != nil || page.Total != 1 || page.Items[0].Attribution != attributionUnresolved || len(tracker.markers) != 2 {
		t.Fatalf("ambiguous usage page = %#v, markers=%#v, err=%v", page, tracker.markers, err)
	}
}

func TestUsageStorePathSwitchDoesNotMigrate(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.db")
	secondPath := filepath.Join(root, "second.db")
	store, err := openUsageStore(firstPath, 365)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Record(storedUsageRecord{RequestedAt: now, Attribution: attributionDirect, ProviderModel: "model", TotalTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Reconfigure(secondPath, 30); err != nil {
		t.Fatal(err)
	}
	page, err := store.Requests(usageFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, "time", "desc", 0, 10)
	if err != nil || page.Total != 0 {
		t.Fatalf("new path requests = %#v, %v", page, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	oldStore, err := openUsageStore(firstPath, 365)
	if err != nil {
		t.Fatal(err)
	}
	defer oldStore.Close()
	page, err = oldStore.Requests(usageFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, "time", "desc", 0, 10)
	if err != nil || page.Total != 1 {
		t.Fatalf("old path requests = %#v, %v", page, err)
	}
}

func TestUsageStoreRelativePathPersists(t *testing.T) {
	root := t.TempDir()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(workingDirectory) })

	store, err := openUsageStore("usage.db", 365)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(storedUsageRecord{RequestedAt: time.Now().UTC(), Attribution: attributionDirect, ProviderModel: "relative", TotalTokens: 1}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openUsageStore("usage.db", 365)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	page, err := reopened.Requests(usageFilter{}, "time", "asc", 0, 10)
	if err != nil || page.Total != 1 || page.Items[0].ProviderModel != "relative" {
		t.Fatalf("relative path requests = %#v, %v", page, err)
	}
}

func TestUsageStoreRestrictsExistingDatabasePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := openUsageStore(path, 365)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err = openUsageStore(path, 365)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("usage database permissions = %o, want 600", got)
	}
}

func TestUsageStorePrunesOldRecordsOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := openUsageStore(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if err := store.Record(storedUsageRecord{RequestedAt: old, Attribution: attributionDirect, ProviderModel: "old", TotalTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openUsageStore(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	page, err := store.Requests(usageFilter{From: old.Add(-time.Hour), To: time.Now().UTC().Add(time.Hour)}, "time", "desc", 0, 10)
	if err != nil || page.Total != 0 {
		t.Fatalf("pruned page = %#v, %v", page, err)
	}
}

func TestUsageOverviewIncludesEfficiencyAverages(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	for _, record := range []storedUsageRecord{
		{RequestedAt: now, Attribution: attributionDirect, ProviderModel: "model", InputTokens: 10, OutputTokens: 10, TotalTokens: 20, LatencyNS: uint64(2 * time.Second), TTFTNS: uint64(500 * time.Millisecond)},
		{RequestedAt: now.Add(time.Minute), Attribution: attributionDirect, ProviderModel: "model", InputTokens: 20, OutputTokens: 15, TotalTokens: 35, LatencyNS: uint64(4 * time.Second), TTFTNS: uint64(time.Second)},
	} {
		if err := store.Record(record); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := store.Overview(usageFilter{From: now.Add(-time.Minute), To: now.Add(time.Hour)}, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Series) != 1 {
		t.Fatalf("series = %#v", overview.Series)
	}
	point := overview.Series[0]
	if point.AverageLatencyNS != uint64(3*time.Second) || point.AverageTTFTNS != uint64(750*time.Millisecond) || !near(point.AverageTPS, 5.833333333333333) {
		t.Fatalf("efficiency point = %#v", point)
	}
}

func TestUsageOverviewTracksCacheAccountingModes(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	if _, err := store.SavePriceBook(saveModelPricesRequest{Prices: map[string]modelPrice{
		"openai/model":    {tokenRates: tokenRates{Input: 1}, AccountingMode: accountingModeInputIncludesCache},
		"anthropic/model": {tokenRates: tokenRates{Input: 1}, AccountingMode: accountingModeInputExcludesCache},
	}}, now); err != nil {
		t.Fatal(err)
	}
	for _, record := range []storedUsageRecord{
		{RequestedAt: now, Provider: "openai", ProviderModel: "openai/model", InputTokens: 10, CacheReadTokens: 3},
		{RequestedAt: now.Add(time.Minute), Provider: "anthropic", ProviderModel: "anthropic/model", InputTokens: 5, CacheReadTokens: 3},
	} {
		if err := store.Record(record); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := store.Overview(usageFilter{From: now.Add(-time.Minute), To: now.Add(time.Hour)}, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Series) != 1 || overview.Series[0].CacheReadIncludedTokens != 3 {
		t.Fatalf("cache accounting series = %#v", overview.Series)
	}
}

func TestUsageAggregatesEffectiveCacheReadAliases(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	for _, record := range []storedUsageRecord{
		{RequestedAt: now, ProviderModel: "cached-only", CachedTokens: 4},
		{RequestedAt: now.Add(time.Minute), ProviderModel: "cache-read", CacheReadTokens: 6},
		{RequestedAt: now.Add(2 * time.Minute), ProviderModel: "both", CachedTokens: 8, CacheReadTokens: 3},
		{RequestedAt: now.Add(3 * time.Minute), ProviderModel: "creation-only", CachedTokens: 9, CacheCreationTokens: 9},
	} {
		if err := store.Record(record); err != nil {
			t.Fatal(err)
		}
	}
	filter := usageFilter{From: now.Add(-time.Minute), To: now.Add(time.Hour)}
	overview, err := store.Overview(filter, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if overview.Summary.EffectiveCacheReadTokens != 13 || len(overview.Series) != 1 || overview.Series[0].EffectiveCacheReadTokens != 13 {
		t.Fatalf("effective cache aliases = summary=%#v series=%#v", overview.Summary, overview.Series)
	}
	groups, err := store.Groups(filter, "provider_model", "total_tokens", "desc", 0, 10)
	if err != nil || len(groups.Items) != 4 {
		t.Fatalf("effective cache groups = %#v, %v", groups, err)
	}
	for _, item := range groups.Items {
		want := uint64(0)
		switch item.ProviderModel {
		case "cached-only":
			want = 4
		case "cache-read":
			want = 6
		case "both":
			want = 3
		case "creation-only":
			want = 0
		}
		if item.EffectiveCacheReadTokens != want {
			t.Fatalf("group %q effective cache = %d, want %d", item.ProviderModel, item.EffectiveCacheReadTokens, want)
		}
	}
	requests, err := store.Requests(filter, "time", "asc", 0, 10)
	if err != nil || len(requests.Items) != 4 || requests.Items[3].EffectiveCacheReadTokens != 0 || requests.Items[3].CacheHit {
		t.Fatalf("request effective cache aliases = %#v, %v", requests.Items, err)
	}
}

func TestUsageOverviewTracksReasoningAccountingModes(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	for _, record := range []storedUsageRecord{
		{RequestedAt: now, Provider: "openai", ProviderModel: "openai/model", OutputTokens: 7, ReasoningTokens: 2},
		{RequestedAt: now.Add(time.Minute), Provider: "google", ProviderModel: "google/model", OutputTokens: 4, ReasoningTokens: 3},
	} {
		if err := store.Record(record); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := store.Overview(usageFilter{From: now.Add(-time.Minute), To: now.Add(time.Hour)}, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Series) != 1 || overview.Series[0].ReasoningIncludedTokens != 2 {
		t.Fatalf("reasoning accounting series = %#v", overview.Series)
	}
}

func TestUsageEfficiencyAveragesIgnoreUnmeasuredTTFT(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	for _, record := range []storedUsageRecord{
		{RequestedAt: now, Attribution: attributionDirect, ProviderModel: "model", OutputTokens: 10, LatencyNS: uint64(2 * time.Second), TTFTNS: uint64(time.Second)},
		{RequestedAt: now.Add(time.Minute), Attribution: attributionDirect, ProviderModel: "model", OutputTokens: 15, LatencyNS: uint64(4 * time.Second)},
	} {
		if err := store.Record(record); err != nil {
			t.Fatal(err)
		}
	}
	filter := usageFilter{From: now.Add(-time.Minute), To: now.Add(time.Hour)}
	overview, err := store.Overview(filter, "hour")
	if err != nil || len(overview.Series) != 1 || overview.Series[0].AverageTTFTNS != uint64(time.Second) {
		t.Fatalf("overview TTFT = %#v, %v", overview.Series, err)
	}
	groups, err := store.Groups(filter, "provider_model", "ttft", "desc", 0, 10)
	if err != nil || len(groups.Items) != 1 || groups.Items[0].AverageTTFTNS != uint64(time.Second) {
		t.Fatalf("group TTFT = %#v, %v", groups.Items, err)
	}
}

func TestUsageStoreConcurrentRecordQueryReconfigureAndClose(t *testing.T) {
	root := t.TempDir()
	store, err := openUsageStore(filepath.Join(root, "first.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var workers sync.WaitGroup
	errorsSeen := make(chan error, 600)
	workers.Add(3)
	go func() {
		defer workers.Done()
		for index := 0; index < 150; index++ {
			if err := store.Record(storedUsageRecord{RequestedAt: now.Add(time.Duration(index) * time.Microsecond), Attribution: attributionDirect, ProviderModel: "model", TotalTokens: 1}); err != nil {
				errorsSeen <- err
			}
		}
	}()
	go func() {
		defer workers.Done()
		for index := 0; index < 150; index++ {
			filter := usageFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
			var err error
			if index%2 == 0 {
				_, err = store.Dashboard(filter, "minute", usageGroupOptions{"provider_model", usagePageOptions{"total_tokens", "desc", 0, 10}}, usagePageOptions{"cost", "desc", 0, 10})
			} else {
				_, err = store.Requests(filter, "time", "desc", 0, 10)
			}
			if err != nil {
				errorsSeen <- err
			}
		}
	}()
	go func() {
		defer workers.Done()
		for index := 0; index < 12; index++ {
			path := filepath.Join(root, "first.db")
			if index%2 == 1 {
				path = filepath.Join(root, "second.db")
			}
			if err := store.Reconfigure(path, 30+index); err != nil {
				errorsSeen <- err
			}
		}
	}()
	workers.Wait()

	workers.Add(2)
	go func() {
		defer workers.Done()
		for index := 0; index < 50; index++ {
			if err := store.Record(storedUsageRecord{RequestedAt: now, Attribution: attributionDirect, ProviderModel: "model"}); err != nil && !strings.Contains(err.Error(), "closed") {
				errorsSeen <- err
			}
		}
	}()
	go func() {
		defer workers.Done()
		if err := store.Close(); err != nil {
			errorsSeen <- err
		}
	}()
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent usage operation: %v", err)
		}
	}
}

func mapHeader(name, value string) map[string][]string {
	return map[string][]string{name: {value}}
}

func usageQueryFixture(t testing.TB) (*usageStore, usageFilter) {
	t.Helper()
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 3650)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Now().UTC().Truncate(24 * time.Hour).Add(-7*24*time.Hour + 23*time.Hour + 30*time.Minute)
	for index := 0; index < 72; index++ {
		record := storedUsageRecord{
			RequestedAt:   start.Add(time.Duration(index/3) * 5 * time.Minute),
			Attribution:   []string{attributionRouted, attributionDirect, attributionUnresolved}[index%3],
			ProviderModel: []string{"model-a", "model-b", "ÜNICODE/model-c"}[index%3],
			Provider:      []string{"openai", "anthropic", "google"}[index%3],
			Source:        []string{"gateway", " gateway ", "ÄPI"}[index%3],
			ServiceTier:   []string{"", "priority", "flex", "Priority"}[index%4],
			InputTokens:   uint64(index + 1), OutputTokens: uint64(index%9 + 1), TotalTokens: uint64(index + index%9 + 2),
			CacheReadTokens: uint64(index % 7), CacheCreationTokens: uint64(index % 5), ReasoningTokens: uint64(index % 3),
			LatencyNS: uint64(index%6) * uint64(time.Second), TTFTNS: uint64(index%4) * uint64(100*time.Millisecond),
			Failed: index%5 == 0,
		}
		if record.Attribution == attributionRouted {
			record.RouterModel = []string{"auto", "direct", "unattributed", "ÅLIAS"}[(index/3)%4]
		}
		if record.Failed && index%2 == 0 {
			record.StatusCode = 429
		}
		if index%4 == 0 {
			record.CachedTokens, record.CacheReadTokens, record.CacheCreationTokens = 5, 0, 0
		}
		if index%7 == 0 {
			record.AccountingMode, record.ReasoningMode = accountingModeInputExcludesCache, reasoningModeSeparate
		}
		if err := store.Record(record); err != nil {
			t.Fatal(err)
		}
	}
	prices := make(map[string]modelPrice)
	for _, model := range []string{"model-a", "model-b", "ÜNICODE/model-c"} {
		prices[model] = modelPrice{tokenRates: tokenRates{Input: 1, Output: 3, CacheRead: .1, CacheCreation: 2},
			ContextTiers: []contextPriceTier{{Threshold: 35, tokenRates: tokenRates{Input: 2, Output: 4, CacheRead: .2, CacheCreation: 3}}},
			ServiceTiers: map[string]serviceTierPrice{"priority": {tokenRates: tokenRates{Input: 5, Output: 6}, ContextTiers: []contextPriceTier{{Threshold: 50, tokenRates: tokenRates{Input: 7, Output: 8}}}}},
		}
	}
	if _, err := store.SavePriceBook(saveModelPricesRequest{Prices: prices}, start); err != nil {
		t.Fatal(err)
	}
	return store, usageFilter{From: start, To: start.Add(2 * time.Hour)}
}

func canonicalUsageOverview(value usageOverview) usageOverview {
	value.GeneratedAt, value.RetainedSince = time.Time{}, time.Time{}
	for _, models := range [][]usageModelStats{value.RouterModels, value.ProviderModels} {
		sort.Slice(models, func(i, j int) bool {
			return models[i].Model+"\x00"+models[i].Attribution < models[j].Model+"\x00"+models[j].Attribution
		})
	}
	return value
}

func TestUsageQueriesMatchOriginalAcrossFiltersAndGranularities(t *testing.T) {
	store, base := usageQueryFixture(t)
	filters := []struct {
		name   string
		filter usageFilter
	}{{"all", base}}
	for _, dimension := range []string{"attribution", "router_model", "provider_model", "source", "service_tier", "result", "combined", "empty", "partial", "offset_zone"} {
		filter := base
		switch dimension {
		case "attribution":
			filter.Attribution = " DIRECT "
		case "router_model":
			filter.RouterModel = " ålias "
		case "provider_model":
			filter.ProviderModel = " ünICODE/model-c "
		case "source":
			filter.Source = " äpi "
		case "service_tier":
			filter.ServiceTier = " PRIORITY "
		case "result":
			filter.Result = " HTTP_429 "
		case "combined":
			filter.ProviderModel, filter.Source, filter.ServiceTier = "MODEL-B", "gateway", "priority"
		case "empty":
			filter.ProviderModel = "absent"
		case "partial":
			filter.From, filter.To = base.From.Add(15*time.Minute+time.Nanosecond), base.From.Add(50*time.Minute)
		case "offset_zone":
			filter.From, filter.To = base.From.In(time.FixedZone("east", 19800)), base.To.In(time.FixedZone("west", -25200))
		}
		filters = append(filters, struct {
			name   string
			filter usageFilter
		}{dimension, filter})
	}
	for _, test := range filters {
		for _, granularity := range []string{"minute", "hour", "day"} {
			t.Run(test.name+"/"+granularity, func(t *testing.T) {
				got, err := store.Overview(test.filter, granularity)
				if err != nil {
					t.Fatal(err)
				}
				want, err := store.referenceUsageOverview(test.filter, granularity)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(canonicalUsageOverview(got), canonicalUsageOverview(want)) {
					t.Fatalf("overview differs from original: got %#v want %#v", got, want)
				}
				groups := usageGroupOptions{"provider_model", usagePageOptions{"total_tokens", "desc", 0, 50}}
				requests := usagePageOptions{"cost", "desc", 3, 7}
				dashboard, err := store.Dashboard(test.filter, granularity, groups, requests)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(canonicalUsageOverview(dashboard.Overview), canonicalUsageOverview(want)) {
					t.Fatal("combined overview differs from original")
				}
				page, err := store.referenceUsageRequests(test.filter, requests.Sort, requests.Order, requests.Offset, requests.Limit)
				if err != nil {
					t.Fatal(err)
				}
				page.GeneratedAt = dashboard.Requests.GeneratedAt
				if !reflect.DeepEqual(dashboard.Requests, page) {
					t.Fatal("combined request page differs from original")
				}
				if dashboard.GeneratedAt != dashboard.Overview.GeneratedAt || dashboard.GeneratedAt != dashboard.Groups.GeneratedAt || dashboard.GeneratedAt != dashboard.Requests.GeneratedAt || dashboard.PriceBookRevision != dashboard.Requests.PriceBookRevision {
					t.Fatal("dashboard metadata does not share one snapshot")
				}
			})
		}
	}
}

func TestUsageQuerySortingAndPaginationMatchOriginal(t *testing.T) {
	store, filter := usageQueryFixture(t)
	for _, field := range requestSortFields {
		for _, order := range []string{"asc", "desc"} {
			for _, offset := range []int{0, 8, 64, 72, int(^uint(0) >> 1)} {
				for _, filtered := range []bool{false, true} {
					t.Run(fmt.Sprintf("requests/%s/%s/%d/%t", field, order, offset, filtered), func(t *testing.T) {
						query := filter
						if filtered {
							query.Source = "GATEWAY"
						}
						got, err := store.Requests(query, field, order, offset, 9)
						if err != nil {
							t.Fatal(err)
						}
						want, err := store.referenceUsageRequests(query, field, order, offset, 9)
						if err != nil {
							t.Fatal(err)
						}
						got.GeneratedAt = want.GeneratedAt
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("request page differs: got %#v want %#v", got, want)
						}
					})
				}
			}
		}
	}
	for _, dimension := range groupDimensions {
		for _, field := range groupSortFields {
			for _, order := range []string{"asc", "desc"} {
				t.Run("groups/"+dimension+"/"+field+"/"+order, func(t *testing.T) {
					got, err := store.Groups(filter, dimension, field, order, 0, 500)
					if err != nil {
						t.Fatal(err)
					}
					want, err := store.referenceUsageGroups(filter, dimension, field, order, 0, 500)
					if err != nil {
						t.Fatal(err)
					}
					for index := 1; index < len(got.Items); index++ {
						comparison := compareGroups(got.Items[index-1], got.Items[index], field)
						if order == "asc" && comparison > 0 || order == "desc" && comparison < 0 {
							t.Fatal("groups are out of order")
						}
					}
					for _, items := range [][]usageGroup{got.Items, want.Items} {
						sort.Slice(items, func(i, j int) bool {
							left, _ := json.Marshal(items[i])
							right, _ := json.Marshal(items[j])
							return string(left) < string(right)
						})
					}
					got.GeneratedAt = want.GeneratedAt
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("group totals differ: got %#v want %#v", got, want)
					}
				})
			}
		}
	}
	for _, offset := range []int{0, 1, 2, 3, int(^uint(0) >> 1)} {
		got, err := store.Groups(filter, "provider_model", "key", "asc", offset, 1)
		if err != nil {
			t.Fatal(err)
		}
		want, err := store.referenceUsageGroups(filter, "provider_model", "key", "asc", offset, 1)
		if err != nil {
			t.Fatal(err)
		}
		got.GeneratedAt = want.GeneratedAt
		if !reflect.DeepEqual(got, want) {
			t.Fatal("group pagination differs")
		}
	}
}

func TestUsageSnapshotAllowsConcurrentWriterAndKeepsPricesAndRows(t *testing.T) {
	store, filter := usageQueryFixture(t)
	writer, err := openUsageStore(store.path, 3650)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	err = store.readSnapshot(func(connection *sql.Conn, _ int) error {
		before, err := priceBookFromSQLite(connection)
		if err != nil {
			return err
		}
		count, err := countUsageRecords(connection, filter)
		if err != nil {
			return err
		}
		if _, err := writer.SavePriceBook(saveModelPricesRequest{Revision: before.Revision, Prices: map[string]modelPrice{"model-a": {tokenRates: tokenRates{Input: 100}}}}, time.Now().UTC()); err != nil {
			return err
		}
		if err := writer.ResetUsage(); err != nil {
			return err
		}
		after, err := priceBookFromSQLite(connection)
		if err != nil {
			return err
		}
		rows, err := indexedUsageRequestPage(connection, filter, usagePageOptions{"time", "desc", 0, 500}, newModelPriceResolver(after.Prices, after.SyncSettings))
		if err != nil {
			return err
		}
		if after.Revision != before.Revision || len(rows) != count || count != 72 {
			return fmt.Errorf("snapshot changed during concurrent writes: prices %d/%d, rows %d/%d", before.Revision, after.Revision, len(rows), count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.Requests(filter, "time", "desc", 0, 10)
	if err != nil || page.Total != 0 || page.PriceBookRevision != 2 {
		t.Fatalf("next snapshot failed to observe writes: %#v, %v", page, err)
	}
}

func TestUsageSnapshotReleasesConnectionAfterFailures(t *testing.T) {
	store, filter := usageQueryFixture(t)
	marker := errors.New("reader failed")
	if err := store.readSnapshot(func(*sql.Conn, int) error { return marker }); !errors.Is(err, marker) {
		t.Fatalf("snapshot error = %v", err)
	}
	if err := store.readSnapshot(func(connection *sql.Conn, _ int) error {
		_, err := connection.ExecContext(context.Background(), "SELECT * FROM missing_usage_table")
		return err
	}); err == nil {
		t.Fatal("SQL error was suppressed")
	}
	if _, err := store.db.Exec(`UPDATE requests SET payload = ? WHERE sequence = 1`, []byte("invalid JSON")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Overview(filter, "day"); err == nil {
		t.Fatal("record decode error was suppressed")
	}
	if store.db.Stats().InUse != 0 {
		t.Fatal("failed read retained a database connection")
	}
	if _, err := store.db.Exec(`DELETE FROM requests WHERE sequence = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Overview(filter, "hour"); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(storedUsageRecord{RequestedAt: filter.From, ProviderModel: "after-error"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Reconfigure(filepath.Join(t.TempDir(), "next.db"), 365); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Requests(filter, "time", "desc", 0, 1); err == nil {
		t.Fatal("closed storage returned success")
	}
}

func BenchmarkUsageQueries(b *testing.B) {
	path := os.Getenv("CPA_USAGE_BENCH_DB")
	if path == "" {
		b.Skip("set CPA_USAGE_BENCH_DB to a representative SQLite usage database")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		b.Fatal(err)
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = url.Values{"mode": {"ro"}, "_query_only": {"1"}}.Encode()
	database, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		b.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	store := &usageStore{db: database, retentionDays: 3650}
	b.Cleanup(func() { _ = store.Close() })
	var first, last int64
	if err := database.QueryRow(`SELECT MIN(requested_at_ns), MAX(requested_at_ns) FROM requests`).Scan(&first, &last); err != nil {
		b.Fatal(err)
	}
	end := time.Unix(0, last+1).UTC()
	base := usageFilter{From: end.Add(-30 * 24 * time.Hour), To: end}
	type queryCase struct {
		name, endpoint, granularity string
		filter                      usageFilter
		group                       usageGroupOptions
		request                     usagePageOptions
	}
	cases := []queryCase{
		{name: "5h_minute", filter: usageFilter{From: end.Add(-5 * time.Hour), To: end}, granularity: "minute"},
		{name: "24h_minute", filter: usageFilter{From: end.Add(-24 * time.Hour), To: end}, granularity: "minute"},
		{name: "7d_hour", filter: usageFilter{From: end.Add(-7 * 24 * time.Hour), To: end}, granularity: "hour"},
		{name: "30d_day", filter: base, granularity: "day"},
		{name: "30d_hour", filter: base, granularity: "hour"},
		{name: "30d_minute", filter: base, granularity: "minute"},
		{name: "month_day", filter: usageFilter{From: time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC), To: end}, granularity: "day"},
		{name: "custom_partial_day", filter: usageFilter{From: end.Add(-14 * 24 * time.Hour).Truncate(24 * time.Hour).Add(-30 * time.Minute), To: end.Add(-14 * 24 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)}, granularity: "minute"},
		{name: "full_retained", filter: usageFilter{From: time.Unix(0, first).UTC(), To: end}, granularity: "day"},
	}
	samples := make(map[string]string)
	rows, err := database.Query(`SELECT payload FROM requests ORDER BY requested_at_ns DESC, sequence DESC LIMIT 512`)
	if err != nil {
		b.Fatal(err)
	}
	var sample storedUsageRecord
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			b.Fatal(err)
		}
		if err := json.Unmarshal(raw, &sample); err != nil {
			b.Fatal(err)
		}
		for key, value := range map[string]string{"attribution": sample.Attribution, "router_model": sample.RouterModel, "provider_model": sample.ProviderModel, "source": sample.Source, "service_tier": sample.ServiceTier, "result": sample.result()} {
			if samples[key] == "" {
				samples[key] = value
			}
		}
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		b.Fatal(err)
	}
	for _, dimension := range []string{"attribution", "router_model", "provider_model", "source", "service_tier", "result", "empty", "combined"} {
		filter := base
		value := samples[dimension]
		if value == "" {
			value = "missing-benchmark-value-281ca4"
		}
		switch dimension {
		case "attribution":
			filter.Attribution = value
		case "router_model":
			filter.RouterModel = value
		case "provider_model":
			filter.ProviderModel = value
		case "source":
			filter.Source = value
		case "service_tier":
			filter.ServiceTier = value
		case "result":
			filter.Result = value
		case "empty":
			filter.ProviderModel = value
		case "combined":
			filter.ProviderModel, filter.Source, filter.Result = sample.ProviderModel, sample.Source, sample.result()
		}
		cases = append(cases, queryCase{name: "filter_" + dimension, filter: filter, granularity: "day"})
	}
	for _, dimension := range groupDimensions {
		cases = append(cases, queryCase{name: "group_" + dimension, endpoint: "groups", filter: base, group: usageGroupOptions{dimension, usagePageOptions{"total_tokens", "desc", 0, 50}}})
	}
	for _, field := range requestSortFields {
		cases = append(cases, queryCase{name: "sort_" + field, endpoint: "requests", filter: base, request: usagePageOptions{field, "desc", 0, 50}})
	}
	for _, offset := range []int{50, 10000} {
		cases = append(cases, queryCase{name: fmt.Sprintf("page_%d", offset), endpoint: "requests", filter: base, request: usagePageOptions{"cost", "asc", offset, 50}})
	}
	cases = append(cases, queryCase{name: "filtered_page", endpoint: "requests", filter: usageFilter{From: base.From, To: base.To, ProviderModel: samples["provider_model"]}, request: usagePageOptions{"time", "desc", 50, 50}})
	cases = append(cases, queryCase{name: "overview_minute", endpoint: "overview", filter: base, granularity: "minute"})
	for _, test := range cases {
		if test.endpoint == "" {
			test.endpoint = "dashboard"
		}
		if test.group.Dimension == "" {
			test.group = usageGroupOptions{"provider_model", usagePageOptions{"total_tokens", "desc", 0, 50}}
		}
		if test.request.Sort == "" {
			test.request = usagePageOptions{"time", "desc", 0, 50}
		}
		for _, original := range []bool{true, false} {
			implementation := "streaming"
			if original {
				implementation = "original"
			}
			b.Run(test.endpoint+"/"+test.name+"/"+implementation, func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					var result any
					var err error
					switch test.endpoint {
					case "overview":
						if original {
							result, err = store.referenceUsageOverview(test.filter, test.granularity)
						} else {
							result, err = store.Overview(test.filter, test.granularity)
						}
					case "groups":
						options := test.group
						if original {
							result, err = store.referenceUsageGroups(test.filter, options.Dimension, options.Sort, options.Order, options.Offset, options.Limit)
						} else {
							result, err = store.Groups(test.filter, options.Dimension, options.Sort, options.Order, options.Offset, options.Limit)
						}
					case "requests":
						options := test.request
						if original {
							result, err = store.referenceUsageRequests(test.filter, options.Sort, options.Order, options.Offset, options.Limit)
						} else {
							result, err = store.Requests(test.filter, options.Sort, options.Order, options.Offset, options.Limit)
						}
					default:
						if original {
							result, err = store.referenceUsageDashboard(test.filter, test.granularity, test.group, test.request)
						} else {
							result, err = store.Dashboard(test.filter, test.granularity, test.group, test.request)
						}
					}
					if err != nil {
						b.Fatal(err)
					}
					if _, err := json.Marshal(result); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func (store *usageStore) referenceUsageDashboard(filter usageFilter, granularity string, groups usageGroupOptions, requests usagePageOptions) (usageDashboard, error) {
	var result usageDashboard
	var workers sync.WaitGroup
	errorsSeen := make(chan error, 3)
	workers.Add(3)
	go func() {
		defer workers.Done()
		var err error
		result.Overview, err = store.referenceUsageOverview(filter, granularity)
		errorsSeen <- err
	}()
	go func() {
		defer workers.Done()
		var err error
		result.Groups, err = store.referenceUsageGroups(filter, groups.Dimension, groups.Sort, groups.Order, groups.Offset, groups.Limit)
		errorsSeen <- err
	}()
	go func() {
		defer workers.Done()
		var err error
		result.Requests, err = store.referenceUsageRequests(filter, requests.Sort, requests.Order, requests.Offset, requests.Limit)
		errorsSeen <- err
	}()
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			return usageDashboard{}, err
		}
	}
	return result, nil
}

// The bulk queries below preserve the original algorithm for real-data parity checks and benchmarks.
func (store *usageStore) referenceUsageOverview(filter usageFilter, granularity string) (usageOverview, error) {
	records, err := store.referenceUsageRecords(filter)
	if err != nil {
		return usageOverview{}, err
	}
	book, err := store.QueryPriceBook()
	if err != nil {
		return usageOverview{}, err
	}
	resolver := newModelPriceResolver(book.Prices, book.SyncSettings)
	now := time.Now().UTC()
	overview := usageOverview{
		SchemaVersion: usageSchemaVersion,
		GeneratedAt:   now,
		From:          filter.From,
		To:            filter.To,
		RetainedSince: store.RetainedSince(now),
		StorageError:  store.LastError(),
	}
	series := make(map[string]*usageSeriesPoint)
	routerModels := make(map[usageRouterKey]*usageModelStats)
	providerModels := make(map[string]*usageModelStats)
	sources := make(map[string]struct{})
	serviceTiers := make(map[string]struct{})
	results := make(map[string]struct{})
	for _, record := range records {
		overview.Summary.add(record)
		cost := estimateUsageCost(record, resolver)
		overview.Costs.add(cost)
		bucket := usageBucket(record.RequestedAt, granularity)
		key := bucket.Format(time.RFC3339)
		point := series[key]
		if point == nil {
			point = &usageSeriesPoint{Time: key}
			series[key] = point
		}
		point.usageCounters.add(record)
		mode := cost.AccountingMode
		if mode == "" {
			mode = defaultAccountingMode(record.Provider, record.ExecutorType)
		}
		if mode == accountingModeInputIncludesCache {
			point.CacheReadIncludedTokens += effectiveCacheReadTokens(record)
		}
		if reasoningIncludedInOutput(record) {
			point.ReasoningIncludedTokens += record.ReasoningTokens
		}
		point.latencyTotal += record.LatencyNS
		point.ttftTotal += record.TTFTNS
		if record.TTFTNS > 0 {
			point.ttftRequests++
		}
		generation := record.LatencyNS
		if record.TTFTNS > 0 && record.LatencyNS >= record.TTFTNS {
			generation -= record.TTFTNS
		}
		if generation > 0 {
			point.tpsTotal += float64(record.OutputTokens) / (float64(generation) / float64(time.Second))
			point.timingRequests++
		}
		point.CostUSD += cost.TotalUSD

		routerKey := usageRouterKey{model: record.RouterModel, attribution: record.Attribution}
		routerLabel, routerAttribution := record.RouterModel, record.Attribution
		if record.Attribution == attributionDirect {
			routerKey.model, routerLabel = "", ""
		} else if record.Attribution == attributionUnresolved {
			routerKey.model, routerLabel = "", ""
		}
		router := routerModels[routerKey]
		if router == nil {
			router = &usageModelStats{Model: routerLabel, Attribution: routerAttribution}
			routerModels[routerKey] = router
		}
		router.usageCounters.add(record)
		router.CostUSD += cost.TotalUSD

		provider := providerModels[record.ProviderModel]
		if provider == nil {
			provider = &usageModelStats{Model: record.ProviderModel}
			providerModels[record.ProviderModel] = provider
		}
		provider.usageCounters.add(record)
		provider.CostUSD += cost.TotalUSD
		if record.Source != "" {
			sources[record.Source] = struct{}{}
		}
		if record.ServiceTier != "" {
			serviceTiers[record.ServiceTier] = struct{}{}
		}
		results[record.result()] = struct{}{}
	}
	for _, point := range series {
		if point.Requests > 0 {
			point.AverageLatencyNS = point.latencyTotal / point.Requests
		}
		if point.ttftRequests > 0 {
			point.AverageTTFTNS = point.ttftTotal / point.ttftRequests
		}
		if point.timingRequests > 0 {
			point.AverageTPS = point.tpsTotal / float64(point.timingRequests)
		}
		overview.Series = append(overview.Series, *point)
	}
	sort.Slice(overview.Series, func(left, right int) bool { return overview.Series[left].Time < overview.Series[right].Time })
	overview.RouterModels = modelStatsValues(routerModels)
	overview.ProviderModels = modelStatsValues(providerModels)
	overview.Sources = sortedStrings(sources)
	overview.ServiceTiers = sortedStrings(serviceTiers)
	overview.Results = sortedStrings(results)
	return overview, nil
}

func (store *usageStore) referenceUsageGroups(filter usageFilter, dimension, sortField, order string, offset, limit int) (usageGroupPage, error) {
	records, err := store.referenceUsageRecords(filter)
	if err != nil {
		return usageGroupPage{}, err
	}
	book, err := store.QueryPriceBook()
	if err != nil {
		return usageGroupPage{}, err
	}
	resolver := newModelPriceResolver(book.Prices, book.SyncSettings)
	groups := make(map[usageGroupKey]*groupAccumulator)
	for _, record := range records {
		value := usageDimensionValue(record, dimension)
		key := usageGroupKey{value: value}
		if dimension == "router_model" {
			key = usageGroupKey{router: usageRouterKey{model: record.RouterModel, attribution: record.Attribution}}
		}
		accumulator := groups[key]
		if accumulator == nil {
			accumulator = &groupAccumulator{group: usageGroup{Key: value}}
			assignGroupDimension(&accumulator.group, record, dimension)
			groups[key] = accumulator
		}
		accumulator.group.usageCounters.add(record)
		accumulator.latencyTotal += record.LatencyNS
		accumulator.ttftTotal += record.TTFTNS
		if record.TTFTNS > 0 {
			accumulator.ttftRequests++
		}
		generation := record.LatencyNS
		if record.TTFTNS > 0 && record.LatencyNS >= record.TTFTNS {
			generation -= record.TTFTNS
		}
		if generation > 0 {
			accumulator.tpsTotal += float64(record.OutputTokens) / (float64(generation) / float64(time.Second))
			accumulator.timingRequests++
		}
		cost := estimateUsageCost(record, resolver)
		accumulator.group.CostUSD += cost.TotalUSD
		if cost.Priced {
			accumulator.group.PricedRequests++
		}
	}
	items := make([]usageGroup, 0, len(groups))
	for _, accumulator := range groups {
		requests := accumulator.group.Requests
		if requests > 0 {
			accumulator.group.AverageLatencyNS = accumulator.latencyTotal / requests
		}
		if accumulator.ttftRequests > 0 {
			accumulator.group.AverageTTFTNS = accumulator.ttftTotal / accumulator.ttftRequests
		}
		if accumulator.timingRequests > 0 {
			accumulator.group.AverageTPS = accumulator.tpsTotal / float64(accumulator.timingRequests)
		}
		items = append(items, accumulator.group)
	}
	sort.SliceStable(items, func(left, right int) bool {
		comparison := compareGroups(items[left], items[right], sortField)
		if comparison == 0 {
			comparison = strings.Compare(strings.ToLower(items[left].Key), strings.ToLower(items[right].Key))
		}
		if order == "asc" {
			return comparison < 0
		}
		return comparison > 0
	})
	total := len(items)
	items = paginateGroups(items, offset, limit)
	return usageGroupPage{SchemaVersion: usageSchemaVersion, GeneratedAt: time.Now().UTC(), Dimension: dimension, Total: total, Offset: offset, Limit: limit, Items: items}, nil
}

func (store *usageStore) referenceUsageRequests(filter usageFilter, sortField, order string, offset, limit int) (usageRequestPage, error) {
	records, err := store.referenceUsageRecords(filter)
	if err != nil {
		return usageRequestPage{}, err
	}
	book, err := store.QueryPriceBook()
	if err != nil {
		return usageRequestPage{}, err
	}
	resolver := newModelPriceResolver(book.Prices, book.SyncSettings)
	items := make([]usageRequestDetail, 0, len(records))
	for _, record := range records {
		items = append(items, requestDetail(record, resolver))
	}
	sort.SliceStable(items, func(left, right int) bool {
		comparison := compareReferenceRequests(items[left], items[right], sortField)
		if comparison == 0 {
			comparison = compareUint64(items[left].Sequence, items[right].Sequence)
		}
		if order == "asc" {
			return comparison < 0
		}
		return comparison > 0
	})
	total := len(items)
	if offset >= len(items) {
		items = []usageRequestDetail{}
	} else {
		end := offset + limit
		if end > len(items) {
			end = len(items)
		}
		items = items[offset:end]
	}
	return usageRequestPage{SchemaVersion: usageSchemaVersion, GeneratedAt: time.Now().UTC(), PriceBookRevision: book.Revision, Total: total, Offset: offset, Limit: limit, Items: items}, nil
}

func compareReferenceRequests(left, right usageRequestDetail, field string) int {
	switch field {
	case "router_model":
		return strings.Compare(strings.ToLower(usageDimensionValue(left.storedUsageRecord, "router_model")), strings.ToLower(usageDimensionValue(right.storedUsageRecord, "router_model")))
	case "provider_model":
		return strings.Compare(strings.ToLower(left.ProviderModel), strings.ToLower(right.ProviderModel))
	case "source":
		return strings.Compare(strings.ToLower(left.Source), strings.ToLower(right.Source))
	case "service_tier":
		return strings.Compare(strings.ToLower(left.ServiceTier), strings.ToLower(right.ServiceTier))
	case "result":
		return strings.Compare(left.Result, right.Result)
	case "latency":
		return compareUint64(left.LatencyNS, right.LatencyNS)
	case "ttft":
		return compareUint64(left.TTFTNS, right.TTFTNS)
	case "tps":
		return compareFloat(left.TPS, right.TPS)
	case "total_tokens":
		return compareUint64(left.TotalTokens, right.TotalTokens)
	case "cost":
		return compareFloat(left.EstimatedCost.TotalUSD, right.EstimatedCost.TotalUSD)
	default:
		if left.RequestedAt.Before(right.RequestedAt) {
			return -1
		}
		if left.RequestedAt.After(right.RequestedAt) {
			return 1
		}
		return 0
	}
}

func (store *usageStore) referenceUsageRecords(filter usageFilter) ([]storedUsageRecord, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return nil, errors.New("usage database is closed")
	}
	query := `SELECT requested_at_ns, payload FROM requests`
	args := make([]any, 0, 2)
	conditions := make([]string, 0, 2)
	if !filter.From.IsZero() {
		conditions = append(conditions, "requested_at_ns >= ?")
		args = append(args, filter.From.UTC().UnixNano())
	}
	if !filter.To.IsZero() {
		conditions = append(conditions, "requested_at_ns < ?")
		args = append(args, filter.To.UTC().UnixNano())
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY requested_at_ns, sequence"
	rows, err := store.db.Query(query, args...)
	if err != nil {
		store.recordError(err)
		return nil, err
	}
	defer rows.Close()
	records := make([]storedUsageRecord, 0)
	for rows.Next() {
		var timestamp int64
		var value []byte
		if err := rows.Scan(&timestamp, &value); err != nil {
			return nil, err
		}
		var record storedUsageRecord
		if err := json.Unmarshal(value, &record); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		if filter.matches(record) {
			records = append(records, record)
		}
	}
	if err := rows.Err(); err != nil {
		store.recordError(err)
		return nil, err
	}
	return records, nil
}
