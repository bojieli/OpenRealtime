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
		Name:          "openrealtime.management.server.authoring-api", Revision: 6,
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
		{Pattern: "POST " + management.APIPrefix + "/authoring/rename", Handler: authoringRenameHandler(
			authorizer, authoring,
		)},
		{Pattern: "POST " + management.APIPrefix + "/authoring/remove-edge", Handler: authoringRemoveEdgeHandler(
			authorizer, authoring,
		)},
		{Pattern: "POST " + management.APIPrefix + "/authoring/create-edge", Handler: authoringCreateEdgeHandler(
			authorizer, authoring,
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

func (factory *AuthoringAPIFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return prepareAPIRouteCandidate(candidate.Services, factory.Mount, func(services pluginruntime.Services) error {
		if _, err := lookupService[management.Authorizer](services, management.AuthorizerContract); err != nil {
			return err
		}
		_, err := lookupService[management.Authoring](services, management.AuthoringContract)
		return err
	})
}

var _ pluginruntime.CandidatePreMounter = (*AuthoringAPIFactory)(nil)

func authoringCreateEdgeHandler(authorizer management.Authorizer, authoring management.Authoring) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := validateQuery(request); err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := authorize(request, authorizer, management.CreateDocumentEdge, "authoring"); err != nil {
			writeServiceError(writer, err)
			return
		}
		var input management.CreateDocumentEdgeRequest
		if err := decodeStrictJSON(request, maxAuthoringBody, &input); err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := management.ValidateCreateDocumentEdgeRequest(input); err != nil {
			writeServiceError(writer, err)
			return
		}
		result, err := authoring.CreateEdge(request.Context(), input)
		if err == nil {
			err = management.ValidateCreateDocumentEdgeResult(input, result)
		}
		if err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := authorize(request, authorizer, management.CreateDocumentEdge, "authoring"); err != nil {
			writeServiceError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	})
}

func authoringRemoveEdgeHandler(authorizer management.Authorizer, authoring management.Authoring) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := validateQuery(request); err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := authorize(request, authorizer, management.RemoveDocumentEdge, "authoring"); err != nil {
			writeServiceError(writer, err)
			return
		}
		var input management.RemoveDocumentEdgeRequest
		if err := decodeStrictJSON(request, maxAuthoringBody, &input); err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := management.ValidateRemoveDocumentEdgeRequest(input); err != nil {
			writeServiceError(writer, err)
			return
		}
		result, err := authoring.RemoveEdge(request.Context(), input)
		if err == nil {
			err = management.ValidateRemoveDocumentEdgeResult(input, result)
		}
		if err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := authorize(request, authorizer, management.RemoveDocumentEdge, "authoring"); err != nil {
			writeServiceError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	})
}

func authoringRenameHandler(authorizer management.Authorizer, authoring management.Authoring) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := validateQuery(request); err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := authorize(request, authorizer, management.RenameDocument, "authoring"); err != nil {
			writeServiceError(writer, err)
			return
		}
		var input management.RenameDocumentRequest
		if err := decodeStrictJSON(request, maxAuthoringBody, &input); err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := management.ValidateRenameDocumentRequest(input); err != nil {
			writeServiceError(writer, err)
			return
		}
		result, err := authoring.Rename(request.Context(), input)
		if err == nil {
			err = management.ValidateRenameDocumentResult(input, result)
		}
		if err != nil {
			writeServiceError(writer, err)
			return
		}
		if err := authorize(request, authorizer, management.RenameDocument, "authoring"); err != nil {
			writeServiceError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
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
