package main

import (
	"bytes"
	"os"
	"path/filepath"
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
	plugin := &modelRouterPlugin{store: store, attribution: newAttributionTracker(func() time.Time { return now })}
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
	if loaded, err := store.QueryPreferences(); err != nil || loaded.RequestPageSize != 50 || loaded.TimeRange != "custom" || loaded.CustomFrom != preferences.CustomFrom || loaded.CustomTo != preferences.CustomTo {
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
	if loaded, _ := store.QueryPreferences(); loaded.RequestPageSize != 50 {
		t.Fatalf("preferences after reset = %#v", loaded)
	}
}

func TestSafeStoredUsageSourceRejectsCredentialShapedValues(t *testing.T) {
	for _, source := range []string{"abcdefgh12345678", "abcdefghijklmno", "123456789012345", "AbCd+1234==", "Ab/Cd+1234=="} {
		record := pluginapi.UsageRecord{Provider: "openai", ExecutorType: "openai", Source: source}
		if got := safeStoredUsageSource(record); got != "openai" {
			t.Errorf("safe source for %q = %q, want provider fallback", source, got)
		}
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
			if _, err := store.Requests(usageFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, "time", "desc", 0, 10); err != nil {
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
