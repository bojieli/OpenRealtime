package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
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

func TestComposeTextBundlePinsCallerModuleAndItsExactDependencies(t *testing.T) {
	source := []byte(`export default {name:"example.client.challenge",revision:1,async mount(){}};`)
	alternative := []byte(`export default {name:"example.client.challenge",revision:1,async mount(){return "v2";}};`)
	stateSchema := plugin.Contract{
		Name: "example.client.challenge.state", Revision: 1,
		Digest: "sha256:" + strings.Repeat("9", 64),
	}
	bundle, err := ComposeTextBundle("openrealtime.browser.composed-test", []ClientModule{{
		Entry: "challenge", Entrypoint: "challenge.js", PluginName: "example.client.challenge",
		Source:       source,
		Alternatives: []ClientModuleAlternative{{Entrypoint: "challenge-v2.js", Source: alternative}},
		Requires: []plugin.Requirement{
			{Contract: presentation.ClientStateContract},
			{Contract: presentation.ClientSessionConfigurationContract},
		},
		StateSchema: &stateSchema, Lifecycle: plugin.Lifecycle{Snapshot: true, Restore: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for name, validate := range map[string]func() error{
		"profile": bundle.Profile.Validate, "lock": bundle.Lock.Validate,
		"plan": bundle.Plan.Validate, "manifest": bundle.Manifest.Validate,
	} {
		if err := validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var order []string
	for _, entry := range bundle.Plan.Entries {
		order = append(order, entry.Entry.ID)
	}
	if !reflect.DeepEqual(order, []string{
		"slots", "transport", "reducer", "session-configuration", "view", "challenge",
	}) {
		t.Fatalf("composed text client mount order = %v", order)
	}
	implementationDigest := manifestImplementationDigest(bundle.Manifest, "challenge")
	wantDigestBytes := sha256.Sum256(source)
	wantDigest := "sha256:" + hex.EncodeToString(wantDigestBytes[:])
	if implementationDigest != wantDigest {
		t.Fatalf("composed module digest = %q, want %q", implementationDigest, wantDigest)
	}
	wantAlternativeBytes := sha256.Sum256(alternative)
	wantAlternative := "sha256:" + hex.EncodeToString(wantAlternativeBytes[:])
	foundAlternative := false
	for _, asset := range bundle.Manifest.Assets {
		if asset.Entry == "challenge" && asset.Name == "challenge-v2.js" {
			foundAlternative = asset.Digest == wantAlternative
		}
	}
	if !foundAlternative {
		t.Fatalf("composed module omitted exact alternative digest %q", wantAlternative)
	}
	foundStateContract := false
	for _, entry := range bundle.Plan.Entries {
		if entry.Entry.ID == "challenge" {
			foundStateContract = entry.Descriptor.StateSchema != nil &&
				*entry.Descriptor.StateSchema == stateSchema && entry.Descriptor.Lifecycle.Snapshot &&
				entry.Descriptor.Lifecycle.Restore
		}
	}
	if !foundStateContract {
		t.Fatalf("composed module omitted exact state lifecycle contract")
	}
	source[0] = 'X'
	alternative[0] = 'X'
	stateSchema.Name = "example.client.mutated.state"
	if err := bundle.Manifest.Validate(); err != nil {
		t.Fatalf("caller source mutation invalidated composed bundle: %v", err)
	}
	if got := manifestImplementationDigest(bundle.Manifest, "challenge"); got != wantDigest {
		t.Fatalf("caller source mutation changed composed digest to %q, want %q", got, wantDigest)
	}
	for _, entry := range bundle.Plan.Entries {
		if entry.Entry.ID == "challenge" && entry.Descriptor.StateSchema.Name != "example.client.challenge.state" {
			t.Fatalf("caller state-schema mutation changed composed descriptor: %#v", entry.Descriptor.StateSchema)
		}
	}
	if _, err := ComposeTextBundle("openrealtime.browser.empty-module", []ClientModule{{
		Entry: "empty", Entrypoint: "empty.js", PluginName: "example.client.empty",
	}}); err == nil || !strings.Contains(err.Error(), "empty source") {
		t.Fatalf("empty caller module error = %v", err)
	}
	if _, err := ComposeTextBundle("openrealtime.browser.empty-alternative", []ClientModule{{
		Entry: "empty", Entrypoint: "empty.js", PluginName: "example.client.empty",
		Source:       []byte(`export default {name:"example.client.empty",revision:1,async mount(){}};`),
		Alternatives: []ClientModuleAlternative{{Entrypoint: "empty-v2.js"}},
	}}); err == nil || !strings.Contains(err.Error(), "alternative") ||
		!strings.Contains(err.Error(), "empty source") {
		t.Fatalf("empty caller alternative error = %v", err)
	}
}

func TestComposeDeveloperBundlePinsShippedEntryAlternativesWithoutWideningAuthority(t *testing.T) {
	effectsSource, err := browserModule("effects-client.js")
	if err != nil {
		t.Fatal(err)
	}
	artifactsSource, err := browserModule("artifact-references.js")
	if err != nil {
		t.Fatal(err)
	}
	effectsAlternative := append(slices.Clone(effectsSource), []byte("\n// effects replacement fixture\n")...)
	artifactsAlternative := append(
		slices.Clone(artifactsSource), []byte("\n// artifact replacement fixture\n")...,
	)
	catalogDigest := "sha256:" + strings.Repeat("c", 64)
	alternatives := []DeveloperImplementationAlternative{
		{Entry: "effects", Entrypoint: "effects-client-v2.js", Source: effectsAlternative},
		{
			Entry: "artifact-references", Entrypoint: "artifact-references-v2.js",
			Source: artifactsAlternative,
		},
	}
	bundle, err := ComposeDeveloperBundle(
		"openrealtime.browser.developer", catalogDigest, slices.Clone(alternatives),
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, validate := range map[string]func() error{
		"profile": bundle.Profile.Validate, "lock": bundle.Lock.Validate,
		"plan": bundle.Plan.Validate, "manifest": bundle.Manifest.Validate,
	} {
		if err := validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	reversed, err := ComposeDeveloperBundle(
		"openrealtime.browser.developer", catalogDigest,
		[]DeveloperImplementationAlternative{alternatives[1], alternatives[0]},
	)
	if err != nil {
		t.Fatal(err)
	}
	if reversed.Manifest.Fingerprint != bundle.Manifest.Fingerprint ||
		reversed.Plan.Fingerprint != bundle.Plan.Fingerprint {
		t.Fatal("developer alternative input order changed a frozen identity")
	}

	base, err := DeveloperBundleWithEffectsCatalog(catalogDigest)
	if err != nil {
		t.Fatal(err)
	}
	withoutAlternatives, err := ComposeDeveloperBundle(
		"openrealtime.browser.developer", catalogDigest, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if withoutAlternatives.Manifest.Fingerprint != base.Manifest.Fingerprint ||
		withoutAlternatives.Plan.Fingerprint != base.Plan.Fingerprint {
		t.Fatal("empty developer composition changed the shipped bundle identity")
	}
	if !reflect.DeepEqual(bundle.Manifest.Endpoints, base.Manifest.Endpoints) ||
		!reflect.DeepEqual(bundle.Manifest.Grants, base.Manifest.Grants) ||
		!reflect.DeepEqual(bundle.Manifest.Implementations, base.Manifest.Implementations) {
		t.Fatal("developer alternatives changed endpoints, grants, or selected implementations")
	}

	wantAssets := map[string]string{}
	for entry, source := range map[string][]byte{
		"effects": effectsAlternative, "artifact-references": artifactsAlternative,
	} {
		digestBytes := sha256.Sum256(source)
		wantAssets[entry] = "sha256:" + hex.EncodeToString(digestBytes[:])
	}
	for _, asset := range bundle.Manifest.Assets {
		switch asset.Name {
		case "effects-client-v2.js":
			if asset.Entry != "effects" || asset.Digest != wantAssets["effects"] {
				t.Fatalf("effects alternative asset = %#v", asset)
			}
			delete(wantAssets, "effects")
		case "artifact-references-v2.js":
			if asset.Entry != "artifact-references" ||
				asset.Digest != wantAssets["artifact-references"] {
				t.Fatalf("artifact alternative asset = %#v", asset)
			}
			delete(wantAssets, "artifact-references")
		}
	}
	if len(wantAssets) != 0 {
		t.Fatalf("developer manifest omitted alternative assets: %v", wantAssets)
	}
	effectsAlternative[0] = 'X'
	artifactsAlternative[0] = 'X'
	if err := bundle.Manifest.Validate(); err != nil {
		t.Fatalf("caller source mutation invalidated developer bundle: %v", err)
	}

	tests := []struct {
		name         string
		alternatives []DeveloperImplementationAlternative
		want         string
	}{
		{
			name: "unknown entry",
			alternatives: []DeveloperImplementationAlternative{{
				Entry: "unknown", Entrypoint: "unknown-v2.js", Source: []byte("export {}"),
			}},
			want: "unknown entry",
		},
		{
			name: "empty source",
			alternatives: []DeveloperImplementationAlternative{{
				Entry: "effects", Entrypoint: "effects-v2.js",
			}},
			want: "empty source",
		},
		{
			name: "invalid entrypoint",
			alternatives: []DeveloperImplementationAlternative{{
				Entry: "effects", Entrypoint: "../effects-v2.js", Source: []byte("export {}"),
			}},
			want: "invalid alternative entrypoint",
		},
		{
			name: "primary collision",
			alternatives: []DeveloperImplementationAlternative{{
				Entry: "effects", Entrypoint: "artifact-references.js", Source: []byte("export {}"),
			}},
			want: "duplicates entry",
		},
		{
			name: "alternative collision",
			alternatives: []DeveloperImplementationAlternative{
				{Entry: "effects", Entrypoint: "shared-v2.js", Source: []byte("export const a=1")},
				{
					Entry: "artifact-references", Entrypoint: "shared-v2.js",
					Source: []byte("export const b=2"),
				},
			},
			want: "duplicates entry",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ComposeDeveloperBundle(
				"openrealtime.browser.developer", catalogDigest, test.alternatives,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("developer alternative error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestComposeDeveloperWebRTCBundlePinsViewAlternativesWithoutWideningAuthority(t *testing.T) {
	video, err := browserModule("video-controls.js")
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := browserModule("transport-diagnostics-view.js")
	if err != nil {
		t.Fatal(err)
	}
	alternatives := []DeveloperImplementationAlternative{
		{Entry: "video-controls", Entrypoint: "video-controls-v2.js",
			Source: append(slices.Clone(video), []byte("\n// candidate\n")...)},
		{Entry: "transport-diagnostics", Entrypoint: "transport-diagnostics-view-v2.js",
			Source: append(slices.Clone(diagnostics), []byte("\n// candidate\n")...)},
	}
	catalogDigest := "sha256:" + strings.Repeat("d", 64)
	bundle, err := ComposeDeveloperWebRTCBundle(
		"openrealtime.browser.developer-webrtc", catalogDigest, alternatives,
	)
	if err != nil {
		t.Fatal(err)
	}
	base, err := DeveloperWebRTCBundleWithEffectsCatalog(catalogDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bundle.Manifest.Endpoints, base.Manifest.Endpoints) ||
		!reflect.DeepEqual(bundle.Manifest.Grants, base.Manifest.Grants) ||
		!reflect.DeepEqual(bundle.Manifest.Implementations, base.Manifest.Implementations) {
		t.Fatal("WebRTC view alternatives changed endpoints, grants, or selected implementations")
	}
	want := map[string]string{
		"video-controls-v2.js":             "video-controls",
		"transport-diagnostics-view-v2.js": "transport-diagnostics",
	}
	for _, asset := range bundle.Manifest.Assets {
		if entry, found := want[asset.Name]; found {
			if asset.Entry != entry {
				t.Fatalf("WebRTC view alternative asset = %#v", asset)
			}
			delete(want, asset.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("WebRTC manifest omitted view alternatives: %v", want)
	}
	reversed, err := ComposeDeveloperWebRTCBundle(
		"openrealtime.browser.developer-webrtc", catalogDigest,
		[]DeveloperImplementationAlternative{alternatives[1], alternatives[0]},
	)
	if err != nil {
		t.Fatal(err)
	}
	if reversed.Manifest.Fingerprint != bundle.Manifest.Fingerprint ||
		reversed.Plan.Fingerprint != bundle.Plan.Fingerprint {
		t.Fatal("WebRTC alternative input order changed a frozen identity")
	}
}

func manifestImplementationDigest(manifest presentation.ClientManifest, entry string) string {
	for _, implementation := range manifest.Implementations {
		if implementation.Entry == entry {
			return implementation.Artifact.Digest
		}
	}
	return ""
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
		"management-source-reading.js", "management-source-publication.js",
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

func TestInspectionViewJoinsExactStaticAndLiveEvidenceAsTextInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	content, err := browserModule("inspection-view.js")
	if err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(t.TempDir(), "inspection-view.mjs")
	if err := os.WriteFile(module, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runner, err := filepath.Abs(filepath.Join("testdata", "inspection_view.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, runner, module).CombinedOutput(); err != nil {
		t.Fatalf("inspection view join conformance: %v\n%s", err, output)
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
		"management-authoring.js", "management-source-reading.js", "management-source-publication.js",
		"authoring-workspace.js", "reducer.js",
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
	goRequest := management.SourceWriteRequest{
		FormatVersion:        management.SourceWriteFormatVersion,
		RootIdentity:         "sha256:" + strings.Repeat("6", 64),
		Mode:                 management.SourceUpdate,
		Path:                 "unicode/agent-β.ortg",
		Source:               "graph browser_go_β {\n}\n",
		ExpectedSourceDigest: "sha256:" + strings.Repeat("5", 64),
	}
	goReceipt, err := management.NewSourceWriteReceipt(goRequest, true)
	if err != nil {
		t.Fatal(err)
	}
	goReadRequest := management.SourceReadRequest{
		FormatVersion: management.SourceReadFormatVersion,
		RootIdentity:  goRequest.RootIdentity,
		Path:          goRequest.Path,
	}
	goReadResult, err := management.NewSourceReadResult(goReadRequest, goRequest.Source)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := json.Marshal(struct {
		Request      management.SourceWriteRequest `json:"request"`
		Receipt      management.SourceWriteReceipt `json:"receipt"`
		Evidence     string                        `json:"evidence"`
		ReadRequest  management.SourceReadRequest  `json:"read_request"`
		ReadResult   management.SourceReadResult   `json:"read_result"`
		ReadEvidence string                        `json:"read_evidence"`
	}{
		Request: goRequest, Receipt: goReceipt, Evidence: "authoring:write:" + goReceipt.ReceiptDigest,
		ReadRequest: goReadRequest, ReadResult: goReadResult,
		ReadEvidence: "authoring:read:" + goReadResult.ResultDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments := append(append([]string{runner}, paths...), string(fixture))
	if output, err := exec.Command(node, arguments...).CombinedOutput(); err != nil {
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
		"management-source-reading", "management-source-publication", "authoring-workspace",
		"management-operator-view", "authoring-editor-view",
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
		!reflect.DeepEqual(grants["management-transport"],
			[]string{"authoring", "publication", "reconciliation", "source-read", "static"}) {
		t.Fatalf("WebRTC developer client grants = %#v", grants)
	}
	var endpoints []string
	for _, endpoint := range bundle.Manifest.Endpoints {
		endpoints = append(endpoints, endpoint.Name+" "+endpoint.Method+" "+endpoint.Path)
	}
	if !reflect.DeepEqual(endpoints, []string{
		"effects.local GET /client/v1/effects",
		"management.authoring POST /client/v1/management/authoring",
		"management.reconciliation POST /client/v1/management/reconciliations",
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
	var workspaceDescriptor *plugin.Descriptor
	for _, entry := range bundle.Plan.Entries {
		order = append(order, entry.Entry.ID)
		if entry.Entry.ID == "authoring-workspace" {
			descriptor := entry.Descriptor
			workspaceDescriptor = &descriptor
		}
	}
	want := []string{
		"slots", "transport", "reducer", "session-configuration", "debug-session", "effects",
		"artifact-references", "inspection", "view", "confirmation-view", "artifact-view",
		"inspection-view", "trace-view",
		"management-operator", "management-transport", "management-static", "management-authoring",
		"management-source-reading", "management-source-publication", "authoring-workspace",
		"management-operator-view", "authoring-editor-view",
		"authoring-configuration-view", "authoring-canvas-view",
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("developer client mount order = %v, want %v", order, want)
	}
	if workspaceDescriptor == nil || workspaceDescriptor.StateSchema == nil ||
		*workspaceDescriptor.StateSchema != presentation.ClientAuthoringWorkspaceStateContract ||
		!workspaceDescriptor.Lifecycle.Snapshot || !workspaceDescriptor.Lifecycle.Restore {
		t.Fatalf("authoring workspace state lifecycle = %#v", workspaceDescriptor)
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
	for _, forbidden := range []string{
		"effects", "artifact-references", "confirmation-view", "artifact-view",
		"management-source-reading", "management-source-publication",
	} {
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
	for _, grant := range bundle.Manifest.Grants {
		if grant.Entry != "management-transport" {
			continue
		}
		for _, permission := range grant.Permissions {
			if slices.Contains(permission.Operations, "source-read") ||
				slices.Contains(permission.Operations, "publication") {
				t.Fatalf("observer developer bundle granted rooted source access: %#v", grant)
			}
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
	for _, forbidden := range []string{
		"effects", "artifact-references", "confirmation-view", "artifact-view",
		"management-source-reading", "management-source-publication",
	} {
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
	for _, grant := range bundle.Manifest.Grants {
		if grant.Entry != "management-transport" {
			continue
		}
		for _, permission := range grant.Permissions {
			if slices.Contains(permission.Operations, "source-read") ||
				slices.Contains(permission.Operations, "publication") {
				t.Fatalf("observer WebRTC bundle granted rooted source access: %#v", grant)
			}
		}
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
			replaced.Implementations[index].Implementation = "browser-esm:reducer-v2.js"
			replaced.Implementations[index].Artifact.ID = "module://reducer-v2"
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
