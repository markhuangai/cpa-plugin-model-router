package main

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginID   = "model-router"
	pluginName = "Model Router"
)

var pluginVersion = "0.4.0"

type modelRouterPlugin struct {
	config           routerConfig
	runtime          *routeRuntime
	store            *usageStore
	attribution      *attributionTracker
	modelsDevFetcher modelsDevCatalogFetcher
	priceSync        *priceSyncState
}

var (
	_ pluginapi.ModelRouter            = (*modelRouterPlugin)(nil)
	_ pluginapi.ModelRegistrar         = (*modelRouterPlugin)(nil)
	_ pluginapi.ProviderExecutor       = (*modelRouterPlugin)(nil)
	_ pluginapi.RequestInterceptor     = (*modelRouterPlugin)(nil)
	_ pluginapi.RequestLifecyclePlugin = (*modelRouterPlugin)(nil)
	_ pluginapi.ResponseInterceptor    = (*modelRouterPlugin)(nil)
	_ pluginapi.StreamChunkInterceptor = (*modelRouterPlugin)(nil)
	_ pluginapi.UsagePlugin            = (*modelRouterPlugin)(nil)
)

func newModelRouterPlugin(configYAML []byte, previous *modelRouterPlugin) (*modelRouterPlugin, pluginapi.Metadata, error) {
	cfg, err := decodeRouterConfig(configYAML)
	if err != nil {
		return nil, pluginapi.Metadata{}, err
	}
	runtime := newRouteRuntime(nil)
	var store *usageStore
	attribution := newAttributionTracker(nil)
	var fetcher modelsDevCatalogFetcher
	priceSync := &priceSyncState{}
	if previous != nil {
		runtime = previous.runtime.Clone(cfg.Routes)
		store = previous.store
		attribution = previous.attribution
		fetcher = previous.modelsDevFetcher
		priceSync = previous.priceSync
		if priceSync == nil {
			priceSync = &priceSyncState{}
		}
		priceSync.mu.Lock()
		if err := store.Reconfigure(cfg.DataPath, cfg.RetentionDays); err != nil {
			priceSync.mu.Unlock()
			return nil, pluginapi.Metadata{}, err
		}
		priceSync.mu.Unlock()
	} else {
		runtime.Sync(cfg.Routes)
		store, err = openUsageStore(cfg.DataPath, cfg.RetentionDays)
		if err != nil {
			return nil, pluginapi.Metadata{}, err
		}
	}
	plugin := &modelRouterPlugin{config: cfg, runtime: runtime, store: store, attribution: attribution, modelsDevFetcher: fetcher, priceSync: priceSync}
	metadata := pluginapi.Metadata{
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           "markhuangai",
		GitHubRepository: "https://github.com/markhuangai/cpa-plugin-model-router",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "routes", Type: pluginapi.ConfigFieldTypeArray, Description: "Logical model aliases backed by priority or round-robin target pools."},
			{Name: "data_path", Type: pluginapi.ConfigFieldTypeString, Description: "SQLite usage database path; defaults to model-router.db in the discovered CPA plugins directory."},
			{Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: "Number of UTC days of usage aggregates and request details to retain (1-3650)."},
		},
	}
	return plugin, metadata, nil
}

func (p *modelRouterPlugin) Identifier() string { return pluginID }

func (p *modelRouterPlugin) Execute(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, fmt.Errorf("%s execution requires the native host callback context", pluginID)
}

func (p *modelRouterPlugin) ExecuteStream(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("%s streaming requires the native stream bridge", pluginID)
}

func (p *modelRouterPlugin) CountTokens(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, newRouteError(501, "model_route_count_tokens_unsupported", "", "token counting for routed aliases is not supported by the current CLIProxyAPI plugin ABI", "")
}

func (p *modelRouterPlugin) HttpRequest(_ context.Context, _ pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("%s does not implement executor.http_request", pluginID)
}

func (p *modelRouterPlugin) HandleUsage(_ context.Context, record pluginapi.UsageRecord) {
	if p == nil || !p.config.Enabled || p.store == nil || p.attribution == nil {
		return
	}
	attribution := p.attribution.Match(record)
	if attribution.Suppress {
		return
	}
	_ = p.store.Record(storedRecordFromUsage(record, attribution))
}

func (p *modelRouterPlugin) InterceptRequestBeforeAuth(_ context.Context, request pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	if p == nil || !p.config.Enabled || p.attribution == nil {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	if _, routed := p.matchingRoute(request.RequestedModel); routed {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	now := p.attribution.now().UTC()
	p.attribution.MarkDirectRequest(request, newDirectUsageCapture(request, now))
	return pluginapi.RequestInterceptResponse{}, nil
}

func (p *modelRouterPlugin) InterceptRequestAfterAuth(_ context.Context, request pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	if p != nil && p.config.Enabled && p.attribution != nil {
		p.attribution.updateDirectRequest(request)
	}
	return pluginapi.RequestInterceptResponse{}, nil
}

func (p *modelRouterPlugin) InterceptResponse(_ context.Context, request pluginapi.ResponseInterceptRequest) (pluginapi.ResponseInterceptResponse, error) {
	if p != nil && p.config.Enabled && p.attribution != nil {
		p.attribution.observeDirectResponse(request)
	}
	return pluginapi.ResponseInterceptResponse{}, nil
}

func (p *modelRouterPlugin) InterceptStreamChunk(_ context.Context, request pluginapi.StreamChunkInterceptRequest) (pluginapi.StreamChunkInterceptResponse, error) {
	if p != nil && p.config.Enabled && p.attribution != nil {
		p.attribution.observeDirectStream(request)
	}
	return pluginapi.StreamChunkInterceptResponse{}, nil
}

func (p *modelRouterPlugin) HandleRequestComplete(_ context.Context, completion pluginapi.RequestCompletion) error {
	if p == nil || !p.config.Enabled || p.store == nil || p.attribution == nil {
		return nil
	}
	marker, claimed := p.attribution.completeDirect(completion)
	if claimed {
		_ = p.store.Record(marker.capture.storedRecord(marker))
	}
	return nil
}

func main() {}
