package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestModelRouterABIAdvertisesConfigurationResource(t *testing.T) {
	resetModelRouterABIState(t)
	lifecycle, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("routes: []\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodPluginRegister, lifecycle); err != nil {
		t.Fatalf("plugin.register error = %v", err)
	}
	raw, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("management.register error = %v", err)
	}
	var envelope abiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode management envelope: %v", err)
	}
	var management managementRegistrationResponse
	if err := json.Unmarshal(envelope.Result, &management); err != nil {
		t.Fatalf("decode management result: %v", err)
	}
	if len(management.Resources) != 1 || management.Resources[0].Path != "/config" || management.Resources[0].Menu != "Model Router" {
		t.Fatalf("resources = %#v", management.Resources)
	}
	wantedRoutes := map[string]bool{
		http.MethodPost + " /plugins/model-router/validate":          false,
		http.MethodGet + " /plugins/model-router/usage/overview":     false,
		http.MethodGet + " /plugins/model-router/usage/groups":       false,
		http.MethodGet + " /plugins/model-router/usage/requests":     false,
		http.MethodGet + " /plugins/model-router/usage/prices":       false,
		http.MethodPut + " /plugins/model-router/usage/prices":       false,
		http.MethodPost + " /plugins/model-router/usage/prices/sync": false,
		http.MethodGet + " /plugins/model-router/usage/preferences":  false,
		http.MethodPut + " /plugins/model-router/usage/preferences":  false,
		http.MethodPost + " /plugins/model-router/usage/reset":       false,
	}
	for _, route := range management.Routes {
		key := route.Method + " " + route.Path
		if _, wanted := wantedRoutes[key]; wanted {
			wantedRoutes[key] = true
		}
	}
	for route, found := range wantedRoutes {
		if !found {
			t.Fatalf("management routes missing %s: %#v", route, management.Routes)
		}
	}
}

func TestModelRouterManagementDashboardReusesCPAMCSessionAndTheme(t *testing.T) {
	request, err := json.Marshal(managementRPCRequest{ManagementRequest: pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   modelRouterDashboardPath,
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleModelRouterABIMethod(t.Context(), pluginabi.MethodManagementHandle, request)
	if err != nil {
		t.Fatalf("management.handle error = %v", err)
	}
	var envelope abiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("response status=%d headers=%v", response.StatusCode, response.Headers)
	}
	page := string(response.Body)
	for _, required := range []string{
		"<title>Model Router</title>",
		"/v0/management/plugins/model-router/config",
		"/v0/management/plugins/model-router/validate",
		"/v0/management/api-keys",
		"/v1/models",
		"headers.Authorization='Bearer '+key",
		"'cli-proxy-auth'",
		"CPA_STORAGE_PREFIX='enc::v1::'",
		":root[data-host-theme=\"dark\"]",
		"new MutationObserver(refresh)",
		"class=\"auth-dock\" aria-labelledby=\"auth-title\" hidden",
		"if(managementKey())loadConfiguration()",
		"<select data-target-field=\"model\"",
		"model+' (unavailable)'",
		"data-action=\"add-target\"",
		"role=\"tablist\" aria-label=\"Model Router sections\"",
		"id=\"configuration-tab\" class=\"page-tab\" type=\"button\" role=\"tab\" aria-selected=\"true\"",
		"id=\"usage-tab\" class=\"page-tab\" type=\"button\" role=\"tab\" aria-selected=\"false\"",
		"id=\"configuration-panel\" class=\"tab-panel\" role=\"tabpanel\"",
		"id=\"usage-panel\" class=\"tab-panel usage-panel\" role=\"tabpanel\"",
		"/v0/management/plugins/model-router/usage",
		"requestManagementJSON(USAGE_API+'/overview?'",
		"requestManagementJSON(USAGE_API+'/groups?'",
		"requestManagementJSON(USAGE_API+'/requests?'",
		"requestManagementJSON(freshManagementURL(USAGE_API+'/prices'),{cache:'no-store'})",
		"const ROUTER_FILTER_MODEL_PREFIX='model:'",
		"const ROUTER_FILTER_ATTRIBUTION_PREFIX='attribution:'",
		"const DEFAULT_HIDDEN_GROUP_COLUMNS=['provider','result','router_model','service_tier','source']",
		"['attribution',usageState.attribution]",
		"@media (max-width: 920px) {",
		".price-grid .price-model-field { grid-column: 1 / -1; }",
		".price-grid > * { min-width: 0; }",
		".price-source { display: block; min-width: 0;",
		"function effectiveCacheReadValue(value)",
		"effective_cache_read_tokens",
		"document.querySelectorAll('#pricing-dialog input, #pricing-dialog select, #pricing-dialog textarea, #pricing-dialog button')",
		"function flushUsagePreferencesSave(generation)",
		"preferenceSaveInFlight",
		"preferenceSaveGeneration",
		"custom_from:usageState.customFrom",
		"custom_to:usageState.customTo",
		"const controller=new AbortController()",
		"generation!==usageState.generation",
		"document.addEventListener('visibilitychange'",
		"window.setTimeout(()=>refreshUsage(false),15000)",
		"if(value&&value.attribution==='direct')return '—'",
		"if(value&&value.attribution==='unattributed')return 'Unattributed'",
		"id=\"token-chart\" class=\"chart-canvas\" role=\"img\"",
		"id=\"model-chart\"",
		"id=\"cost-chart\"",
		"id=\"efficiency-chart\"",
		"id=\"chart-tooltip\" class=\"chart-tooltip\" role=\"tooltip\"",
		"function initializeChartInteractions()",
		"function initializeDashboardResizeObserver()",
		"badge.className='status-pill '+(result==='success'?'success':'failure')",
		"applyPriceBook(book);closePricingDialog();showToast('Model pricing saved.'",
		"id=\"pricing-dialog\"",
		"id=\"reset-dialog\"",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("dashboard missing %q", required)
		}
	}
	refreshStart := strings.Index(page, "async function refreshUsage")
	refreshEndRelative := -1
	if refreshStart >= 0 {
		refreshEndRelative = strings.Index(page[refreshStart:], "function validHiddenColumns")
	}
	refreshEnd := refreshStart + refreshEndRelative
	if refreshStart < 0 || refreshEnd < 0 || strings.Contains(page[refreshStart:refreshEnd], "resetChartInteractions()") {
		t.Fatal("refreshUsage must preserve active chart interactions while redrawing data")
	}
	for _, required := range []string{
		"pricingDialogGeneration:0",
		"pricingWriteInFlight:null",
		"const dialogGeneration=++usageState.pricingDialogGeneration",
		"const pendingWrite=usageState.pricingWriteInFlight",
		"if(pendingWrite){try{await pendingWrite}catch(_error){}}",
		"const writeRequest=requestManagementJSON(USAGE_API+'/prices'",
		"if(usageState.pricingWriteInFlight===writeRequest)usageState.pricingWriteInFlight=null",
		"const writeRequest=requestManagementJSON(USAGE_API+'/prices/sync'",
		"if(dialogGeneration!==usageState.pricingDialogGeneration)return",
		"if(dialogGeneration===usageState.pricingDialogGeneration&&pricingDialogEl.open)",
		"const chartActive={id:'',index:-1,anchor:null,key:null}",
		"function chartItemKey(id,item)",
		"function remapChartActive(id,items)",
		"focusedKey",
		"if(entry)entry.focus({preventScroll:true})",
		"key:'model:'+item.model",
		"key:'aggregate:other'",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("dashboard missing async pricing guard %q", required)
		}
	}
	for _, forbidden := range []string{"http://", "https://", "innerHTML", "<input type=\"text\" data-target-field=\"model\""} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("dashboard contains forbidden text %q", forbidden)
		}
	}
	if csp := response.Headers.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", csp)
	}
}

func TestModelRouterManagementValidationUsesPluginParser(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantText   string
	}{
		{
			name:       "valid",
			body:       `{"enabled":true,"routes":[{"alias":"auto","strategy":"priority","cooldown_seconds":60,"models":["provider-a/model","provider-b/model"]}]}`,
			wantStatus: http.StatusOK,
			wantText:   `"valid":true`,
		},
		{
			name:       "recursive target",
			body:       `{"enabled":true,"routes":[{"alias":"auto","strategy":"priority","cooldown_seconds":60,"models":["auto(high)"]}]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "target must not reference route alias",
		},
		{
			name:       "empty target pool",
			body:       `{"enabled":true,"routes":[{"alias":"auto","strategy":"priority","cooldown_seconds":60,"models":[]}]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "at least one model is required",
		},
		{
			name:       "unknown route field",
			body:       `{"enabled":true,"routes":[{"alias":"auto","models":["provider/model"],"unknown":true}]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "unknown field",
		},
		{
			name:       "multiple documents",
			body:       `{"enabled":true,"routes":[]} {"enabled":true,"routes":[]}`,
			wantStatus: http.StatusBadRequest,
			wantText:   "multiple JSON values",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := handleModelRouterManagement(nil, pluginapi.ManagementRequest{
				Method: http.MethodPost,
				Path:   modelRouterValidationPath,
				Body:   []byte(test.body),
			})
			if response.StatusCode != test.wantStatus || !strings.Contains(string(response.Body), test.wantText) {
				t.Fatalf("status=%d body=%s, want status=%d containing %q", response.StatusCode, response.Body, test.wantStatus, test.wantText)
			}
		})
	}
}

func TestModelRouterManagementRejectsUnsupportedMethod(t *testing.T) {
	response := handleModelRouterManagement(nil, pluginapi.ManagementRequest{Method: http.MethodDelete, Path: modelRouterDashboardPath})
	if response.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(string(response.Body), "method_not_allowed") {
		t.Fatalf("response = %#v, body=%s", response, response.Body)
	}
}
