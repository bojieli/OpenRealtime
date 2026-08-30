package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/presentation"
	"github.com/bojieli/OpenRealtime/presentation/host"
)

func TestMinimalBundleIsAnExactReplaceableClientPlan(t *testing.T) {
	bundle, err := MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Lock.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, entry := range bundle.Plan.Entries {
		order = append(order, entry.Entry.ID)
	}
	if !reflect.DeepEqual(order, []string{"slots", "transport", "reducer", "view"}) {
		t.Fatalf("client mount order = %v", order)
	}
	if len(bundle.Manifest.Grants) != 1 || bundle.Manifest.Grants[0].Entry != "transport" {
		t.Fatalf("client permission grants = %#v", bundle.Manifest.Grants)
	}
	for _, entry := range bundle.Plan.Entries {
		if entry.Entry.ID != "transport" && len(entry.Descriptor.Permissions) != 0 {
			t.Fatalf("non-transport plugin %s has permissions %#v",
				entry.Entry.ID, entry.Descriptor.Permissions)
		}
	}
	if bundle.Manifest.Plan.Fingerprint != bundle.Plan.Fingerprint ||
		bundle.Manifest.Platform != "browser" || bundle.Manifest.FormatVersion != presentation.ManifestFormatVersion {
		t.Fatalf("manifest does not pin client plan: %#v", bundle.Manifest)
	}
}

func TestCachedBundlesReturnMutationIsolatedValuesAndConstructConcurrently(t *testing.T) {
	first, err := MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	second, err := MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	wantProfile := second.Profile.Fingerprint
	wantPlan := second.Plan.Fingerprint
	wantManifest := second.Manifest.Fingerprint
	first.Profile.Fingerprint = "mutated"
	first.Plan.Fingerprint = "mutated"
	first.Manifest.Fingerprint = "mutated"
	first.Manifest.Assets[0].Digest = "mutated"
	if second.Profile.Fingerprint != wantProfile || second.Plan.Fingerprint != wantPlan ||
		second.Manifest.Fingerprint != wantManifest || second.Manifest.Assets[0].Digest == "mutated" {
		t.Fatal("cached bundle values shared caller-mutable state")
	}

	builders := []func() (*Bundle, error){
		MinimalBundle, ObserverDeveloperBundle, ObserverDeveloperWebRTCBundle,
		DeveloperBundle, DeveloperWebRTCBundle,
	}
	var group sync.WaitGroup
	errors := make(chan error, 96)
	for index := 0; index < 96; index++ {
		builder := builders[index%len(builders)]
		group.Add(1)
		go func() {
			defer group.Done()
			bundle, buildErr := builder()
			if buildErr != nil {
				errors <- buildErr
				return
			}
			if validateErr := bundle.Manifest.Validate(); validateErr != nil {
				errors <- validateErr
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestEmbeddedBrowserModulesParseAsJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	for _, name := range []string{
		"bootstrap.js", "slots.js", "transport-websocket.js", "reducer.js", "text-view.js",
		"session-configuration.js", "debug-session.js", "inspection-client.js", "inspection-view.js",
		"trace-view.js", "media-webrtc.js", "transport-webrtc.js", "video-protocol.js", "video-controls.js",
		"transport-diagnostics-view.js", "effects-client.js", "artifact-references.js",
		"confirmation-view.js", "artifact-view.js", "management-operator-capability.js",
		"management-transport.js", "management-static.js", "management-authoring.js",
		"authoring-workspace.js", "management-operator-view.js", "authoring-editor-view.js",
		"authoring-configuration-view.js", "authoring-canvas-view.js",
	} {
		t.Run(name, func(t *testing.T) {
			content, err := browserModule(name)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
				t.Fatalf("node --check: %v\n%s", err, output)
			}
		})
	}
}

func TestEffectClientFailsClosedAndPublishesOnlyBoundedReferencesInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	temporary := t.TempDir()
	paths := make([]string, 0, 3)
	for _, name := range []string{"effects-client.js", "artifact-references.js", "reducer.js"} {
		content, err := browserModule(name)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(temporary, strings.TrimSuffix(name, ".js")+".mjs")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	runner, err := filepath.Abs(filepath.Join("testdata", "effects_client.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, append([]string{runner}, paths...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("effect client conformance: %v\n%s", err, output)
	}
}

func TestSessionConfigurationComposesScopedContributionsInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	content, err := browserModule("session-configuration.js")
	if err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(t.TempDir(), "session-configuration.mjs")
	if err := os.WriteFile(module, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runner, err := filepath.Abs(filepath.Join("testdata", "session_configuration.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, runner, module).CombinedOutput(); err != nil {
		t.Fatalf("session configuration conformance: %v\n%s", err, output)
	}
}

func TestTransportSubscribersAreIsolatedInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	websocket, err := filepath.Abs(filepath.Join("assets", "transport-websocket.js"))
	if err != nil {
		t.Fatal(err)
	}
	webrtc, err := filepath.Abs(filepath.Join("assets", "transport-webrtc.js"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := filepath.Abs(filepath.Join("testdata", "transport_isolation.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, runner, websocket, webrtc).CombinedOutput(); err != nil {
		t.Fatalf("transport subscriber isolation: %v\n%s", err, output)
	}
}

func TestInspectionClientTreatsOnlyCapabilityRotationAsLifecycleInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	content, err := browserModule("inspection-client.js")
	if err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(t.TempDir(), "inspection-client.mjs")
	if err := os.WriteFile(module, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runner, err := filepath.Abs(filepath.Join("testdata", "inspection_client.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, runner, module).CombinedOutput(); err != nil {
		t.Fatalf("inspection client lifecycle conformance: %v\n%s", err, output)
	}
}

func TestManagementClientsKeepOperatorAuthorityStrictBoundedAndPrivateInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	temporary := t.TempDir()
	names := []string{
		"management-operator-capability.js", "management-transport.js", "management-static.js",
		"management-authoring.js", "authoring-workspace.js", "reducer.js",
	}
	paths := make([]string, 0, len(names))
	for _, name := range names {
		content, readErr := browserModule(name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		path := filepath.Join(temporary, strings.TrimSuffix(name, ".js")+".mjs")
		if writeErr := os.WriteFile(path, content, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		paths = append(paths, path)
	}
	runner, err := filepath.Abs(filepath.Join("testdata", "management_clients.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, append([]string{runner}, paths...)...).CombinedOutput(); err != nil {
		t.Fatalf("management client conformance: %v\n%s", err, output)
	}
}

func TestManagementViewsRenderMetadataAsTextAndDisposeInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	temporary := t.TempDir()
	names := []string{
		"management-operator-view.js", "authoring-editor-view.js",
		"authoring-configuration-view.js", "authoring-canvas-view.js",
	}
	paths := make([]string, 0, len(names))
	for _, name := range names {
		content, readErr := browserModule(name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		path := filepath.Join(temporary, strings.TrimSuffix(name, ".js")+".mjs")
		if writeErr := os.WriteFile(path, content, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		paths = append(paths, path)
	}
	runner, err := filepath.Abs(filepath.Join("testdata", "management_views.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, append([]string{runner}, paths...)...).CombinedOutput(); err != nil {
		t.Fatalf("management view conformance: %v\n%s", err, output)
	}
}

func TestReducerAdapterPublishesOnlyCanonicalAcceptedEventsInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	content, err := browserModule("reducer.js")
	if err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(t.TempDir(), "reducer.mjs")
	if err := os.WriteFile(module, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runner, err := filepath.Abs(filepath.Join("testdata", "reducer_adapter.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, runner, module).CombinedOutput(); err != nil {
		t.Fatalf("reducer adapter conformance: %v\n%s", err, output)
	}
}

func TestDeveloperWebRTCBundleReplacesTransportAndOwnsMediaPermission(t *testing.T) {
	bundle, err := DeveloperWebRTCBundle()
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, entry := range bundle.Plan.Entries {
		order = append(order, entry.Entry.ID)
	}
	want := []string{
		"slots", "media", "transport", "reducer", "session-configuration", "video", "debug-session",
		"effects", "artifact-references", "inspection", "view", "confirmation-view", "artifact-view",
		"video-controls", "transport-diagnostics", "inspection-view", "trace-view",
		"management-operator", "management-transport", "management-static", "management-authoring",
		"authoring-workspace", "management-operator-view", "authoring-editor-view",
		"authoring-configuration-view", "authoring-canvas-view",
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("WebRTC developer client mount order = %v, want %v", order, want)
	}
	grants := make(map[string][]string)
	for _, row := range bundle.Manifest.Grants {
		for _, permission := range row.Permissions {
			grants[row.Entry] = append(grants[row.Entry], permission.Operations...)
		}
	}
	if !reflect.DeepEqual(grants["media"], []string{"microphone", "playout", "camera", "screen"}) ||
		!reflect.DeepEqual(grants["transport"], []string{"webrtc"}) ||
		!reflect.DeepEqual(grants["inspection"], []string{"http"}) ||
		!reflect.DeepEqual(grants["effects"], []string{"websocket"}) ||
		!reflect.DeepEqual(grants["management-operator"], []string{"header"}) ||
		!reflect.DeepEqual(grants["management-transport"], []string{"authoring", "static"}) {
		t.Fatalf("WebRTC developer client grants = %#v", grants)
	}
	var endpoints []string
	for _, endpoint := range bundle.Manifest.Endpoints {
		endpoints = append(endpoints, endpoint.Name+" "+endpoint.Method+" "+endpoint.Path)
	}
	if !reflect.DeepEqual(endpoints, []string{
		"effects.local GET /client/v1/effects",
		"management.authoring POST /client/v1/management/authoring",
		"management.sessions GET /client/v1/management/sessions",
		"management.static GET /client/v1/management",
		"realtime.webrtc POST /client/v1/realtime/calls",
	}) {
		t.Fatalf("WebRTC developer endpoints = %v", endpoints)
	}
	websocket, err := DeveloperBundle()
	if err != nil {
		t.Fatal(err)
	}
	if websocket.Plan.Fingerprint == bundle.Plan.Fingerprint ||
		websocket.Manifest.Fingerprint == bundle.Manifest.Fingerprint {
		t.Fatal("transport/media replacement did not change locked client identity")
	}
}

func TestDeveloperBundleAddsInspectionAsReplaceableCapability(t *testing.T) {
	bundle, err := DeveloperBundle()
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, entry := range bundle.Plan.Entries {
		order = append(order, entry.Entry.ID)
	}
	want := []string{
		"slots", "transport", "reducer", "session-configuration", "debug-session", "effects",
		"artifact-references", "inspection", "view", "confirmation-view", "artifact-view",
		"inspection-view", "trace-view",
		"management-operator", "management-transport", "management-static", "management-authoring",
		"authoring-workspace", "management-operator-view", "authoring-editor-view",
		"authoring-configuration-view", "authoring-canvas-view",
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("developer client mount order = %v, want %v", order, want)
	}
	if len(bundle.Manifest.Grants) != 5 ||
		bundle.Manifest.Grants[0].Entry != "effects" ||
		bundle.Manifest.Grants[1].Entry != "inspection" ||
		bundle.Manifest.Grants[2].Entry != "management-operator" ||
		bundle.Manifest.Grants[3].Entry != "management-transport" ||
		bundle.Manifest.Grants[4].Entry != "transport" {
		t.Fatalf("developer client permission grants = %#v", bundle.Manifest.Grants)
	}
	var managementEndpoint bool
	for _, endpoint := range bundle.Manifest.Endpoints {
		managementEndpoint = managementEndpoint || endpoint.Name == "management.sessions" &&
			endpoint.Path == "/client/v1/management/sessions"
	}
	if !managementEndpoint {
		t.Fatalf("developer manifest endpoints = %#v", bundle.Manifest.Endpoints)
	}
	defaultCatalog, err := host.DefaultEffectsCatalogDigest()
	if err != nil {
		t.Fatal(err)
	}
	var effectEndpoint *presentation.ManifestEndpoint
	for index := range bundle.Manifest.Endpoints {
		if bundle.Manifest.Endpoints[index].Name == "effects.local" {
			effectEndpoint = &bundle.Manifest.Endpoints[index]
		}
	}
	if effectEndpoint == nil || effectEndpoint.Protocol != clientEffectsProtocol ||
		effectEndpoint.CatalogDigest != defaultCatalog {
		t.Fatalf("developer effect endpoint is not catalog-pinned: %#v", effectEndpoint)
	}
	customCatalog := "sha256:" + strings.Repeat("b", 64)
	custom, err := DeveloperBundleWithEffectsCatalog(customCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if custom.Manifest.Fingerprint == bundle.Manifest.Fingerprint ||
		custom.Plan.Fingerprint != bundle.Plan.Fingerprint {
		t.Fatal("effect catalog replacement did not change only the client deployment identity")
	}
	minimal, err := MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Plan.Fingerprint == minimal.Plan.Fingerprint ||
		bundle.Manifest.Fingerprint == minimal.Manifest.Fingerprint {
		t.Fatal("adding inspection did not change locked client identity")
	}
}

func TestObserverDeveloperBundleHasManagementWithoutImplicitEffects(t *testing.T) {
	bundle, err := ObserverDeveloperBundle()
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]struct{}, len(bundle.Plan.Entries))
	for _, planned := range bundle.Plan.Entries {
		entries[planned.Entry.ID] = struct{}{}
	}
	for _, required := range []string{
		"inspection", "management-operator", "management-transport", "management-static",
		"management-authoring", "authoring-workspace", "authoring-editor-view",
		"authoring-configuration-view", "authoring-canvas-view",
	} {
		if _, found := entries[required]; !found {
			t.Fatalf("observer developer bundle omitted %s", required)
		}
	}
	for _, forbidden := range []string{"effects", "artifact-references", "confirmation-view", "artifact-view"} {
		if _, found := entries[forbidden]; found {
			t.Fatalf("observer developer bundle retained implicit effect plugin %s", forbidden)
		}
	}
	for _, endpoint := range bundle.Manifest.Endpoints {
		if endpoint.Name == "effects.local" {
			t.Fatalf("observer developer manifest advertised local effects: %#v", endpoint)
		}
		if strings.HasPrefix(endpoint.Name, "management.") && endpoint.Protocol != managementProtocol {
			t.Fatalf("management endpoint is not protocol-locked: %#v", endpoint)
		}
	}
	encoded, err := presentation.MarshalManifest(bundle.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Management-Token") || strings.Contains(string(encoded), "Bearer ") {
		t.Fatalf("observer manifest retained capability material: %s", encoded)
	}
}

func TestObserverDeveloperWebRTCBundleHasMediaManagementWithoutImplicitEffects(t *testing.T) {
	bundle, err := ObserverDeveloperWebRTCBundle()
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]struct{}, len(bundle.Plan.Entries))
	for _, planned := range bundle.Plan.Entries {
		entries[planned.Entry.ID] = struct{}{}
	}
	for _, required := range []string{
		"media", "transport", "video", "video-controls", "transport-diagnostics", "inspection",
		"management-static", "management-authoring", "authoring-workspace", "authoring-canvas-view",
	} {
		if _, found := entries[required]; !found {
			t.Fatalf("observer WebRTC bundle omitted %s", required)
		}
	}
	for _, forbidden := range []string{"effects", "artifact-references", "confirmation-view", "artifact-view"} {
		if _, found := entries[forbidden]; found {
			t.Fatalf("observer WebRTC bundle retained implicit effect plugin %s", forbidden)
		}
	}
	foundWebRTC := false
	for _, endpoint := range bundle.Manifest.Endpoints {
		if endpoint.Name == "effects.local" {
			t.Fatalf("observer WebRTC manifest advertised local effects: %#v", endpoint)
		}
		foundWebRTC = foundWebRTC || endpoint.Name == "realtime.webrtc" &&
			endpoint.Method == "POST" && endpoint.Path == "/client/v1/realtime/calls"
	}
	if !foundWebRTC {
		t.Fatalf("observer WebRTC manifest endpoints = %#v", bundle.Manifest.Endpoints)
	}
}

func TestBrowserReducerModuleIsCanonicalCorePlusAdapter(t *testing.T) {
	content, err := browserModule("reducer.js")
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(content), "export class ClientReducer"); count != 1 {
		t.Fatalf("canonical reducer definitions = %d, want 1", count)
	}
	adapter, err := modules.ReadFile("assets/reducer-adapter.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"switch (event.type)", "response.output_text.delta", "new Map()"} {
		if strings.Contains(string(adapter), forbidden) {
			t.Errorf("browser adapter contains reducer logic %q", forbidden)
		}
	}
}

func TestMinimalBundleIdentityPinsCanonicalReducerModule(t *testing.T) {
	bundle, err := MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	content, err := browserModule("reducer.js")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])

	var descriptorDigest, manifestDigest, artifactDigest string
	for _, entry := range bundle.Plan.Entries {
		if entry.Entry.ID == "reducer" {
			if len(entry.Descriptor.Assets) != 1 {
				t.Fatalf("reducer descriptor assets = %#v", entry.Descriptor.Assets)
			}
			descriptorDigest = entry.Descriptor.Assets[0].Digest
		}
	}
	for _, asset := range bundle.Manifest.Assets {
		if asset.Entry == "reducer" && asset.Name == "reducer.js" {
			manifestDigest = asset.Digest
		}
	}
	for _, implementation := range bundle.Manifest.Implementations {
		if implementation.Entry == "reducer" {
			artifactDigest = implementation.Artifact.Digest
		}
	}
	if descriptorDigest != wantDigest || manifestDigest != wantDigest || artifactDigest != wantDigest {
		t.Fatalf("reducer identity descriptor=%q manifest=%q artifact=%q, want %q",
			descriptorDigest, manifestDigest, artifactDigest, wantDigest)
	}

	again, err := MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Profile.Fingerprint != again.Profile.Fingerprint ||
		bundle.Lock.Fingerprint != again.Lock.Fingerprint ||
		bundle.Plan.Fingerprint != again.Plan.Fingerprint ||
		bundle.Manifest.Fingerprint != again.Manifest.Fingerprint {
		t.Fatalf("browser composition identity is not deterministic")
	}

	replaced := bundle.Manifest.Clone()
	for index := range replaced.Implementations {
		if replaced.Implementations[index].Entry == "reducer" {
			replacement := sha256.Sum256([]byte("replacement reducer implementation"))
			replaced.Implementations[index].Artifact.Digest = "sha256:" + hex.EncodeToString(replacement[:])
		}
	}
	replaced, err = presentation.FreezeManifest(replaced)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Fingerprint == bundle.Manifest.Fingerprint {
		t.Fatal("replacement reducer implementation did not change manifest identity")
	}
}
