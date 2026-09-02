package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestAllOperatorAPIRoutesReconcileAtomically(t *testing.T) {
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
	originalArtifact := managementRouteArtifact("go://management/operator-routes-v1", "build-1", "a")
	for _, factory := range bundle.Factories {
		if err := registry.RegisterArtifact(factory.Descriptor().Name, originalArtifact, factory); err != nil {
			t.Fatal(err)
		}
	}
	type routeCandidate struct {
		entry          string
		implementation string
		artifact       inspect.ArtifactIdentity
		factory        pluginruntime.Factory
	}
	candidates := []routeCandidate{
		{"static-api", "test/management-static-api-v2",
			managementRouteArtifact("go://management/static-api-v2", "build-2", "1"), NewStaticAPIFactory()},
		{"session-api", "test/management-session-api-v2",
			managementRouteArtifact("go://management/session-api-v2", "build-2", "2"), NewSessionAPIFactory()},
		{"authoring-api", "test/management-authoring-api-v2",
			managementRouteArtifact("go://management/authoring-api-v2", "build-2", "3"), NewAuthoringAPIFactory()},
		{"source-reading-api", "test/management-source-reading-api-v2",
			managementRouteArtifact("go://management/source-reading-api-v2", "build-2", "4"), NewSourceReadingAPIFactory()},
		{"source-publication-api", "test/management-source-publication-api-v2",
			managementRouteArtifact("go://management/source-publication-api-v2", "build-2", "5"), NewSourcePublicationAPIFactory()},
		{"reconciliation-api", "test/management-reconciliation-api-v2",
			managementRouteArtifact("go://management/reconciliation-api-v2", "build-2", "6"), NewReconciliationAPIFactory()},
	}
	for _, candidate := range candidates {
		if err := registry.RegisterArtifact(
			candidate.implementation, candidate.artifact, candidate.factory,
		); err != nil {
			t.Fatal(err)
		}
	}

	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: bundle.Plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	httpValue, httpContract, httpProvider, httpRevision, err := mounted.Export("http")
	if err != nil || httpContract != management.HTTPHandlerContract || httpProvider != "router" {
		t.Fatalf("operator HTTP export = %T %+v %q %d, %v",
			httpValue, httpContract, httpProvider, httpRevision, err)
	}
	handler, ok := httpValue.(http.Handler)
	if !ok {
		t.Fatalf("operator HTTP export value = %T", httpValue)
	}
	assertOperatorRouteMethods(t, handler, graph.Fingerprint, http.StatusMethodNotAllowed)

	before := mounted.Live()
	updates := make([]pluginruntime.EntryUpdate, 0, len(candidates))
	for _, candidate := range candidates {
		updates = append(updates, pluginruntime.EntryUpdate{
			Entry: candidate.entry, SetImplementation: true, Implementation: candidate.implementation,
		})
	}
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: bundle.Plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: updates,
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != bundle.Plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != len(candidates) ||
		len(receipt.Retirements) != len(candidates) || len(receipt.StateTransfers) != 0 {
		t.Fatalf("operator route reconciliation receipt = %#v", receipt)
	}
	transitions := make(map[string]pluginruntime.EntryTransition, len(receipt.Transitions))
	for _, transition := range receipt.Transitions {
		transitions[transition.Entry] = transition
	}
	retired := make(map[string]bool, len(receipt.Retirements))
	for _, retirement := range receipt.Retirements {
		retired[retirement.Entry] = true
		if retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("operator route retirement retained ownership = %#v", retirement)
		}
	}
	for _, candidate := range candidates {
		transition, found := transitions[candidate.entry]
		if !found || transition.BeforeRuntime != originalArtifact ||
			transition.AfterRuntime != candidate.artifact ||
			transition.AfterImplementation != candidate.implementation {
			t.Fatalf("operator route transition %s = %#v", candidate.entry, transition)
		}
		if !retired[candidate.entry] {
			t.Fatalf("operator route %s was not retired: %#v", candidate.entry, receipt.Retirements)
		}
	}

	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["router"].Runtime != originalArtifact ||
		after.Entries["authorizer"].Runtime != originalArtifact {
		t.Fatalf("operator route live evidence = %+v", after)
	}
	for _, candidate := range candidates {
		entry := after.Entries[candidate.entry]
		if entry.Implementation != candidate.implementation || entry.Runtime != candidate.artifact ||
			entry.Workers != 0 || entry.Effects != 1 || len(entry.Services) != 0 {
			t.Fatalf("replacement operator route %s = %+v", candidate.entry, entry)
		}
	}
	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err := mounted.Export("http")
	if err != nil || afterHTTPValue != httpValue || afterHTTPContract != httpContract ||
		afterHTTPProvider != httpProvider || afterHTTPRevision != httpRevision {
		t.Fatalf("stable operator HTTP export changed across route reconciliation: %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}
	assertOperatorRouteMethods(t, handler, graph.Fingerprint, http.StatusMethodNotAllowed)

	closeContext, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelClose()
	if err := mounted.Close(closeContext); err != nil {
		t.Fatal(err)
	}
	assertOperatorRouteMethods(t, handler, graph.Fingerprint, http.StatusNotFound)
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

type routeTestSourceBoundary struct{}

func (routeTestSourceBoundary) Read(
	context.Context, management.SourceReadRequest,
) (management.SourceReadResult, error) {
	return management.SourceReadResult{}, management.ErrUnavailable
}

func (routeTestSourceBoundary) Publish(
	context.Context, management.SourceWriteRequest,
) (management.SourceWriteReceipt, error) {
	return management.SourceWriteReceipt{}, management.ErrUnavailable
}

func managementRouteArtifact(id, revision, hexadecimal string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: id, Revision: revision, Digest: "sha256:" + strings.Repeat(hexadecimal, 64),
	}
}

func assertOperatorRouteMethods(t *testing.T, handler http.Handler, fingerprint string, want int) {
	t.Helper()
	checks := []struct {
		method string
		path   string
	}{
		{http.MethodPost, management.APIPrefix + "/graphs/" + fingerprint},
		{http.MethodPost, management.APIPrefix + "/sessions/sess-test/live"},
		{http.MethodGet, management.APIPrefix + "/authoring/analyze"},
		{http.MethodGet, management.APIPrefix + "/authoring/read"},
		{http.MethodGet, management.APIPrefix + "/authoring/write"},
		{http.MethodGet, management.APIPrefix + "/reconciliations"},
	}
	for _, check := range checks {
		response := request(t, handler, check.method, check.path, "", nil)
		if response.Code != want {
			t.Fatalf("%s %s status = %d, want %d", check.method, check.path, response.Code, want)
		}
	}
}

var (
	_ management.SourceReading          = routeTestSourceBoundary{}
	_ management.SourcePublication      = routeTestSourceBoundary{}
	_ pluginruntime.CandidatePreMounter = NewStaticAPIFactory()
	_ pluginruntime.CandidatePreMounter = NewSessionAPIFactory()
	_ pluginruntime.CandidatePreMounter = NewAuthoringAPIFactory()
	_ pluginruntime.CandidatePreMounter = NewSourceReadingAPIFactory()
	_ pluginruntime.CandidatePreMounter = NewSourcePublicationAPIFactory()
	_ pluginruntime.CandidatePreMounter = NewReconciliationAPIFactory()
)
