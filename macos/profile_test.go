package macos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestNativeBundlePinsProfileLockPlanAndImplementations(t *testing.T) {
	bundle, err := NewNativeBundle()
	if err != nil {
		t.Fatal(err)
	}
	for name, validate := range map[string]func() error{
		"profile":  bundle.Profile.Validate,
		"lock":     bundle.Lock.Validate,
		"plan":     bundle.Plan.Validate,
		"manifest": bundle.Manifest.Validate,
	} {
		if err := validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if bundle.Lock.ProfileFingerprint != bundle.Profile.Fingerprint ||
		bundle.Plan.ProfileFingerprint != bundle.Profile.Fingerprint ||
		bundle.Plan.LockFingerprint != bundle.Lock.Fingerprint ||
		bundle.Manifest.Plan.Fingerprint != bundle.Plan.Fingerprint {
		t.Fatal("native composition identity chain is not exact")
	}
	var order []string
	for _, entry := range bundle.Plan.Entries {
		order = append(order, entry.Entry.ID)
	}
	if want := []string{
		"slots", "strict-json", "transport", "transport-diagnostics", "reducer",
		"protocol-events", "inspection-access", "session-configuration", "media",
		"video", "effects", "artifacts", "inspection", "view",
	}; !reflect.DeepEqual(order, want) {
		t.Fatalf("native mount order = %v, want %v", order, want)
	}
	if want := []presentation.ManifestEndpoint{{
		Name: "effects.local", Method: "GET", Path: "/client/v1/effects",
		Protocol:      "openrealtime.client-effects.v1",
		CatalogDigest: bundle.Manifest.Endpoints[0].CatalogDigest,
	}}; !reflect.DeepEqual(bundle.Manifest.Endpoints, want) ||
		!strings.HasPrefix(bundle.Manifest.Endpoints[0].CatalogDigest, "sha256:") {
		t.Fatalf("native host effect endpoint = %#v", bundle.Manifest.Endpoints)
	}

	again, err := NewNativeBundle()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Profile.Fingerprint != again.Profile.Fingerprint || bundle.Lock.Fingerprint != again.Lock.Fingerprint ||
		bundle.Plan.Fingerprint != again.Plan.Fingerprint || bundle.Manifest.Fingerprint != again.Manifest.Fingerprint {
		t.Fatal("native composition identity is not deterministic")
	}
	t.Logf("native identity profile=%s lock=%s plan=%s manifest=%s",
		bundle.Profile.Fingerprint, bundle.Lock.Fingerprint, bundle.Plan.Fingerprint, bundle.Manifest.Fingerprint)

	replaced := bundle.Manifest.Clone()
	for index := range replaced.Implementations {
		if replaced.Implementations[index].Entry == "reducer" {
			replaced.Implementations[index].Implementation = "portable.swift-reducer.replacement"
			artifact, err := nativeArtifact("portable.swift-reducer.replacement")
			if err != nil {
				t.Fatal(err)
			}
			replaced.Implementations[index].Artifact = artifact
		}
	}
	replaced, err = presentation.FreezeManifest(replaced)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Fingerprint == bundle.Manifest.Fingerprint {
		t.Fatal("native reducer replacement did not change manifest identity")
	}
}

func TestNativeDistributionSelectionIsExplicitAndObserverHasNoEffectsAuthority(t *testing.T) {
	compatibility, err := NewNativeBundle()
	if err != nil {
		t.Fatal(err)
	}
	effects, err := NewNativeEffectsDeveloperBundle()
	if err != nil {
		t.Fatal(err)
	}
	if compatibility.Profile.Fingerprint != effects.Profile.Fingerprint ||
		compatibility.Lock.Fingerprint != effects.Lock.Fingerprint ||
		compatibility.Plan.Fingerprint != effects.Plan.Fingerprint ||
		compatibility.Manifest.Fingerprint != effects.Manifest.Fingerprint {
		t.Fatal("compatibility constructor no longer selects the explicit effects distribution")
	}

	observer, err := NewNativeObserverDeveloperBundle()
	if err != nil {
		t.Fatal(err)
	}
	for name, validate := range map[string]func() error{
		"profile": observer.Profile.Validate, "lock": observer.Lock.Validate,
		"plan": observer.Plan.Validate, "manifest": observer.Manifest.Validate,
	} {
		if err := validate(); err != nil {
			t.Fatalf("observer %s: %v", name, err)
		}
	}
	if observer.Profile.Name != "openrealtime.macos.observer-developer" {
		t.Fatalf("observer profile name = %q", observer.Profile.Name)
	}
	if len(observer.Manifest.Endpoints) != 0 {
		t.Fatalf("observer profile invented an effects endpoint: %#v", observer.Manifest.Endpoints)
	}
	for _, entry := range observer.Plan.Entries {
		if entry.Entry.ID == "effects" || entry.Entry.ID == "artifacts" {
			t.Fatalf("observer plan includes privileged provider %q", entry.Entry.ID)
		}
		if entry.Entry.ID == "view" {
			var dependencies []string
			for _, dependency := range entry.Dependencies {
				dependencies = append(dependencies, dependency.Service.Name)
			}
			for _, forbidden := range []string{
				presentation.ClientEffectsContract.Name,
				presentation.ClientArtifactsContract.Name,
			} {
				if slicesContains(dependencies, forbidden) {
					t.Fatalf("observer view depends on omitted service %q", forbidden)
				}
			}
		}
	}
	for _, implementation := range observer.Manifest.Implementations {
		if implementation.Entry == "effects" || implementation.Entry == "artifacts" {
			t.Fatalf("observer manifest installs privileged provider %q", implementation.Entry)
		}
		if implementation.Entry == "view" &&
			implementation.Implementation != "macos.swiftui-observer-view.v1" {
			t.Fatalf("observer view implementation = %q", implementation.Implementation)
		}
	}
	for _, grant := range observer.Manifest.Grants {
		for _, permission := range grant.Permissions {
			if permission.Resource == "host-effects" || permission.Resource == "host-resources" {
				t.Fatalf("observer permission escaped its authority-free profile: %#v", grant)
			}
		}
	}
	if _, err := NewNativeBundleForDistribution(NativeDistribution("unknown")); err == nil {
		t.Fatal("unknown native distribution was accepted")
	}
}

func TestNativeEndpointDirectoriesAreExactAndResourceBacked(t *testing.T) {
	tests := []struct {
		name         string
		distribution NativeDistribution
		resource     string
		wantNames    []presentation.EndpointName
	}{
		{
			name: "observer", distribution: NativeObserverDeveloperDistribution,
			resource: "Sources/OpenRealtimeMac/Resources/native-observer-endpoints.json",
			wantNames: []presentation.EndpointName{
				presentation.EndpointManagement, presentation.EndpointRealtimeWebSocket,
			},
		},
		{
			name: "effects", distribution: NativeEffectsDeveloperDistribution,
			resource: "Sources/OpenRealtimeMac/Resources/native-endpoints.json",
			wantNames: []presentation.EndpointName{
				presentation.EndpointEffects, presentation.EndpointManagement,
				presentation.EndpointRealtimeWebSocket, presentation.EndpointArtifacts,
				presentation.EndpointDownloads,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle, err := NewNativeBundleForDistribution(test.distribution)
			if err != nil {
				t.Fatal(err)
			}
			if err := bundle.EndpointDirectory.Validate(); err != nil {
				t.Fatal(err)
			}
			var names []presentation.EndpointName
			for _, endpoint := range bundle.EndpointDirectory.Endpoints {
				names = append(names, endpoint.Name)
				if strings.Contains(endpoint.URL, "@") || strings.Contains(endpoint.URL, "?") {
					t.Fatalf("native endpoint contains URL authority material: %#v", endpoint)
				}
			}
			if !reflect.DeepEqual(names, test.wantNames) {
				t.Fatalf("native endpoint names = %v, want %v", names, test.wantNames)
			}
			source, err := os.ReadFile(test.resource)
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(bytes.NewReader(source))
			decoder.DisallowUnknownFields()
			var resource presentation.EndpointDirectory
			if err := decoder.Decode(&resource); err != nil {
				t.Fatal(err)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				t.Fatalf("endpoint resource has trailing data: %v", err)
			}
			if err := resource.Validate(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resource, bundle.EndpointDirectory) {
				t.Fatalf("endpoint resource = %#v, want %#v", resource, bundle.EndpointDirectory)
			}

			bundle.EndpointDirectory.Endpoints[0].URL = "https://attacker.invalid/rebound"
			again, err := NewNativeBundleForDistribution(test.distribution)
			if err != nil {
				t.Fatal(err)
			}
			if again.EndpointDirectory.Fingerprint != resource.Fingerprint ||
				again.EndpointDirectory.Endpoints[0].URL != resource.Endpoints[0].URL {
				t.Fatal("native endpoint directory retained caller mutation")
			}
		})
	}
}

func TestNativeEndpointDirectoryRefusesOmissionTamperSubstitutionAndCredentials(t *testing.T) {
	observer, err := DefaultNativeEndpointDirectory(NativeObserverDeveloperDistribution)
	if err != nil {
		t.Fatal(err)
	}
	missing := observer.Clone()
	missing.Endpoints = missing.Endpoints[1:]
	missing, err = presentation.FreezeEndpointDirectory(missing.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewNativeBundleForDistributionWithEndpointDirectory(
		NativeObserverDeveloperDistribution, missing,
	); err == nil || !strings.Contains(err.Error(), `does not declare "management.canonical"`) {
		t.Fatalf("missing native management endpoint error = %v", err)
	}

	tampered := observer.Clone()
	tampered.Endpoints[0].URL = "https://other.example.invalid/client/v1/management"
	if _, err := NewNativeBundleForDistributionWithEndpointDirectory(
		NativeObserverDeveloperDistribution, tampered,
	); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("tampered native endpoint directory error = %v", err)
	}

	effects, err := DefaultNativeEndpointDirectory(NativeEffectsDeveloperDistribution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewNativeBundleForDistributionWithEndpointDirectory(
		NativeObserverDeveloperDistribution, effects,
	); err == nil || !strings.Contains(err.Error(), "exact selected set") {
		t.Fatalf("observer accepted privileged endpoint set: %v", err)
	}
	substituted := effects.Clone()
	for index := range substituted.Endpoints {
		if substituted.Endpoints[index].Name == presentation.EndpointEffects {
			substituted.Endpoints[index].URL = "ws://127.0.0.1:8765/client/v1/substituted"
		}
	}
	substituted, err = presentation.FreezeEndpointDirectory(substituted.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewNativeBundleForDistributionWithEndpointDirectory(
		NativeEffectsDeveloperDistribution, substituted,
	); err == nil || !strings.Contains(err.Error(), "manifest declaration") {
		t.Fatalf("substituted effects endpoint error = %v", err)
	}
	substituted = effects.Clone()
	for index := range substituted.Endpoints {
		if substituted.Endpoints[index].Name == presentation.EndpointArtifacts {
			substituted.Endpoints[index].URL = "http://127.0.0.1:8765/client/v1/substituted"
		}
	}
	substituted, err = presentation.FreezeEndpointDirectory(substituted.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewNativeBundleForDistributionWithEndpointDirectory(
		NativeEffectsDeveloperDistribution, substituted,
	); err == nil || !strings.Contains(err.Error(), "protocol path") {
		t.Fatalf("substituted artifact endpoint error = %v", err)
	}
	custom, err := presentation.FreezeEndpointDirectory([]presentation.Endpoint{
		{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "wss://realtime.example/client/v1/realtime",
		},
		{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: "https://management.example/custom/v7",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	customBundle, err := NewNativeBundleForDistributionWithEndpointDirectory(
		NativeObserverDeveloperDistribution, custom,
	)
	if err != nil || customBundle.EndpointDirectory.Fingerprint != custom.Fingerprint {
		t.Fatalf("exact cross-origin native directory = %#v, %v", customBundle, err)
	}
	custom.Endpoints[0].URL = "https://attacker.invalid/rebound"
	if err := customBundle.EndpointDirectory.Validate(); err != nil {
		t.Fatalf("native bundle retained its caller's endpoint slice: %v", err)
	}
	queryDirectory, err := presentation.FreezeEndpointDirectory([]presentation.Endpoint{
		{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "wss://realtime.example/client/v1/realtime?token=secret",
		},
		{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: "https://management.example/custom/v7",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewNativeBundleForDistributionWithEndpointDirectory(
		NativeObserverDeveloperDistribution, queryDirectory,
	); err == nil || !strings.Contains(err.Error(), "portable exact subset") {
		t.Fatalf("credential-like realtime query error = %v", err)
	}

	for _, endpoint := range []presentation.Endpoint{
		{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "wss://user:secret@server.example/client/v1/realtime",
		},
		{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolClientEffects,
			URL: "https://server.example/client/v1/management",
		},
	} {
		if _, err := presentation.FreezeEndpointDirectory([]presentation.Endpoint{endpoint}); err == nil {
			t.Fatalf("invalid native endpoint was accepted: %#v", endpoint)
		}
	}
}

func TestLegacyNativeSameOriginEndpointDirectoryIsExplicit(t *testing.T) {
	directory, err := LegacyNativeSameOriginEndpointDirectory(
		NativeEffectsDeveloperDistribution, "wss://legacy.example/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[presentation.EndpointName]string{
		presentation.EndpointRealtimeWebSocket: "wss://legacy.example/v1/realtime",
		presentation.EndpointManagement:        "https://legacy.example/openrealtime/v1",
		presentation.EndpointEffects:           "wss://legacy.example/client/v1/effects",
		presentation.EndpointArtifacts:         "https://legacy.example/client/v1/artifacts",
		presentation.EndpointDownloads:         "https://legacy.example/client/v1/downloads",
	}
	for name, url := range want {
		endpoint, found := directory.Lookup(name)
		if !found || endpoint.URL != url {
			t.Fatalf("legacy native endpoint %s = %#v", name, endpoint)
		}
	}
	if _, err := LegacyNativeSameOriginEndpointDirectory(
		NativeObserverDeveloperDistribution, "wss://user:secret@legacy.example/v1/realtime",
	); err == nil {
		t.Fatal("legacy native compatibility accepted URL credentials")
	}
}

func TestNativeEffectsDistributionPinsSelectedCatalogDigest(t *testing.T) {
	defaultBundle, err := NewNativeBundle()
	if err != nil {
		t.Fatal(err)
	}
	alternateDigest := "sha256:" + strings.Repeat("9", 64)
	alternate, err := NewNativeEffectsDeveloperBundleWithEffectsCatalog(alternateDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(alternate.Manifest.Endpoints) != 1 ||
		alternate.Manifest.Endpoints[0].CatalogDigest != alternateDigest {
		t.Fatalf("alternate native effects endpoint = %#v", alternate.Manifest.Endpoints)
	}
	if alternate.Profile.Fingerprint != defaultBundle.Profile.Fingerprint ||
		alternate.Lock.Fingerprint != defaultBundle.Lock.Fingerprint ||
		alternate.Plan.Fingerprint != defaultBundle.Plan.Fingerprint {
		t.Fatal("catalog selection changed the installed native provider graph")
	}
	if alternate.Manifest.Fingerprint == defaultBundle.Manifest.Fingerprint {
		t.Fatal("catalog replacement did not change the exact client manifest identity")
	}
	directory, err := presentation.FreezeEndpointDirectory([]presentation.Endpoint{
		{Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "wss://realtime.example/client/v1/realtime"},
		{Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: "https://management.example/custom/v7"},
		{Name: presentation.EndpointEffects, Protocol: presentation.ProtocolClientEffects,
			URL: "wss://effects.example/client/v1/effects"},
		{Name: presentation.EndpointArtifacts, Protocol: presentation.ProtocolHostArtifacts,
			URL: "https://artifacts.example/client/v1/artifacts"},
		{Name: presentation.EndpointDownloads, Protocol: presentation.ProtocolHostDownloads,
			URL: "https://downloads.example/client/v1/downloads"},
	})
	if err != nil {
		t.Fatal(err)
	}
	combined, err := NewNativeEffectsDeveloperBundleWithEffectsCatalogAndEndpointDirectory(
		alternateDigest, directory,
	)
	if err != nil || combined.Manifest.Endpoints[0].CatalogDigest != alternateDigest ||
		combined.EndpointDirectory.Fingerprint != directory.Fingerprint {
		t.Fatalf("combined native host identities = %#v, %v", combined, err)
	}
	for _, invalid := range []string{
		"", "sha256:abc", "sha256:" + strings.Repeat("A", 64),
		"sha512:" + strings.Repeat("9", 64),
	} {
		if _, err := NewNativeEffectsDeveloperBundleWithEffectsCatalog(invalid); err == nil {
			t.Errorf("invalid native effects digest %q was accepted", invalid)
		}
	}
}

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestNativeManifestResourceMatchesGoComposition(t *testing.T) {
	bundle, err := NewNativeBundle()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("Sources/OpenRealtimeMac/Resources/native-client-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var resource nativeResourceManifest
	if err := decoder.Decode(&resource); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("native manifest has trailing data: %v", err)
	}
	if resource.FormatVersion != 1 || resource.Platform != "macos" ||
		resource.ProfileFingerprint != bundle.Profile.Fingerprint ||
		resource.LockFingerprint != bundle.Lock.Fingerprint ||
		resource.PlanFingerprint != bundle.Plan.Fingerprint ||
		resource.ManifestFingerprint != bundle.Manifest.Fingerprint {
		t.Fatalf("native resource identity = %#v; Go profile=%s lock=%s plan=%s manifest=%s",
			resource, bundle.Profile.Fingerprint, bundle.Lock.Fingerprint,
			bundle.Plan.Fingerprint, bundle.Manifest.Fingerprint)
	}
	if !reflect.DeepEqual(resource.Endpoints, bundle.Manifest.Endpoints) {
		t.Fatalf("native resource endpoints = %#v, want %#v", resource.Endpoints, bundle.Manifest.Endpoints)
	}
	core, err := os.ReadFile("../client/reducer/swift/NativeClientCore.swift")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(core), bundle.Manifest.Endpoints[0].CatalogDigest) {
		t.Fatal("portable Swift manifest validator does not pin the shipped host effect catalog")
	}
	if len(resource.Providers) != len(nativeDefinitions) {
		t.Fatalf("native resource providers = %d, want %d", len(resource.Providers), len(nativeDefinitions))
	}
	implementations := make(map[string]string)
	for _, implementation := range bundle.Manifest.Implementations {
		implementations[implementation.Entry] = implementation.Implementation
	}
	planned := make(map[string]plugin.PlannedEntry)
	for _, entry := range bundle.Plan.Entries {
		planned[entry.Entry.ID] = entry
	}
	for index, row := range resource.Providers {
		definition := nativeDefinitions[index]
		if row.ID != definition.id || row.Service != definition.provides.Name ||
			row.Implementation != implementations[row.ID] {
			t.Errorf("native resource provider %d = %#v", index, row)
			continue
		}
		var requires []string
		for _, dependency := range planned[row.ID].Dependencies {
			requires = append(requires, dependency.Service.Name)
		}
		sort.Strings(requires)
		gotRequires := append([]string(nil), row.Requires...)
		sort.Strings(gotRequires)
		if !reflect.DeepEqual(gotRequires, requires) {
			t.Errorf("native resource provider %s dependencies = %v, want %v", row.ID, gotRequires, requires)
		}
		if !reflect.DeepEqual(canonicalPermissions(row.Permissions), canonicalPermissions(definition.permissions)) {
			t.Errorf("native resource provider %s permissions = %#v, want %#v", row.ID, row.Permissions, definition.permissions)
		}
	}
}

func TestNativeObserverManifestResourceMatchesGoComposition(t *testing.T) {
	bundle, err := NewNativeObserverDeveloperBundle()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("Sources/OpenRealtimeMac/Resources/native-observer-client-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var resource nativeResourceManifest
	if err := decoder.Decode(&resource); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("observer native manifest has trailing data: %v", err)
	}
	if resource.FormatVersion != 1 || resource.Platform != "macos" ||
		resource.ProfileFingerprint != bundle.Profile.Fingerprint ||
		resource.LockFingerprint != bundle.Lock.Fingerprint ||
		resource.PlanFingerprint != bundle.Plan.Fingerprint ||
		resource.ManifestFingerprint != bundle.Manifest.Fingerprint {
		t.Fatalf("observer native resource identity = %#v; Go profile=%s lock=%s plan=%s manifest=%s",
			resource, bundle.Profile.Fingerprint, bundle.Lock.Fingerprint,
			bundle.Plan.Fingerprint, bundle.Manifest.Fingerprint)
	}
	if len(resource.Endpoints) != 0 {
		t.Fatalf("observer native resource has endpoints %#v", resource.Endpoints)
	}
	definitions, _, _, err := nativeDistributionDefinitions(NativeObserverDeveloperDistribution)
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Providers) != len(definitions) {
		t.Fatalf("observer native resource providers = %d, want %d", len(resource.Providers), len(definitions))
	}
	implementations := make(map[string]string)
	for _, implementation := range bundle.Manifest.Implementations {
		implementations[implementation.Entry] = implementation.Implementation
	}
	for index, row := range resource.Providers {
		definition := definitions[index]
		if row.ID != definition.id || row.Service != definition.provides.Name ||
			row.Implementation != implementations[row.ID] {
			t.Errorf("observer native resource provider %d = %#v", index, row)
		}
	}
}

func TestNativePermissionsAreIsolatedToNativeAdapters(t *testing.T) {
	bundle, err := NewNativeBundle()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"transport":  {"network.connect\x00realtime-endpoint"},
		"media":      {"device.media\x00native-audio"},
		"video":      {"device.media\x00native-browser", "device.media\x00native-video"},
		"effects":    {"network.connect\x00host-effects"},
		"artifacts":  {"network.connect\x00host-resources"},
		"inspection": {"network.connect\x00management-endpoint"},
	}
	if len(bundle.Manifest.Grants) != len(want) {
		t.Fatalf("native grants = %#v", bundle.Manifest.Grants)
	}
	for _, grant := range bundle.Manifest.Grants {
		var actual []string
		for _, permission := range grant.Permissions {
			actual = append(actual, permission.Kind+"\x00"+permission.Resource)
		}
		sort.Strings(actual)
		expected := append([]string(nil), want[grant.Entry]...)
		sort.Strings(expected)
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("native grant escapes adapter boundary: %#v", grant)
		}
	}
	for _, entry := range bundle.Plan.Entries {
		if _, privileged := want[entry.Entry.ID]; !privileged && len(entry.Descriptor.Permissions) != 0 {
			t.Errorf("native provider %s unexpectedly has permissions %#v", entry.Entry.ID, entry.Descriptor.Permissions)
		}
	}
}

type nativeResourceManifest struct {
	FormatVersion       uint64                          `json:"format_version"`
	Platform            string                          `json:"platform"`
	ProfileFingerprint  string                          `json:"profile_fingerprint"`
	LockFingerprint     string                          `json:"lock_fingerprint"`
	PlanFingerprint     string                          `json:"plan_fingerprint"`
	ManifestFingerprint string                          `json:"manifest_fingerprint"`
	Endpoints           []presentation.ManifestEndpoint `json:"endpoints"`
	Providers           []nativeResourceProvider        `json:"providers"`
}

type nativeResourceProvider struct {
	ID             string              `json:"id"`
	Service        string              `json:"service"`
	Implementation string              `json:"implementation"`
	Requires       []string            `json:"requires"`
	Permissions    []plugin.Permission `json:"permissions"`
}

func canonicalPermissions(input []plugin.Permission) []string {
	var result []string
	for _, permission := range input {
		operations := append([]string(nil), permission.Operations...)
		sort.Strings(operations)
		payload := permission.Kind + "\x00" + permission.Resource + "\x00"
		for _, operation := range operations {
			payload += operation + "\x00"
		}
		digest := sha256.Sum256([]byte(payload))
		result = append(result, hex.EncodeToString(digest[:]))
	}
	sort.Strings(result)
	return result
}

func BenchmarkNewNativeBundle(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		if _, err := NewNativeBundle(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewNativeObserverDeveloperBundle(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		if _, err := NewNativeObserverDeveloperBundle(); err != nil {
			b.Fatal(err)
		}
	}
}
