package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *modelRouterPlugin) startStream(request executorRPCRequest, host modelHost) ([]byte, error) {
	streamID := strings.TrimSpace(request.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream", 0), nil
	}
	ctx, release, err := beginNativeStream(request)
	if err != nil {
		return errorForExecution(err, "executor_error"), nil
	}
	if callback, ok := host.(callbackModelHost); ok {
		callback.streamContext = ctx
		host = callback
	}
	startup := make(chan error, 1)
	go func() {
		defer release()
		var closeOnce sync.Once
		closePlugin := func(message string) {
			closeOnce.Do(func() {
				setStreamCleanup(ctx, "client", "running")
				if host != nil {
					host.ClosePluginStream(streamID, message)
				}
				setStreamCleanup(ctx, "client", "returned")
			})
		}
		stopCancellation := context.AfterFunc(ctx, func() { closePlugin("model router is shutting down") })
		started := false
		notifyStartup := func(err error) {
			if !started {
				started = true
				startup <- err
			}
		}
		var streamErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				streamErr = fmt.Errorf("stream orchestration panic: %v", recovered)
			}
			notifyStartup(streamErr)
			errorMessage := ""
			if streamErr != nil {
				errorMessage = streamErr.Error()
			}
			stopCancellation()
			closePlugin(errorMessage)
			logStreamOutcome(ctx, streamErr)
		}()
		streamErr = p.executeStreamWithHost(ctx, request.ExecutorRequest, streamID, host, func() { notifyStartup(nil) })
	}()
	if err := <-startup; err != nil {
		return errorForExecution(err, "executor_error"), nil
	}
	return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

func (p *modelRouterPlugin) executeStreamWithHost(ctx context.Context, request pluginapi.ExecutorRequest, pluginStreamID string, host modelHost, onPayload func()) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || host == nil {
		return statusError{status: http.StatusBadGateway, message: "model router host callback is unavailable"}
	}
	route, ok := p.matchingRoute(request.Model)
	if !ok {
		return statusError{status: http.StatusBadGateway, message: "no model route matched executor stream request"}
	}
	requestedModel := strings.TrimSpace(request.Model)
	bodyInfo := bodyForExecution(request)
	policy := p.config.fallbackPolicy()
	var lastErr error
	attempted := make(map[string]struct{}, len(route.Targets))
	for attempt := 0; attempt < len(route.Targets); attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		selection := p.runtime.SelectExcluding(route, attempted)
		if selection.allCooling {
			return cooldownRouteError(route, selection.retryAfter)
		}
		if selection.model == "" {
			break
		}
		attempted[routeKey(selection.model)] = struct{}{}
		target := targetModel(requestedModel, selection.model)
		p.attribution.MarkRouted(route.Alias, target, request.Headers)
		setStreamPhase(ctx, "start_stream", target, attempt+1)
		receivedPayload, err := forwardStreamAttempt(ctx, request, bodyInfo, target, requestedModel, pluginStreamID, host, onPayload)
		if err == nil {
			return nil
		}
		if cancelled := ctx.Err(); cancelled != nil {
			return cancelled
		}
		if eligibleRouteFailure(err, policy) {
			p.runtime.MarkFailure(route, selection.model)
		}
		if receivedPayload || !eligibleRouteFailure(err, policy) {
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

func forwardStreamAttempt(ctx context.Context, request pluginapi.ExecutorRequest, bodyInfo executionBody, target, requestedModel, pluginStreamID string, host modelHost, onPayload func()) (bool, error) {
	setStreamCleanup(ctx, "upstream", "pending")
	response, err := host.StartStream(hostRequest(request, bodyInfo, target, true))
	if response.StreamID != "" {
		if call, ok := ctx.Value(nativeStreamContextKey{}).(*nativeStreamCall); ok {
			call.mu.Lock()
			call.fields["upstream_stream_id"] = response.StreamID
			call.mu.Unlock()
		}
		closeUpstream := sync.OnceFunc(func() {
			setStreamCleanup(ctx, "upstream", "running")
			if err := host.CloseStream(response.StreamID); err != nil {
				logStreamCleanupError(ctx, response.StreamID, "close_upstream", err)
			}
			setStreamCleanup(ctx, "upstream", "returned")
		})
		stopCancellation := context.AfterFunc(ctx, closeUpstream)
		defer func() { stopCancellation(); closeUpstream() }()
	}
	if cancelled := ctx.Err(); cancelled != nil {
		return false, cancelled
	}
	if err != nil {
		// StartStream may report the upstream status alongside the error. Preserve it
		// so the failure is classified by the fallback policy instead of falling
		// through to the ambiguous-error path.
		if status := response.StatusCode; status > 0 {
			return false, statusError{status: status, message: err.Error()}
		}
		return false, err
	}
	if response.StatusCode >= 400 {
		return false, statusError{status: response.StatusCode, message: fmt.Sprintf("host model %s stream returned status %d", target, response.StatusCode)}
	}
	if strings.TrimSpace(response.StreamID) == "" {
		return false, statusError{status: http.StatusBadGateway, message: "host model stream returned an empty stream id"}
	}
	rewriter := newStreamModelRewriter(requestedModel)
	receivedPayload := false
	for {
		if err := ctx.Err(); err != nil {
			return receivedPayload, err
		}
		setStreamPhase(ctx, "read_stream", target, 0)
		chunk, err := host.ReadStream(response.StreamID)
		if cancelled := ctx.Err(); cancelled != nil {
			return receivedPayload, cancelled
		}
		if err != nil {
			return receivedPayload, err
		}
		if strings.TrimSpace(chunk.Error) != "" {
			return receivedPayload, statusError{status: statusFromError(fmt.Errorf("%s", chunk.Error)), message: chunk.Error}
		}
		if len(chunk.Payload) > 0 {
			if !receivedPayload && onPayload != nil {
				onPayload()
			}
			receivedPayload = true
			if rewritten := rewriter.Rewrite(chunk.Payload); len(rewritten) > 0 {
				setStreamPhase(ctx, "emit", target, 0)
				if err := host.Emit(pluginStreamID, rewritten); err != nil {
					return true, err
				}
			}
		}
		if chunk.Done {
			if tail := rewriter.Finish(); len(tail) > 0 {
				setStreamPhase(ctx, "emit", target, 0)
				if err := host.Emit(pluginStreamID, tail); err != nil {
					return receivedPayload, err
				}
			}
			return receivedPayload, nil
		}
	}
}
