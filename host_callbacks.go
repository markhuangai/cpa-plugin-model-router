package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type callbackModelHost struct {
	hostCallbackID string
	streamContext  context.Context
}

type hostModelExecutionRPC struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type streamEmitRPC struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type streamCloseRPC struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func (host callbackModelHost) Execute(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	result, err := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRPC{HostModelExecutionRequest: request, HostCallbackID: host.hostCallbackID})
	if err != nil {
		return pluginapi.HostModelExecutionResponse{}, err
	}
	var response pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return pluginapi.HostModelExecutionResponse{}, fmt.Errorf("decode host.model.execute response: %w", err)
	}
	return response, nil
}

func (host callbackModelHost) StartStream(request pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
	result, err := callHost(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRPC{HostModelExecutionRequest: request, HostCallbackID: host.hostCallbackID})
	if err != nil {
		return pluginapi.HostModelStreamResponse{}, err
	}
	var response pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return pluginapi.HostModelStreamResponse{}, fmt.Errorf("decode host.model.execute_stream response: %w", err)
	}
	return response, nil
}

func (callbackModelHost) ReadStream(streamID string) (pluginapi.HostModelStreamReadResponse, error) {
	result, err := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: streamID})
	if err != nil {
		return pluginapi.HostModelStreamReadResponse{}, err
	}
	var response pluginapi.HostModelStreamReadResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return pluginapi.HostModelStreamReadResponse{}, fmt.Errorf("decode host.model.stream_read response: %w", err)
	}
	return response, nil
}

func (callbackModelHost) CloseStream(streamID string) error {
	if strings.TrimSpace(streamID) == "" {
		return nil
	}
	_, err := callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
	return err
}

func (callbackModelHost) Emit(streamID string, payload []byte) error {
	_, err := callHost(pluginabi.MethodHostStreamEmit, streamEmitRPC{StreamID: streamID, Payload: payload})
	return err
}

func (host callbackModelHost) ClosePluginStream(streamID, errorMessage string) {
	_, err := callHost(pluginabi.MethodHostStreamClose, streamCloseRPC{StreamID: streamID, Error: strings.TrimSpace(errorMessage)})
	if err != nil {
		ctx := host.streamContext
		if ctx == nil {
			ctx = context.Background()
		}
		logStreamCleanupError(ctx, streamID, "close_client", err)
	}
}

var diagnosticSequence atomic.Uint64

func diagnosticOperationID() string {
	return fmt.Sprintf("%x-%x", time.Now().UTC().UnixNano(), diagnosticSequence.Add(1))
}

func nativeDiagnosticsAvailable() bool {
	modelRouterABIState.Lock()
	defer modelRouterABIState.Unlock()
	return modelRouterABIState.hostAttached
}

func diagnosticErrorFields(err error) map[string]any {
	fields := map[string]any{}
	if err == nil {
		return fields
	}
	fields["error_kind"] = fmt.Sprintf("%T", err)
	fields["status_code"] = statusFromError(err)
	fields["error_code"] = codeFromError(err, "")
	if errors.Is(err, context.Canceled) {
		fields["cause"] = "cancelled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		fields["cause"] = "deadline_exceeded"
	} else {
		fields["cause"] = "operation_failed"
	}
	return fields
}

func diagnosticFilterValue(value string) string {
	if len(value) > 512 {
		return "[omitted: filter exceeds 512 bytes]"
	}
	if hasCredentialPrefix(value) || len(value) >= 32 && looksLikeCredential(value) {
		return "[omitted: credential-shaped filter]"
	}
	if parsed, err := url.Parse(value); err == nil && (parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "") {
		return "[omitted: credential-bearing filter]"
	}
	return value
}

func logModelRouterEvent(callbackID, level, event string, fields map[string]any) {
	if !nativeDiagnosticsAvailable() {
		return
	}
	fields["plugin_version"] = pluginVersion
	fields["plugin_id"] = pluginID
	raw, err := json.Marshal(fields)
	if err != nil {
		log.Printf("model-router: diagnostic encoding failed event=%s cause=%T", event, err)
		return
	}
	// CPA v7.3.9's text formatter omits unknown fields, so keep the full event in its message.
	_, err = callHost(pluginabi.MethodHostLog, struct {
		HostCallbackID string         `json:"host_callback_id,omitempty"`
		Level          string         `json:"level"`
		Message        string         `json:"message"`
		Fields         map[string]any `json:"fields"`
	}{callbackID, level, event + " " + string(raw), fields})
	if err != nil {
		log.Printf("model-router: diagnostic delivery failed event=%s status=%d cause=%T", event, statusFromError(err), err)
	}
}

func logModelRouterJSON(operation, event string, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		logModelRouterEvent("", "error", "model_router.query.diagnostic_failed", diagnosticErrorFields(err))
		return
	}
	var parts []string
	for len(raw) > 0 {
		end := min(len(raw), 32<<10)
		for end < len(raw) && !utf8.RuneStart(raw[end]) {
			end--
		}
		parts = append(parts, string(raw[:end]))
		raw = raw[end:]
	}
	for index, part := range parts {
		logModelRouterEvent("", "debug", event, map[string]any{"operation_id": operation, "chunk": index + 1, "chunks": len(parts), "decision_json": part})
	}
}
