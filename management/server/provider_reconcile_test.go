package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestAllOperatorProvidersReconcileAfterJoiningActiveRequest(t *testing.T) {
	graph := testGraph(t)
	oldReader := &lifecycleSourceReader{
		started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}),
		source: "graph predecessor {\n}\n",
	}
	sourceBoundary := routeTestSourceBoundary{}
	bundle, err := NewBundle(BundleConfig{
		Authorizer:    sourceReadAuthorizer{},
		StaticCatalog: testStaticCatalog{graph: graph},
		Sessions:      testSessions{live: testLive(graph)},
		Authoring:     &testAuthoring{},
		SourceReading: oldReader, SourcePublication: sourceBoundary,
		Reconciliation: testReconciler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := pluginruntime.NewRegistry()
	originalArtifact := managementRouteArtifact("go://management/operator-providers-v1", "build-1", "f")
	for _, factory := range bundle.Factories {
		if err := registry.RegisterArtifact(factory.Descriptor().Name, originalArtifact, factory); err != nil {
			t.Fatal(err)
		}
	}

	candidateReader := successfulSourceReader{source: "graph replacement {\n}\n"}
	mustProvider := func(factory pluginruntime.Factory, err error) pluginruntime.Factory {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return factory
	}
	providerCandidates := []providerReconcileCandidate{
		providerReconcileFactory(t, "authorizer", "test/management-authorizer-v2", "1",
			mustProvider(NewAuthorizerProvider(sourceReadAuthorizer{}))),
		providerReconcileFactory(t, "static-source", "test/management-static-source-v2", "2",
			mustProvider(NewStaticCatalogProvider(testStaticCatalog{graph: graph}))),
		providerReconcileFactory(t, "session-source", "test/management-session-source-v2", "3",
			mustProvider(NewSessionInspectionProvider(testSessions{live: testLive(graph)}))),
		providerReconcileFactory(t, "authoring-source", "test/management-authoring-source-v2", "4",
			mustProvider(NewAuthoringProvider(&testAuthoring{}))),
		providerReconcileFactory(t, "source-reading-source", "test/management-source-reading-source-v2", "5",
			mustProvider(NewSourceReadingProvider(candidateReader))),
		providerReconcileFactory(t, "source-publication-source", "test/management-source-publication-source-v2", "6",
			mustProvider(NewSourcePublicationProvider(sourceBoundary))),
		providerReconcileFactory(t, "reconciliation-source", "test/management-reconciliation-source-v2", "7",
			mustProvider(NewReconciliationProvider(testReconciler{}))),
	}
	for _, candidate := range providerCandidates {
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
	handlerValue, handlerContract, handlerProvider, handlerRevision, err := mounted.Export("http")
	if err != nil || handlerContract != management.HTTPHandlerContract || handlerProvider != "router" {
		t.Fatalf("operator HTTP export = %T %+v %q %d, %v",
			handlerValue, handlerContract, handlerProvider, handlerRevision, err)
	}
	handler, ok := handlerValue.(http.Handler)
	if !ok {
		t.Fatalf("operator HTTP export value = %T", handlerValue)
	}

	input := management.SourceReadRequest{
		FormatVersion: management.SourceReadFormatVersion,
		RootIdentity:  "sha256:" + strings.Repeat("8", 64),
		Path:          "replacement.ortg",
	}
	request := sourceReadHTTPRequest(t, input)
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(response, request)
	}()
	waitManagementRequestSignal(t, oldReader.started, "predecessor provider did not receive the request")
	if workers := mounted.Live().Entries["source-reading-api"].Workers; workers != 1 {
		t.Fatalf("active provider request workers = %d, want 1", workers)
	}

	before := mounted.Live()
	updates := make([]pluginruntime.EntryUpdate, 0, len(providerCandidates))
	for _, candidate := range providerCandidates {
		updates = append(updates, pluginruntime.EntryUpdate{
			Entry: candidate.entry, SetImplementation: true, Implementation: candidate.implementation,
		})
	}
	reconciled := make(chan managementReconcileResult, 1)
	go func() {
		receipt, reconcileErr := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
			ExpectedPlanFingerprint: bundle.Plan.Fingerprint, ExpectedSequence: before.Sequence,
			Updates: updates,
		})
		reconciled <- managementReconcileResult{receipt: receipt, err: reconcileErr}
	}()
	assertManagementReconcilePending(t, reconciled)
	close(oldReader.release)
	reconcileResult := waitManagementReconcileResult(t, reconciled)
	receipt, err := reconcileResult.receipt, reconcileResult.err
	if err != nil {
		t.Fatal(err)
	}
	waitManagementRequestSignal(t, oldReader.finished, "predecessor provider request did not finish")
	waitManagementRequestSignal(t, served, "predecessor provider request was not joined")
	if response.Code != http.StatusOK {
		t.Fatalf("drained predecessor provider request status = %d: %s",
			response.Code, response.Body.String())
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != bundle.Plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != len(providerCandidates) ||
		len(receipt.Retirements) != 13 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("operator provider reconciliation receipt = %#v", receipt)
	}
	transitions := make(map[string]pluginruntime.EntryTransition, len(receipt.Transitions))
	for _, transition := range receipt.Transitions {
		transitions[transition.Entry] = transition
	}
	for _, candidate := range providerCandidates {
		transition := transitions[candidate.entry]
		if transition.BeforeRuntime != originalArtifact || transition.AfterRuntime != candidate.artifact ||
			transition.AfterImplementation != candidate.implementation {
			t.Fatalf("operator provider transition %s = %#v", candidate.entry, transition)
		}
	}
	wantRetired := map[string]bool{
		"authorizer": true, "static-source": true, "session-source": true,
		"authoring-source": true, "source-reading-source": true,
		"source-publication-source": true, "reconciliation-source": true,
		"static-api": true, "session-api": true, "authoring-api": true,
		"source-reading-api": true, "source-publication-api": true, "reconciliation-api": true,
	}
	for _, retirement := range receipt.Retirements {
		if !wantRetired[retirement.Entry] {
			t.Fatalf("unexpected operator provider dependent retired = %#v", retirement)
		}
		delete(wantRetired, retirement.Entry)
		if retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("operator provider retirement retained ownership = %#v", retirement)
		}
	}
	if len(wantRetired) != 0 {
		t.Fatalf("operator provider retirement omitted entries = %v", wantRetired)
	}

	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence || after.Entries["router"].Runtime != originalArtifact {
		t.Fatalf("operator provider live evidence = %+v", after)
	}
	for _, candidate := range providerCandidates {
		entry := after.Entries[candidate.entry]
		if entry.Implementation != candidate.implementation || entry.Runtime != candidate.artifact ||
			entry.Workers != 0 || len(entry.Services) != 1 {
			t.Fatalf("replacement operator provider %s = %+v", candidate.entry, entry)
		}
	}
	for _, entryID := range []string{
		"static-api", "session-api", "authoring-api", "source-reading-api",
		"source-publication-api", "reconciliation-api",
	} {
		entry := after.Entries[entryID]
		if entry.Runtime != originalArtifact || entry.Workers != 0 || entry.Effects != 1 {
			t.Fatalf("remounted operator route %s = %+v", entryID, entry)
		}
	}
	afterHandlerValue, afterContract, afterProvider, afterRevision, err := mounted.Export("http")
	if err != nil || afterHandlerValue != handlerValue || afterContract != handlerContract ||
		afterProvider != handlerProvider || afterRevision != handlerRevision {
		t.Fatalf("stable operator HTTP export changed across provider reconciliation: %T/%+v/%s/%d, %v",
			afterHandlerValue, afterContract, afterProvider, afterRevision, err)
	}
	assertOperatorRouteMethods(t, handler, graph.Fingerprint, http.StatusMethodNotAllowed)

	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, sourceReadHTTPRequest(t, input))
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("replacement provider response = %d: %s",
			secondResponse.Code, secondResponse.Body.String())
	}
	var result management.SourceReadResult
	if err := json.Unmarshal(secondResponse.Body.Bytes(), &result); err != nil ||
		management.ValidateSourceReadResult(input, result) != nil || result.Source != candidateReader.source {
		t.Fatalf("replacement provider result = %#v, %v", result, err)
	}

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertOperatorRouteMethods(t, handler, graph.Fingerprint, http.StatusNotFound)
	for id, entry := range mounted.Live().Entries {
		if entry.State != "closed" || entry.Workers != 0 || entry.Effects != 0 ||
			len(entry.Services) != 0 || entry.Error != "" {
			t.Fatalf("closed operator entry %s retained ownership = %+v", id, entry)
		}
	}
}

type providerReconcileCandidate struct {
	entry          string
	implementation string
	artifact       inspect.ArtifactIdentity
	factory        pluginruntime.Factory
}

func providerReconcileFactory(
	t *testing.T,
	entry string,
	implementation string,
	hexadecimal string,
	factory pluginruntime.Factory,
) providerReconcileCandidate {
	t.Helper()
	return providerReconcileCandidate{
		entry: entry, implementation: implementation,
		artifact: managementRouteArtifact("go://management/"+entry+"-v2", "build-2", hexadecimal),
		factory:  factory,
	}
}

type successfulSourceReader struct{ source string }

func (reader successfulSourceReader) Read(
	_ context.Context, request management.SourceReadRequest,
) (management.SourceReadResult, error) {
	return management.NewSourceReadResult(request, reader.source)
}

var _ management.SourceReading = successfulSourceReader{}
