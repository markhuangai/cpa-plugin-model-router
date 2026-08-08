package main

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *modelRouterPlugin) RouteModel(_ context.Context, request pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, error) {
	if p == nil || !p.config.Enabled {
		return pluginapi.ModelRouteResponse{Handled: false}, nil
	}
	if _, ok := p.matchingRoute(request.RequestedModel); !ok {
		return pluginapi.ModelRouteResponse{Handled: false}, nil
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     pluginID + ":matched",
	}, nil
}

func (p *modelRouterPlugin) RegisterModels(_ context.Context, _ pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	response := pluginapi.ModelRegistrationResponse{Provider: pluginID}
	if p == nil || !p.config.Enabled {
		return response, nil
	}
	response.Models = make([]pluginapi.ModelInfo, 0, len(p.config.Routes))
	for _, route := range p.config.Routes {
		alias := strings.TrimSpace(route.Alias)
		response.Models = append(response.Models, pluginapi.ModelInfo{
			ID:          alias,
			Object:      "model",
			OwnedBy:     pluginID,
			DisplayName: alias,
			Name:        alias,
			Description: "CLIProxyAPI logical model route (" + route.Strategy + ")",
			UserDefined: true,
		})
	}
	return response, nil
}

func (p *modelRouterPlugin) matchingRoute(requestedModel string) (modelRoute, bool) {
	requestedBase, _, _ := splitThinkingSuffix(requestedModel)
	for _, route := range p.config.Routes {
		if strings.EqualFold(route.Alias, requestedBase) {
			return route, true
		}
	}
	return modelRoute{}, false
}
