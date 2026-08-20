package main

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type modelHost interface {
	Execute(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error)
	StartStream(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error)
	ReadStream(string) (pluginapi.HostModelStreamReadResponse, error)
	CloseStream(string) error
	Emit(string, []byte) error
	ClosePluginStream(string, string)
}

func (p *modelRouterPlugin) executeWithHost(request pluginapi.ExecutorRequest, host modelHost) (pluginapi.ExecutorResponse, error) {
	if p == nil || host == nil {
		return pluginapi.ExecutorResponse{}, statusError{status: http.StatusBadGateway, message: "model router host callback is unavailable"}
	}
	route, ok := p.matchingRoute(request.Model)
	if !ok {
		return pluginapi.ExecutorResponse{}, statusError{status: http.StatusBadGateway, message: "no model route matched executor request"}
	}
	requestedModel := strings.TrimSpace(request.Model)
	bodyInfo := bodyForExecution(request)
	var lastErr error
	for attempt := 0; attempt < len(route.Models); attempt++ {
		selection := p.runtime.Select(route)
		if selection.allCooling {
			return pluginapi.ExecutorResponse{}, cooldownRouteError(route, selection.retryAfter)
		}
		if selection.model == "" {
			break
		}
		target := targetModel(requestedModel, selection.model)
		startedAt := time.Now().UTC()
		capture := newRoutedUsageCapture(request, target, startedAt)
		mark := p.attribution.MarkRouted(route.Alias, target, request.Headers, capture)
		response, err := host.Execute(hostRequest(request, bodyInfo, target, false))
		status := response.StatusCode
		if status == 0 && err == nil {
			status = http.StatusOK
		}
		if status == 0 && err != nil {
			status = statusFromError(err)
		}
		capture.observePayload(response.Body)
		capture.finishAttempt(status, err != nil || status < 200 || status >= 300, time.Now().UTC())
		p.recordUsageFallback(mark, capture)
		if err == nil && status >= 200 && status < 300 {
			return pluginapi.ExecutorResponse{
				Payload: rewriteResponseModel(response.Body, requestedModel),
				Headers: cloneHeader(response.Headers),
				Metadata: map[string]any{
					"route_alias":    route.Alias,
					"selected_model": target,
					"selected_index": attempt,
				},
			}, nil
		}
		if err == nil {
			err = statusError{status: status, message: hostStatusMessage(target, status, response.Body)}
		}
		if !eligibleRouteFailure(err) {
			return pluginapi.ExecutorResponse{}, err
		}
		lastErr = err
		p.runtime.MarkFailure(route, selection.model)
	}
	detail := ""
	if lastErr != nil {
		detail = lastErr.Error()
	}
	return pluginapi.ExecutorResponse{}, newRouteError(http.StatusServiceUnavailable, "model_route_unavailable", route.Alias, fmt.Sprintf("no available candidate for model route %q", route.Alias), detail)
}

func hostRequest(request pluginapi.ExecutorRequest, bodyInfo executionBody, model string, stream bool) pluginapi.HostModelExecutionRequest {
	return pluginapi.HostModelExecutionRequest{
		EntryProtocol: bodyInfo.entryProtocol,
		ExitProtocol:  bodyInfo.responseProtocol,
		Model:         model,
		Stream:        stream,
		Body:          requestBodyForTarget(bodyInfo.body, model),
		Headers:       sanitizeNestedHeaders(request.Headers),
		Query:         cloneValues(request.Query),
		Alt:           request.Alt,
	}
}

func hostStatusMessage(model string, status int, body []byte) string {
	message := fmt.Sprintf("host model %s returned status %d", model, status)
	detail := strings.TrimSpace(string(body))
	if len(detail) > 512 {
		detail = detail[:512]
	}
	if detail != "" {
		message += ": " + detail
	}
	return message
}

func cooldownRouteError(route modelRoute, retryAfter time.Duration) error {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return newRouteError(http.StatusTooManyRequests, "model_route_cooldown", route.Alias, fmt.Sprintf("all candidates for model route %q are cooling down", route.Alias), fmt.Sprintf("retry after %d seconds", seconds))
}
