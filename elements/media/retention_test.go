package media

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TestRetainedMediaCancellationBeforeAdmissionIsTerminal(t *testing.T) {
	ports := newRetentionTestPorts()
	runner := newRetentionTestRunner(t, ports, RetainedMediaConfig{
		Scope: "session", MaxItems: 4, MaxBytes: 1024, MaxItemBytes: 512,
		TerminalMemory: 8, CancelMemory: 8,
	})

	cancel := element.Envelope{
		Type: storeCancelType, ItemID: "cancel-1", RunID: "retain-1",
		Payload: StoreCancel{RequestID: "retain-1", Reason: "newer input"},
	}
	if err := runner.cancel(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(RetentionOutcome); outcome.Kind != ResultSucceeded || outcome.Code != "cancel_recorded" {
		t.Fatalf("cancel outcome = %+v", outcome)
	}

	request := element.Envelope{
		Type: retainRequestType, ItemID: "retain-1",
		Payload: RetainRequest{
			AttachmentID: "file-1", Kind: AttachmentFile, Name: "a.txt",
			MIMEType: "text/plain", Content: []byte("hello"), Source: "message",
			Scope: "session", SourceRevision: 1,
		},
	}
	if err := runner.retain(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result := ports.retained.last().Payload.(RetainResult)
	if result.Kind != ResultCanceled || result.Code != "canceled" || len(runner.entries) != 0 {
		t.Fatalf("pre-canceled retain = %+v entries=%d", result, len(runner.entries))
	}

	// Cancellation after the operation terminalized cannot retroactively undo
	// or change that result; the boundary crossing is explicitly reported.
	if err := runner.cancel(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	late := ports.outcome.last().Payload.(RetentionOutcome)
	if late.Kind != ResultIgnored || late.Code != "already_terminal" || !late.Crossed {
		t.Fatalf("late cancellation = %+v", late)
	}
}

func TestRetainedMediaSourceRevisionFencesReplayAfterTerminalEviction(t *testing.T) {
	ports := newRetentionTestPorts()
	runner := newRetentionTestRunner(t, ports, RetainedMediaConfig{
		Scope: "session", MaxItems: 4, MaxBytes: 1024, MaxItemBytes: 512,
		TerminalMemory: 1, CancelMemory: 2,
	})
	base := RetainRequest{
		AttachmentID: "same", Kind: AttachmentFile, Name: "a.txt", MIMEType: "text/plain",
		Content: []byte("first"), Source: "message", Scope: "session", SourceRevision: 7,
	}
	if err := runner.retain(context.Background(), element.Envelope{Type: retainRequestType, ItemID: "request-a", Payload: base}); err != nil {
		t.Fatal(err)
	}
	first := ports.retained.last().Payload.(RetainResult)
	if first.Kind != ResultSucceeded {
		t.Fatalf("first retention = %+v", first)
	}

	// A second request terminalizes and evicts request-a from bounded terminal
	// memory. The retained source-revision index still fences replay/conflict.
	other := base
	other.AttachmentID = "other"
	other.SourceRevision = 1
	if err := runner.retain(context.Background(), element.Envelope{Type: retainRequestType, ItemID: "request-b", Payload: other}); err != nil {
		t.Fatal(err)
	}
	if _, found := runner.terminals["request-a"]; found {
		t.Fatal("request-a was not evicted from terminal memory")
	}

	if err := runner.retain(context.Background(), element.Envelope{Type: retainRequestType, ItemID: "request-c", Payload: base}); err != nil {
		t.Fatal(err)
	}
	replay := ports.retained.last().Payload.(RetainResult)
	if replay.Kind != ResultSucceeded || replay.Handle.Handle != first.Handle.Handle || len(runner.entries) != 2 {
		t.Fatalf("idempotent replay = %+v entries=%d", replay, len(runner.entries))
	}
	conflict := base
	conflict.Content = []byte("different")
	if err := runner.retain(context.Background(), element.Envelope{Type: retainRequestType, ItemID: "request-d", Payload: conflict}); err != nil {
		t.Fatal(err)
	}
	refused := ports.retained.last().Payload.(RetainResult)
	if refused.Kind != ResultRefused || refused.Code != "source_revision_conflict" || len(runner.entries) != 2 {
		t.Fatalf("conflicting replay = %+v entries=%d", refused, len(runner.entries))
	}
}

func TestRetainedMediaConfigRejectsUnknownAndUnsafeBounds(t *testing.T) {
	factory := retainedMediaFactory{}
	for _, source := range []string{
		`{"scope":"session","unknown":1}`,
		`{"scope":"session","max_bytes":4,"max_item_bytes":8}`,
		`{"scope":"session","terminal_memory":0}`,
	} {
		if err := factory.ValidateConfig(json.RawMessage(source)); err == nil {
			t.Fatalf("invalid config was accepted: %s", source)
		}
	}
	if err := factory.ValidateConfig(json.RawMessage(`{"scope":"session","max_bytes":1024,"max_item_bytes":512}`)); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityComparisonAndDigestAreCanonical(t *testing.T) {
	if secureEqual("secret", "secreu") || !secureEqual("secret", "secret") {
		t.Fatal("constant-time capability equality result is incorrect")
	}
	digest := digestContent([]byte("x"))
	if canonical, err := canonicalSHA256(digest); err != nil || canonical != digest {
		t.Fatalf("canonical digest = %q, %v", canonical, err)
	}
	if _, err := canonicalSHA256(strings.ToUpper(digest)); err == nil {
		t.Fatal("noncanonical uppercase digest was accepted")
	}
}

func TestRetainedMediaRejectsNoncanonicalCorrelationAndBoundsReasons(t *testing.T) {
	ports := newRetentionTestPorts()
	runner := newRetentionTestRunner(t, ports, RetainedMediaConfig{
		Scope: "session", MaxItems: 4, MaxBytes: 1024, MaxItemBytes: 512,
		TerminalMemory: 8, CancelMemory: 8,
	})
	request := RetainRequest{
		AttachmentID: "file-1", Kind: AttachmentFile, Name: "a.txt",
		MIMEType: "text/plain", Content: []byte("hello"), Source: "message",
		Scope: "session", SourceRevision: 1,
	}
	if err := runner.retain(context.Background(), element.Envelope{
		Type: retainRequestType, ItemID: " retain-1 ", Payload: request,
	}); err != nil {
		t.Fatal(err)
	}
	if result := ports.retained.last().Payload.(RetainResult); result.Code != "invalid_request_id" {
		t.Fatalf("noncanonical retain result = %+v", result)
	}
	if len(runner.entries) != 0 || len(runner.terminals) != 0 {
		t.Fatalf("noncanonical request changed state: entries=%d terminals=%d", len(runner.entries), len(runner.terminals))
	}

	if err := runner.cancel(context.Background(), element.Envelope{
		Type: storeCancelType, ItemID: "cancel-spaced",
		Payload: StoreCancel{RequestID: " retain-1 ", Reason: "ignored"},
	}); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(RetentionOutcome); outcome.Code != "invalid_request_id" {
		t.Fatalf("noncanonical cancel outcome = %+v", outcome)
	}
	if len(runner.canceled) != 0 {
		t.Fatalf("noncanonical cancellation was retained: %+v", runner.canceled)
	}

	longReason := strings.Repeat("r", maximumReasonBytes+100)
	if err := runner.cancel(context.Background(), element.Envelope{
		Type: storeCancelType, ItemID: "cancel-bounded",
		Payload: StoreCancel{RequestID: "retain-bounded", Reason: longReason},
	}); err != nil {
		t.Fatal(err)
	}
	stored := runner.canceled["retain-bounded"]
	if len(stored) > maximumReasonBytes || !strings.HasSuffix(stored, "…") {
		t.Fatalf("bounded reason has %d bytes and suffix %q", len(stored), stored[max(0, len(stored)-3):])
	}
}

func TestBlobLeaseValidationBindsIdentitySessionAndRevocationAtomically(t *testing.T) {
	handle := AttachmentHandle{
		Handle: "media-test", Capability: "capability", AttachmentID: "attachment",
		Kind: AttachmentFile, Name: "a.txt", MIMEType: "text/plain",
		SHA256: digestContent([]byte("hello")), Bytes: len("hello"),
		Source: "message", Scope: "session", SourceRevision: 1,
	}
	lease := newBlobLease("lease-test", "session-a", handle, newImmutableBlob([]byte("hello")))
	if err := lease.validateFor("session-a", handle); err != nil {
		t.Fatal(err)
	}
	if err := lease.validateFor("session-b", handle); err == nil {
		t.Fatal("cross-session lease validation succeeded")
	}
	altered := handle
	altered.Name = "other.txt"
	if err := lease.validateFor("session-a", altered); err == nil {
		t.Fatal("altered complete handle validation succeeded")
	}
	lease.revoke()
	if err := lease.validateFor("session-a", handle); err == nil {
		t.Fatal("revoked lease validation succeeded")
	}
}

func TestBlobLeaseCopiesOnceAtRetentionBoundaryAndReturnRevokes(t *testing.T) {
	ports := newRetentionTestPorts()
	runner := newRetentionTestRunner(t, ports, RetainedMediaConfig{
		Scope: "session", MaxItems: 2, MaxBytes: 32, MaxItemBytes: 16,
		MaxActiveLeases: 2, TerminalMemory: 16, CancelMemory: 8,
	})
	original := []byte("hello")
	handle := retainForTest(t, runner, ports, "retain-lease", "attachment-lease", original)
	// The transport payload obeys Envelope's immutable ownership contract, but
	// retention nevertheless owns a distinct trust-boundary copy.
	original[0] = 'X'
	lease := fetchForTest(t, runner, ports, "fetch-lease", "session-a", handle)
	reader, err := lease.Open()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("lease content = %q", content)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err = lease.Open()
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := reader.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if err := runner.returnLease(context.Background(), element.Envelope{
		Type: returnLeaseRequestType, ItemID: "return-lease", SessionID: "session-a",
		Payload: ReturnLeaseRequest{Lease: lease, Reason: "consumer finished"},
	}); err != nil {
		t.Fatal(err)
	}
	returned := ports.leaseReturned.last().Payload.(ReturnLeaseResult)
	if returned.Kind != ResultSucceeded || runner.leasedBytes != 0 || len(runner.leases) != 0 {
		t.Fatalf("returned=%+v leased_bytes=%d leases=%d", returned, runner.leasedBytes, len(runner.leases))
	}
	if _, err := reader.Read(buffer); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("reader remained live after return: %v", err)
	}
	if _, err := lease.Open(); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("lease reopened after return: %v", err)
	}
}

func TestEvictionLeavesDeliveredLeaseLiveUntilExplicitReturn(t *testing.T) {
	ports := newRetentionTestPorts()
	runner := newRetentionTestRunner(t, ports, RetainedMediaConfig{
		Scope: "session", MaxItems: 1, MaxBytes: 10, MaxItemBytes: 5,
		MaxActiveLeases: 2, TerminalMemory: 32, CancelMemory: 8,
	})
	first := retainForTest(t, runner, ports, "retain-first", "first", []byte("first"))
	lease := fetchForTest(t, runner, ports, "fetch-first", "session-a", first)
	_ = retainForTest(t, runner, ports, "retain-second", "second", []byte("later"))
	if runner.addressableItems != 1 || runner.liveBytes != 10 || runner.metrics.Evicted != 1 {
		t.Fatalf("post-eviction items=%d bytes=%d metrics=%+v", runner.addressableItems, runner.liveBytes, runner.metrics)
	}
	if err := runner.fetch(context.Background(), element.Envelope{
		Type: fetchRequestType, ItemID: "fetch-evicted", SessionID: "session-a",
		Payload: FetchRequest{Handle: first, ExpectedSHA256: first.SHA256, ExpectedSourceRevision: first.SourceRevision},
	}); err != nil {
		t.Fatal(err)
	}
	if result := ports.fetched.last().Payload.(FetchResult); result.Kind != ResultExpired {
		t.Fatalf("new fetch of evicted handle = %+v", result)
	}
	reader, err := lease.Open()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil || string(content) != "first" {
		t.Fatalf("delivered lease after eviction = %q, %v", content, err)
	}
	_ = reader.Close()
	if err := runner.returnLease(context.Background(), element.Envelope{
		Type: returnLeaseRequestType, ItemID: "return-first", SessionID: "session-a",
		Payload: ReturnLeaseRequest{Lease: lease},
	}); err != nil {
		t.Fatal(err)
	}
	if runner.liveBytes != 5 || len(runner.entries) != 1 {
		t.Fatalf("final orphan return left entries=%d bytes=%d", len(runner.entries), runner.liveBytes)
	}
}

func TestPinnedLeaseBoundsAdmissionAndUnmountRevokes(t *testing.T) {
	ports := newRetentionTestPorts()
	runner := newRetentionTestRunner(t, ports, RetainedMediaConfig{
		Scope: "session", MaxItems: 1, MaxBytes: 5, MaxItemBytes: 5,
		MaxActiveLeases: 1, TerminalMemory: 32, CancelMemory: 8,
	})
	handle := retainForTest(t, runner, ports, "retain-pinned", "pinned", []byte("12345"))
	lease := fetchForTest(t, runner, ports, "fetch-pinned", "session-a", handle)
	if err := runner.fetch(context.Background(), element.Envelope{
		Type: fetchRequestType, ItemID: "fetch-over-limit", SessionID: "session-a",
		Payload: FetchRequest{Handle: handle},
	}); err != nil {
		t.Fatal(err)
	}
	if result := ports.fetched.last().Payload.(FetchResult); result.Kind != ResultRefused || result.Code != "lease_limit" {
		t.Fatalf("active lease bound = %+v", result)
	}
	if err := runner.release(context.Background(), element.Envelope{
		Type: releaseRequestType, ItemID: "release-pinned",
		Payload: ReleaseRequest{Handle: handle.Handle, Capability: handle.Capability, Scope: handle.Scope},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.retain(context.Background(), element.Envelope{
		Type: retainRequestType, ItemID: "retain-blocked",
		Payload: RetainRequest{
			AttachmentID: "blocked", Kind: AttachmentFile, Name: "blocked.txt",
			MIMEType: "text/plain", Content: []byte("x"), Source: "message",
			Scope: "session", SourceRevision: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if result := ports.retained.last().Payload.(RetainResult); result.Kind != ResultRefused || result.Code != "lease_capacity" {
		t.Fatalf("pinned-byte admission = %+v", result)
	}
	runner.revokeAllLeases()
	if _, err := lease.Open(); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("unmount did not revoke lease: %v", err)
	}
	if runner.liveBytes != 0 || runner.leasedBytes != 0 || len(runner.entries) != 0 || len(runner.leases) != 0 {
		t.Fatalf("unmount accounting entries=%d leases=%d live=%d leased=%d", len(runner.entries), len(runner.leases), runner.liveBytes, runner.leasedBytes)
	}
}

func TestDuplicateTerminalRepliesHaveDistinctEvidenceIDs(t *testing.T) {
	ports := newRetentionTestPorts()
	runner := newRetentionTestRunner(t, ports, RetainedMediaConfig{
		Scope: "session", MaxItems: 2, MaxBytes: 32, MaxItemBytes: 16,
		MaxActiveLeases: 2, TerminalMemory: 16, CancelMemory: 8,
	})
	request := element.Envelope{Type: retainRequestType, ItemID: "same-request", Payload: RetainRequest{
		AttachmentID: "same", Kind: AttachmentFile, Name: "same.txt", MIMEType: "text/plain",
		Content: []byte("same"), Source: "message", Scope: "session", SourceRevision: 1,
	}}
	if err := runner.retain(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	first := ports.retained.last()
	if first.ItemID != "same-request:retained" || len(first.CausalParents) != 1 || first.CausalParents[0] != request.ItemID {
		t.Fatalf("first terminal framing = %+v", first)
	}
	if err := runner.retain(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	duplicate := ports.retained.last()
	if duplicate.ItemID == first.ItemID || !strings.HasPrefix(duplicate.ItemID, "same-request:retained:duplicate:") {
		t.Fatalf("duplicate terminal IDs first=%q duplicate=%q", first.ItemID, duplicate.ItemID)
	}
}

func retainForTest(t *testing.T, runner *retainedMediaRunner, ports *retentionTestPorts, requestID, attachmentID string, content []byte) AttachmentHandle {
	t.Helper()
	if err := runner.retain(context.Background(), element.Envelope{
		Type: retainRequestType, ItemID: requestID,
		Payload: RetainRequest{
			AttachmentID: attachmentID, Kind: AttachmentFile, Name: attachmentID + ".txt",
			MIMEType: "text/plain", Content: content, Source: "message",
			Scope: "session", SourceRevision: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	result := ports.retained.last().Payload.(RetainResult)
	if result.Kind != ResultSucceeded {
		t.Fatalf("retain %s = %+v", requestID, result)
	}
	return result.Handle
}

func fetchForTest(t *testing.T, runner *retainedMediaRunner, ports *retentionTestPorts, requestID, sessionID string, handle AttachmentHandle) BlobLease {
	t.Helper()
	if err := runner.fetch(context.Background(), element.Envelope{
		Type: fetchRequestType, ItemID: requestID, SessionID: sessionID,
		Payload: FetchRequest{Handle: handle, ExpectedSHA256: handle.SHA256, ExpectedSourceRevision: handle.SourceRevision},
	}); err != nil {
		t.Fatal(err)
	}
	result := ports.fetched.last().Payload.(FetchResult)
	if result.Kind != ResultSucceeded {
		t.Fatalf("fetch %s = %+v", requestID, result)
	}
	return result.Lease
}

type testClock struct{ now uint64 }

func (clock testClock) NowNS() uint64 { return clock.now }

func newRetentionTestRunner(t *testing.T, ports *retentionTestPorts, config RetainedMediaConfig) *retainedMediaRunner {
	t.Helper()
	if config.MaxMetadataBytes == 0 {
		config.MaxMetadataBytes = 64 << 10
	}
	if config.MaxActiveLeases == 0 {
		config.MaxActiveLeases = 16
	}
	return &retainedMediaRunner{
		instance: "retention", config: config, clock: testClock{now: 10},
		sequences: graphruntime.NewSequenceAllocator(),
		ports: retainedPorts{
			retained: ports.retained, fetched: ports.fetched, released: ports.released,
			leaseReturned: ports.leaseReturned, outcome: ports.outcome, metrics: ports.metrics,
		},
		entries: make(map[string]*retainedEntry), sourceIndex: make(map[string]string),
		leases:    make(map[string]*leaseRecord),
		terminals: make(map[string]terminalRequest), canceled: make(map[string]string),
	}
}

type captureOutput struct {
	name   string
	typeOf element.Type
	mu     sync.Mutex
	values []element.Envelope
}

func (output *captureOutput) Name() string            { return output.name }
func (output *captureOutput) Type() element.Type      { return output.typeOf.Clone() }
func (output *captureOutput) Lanes() []element.Sender { return nil }
func (output *captureOutput) Broadcast(_ context.Context, envelope element.Envelope) (element.SendResult, error) {
	output.mu.Lock()
	output.values = append(output.values, envelope.Clone())
	output.mu.Unlock()
	return element.SendResult{Delivered: 1}, nil
}
func (output *captureOutput) last() element.Envelope {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.values[len(output.values)-1]
}

type retentionTestPorts struct {
	retained, fetched, released, leaseReturned, outcome, metrics *captureOutput
}

func newRetentionTestPorts() *retentionTestPorts {
	return &retentionTestPorts{
		retained:      &captureOutput{name: "retained", typeOf: retainResultType},
		fetched:       &captureOutput{name: "fetched", typeOf: fetchResultType},
		released:      &captureOutput{name: "released", typeOf: releaseResultType},
		leaseReturned: &captureOutput{name: "lease_returned", typeOf: returnLeaseResultType},
		outcome:       &captureOutput{name: "outcome", typeOf: retentionOutcomeType},
		metrics:       &captureOutput{name: "metrics", typeOf: retentionMetricsType},
	}
}
