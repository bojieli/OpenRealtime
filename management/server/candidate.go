package server

import (
	"context"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin/httpservice"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type apiRouteCandidate struct {
	activate func(context.Context, pluginruntime.MountContext) error
}

func (candidate apiRouteCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.activate(ctx, mount)
}

func prepareAPIRouteCandidate(
	services pluginruntime.Services,
	activate func(context.Context, pluginruntime.MountContext) error,
	validate func(pluginruntime.Services) error,
) (pluginruntime.CandidateMount, error) {
	if _, err := httpservice.LookupRegistry(services, management.HTTPRoutesContract); err != nil {
		return nil, err
	}
	if err := validate(services); err != nil {
		return nil, err
	}
	return apiRouteCandidate{activate: activate}, nil
}
