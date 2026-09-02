package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type SourcePublicationAPIFactory struct{ descriptor plugin.Descriptor }

func NewSourcePublicationAPIFactory() *SourcePublicationAPIFactory {
	return &SourcePublicationAPIFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.management.server.source-publication-api", Revision: 1,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{
			{Contract: management.HTTPRoutesContract},
			{Contract: management.AuthorizerContract},
			{Contract: management.SourcePublicationContract},
		},
	}}
}

func (factory *SourcePublicationAPIFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *SourcePublicationAPIFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	authorizer, err := lookupService[management.Authorizer](
		mount.Services, management.AuthorizerContract,
	)
	if err != nil {
		return err
	}
	publication, err := lookupService[management.SourcePublication](
		mount.Services, management.SourcePublicationContract,
	)
	if err != nil {
		return err
	}
	return registerRoutes(mount, []Route{{
		Pattern: "POST " + management.APIPrefix + "/authoring/write",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if err := validateQuery(request); err != nil {
				writeServiceError(writer, err)
				return
			}
			var input management.SourceWriteRequest
			if err := decodeStrictJSON(request, maxAuthoringBody, &input); err != nil {
				writeServiceError(writer, err)
				return
			}
			var operation management.Operation
			switch input.Mode {
			case management.SourceCreate:
				operation = management.CreateSource
			case management.SourceUpdate:
				operation = management.UpdateSource
			default:
				writeServiceError(writer, management.ErrInvalid)
				return
			}
			if !management.CanonicalDigest(input.RootIdentity) {
				writeServiceError(writer, management.ErrInvalid)
				return
			}
			if err := authorize(request, authorizer, operation, input.RootIdentity); err != nil {
				writeServiceError(writer, err)
				return
			}
			if err := management.ValidateSourceWriteRequest(input); err != nil {
				writeServiceError(writer, err)
				return
			}
			receipt, err := publication.Publish(request.Context(), input)
			if err != nil {
				writeServiceError(writer, err)
				return
			}
			if err := management.ValidateSourceWriteReceipt(input, receipt); err != nil {
				writeServiceError(writer, fmt.Errorf(
					"%w: source publisher returned another mutation", management.ErrConflict,
				))
				return
			}
			writeJSON(writer, http.StatusOK, receipt)
		}),
	}})
}

func (factory *SourcePublicationAPIFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return prepareAPIRouteCandidate(candidate.Services, factory.Mount, func(services pluginruntime.Services) error {
		if _, err := lookupService[management.Authorizer](services, management.AuthorizerContract); err != nil {
			return err
		}
		_, err := lookupService[management.SourcePublication](services, management.SourcePublicationContract)
		return err
	})
}

var _ pluginruntime.CandidatePreMounter = (*SourcePublicationAPIFactory)(nil)
