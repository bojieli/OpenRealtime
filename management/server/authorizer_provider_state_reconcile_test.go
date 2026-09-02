package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestAuthorizerProviderReplacementPreservesExternalCapabilityState(t *testing.T) {
	graph := testGraph(t)
	authority := management.NewCapabilityRegistry()
	access, revoke, err := authority.IssueScoped(time.Minute, []management.Grant{{
		Operation: management.ReadGraph, Resource: graph.Fingerprint,
	}})
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := NewBundle(BundleConfig{
		Authorizer:    authority,
		StaticCatalog: testStaticCatalog{graph: graph},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeRegistry := pluginruntime.NewRegistry()
	originalArtifact := managementRouteArtifact(
		"go://management/authorizer-state-v1", "build-1", "a",
	)
	for _, factory := range bundle.Factories {
		if err := runtimeRegistry.RegisterArtifact(
			factory.Descriptor().Name, originalArtifact, factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	candidateFactory, err := NewAuthorizerProvider(authority)
	if err != nil {
		t.Fatal(err)
	}
	const candidateImplementation = "test/management-capability-authorizer-v2"
	candidateArtifact := managementRouteArtifact(
		"go://management/authorizer-state-v2", "build-2", "b",
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
	graphPath := management.APIPrefix + "/graphs/" + graph.Fingerprint
	assertCapabilityGraphStatus(t, handler, graphPath, access.Token, http.StatusOK)

	before := mounted.Live()
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: bundle.Plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "authorizer", SetImplementation: true, Implementation: candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != bundle.Plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 2 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("authorizer state-continuity receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "authorizer" || transition.BeforeRuntime != originalArtifact ||
		transition.AfterRuntime != candidateArtifact ||
		transition.AfterImplementation != candidateImplementation {
		t.Fatalf("authorizer transition = %#v", transition)
	}
	wantRetired := map[string]bool{"authorizer": true, "static-api": true}
	for _, retirement := range receipt.Retirements {
		if !wantRetired[retirement.Entry] || retirement.RetiredScopes == 0 ||
			retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
			retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
			retirement.RemainingServices != 0 {
			t.Fatalf("authorizer retirement = %#v", retirement)
		}
		delete(wantRetired, retirement.Entry)
	}
	if len(wantRetired) != 0 {
		t.Fatalf("authorizer retirement omitted %v", wantRetired)
	}
	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["authorizer"].Implementation != candidateImplementation ||
		after.Entries["authorizer"].Runtime != candidateArtifact ||
		after.Entries["static-api"].Runtime != originalArtifact ||
		after.Entries["router"].Runtime != originalArtifact {
		t.Fatalf("replacement authorizer live evidence = %+v", after)
	}
	afterHandlerValue, afterHandlerContract, afterHandlerProvider, afterHandlerRevision, err :=
		mounted.Export("http")
	if err != nil || afterHandlerValue != handlerValue || afterHandlerContract != handlerContract ||
		afterHandlerProvider != handlerProvider || afterHandlerRevision != handlerRevision {
		t.Fatalf("stable operator export changed across authorizer replacement: %T/%+v/%s/%d, %v",
			afterHandlerValue, afterHandlerContract, afterHandlerProvider, afterHandlerRevision, err)
	}
	assertCapabilityGraphStatus(t, handler, graphPath, access.Token, http.StatusOK)

	revoke()
	assertCapabilityGraphStatus(t, handler, graphPath, access.Token, http.StatusNotFound)
	replacementAccess, revokeReplacement, err := authority.IssueScoped(time.Minute, []management.Grant{{
		Operation: management.ReadGraph, Resource: graph.Fingerprint,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer revokeReplacement()
	assertCapabilityGraphStatus(t, handler, graphPath, replacementAccess.Token, http.StatusOK)

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, entry := range mounted.Live().Entries {
		if entry.State != "closed" || entry.Workers != 0 || entry.Effects != 0 ||
			len(entry.Services) != 0 || entry.Error != "" {
			t.Fatalf("closed operator entry %s retained ownership = %+v", id, entry)
		}
	}
	if err := authority.Authorize(context.Background(), management.AuthorizationRequest{
		Capability: replacementAccess.Token,
		Operation:  management.ReadGraph,
		Resource:   graph.Fingerprint,
	}); err != nil {
		t.Fatalf("realm close took ownership of the external capability registry: %v", err)
	}
}

func assertCapabilityGraphStatus(
	t *testing.T, handler http.Handler, path, capability string, want int,
) {
	t.Helper()
	response := request(t, handler, http.MethodGet, path, capability, nil)
	if response.Code != want {
		t.Fatalf("capability graph status = %d, want %d: %s",
			response.Code, want, response.Body.String())
	}
}
