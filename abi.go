package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int ModelRouterPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void ModelRouterPluginFree(void*, size_t);
extern void ModelRouterPluginShutdown(void);

static const cliproxy_host_api* model_router_host;

static void model_router_store_host(const cliproxy_host_api* host) {
	model_router_host = host;
}

static int model_router_call_host(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (model_router_host == NULL || model_router_host->call == NULL) {
		return 1;
	}
	return model_router_host->call(model_router_host->host_ctx, method, request, request_len, response);
}

static void model_router_free_host_buffer(void* ptr, size_t len) {
	if (model_router_host != NULL && model_router_host->free_buffer != NULL && ptr != NULL) {
		model_router_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type abiEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *abiError       `json:"error,omitempty"`
}

type abiError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type executorRPCRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type modelRouteRPCRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  abiCapabilities    `json:"capabilities"`
}

type abiCapabilities struct {
	ModelRegistrar        bool                         `json:"model_registrar"`
	ModelRouter           bool                         `json:"model_router"`
	Executor              bool                         `json:"executor"`
	RequestInterceptor    bool                         `json:"request_interceptor"`
	RequestLifecycle      bool                         `json:"request_lifecycle_plugin"`
	ResponseInterceptor   bool                         `json:"response_interceptor"`
	StreamInterceptor     bool                         `json:"response_stream_interceptor"`
	UsagePlugin           bool                         `json:"usage_plugin"`
	ManagementAPI         bool                         `json:"management_api"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
}

var modelRouterABIState = struct {
	sync.Mutex
	plugin       *modelRouterPlugin
	metadata     pluginapi.Metadata
	shuttingDown bool
	inFlight     sync.WaitGroup
	calls        int
	streams      map[*nativeStreamCall]struct{}
	shutdownDone chan struct{}
	hostAttached bool
}{}

var modelRouterRegistration sync.Mutex

const maxCGoRequestLen = C.size_t(1<<31 - 1)

const registrationSchemaVersion uint32 = 2
const streamBodyOmissionSchemaVersion uint32 = 3

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	C.model_router_store_host(host)
	modelRouterABIState.Lock()
	modelRouterABIState.shuttingDown = false
	modelRouterABIState.shutdownDone = nil
	modelRouterABIState.hostAttached = true
	modelRouterABIState.Unlock()
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.ModelRouterPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.ModelRouterPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.ModelRouterPluginShutdown)
	return 0
}

//export ModelRouterPluginCall
func ModelRouterPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeABIResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 0
	}
	if requestLen > maxCGoRequestLen {
		writeABIResponse(response, errorEnvelope("request_too_large", "request payload is too large", 0))
		return 0
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleModelRouterABIMethod(context.Background(), C.GoString(method), requestBytes)
	if err != nil {
		raw = errorForExecution(err, "plugin_error")
	}
	writeABIResponse(response, raw)
	return 0
}

//export ModelRouterPluginFree
func ModelRouterPluginFree(pointer unsafe.Pointer, _ C.size_t) {
	if pointer != nil {
		C.free(pointer)
	}
}

//export ModelRouterPluginShutdown
func ModelRouterPluginShutdown() {
	modelRouterABIState.Lock()
	if modelRouterABIState.shuttingDown {
		done := modelRouterABIState.shutdownDone
		modelRouterABIState.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	modelRouterABIState.shuttingDown = true
	modelRouterABIState.shutdownDone = make(chan struct{})
	done := modelRouterABIState.shutdownDone
	plugin := modelRouterABIState.plugin
	modelRouterABIState.plugin = nil
	modelRouterABIState.metadata = pluginapi.Metadata{}
	streams := make([]*nativeStreamCall, 0, len(modelRouterABIState.streams))
	for stream := range modelRouterABIState.streams {
		streams = append(streams, stream)
	}
	modelRouterABIState.Unlock()
	started := time.Now()
	operation := diagnosticOperationID()
	for _, stream := range streams {
		stream.mu.Lock()
		stream.fields["shutdown_operation_id"] = operation
		stream.mu.Unlock()
	}
	logModelRouterEvent("", "info", "model_router.shutdown.start", map[string]any{"operation_id": operation, "active_streams": len(streams), "phase": "cancel", "elapsed_ms": 0})
	for _, stream := range streams {
		stream.cancel()
	}
	drained := make(chan struct{})
	go func() { modelRouterABIState.inFlight.Wait(); close(drained) }()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
wait:
	for {
		select {
		case <-drained:
			break wait
		case <-ticker.C:
			modelRouterABIState.Lock()
			remaining := modelRouterABIState.calls
			active := make([]map[string]any, 0, len(modelRouterABIState.streams))
			for stream := range modelRouterABIState.streams {
				active = append(active, stream.snapshot())
			}
			modelRouterABIState.Unlock()
			logModelRouterEvent("", "warn", "model_router.shutdown.stalled", map[string]any{"operation_id": operation, "remaining_calls": remaining, "streams": active, "elapsed_ms": time.Since(started).Milliseconds()})
		}
	}
	if plugin != nil && plugin.store != nil {
		if err := plugin.store.Close(); err != nil {
			fields := diagnosticErrorFields(err)
			fields["operation_id"], fields["phase"] = operation, "close_storage"
			logModelRouterEvent("", "error", "model_router.shutdown.storage_failed", fields)
		}
	}
	logModelRouterEvent("", "info", "model_router.shutdown.complete", map[string]any{"operation_id": operation, "remaining_calls": 0, "elapsed_ms": time.Since(started).Milliseconds()})
	modelRouterABIState.Lock()
	modelRouterABIState.hostAttached = false
	C.model_router_store_host(nil)
	close(done)
	modelRouterABIState.Unlock()
}

func handleModelRouterABIMethod(ctx context.Context, method string, request []byte) ([]byte, error) {
	if method == pluginabi.MethodPluginRegister || method == pluginabi.MethodPluginReconfigure {
		return registerModelRouter(request)
	}
	switch method {
	case pluginabi.MethodManagementRegister:
		return okEnvelope(modelRouterManagementRegistration())
	case pluginabi.MethodManagementHandle:
		var rpcRequest managementRPCRequest
		if err := json.Unmarshal(request, &rpcRequest); err != nil {
			return nil, fmt.Errorf("decode management.handle request: %w", err)
		}
		path := strings.TrimRight(strings.TrimSpace(rpcRequest.Path), "/")
		if !strings.HasPrefix(path, modelRouterUsageBasePath+"/") {
			return okEnvelope(handleModelRouterManagement(nil, rpcRequest.ManagementRequest))
		}
	}
	plugin, metadata, done, err := beginModelRouterCall()
	if err != nil {
		return nil, err
	}
	defer done()
	switch method {
	case pluginabi.MethodManagementHandle:
		var rpcRequest managementRPCRequest
		if err := json.Unmarshal(request, &rpcRequest); err != nil {
			return nil, fmt.Errorf("decode management.handle request: %w", err)
		}
		return okEnvelope(handleModelRouterManagement(plugin, rpcRequest.ManagementRequest))
	case pluginabi.MethodModelRegister:
		response, err := plugin.RegisterModels(ctx, pluginapi.ModelRegistrationRequest{Plugin: metadata})
		return okEnvelopeWithError(response, err)
	case pluginabi.MethodModelRoute:
		var rpcRequest modelRouteRPCRequest
		if err := json.Unmarshal(request, &rpcRequest); err != nil {
			return nil, fmt.Errorf("decode model.route request: %w", err)
		}
		response, err := plugin.RouteModel(ctx, rpcRequest.ModelRouteRequest)
		return okEnvelopeWithError(response, err)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodExecutorExecute:
		var rpcRequest executorRPCRequest
		if err := json.Unmarshal(request, &rpcRequest); err != nil {
			return nil, fmt.Errorf("decode executor.execute request: %w", err)
		}
		response, err := plugin.executeWithHost(rpcRequest.ExecutorRequest, callbackModelHost{hostCallbackID: rpcRequest.HostCallbackID})
		if err != nil {
			return errorForExecution(err, "executor_error"), nil
		}
		return okEnvelope(response)
	case pluginabi.MethodExecutorExecuteStream:
		var rpcRequest executorRPCRequest
		if err := json.Unmarshal(request, &rpcRequest); err != nil {
			return nil, fmt.Errorf("decode executor.execute_stream request: %w", err)
		}
		return plugin.startStream(rpcRequest, callbackModelHost{hostCallbackID: rpcRequest.HostCallbackID})
	case pluginabi.MethodExecutorCountTokens:
		_, err := plugin.CountTokens(ctx, pluginapi.ExecutorRequest{})
		return errorForExecution(err, "model_route_count_tokens_unsupported"), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return errorEnvelope("unsupported_method", "executor.http_request is not supported by model-router", 0), nil
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		var rpcRequest pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(request, &rpcRequest); err != nil {
			return nil, fmt.Errorf("decode %s request: %w", method, err)
		}
		var response pluginapi.RequestInterceptResponse
		var err error
		if method == pluginabi.MethodRequestInterceptBefore {
			response, err = plugin.InterceptRequestBeforeAuth(ctx, rpcRequest)
		} else {
			response, err = plugin.InterceptRequestAfterAuth(ctx, rpcRequest)
		}
		return okEnvelopeWithError(response, err)
	case pluginabi.MethodUsageHandle:
		var record pluginapi.UsageRecord
		if err := json.Unmarshal(request, &record); err != nil {
			return nil, fmt.Errorf("decode usage.handle request: %w", err)
		}
		plugin.HandleUsage(ctx, record)
		return okEnvelope(map[string]any{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

func registerModelRouter(raw []byte) ([]byte, error) {
	modelRouterABIState.Lock()
	if modelRouterABIState.shuttingDown {
		modelRouterABIState.Unlock()
		return nil, fmt.Errorf("%s is shutting down", pluginID)
	}
	modelRouterABIState.inFlight.Add(1)
	modelRouterABIState.calls++
	modelRouterABIState.Unlock()
	defer finishNativeCall()
	modelRouterRegistration.Lock()
	defer modelRouterRegistration.Unlock()
	var request lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	modelRouterABIState.Lock()
	if modelRouterABIState.shuttingDown {
		modelRouterABIState.Unlock()
		return nil, fmt.Errorf("%s is shutting down", pluginID)
	}
	var previous *modelRouterPlugin
	if modelRouterABIState.plugin != nil {
		previous = modelRouterABIState.plugin
	}
	modelRouterABIState.Unlock()
	plugin, metadata, err := newModelRouterPlugin(request.ConfigYAML, previous)
	if err != nil {
		return nil, err
	}
	modelRouterABIState.Lock()
	if modelRouterABIState.shuttingDown {
		modelRouterABIState.Unlock()
		if previous == nil {
			if err := plugin.store.Close(); err != nil {
				logModelRouterEvent("", "error", "model_router.shutdown.storage_failed", diagnosticErrorFields(err))
			}
		}
		return nil, fmt.Errorf("%s is shutting down", pluginID)
	}
	modelRouterABIState.plugin = plugin
	modelRouterABIState.metadata = metadata
	modelRouterABIState.Unlock()
	formats := []string{"openai", "openai-response", "claude", "gemini", "codex", "antigravity", "interactions"}
	schemaVersion := registrationSchemaVersion
	if request.SchemaVersion >= streamBodyOmissionSchemaVersion {
		schemaVersion = streamBodyOmissionSchemaVersion
	}
	return okEnvelope(abiRegistration{
		SchemaVersion: schemaVersion,
		Metadata:      metadata,
		Capabilities: abiCapabilities{
			ModelRegistrar:        true,
			ModelRouter:           true,
			Executor:              true,
			RequestInterceptor:    true,
			RequestLifecycle:      false,
			ResponseInterceptor:   false,
			StreamInterceptor:     false,
			UsagePlugin:           true,
			ManagementAPI:         true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeStatic,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
		},
	})
}

func beginModelRouterCall() (*modelRouterPlugin, pluginapi.Metadata, func(), error) {
	modelRouterABIState.Lock()
	defer modelRouterABIState.Unlock()
	if modelRouterABIState.shuttingDown {
		return nil, pluginapi.Metadata{}, nil, fmt.Errorf("%s is shutting down", pluginID)
	}
	if modelRouterABIState.plugin == nil {
		return nil, pluginapi.Metadata{}, nil, fmt.Errorf("%s is not registered", pluginID)
	}
	modelRouterABIState.inFlight.Add(1)
	modelRouterABIState.calls++
	return modelRouterABIState.plugin, modelRouterABIState.metadata, finishNativeCall, nil
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPointer *C.uint8_t
	if len(rawPayload) > 0 {
		pointer := C.CBytes(rawPayload)
		if pointer == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(pointer)
		requestPointer = (*C.uint8_t)(pointer)
	}
	var response C.cliproxy_buffer
	callCode := C.model_router_call_host(cMethod, requestPointer, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.model_router_free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var envelope abiEnvelope
	if err := json.Unmarshal(rawResponse, &envelope); err != nil {
		return nil, fmt.Errorf("decode host callback %s envelope: %w", method, err)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return nil, fmt.Errorf("host callback %s failed", method)
		}
		return nil, statusError{status: envelope.Error.HTTPStatus, code: envelope.Error.Code, message: strings.TrimSpace(envelope.Error.Message)}
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), envelope.Result...), nil
}

func okEnvelopeWithError(value any, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	return okEnvelope(value)
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(abiEnvelope{OK: true, Result: raw})
}

func errorForExecution(err error, fallbackCode string) []byte {
	if err == nil {
		return errorEnvelope(fallbackCode, "model router execution failed", 0)
	}
	return errorEnvelope(codeFromError(err, fallbackCode), err.Error(), statusFromError(err))
}

func errorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(abiEnvelope{OK: false, Error: &abiError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

func writeABIResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	pointer := C.CBytes(raw)
	if pointer == nil {
		return
	}
	response.ptr = pointer
	response.len = C.size_t(len(raw))
}

func finishNativeCall() {
	modelRouterABIState.Lock()
	modelRouterABIState.calls--
	modelRouterABIState.Unlock()
	modelRouterABIState.inFlight.Done()
}

type nativeStreamContextKey struct{}

type nativeStreamCall struct {
	cancel  context.CancelFunc
	mu      sync.Mutex
	fields  map[string]any
	started time.Time
}

func (call *nativeStreamCall) snapshot() map[string]any {
	call.mu.Lock()
	defer call.mu.Unlock()
	fields := make(map[string]any, len(call.fields))
	for key, value := range call.fields {
		fields[key] = value
	}
	fields["elapsed_ms"] = time.Since(call.started).Milliseconds()
	return fields
}

func beginNativeStream(request executorRPCRequest) (context.Context, func(), error) {
	modelRouterABIState.Lock()
	defer modelRouterABIState.Unlock()
	if modelRouterABIState.shuttingDown {
		return nil, nil, fmt.Errorf("%s is shutting down", pluginID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	call := &nativeStreamCall{cancel: cancel, started: time.Now(), fields: map[string]any{"operation_id": diagnosticOperationID(), "stream_id": request.StreamID, "host_callback_id": request.HostCallbackID, "route": request.Model, "phase": "startup", "attempt": 0, "target": "", "upstream_cleanup": "pending", "client_cleanup": "pending"}}
	if modelRouterABIState.streams == nil {
		modelRouterABIState.streams = make(map[*nativeStreamCall]struct{})
	}
	modelRouterABIState.streams[call] = struct{}{}
	modelRouterABIState.inFlight.Add(1)
	modelRouterABIState.calls++
	return context.WithValue(ctx, nativeStreamContextKey{}, call), func() {
		cancel()
		modelRouterABIState.Lock()
		delete(modelRouterABIState.streams, call)
		modelRouterABIState.Unlock()
		finishNativeCall()
	}, nil
}

func setStreamPhase(ctx context.Context, phase, target string, attempt int) {
	if call, ok := ctx.Value(nativeStreamContextKey{}).(*nativeStreamCall); ok {
		call.mu.Lock()
		call.fields["phase"] = phase
		if target != "" {
			call.fields["target"] = target
		}
		if attempt > 0 {
			call.fields["attempt"] = attempt
		}
		call.mu.Unlock()
	}
}

func logStreamOutcome(ctx context.Context, err error) {
	if call, ok := ctx.Value(nativeStreamContextKey{}).(*nativeStreamCall); ok {
		fields := call.snapshot()
		setStreamPhase(ctx, "log_outcome", "", 0)
		for key, value := range diagnosticErrorFields(err) {
			fields[key] = value
		}
		level := "debug"
		if ctx.Err() != nil {
			level = "info"
			fields["outcome"] = "cancelled_shutdown"
		} else if err != nil {
			fields["outcome"] = "failed"
		} else {
			fields["outcome"] = "complete"
		}
		callbackID, _ := fields["host_callback_id"].(string)
		logModelRouterEvent(callbackID, level, "model_router.stream.complete", fields)
	}
}

func setStreamCleanup(ctx context.Context, name, state string) {
	if call, ok := ctx.Value(nativeStreamContextKey{}).(*nativeStreamCall); ok {
		call.mu.Lock()
		call.fields[name+"_cleanup"] = state
		call.mu.Unlock()
	}
}

func logStreamCleanupError(ctx context.Context, streamID, phase string, err error) {
	fields := diagnosticErrorFields(err)
	if call, ok := ctx.Value(nativeStreamContextKey{}).(*nativeStreamCall); ok {
		for key, value := range call.snapshot() {
			fields[key] = value
		}
	}
	fields["phase"], fields["cleanup_stream_id"] = phase, streamID
	callbackID, _ := fields["host_callback_id"].(string)
	logModelRouterEvent(callbackID, "error", "model_router.stream.cleanup_failed", fields)
}
