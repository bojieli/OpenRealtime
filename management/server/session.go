package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/plugin/httpservice"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type SessionAPIFactory struct{ descriptor plugin.Descriptor }

func NewSessionAPIFactory() *SessionAPIFactory {
	return &SessionAPIFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.management.server.session-api", Revision: 1,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{
			{Contract: management.HTTPRoutesContract},
			{Contract: management.AuthorizerContract},
			{Contract: management.SessionInspectionContract},
		},
	}}
}

func (factory *SessionAPIFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *SessionAPIFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	authorizer, source, err := sessionAPIDependencies(mount.Services)
	if err != nil {
		return err
	}
	return registerRoutes(mount, []Route{
		{Pattern: "GET " + management.APIPrefix + "/sessions/{session}/live", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request); err != nil {
					writeServiceError(writer, err)
					return
				}
				session := request.PathValue("session")
				if !management.CanonicalSessionID(session) {
					writeServiceError(writer, management.ErrNotFound)
					return
				}
				if err := authorize(request, authorizer, management.ReadSession, session); err != nil {
					writeServiceError(writer, err)
					return
				}
				snapshot, err := source.Snapshot(request.Context(), session)
				if err == nil {
					err = management.ValidateSessionSnapshot(snapshot)
				}
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				// A scoped capability may be revoked while a runtime snapshot is
				// being collected. Recheck at the response boundary so revocation
				// cannot race a slow source and disclose the completed result.
				if err := authorize(request, authorizer, management.ReadSession, session); err != nil {
					writeServiceError(writer, err)
					return
				}
				writeJSON(writer, http.StatusOK, management.RedactLive(snapshot))
			},
		)},
		{Pattern: "GET " + management.APIPrefix + "/sessions/{session}/model", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request); err != nil {
					writeServiceError(writer, err)
					return
				}
				session := request.PathValue("session")
				if !management.CanonicalSessionID(session) {
					writeServiceError(writer, management.ErrNotFound)
					return
				}
				if err := authorize(request, authorizer, management.ReadSession, session); err != nil {
					writeServiceError(writer, err)
					return
				}
				snapshot, err := source.Snapshot(request.Context(), session)
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				model, err := source.Model(request.Context(), session)
				if err == nil {
					err = management.ValidateSessionModel(snapshot, model)
				}
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				if err := authorize(request, authorizer, management.ReadSession, session); err != nil {
					writeServiceError(writer, err)
					return
				}
				writeJSON(writer, http.StatusOK, model)
			},
		)},
		{Pattern: "GET " + management.APIPrefix + "/sessions/{session}/deltas", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request, "after", "limit"); err != nil {
					writeServiceError(writer, err)
					return
				}
				session := request.PathValue("session")
				if !management.CanonicalSessionID(session) {
					writeServiceError(writer, management.ErrNotFound)
					return
				}
				if err := authorize(request, authorizer, management.ReadSession, session); err != nil {
					writeServiceError(writer, err)
					return
				}
				after, err := parseUint(request.URL.Query().Get("after"), "after", 0, ^uint64(0))
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				limit, err := parseUint(request.URL.Query().Get("limit"), "limit", defaultDeltaLimit, maxDeltaLimit)
				if err != nil || limit == 0 {
					writeServiceError(writer, fmt.Errorf("%w: invalid delta limit", management.ErrInvalid))
					return
				}
				page, err := source.Deltas(request.Context(), session, after, uint32(limit))
				if err == nil {
					err = management.ValidateDeltaPage(session, after, uint32(limit), page)
				}
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				if err := authorize(request, authorizer, management.ReadSession, session); err != nil {
					writeServiceError(writer, err)
					return
				}
				writeJSON(writer, http.StatusOK, page)
			},
		)},
		{Pattern: "GET " + management.APIPrefix + "/sessions/{session}/trace", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request); err != nil {
					writeServiceError(writer, err)
					return
				}
				session := request.PathValue("session")
				if !management.CanonicalSessionID(session) {
					writeServiceError(writer, management.ErrNotFound)
					return
				}
				if err := authorize(request, authorizer, management.ReadTrace, session); err != nil {
					writeServiceError(writer, err)
					return
				}
				trace, err := source.Trace(request.Context(), session)
				if err == nil {
					err = trace.Validate()
				}
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				if err := authorize(request, authorizer, management.ReadTrace, session); err != nil {
					writeServiceError(writer, err)
					return
				}
				writeJSON(writer, http.StatusOK, trace)
			},
		)},
	})
}

func (factory *SessionAPIFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	if factory == nil {
		return nil, errors.New("pre-mount management session API: nil factory")
	}
	if _, _, err := sessionAPIDependencies(candidate.Services); err != nil {
		return nil, err
	}
	return sessionAPICandidate{factory: factory}, nil
}

func sessionAPIDependencies(
	services pluginruntime.Services,
) (management.Authorizer, management.SessionInspection, error) {
	if _, err := httpservice.LookupRegistry(services, management.HTTPRoutesContract); err != nil {
		return nil, nil, err
	}
	authorizer, err := lookupService[management.Authorizer](services, management.AuthorizerContract)
	if err != nil {
		return nil, nil, err
	}
	source, err := lookupService[management.SessionInspection](
		services, management.SessionInspectionContract,
	)
	if err != nil {
		return nil, nil, err
	}
	return authorizer, source, nil
}

type sessionAPICandidate struct{ factory *SessionAPIFactory }

func (candidate sessionAPICandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

var _ pluginruntime.Factory = (*SessionAPIFactory)(nil)
var _ pluginruntime.CandidatePreMounter = (*SessionAPIFactory)(nil)
