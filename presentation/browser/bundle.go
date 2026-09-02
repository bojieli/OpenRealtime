// Package browser supplies optional browser implementations of the shared
// client-plugin contracts. Its minimal bundle is a distribution profile, not
// gateway behavior; callers may layer, replace, or omit every row.
package browser

import (
	"bytes"
	"embed"
	"fmt"
	"slices"
	"strings"
	"sync"

	clientreducer "github.com/bojieli/OpenRealtime/client/reducer"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/presentation"
	"github.com/bojieli/OpenRealtime/presentation/host"
)

//go:embed assets/*.js
var modules embed.FS

type Bundle struct {
	Profile  plugin.Profile
	Lock     plugin.Lock
	Plan     plugin.Plan
	Manifest presentation.ClientManifest

	ModuleStore  *host.ModuleStoreFactory
	ManifestHost *host.ManifestFactory
	Shell        *host.BrowserShellFactory
}

// ClientModule is one caller-supplied browser plugin in a composed text
// client. Its exact source bytes become both the served asset identity and the
// implementation artifact identity. Revision is fixed at 1; changed source
// changes the immutable artifact and plan fingerprints.
type ClientModule struct {
	Entry        string
	Entrypoint   string
	PluginName   string
	Source       []byte
	Alternatives []ClientModuleAlternative
	Provides     []plugin.Contract
	Requires     []plugin.Requirement
	Permissions  []plugin.Permission
	Grants       []plugin.Permission
}

// ClientModuleAlternative is another immutable implementation asset admitted
// by the same descriptor and client plan. It is declared and content-addressed
// at boot, but remains unloaded and inactive until an exact candidate manifest
// selects and verifies it. Alternatives do not widen contracts, dependencies,
// or permission ceilings.
type ClientModuleAlternative struct {
	Entrypoint string
	Source     []byte
}

type moduleAlternative struct {
	file    string
	content []byte
}

type moduleDefinition struct {
	entry        string
	file         string
	pluginName   string
	content      []byte
	alternatives []moduleAlternative
	provides     []plugin.Contract
	requires     []plugin.Requirement
	permissions  []plugin.Permission
	grants       []plugin.Permission
}

const clientEffectsProtocol = presentation.ProtocolClientEffects

var (
	minimalBundleTemplate                 = sync.OnceValues(buildMinimalBundle)
	observerDeveloperBundleTemplate       = sync.OnceValues(buildObserverDeveloperBundle)
	observerDeveloperWebRTCBundleTemplate = sync.OnceValues(buildObserverDeveloperWebRTCBundle)
	developerBundleTemplate               = sync.OnceValues(func() (*Bundle, error) {
		digest, err := host.DefaultEffectsCatalogDigest()
		if err != nil {
			return nil, fmt.Errorf("default effect catalog: %w", err)
		}
		return buildDeveloperBundle(digest)
	})
	developerWebRTCBundleTemplate = sync.OnceValues(func() (*Bundle, error) {
		digest, err := host.DefaultEffectsCatalogDigest()
		if err != nil {
			return nil, fmt.Errorf("default effect catalog: %w", err)
		}
		return buildDeveloperWebRTCBundle(digest)
	})
)

const managementProtocol = presentation.ProtocolManagement

func cloneBundle(source *Bundle) *Bundle {
	if source == nil {
		return nil
	}
	return &Bundle{
		Profile: source.Profile.Clone(), Lock: source.Lock.Clone(), Plan: source.Plan.Clone(),
		Manifest: source.Manifest.Clone(), ModuleStore: source.ModuleStore,
		ManifestHost: source.ManifestHost, Shell: source.Shell,
	}
}

// MinimalBundle constructs the shipped text/WebSocket client profile from
// independent slot, transport, reducer, and view plugins.
func MinimalBundle() (*Bundle, error) {
	bundle, err := minimalBundleTemplate()
	return cloneBundle(bundle), err
}

func buildMinimalBundle() (*Bundle, error) {
	return buildBundle("openrealtime.browser.minimal", minimalTextDefinitions(false),
		[]presentation.ManifestEndpoint{{
			Name: "realtime.websocket", Method: "GET", Path: "/client/v1/realtime",
		}})
}

// ComposeTextBundle layers caller-supplied descriptor modules over the normal
// slots, WebSocket transport, reducer, session-configuration, and text-view
// providers. Extensions receive only services named by their requirements and
// only grants declared here; this API adds no host effect or implicit authority.
func ComposeTextBundle(profileName string, extensions []ClientModule) (*Bundle, error) {
	definitions := minimalTextDefinitions(true)
	for _, extension := range extensions {
		if len(extension.Source) == 0 {
			return nil, fmt.Errorf("browser client module %q has empty source", extension.Entry)
		}
		alternatives := make([]moduleAlternative, len(extension.Alternatives))
		for index, alternative := range extension.Alternatives {
			if len(alternative.Source) == 0 {
				return nil, fmt.Errorf(
					"browser client module %q alternative %q has empty source",
					extension.Entry, alternative.Entrypoint,
				)
			}
			alternatives[index] = moduleAlternative{
				file: alternative.Entrypoint, content: slices.Clone(alternative.Source),
			}
		}
		definitions = append(definitions, moduleDefinition{
			entry: extension.Entry, file: extension.Entrypoint, pluginName: extension.PluginName,
			content: slices.Clone(extension.Source), alternatives: alternatives,
			provides: slices.Clone(extension.Provides),
			requires: slices.Clone(extension.Requires), permissions: slices.Clone(extension.Permissions),
			grants: slices.Clone(extension.Grants),
		})
	}
	return buildBundle(profileName, definitions, []presentation.ManifestEndpoint{{
		Name: "realtime.websocket", Method: "GET", Path: "/client/v1/realtime",
	}})
}

func minimalTextDefinitions(withSessionConfiguration bool) []moduleDefinition {
	websocketPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-realtime", Operations: []string{"websocket"},
	}
	definitions := []moduleDefinition{
		{
			entry: "slots", file: "slots.js", pluginName: "openrealtime.presentation.client.slots",
			provides: []plugin.Contract{presentation.ClientSlotsContract},
		},
		{
			entry: "transport", file: "transport-websocket.js",
			pluginName:  "openrealtime.presentation.client.websocket",
			provides:    []plugin.Contract{presentation.ClientConnectionContract},
			permissions: []plugin.Permission{websocketPermission},
			grants:      []plugin.Permission{websocketPermission},
		},
		{
			entry: "reducer", file: "reducer.js", pluginName: "openrealtime.presentation.client.reducer",
			provides: []plugin.Contract{
				presentation.ClientStateContract, presentation.ClientInspectionAccessContract,
				presentation.ClientProtocolEventsContract, presentation.ClientCodecContract,
			},
			requires: []plugin.Requirement{{Contract: presentation.ClientConnectionContract}},
		},
	}
	if withSessionConfiguration {
		definitions = append(definitions, moduleDefinition{
			entry: "session-configuration", file: "session-configuration.js",
			pluginName: "openrealtime.presentation.client.session-configuration",
			provides:   []plugin.Contract{presentation.ClientSessionConfigurationContract},
			requires:   []plugin.Requirement{{Contract: presentation.ClientStateContract}},
		})
	}
	definitions = append(definitions, moduleDefinition{
		entry: "view", file: "text-view.js", pluginName: "openrealtime.presentation.client.text-view",
		requires: []plugin.Requirement{
			{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientStateContract},
		},
	})
	return definitions
}

// ObserverDeveloperBundle is the normal inspection and authoring composition.
// It deliberately omits local effects, effect declarations, artifact stores,
// and confirmation views: an effects-enabled deployment must select
// DeveloperBundle and mount an explicit receipt verifier/issuer authority.
func ObserverDeveloperBundle() (*Bundle, error) {
	bundle, err := observerDeveloperBundleTemplate()
	return cloneBundle(bundle), err
}

func buildObserverDeveloperBundle() (*Bundle, error) {
	websocketPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-realtime", Operations: []string{"websocket"},
	}
	inspectionPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-management", Operations: []string{"http"},
	}
	definitions := []moduleDefinition{
		{
			entry: "slots", file: "slots.js", pluginName: "openrealtime.presentation.client.slots",
			provides: []plugin.Contract{presentation.ClientSlotsContract},
		},
		{
			entry: "transport", file: "transport-websocket.js",
			pluginName:  "openrealtime.presentation.client.websocket",
			provides:    []plugin.Contract{presentation.ClientConnectionContract},
			permissions: []plugin.Permission{websocketPermission}, grants: []plugin.Permission{websocketPermission},
		},
		{
			entry: "reducer", file: "reducer.js", pluginName: "openrealtime.presentation.client.reducer",
			provides: []plugin.Contract{
				presentation.ClientStateContract, presentation.ClientInspectionAccessContract,
				presentation.ClientProtocolEventsContract, presentation.ClientCodecContract,
			},
			requires: []plugin.Requirement{{Contract: presentation.ClientConnectionContract}},
		},
		{
			entry: "session-configuration", file: "session-configuration.js",
			pluginName: "openrealtime.presentation.client.session-configuration",
			provides:   []plugin.Contract{presentation.ClientSessionConfigurationContract},
			requires:   []plugin.Requirement{{Contract: presentation.ClientStateContract}},
		},
		{
			entry: "debug-session", file: "debug-session.js",
			pluginName: "openrealtime.presentation.client.debug-session",
			requires:   []plugin.Requirement{{Contract: presentation.ClientSessionConfigurationContract}},
		},
		{
			entry: "inspection", file: "inspection-client.js",
			pluginName: "openrealtime.presentation.client.inspection",
			provides:   []plugin.Contract{presentation.ClientInspectionContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientInspectionAccessContract}, {Contract: presentation.ClientCodecContract},
			},
			permissions: []plugin.Permission{inspectionPermission}, grants: []plugin.Permission{inspectionPermission},
		},
		{
			entry: "view", file: "text-view.js", pluginName: "openrealtime.presentation.client.text-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientStateContract},
			},
		},
		{
			entry: "inspection-view", file: "inspection-view.js",
			pluginName: "openrealtime.presentation.client.inspection-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientInspectionContract},
			},
		},
		{
			entry: "trace-view", file: "trace-view.js",
			pluginName: "openrealtime.presentation.client.trace-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientInspectionContract},
			},
		},
	}
	definitions = append(definitions, developerManagementDefinitions(false)...)
	return buildBundle("openrealtime.browser.developer-observer", definitions, developerManagementEndpoints())
}

// developerManagementDefinitions is a self-contained descriptor-locked
// subtree. Rooted source reading and publication are selected only by
// effects-enabled profiles; observer profiles retain analysis and rendering
// without a filesystem grant.
func developerManagementDefinitions(sourceAccess bool) []moduleDefinition {
	operatorPermission := plugin.Permission{
		Kind: "credential.use", Resource: "management-operator", Operations: []string{"header"},
	}
	transportPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-management",
		Operations: []string{"static", "authoring", "source-read", "publication"},
	}
	transportGrant := transportPermission
	if !sourceAccess {
		transportGrant.Operations = []string{"static", "authoring"}
	}
	definitions := []moduleDefinition{
		{
			entry: "management-operator", file: "management-operator-capability.js",
			pluginName: "openrealtime.presentation.client.management-operator-capability",
			provides: []plugin.Contract{
				presentation.ClientManagementOperatorAccessContract,
				presentation.ClientManagementOperatorControlContract,
			},
			permissions: []plugin.Permission{operatorPermission}, grants: []plugin.Permission{operatorPermission},
		},
		{
			entry: "management-transport", file: "management-transport.js",
			pluginName: "openrealtime.presentation.client.management-transport",
			provides:   []plugin.Contract{presentation.ClientManagementTransportContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientManagementOperatorAccessContract},
				{Contract: presentation.ClientCodecContract},
			},
			permissions: []plugin.Permission{transportPermission}, grants: []plugin.Permission{transportGrant},
		},
		{
			entry: "management-static", file: "management-static.js",
			pluginName: "openrealtime.presentation.client.management-static",
			provides:   []plugin.Contract{presentation.ClientManagementStaticContract},
			requires:   []plugin.Requirement{{Contract: presentation.ClientManagementTransportContract}},
		},
		{
			entry: "management-authoring", file: "management-authoring.js",
			pluginName: "openrealtime.presentation.client.management-authoring",
			provides: []plugin.Contract{
				presentation.ClientManagementAuthoringContract,
				presentation.ClientManagementEditingContract,
			},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientManagementTransportContract},
				{Contract: presentation.ClientCodecContract},
			},
		},
	}
	if sourceAccess {
		definitions = append(definitions,
			moduleDefinition{
				entry: "management-source-reading", file: "management-source-reading.js",
				pluginName: "openrealtime.presentation.client.management-source-reading",
				provides:   []plugin.Contract{presentation.ClientSourceReadingContract},
				requires:   []plugin.Requirement{{Contract: presentation.ClientManagementTransportContract}},
			},
			moduleDefinition{
				entry: "management-source-publication", file: "management-source-publication.js",
				pluginName: "openrealtime.presentation.client.management-source-publication",
				provides:   []plugin.Contract{presentation.ClientSourcePublicationContract},
				requires:   []plugin.Requirement{{Contract: presentation.ClientManagementTransportContract}},
			},
		)
	}
	definitions = append(definitions, []moduleDefinition{
		{
			entry: "authoring-workspace", file: "authoring-workspace.js",
			pluginName: "openrealtime.presentation.client.authoring-workspace",
			provides:   []plugin.Contract{presentation.ClientAuthoringWorkspaceContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientManagementAuthoringContract},
				{Contract: presentation.ClientManagementEditingContract},
				{Contract: presentation.ClientSourceReadingContract, Optional: true},
				{Contract: presentation.ClientSourcePublicationContract, Optional: true},
			},
		},
		{
			entry: "management-operator-view", file: "management-operator-view.js",
			pluginName: "openrealtime.presentation.client.management-operator-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract},
				{Contract: presentation.ClientManagementOperatorControlContract},
			},
		},
		{
			entry: "authoring-editor-view", file: "authoring-editor-view.js",
			pluginName: "openrealtime.presentation.client.authoring-editor-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientAuthoringWorkspaceContract},
			},
		},
		{
			entry: "authoring-configuration-view", file: "authoring-configuration-view.js",
			pluginName: "openrealtime.presentation.client.authoring-configuration-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientAuthoringWorkspaceContract},
				{Contract: presentation.ClientManagementStaticContract},
			},
		},
		{
			entry: "authoring-canvas-view", file: "authoring-canvas-view.js",
			pluginName: "openrealtime.presentation.client.authoring-canvas-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientAuthoringWorkspaceContract},
			},
		},
	}...)
	return definitions
}

func developerManagementEndpoints() []presentation.ManifestEndpoint {
	return []presentation.ManifestEndpoint{
		{Name: "management.authoring", Method: "POST", Path: "/client/v1/management/authoring",
			Protocol: managementProtocol},
		{Name: "management.sessions", Method: "GET", Path: "/client/v1/management/sessions",
			Protocol: managementProtocol},
		{Name: "management.static", Method: "GET", Path: "/client/v1/management",
			Protocol: managementProtocol},
		{Name: "realtime.websocket", Method: "GET", Path: "/client/v1/realtime"},
	}
}

// DeveloperBundle layers session-scoped graph inspection over the same
// transport, reducer, slots, and text view used by MinimalBundle. Inspection
// is a separate capability-bearing client plugin and can be removed without
// changing realtime protocol behavior or the view that renders conversation.
func DeveloperBundle() (*Bundle, error) {
	bundle, err := developerBundleTemplate()
	return cloneBundle(bundle), err
}

// DeveloperBundleWithEffectsCatalog composes the developer WebSocket client
// against one exact host effect catalog. Custom host effect profiles pass the
// digest exposed by their EffectsFactory, so the socket cannot substitute a
// different declaration set after manifest verification.
func DeveloperBundleWithEffectsCatalog(catalogDigest string) (*Bundle, error) {
	defaultDigest, err := host.DefaultEffectsCatalogDigest()
	if err != nil {
		return nil, fmt.Errorf("default effect catalog: %w", err)
	}
	if catalogDigest == defaultDigest {
		return DeveloperBundle()
	}
	return buildDeveloperBundle(catalogDigest)
}

func buildDeveloperBundle(catalogDigest string) (*Bundle, error) {
	websocketPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-realtime", Operations: []string{"websocket"},
	}
	managementPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-management", Operations: []string{"http"},
	}
	effectsPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-effects", Operations: []string{"websocket"},
	}
	definitions := []moduleDefinition{
		{
			entry: "slots", file: "slots.js", pluginName: "openrealtime.presentation.client.slots",
			provides: []plugin.Contract{presentation.ClientSlotsContract},
		},
		{
			entry: "transport", file: "transport-websocket.js",
			pluginName:  "openrealtime.presentation.client.websocket",
			provides:    []plugin.Contract{presentation.ClientConnectionContract},
			permissions: []plugin.Permission{websocketPermission},
			grants:      []plugin.Permission{websocketPermission},
		},
		{
			entry: "reducer", file: "reducer.js", pluginName: "openrealtime.presentation.client.reducer",
			provides: []plugin.Contract{
				presentation.ClientStateContract, presentation.ClientInspectionAccessContract,
				presentation.ClientProtocolEventsContract, presentation.ClientCodecContract,
			},
			requires: []plugin.Requirement{{Contract: presentation.ClientConnectionContract}},
		},
		{
			entry: "session-configuration", file: "session-configuration.js",
			pluginName: "openrealtime.presentation.client.session-configuration",
			provides:   []plugin.Contract{presentation.ClientSessionConfigurationContract},
			requires:   []plugin.Requirement{{Contract: presentation.ClientStateContract}},
		},
		{
			entry: "debug-session", file: "debug-session.js",
			pluginName: "openrealtime.presentation.client.debug-session",
			requires:   []plugin.Requirement{{Contract: presentation.ClientSessionConfigurationContract}},
		},
		{
			entry: "effects", file: "effects-client.js",
			pluginName: "openrealtime.presentation.client.effects",
			provides:   []plugin.Contract{presentation.ClientEffectsContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientStateContract},
				{Contract: presentation.ClientProtocolEventsContract},
				{Contract: presentation.ClientCodecContract},
				{Contract: presentation.ClientSessionConfigurationContract},
			},
			permissions: []plugin.Permission{effectsPermission}, grants: []plugin.Permission{effectsPermission},
		},
		{
			entry: "artifact-references", file: "artifact-references.js",
			pluginName: "openrealtime.presentation.client.artifact-references",
			provides:   []plugin.Contract{presentation.ClientArtifactsContract},
			requires:   []plugin.Requirement{{Contract: presentation.ClientEffectsContract}},
		},
		{
			entry: "inspection", file: "inspection-client.js",
			pluginName: "openrealtime.presentation.client.inspection",
			provides:   []plugin.Contract{presentation.ClientInspectionContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientInspectionAccessContract}, {Contract: presentation.ClientCodecContract},
			},
			permissions: []plugin.Permission{managementPermission},
			grants:      []plugin.Permission{managementPermission},
		},
		{
			entry: "view", file: "text-view.js", pluginName: "openrealtime.presentation.client.text-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientStateContract},
			},
		},
		{
			entry: "confirmation-view", file: "confirmation-view.js",
			pluginName: "openrealtime.presentation.client.confirmation-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientEffectsContract},
			},
		},
		{
			entry: "artifact-view", file: "artifact-view.js",
			pluginName: "openrealtime.presentation.client.artifact-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientArtifactsContract},
			},
		},
		{
			entry: "inspection-view", file: "inspection-view.js",
			pluginName: "openrealtime.presentation.client.inspection-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientInspectionContract},
			},
		},
		{
			entry: "trace-view", file: "trace-view.js",
			pluginName: "openrealtime.presentation.client.trace-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientInspectionContract},
			},
		},
	}
	definitions = append(definitions, developerManagementDefinitions(true)...)
	return buildBundle("openrealtime.browser.developer", definitions, []presentation.ManifestEndpoint{
		{Name: "effects.local", Method: "GET", Path: "/client/v1/effects",
			Protocol: clientEffectsProtocol, CatalogDigest: catalogDigest},
		{Name: "management.authoring", Method: "POST", Path: "/client/v1/management/authoring",
			Protocol: managementProtocol},
		{Name: "management.sessions", Method: "GET", Path: "/client/v1/management/sessions",
			Protocol: managementProtocol},
		{Name: "management.static", Method: "GET", Path: "/client/v1/management",
			Protocol: managementProtocol},
		{Name: "realtime.websocket", Method: "GET", Path: "/client/v1/realtime"},
	})
}

// ObserverDeveloperWebRTCBundle is the media-enabled observer composition.
// Like ObserverDeveloperBundle it cannot advertise or execute local effects;
// WebRTC media, video, diagnostics, inspection, and authoring remain separate
// descriptor-locked plugins.
func ObserverDeveloperWebRTCBundle() (*Bundle, error) {
	bundle, err := observerDeveloperWebRTCBundleTemplate()
	return cloneBundle(bundle), err
}

func buildObserverDeveloperWebRTCBundle() (*Bundle, error) {
	audioPermission := plugin.Permission{
		Kind: "device.media", Resource: "browser-audio", Operations: []string{"microphone", "playout"},
	}
	videoPermission := plugin.Permission{
		Kind: "device.media", Resource: "browser-video", Operations: []string{"camera", "screen"},
	}
	webrtcPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-realtime", Operations: []string{"webrtc"},
	}
	inspectionPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-management", Operations: []string{"http"},
	}
	definitions := []moduleDefinition{
		{
			entry: "slots", file: "slots.js", pluginName: "openrealtime.presentation.client.slots",
			provides: []plugin.Contract{presentation.ClientSlotsContract},
		},
		{
			entry: "media", file: "media-webrtc.js",
			pluginName:  "openrealtime.presentation.client.webrtc-media",
			provides:    []plugin.Contract{presentation.ClientMediaContract},
			permissions: []plugin.Permission{audioPermission, videoPermission},
			grants:      []plugin.Permission{audioPermission, videoPermission},
		},
		{
			entry: "transport", file: "transport-webrtc.js",
			pluginName: "openrealtime.presentation.client.webrtc",
			provides: []plugin.Contract{
				presentation.ClientConnectionContract, presentation.ClientTransportDiagnosticsContract,
			},
			requires:    []plugin.Requirement{{Contract: presentation.ClientMediaContract}},
			permissions: []plugin.Permission{webrtcPermission}, grants: []plugin.Permission{webrtcPermission},
		},
		{
			entry: "reducer", file: "reducer.js", pluginName: "openrealtime.presentation.client.reducer",
			provides: []plugin.Contract{
				presentation.ClientStateContract, presentation.ClientInspectionAccessContract,
				presentation.ClientProtocolEventsContract, presentation.ClientCodecContract,
			},
			requires: []plugin.Requirement{{Contract: presentation.ClientConnectionContract}},
		},
		{
			entry: "session-configuration", file: "session-configuration.js",
			pluginName: "openrealtime.presentation.client.session-configuration",
			provides:   []plugin.Contract{presentation.ClientSessionConfigurationContract},
			requires:   []plugin.Requirement{{Contract: presentation.ClientStateContract}},
		},
		{
			entry: "video", file: "video-protocol.js",
			pluginName: "openrealtime.presentation.client.video-protocol",
			provides:   []plugin.Contract{presentation.ClientVideoContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientConnectionContract},
				{Contract: presentation.ClientStateContract},
				{Contract: presentation.ClientMediaContract},
				{Contract: presentation.ClientSessionConfigurationContract},
			},
		},
		{
			entry: "debug-session", file: "debug-session.js",
			pluginName: "openrealtime.presentation.client.debug-session",
			requires:   []plugin.Requirement{{Contract: presentation.ClientSessionConfigurationContract}},
		},
		{
			entry: "inspection", file: "inspection-client.js",
			pluginName: "openrealtime.presentation.client.inspection",
			provides:   []plugin.Contract{presentation.ClientInspectionContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientInspectionAccessContract}, {Contract: presentation.ClientCodecContract},
			},
			permissions: []plugin.Permission{inspectionPermission}, grants: []plugin.Permission{inspectionPermission},
		},
		{
			entry: "view", file: "text-view.js", pluginName: "openrealtime.presentation.client.text-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientStateContract},
			},
		},
		{
			entry: "video-controls", file: "video-controls.js",
			pluginName: "openrealtime.presentation.client.video-controls",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientVideoContract},
			},
		},
		{
			entry: "transport-diagnostics", file: "transport-diagnostics-view.js",
			pluginName: "openrealtime.presentation.client.transport-diagnostics-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract},
				{Contract: presentation.ClientTransportDiagnosticsContract},
			},
		},
		{
			entry: "inspection-view", file: "inspection-view.js",
			pluginName: "openrealtime.presentation.client.inspection-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientInspectionContract},
			},
		},
		{
			entry: "trace-view", file: "trace-view.js",
			pluginName: "openrealtime.presentation.client.trace-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientInspectionContract},
			},
		},
	}
	definitions = append(definitions, developerManagementDefinitions(false)...)
	return buildBundle("openrealtime.browser.developer-observer-webrtc", definitions, []presentation.ManifestEndpoint{
		{Name: "management.authoring", Method: "POST", Path: "/client/v1/management/authoring",
			Protocol: managementProtocol},
		{Name: "management.sessions", Method: "GET", Path: "/client/v1/management/sessions",
			Protocol: managementProtocol},
		{Name: "management.static", Method: "GET", Path: "/client/v1/management",
			Protocol: managementProtocol},
		{Name: "realtime.webrtc", Method: "POST", Path: "/client/v1/realtime/calls"},
	})
}

// DeveloperWebRTCBundle selects a native browser media provider and WebRTC
// transport while retaining the same reducer, views, and management client as
// DeveloperBundle. The profile is a lockable composition, not a transport
// branch inside those consumers.
func DeveloperWebRTCBundle() (*Bundle, error) {
	bundle, err := developerWebRTCBundleTemplate()
	return cloneBundle(bundle), err
}

// DeveloperWebRTCBundleWithEffectsCatalog is the media-enabled counterpart
// of DeveloperBundleWithEffectsCatalog.
func DeveloperWebRTCBundleWithEffectsCatalog(catalogDigest string) (*Bundle, error) {
	defaultDigest, err := host.DefaultEffectsCatalogDigest()
	if err != nil {
		return nil, fmt.Errorf("default effect catalog: %w", err)
	}
	if catalogDigest == defaultDigest {
		return DeveloperWebRTCBundle()
	}
	return buildDeveloperWebRTCBundle(catalogDigest)
}

func buildDeveloperWebRTCBundle(catalogDigest string) (*Bundle, error) {
	audioPermission := plugin.Permission{
		Kind: "device.media", Resource: "browser-audio", Operations: []string{"microphone", "playout"},
	}
	videoPermission := plugin.Permission{
		Kind: "device.media", Resource: "browser-video", Operations: []string{"camera", "screen"},
	}
	webrtcPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-realtime", Operations: []string{"webrtc"},
	}
	managementPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-management", Operations: []string{"http"},
	}
	effectsPermission := plugin.Permission{
		Kind: "network.connect", Resource: "host-effects", Operations: []string{"websocket"},
	}
	definitions := []moduleDefinition{
		{
			entry: "slots", file: "slots.js", pluginName: "openrealtime.presentation.client.slots",
			provides: []plugin.Contract{presentation.ClientSlotsContract},
		},
		{
			entry: "media", file: "media-webrtc.js",
			pluginName:  "openrealtime.presentation.client.webrtc-media",
			provides:    []plugin.Contract{presentation.ClientMediaContract},
			permissions: []plugin.Permission{audioPermission, videoPermission},
			grants:      []plugin.Permission{audioPermission, videoPermission},
		},
		{
			entry: "transport", file: "transport-webrtc.js",
			pluginName: "openrealtime.presentation.client.webrtc",
			provides: []plugin.Contract{
				presentation.ClientConnectionContract, presentation.ClientTransportDiagnosticsContract,
			},
			requires:    []plugin.Requirement{{Contract: presentation.ClientMediaContract}},
			permissions: []plugin.Permission{webrtcPermission}, grants: []plugin.Permission{webrtcPermission},
		},
		{
			entry: "reducer", file: "reducer.js", pluginName: "openrealtime.presentation.client.reducer",
			provides: []plugin.Contract{
				presentation.ClientStateContract, presentation.ClientInspectionAccessContract,
				presentation.ClientProtocolEventsContract, presentation.ClientCodecContract,
			},
			requires: []plugin.Requirement{{Contract: presentation.ClientConnectionContract}},
		},
		{
			entry: "session-configuration", file: "session-configuration.js",
			pluginName: "openrealtime.presentation.client.session-configuration",
			provides:   []plugin.Contract{presentation.ClientSessionConfigurationContract},
			requires:   []plugin.Requirement{{Contract: presentation.ClientStateContract}},
		},
		{
			entry: "video", file: "video-protocol.js",
			pluginName: "openrealtime.presentation.client.video-protocol",
			provides:   []plugin.Contract{presentation.ClientVideoContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientConnectionContract},
				{Contract: presentation.ClientStateContract},
				{Contract: presentation.ClientMediaContract},
				{Contract: presentation.ClientSessionConfigurationContract},
			},
		},
		{
			entry: "debug-session", file: "debug-session.js",
			pluginName: "openrealtime.presentation.client.debug-session",
			requires:   []plugin.Requirement{{Contract: presentation.ClientSessionConfigurationContract}},
		},
		{
			entry: "effects", file: "effects-client.js",
			pluginName: "openrealtime.presentation.client.effects",
			provides:   []plugin.Contract{presentation.ClientEffectsContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientStateContract},
				{Contract: presentation.ClientProtocolEventsContract},
				{Contract: presentation.ClientCodecContract},
				{Contract: presentation.ClientSessionConfigurationContract},
			},
			permissions: []plugin.Permission{effectsPermission}, grants: []plugin.Permission{effectsPermission},
		},
		{
			entry: "artifact-references", file: "artifact-references.js",
			pluginName: "openrealtime.presentation.client.artifact-references",
			provides:   []plugin.Contract{presentation.ClientArtifactsContract},
			requires:   []plugin.Requirement{{Contract: presentation.ClientEffectsContract}},
		},
		{
			entry: "inspection", file: "inspection-client.js",
			pluginName: "openrealtime.presentation.client.inspection",
			provides:   []plugin.Contract{presentation.ClientInspectionContract},
			requires: []plugin.Requirement{
				{Contract: presentation.ClientInspectionAccessContract}, {Contract: presentation.ClientCodecContract},
			},
			permissions: []plugin.Permission{managementPermission},
			grants:      []plugin.Permission{managementPermission},
		},
		{
			entry: "view", file: "text-view.js", pluginName: "openrealtime.presentation.client.text-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientStateContract},
			},
		},
		{
			entry: "confirmation-view", file: "confirmation-view.js",
			pluginName: "openrealtime.presentation.client.confirmation-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientEffectsContract},
			},
		},
		{
			entry: "artifact-view", file: "artifact-view.js",
			pluginName: "openrealtime.presentation.client.artifact-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientArtifactsContract},
			},
		},
		{
			entry: "video-controls", file: "video-controls.js",
			pluginName: "openrealtime.presentation.client.video-controls",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientVideoContract},
			},
		},
		{
			entry: "transport-diagnostics", file: "transport-diagnostics-view.js",
			pluginName: "openrealtime.presentation.client.transport-diagnostics-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract},
				{Contract: presentation.ClientTransportDiagnosticsContract},
			},
		},
		{
			entry: "inspection-view", file: "inspection-view.js",
			pluginName: "openrealtime.presentation.client.inspection-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientInspectionContract},
			},
		},
		{
			entry: "trace-view", file: "trace-view.js",
			pluginName: "openrealtime.presentation.client.trace-view",
			requires: []plugin.Requirement{
				{Contract: presentation.ClientSlotsContract}, {Contract: presentation.ClientInspectionContract},
			},
		},
	}
	definitions = append(definitions, developerManagementDefinitions(true)...)
	return buildBundle("openrealtime.browser.developer-webrtc", definitions, []presentation.ManifestEndpoint{
		{Name: "effects.local", Method: "GET", Path: "/client/v1/effects",
			Protocol: clientEffectsProtocol, CatalogDigest: catalogDigest},
		{Name: "management.authoring", Method: "POST", Path: "/client/v1/management/authoring",
			Protocol: managementProtocol},
		{Name: "management.sessions", Method: "GET", Path: "/client/v1/management/sessions",
			Protocol: managementProtocol},
		{Name: "management.static", Method: "GET", Path: "/client/v1/management",
			Protocol: managementProtocol},
		{Name: "realtime.webrtc", Method: "POST", Path: "/client/v1/realtime/calls"},
	})
}

func buildBundle(
	profileName string,
	definitions []moduleDefinition,
	endpoints []presentation.ManifestEndpoint,
) (*Bundle, error) {
	sources := make([]host.ModuleSource, 0, len(definitions)+1)
	bootstrap, err := modules.ReadFile("assets/bootstrap.js")
	if err != nil {
		return nil, err
	}
	sources = append(sources, host.ModuleSource{
		Name: "bootstrap.js", MediaType: "text/javascript", Content: bootstrap,
	})
	for _, definition := range definitions {
		content := slices.Clone(definition.content)
		if len(content) == 0 {
			content, err = browserModule(definition.file)
			if err != nil {
				return nil, err
			}
		}
		sources = append(sources, host.ModuleSource{
			Name: definition.file, MediaType: "text/javascript", Content: content,
		})
		for _, alternative := range definition.alternatives {
			sources = append(sources, host.ModuleSource{
				Name: alternative.file, MediaType: "text/javascript",
				Content: slices.Clone(alternative.content),
			})
		}
	}
	moduleStore, err := host.NewModuleStoreFactory(1, sources)
	if err != nil {
		return nil, err
	}
	assets := make(map[string]plugin.Asset)
	for _, asset := range moduleStore.Descriptor().Assets {
		assets[asset.Name] = asset
	}

	catalog := plugin.NewCatalog()
	entries := make([]plugin.ProfileEntry, 0, len(definitions))
	for _, definition := range definitions {
		descriptorAssets := []plugin.Asset{assets[definition.file]}
		for _, alternative := range definition.alternatives {
			descriptorAssets = append(descriptorAssets, assets[alternative.file])
		}
		descriptor := plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          definition.pluginName, Revision: 1,
			Realm: plugin.ClientRealm, Platforms: []string{"browser"},
			Provides: definition.provides, Requires: definition.requires,
			Permissions: definition.permissions, Assets: descriptorAssets,
		}
		if _, err := catalog.Register(descriptor); err != nil {
			return nil, err
		}
		entries = append(entries, plugin.ProfileEntry{
			ID: definition.entry, Plugin: definition.pluginName, Scope: "root",
		})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          profileName, Revision: 1, Realm: plugin.ClientRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries,
	})
	if err != nil {
		return nil, err
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		return nil, err
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		return nil, err
	}
	manifest := presentation.ClientManifest{
		FormatVersion: presentation.ManifestFormatVersion, Platform: "browser", Plan: plan,
		Endpoints: endpoints,
	}
	for _, definition := range definitions {
		asset := assets[definition.file]
		manifest.Implementations = append(manifest.Implementations, presentation.ManifestImplementation{
			Entry: definition.entry, Implementation: "browser-esm:" + definition.file,
			Artifact: inspect.ArtifactIdentity{
				ID: "module://" + strings.TrimSuffix(definition.file, ".js"), Digest: asset.Digest,
			},
			Entrypoint: definition.file,
		})
		manifestAssets := []plugin.Asset{asset}
		for _, alternative := range definition.alternatives {
			manifestAssets = append(manifestAssets, assets[alternative.file])
		}
		for _, candidate := range manifestAssets {
			manifest.Assets = append(manifest.Assets, presentation.ManifestAsset{
				Entry: definition.entry, Name: candidate.Name, MediaType: candidate.MediaType,
				Digest: candidate.Digest,
				Path:   "/client/v1/modules/" + strings.TrimPrefix(candidate.Digest, "sha256:"),
			})
		}
		if len(definition.grants) > 0 {
			manifest.Grants = append(manifest.Grants, presentation.ManifestGrant{
				Entry: definition.entry, Permissions: definition.grants,
			})
		}
	}
	manifest, err = presentation.FreezeManifest(manifest)
	if err != nil {
		return nil, err
	}
	manifestHost, err := host.NewManifestFactory(manifest)
	if err != nil {
		return nil, err
	}
	bootstrapAsset, found := assets["bootstrap.js"]
	if !found {
		return nil, fmt.Errorf("browser bundle omitted bootstrap asset")
	}
	shell, err := host.NewBrowserShellFactory(1, bootstrapAsset.Digest)
	if err != nil {
		return nil, err
	}
	return &Bundle{
		Profile: profile, Lock: lock, Plan: plan, Manifest: manifest,
		ModuleStore: moduleStore, ManifestHost: manifestHost, Shell: shell,
	}, nil
}

func browserModule(name string) ([]byte, error) {
	if name != "reducer.js" {
		return modules.ReadFile("assets/" + name)
	}
	core, err := clientreducer.JavaScriptSource()
	if err != nil {
		return nil, err
	}
	adapter, err := modules.ReadFile("assets/reducer-adapter.js")
	if err != nil {
		return nil, err
	}
	var combined bytes.Buffer
	combined.Grow(len(core) + len(adapter) + 2)
	combined.Write(core)
	combined.WriteString("\n\n")
	combined.Write(adapter)
	return combined.Bytes(), nil
}
