package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestDenyEffectAuthorityReplacementReconcilesEffectsClosure(t *testing.T) {
	router := NewRouterFactory()
	artifacts := NewArtifactStoreFactory()
	downloads := NewDownloadStoreFactory()
	authorityV1 := NewDenyEffectAuthorityFactory()
	authorityV2 := NewDenyEffectAuthorityFactory()
	effects, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}

	factories := []pluginruntime.Factory{router, artifacts, downloads, authorityV1, effects}
	catalog := plugin.NewCatalog()
	for _, factory := range factories {
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "presentation.effect-authority-replacement.test",
		Revision:      1,
		Realm:         plugin.PresentationHostRealm,
		Scopes:        []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{
			{ID: "router", Plugin: router.Descriptor().Name, Scope: "root"},
			{ID: "artifacts", Plugin: artifacts.Descriptor().Name, Scope: "root"},
			{ID: "downloads", Plugin: downloads.Descriptor().Name, Scope: "root"},
			{ID: "authority", Plugin: authorityV1.Descriptor().Name, Scope: "root"},
			{ID: "effects", Plugin: effects.Descriptor().Name, Scope: "root"},
		},
		Exports: []plugin.ProfileExport{
			{Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name},
			{Name: "effects", Provider: "effects", Service: presentation.EffectsContract.Name},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}

	originalArtifacts := map[string]inspect.ArtifactIdentity{
		"router":    hostTestArtifact("go://host-router", "build-1", "1"),
		"artifacts": hostTestArtifact("go://host-artifact-store", "build-1", "2"),
		"downloads": hostTestArtifact("go://host-download-store", "build-1", "3"),
		"authority": hostTestArtifact("go://host-deny-effect-authority-v1", "build-1", "4"),
		"effects":   hostTestArtifact("go://host-effects", "build-1", "5"),
	}
	registry := pluginruntime.NewRegistry()
	for _, row := range []struct {
		entry   string
		factory pluginruntime.Factory
	}{
		{"router", router}, {"artifacts", artifacts}, {"downloads", downloads},
		{"authority", authorityV1}, {"effects", effects},
	} {
		if err := registry.RegisterArtifact(
			row.factory.Descriptor().Name, originalArtifacts[row.entry], row.factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	const candidateImplementation = "openrealtime.presentation.host.deny-effect-authority-v2"
	candidateArtifact := hostTestArtifact("go://host-deny-effect-authority-v2", "build-2", "6")
	if err := registry.RegisterArtifact(candidateImplementation, candidateArtifact, authorityV2); err != nil {
		t.Fatal(err)
	}

	permissions := map[string][]plugin.Permission{
		"artifacts": artifacts.Descriptor().Permissions,
		"downloads": downloads.Descriptor().Permissions,
		"effects":   effects.Descriptor().Permissions,
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{
			"artifacts": json.RawMessage(`{}`),
			"downloads": json.RawMessage(`{}`),
			"effects":   json.RawMessage(`{}`),
		},
		Permissions: permissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })

	httpValue, httpContract, httpProvider, httpRevision, err := mounted.Export("http")
	if err != nil || httpContract != presentation.HTTPHandlerContract || httpProvider != "router" {
		t.Fatalf("host HTTP export = %T %+v %q %d, %v",
			httpValue, httpContract, httpProvider, httpRevision, err)
	}
	handler, ok := httpValue.(http.Handler)
	if !ok {
		t.Fatalf("host HTTP export value = %T", httpValue)
	}
	effectsValue, effectsContract, effectsProvider, effectsRevision, err := mounted.Export("effects")
	if err != nil || effectsContract != presentation.EffectsContract || effectsProvider != "effects" {
		t.Fatalf("host effects export = %T %+v %q %d, %v",
			effectsValue, effectsContract, effectsProvider, effectsRevision, err)
	}
	predecessorEffects, ok := effectsValue.(Effects)
	if !ok {
		t.Fatalf("host effects export value = %T", effectsValue)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	predecessorSocket := dialEffectTestSocket(t, server)
	if stats := predecessorEffects.Stats(); stats.ActiveSessions != 1 || stats.Closed {
		t.Fatalf("predecessor effect-session evidence = %#v", stats)
	}

	before := mounted.Live()
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "authority", SetImplementation: true, Implementation: candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := predecessorSocket.connection.Read(predecessorSocket.ctx); err == nil {
		t.Fatal("effect-authority replacement left the predecessor socket active")
	}
	if stats := predecessorEffects.Stats(); !stats.Closed || stats.ActiveSessions != 0 {
		t.Fatalf("retired effects provider evidence = %#v", stats)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 2 || len(receipt.StateTransfers) != 1 {
		t.Fatalf("effect-authority replacement receipt = %#v", receipt)
	}
	transfer := receipt.StateTransfers[0]
	if transfer.Entry != "effects" || transfer.Schema != presentation.EffectsStateContract ||
		transfer.BeforeStateDigest == "" || transfer.BeforeStateDigest != transfer.AfterStateDigest ||
		transfer.MigratorImplementation != "" {
		t.Fatalf("unchanged effects state transfer = %#v", transfer)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "authority" ||
		transition.BeforeImplementation != authorityV1.Descriptor().Name ||
		transition.AfterImplementation != candidateImplementation ||
		transition.BeforeRuntime != originalArtifacts["authority"] ||
		transition.AfterRuntime != candidateArtifact {
		t.Fatalf("effect-authority transition = %#v", transition)
	}
	retired := make(map[string]bool, len(receipt.Retirements))
	for _, retirement := range receipt.Retirements {
		retired[retirement.Entry] = true
		if retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("effect-authority retirement retained ownership = %#v", retirement)
		}
	}
	if !retired["authority"] || !retired["effects"] {
		t.Fatalf("effect-authority replacement closure = %#v", receipt.Retirements)
	}

	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["authority"].Implementation != candidateImplementation ||
		after.Entries["authority"].Runtime != candidateArtifact ||
		after.Entries["effects"].Runtime != originalArtifacts["effects"] ||
		after.Entries["router"].Runtime != originalArtifacts["router"] ||
		after.Entries["artifacts"].Runtime != originalArtifacts["artifacts"] ||
		after.Entries["downloads"].Runtime != originalArtifacts["downloads"] {
		t.Fatalf("replacement effect-authority live evidence = %+v", after)
	}
	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err := mounted.Export("http")
	if err != nil || afterHTTPValue != httpValue || afterHTTPContract != httpContract ||
		afterHTTPProvider != httpProvider || afterHTTPRevision != httpRevision {
		t.Fatalf("stable host export changed across effect-authority replacement: %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}
	afterEffectsValue, afterEffectsContract, afterEffectsProvider, afterEffectsRevision, err :=
		mounted.Export("effects")
	if err != nil || afterEffectsContract != effectsContract || afterEffectsProvider != effectsProvider ||
		afterEffectsRevision <= effectsRevision || afterEffectsValue == effectsValue {
		t.Fatalf("replacement effects export = %T/%+v/%s/%d, %v",
			afterEffectsValue, afterEffectsContract, afterEffectsProvider, afterEffectsRevision, err)
	}
	replacementEffects, ok := afterEffectsValue.(Effects)
	if !ok {
		t.Fatalf("replacement effects export value = %T", afterEffectsValue)
	}

	replacementSocket := dialEffectTestSocket(t, server)
	replacementSocket.send(effectFixtureCall(
		"sess_replacement", "call_denied", "display_artifact",
		map[string]any{"artifact_id": "denied", "title": "Denied", "html": "<p>denied</p>"},
		"unsigned",
	))
	denied := replacementSocket.receive()
	if denied.Type != "result" || denied.Error == nil || denied.Error.Code != "authority_denied" ||
		denied.Artifact != nil || replacementEffects.Stats().Executed != 0 {
		t.Fatalf("replacement deny-authority result = %#v stats=%#v", denied, replacementEffects.Stats())
	}

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, mounted.Live(), plan.Fingerprint)
}
