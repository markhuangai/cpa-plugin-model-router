package main

import (
	"fmt"
	"sort"
	"strings"
)

const maxDashboardPageSize = 500

var defaultHiddenGroupColumns = []string{"provider", "result", "router_model", "service_tier", "source"}

type dashboardPreferences struct {
	RequestPageSize      int      `json:"request_page_size"`
	GroupPageSize        int      `json:"group_page_size"`
	HiddenRequestColumns []string `json:"hidden_request_columns"`
	HiddenGroupColumns   []string `json:"hidden_group_columns"`
	TimeRange            string   `json:"time_range"`
	Granularity          string   `json:"granularity"`
	RequestSort          string   `json:"request_sort"`
	RequestOrder         string   `json:"request_order"`
	GroupDimension       string   `json:"group_dimension"`
	GroupSort            string   `json:"group_sort"`
	GroupOrder           string   `json:"group_order"`
	HiddenTokenSeries    []string `json:"hidden_token_series"`
	CustomFrom           string   `json:"custom_from,omitempty"`
	CustomTo             string   `json:"custom_to,omitempty"`
}

func defaultDashboardPreferences() dashboardPreferences {
	return dashboardPreferences{
		RequestPageSize:    50,
		GroupPageSize:      50,
		HiddenGroupColumns: append([]string(nil), defaultHiddenGroupColumns...),
		TimeRange:          "24h",
		Granularity:        "hour",
		RequestSort:        "time",
		RequestOrder:       "desc",
		GroupDimension:     "provider_model",
		GroupSort:          "total_tokens",
		GroupOrder:         "desc",
	}
}

func normalizeDashboardPreferences(input dashboardPreferences) (dashboardPreferences, error) {
	defaults := defaultDashboardPreferences()
	if input.RequestPageSize == 0 {
		input.RequestPageSize = defaults.RequestPageSize
	}
	if input.GroupPageSize == 0 {
		input.GroupPageSize = defaults.GroupPageSize
	}
	if input.HiddenGroupColumns == nil {
		input.HiddenGroupColumns = append([]string(nil), defaults.HiddenGroupColumns...)
	}
	if input.RequestPageSize < 1 || input.RequestPageSize > maxDashboardPageSize || input.GroupPageSize < 1 || input.GroupPageSize > maxDashboardPageSize {
		return dashboardPreferences{}, fmt.Errorf("dashboard page sizes must be between 1 and %d", maxDashboardPageSize)
	}
	input.TimeRange = defaultAllowed(input.TimeRange, defaults.TimeRange, "5h", "24h", "7d", "30d", "month", "custom")
	input.Granularity = defaultAllowed(input.Granularity, defaults.Granularity, "minute", "hour", "day")
	input.RequestSort = defaultAllowed(input.RequestSort, defaults.RequestSort, requestSortFields...)
	input.RequestOrder = defaultAllowed(input.RequestOrder, defaults.RequestOrder, "asc", "desc")
	input.GroupDimension = defaultAllowed(input.GroupDimension, defaults.GroupDimension, groupDimensions...)
	input.GroupSort = defaultAllowed(input.GroupSort, defaults.GroupSort, groupSortFields...)
	input.GroupOrder = defaultAllowed(input.GroupOrder, defaults.GroupOrder, "asc", "desc")
	input.CustomFrom = strings.TrimSpace(input.CustomFrom)
	input.CustomTo = strings.TrimSpace(input.CustomTo)
	var err error
	if input.HiddenRequestColumns, err = normalizeColumnList(input.HiddenRequestColumns, requestColumnFields); err != nil {
		return dashboardPreferences{}, fmt.Errorf("hidden_request_columns: %w", err)
	}
	if input.HiddenGroupColumns, err = normalizeColumnList(input.HiddenGroupColumns, groupColumnFields); err != nil {
		return dashboardPreferences{}, fmt.Errorf("hidden_group_columns: %w", err)
	}
	if input.HiddenTokenSeries, err = normalizeColumnList(input.HiddenTokenSeries, []string{"input", "output", "cache_read", "reasoning"}); err != nil {
		return dashboardPreferences{}, fmt.Errorf("hidden_token_series: %w", err)
	}
	return input, nil
}

func defaultAllowed(value, fallback string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return fallback
}

func normalizeColumnList(input, allowed []string) ([]string, error) {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, value := range allowed {
		allowedSet[value] = struct{}{}
	}
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, value := range input {
		value = strings.TrimSpace(value)
		if _, ok := allowedSet[value]; !ok {
			return nil, fmt.Errorf("unsupported column %q", value)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) >= len(allowed) && len(allowed) > 0 {
		return nil, fmt.Errorf("at least one column must remain visible")
	}
	sort.Strings(result)
	return result, nil
}

var requestColumnFields = []string{
	"time", "router_model", "provider_model", "provider", "source", "service_tier", "result", "latency", "ttft", "tps",
	"input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens", "cost", "api_key",
}

var groupColumnFields = []string{
	"key", "router_model", "provider_model", "provider", "source", "service_tier", "result", "requests", "failed_requests",
	"input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens", "latency", "ttft", "tps", "cost",
}

var requestSortFields = []string{"time", "router_model", "provider_model", "source", "service_tier", "result", "latency", "ttft", "tps", "total_tokens", "cost"}
var groupSortFields = []string{"key", "requests", "failed_requests", "input_tokens", "output_tokens", "total_tokens", "latency", "ttft", "tps", "cost"}
var groupDimensions = []string{"router_model", "provider_model", "provider", "source", "service_tier", "result"}
