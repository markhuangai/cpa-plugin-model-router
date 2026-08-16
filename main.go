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

var pluginVersion = "0.2.2"

type modelRouterPlugin struct {
	config  routerConfig
	runtime *routeRuntime
}

var (
	_ pluginapi.ModelRouter      = (*modelRouterPlugin)(nil)
	_ pluginapi.ModelRegistrar   = (*modelRouterPlugin)(nil)
	_ pluginapi.ProviderExecutor = (*modelRouterPlugin)(nil)
)

func newModelRouterPlugin(configYAML []byte, previous *routeRuntime) (*modelRouterPlugin, pluginapi.Metadata, error) {
	cfg, err := decodeRouterConfig(configYAML)
	if err != nil {
		return nil, pluginapi.Metadata{}, err
	}
	runtime := newRouteRuntime(nil)
	if previous != nil {
		runtime = previous.Clone(cfg.Routes)
	} else {
		runtime.Sync(cfg.Routes)
	}
	plugin := &modelRouterPlugin{config: cfg, runtime: runtime}
	metadata := pluginapi.Metadata{
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           "markhuangai",
		GitHubRepository: "https://github.com/markhuangai/cpa-plugin-model-router",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "routes", Type: pluginapi.ConfigFieldTypeArray, Description: "Logical model aliases backed by priority or round-robin target pools."},
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

func main() {}
