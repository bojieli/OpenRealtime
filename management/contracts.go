// Package management defines the UI-independent OpenRealtime management-plane
// contracts. Browser, native, headless, and third-party inspectors consume the
// same versioned HTTP API; no view or gateway implementation is part of these
// interfaces.
package management

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/bojieli/OpenRealtime/plugin"
)

const (
	// CapabilityHeader carries a narrow management capability. Capabilities are
	// never accepted in URLs, where access logs and referrers can retain them.
	CapabilityHeader = "OpenRealtime-Management-Token"

	APIPrefix = "/openrealtime/v1"
)

var (
	HTTPRoutesContract = semanticContract(
		"openrealtime.management.http_routes",
		"openrealtime/management/http-routes/v1:Register(owner,routes)->dispose",
	)
	HTTPHandlerContract = semanticContract(
		"openrealtime.management.http_handler",
		"openrealtime/management/http-handler/v1:http.Handler",
	)
	AuthorizerContract = semanticContract(
		"openrealtime.management.authorizer",
		"openrealtime/management/authorizer/v1:Authorize(capability,operation,resource)",
	)
	StaticCatalogContract = semanticContract(
		"openrealtime.management.static_catalog",
		"openrealtime/management/static-catalog/v1:graph-element-plugin-values-schema-by-immutable-identity",
	)
	SessionInspectionContract = semanticContract(
		"openrealtime.management.session_inspection",
		"openrealtime/management/session-inspection/v1:live-static-model-deltas-trace-by-session-cursor",
	)
	AuthoringContract = semanticContract(
		"openrealtime.management.authoring",
		"openrealtime/management/authoring/v1:analyze-compile-render-bounded-documents",
	)
	SourcePublicationContract = semanticContract(
		"openrealtime.management.source_publication",
		"openrealtime/management/source-publication/v1:root-identity-create-update-stale-digest-atomic-receipt",
	)
	ReconciliationContract = semanticContract(
		"openrealtime.management.reconciliation",
		"openrealtime/management/reconciliation/v1:validate-premount-safe-point-swap-receipt",
	)
)

func semanticContract(name, definition string) plugin.Contract {
	digest := sha256.Sum256([]byte(definition))
	return plugin.Contract{
		Name: name, Revision: 1, Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}
