package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type SourceReadingAPIFactory struct{ descriptor plugin.Descriptor }

func NewSourceReadingAPIFactory() *SourceReadingAPIFactory {
	return &SourceReadingAPIFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.management.server.source-reading-api", Revision: 1,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{
			{Contract: management.HTTPRoutesContract},
			{Contract: management.AuthorizerContract},
			{Contract: management.SourceReadingContract},
		},
	}}
}

func (factory *SourceReadingAPIFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *SourceReadingAPIFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	authorizer, err := lookupService[management.Authorizer](
		mount.Services, management.AuthorizerContract,
	)
	if err != nil {
		return err
	}
	reading, err := lookupService[management.SourceReading](
		mount.Services, management.SourceReadingContract,
	)
	if err != nil {
		return err
	}
	return registerRoutes(mount, []Route{{
		Pattern: "POST " + management.APIPrefix + "/authoring/read",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if err := validateQuery(request); err != nil {
				writeServiceError(writer, err)
				return
			}
			var input management.SourceReadRequest
			if err := decodeStrictJSON(request, maxAuthoringBody, &input); err != nil {
				writeServiceError(writer, err)
				return
			}
			if !management.CanonicalDigest(input.RootIdentity) {
				writeServiceError(writer, management.ErrInvalid)
				return
			}
			if err := authorize(
				request, authorizer, management.ReadSource, input.RootIdentity,
			); err != nil {
				writeServiceError(writer, err)
				return
			}
			if err := management.ValidateSourceReadRequest(input); err != nil {
				writeServiceError(writer, err)
				return
			}
			result, err := reading.Read(request.Context(), input)
			if err != nil {
				writeServiceError(writer, err)
				return
			}
			if err := management.ValidateSourceReadResult(input, result); err != nil {
				writeServiceError(writer, fmt.Errorf(
					"%w: source reader returned another artifact", management.ErrConflict,
				))
				return
			}
			writeJSON(writer, http.StatusOK, result)
		}),
	}})
}

func (factory *SourceReadingAPIFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return prepareAPIRouteCandidate(candidate.Services, factory.Mount, func(services pluginruntime.Services) error {
		if _, err := lookupService[management.Authorizer](services, management.AuthorizerContract); err != nil {
			return err
		}
		_, err := lookupService[management.SourceReading](services, management.SourceReadingContract)
		return err
	})
}

var _ pluginruntime.CandidatePreMounter = (*SourceReadingAPIFactory)(nil)
