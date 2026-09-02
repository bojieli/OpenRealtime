package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestSessionProviderReplacementPreservesExternalRegistryState(t *testing.T) {
	graph := testGraph(t)
	registryState := management.NewSessionRegistry()
	predecessor := testRegisteredRuntime{live: testLive(graph)}
	disposePredecessor, err := registryState.Register("sess-test", predecessor)
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := NewBundle(BundleConfig{
		Authorizer: testAuthorizer{graph: graph.Fingerprint},
		Sessions:   registryState,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeRegistry := pluginruntime.NewRegistry()
	originalArtifact := managementRouteArtifact(
		"go://management/session-provider-state-v1", "build-1", "8",
	)
	for _, factory := range bundle.Factories {
		if err := runtimeRegistry.RegisterArtifact(
			factory.Descriptor().Name, originalArtifact, factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	candidateFactory, err := NewSessionInspectionProvider(registryState)
	if err != nil {
		t.Fatal(err)
	}
	const candidateImplementation = "test/management-session-registry-binding-v2"
	candidateArtifact := managementRouteArtifact(
		"go://management/session-provider-state-v2", "build-2", "9",
	)
	if err := runtimeRegistry.RegisterArtifact(
		candidateImplementation, candidateArtifact, candidateFactory,
	); err != nil {
		t.Fatal(err)
	}

	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: bundle.Plan, Registry: runtimeRegistry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handlerValue, handlerContract, handlerProvider, handlerRevision, err := mounted.Export("http")
	if err != nil || handlerContract != management.HTTPHandlerContract || handlerProvider != "router" {
		t.Fatalf("operator HTTP export = %T %+v %q %d, %v",
			handlerValue, handlerContract, handlerProvider, handlerRevision, err)
	}
	handler, ok := handlerValue.(http.Handler)
	if !ok {
		t.Fatalf("operator HTTP export value = %T", handlerValue)
	}
	assertRegisteredSessionSequence(t, handler, 1)

	before := mounted.Live()
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: bundle.Plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "session-source", SetImplementation: true, Implementation: candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != bundle.Plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 2 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("session-provider state-continuity receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "session-source" || transition.BeforeRuntime != originalArtifact ||
		transition.AfterRuntime != candidateArtifact ||
		transition.AfterImplementation != candidateImplementation {
		t.Fatalf("session-provider transition = %#v", transition)
	}
	wantRetired := map[string]bool{"session-source": true, "session-api": true}
	for _, retirement := range receipt.Retirements {
		if !wantRetired[retirement.Entry] || retirement.RetiredScopes == 0 ||
			retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
			retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
			retirement.RemainingServices != 0 {
			t.Fatalf("session-provider retirement = %#v", retirement)
		}
		delete(wantRetired, retirement.Entry)
	}
	if len(wantRetired) != 0 {
		t.Fatalf("session-provider retirement omitted %v", wantRetired)
	}
	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["session-source"].Implementation != candidateImplementation ||
		after.Entries["session-source"].Runtime != candidateArtifact ||
		after.Entries["session-api"].Runtime != originalArtifact ||
		after.Entries["router"].Runtime != originalArtifact {
		t.Fatalf("replacement session-provider live evidence = %+v", after)
	}
	afterHandlerValue, afterHandlerContract, afterHandlerProvider, afterHandlerRevision, err :=
		mounted.Export("http")
	if err != nil || afterHandlerValue != handlerValue || afterHandlerContract != handlerContract ||
		afterHandlerProvider != handlerProvider || afterHandlerRevision != handlerRevision {
		t.Fatalf("stable operator export changed across session-provider replacement: %T/%+v/%s/%d, %v",
			afterHandlerValue, afterHandlerContract, afterHandlerProvider, afterHandlerRevision, err)
	}
	assertRegisteredSessionSequence(t, handler, 1)

	disposePredecessor()
	if response := request(t, handler, http.MethodGet,
		management.APIPrefix+"/sessions/sess-test/live", "session-token", nil,
	); response.Code != http.StatusNotFound {
		t.Fatalf("disposed external session remained visible = %d: %s",
			response.Code, response.Body.String())
	}
	replacementLive := testLive(graph)
	replacementLive.Sequence = 2
	disposeReplacement, err := registryState.Register(
		"sess-test", testRegisteredRuntime{live: replacementLive},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer disposeReplacement()
	assertRegisteredSessionSequence(t, handler, 2)

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, entry := range mounted.Live().Entries {
		if entry.State != "closed" || entry.Workers != 0 || entry.Effects != 0 ||
			len(entry.Services) != 0 || entry.Error != "" {
			t.Fatalf("closed operator entry %s retained ownership = %+v", id, entry)
		}
	}
	if snapshot, err := registryState.Snapshot(context.Background(), "sess-test"); err != nil ||
		snapshot.Sequence != 2 {
		t.Fatalf("realm close took ownership of the external session registry = %+v, %v", snapshot, err)
	}
}

func assertRegisteredSessionSequence(t *testing.T, handler http.Handler, want uint64) {
	t.Helper()
	response := request(t, handler, http.MethodGet,
		management.APIPrefix+"/sessions/sess-test/live", "session-token", nil,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("registered session response = %d: %s", response.Code, response.Body.String())
	}
	var live inspect.Live
	if err := json.Unmarshal(response.Body.Bytes(), &live); err != nil {
		t.Fatal(err)
	}
	if live.Sequence != want {
		t.Fatalf("registered session sequence = %d, want %d", live.Sequence, want)
	}
}

type testRegisteredRuntime struct{ live inspect.Live }

func (runtime testRegisteredRuntime) Live() inspect.Live { return runtime.live.Clone() }

func (testRegisteredRuntime) RecordedTrace() (inspect.LiveTrace, error) {
	return inspect.LiveTrace{}, management.ErrUnavailable
}

var _ management.RuntimeSession = testRegisteredRuntime{}
