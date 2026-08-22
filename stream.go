package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *modelRouterPlugin) startStream(request executorRPCRequest, host modelHost) ([]byte, error) {
	streamID := strings.TrimSpace(request.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream", 0), nil
	}
	go func() {
		errorMessage := ""
		defer func() {
			if recovered := recover(); recovered != nil {
				errorMessage = fmt.Sprintf("stream orchestration panic: %v", recovered)
			}
			host.ClosePluginStream(streamID, errorMessage)
		}()
		if err := p.executeStreamWithHost(context.Background(), request.ExecutorRequest, streamID, host); err != nil {
			errorMessage = err.Error()
		}
	}()
	return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

func (p *modelRouterPlugin) executeStreamWithHost(_ context.Context, request pluginapi.ExecutorRequest, pluginStreamID string, host modelHost) error {
	if p == nil || host == nil {
		return statusError{status: http.StatusBadGateway, message: "model router host callback is unavailable"}
	}
	route, ok := p.matchingRoute(request.Model)
	if !ok {
		return statusError{status: http.StatusBadGateway, message: "no model route matched executor stream request"}
	}
	requestedModel := strings.TrimSpace(request.Model)
	bodyInfo := bodyForExecution(request)
	var lastErr error
	attempted := make(map[string]struct{}, len(route.Targets))
	for attempt := 0; attempt < len(route.Targets); attempt++ {
		selection := p.runtime.SelectExcluding(route, attempted)
		if selection.allCooling {
			return cooldownRouteError(route, selection.retryAfter)
		}
		if selection.model == "" {
			break
		}
		attempted[routeKey(selection.model)] = struct{}{}
		target := targetModel(requestedModel, selection.model)
		capture := newRoutedUsageCapture(request, target, time.Now().UTC())
		mark := p.attribution.MarkRouted(route.Alias, target, request.Headers, capture)
		receivedPayload, err := forwardStreamAttempt(request, bodyInfo, target, requestedModel, pluginStreamID, host, &capture)
		status := capture.statusCode
		if err != nil {
			if errorStatus := statusFromError(err); errorStatus > 0 {
				status = errorStatus
			}
		}
		capture.finishAttempt(status, err != nil, time.Now().UTC())
		p.recordUsageFallback(mark, capture)
		if err == nil {
			return nil
		}
		if eligibleRouteFailure(err) {
			p.runtime.MarkFailure(route, selection.model)
		}
		if receivedPayload || !eligibleRouteFailure(err) {
			return err
		}
		lastErr = err
	}
	detail := ""
	if lastErr != nil {
		detail = lastErr.Error()
	}
	return newRouteError(http.StatusServiceUnavailable, "model_route_unavailable", route.Alias, fmt.Sprintf("no available candidate for model route %q", route.Alias), detail)
}

func forwardStreamAttempt(request pluginapi.ExecutorRequest, bodyInfo executionBody, target, requestedModel, pluginStreamID string, host modelHost, capture *directUsageCapture) (bool, error) {
	response, err := host.StartStream(hostRequest(request, bodyInfo, target, true))
	if err != nil {
		return false, err
	}
	if capture != nil {
		capture.statusCode = response.StatusCode
	}
	if response.StatusCode >= 400 {
		_ = host.CloseStream(response.StreamID)
		return false, statusError{status: response.StatusCode, message: fmt.Sprintf("host model %s stream returned status %d", target, response.StatusCode)}
	}
	if strings.TrimSpace(response.StreamID) == "" {
		return false, statusError{status: http.StatusBadGateway, message: "host model stream returned an empty stream id"}
	}
	defer func() { _ = host.CloseStream(response.StreamID) }()
	rewriter := newStreamModelRewriter(requestedModel)
	receivedPayload := false
	for {
		chunk, err := host.ReadStream(response.StreamID)
		if err != nil {
			return receivedPayload, err
		}
		if strings.TrimSpace(chunk.Error) != "" {
			return receivedPayload, statusError{status: statusFromError(fmt.Errorf("%s", chunk.Error)), message: chunk.Error}
		}
		if len(chunk.Payload) > 0 {
			receivedPayload = true
			if capture != nil {
				if capture.firstTokenAt.IsZero() {
					capture.firstTokenAt = time.Now().UTC()
				}
				capture.observeStreamPayload(chunk.Payload)
			}
			if rewritten := rewriter.Rewrite(chunk.Payload); len(rewritten) > 0 {
				if err := host.Emit(pluginStreamID, rewritten); err != nil {
					return true, err
				}
			}
		}
		if chunk.Done {
			if tail := rewriter.Finish(); len(tail) > 0 {
				if err := host.Emit(pluginStreamID, tail); err != nil {
					return receivedPayload, err
				}
			}
			return receivedPayload, nil
		}
	}
}
