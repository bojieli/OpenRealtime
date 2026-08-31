package macos

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"sort"
	"sync"

	clientreducer "github.com/bojieli/OpenRealtime/client/reducer"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationhost "github.com/bojieli/OpenRealtime/presentation/host"
)

// nativeSources contains the platform tree. loadNativeArtifactSource filters
// the two legacy local executors excluded by Package.swift so the artifact
// identity covers exactly the code the composable client compiles.
//
//go:embed Package.swift Sources/OpenRealtimeMac/*.swift
var nativeSources embed.FS

type NativeBundle struct {
	Profile           plugin.Profile
	Lock              plugin.Lock
	Plan              plugin.Plan
	Manifest          presentation.ClientManifest
	EndpointDirectory presentation.EndpointDirectory
}

type NativeDistribution string

const (
	// NativeEffectsDeveloperDistribution preserves the original shipped native
	// developer composition, including the explicitly pinned host-effects and
	// hosted-resource providers.
	NativeEffectsDeveloperDistribution NativeDistribution = "effects-developer"
	// NativeObserverDeveloperDistribution is the authority-free developer
	// client: it retains media, protocol diagnostics, and graph inspection but
	// has no effects/artifact providers, grants, or endpoint declaration.
	NativeObserverDeveloperDistribution NativeDistribution = "observer-developer"
)

type nativeDefinition struct {
	id             string
	pluginName     string
	implementation string
	provides       plugin.Contract
	requires       []plugin.Contract
	permissions    []plugin.Permission
}

var nativeDefinitions = []nativeDefinition{
	{id: "slots", pluginName: "openrealtime.presentation.macos.slots", implementation: "macos.swiftui-slots.v1", provides: presentation.ClientSlotsContract},
	{
		id: "strict-json", pluginName: "openrealtime.presentation.macos.strict-json",
		implementation: "portable.swift-strict-json.v1", provides: presentation.ClientCodecContract,
	},
	{
		id: "transport", pluginName: "openrealtime.presentation.macos.websocket",
		implementation: "macos.urlsession-websocket.v1", provides: presentation.ClientConnectionContract,
		requires: []plugin.Contract{presentation.ClientCodecContract},
		permissions: []plugin.Permission{{
			Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"websocket"},
		}},
	},
	{
		id: "transport-diagnostics", pluginName: "openrealtime.presentation.macos.transport-diagnostics",
		implementation: "macos.websocket-diagnostics.v1", provides: presentation.ClientTransportDiagnosticsContract,
		requires: []plugin.Contract{presentation.ClientConnectionContract},
	},
	{
		id: "reducer", pluginName: "openrealtime.presentation.macos.reducer",
		implementation: "portable.swift-reducer.v1", provides: presentation.ClientStateContract,
		requires: []plugin.Contract{presentation.ClientCodecContract, presentation.ClientConnectionContract},
	},
	{
		id: "protocol-events", pluginName: "openrealtime.presentation.macos.protocol-events",
		implementation: "portable.swift-protocol-events.v1", provides: presentation.ClientProtocolEventsContract,
		requires: []plugin.Contract{presentation.ClientStateContract},
	},
	{
		id: "inspection-access", pluginName: "openrealtime.presentation.macos.inspection-access",
		implementation: "portable.swift-inspection-access.v1", provides: presentation.ClientInspectionAccessContract,
		requires: []plugin.Contract{presentation.ClientStateContract},
	},
	{
		id: "session-configuration", pluginName: "openrealtime.presentation.macos.session-configuration",
		implementation: "portable.swift-session-configuration.v1", provides: presentation.ClientSessionConfigurationContract,
		requires: []plugin.Contract{presentation.ClientStateContract},
	},
	{
		id: "media", pluginName: "openrealtime.presentation.macos.media",
		implementation: "macos.av-media.v2", provides: presentation.ClientMediaContract,
		requires: []plugin.Contract{
			presentation.ClientConnectionContract, presentation.ClientStateContract,
			presentation.ClientProtocolEventsContract,
		},
		permissions: []plugin.Permission{
			{Kind: "device.media", Resource: "native-audio", Operations: []string{"microphone", "playout"}},
		},
	},
	{
		id: "video", pluginName: "openrealtime.presentation.macos.video",
		implementation: "macos.video-protocol.v2", provides: presentation.ClientVideoContract,
		requires: []plugin.Contract{
			presentation.ClientConnectionContract, presentation.ClientStateContract,
			presentation.ClientSessionConfigurationContract,
		},
		permissions: []plugin.Permission{
			{Kind: "device.media", Resource: "native-video", Operations: []string{"camera", "screen"}},
			{Kind: "device.media", Resource: "native-browser", Operations: []string{"capture"}},
		},
	},
	{
		id: "effects", pluginName: "openrealtime.presentation.macos.effects",
		implementation: "macos.host-effects.v1", provides: presentation.ClientEffectsContract,
		requires: []plugin.Contract{
			presentation.ClientCodecContract, presentation.ClientStateContract,
			presentation.ClientProtocolEventsContract, presentation.ClientSessionConfigurationContract,
		},
		permissions: []plugin.Permission{{
			Kind: "network.connect", Resource: "host-effects", Operations: []string{"websocket"},
		}},
	},
	{
		id: "artifacts", pluginName: "openrealtime.presentation.macos.artifacts",
		implementation: "macos.host-resource-references.v1", provides: presentation.ClientArtifactsContract,
		requires: []plugin.Contract{presentation.ClientEffectsContract},
		permissions: []plugin.Permission{{
			Kind: "network.connect", Resource: "host-resources", Operations: []string{"http"},
		}},
	},
	{
		id: "inspection", pluginName: "openrealtime.presentation.macos.inspection",
		implementation: "macos.inspection.v1", provides: presentation.ClientInspectionContract,
		requires: []plugin.Contract{presentation.ClientInspectionAccessContract},
		permissions: []plugin.Permission{{
			Kind: "network.connect", Resource: "management-endpoint", Operations: []string{"http"},
		}},
	},
	{
		id: "view", pluginName: "openrealtime.presentation.macos.view",
		implementation: "macos.swiftui-view.v1", provides: presentation.ClientViewContract,
		requires: []plugin.Contract{
			presentation.ClientSlotsContract, presentation.ClientTransportDiagnosticsContract,
			presentation.ClientStateContract, presentation.ClientMediaContract, presentation.ClientVideoContract,
			presentation.ClientEffectsContract, presentation.ClientArtifactsContract,
			presentation.ClientInspectionContract,
		},
	},
}

type nativeArtifactSource struct {
	portableDigest string
	payload        []byte
}

var (
	nativeArtifactSourceOnce = sync.OnceValues(loadNativeArtifactSource)
	nativeArtifactCache      sync.Map
)

// NewNativeBundle is the compatibility constructor for the original explicit
// effects-enabled developer distribution.
func NewNativeBundle() (*NativeBundle, error) {
	return NewNativeEffectsDeveloperBundle()
}

func NewNativeEffectsDeveloperBundle() (*NativeBundle, error) {
	return NewNativeBundleForDistribution(NativeEffectsDeveloperDistribution)
}

func NewNativeObserverDeveloperBundle() (*NativeBundle, error) {
	return NewNativeBundleForDistribution(NativeObserverDeveloperDistribution)
}

// NewNativeEffectsDeveloperBundleWithEffectsCatalog selects an alternate,
// exact host effect catalog without changing the installed native provider
// graph. The digest is data-plane identity, never client authority.
func NewNativeEffectsDeveloperBundleWithEffectsCatalog(catalogDigest string) (*NativeBundle, error) {
	if !validNativeDigest(catalogDigest) {
		return nil, fmt.Errorf("native effects catalog digest must be canonical sha256")
	}
	definitions, profileName, _, err := nativeDistributionDefinitions(NativeEffectsDeveloperDistribution)
	if err != nil {
		return nil, err
	}
	directory, err := DefaultNativeEndpointDirectory(NativeEffectsDeveloperDistribution)
	if err != nil {
		return nil, err
	}
	return newNativeBundle(definitions, profileName, catalogDigest, directory)
}

// NewNativeEffectsDeveloperBundleWithEffectsCatalogAndEndpointDirectory binds
// both replaceable host identities explicitly while preserving the selected
// provider graph and its permission ceilings.
func NewNativeEffectsDeveloperBundleWithEffectsCatalogAndEndpointDirectory(
	catalogDigest string,
	directory presentation.EndpointDirectory,
) (*NativeBundle, error) {
	if !validNativeDigest(catalogDigest) {
		return nil, fmt.Errorf("native effects catalog digest must be canonical sha256")
	}
	definitions, profileName, _, err := nativeDistributionDefinitions(NativeEffectsDeveloperDistribution)
	if err != nil {
		return nil, err
	}
	return newNativeBundle(definitions, profileName, catalogDigest, directory)
}

func NewNativeBundleForDistribution(distribution NativeDistribution) (*NativeBundle, error) {
	definitions, profileName, effectsEnabled, err := nativeDistributionDefinitions(distribution)
	if err != nil {
		return nil, err
	}
	catalogDigest := ""
	if effectsEnabled {
		catalogDigest, err = presentationhost.DefaultEffectsCatalogDigest()
		if err != nil {
			return nil, fmt.Errorf("resolve native host effect catalog: %w", err)
		}
	}
	directory, err := DefaultNativeEndpointDirectory(distribution)
	if err != nil {
		return nil, err
	}
	return newNativeBundle(definitions, profileName, catalogDigest, directory)
}

// NewNativeBundleForDistributionWithEndpointDirectory binds the exact public
// destinations selected by a deployment to an otherwise unchanged native
// provider composition. Endpoint presence grants no authority.
func NewNativeBundleForDistributionWithEndpointDirectory(
	distribution NativeDistribution,
	directory presentation.EndpointDirectory,
) (*NativeBundle, error) {
	definitions, profileName, effectsEnabled, err := nativeDistributionDefinitions(distribution)
	if err != nil {
		return nil, err
	}
	catalogDigest := ""
	if effectsEnabled {
		catalogDigest, err = presentationhost.DefaultEffectsCatalogDigest()
		if err != nil {
			return nil, fmt.Errorf("resolve native host effect catalog: %w", err)
		}
	}
	return newNativeBundle(definitions, profileName, catalogDigest, directory)
}

func newNativeBundle(
	definitions []nativeDefinition,
	profileName, effectsCatalogDigest string,
	directory presentation.EndpointDirectory,
) (*NativeBundle, error) {
	if err := validateNativeEndpointDirectory(definitions, directory); err != nil {
		return nil, err
	}
	catalog := plugin.NewCatalog()
	entries := make([]plugin.ProfileEntry, 0, len(definitions))
	for _, definition := range definitions {
		requirements := make([]plugin.Requirement, 0, len(definition.requires))
		for _, contract := range definition.requires {
			requirements = append(requirements, plugin.Requirement{Contract: contract})
		}
		descriptor := plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          definition.pluginName,
			Revision:      1,
			Realm:         plugin.ClientRealm,
			Platforms:     []string{"macos"},
			Provides:      []plugin.Contract{definition.provides},
			Requires:      requirements,
			Permissions:   definition.permissions,
		}
		if _, err := catalog.Register(descriptor); err != nil {
			return nil, err
		}
		entries = append(entries, plugin.ProfileEntry{
			ID: definition.id, Plugin: definition.pluginName, Scope: "root",
		})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          profileName,
		Revision:      1,
		Realm:         plugin.ClientRealm,
		Scopes:        []plugin.ProfileScope{{Path: "root"}},
		Entries:       entries,
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
		FormatVersion: presentation.ManifestFormatVersion,
		Platform:      "macos",
		Plan:          plan,
	}
	if effectsCatalogDigest != "" {
		manifest.Endpoints = []presentation.ManifestEndpoint{{
			Name: "effects.local", Method: "GET", Path: "/client/v1/effects",
			Protocol: "openrealtime.client-effects.v1", CatalogDigest: effectsCatalogDigest,
		}}
	}
	for _, definition := range definitions {
		artifact, err := nativeArtifact(definition.implementation)
		if err != nil {
			return nil, err
		}
		manifest.Implementations = append(manifest.Implementations, presentation.ManifestImplementation{
			Entry:          definition.id,
			Implementation: definition.implementation,
			Artifact:       artifact,
		})
		if len(definition.permissions) != 0 {
			manifest.Grants = append(manifest.Grants, presentation.ManifestGrant{
				Entry: definition.id, Permissions: definition.permissions,
			})
		}
	}
	manifest, err = presentation.FreezeManifest(manifest)
	if err != nil {
		return nil, err
	}
	return &NativeBundle{
		Profile: profile, Lock: lock, Plan: plan, Manifest: manifest,
		EndpointDirectory: directory.Clone(),
	}, nil
}

// DefaultNativeEndpointDirectory declares the presentation-host routes used
// by the bundled app. Browser and native clients can connect to the same
// unchanged listener because these are public wire endpoints, not in-process
// server bindings.
func DefaultNativeEndpointDirectory(distribution NativeDistribution) (presentation.EndpointDirectory, error) {
	definitions, _, _, err := nativeDistributionDefinitions(distribution)
	if err != nil {
		return presentation.EndpointDirectory{}, err
	}
	endpoints := []presentation.Endpoint{
		{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "ws://" + presentation.DefaultLoopbackHostAddress + "/client/v1/realtime",
		},
		{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: "http://" + presentation.DefaultLoopbackHostAddress + "/client/v1/management",
		},
	}
	if nativeDefinitionsProvide(definitions, presentation.ClientEffectsContract) {
		endpoints = append(endpoints, presentation.Endpoint{
			Name: presentation.EndpointEffects, Protocol: presentation.ProtocolClientEffects,
			URL: "ws://" + presentation.DefaultLoopbackHostAddress + "/client/v1/effects",
		})
	}
	if nativeDefinitionsProvide(definitions, presentation.ClientArtifactsContract) {
		endpoints = append(endpoints,
			presentation.Endpoint{
				Name: presentation.EndpointArtifacts, Protocol: presentation.ProtocolHostArtifacts,
				URL: "http://" + presentation.DefaultLoopbackHostAddress + "/client/v1/artifacts",
			},
			presentation.Endpoint{
				Name: presentation.EndpointDownloads, Protocol: presentation.ProtocolHostDownloads,
				URL: "http://" + presentation.DefaultLoopbackHostAddress + "/client/v1/downloads",
			},
		)
	}
	return presentation.FreezeEndpointDirectory(endpoints)
}

func validateNativeEndpointDirectory(
	definitions []nativeDefinition,
	directory presentation.EndpointDirectory,
) error {
	if err := directory.Validate(); err != nil {
		return fmt.Errorf("native endpoint directory: %w", err)
	}
	want := []struct {
		name     presentation.EndpointName
		protocol string
	}{
		{name: presentation.EndpointRealtimeWebSocket, protocol: presentation.ProtocolRealtimeWebSocket},
	}
	if nativeDefinitionsProvide(definitions, presentation.ClientInspectionContract) {
		want = append(want, struct {
			name     presentation.EndpointName
			protocol string
		}{name: presentation.EndpointManagement, protocol: presentation.ProtocolManagement})
	}
	if nativeDefinitionsProvide(definitions, presentation.ClientEffectsContract) {
		want = append(want, struct {
			name     presentation.EndpointName
			protocol string
		}{name: presentation.EndpointEffects, protocol: presentation.ProtocolClientEffects})
	}
	if nativeDefinitionsProvide(definitions, presentation.ClientArtifactsContract) {
		want = append(want,
			struct {
				name     presentation.EndpointName
				protocol string
			}{name: presentation.EndpointArtifacts, protocol: presentation.ProtocolHostArtifacts},
			struct {
				name     presentation.EndpointName
				protocol string
			}{name: presentation.EndpointDownloads, protocol: presentation.ProtocolHostDownloads},
		)
	}
	for _, endpoint := range want {
		selected, err := directory.Require(endpoint.name, endpoint.protocol)
		if err != nil {
			return fmt.Errorf("native endpoint directory: %w", err)
		}
		if !validNativeEndpointURL(selected.URL) {
			return fmt.Errorf("native endpoint directory %q URL is outside the portable exact subset", endpoint.name)
		}
		if endpoint.name == presentation.EndpointEffects {
			parsed, parseErr := url.Parse(selected.URL)
			if parseErr != nil || parsed.Path != "/client/v1/effects" {
				return fmt.Errorf("native effects endpoint does not match its manifest declaration")
			}
		}
		if endpoint.name == presentation.EndpointArtifacts || endpoint.name == presentation.EndpointDownloads {
			wantPath := "/client/v1/artifacts"
			if endpoint.name == presentation.EndpointDownloads {
				wantPath = "/client/v1/downloads"
			}
			parsed, parseErr := url.Parse(selected.URL)
			if parseErr != nil || parsed.Path != wantPath {
				return fmt.Errorf("native resource endpoint does not match its protocol path")
			}
		}
	}
	if len(directory.Endpoints) != len(want) {
		return fmt.Errorf("native endpoint directory has %d endpoints, want exact selected set of %d",
			len(directory.Endpoints), len(want))
	}
	return nil
}

func validNativeEndpointURL(value string) bool {
	if len(value) == 0 || len(value) > 64<<10 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e ||
			character == '"' || character == '&' || character == '<' ||
			character == '>' || character == '\\' {
			return false
		}
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.RawQuery == "" && parsed.String() == value
}

func nativeDefinitionsProvide(definitions []nativeDefinition, contract plugin.Contract) bool {
	for _, definition := range definitions {
		if definition.provides == contract {
			return true
		}
	}
	return false
}

func validNativeDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func nativeDistributionDefinitions(distribution NativeDistribution) ([]nativeDefinition, string, bool, error) {
	switch distribution {
	case NativeEffectsDeveloperDistribution:
		return append([]nativeDefinition(nil), nativeDefinitions...),
			"openrealtime.macos.developer", true, nil
	case NativeObserverDeveloperDistribution:
		definitions := make([]nativeDefinition, 0, len(nativeDefinitions)-2)
		for _, definition := range nativeDefinitions {
			switch definition.id {
			case "effects", "artifacts":
				continue
			case "view":
				definition.pluginName = "openrealtime.presentation.macos.observer-view"
				definition.implementation = "macos.swiftui-observer-view.v1"
				definition.requires = []plugin.Contract{
					presentation.ClientSlotsContract,
					presentation.ClientTransportDiagnosticsContract,
					presentation.ClientStateContract,
					presentation.ClientMediaContract,
					presentation.ClientVideoContract,
					presentation.ClientInspectionContract,
				}
			}
			definitions = append(definitions, definition)
		}
		return definitions, "openrealtime.macos.observer-developer", false, nil
	default:
		return nil, "", false, fmt.Errorf("unsupported native distribution %q", distribution)
	}
}

func nativeArtifact(implementation string) (inspect.ArtifactIdentity, error) {
	if cached, ok := nativeArtifactCache.Load(implementation); ok {
		return cached.(inspect.ArtifactIdentity), nil
	}
	source, err := nativeArtifactSourceOnce()
	if err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, "openrealtime/native-client-source/v1\x00"+implementation+"\x00"+source.portableDigest)
	_, _ = digest.Write(source.payload)
	identity := inspect.ArtifactIdentity{
		ID:     "native://" + implementation,
		Digest: "sha256:" + hex.EncodeToString(digest.Sum(nil)),
	}
	actual, _ := nativeArtifactCache.LoadOrStore(implementation, identity)
	return actual.(inspect.ArtifactIdentity), nil
}

func loadNativeArtifactSource() (nativeArtifactSource, error) {
	portableDigest, err := clientreducer.SwiftSourceDigest()
	if err != nil {
		return nativeArtifactSource{}, err
	}
	names, err := fs.Glob(nativeSources, "Sources/OpenRealtimeMac/*.swift")
	if err != nil {
		return nativeArtifactSource{}, fmt.Errorf("enumerate native Swift sources: %w", err)
	}
	filtered := names[:0]
	for _, name := range names {
		if name == "Sources/OpenRealtimeMac/DesktopComputer.swift" ||
			name == "Sources/OpenRealtimeMac/ToolHost.swift" {
			continue
		}
		filtered = append(filtered, name)
	}
	names = append(filtered, "Package.swift")
	sort.Strings(names)
	var payload bytes.Buffer
	for _, name := range names {
		source, err := nativeSources.ReadFile(name)
		if err != nil {
			return nativeArtifactSource{}, fmt.Errorf("read native Swift source %s: %w", name, err)
		}
		_, _ = fmt.Fprintf(&payload, "%d:%s%d:", len(name), name, len(source))
		_, _ = payload.Write(source)
	}
	return nativeArtifactSource{portableDigest: portableDigest, payload: payload.Bytes()}, nil
}
