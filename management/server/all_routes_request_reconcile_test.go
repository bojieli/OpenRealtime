package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestAllOperatorRouteFamiliesCancelAndJoinActiveRequests(t *testing.T) {
	graph := testGraph(t)
	staticBlocker := newManagementRequestBlocker()
	sessionBlocker := newManagementRequestBlocker()
	authoringBlocker := newManagementRequestBlocker()
	readingBlocker := newManagementRequestBlocker()
	publicationBlocker := newManagementRequestBlocker()
	reconciliationBlocker := newManagementRequestBlocker()
	bundle, err := NewBundle(BundleConfig{
		Authorizer: allFamilyAuthorizer{},
		StaticCatalog: blockingStaticCatalog{
			testStaticCatalog: testStaticCatalog{graph: graph}, blocker: staticBlocker,
		},
		Sessions: blockingSessionInspection{
			testSessions: testSessions{live: testLive(graph)}, blocker: sessionBlocker,
		},
		Authoring: &blockingAuthoring{
			testAuthoring: &testAuthoring{}, blocker: authoringBlocker,
		},
		SourceReading:     blockingSourceReading{blocker: readingBlocker},
		SourcePublication: blockingSourcePublication{blocker: publicationBlocker},
		Reconciliation:    blockingReconciliation{blocker: reconciliationBlocker},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := pluginruntime.NewRegistry()
	originalArtifact := managementRouteArtifact("go://management/all-active-requests-v1", "build-1", "9")
	for _, factory := range bundle.Factories {
		if err := registry.RegisterArtifact(factory.Descriptor().Name, originalArtifact, factory); err != nil {
			t.Fatal(err)
		}
	}
	candidates := []allFamilyRouteCandidate{
		{entry: "static-api", implementation: "test/active-static-api-v2",
			hexadecimal: "1", factory: NewStaticAPIFactory()},
		{entry: "session-api", implementation: "test/active-session-api-v2",
			hexadecimal: "2", factory: NewSessionAPIFactory()},
		{entry: "authoring-api", implementation: "test/active-authoring-api-v2",
			hexadecimal: "3", factory: NewAuthoringAPIFactory()},
		{entry: "source-reading-api", implementation: "test/active-source-reading-api-v2",
			hexadecimal: "4", factory: NewSourceReadingAPIFactory()},
		{entry: "source-publication-api", implementation: "test/active-source-publication-api-v2",
			hexadecimal: "5", factory: NewSourcePublicationAPIFactory()},
		{entry: "reconciliation-api", implementation: "test/active-reconciliation-api-v2",
			hexadecimal: "6", factory: NewReconciliationAPIFactory()},
	}
	for index := range candidates {
		candidate := &candidates[index]
		candidate.artifact = managementRouteArtifact(
			"go://management/"+candidate.entry+"-active-v2", "build-2", candidate.hexadecimal,
		)
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
	handler := handlerValue.(http.Handler)

	rootIdentity := "sha256:" + strings.Repeat("a", 64)
	active := []activeManagementRequest{
		{
			entry: "static-api", blocker: staticBlocker,
			request: managementHTTPRequest(t, http.MethodGet,
				management.APIPrefix+"/graphs/"+graph.Fingerprint, nil),
		},
		{
			entry: "session-api", blocker: sessionBlocker,
			request: managementHTTPRequest(t, http.MethodGet,
				management.APIPrefix+"/sessions/sess-test/live", nil),
		},
		{
			entry: "authoring-api", blocker: authoringBlocker,
			request: managementHTTPRequest(t, http.MethodPost,
				management.APIPrefix+"/authoring/analyze", management.AuthoringDocument{
					Path: "active.ortg", Source: "graph active {\n}\n",
				}),
		},
		{
			entry: "source-reading-api", blocker: readingBlocker,
			request: managementHTTPRequest(t, http.MethodPost,
				management.APIPrefix+"/authoring/read", management.SourceReadRequest{
					FormatVersion: management.SourceReadFormatVersion,
					RootIdentity:  rootIdentity, Path: "active.ortg",
				}),
		},
		{
			entry: "source-publication-api", blocker: publicationBlocker,
			request: managementHTTPRequest(t, http.MethodPost,
				management.APIPrefix+"/authoring/write", management.SourceWriteRequest{
					FormatVersion: management.SourceWriteFormatVersion,
					RootIdentity:  rootIdentity, Mode: management.SourceCreate,
					Path: "active.ortg", Source: "graph active {\n}\n",
				}),
		},
		{
			entry: "reconciliation-api", blocker: reconciliationBlocker,
			request: managementHTTPRequest(t, http.MethodPost,
				management.APIPrefix+"/reconciliations", management.ReconciliationRequest{
					SessionID: "sess-test", ExpectedFingerprint: graph.Fingerprint,
					Candidate: graph, ValuesFingerprint: rootIdentity,
					DeploymentFingerprint: "sha256:" + strings.Repeat("b", 64),
				}),
		},
	}
	for index := range active {
		active[index].response = httptest.NewRecorder()
		active[index].served = make(chan struct{})
		request := &active[index]
		go func() {
			defer close(request.served)
			handler.ServeHTTP(request.response, request.request)
		}()
	}
	for index := range active {
		waitManagementRequestSignal(
			t, active[index].blocker.started, active[index].entry+" provider did not start",
		)
	}
	liveWithRequests := mounted.Live()
	for _, request := range active {
		if workers := liveWithRequests.Entries[request.entry].Workers; workers != 1 {
			t.Fatalf("active %s workers = %d, want 1", request.entry, workers)
		}
	}

	updates := make([]pluginruntime.EntryUpdate, 0, len(candidates))
	for _, candidate := range candidates {
		updates = append(updates, pluginruntime.EntryUpdate{
			Entry: candidate.entry, SetImplementation: true, Implementation: candidate.implementation,
		})
	}
	before := mounted.Live()
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: bundle.Plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: updates,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := range active {
		request := &active[index]
		waitManagementRequestSignal(t, request.blocker.canceled, request.entry+" was not canceled")
		waitManagementRequestSignal(t, request.served, request.entry+" was not joined")
		if request.response.Code == http.StatusOK {
			t.Fatalf("canceled %s response status = %d, want failure", request.entry, request.response.Code)
		}
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != bundle.Plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != len(candidates) ||
		len(receipt.Retirements) != len(candidates) || len(receipt.StateTransfers) != 0 {
		t.Fatalf("all-active-route reconciliation receipt = %#v", receipt)
	}
	transitions := make(map[string]pluginruntime.EntryTransition, len(receipt.Transitions))
	for _, transition := range receipt.Transitions {
		transitions[transition.Entry] = transition
	}
	retirements := make(map[string]pluginruntime.EntryRetirement, len(receipt.Retirements))
	for _, retirement := range receipt.Retirements {
		retirements[retirement.Entry] = retirement
	}
	for _, candidate := range candidates {
		transition := transitions[candidate.entry]
		if transition.BeforeRuntime != originalArtifact || transition.AfterRuntime != candidate.artifact ||
			transition.AfterImplementation != candidate.implementation {
			t.Fatalf("active route transition %s = %#v", candidate.entry, transition)
		}
		retirement := retirements[candidate.entry]
		if retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("active route retirement %s = %#v", candidate.entry, retirement)
		}
	}

	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence || after.Entries["router"].Runtime != originalArtifact {
		t.Fatalf("all-active-route live evidence = %+v", after)
	}
	for _, candidate := range candidates {
		entry := after.Entries[candidate.entry]
		if entry.Implementation != candidate.implementation || entry.Runtime != candidate.artifact ||
			entry.Workers != 0 || entry.Effects != 1 {
			t.Fatalf("replacement active route %s = %+v", candidate.entry, entry)
		}
	}
	afterHandlerValue, afterContract, afterProvider, afterRevision, err := mounted.Export("http")
	if err != nil || afterHandlerValue != handlerValue || afterContract != handlerContract ||
		afterProvider != handlerProvider || afterRevision != handlerRevision {
		t.Fatalf("stable operator export changed across active route reconciliation: %T/%+v/%s/%d, %v",
			afterHandlerValue, afterContract, afterProvider, afterRevision, err)
	}
	assertOperatorRouteMethods(t, handler, graph.Fingerprint, http.StatusMethodNotAllowed)

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

type allFamilyRouteCandidate struct {
	entry          string
	implementation string
	hexadecimal    string
	factory        pluginruntime.Factory
	artifact       inspect.ArtifactIdentity
}

type activeManagementRequest struct {
	entry    string
	blocker  *managementRequestBlocker
	request  *http.Request
	response *httptest.ResponseRecorder
	served   chan struct{}
}

type managementRequestBlocker struct {
	started  chan struct{}
	canceled chan struct{}
}

func newManagementRequestBlocker() *managementRequestBlocker {
	return &managementRequestBlocker{started: make(chan struct{}), canceled: make(chan struct{})}
}

func (blocker *managementRequestBlocker) wait(ctx context.Context) error {
	close(blocker.started)
	<-ctx.Done()
	close(blocker.canceled)
	return ctx.Err()
}

type allFamilyAuthorizer struct{}

func (allFamilyAuthorizer) Authorize(
	_ context.Context, request management.AuthorizationRequest,
) error {
	if request.Capability == "all-family-token" {
		return nil
	}
	return management.ErrUnauthorized
}

type blockingStaticCatalog struct {
	testStaticCatalog
	blocker *managementRequestBlocker
}

func (catalog blockingStaticCatalog) Graph(ctx context.Context, _ string) (ir.Graph, error) {
	return ir.Graph{}, catalog.blocker.wait(ctx)
}

type blockingSessionInspection struct {
	testSessions
	blocker *managementRequestBlocker
}

func (source blockingSessionInspection) Snapshot(ctx context.Context, _ string) (inspect.Live, error) {
	return inspect.Live{}, source.blocker.wait(ctx)
}

type blockingAuthoring struct {
	*testAuthoring
	blocker *managementRequestBlocker
}

func (authoring *blockingAuthoring) Analyze(
	ctx context.Context, _ management.AuthoringDocument,
) (management.AnalysisResult, error) {
	return management.AnalysisResult{}, authoring.blocker.wait(ctx)
}

type blockingSourceReading struct{ blocker *managementRequestBlocker }

func (reading blockingSourceReading) Read(
	ctx context.Context, _ management.SourceReadRequest,
) (management.SourceReadResult, error) {
	return management.SourceReadResult{}, reading.blocker.wait(ctx)
}

type blockingSourcePublication struct{ blocker *managementRequestBlocker }

func (publication blockingSourcePublication) Publish(
	ctx context.Context, _ management.SourceWriteRequest,
) (management.SourceWriteReceipt, error) {
	return management.SourceWriteReceipt{}, publication.blocker.wait(ctx)
}

type blockingReconciliation struct{ blocker *managementRequestBlocker }

func (reconciliation blockingReconciliation) Apply(
	ctx context.Context, _ management.ReconciliationRequest,
) (management.ReconciliationReceipt, error) {
	return management.ReconciliationReceipt{}, reconciliation.blocker.wait(ctx)
}

func managementHTTPRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set(management.CapabilityHeader, "all-family-token")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

var (
	_ management.Authorizer        = allFamilyAuthorizer{}
	_ management.StaticCatalog     = blockingStaticCatalog{}
	_ management.SessionInspection = blockingSessionInspection{}
	_ management.Authoring         = (*blockingAuthoring)(nil)
	_ management.SourceReading     = blockingSourceReading{}
	_ management.SourcePublication = blockingSourcePublication{}
	_ management.Reconciliation    = blockingReconciliation{}
)
