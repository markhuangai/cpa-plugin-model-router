package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	modelRouterDashboardPath  = "/v0/resource/plugins/" + pluginID + "/config"
	modelRouterValidationPath = "/v0/management/plugins/" + pluginID + "/validate"
	modelRouterUsageBasePath  = "/v0/management/plugins/" + pluginID + "/usage"
	maxManagementRequestBytes = 1 << 20
)

type managementRPCRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type modelRouterValidationRequest struct {
	Enabled *bool            `json:"enabled"`
	Routes  []modelRouteYAML `json:"routes"`
}

func modelRouterManagementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodPost, Path: "/plugins/" + pluginID + "/validate", Description: "Validate Model Router configuration before saving it."},
			{Method: http.MethodGet, Path: "/plugins/" + pluginID + "/usage/overview", Description: "Read usage summaries, trends, costs, and model breakdowns."},
			{Method: http.MethodGet, Path: "/plugins/" + pluginID + "/usage/groups", Description: "Read paginated usage dimension groups."},
			{Method: http.MethodGet, Path: "/plugins/" + pluginID + "/usage/requests", Description: "Read paginated request-level usage."},
			{Method: http.MethodGet, Path: "/plugins/" + pluginID + "/usage/prices", Description: "Read the persisted model price book."},
			{Method: http.MethodPut, Path: "/plugins/" + pluginID + "/usage/prices", Description: "Persist a revision-protected model price book."},
			{Method: http.MethodPost, Path: "/plugins/" + pluginID + "/usage/prices/sync", Description: "Synchronize CPA model prices from models.dev."},
			{Method: http.MethodGet, Path: "/plugins/" + pluginID + "/usage/preferences", Description: "Read usage dashboard preferences."},
			{Method: http.MethodPut, Path: "/plugins/" + pluginID + "/usage/preferences", Description: "Persist usage dashboard preferences."},
			{Method: http.MethodPost, Path: "/plugins/" + pluginID + "/usage/reset", Description: "Clear usage aggregates and request history while preserving prices and preferences."},
		},
		Resources: []resourceRoute{{
			Path:        "/config",
			Menu:        "Model Router",
			Description: "Configure logical aliases and ordered model target pools.",
		}},
	}
}

func handleModelRouterManagement(plugin *modelRouterPlugin, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(strings.TrimSpace(request.Path), "/")
	if strings.HasPrefix(path, modelRouterUsageBasePath+"/") {
		return handleUsageManagement(plugin, path, request)
	}
	switch path {
	case modelRouterDashboardPath:
		if !strings.EqualFold(request.Method, http.MethodGet) {
			return modelRouterJSONResponse(http.StatusMethodNotAllowed, map[string]any{
				"error":   "method_not_allowed",
				"message": "dashboard only supports GET",
			})
		}
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-store"},
				"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'"},
				"Referrer-Policy":         {"no-referrer"},
				"X-Content-Type-Options":  {"nosniff"},
			},
			Body: append([]byte(nil), configDashboardHTML...),
		}
	case modelRouterValidationPath:
		if !strings.EqualFold(request.Method, http.MethodPost) {
			return modelRouterJSONResponse(http.StatusMethodNotAllowed, map[string]any{
				"error":   "method_not_allowed",
				"message": "validation only supports POST",
			})
		}
		return validateModelRouterManagementConfig(request.Body)
	default:
		return modelRouterJSONResponse(http.StatusNotFound, map[string]any{
			"error":   "not_found",
			"message": "management resource not found",
		})
	}
}

func validateModelRouterManagementConfig(body []byte) pluginapi.ManagementResponse {
	if len(body) > maxManagementRequestBytes {
		return modelRouterJSONResponse(http.StatusRequestEntityTooLarge, map[string]any{
			"valid":   false,
			"error":   "request_too_large",
			"message": "configuration exceeds the 1 MiB validation limit",
		})
	}
	var request modelRouterValidationRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return modelRouterJSONResponse(http.StatusBadRequest, map[string]any{
			"valid":   false,
			"error":   "invalid_request",
			"message": err.Error(),
		})
	}
	if err := ensureModelRouterJSONEOF(decoder); err != nil {
		return modelRouterJSONResponse(http.StatusBadRequest, map[string]any{
			"valid":   false,
			"error":   "invalid_request",
			"message": err.Error(),
		})
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	wire := struct {
		Enabled bool             `yaml:"enabled"`
		Routes  []modelRouteYAML `yaml:"routes"`
	}{Enabled: enabled, Routes: request.Routes}
	raw, err := yaml.Marshal(wire)
	if err != nil {
		return modelRouterJSONResponse(http.StatusInternalServerError, map[string]any{
			"valid":   false,
			"error":   "validation_failed",
			"message": err.Error(),
		})
	}
	if _, err := decodeRouterConfig(raw); err != nil {
		return modelRouterJSONResponse(http.StatusBadRequest, map[string]any{
			"valid":   false,
			"error":   "invalid_config",
			"message": err.Error(),
		})
	}
	return modelRouterJSONResponse(http.StatusOK, map[string]any{
		"valid":       true,
		"route_count": len(request.Routes),
	})
}

func ensureModelRouterJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not supported")
		}
		return err
	}
	return nil
}

func modelRouterJSONResponse(status int, value any) pluginapi.ManagementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"response_encode_failed"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body,
	}
}
