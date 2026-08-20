package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUsageManagementOverviewRequestsPricesPreferencesAndReset(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Add(-time.Minute)
	if err := store.Record(storedUsageRecord{
		RequestedAt: now, Attribution: attributionRouted, RouterModel: "auto", ProviderModel: "gpt-5.4", Provider: "openai",
		Source: "openai", InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
	}); err != nil {
		t.Fatal(err)
	}
	plugin := &modelRouterPlugin{store: store, attribution: newAttributionTracker(nil)}
	query := url.Values{"from": {now.Add(-time.Hour).Format(time.RFC3339)}, "to": {now.Add(time.Hour).Format(time.RFC3339)}}

	overview := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/overview", Query: query})
	if overview.StatusCode != http.StatusOK || !strings.Contains(string(overview.Body), `"router_models"`) || !strings.Contains(string(overview.Body), `"auto"`) {
		t.Fatalf("overview = status %d body %s", overview.StatusCode, overview.Body)
	}
	requests := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/requests", Query: query})
	if requests.StatusCode != http.StatusOK || !strings.Contains(string(requests.Body), `"provider_model":"gpt-5.4"`) {
		t.Fatalf("requests = status %d body %s", requests.StatusCode, requests.Body)
	}

	priceBody := `{"revision":0,"prices":{"gpt-5.4":{"input":1,"output":2,"cache_read":0.5,"cache_creation":0.75,"accounting_mode":"input_includes_cache"}},"sync_settings":{"provider_priority":["openai"],"ignored_suffixes":["(high)"],"mappings":[]}}`
	prices := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodPut, Path: modelRouterUsageBasePath + "/prices", Body: []byte(priceBody)})
	if prices.StatusCode != http.StatusOK || !strings.Contains(string(prices.Body), `"revision":1`) {
		t.Fatalf("prices = status %d body %s", prices.StatusCode, prices.Body)
	}
	stale := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodPut, Path: modelRouterUsageBasePath + "/prices", Body: []byte(priceBody)})
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale price status = %d body %s", stale.StatusCode, stale.Body)
	}

	preferences := defaultDashboardPreferences()
	preferences.RequestPageSize = 25
	preferencesBody, _ := json.Marshal(preferences)
	savedPreferences := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodPut, Path: modelRouterUsageBasePath + "/preferences", Body: preferencesBody})
	if savedPreferences.StatusCode != http.StatusOK || !strings.Contains(string(savedPreferences.Body), `"request_page_size":25`) {
		t.Fatalf("preferences = status %d body %s", savedPreferences.StatusCode, savedPreferences.Body)
	}
	reset := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodPost, Path: modelRouterUsageBasePath + "/reset", Body: []byte(`{"confirm":"reset"}`)})
	if reset.StatusCode != http.StatusOK {
		t.Fatalf("reset = status %d body %s", reset.StatusCode, reset.Body)
	}
	book, _ := store.QueryPriceBook()
	loadedPreferences, _ := store.QueryPreferences()
	if book.Revision != 1 || loadedPreferences.RequestPageSize != 25 {
		t.Fatalf("reset removed settings: prices=%#v preferences=%#v", book, loadedPreferences)
	}
}

func TestUsageManagementRejectsBadQueriesBodiesAndMethods(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	plugin := &modelRouterPlugin{store: store, attribution: newAttributionTracker(nil)}
	tests := []pluginapi.ManagementRequest{
		{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/overview", Query: url.Values{"from": {"bad"}}},
		{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/requests", Query: url.Values{"limit": {"501"}}},
		{Method: http.MethodPut, Path: modelRouterUsageBasePath + "/preferences", Body: []byte(`{"request_page_size":100,"surprise":true}`)},
		{Method: http.MethodPost, Path: modelRouterUsageBasePath + "/reset", Body: []byte(`{"confirm":"no"}`)},
	}
	for _, request := range tests {
		response := handleModelRouterManagement(plugin, request)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("request %#v status = %d body %s", request, response.StatusCode, response.Body)
		}
	}
	response := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodPost, Path: modelRouterUsageBasePath + "/overview"})
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d body %s", response.StatusCode, response.Body)
	}
}
