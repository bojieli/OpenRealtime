package host

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestImmutableBrowserHostingReplacementIsAtomic(t *testing.T) {
	source := ModuleSource{
		Name: "bootstrap.js", MediaType: "text/javascript",
		Content: []byte("export const generation = 'stable';\n"),
	}
	moduleV1, err := NewModuleStoreFactory(1, []ModuleSource{source})
	if err != nil {
		t.Fatal(err)
	}
	moduleV2, err := NewModuleStoreFactory(1, []ModuleSource{source})
	if err != nil {
		t.Fatal(err)
	}
	asset := moduleV1.Descriptor().Assets[0]
	manifest := hostingTestManifest(t, asset)
	manifestV1, err := NewManifestFactory(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestV2, err := NewManifestFactory(manifest)
	if err != nil {
		t.Fatal(err)
	}
	shellV1, err := NewBrowserShellFactory(1, asset.Digest)
	if err != nil {
		t.Fatal(err)
	}
	shellV2, err := NewBrowserShellFactory(1, asset.Digest)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouterFactory()
	factories := []pluginruntime.Factory{shellV1, manifestV1, moduleV1, router}
	plan := makeHostPlan(t, factories)
	registry := pluginruntime.NewRegistry()
	originalArtifacts := map[string]inspect.ArtifactIdentity{
		"modules":  hostTestArtifact("go://host-module-store-v1", "build-1", "1"),
		"manifest": hostTestArtifact("go://host-manifest-v1", "build-1", "2"),
		"shell":    hostTestArtifact("go://host-browser-shell-v1", "build-1", "3"),
		"router":   hostTestArtifact("go://host-router", "build-1", "4"),
	}
	for _, row := range []struct {
		entry   string
		factory pluginruntime.Factory
	}{
		{"modules", moduleV1}, {"manifest", manifestV1},
		{"shell", shellV1}, {"router", router},
	} {
		if err := registry.RegisterArtifact(
			row.factory.Descriptor().Name, originalArtifacts[row.entry], row.factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	candidateImplementations := map[string]string{
		"modules":  "openrealtime.presentation.host.module-store-v2",
		"manifest": "openrealtime.presentation.host.client-manifest-v2",
		"shell":    "openrealtime.presentation.host.browser-shell-v2",
	}
	candidateArtifacts := map[string]inspect.ArtifactIdentity{
		"modules":  hostTestArtifact("go://host-module-store-v2", "build-2", "5"),
		"manifest": hostTestArtifact("go://host-manifest-v2", "build-2", "6"),
		"shell":    hostTestArtifact("go://host-browser-shell-v2", "build-2", "7"),
	}
	for _, row := range []struct {
		entry   string
		factory pluginruntime.Factory
	}{
		{"modules", moduleV2}, {"manifest", manifestV2}, {"shell", shellV2},
	} {
		if err := registry.RegisterArtifact(
			candidateImplementations[row.entry], candidateArtifacts[row.entry], row.factory,
		); err != nil {
			t.Fatal(err)
		}
	}

	missingModule, err := NewModuleStoreFactory(1, []ModuleSource{{
		Name: "missing.js", MediaType: "text/javascript", Content: []byte("export {};\n"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	missingManifest, err := NewManifestFactory(
		hostingTestManifest(t, missingModule.Descriptor().Assets[0]),
	)
	if err != nil {
		t.Fatal(err)
	}
	const missingImplementation = "openrealtime.presentation.host.client-manifest-missing-asset"
	if err := registry.RegisterArtifact(
		missingImplementation,
		hostTestArtifact("go://host-manifest-missing-asset", "build-2", "8"),
		missingManifest,
	); err != nil {
		t.Fatal(err)
	}

	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	value, contract, provider, revision, err := mounted.Export("http")
	if err != nil || contract != presentation.HTTPHandlerContract || provider != "router" {
		t.Fatalf("host HTTP export = %T %+v %q %d, %v", value, contract, provider, revision, err)
	}
	handler, ok := value.(http.Handler)
	if !ok {
		t.Fatalf("host HTTP export value = %T", value)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	modulePath := manifest.Assets[0].Path
	beforeBodies := map[string]string{
		"/":                   hostingResponseBody(t, server.URL+"/"),
		"/client/v1/manifest": hostingResponseBody(t, server.URL+"/client/v1/manifest"),
		modulePath:            hostingResponseBody(t, server.URL+modulePath),
	}
	if !strings.Contains(beforeBodies["/"], modulePath) ||
		beforeBodies[modulePath] != string(source.Content) {
		t.Fatalf("initial immutable hosting surface = %#v", beforeBodies)
	}

	before := mounted.Live()
	if receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "manifest", SetImplementation: true, Implementation: missingImplementation,
		}},
	}); err == nil || !strings.Contains(err.Error(), "asset absent") || receipt.FormatVersion != 0 {
		t.Fatalf("missing-asset manifest candidate receipt/error = %#v, %v", receipt, err)
	}
	afterRefusal := mounted.Live()
	if afterRefusal.Sequence != before.Sequence ||
		afterRefusal.Entries["manifest"].Implementation != manifestV1.Descriptor().Name ||
		afterRefusal.Entries["manifest"].Runtime != originalArtifacts["manifest"] {
		t.Fatalf("refused hosting candidate disturbed predecessor = %+v", afterRefusal)
	}
	for path, want := range beforeBodies {
		if got := hostingResponseBody(t, server.URL+path); got != want {
			t.Fatalf("hosting body after refused candidate at %s = %q, want %q", path, got, want)
		}
	}

	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: afterRefusal.Sequence,
		Updates: []pluginruntime.EntryUpdate{
			{Entry: "modules", SetImplementation: true, Implementation: candidateImplementations["modules"]},
			{Entry: "manifest", SetImplementation: true, Implementation: candidateImplementations["manifest"]},
			{Entry: "shell", SetImplementation: true, Implementation: candidateImplementations["shell"]},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 3 ||
		len(receipt.Retirements) != 3 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("immutable hosting replacement receipt = %#v", receipt)
	}
	for _, transition := range receipt.Transitions {
		if transition.BeforeRuntime != originalArtifacts[transition.Entry] ||
			transition.AfterRuntime != candidateArtifacts[transition.Entry] ||
			transition.AfterImplementation != candidateImplementations[transition.Entry] {
			t.Fatalf("immutable hosting transition = %#v", transition)
		}
	}
	for _, retirement := range receipt.Retirements {
		if _, found := candidateImplementations[retirement.Entry]; !found ||
			retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("immutable hosting retirement retained ownership = %#v", retirement)
		}
	}
	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["router"].Runtime != originalArtifacts["router"] {
		t.Fatalf("immutable hosting live evidence = %+v", after)
	}
	for entry, implementation := range candidateImplementations {
		live := after.Entries[entry]
		if live.Implementation != implementation || live.Runtime != candidateArtifacts[entry] ||
			live.Workers != 0 || live.Effects == 0 {
			t.Fatalf("replacement hosting entry %s = %+v", entry, live)
		}
	}
	afterValue, afterContract, afterProvider, afterRevision, err := mounted.Export("http")
	if err != nil || afterValue != value || afterContract != contract || afterProvider != provider ||
		afterRevision != revision {
		t.Fatalf("stable host export changed across hosting replacement: %T/%+v/%s/%d, %v",
			afterValue, afterContract, afterProvider, afterRevision, err)
	}
	for path, want := range beforeBodies {
		if got := hostingResponseBody(t, server.URL+path); got != want {
			t.Fatalf("replacement hosting body at %s = %q, want %q", path, got, want)
		}
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, mounted.Live(), plan.Fingerprint)
}

func hostingTestManifest(t *testing.T, asset plugin.Asset) presentation.ClientManifest {
	t.Helper()
	manifest, err := presentation.FreezeManifest(presentation.ClientManifest{
		FormatVersion: presentation.ManifestFormatVersion,
		Platform:      "browser",
		Plan:          makeClientPlan(t, asset),
		Implementations: []presentation.ManifestImplementation{{
			Entry: "shell", Implementation: "browser-shell",
			Artifact: inspect.ArtifactIdentity{
				ID: "module://browser-shell", Digest: asset.Digest,
			},
			Entrypoint: asset.Name,
		}},
		Assets: []presentation.ManifestAsset{{
			Entry: "shell", Name: asset.Name, MediaType: asset.MediaType, Digest: asset.Digest,
			Path: "/client/v1/modules/" + strings.TrimPrefix(asset.Digest, "sha256:"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func hostingResponseBody(t *testing.T, target string) string {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%q, error=%v", target, response.StatusCode, payload, readErr)
	}
	return string(payload)
}
