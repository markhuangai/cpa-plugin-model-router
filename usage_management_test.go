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

func TestUsageManagementDashboardQueryOptions(t *testing.T) {
	store, filter := usageQueryFixture(t)
	plugin := &modelRouterPlugin{store: store}
	query := url.Values{
		"from": {filter.From.Format(time.RFC3339Nano)}, "to": {filter.To.Format(time.RFC3339Nano)}, "granularity": {"minute"},
		"source": {"GATEWAY"}, "group_dimension": {"router_model"}, "group_sort": {"key"}, "group_order": {"asc"}, "group_offset": {"1"}, "group_limit": {"3"},
		"request_sort": {"cost"}, "request_order": {"asc"}, "request_offset": {"2"}, "request_limit": {"4"},
	}
	response := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/dashboard", Query: query})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dashboard returned %d: %s", response.StatusCode, response.Body)
	}
	var dashboard usageDashboard
	if err := json.Unmarshal(response.Body, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.Overview.Summary.Requests != 48 || dashboard.Requests.Total != 48 || len(dashboard.Requests.Items) != 4 || dashboard.Requests.Offset != 2 || dashboard.Requests.Limit != 4 || dashboard.Groups.Dimension != "router_model" || dashboard.Groups.Offset != 1 || dashboard.Groups.Limit != 3 {
		t.Fatalf("query options were not applied: %#v", dashboard)
	}
	filter.Source = "GATEWAY"
	want, err := store.referenceUsageRequests(filter, "cost", "asc", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	for index, item := range dashboard.Requests.Items {
		if item.Sequence != want.Items[index].Sequence {
			t.Fatal("request ordering did not use the prefixed options")
		}
	}
	if dashboard.GeneratedAt != dashboard.Overview.GeneratedAt || dashboard.GeneratedAt != dashboard.Groups.GeneratedAt || dashboard.GeneratedAt != dashboard.Requests.GeneratedAt || dashboard.PriceBookRevision != dashboard.Requests.PriceBookRevision {
		t.Fatal("dashboard response has inconsistent snapshot metadata")
	}
	if _, err := store.SavePriceBook(saveModelPricesRequest{Revision: dashboard.PriceBookRevision, Prices: map[string]modelPrice{"model-a": {tokenRates: tokenRates{Input: 10}}}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response = handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/dashboard", Query: query})
	if err := json.Unmarshal(response.Body, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.PriceBookRevision != 2 || dashboard.Requests.PriceBookRevision != 2 || dashboard.Overview.Costs.UnpricedRequests == 0 {
		t.Fatal("new price revision was not applied to the whole dashboard")
	}
}

func TestUsageManagementDashboardRejectsInvalidOptions(t *testing.T) {
	store, _ := usageQueryFixture(t)
	plugin := &modelRouterPlugin{store: store}
	for _, test := range []struct{ key, value string }{
		{"granularity", "week"}, {"from", "invalid"}, {"group_dimension", "api_key"}, {"group_sort", "time"}, {"group_order", "sideways"}, {"group_offset", "-1"}, {"group_limit", "501"},
		{"request_sort", "requests"}, {"request_order", "sideways"}, {"request_offset", "-1"}, {"request_limit", "0"},
	} {
		t.Run(test.key, func(t *testing.T) {
			response := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/dashboard", Query: url.Values{test.key: {test.value}}})
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid option returned %d: %s", response.StatusCode, response.Body)
			}
		})
	}
	response := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodPost, Path: modelRouterUsageBasePath + "/dashboard"})
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported method returned %d", response.StatusCode)
	}
}

func TestUsageManagementNamespacesRouterAliasesAndAttribution(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	for _, record := range []storedUsageRecord{
		{RequestedAt: now, Attribution: attributionRouted, RouterModel: "direct", ProviderModel: "routed-direct"},
		{RequestedAt: now, Attribution: attributionDirect, ProviderModel: "provider-direct"},
		{RequestedAt: now, Attribution: attributionRouted, RouterModel: "unattributed", ProviderModel: "routed-unattributed"},
		{RequestedAt: now, Attribution: attributionUnresolved, ProviderModel: "provider-unattributed"},
	} {
		if err := store.Record(record); err != nil {
			t.Fatal(err)
		}
	}
	plugin := &modelRouterPlugin{store: store, attribution: newAttributionTracker(nil)}
	baseQuery := func() url.Values {
		return url.Values{"from": {now.Add(-time.Hour).Format(time.RFC3339)}, "to": {now.Add(time.Hour).Format(time.RFC3339)}}
	}
	for _, test := range []struct {
		name            string
		parameter       string
		value           string
		wantAttribution string
		wantRouterModel string
	}{
		{name: "direct alias", parameter: "router_model", value: "direct", wantAttribution: attributionRouted, wantRouterModel: "direct"},
		{name: "direct traffic", parameter: "attribution", value: attributionDirect, wantAttribution: attributionDirect},
		{name: "unattributed alias", parameter: "router_model", value: "unattributed", wantAttribution: attributionRouted, wantRouterModel: "unattributed"},
		{name: "unattributed traffic", parameter: "attribution", value: attributionUnresolved, wantAttribution: attributionUnresolved},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := baseQuery()
			query.Set(test.parameter, test.value)
			response := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/requests", Query: query})
			var page usageRequestPage
			if err := json.Unmarshal(response.Body, &page); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || page.Total != 1 || page.Items[0].Attribution != test.wantAttribution || page.Items[0].RouterModel != test.wantRouterModel {
				t.Fatalf("filtered requests = status %d page %#v", response.StatusCode, page)
			}
		})
	}

	overviewResponse := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/overview", Query: baseQuery()})
	var overview usageOverview
	if err := json.Unmarshal(overviewResponse.Body, &overview); err != nil {
		t.Fatal(err)
	}
	seen := make(map[usageRouterKey]uint64, len(overview.RouterModels))
	for _, item := range overview.RouterModels {
		seen[usageRouterKey{model: item.Model, attribution: item.Attribution}] = item.Requests
	}
	for _, key := range []usageRouterKey{
		{model: "direct", attribution: attributionRouted},
		{attribution: attributionDirect},
		{model: "unattributed", attribution: attributionRouted},
		{attribution: attributionUnresolved},
	} {
		if seen[key] != 1 {
			t.Fatalf("router overview buckets = %#v", seen)
		}
	}

	groupQuery := baseQuery()
	groupQuery.Set("dimension", "router_model")
	groupsResponse := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/groups", Query: groupQuery})
	var groups usageGroupPage
	if err := json.Unmarshal(groupsResponse.Body, &groups); err != nil {
		t.Fatal(err)
	}
	if groupsResponse.StatusCode != http.StatusOK || groups.Total != 4 {
		t.Fatalf("router groups = status %d page %#v", groupsResponse.StatusCode, groups)
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

func TestUsageOverviewListsResultsOutsideRequestPage(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	for _, record := range []storedUsageRecord{
		{RequestedAt: now.Add(-time.Minute), Attribution: attributionDirect, ProviderModel: "model", Failed: true, StatusCode: http.StatusTooManyRequests},
		{RequestedAt: now, Attribution: attributionDirect, ProviderModel: "model"},
	} {
		if err := store.Record(record); err != nil {
			t.Fatal(err)
		}
	}
	plugin := &modelRouterPlugin{store: store, attribution: newAttributionTracker(nil)}
	query := url.Values{
		"from":  {now.Add(-time.Hour).Format(time.RFC3339)},
		"to":    {now.Add(time.Hour).Format(time.RFC3339)},
		"limit": {"1"},
	}
	requests := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/requests", Query: query})
	if !strings.Contains(string(requests.Body), `"result":"success"`) || strings.Contains(string(requests.Body), `"result":"http_429"`) {
		t.Fatalf("first request page = %s", requests.Body)
	}
	overview := handleModelRouterManagement(plugin, pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelRouterUsageBasePath + "/overview", Query: query})
	var body struct {
		Results []string `json:"results"`
	}
	if err := json.Unmarshal(overview.Body, &body); err != nil {
		t.Fatal(err)
	}
	if strings.Join(body.Results, ",") != "http_429,success" {
		t.Fatalf("overview results = %#v", body.Results)
	}
}
