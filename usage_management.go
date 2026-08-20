package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxPriceRequestBytes = 2 << 20

func handleUsageManagement(plugin *modelRouterPlugin, path string, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if plugin == nil || plugin.store == nil {
		return modelRouterJSONResponse(http.StatusServiceUnavailable, map[string]string{"error": "usage_storage_unavailable", "message": "usage storage is not initialized"})
	}
	switch path {
	case modelRouterUsageBasePath + "/overview":
		if !strings.EqualFold(request.Method, http.MethodGet) {
			return usageMethodNotAllowed(http.MethodGet)
		}
		filter, err := parseUsageFilter(request.Query)
		if err != nil {
			return usageBadRequest(err)
		}
		granularity, err := validateQueryChoice(request.Query.Get("granularity"), "hour", []string{"minute", "hour", "day"})
		if err != nil {
			return usageBadRequest(fmt.Errorf("granularity: %w", err))
		}
		overview, err := plugin.store.Overview(filter, granularity)
		if err != nil {
			return usageStorageError(err)
		}
		return modelRouterJSONResponse(http.StatusOK, overview)
	case modelRouterUsageBasePath + "/groups":
		if !strings.EqualFold(request.Method, http.MethodGet) {
			return usageMethodNotAllowed(http.MethodGet)
		}
		filter, err := parseUsageFilter(request.Query)
		if err != nil {
			return usageBadRequest(err)
		}
		dimension, err := validateQueryChoice(request.Query.Get("dimension"), "provider_model", groupDimensions)
		if err != nil {
			return usageBadRequest(fmt.Errorf("dimension: %w", err))
		}
		sortField, err := validateQueryChoice(request.Query.Get("sort"), "total_tokens", groupSortFields)
		if err != nil {
			return usageBadRequest(fmt.Errorf("sort: %w", err))
		}
		order, err := validateQueryChoice(request.Query.Get("order"), "desc", []string{"asc", "desc"})
		if err != nil {
			return usageBadRequest(fmt.Errorf("order: %w", err))
		}
		offset, limit, err := parsePagination(request.Query.Get("offset"), request.Query.Get("limit"))
		if err != nil {
			return usageBadRequest(err)
		}
		page, err := plugin.store.Groups(filter, dimension, sortField, order, offset, limit)
		if err != nil {
			return usageStorageError(err)
		}
		return modelRouterJSONResponse(http.StatusOK, page)
	case modelRouterUsageBasePath + "/requests":
		if !strings.EqualFold(request.Method, http.MethodGet) {
			return usageMethodNotAllowed(http.MethodGet)
		}
		filter, err := parseUsageFilter(request.Query)
		if err != nil {
			return usageBadRequest(err)
		}
		sortField, err := validateQueryChoice(request.Query.Get("sort"), "time", requestSortFields)
		if err != nil {
			return usageBadRequest(fmt.Errorf("sort: %w", err))
		}
		order, err := validateQueryChoice(request.Query.Get("order"), "desc", []string{"asc", "desc"})
		if err != nil {
			return usageBadRequest(fmt.Errorf("order: %w", err))
		}
		offset, limit, err := parsePagination(request.Query.Get("offset"), request.Query.Get("limit"))
		if err != nil {
			return usageBadRequest(err)
		}
		page, err := plugin.store.Requests(filter, sortField, order, offset, limit)
		if err != nil {
			return usageStorageError(err)
		}
		return modelRouterJSONResponse(http.StatusOK, page)
	case modelRouterUsageBasePath + "/prices":
		return handleUsagePrices(plugin, request)
	case modelRouterUsageBasePath + "/prices/sync":
		return handleUsagePriceSync(plugin, request)
	case modelRouterUsageBasePath + "/preferences":
		return handleUsagePreferences(plugin, request)
	case modelRouterUsageBasePath + "/reset":
		return handleUsageReset(plugin, request)
	default:
		return modelRouterJSONResponse(http.StatusNotFound, map[string]string{"error": "not_found", "message": "usage management resource not found"})
	}
}

func handleUsagePrices(plugin *modelRouterPlugin, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch {
	case strings.EqualFold(request.Method, http.MethodGet):
		book, err := plugin.store.QueryPriceBook()
		if err != nil {
			return usageStorageError(err)
		}
		return modelRouterJSONResponse(http.StatusOK, book)
	case strings.EqualFold(request.Method, http.MethodPut):
		var input saveModelPricesRequest
		if err := decodeManagementJSON(request.Body, maxPriceRequestBytes, &input); err != nil {
			return usageBadRequest(err)
		}
		book, err := plugin.store.SavePriceBook(input, time.Now().UTC())
		if errors.Is(err, errPriceRevisionConflict) {
			return modelRouterJSONResponse(http.StatusConflict, map[string]string{"error": "revision_conflict", "message": "model prices changed; reload before saving"})
		}
		if err != nil {
			return usageBadRequest(err)
		}
		return modelRouterJSONResponse(http.StatusOK, book)
	default:
		return usageMethodNotAllowed(http.MethodGet, http.MethodPut)
	}
}

func handleUsagePriceSync(plugin *modelRouterPlugin, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if !strings.EqualFold(request.Method, http.MethodPost) {
		return usageMethodNotAllowed(http.MethodPost)
	}
	var input syncModelPricesRequest
	if err := decodeManagementJSON(request.Body, maxPriceRequestBytes, &input); err != nil {
		return usageBadRequest(err)
	}
	if source := strings.TrimSpace(input.Source); source != "" && source != priceSourceModelsDev {
		return usageBadRequest(errors.New(`source must be "models.dev"`))
	}
	book, err := plugin.syncModelsDev(input)
	if errors.Is(err, errPriceRevisionConflict) {
		return modelRouterJSONResponse(http.StatusConflict, map[string]string{"error": "revision_conflict", "message": "model prices changed; reload before synchronizing"})
	}
	if err != nil {
		message := err.Error()
		status := http.StatusBadRequest
		code := "invalid_sync_request"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			status, code, message = http.StatusGatewayTimeout, "models_dev_timeout", "models.dev synchronization timed out"
		} else if strings.Contains(message, "models.dev") || strings.Contains(message, "catalog") {
			status, code, message = http.StatusBadGateway, "models_dev_failed", "models.dev synchronization failed"
		} else if strings.Contains(message, "already running") {
			status, code = http.StatusConflict, "sync_in_progress"
		}
		return modelRouterJSONResponse(status, map[string]string{"error": code, "message": message})
	}
	return modelRouterJSONResponse(http.StatusOK, book)
}

func handleUsagePreferences(plugin *modelRouterPlugin, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch {
	case strings.EqualFold(request.Method, http.MethodGet):
		preferences, err := plugin.store.QueryPreferences()
		if err != nil {
			return usageStorageError(err)
		}
		return modelRouterJSONResponse(http.StatusOK, preferences)
	case strings.EqualFold(request.Method, http.MethodPut):
		var input dashboardPreferences
		if err := decodeManagementJSON(request.Body, maxManagementRequestBytes, &input); err != nil {
			return usageBadRequest(err)
		}
		preferences, err := plugin.store.SavePreferences(input)
		if err != nil {
			return usageBadRequest(err)
		}
		return modelRouterJSONResponse(http.StatusOK, preferences)
	default:
		return usageMethodNotAllowed(http.MethodGet, http.MethodPut)
	}
}

func handleUsageReset(plugin *modelRouterPlugin, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if !strings.EqualFold(request.Method, http.MethodPost) {
		return usageMethodNotAllowed(http.MethodPost)
	}
	var input struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeManagementJSON(request.Body, maxManagementRequestBytes, &input); err != nil {
		return usageBadRequest(err)
	}
	if input.Confirm != "reset" {
		return usageBadRequest(errors.New(`confirm must equal "reset"`))
	}
	if err := plugin.store.ResetUsage(); err != nil {
		return usageStorageError(err)
	}
	return modelRouterJSONResponse(http.StatusOK, map[string]any{"reset": true})
}

func parseUsageFilter(query map[string][]string) (usageFilter, error) {
	now := time.Now().UTC()
	filter := usageFilter{From: now.Add(-24 * time.Hour), To: now}
	var err error
	if raw := strings.TrimSpace(firstQuery(query, "from")); raw != "" {
		filter.From, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return usageFilter{}, fmt.Errorf("from must be RFC3339")
		}
		filter.From = filter.From.UTC()
	}
	if raw := strings.TrimSpace(firstQuery(query, "to")); raw != "" {
		filter.To, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return usageFilter{}, fmt.Errorf("to must be RFC3339")
		}
		filter.To = filter.To.UTC()
	}
	if !filter.From.Before(filter.To) {
		return usageFilter{}, errors.New("from must be earlier than to")
	}
	filter.RouterModel = strings.TrimSpace(firstQuery(query, "router_model"))
	filter.ProviderModel = strings.TrimSpace(firstQuery(query, "provider_model"))
	filter.Source = strings.TrimSpace(firstQuery(query, "source"))
	filter.ServiceTier = strings.TrimSpace(firstQuery(query, "service_tier"))
	filter.Result = strings.TrimSpace(firstQuery(query, "result"))
	return filter, nil
}

func firstQuery(query map[string][]string, name string) string {
	values := query[name]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func parsePagination(offsetText, limitText string) (int, int, error) {
	offset, limit := 0, 100
	var err error
	if strings.TrimSpace(offsetText) != "" {
		offset, err = strconv.Atoi(offsetText)
		if err != nil || offset < 0 {
			return 0, 0, errors.New("offset must be a non-negative integer")
		}
	}
	if strings.TrimSpace(limitText) != "" {
		limit, err = strconv.Atoi(limitText)
		if err != nil || limit < 1 || limit > maxDashboardPageSize {
			return 0, 0, fmt.Errorf("limit must be between 1 and %d", maxDashboardPageSize)
		}
	}
	return offset, limit, nil
}

func decodeManagementJSON(raw []byte, limit int, destination any) error {
	if len(raw) > limit {
		return fmt.Errorf("request exceeds %d bytes", limit)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not supported")
		}
		return err
	}
	return nil
}

func usageBadRequest(err error) pluginapi.ManagementResponse {
	return modelRouterJSONResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request", "message": err.Error()})
}

func usageStorageError(err error) pluginapi.ManagementResponse {
	return modelRouterJSONResponse(http.StatusInternalServerError, map[string]string{"error": "storage_error", "message": err.Error()})
}

func usageMethodNotAllowed(methods ...string) pluginapi.ManagementResponse {
	return modelRouterJSONResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed", "message": "allowed methods: " + strings.Join(methods, ", ")})
}
