// Package presentation defines language-neutral contracts shared by the
// optional presentation host, browser clients, native clients, and headless
// conformance drivers. None of these contracts grants access to a realtime
// server implementation; cross-realm access always uses a versioned wire API.
package presentation

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/bojieli/OpenRealtime/plugin"
)

var (
	HTTPRoutesContract = semanticContract(
		"presentation.host.http_routes",
		"openrealtime/presentation/host/http-routes/v1:Register(owner,routes)->dispose",
	)
	HTTPHandlerContract = semanticContract(
		"presentation.host.http_handler",
		"openrealtime/presentation/host/http-handler/v1:http.Handler",
	)
	ModuleCatalogContract = semanticContract(
		"presentation.host.module_catalog",
		"openrealtime/presentation/host/module-catalog/v1:content-addressed-assets",
	)
	ClientManifestContract = semanticContract(
		"presentation.host.client_manifest",
		"openrealtime/presentation/client-manifest/v1:immutable-profile-lock-implementations-assets-endpoints",
	)
	ArtifactStoreContract = semanticContract(
		"presentation.host.artifact_store",
		"openrealtime/presentation/host/artifact-store/v1:bounded-publish-lookup-list-stats-sandboxed-resource",
	)
	DownloadStoreContract = semanticContract(
		"presentation.host.download_store",
		"openrealtime/presentation/host/download-store/v1:bounded-publish-lookup-list-stats-attachment-resource",
	)
	EffectsContract = semanticContract(
		"presentation.host.effects",
		"openrealtime/presentation/host/effects/v1:descriptor-locked-declarations-scoped-websocket-admission-authority-confirmation-target-idempotency-audit",
	)
	EffectAuthorityContract = semanticContract(
		"presentation.host.effect_authority",
		"openrealtime/presentation/host/effect-authority/v1:verify-opaque-receipt-exact-session-call-declaration-arguments-target",
	)
	ListenerContract = semanticContract(
		"presentation.host.listener",
		"openrealtime/presentation/host/listener/v1:network-address",
	)
	EndpointDirectoryContract = semanticContract(
		"presentation.host.endpoint_directory",
		"openrealtime/presentation/host/endpoint-directory/v1:immutable-fingerprinted-closed-named-endpoints-model-timeout-limit",
	)
	CredentialContract = semanticContract(
		"presentation.host.realtime_credential",
		"openrealtime/presentation/host/realtime-credential/v1:authorization-header-provider",
	)
	EndpointDirectoryConfigContract = semanticContract(
		"presentation.host.endpoint_directory.config",
		"openrealtime/presentation/host/endpoint-directory-config/v1:closed-explicit-endpoints-model-timeout-limit",
	)
	ListenerConfigContract = semanticContract(
		"presentation.host.listener.config",
		"openrealtime/presentation/host/listener-config/v1:loopback-address-timeouts",
	)
	ArtifactStoreConfigContract = semanticContract(
		"presentation.host.artifact_store.config",
		"openrealtime/presentation/host/artifact-store-config/v1:max-entries-item-bytes-total-bytes",
	)
	DownloadStoreConfigContract = semanticContract(
		"presentation.host.download_store.config",
		"openrealtime/presentation/host/download-store-config/v1:max-entries-item-bytes-total-bytes",
	)
	EffectsConfigContract = semanticContract(
		"presentation.host.effects.config",
		"openrealtime/presentation/host/effects-config/v1:max-sessions-message-result-inflight-calls-audit-confirmation-execution-timeouts",
	)
	ClientConnectionContract = semanticContract(
		"presentation.client.connection",
		"openrealtime/presentation/client/connection/v1:connect-send-subscribe-close-state",
	)
	ClientTransportDiagnosticsContract = semanticContract(
		"presentation.client.transport_diagnostics",
		"openrealtime/presentation/client/transport-diagnostics/v1:payload-free-media-data-channel-queue-stats-snapshot",
	)
	ClientProtocolEventsContract = semanticContract(
		"presentation.client.protocol_events",
		"openrealtime/presentation/client/protocol-events/v1:bounded-validated-inbound-subscribe-no-send-or-effect-authority",
	)
	ClientSessionConfigurationContract = semanticContract(
		"presentation.client.session_configuration",
		"openrealtime/presentation/client/session-configuration/v1:scoped-declarative-contributions-merged-session-update",
	)
	ClientStateContract = semanticContract(
		"presentation.client.session_state",
		"openrealtime/presentation/client/session-state/v1:canonical-snapshot-subscribe-connect-send-text-end-turn-cancel-playout-tool-result",
	)
	ClientCodecContract = semanticContract(
		"presentation.client.strict_json",
		"openrealtime/presentation/client/strict-json/v1:duplicate-key-depth-finite-number-parse-stable-encode",
	)
	ClientSlotsContract = semanticContract(
		"presentation.client.slots",
		"openrealtime/presentation/client/slots/v1:register-dispose",
	)
	ClientMediaContract = semanticContract(
		"presentation.client.media",
		"openrealtime/presentation/client/media/v1:native-audio-video-capture-playout-lifecycle",
	)
	ClientVideoContract = semanticContract(
		"presentation.client.video",
		"openrealtime/presentation/client/video/v1:negotiated-camera-screen-capture-start-stop-snapshot-bounded-frame-publication",
	)
	ClientEffectsContract = semanticContract(
		"presentation.client.tools_effects",
		"openrealtime/presentation/client/tools-effects/v1:bounded-tools-confirmation-declared-targets",
	)
	ClientArtifactsContract = semanticContract(
		"presentation.client.artifacts",
		"openrealtime/presentation/client/artifacts/v1:bounded-immutable-artifact-download-references-view-export",
	)
	ClientInspectionAccessContract = semanticContract(
		"presentation.client.inspection_access",
		"openrealtime/presentation/client/inspection-access/v1:scoped-ephemeral-capability-subscribe",
	)
	ClientInspectionContract = semanticContract(
		"presentation.client.inspection",
		"openrealtime/presentation/client/inspection/v1:live-static-model-delta-trace-redacted-projections",
	)
	ClientManagementOperatorAccessContract = semanticContract(
		"presentation.client.management_operator_access",
		"openrealtime/presentation/client/management-operator-access/v1:private-lease-operation-resource-generation-abort-no-serialization",
	)
	ClientManagementOperatorControlContract = semanticContract(
		"presentation.client.management_operator_control",
		"openrealtime/presentation/client/management-operator-control/v1:replace-clear-redacted-status-subscribe",
	)
	ClientManagementTransportContract = semanticContractRevision(
		"presentation.client.management_transport",
		4,
		"openrealtime/presentation/client/management-transport/v4:whitelisted-static-authoring-reconciliation-edge-removal-creation-header-capability-strict-bounded-no-redirect",
	)
	ClientManagementStaticContract = semanticContract(
		"presentation.client.management_static",
		"openrealtime/presentation/client/management-static/v1:exact-graph-element-plugin-values-schema-by-immutable-identity",
	)
	ClientManagementAuthoringContract = semanticContract(
		"presentation.client.management_authoring",
		"openrealtime/presentation/client/management-authoring/v1:analyze-compile-render-exact-document-graph-identity",
	)
	ClientManagementEditingContract = semanticContractRevision(
		"presentation.client.management_editing",
		4,
		"openrealtime/presentation/client/management-editing/v4:canonical-ortg-yaml-json-exact-node-rename-edge-removal-creation-validated-local-edit-application",
	)
	ClientSourceReadingContract = semanticContract(
		"presentation.client.source_reading",
		"openrealtime/presentation/client/source-reading/v1:explicit-root-path-exact-utf8-source-result-revalidation",
	)
	ClientSourcePublicationContract = semanticContract(
		"presentation.client.source_publication",
		"openrealtime/presentation/client/source-publication/v1:explicit-root-create-stale-update-receipt-revalidation",
	)
	ClientAuthoringWorkspaceContract = semanticContractRevision(
		"presentation.client.authoring_workspace",
		4,
		"openrealtime/presentation/client/authoring-workspace/v4:bounded-ortg-yaml-json-document-optional-rooted-read-analysis-compile-render-graph-node-rename-edge-removal-creation-cas-optional-publication-immutable-snapshot-subscribe",
	)
	ClientAuthoringWorkspaceStateContract = semanticContract(
		"presentation.client.authoring_workspace.state",
		"openrealtime/presentation/client/authoring-workspace-state/v1:bounded-durable-document-path-source-revision-derived-state-invalidated",
	)
	ClientViewContract = semanticContract(
		"presentation.client.view",
		"openrealtime/presentation/client/view/v1:native-slots-state-media-effects-inspection-authoring",
	)
)

func semanticContract(name, definition string) plugin.Contract {
	return semanticContractRevision(name, 1, definition)
}

func semanticContractRevision(name string, revision uint64, definition string) plugin.Contract {
	digest := sha256.Sum256([]byte(definition))
	return plugin.Contract{
		Name: name, Revision: revision, Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}
