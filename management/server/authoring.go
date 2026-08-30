package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type AuthoringAPIFactory struct{ descriptor plugin.Descriptor }

func NewAuthoringAPIFactory() *AuthoringAPIFactory {
	return &AuthoringAPIFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.management.server.authoring-api", Revision: 2,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{
			{Contract: management.HTTPRoutesContract},
			{Contract: management.AuthorizerContract},
			{Contract: management.AuthoringContract},
		},
	}}
}

func (factory *AuthoringAPIFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *AuthoringAPIFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	authorizer, err := lookupService[management.Authorizer](mount.Services, management.AuthorizerContract)
	if err != nil {
		return err
	}
	authoring, err := lookupService[management.Authoring](mount.Services, management.AuthoringContract)
	if err != nil {
		return err
	}
	return registerRoutes(mount, []Route{
		{Pattern: "POST " + management.APIPrefix + "/authoring/analyze", Handler: authoringDocumentHandler(
			authorizer, management.AnalyzeDocument,
			func(ctx context.Context, document management.AuthoringDocument) (any, error) {
				result, err := authoring.Analyze(ctx, document)
				if err == nil {
					err = management.ValidateAnalysisResult(document, result)
				}
				return result, err
			},
		)},
		{Pattern: "POST " + management.APIPrefix + "/authoring/compile", Handler: authoringDocumentHandler(
			authorizer, management.CompileDocument,
			func(ctx context.Context, document management.AuthoringDocument) (any, error) {
				result, err := authoring.Compile(ctx, document)
				if err == nil {
					err = management.ValidateCompileResult(document, result)
				}
				return result, err
			},
		)},
		{Pattern: "POST " + management.APIPrefix + "/authoring/render", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request); err != nil {
					writeServiceError(writer, err)
					return
				}
				if err := authorize(request, authorizer, management.RenderGraph, "authoring"); err != nil {
					writeServiceError(writer, err)
					return
				}
				var input management.RenderRequest
				if err := decodeStrictJSON(request, maxAuthoringBody, &input); err != nil {
					writeServiceError(writer, err)
					return
				}
				result, err := authoring.Render(request.Context(), input)
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				if result.Fingerprint != input.Graph.Fingerprint || result.Format != input.Format {
					writeServiceError(writer, fmt.Errorf("%w: authoring renderer returned another graph", management.ErrConflict))
					return
				}
				writeJSON(writer, http.StatusOK, result)
			},
		)},
	})
}

func authoringDocumentHandler(
	authorizer management.Authorizer,
	operation management.Operation,
	invoke func(context.Context, management.AuthoringDocument) (any, error),
) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := validateQuery(request); err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := authorize(request, authorizer, operation, "authoring"); err != nil {
			writeServiceError(writer, err)
			return
		}
		var document management.AuthoringDocument
		if err := decodeStrictJSON(request, maxAuthoringBody, &document); err != nil {
			writeServiceError(writer, err)
			return
		}
		result, err := invoke(request.Context(), document)
		if err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := authorize(request, authorizer, operation, "authoring"); err != nil {
			writeServiceError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	})
}
