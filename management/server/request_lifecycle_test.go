package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestManagementRouteReplacementDrainsAndJoinsActiveRequest(t *testing.T) {
	reader := &lifecycleSourceReader{
		started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}),
		source: "graph predecessor {\n}\n",
	}
	bundle, err := NewBundle(BundleConfig{
		Authorizer: sourceReadAuthorizer{}, SourceReading: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := pluginruntime.NewRegistry()
	originalArtifact := managementRouteArtifact("go://management/request-lifecycle-v1", "build-1", "d")
	for _, factory := range bundle.Factories {
		if err := registry.RegisterArtifact(factory.Descriptor().Name, originalArtifact, factory); err != nil {
			t.Fatal(err)
		}
	}
	const candidateImplementation = "test/management-source-reading-api-v2"
	candidateArtifact := managementRouteArtifact("go://management/source-reading-api-v2", "build-2", "e")
	if err := registry.RegisterArtifact(
		candidateImplementation, candidateArtifact, NewSourceReadingAPIFactory(),
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
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}

	input := management.SourceReadRequest{
		FormatVersion: management.SourceReadFormatVersion,
		RootIdentity:  "sha256:" + strings.Repeat("a", 64),
		Path:          "agent.ortg",
	}
	request := sourceReadHTTPRequest(t, input)
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(response, request)
	}()
	waitManagementRequestSignal(t, reader.started, "source reader did not receive the request")
	if workers := mounted.Live().Entries["source-reading-api"].Workers; workers != 1 {
		t.Fatalf("active source-reading request workers = %d, want 1", workers)
	}

	before := mounted.Live()
	reconciled := make(chan managementReconcileResult, 1)
	go func() {
		receipt, reconcileErr := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
			ExpectedPlanFingerprint: bundle.Plan.Fingerprint, ExpectedSequence: before.Sequence,
			Updates: []pluginruntime.EntryUpdate{{
				Entry: "source-reading-api", SetImplementation: true,
				Implementation: candidateImplementation,
			}},
		})
		reconciled <- managementReconcileResult{receipt: receipt, err: reconcileErr}
	}()
	assertManagementReconcilePending(t, reconciled)
	close(reader.release)
	reconcileResult := waitManagementReconcileResult(t, reconciled)
	receipt, err := reconcileResult.receipt, reconcileResult.err
	if err != nil {
		t.Fatal(err)
	}
	waitManagementRequestSignal(t, reader.finished, "predecessor provider request did not finish")
	waitManagementRequestSignal(t, served, "retired request handler was not joined")
	if response.Code != http.StatusOK {
		t.Fatalf("drained predecessor request status = %d: %s", response.Code, response.Body.String())
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != bundle.Plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 1 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("active-request route reconciliation receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "source-reading-api" || transition.BeforeRuntime != originalArtifact ||
		transition.AfterRuntime != candidateArtifact || transition.AfterImplementation != candidateImplementation {
		t.Fatalf("active-request route transition = %#v", transition)
	}
	retirement := receipt.Retirements[0]
	if retirement.Entry != "source-reading-api" || retirement.RetiredScopes == 0 ||
		retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
		retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
		retirement.RemainingServices != 0 {
		t.Fatalf("active-request route retirement = %#v", retirement)
	}
	after := mounted.Live()
	entry := after.Entries["source-reading-api"]
	if after.Sequence != receipt.AfterSequence || entry.Implementation != candidateImplementation ||
		entry.Runtime != candidateArtifact || entry.Workers != 0 || entry.Effects != 1 {
		t.Fatalf("replacement source-reading route evidence = %+v", after)
	}

	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, sourceReadHTTPRequest(t, input))
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("replacement source-reading response = %d: %s",
			secondResponse.Code, secondResponse.Body.String())
	}
	var result management.SourceReadResult
	if err := json.Unmarshal(secondResponse.Body.Bytes(), &result); err != nil ||
		management.ValidateSourceReadResult(input, result) != nil {
		t.Fatalf("replacement source-reading result = %#v, %v", result, err)
	}

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed := mounted.Live()
	for id, live := range closed.Entries {
		if live.State != "closed" || live.Workers != 0 || live.Effects != 0 ||
			len(live.Services) != 0 || live.Error != "" {
			t.Fatalf("closed management entry %s retained ownership = %+v", id, live)
		}
	}
}

type sourceReadAuthorizer struct{}

func (sourceReadAuthorizer) Authorize(
	_ context.Context, request management.AuthorizationRequest,
) error {
	if request.Capability == "source-token" && request.Operation == management.ReadSource &&
		management.CanonicalDigest(request.Resource) {
		return nil
	}
	return management.ErrUnauthorized
}

type lifecycleSourceReader struct {
	calls    atomic.Uint64
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	source   string
}

func (reader *lifecycleSourceReader) Read(
	ctx context.Context, request management.SourceReadRequest,
) (management.SourceReadResult, error) {
	if reader.calls.Add(1) == 1 {
		close(reader.started)
		select {
		case <-reader.release:
			close(reader.finished)
		case <-ctx.Done():
			return management.SourceReadResult{}, ctx.Err()
		}
	}
	return management.NewSourceReadResult(request, reader.source)
}

func sourceReadHTTPRequest(t *testing.T, input management.SourceReadRequest) *http.Request {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost, management.APIPrefix+"/authoring/read", strings.NewReader(string(payload)),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(management.CapabilityHeader, "source-token")
	return request
}

func waitManagementRequestSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}

type managementReconcileResult struct {
	receipt pluginruntime.ReconcileReceipt
	err     error
}

func assertManagementReconcilePending(t *testing.T, result <-chan managementReconcileResult) {
	t.Helper()
	select {
	case completed := <-result:
		t.Fatalf("reconciliation completed before active requests drained: %#v, %v",
			completed.receipt, completed.err)
	case <-time.After(25 * time.Millisecond):
	}
}

func waitManagementReconcileResult(
	t *testing.T, result <-chan managementReconcileResult,
) managementReconcileResult {
	t.Helper()
	select {
	case completed := <-result:
		return completed
	case <-time.After(2 * time.Second):
		t.Fatal("reconciliation did not finish after active requests drained")
		return managementReconcileResult{}
	}
}

var _ management.Authorizer = sourceReadAuthorizer{}
var _ management.SourceReading = (*lifecycleSourceReader)(nil)
