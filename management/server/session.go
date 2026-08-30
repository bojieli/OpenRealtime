package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
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
	authorizer, err := lookupService[management.Authorizer](mount.Services, management.AuthorizerContract)
	if err != nil {
		return err
	}
	source, err := lookupService[management.SessionInspection](mount.Services, management.SessionInspectionContract)
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
