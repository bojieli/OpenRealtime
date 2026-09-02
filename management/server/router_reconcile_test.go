package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestOperatorRouterReplacementRollsBackAndRemountsAllRoutes(t *testing.T) {
	graph := testGraph(t)
	sourceBoundary := routeTestSourceBoundary{}
	bundle, err := NewBundle(BundleConfig{
		Authorizer:    testAuthorizer{graph: graph.Fingerprint},
		StaticCatalog: testStaticCatalog{graph: graph},
		Sessions:      testSessions{live: testLive(graph)},
		Authoring:     &testAuthoring{},
		SourceReading: sourceBoundary, SourcePublication: sourceBoundary,
		Reconciliation: testReconciler{},
	})
	if err != nil {
		t.Fatal(err)
	}

	registry := pluginruntime.NewRegistry()
	originalArtifact := managementRouteArtifact("go://management/operator-bundle-v1", "build-1", "a")
	for _, factory := range bundle.Factories {
		if err := registry.RegisterArtifact(factory.Descriptor().Name, originalArtifact, factory); err != nil {
			t.Fatal(err)
		}
	}

	const failingImplementation = "test/management-router-failing"
	failing := &failingManagementRouterFactory{
		delegate: NewRouterFactory(), failure: errors.New("fixture management router activation failed"),
	}
	if err := registry.RegisterArtifact(
		failingImplementation,
		managementRouteArtifact("go://management/router-failing", "build-2", "b"),
		failing,
	); err != nil {
		t.Fatal(err)
	}
	const candidateImplementation = "test/management-router-v2"
	candidateArtifact := managementRouteArtifact("go://management/router-v2", "build-2", "c")
	if err := registry.RegisterArtifact(
		candidateImplementation, candidateArtifact, NewRouterFactory(),
	); err != nil {
		t.Fatal(err)
	}

	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: bundle.Plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })

	originalHTTPValue, httpContract, httpProvider, originalHTTPRevision, err := mounted.Export("http")
	if err != nil || httpContract != management.HTTPHandlerContract || httpProvider != "router" {
		t.Fatalf("operator HTTP export = %T %+v %q %d, %v",
			originalHTTPValue, httpContract, httpProvider, originalHTTPRevision, err)
	}
	originalHandler, ok := originalHTTPValue.(http.Handler)
	if !ok {
		t.Fatalf("operator HTTP export value = %T", originalHTTPValue)
	}
	assertOperatorRouteMethods(t, originalHandler, graph.Fingerprint, http.StatusMethodNotAllowed)

	before := mounted.Live()
	failedReceipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: bundle.Plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "router", SetImplementation: true, Implementation: failingImplementation,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), failing.failure.Error()) || failedReceipt.FormatVersion != 0 {
		t.Fatalf("failing operator router receipt/error = %#v, %v", failedReceipt, err)
	}
	afterRollback := mounted.Live()
	if afterRollback.Sequence <= before.Sequence ||
		afterRollback.Entries["router"].Implementation != NewRouterFactory().Descriptor().Name ||
		afterRollback.Entries["router"].Runtime != originalArtifact {
		t.Fatalf("operator router rollback did not restore the predecessor = %+v", afterRollback)
	}
	assertUnchangedOperatorEntries(t, afterRollback, originalArtifact)
	rollbackHTTPValue, rollbackHTTPContract, rollbackHTTPProvider, rollbackHTTPRevision, err := mounted.Export("http")
	if err != nil || rollbackHTTPContract != httpContract || rollbackHTTPProvider != httpProvider ||
		rollbackHTTPRevision <= originalHTTPRevision || rollbackHTTPValue == originalHTTPValue {
		t.Fatalf("rollback operator HTTP export = %T/%+v/%s/%d, %v",
			rollbackHTTPValue, rollbackHTTPContract, rollbackHTTPProvider, rollbackHTTPRevision, err)
	}
	rollbackHandler := rollbackHTTPValue.(http.Handler)
	assertOperatorRouteMethods(t, originalHandler, graph.Fingerprint, http.StatusNotFound)
	assertOperatorRouteMethods(t, rollbackHandler, graph.Fingerprint, http.StatusMethodNotAllowed)

	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: bundle.Plan.Fingerprint, ExpectedSequence: afterRollback.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "router", SetImplementation: true, Implementation: candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != bundle.Plan.Fingerprint || receipt.BeforeSequence != afterRollback.Sequence ||
		receipt.AfterSequence <= afterRollback.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 7 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("operator router reconciliation receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "router" || transition.BeforeRuntime != originalArtifact ||
		transition.AfterRuntime != candidateArtifact || transition.AfterImplementation != candidateImplementation {
		t.Fatalf("operator router transition = %#v", transition)
	}
	wantRetired := map[string]bool{
		"router": true, "static-api": true, "session-api": true, "authoring-api": true,
		"source-reading-api": true, "source-publication-api": true, "reconciliation-api": true,
	}
	for _, retirement := range receipt.Retirements {
		if !wantRetired[retirement.Entry] {
			t.Fatalf("unexpected operator router dependent retired = %#v", retirement)
		}
		delete(wantRetired, retirement.Entry)
		if retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("operator router retirement retained ownership = %#v", retirement)
		}
	}
	if len(wantRetired) != 0 {
		t.Fatalf("operator router retirement omitted entries = %v", wantRetired)
	}

	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["router"].Implementation != candidateImplementation ||
		after.Entries["router"].Runtime != candidateArtifact {
		t.Fatalf("replacement operator router live evidence = %+v", after)
	}
	assertUnchangedOperatorEntries(t, after, originalArtifact)
	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err := mounted.Export("http")
	if err != nil || afterHTTPContract != httpContract || afterHTTPProvider != httpProvider ||
		afterHTTPRevision <= rollbackHTTPRevision || afterHTTPValue == rollbackHTTPValue {
		t.Fatalf("replacement operator HTTP export = %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}
	afterHandler := afterHTTPValue.(http.Handler)
	assertOperatorRouteMethods(t, rollbackHandler, graph.Fingerprint, http.StatusNotFound)
	assertOperatorRouteMethods(t, afterHandler, graph.Fingerprint, http.StatusMethodNotAllowed)

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertOperatorRouteMethods(t, afterHandler, graph.Fingerprint, http.StatusNotFound)
	closed := mounted.Live()
	if closed.State != "closed" || closed.Fingerprint != bundle.Plan.Fingerprint ||
		closed.Exports["http"].Available {
		t.Fatalf("closed operator realm evidence = %+v", closed)
	}
	for id, entry := range closed.Entries {
		if entry.State != "closed" || entry.Workers != 0 || entry.Effects != 0 ||
			len(entry.Services) != 0 || entry.Error != "" {
			t.Fatalf("closed operator entry %s retained ownership = %+v", id, entry)
		}
	}
}

func assertUnchangedOperatorEntries(
	t *testing.T, live pluginruntime.Live, originalArtifact inspect.ArtifactIdentity,
) {
	t.Helper()
	for _, entryID := range []string{
		"authorizer", "static-source", "session-source", "authoring-source",
		"source-reading-source", "source-publication-source", "reconciliation-source",
		"static-api", "session-api", "authoring-api", "source-reading-api",
		"source-publication-api", "reconciliation-api",
	} {
		entry := live.Entries[entryID]
		if entry.Runtime != originalArtifact || entry.State != "active" {
			t.Fatalf("operator entry %s changed unexpectedly = %+v", entryID, entry)
		}
	}
}

type failingManagementRouterFactory struct {
	delegate *RouterFactory
	failure  error
}

func (factory *failingManagementRouterFactory) Descriptor() plugin.Descriptor {
	return factory.delegate.Descriptor()
}

func (factory *failingManagementRouterFactory) Mount(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return factory.delegate.Mount(ctx, mount)
}

func (factory *failingManagementRouterFactory) PreMount(
	_ context.Context, _ pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return failingManagementRouterCandidate{failure: factory.failure}, nil
}

type failingManagementRouterCandidate struct{ failure error }

func (candidate failingManagementRouterCandidate) Activate(
	context.Context, pluginruntime.MountContext,
) error {
	return candidate.failure
}

var _ pluginruntime.CandidatePreMounter = (*failingManagementRouterFactory)(nil)
