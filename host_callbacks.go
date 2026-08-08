package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type callbackModelHost struct {
	hostCallbackID string
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

func (callbackModelHost) ClosePluginStream(streamID, errorMessage string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, streamCloseRPC{StreamID: streamID, Error: strings.TrimSpace(errorMessage)})
}
