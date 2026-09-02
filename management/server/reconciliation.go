package server

import (
	"context"
	"net/http"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type ReconciliationAPIFactory struct{ descriptor plugin.Descriptor }

func NewReconciliationAPIFactory() *ReconciliationAPIFactory {
	return &ReconciliationAPIFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.management.server.reconciliation-api", Revision: 1,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{
			{Contract: management.HTTPRoutesContract},
			{Contract: management.AuthorizerContract},
			{Contract: management.ReconciliationContract},
		},
	}}
}

func (factory *ReconciliationAPIFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *ReconciliationAPIFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	authorizer, err := lookupService[management.Authorizer](mount.Services, management.AuthorizerContract)
	if err != nil {
		return err
	}
	reconciler, err := lookupService[management.Reconciliation](mount.Services, management.ReconciliationContract)
	if err != nil {
		return err
	}
	return registerRoutes(mount, []Route{{
		Pattern: "POST " + management.APIPrefix + "/reconciliations",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if err := validateQuery(request); err != nil {
				writeServiceError(writer, err)
				return
			}
			var input management.ReconciliationRequest
			if err := decodeStrictJSON(request, maxReconcileBody, &input); err != nil {
				writeServiceError(writer, err)
				return
			}
			session := input.SessionID
			if !management.CanonicalSessionID(session) {
				writeServiceError(writer, management.ErrNotFound)
				return
			}
			if err := authorize(request, authorizer, management.ApplyCandidate, session); err != nil {
				writeServiceError(writer, err)
				return
			}
			if err := management.ValidateReconciliationRequest(input); err != nil {
				writeServiceError(writer, err)
				return
			}
			receipt, err := reconciler.Apply(request.Context(), input)
			if err == nil {
				err = management.ValidateReconciliationReceipt(input, receipt)
			}
			if err != nil {
				writeServiceError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, receipt)
		}),
	}})
}

func (factory *ReconciliationAPIFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return prepareAPIRouteCandidate(candidate.Services, factory.Mount, func(services pluginruntime.Services) error {
		if _, err := lookupService[management.Authorizer](services, management.AuthorizerContract); err != nil {
			return err
		}
		_, err := lookupService[management.Reconciliation](services, management.ReconciliationContract)
		return err
	})
}

var _ pluginruntime.CandidatePreMounter = (*ReconciliationAPIFactory)(nil)
