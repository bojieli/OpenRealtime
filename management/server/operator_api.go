package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

// OperatorAPIConfig selects UI-independent management services that require an
// operator-owned capability. Session inspection is intentionally absent: its
// short-lived read capability remains owned by the realtime gateway's separate
// management realm and can never authorize one of these routes.
type OperatorAPIConfig struct {
	Authorizer        management.Authorizer
	StaticCatalog     management.StaticCatalog
	Authoring         management.Authoring
	SourcePublication management.SourcePublication
	Reconciliation    management.Reconciliation
}

// OperatorAPI is an independently mounted management realm over a caller's
// existing server handler. It owns no credentials and mints no capabilities;
// every request is authorized by the provider supplied in OperatorAPIConfig.
type OperatorAPI struct {
	handler http.Handler
	realm   *pluginruntime.Mounted
}

// MountOperatorAPI mounts the selected operator services and overlays only
// their versioned management route families. Every other request, including
// session inspection, falls through unchanged to base.
func MountOperatorAPI(
	ctx context.Context, base http.Handler, config OperatorAPIConfig,
) (*OperatorAPI, error) {
	if ctx == nil {
		return nil, errors.New("mount operator management API: nil context")
	}
	if base == nil {
		return nil, errors.New("mount operator management API: nil base handler")
	}
	staticSelected := !nilInterface(config.StaticCatalog)
	authoringSelected := !nilInterface(config.Authoring)
	sourcePublicationSelected := !nilInterface(config.SourcePublication)
	reconciliationSelected := !nilInterface(config.Reconciliation)
	if !staticSelected && !authoringSelected && !sourcePublicationSelected && !reconciliationSelected {
		return nil, errors.New("mount operator management API: no operator service selected")
	}
	bundle, err := NewBundle(BundleConfig{
		Authorizer: config.Authorizer, StaticCatalog: config.StaticCatalog,
		Authoring: config.Authoring, SourcePublication: config.SourcePublication,
		Reconciliation: config.Reconciliation,
	})
	if err != nil {
		return nil, err
	}
	realm, err := bundle.Mount(ctx)
	if err != nil {
		return nil, err
	}
	operator, err := HTTPHandler(realm, "http")
	if err != nil {
		return nil, errors.Join(err, realm.Close(context.Background()))
	}

	selected := func(path string) bool {
		if staticSelected && (strings.HasPrefix(path, management.APIPrefix+"/graphs/") ||
			strings.HasPrefix(path, management.APIPrefix+"/descriptors/") ||
			strings.HasPrefix(path, management.APIPrefix+"/schemas/")) {
			return true
		}
		if (authoringSelected || sourcePublicationSelected) &&
			strings.HasPrefix(path, management.APIPrefix+"/authoring/") {
			return true
		}
		return reconciliationSelected && path == management.APIPrefix+"/reconciliations"
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if selected(request.URL.Path) {
			operator.ServeHTTP(writer, request)
			return
		}
		base.ServeHTTP(writer, request)
	})
	return &OperatorAPI{handler: handler, realm: realm}, nil
}

// Handler returns the composed server handler.
func (api *OperatorAPI) Handler() http.Handler {
	if api == nil {
		return http.NotFoundHandler()
	}
	return api.handler
}

// Close idempotently withdraws only the operator routes and disposes their
// providers. The base handler and its session management realm remain live.
func (api *OperatorAPI) Close(ctx context.Context) error {
	if api == nil || api.realm == nil {
		return errors.New("close operator management API: nil API")
	}
	return api.realm.Close(ctx)
}
