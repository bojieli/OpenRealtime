package media

import (
	"context"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TestResolverRequiresExactCorrelationAndPreservesPendingOnMalformedReply(t *testing.T) {
	ports := newResolverTestPorts()
	runner := newResolverTestRunner(ports)
	handle := resolverTestHandle([]byte("image"))
	cause := element.Envelope{
		Type: resolveRequestType, ItemID: "resolve-1", SessionID: "session-a",
		Payload: ResolveRequest{Handle: handle, AcceptMIMETypes: []string{"text/*"}},
	}
	if err := runner.begin(context.Background(), cause); err != nil {
		t.Fatal(err)
	}
	fetch := ports.fetch.last()
	if len(fetch.CausalParents) != 1 || fetch.CausalParents[0] != cause.ItemID {
		t.Fatalf("fetch causal framing = %+v", fetch)
	}
	expectedReplyID := fetch.ItemID + ":fetched"
	validLease := newBlobLease("lease-valid", cause.SessionID, handle, newImmutableBlob([]byte("image")))
	valid := FetchResult{Kind: ResultSucceeded, Handle: handle, Lease: validLease}

	cases := []struct {
		name     string
		envelope element.Envelope
		code     string
	}{
		{
			name: "wrong item ID",
			envelope: element.Envelope{Type: fetchResultType, ItemID: fetch.ItemID + ":other", SessionID: cause.SessionID,
				CausalParents: []string{fetch.ItemID}, Payload: valid},
			code: "unknown_fetch_reply",
		},
		{
			name: "extra causal parent",
			envelope: element.Envelope{Type: fetchResultType, ItemID: expectedReplyID, SessionID: cause.SessionID,
				CausalParents: []string{fetch.ItemID, "ambiguous"}, Payload: valid},
			code: "invalid_fetch_parent",
		},
		{
			name: "cross session",
			envelope: element.Envelope{Type: fetchResultType, ItemID: expectedReplyID, SessionID: "session-b",
				CausalParents: []string{fetch.ItemID}, Payload: valid},
			code: "cross_session_fetch_reply",
		},
		{
			name: "altered complete handle",
			envelope: element.Envelope{Type: fetchResultType, ItemID: expectedReplyID, SessionID: cause.SessionID,
				CausalParents: []string{fetch.ItemID}, Payload: func() FetchResult {
					altered := handle
					altered.Name = "other.txt"
					return FetchResult{Kind: ResultSucceeded, Handle: altered, Lease: validLease}
				}()},
			code: "fetch_handle_mismatch",
		},
		{
			name: "forged lease",
			envelope: element.Envelope{Type: fetchResultType, ItemID: expectedReplyID, SessionID: cause.SessionID,
				CausalParents: []string{fetch.ItemID}, Payload: FetchResult{
					Kind: ResultSucceeded, Handle: handle,
					Lease: BlobLease{LeaseID: "forged", Handle: handle},
				}},
			code: "fetched_content_mismatch",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := runner.complete(context.Background(), test.envelope); err != nil {
				t.Fatal(err)
			}
			outcome := ports.outcome.last().Payload.(ResolverOutcome)
			if outcome.Code != test.code {
				t.Fatalf("outcome = %+v", outcome)
			}
			if len(runner.pending) != 1 || runner.pending["resolve-1"].fetchID != fetch.ItemID {
				t.Fatalf("malformed reply consumed pending state: %+v", runner.pending)
			}
		})
	}

	validReply := element.Envelope{
		Type: fetchResultType, ItemID: expectedReplyID, SessionID: cause.SessionID,
		CausalParents: []string{fetch.ItemID}, Payload: valid,
	}
	if err := runner.complete(context.Background(), validReply); err != nil {
		t.Fatal(err)
	}
	if len(runner.pending) != 0 || len(runner.byReply) != 0 {
		t.Fatalf("valid reply left pending state: %+v / %+v", runner.pending, runner.byReply)
	}
	resolved := ports.resolved.last()
	result := resolved.Payload.(ResolvedAttachment)
	if resolved.ItemID != "resolve-1:resolved" || result.Kind != ResultSucceeded || result.Lease.state != validLease.state {
		t.Fatalf("resolved = %+v payload=%+v", resolved, result)
	}
	if err := runner.begin(context.Background(), cause); err != nil {
		t.Fatal(err)
	}
	duplicate := ports.resolved.last()
	if duplicate.ItemID == resolved.ItemID || !strings.HasPrefix(duplicate.ItemID, "resolve-1:resolved:duplicate:") {
		t.Fatalf("duplicate result IDs first=%q duplicate=%q", resolved.ItemID, duplicate.ItemID)
	}
}

func TestResolverReturnsSuccessfulLeaseWhenPendingResolutionWasCanceled(t *testing.T) {
	ports := newResolverTestPorts()
	runner := newResolverTestRunner(ports)
	handle := resolverTestHandle([]byte("content"))
	cause := element.Envelope{
		Type: resolveRequestType, ItemID: "resolve-cancel", SessionID: "session-a",
		Payload: ResolveRequest{Handle: handle},
	}
	if err := runner.begin(context.Background(), cause); err != nil {
		t.Fatal(err)
	}
	fetch := ports.fetch.last()
	if err := runner.cancel(context.Background(), element.Envelope{
		Type: storeCancelType, ItemID: "cancel-resolve", SessionID: cause.SessionID,
		Payload: StoreCancel{RequestID: cause.ItemID, Reason: "superseded"},
	}); err != nil {
		t.Fatal(err)
	}
	lease := newBlobLease("lease-canceled", cause.SessionID, handle, newImmutableBlob([]byte("content")))
	if err := runner.complete(context.Background(), element.Envelope{
		Type: fetchResultType, ItemID: fetch.ItemID + ":fetched", SessionID: cause.SessionID,
		CausalParents: []string{fetch.ItemID},
		Payload:       FetchResult{Kind: ResultSucceeded, Handle: handle, Lease: lease},
	}); err != nil {
		t.Fatal(err)
	}
	returnedEnvelope := ports.returnLease.last()
	returned := returnedEnvelope.Payload.(ReturnLeaseRequest)
	if returned.Lease.state != lease.state || len(returnedEnvelope.CausalParents) != 1 ||
		returnedEnvelope.CausalParents[0] != fetch.ItemID+":fetched" {
		t.Fatalf("returned lease = %+v envelope=%+v", returned, returnedEnvelope)
	}
	if result := ports.resolved.last().Payload.(ResolvedAttachment); result.Kind != ResultCanceled || result.Lease.LeaseID != "" {
		t.Fatalf("canceled resolution = %+v", result)
	}
}

func resolverTestHandle(content []byte) AttachmentHandle {
	return AttachmentHandle{
		Handle: "media-test", Capability: "capability-test", AttachmentID: "attachment-test",
		Kind: AttachmentFile, Name: "test.txt", MIMEType: "text/plain",
		SHA256: digestContent(content), Bytes: len(content), Source: "message",
		Scope: "session", SourceRevision: 1,
	}
}

type resolverTestPorts struct {
	fetch, storeCancel, returnLease, resolved, outcome *captureOutput
}

func newResolverTestPorts() *resolverTestPorts {
	return &resolverTestPorts{
		fetch:       &captureOutput{name: "fetch", typeOf: fetchRequestType},
		storeCancel: &captureOutput{name: "store_cancel", typeOf: storeCancelType},
		returnLease: &captureOutput{name: "return_lease", typeOf: returnLeaseRequestType},
		resolved:    &captureOutput{name: "resolved", typeOf: resolvedAttachmentType},
		outcome:     &captureOutput{name: "outcome", typeOf: resolverOutcomeType},
	}
}

func newResolverTestRunner(ports *resolverTestPorts) *resolveAttachmentRunner {
	return &resolveAttachmentRunner{
		instance: "resolver", config: ResolveAttachmentConfig{
			MaxPending: 8, MaxBytes: 1024, MaxMetadataBytes: 4096,
			AllowedMIMETypes: []string{"*/*"}, TerminalMemory: 16,
		},
		clock: testClock{now: 10}, sequences: graphruntime.NewSequenceAllocator(),
		fetchOutput: ports.fetch, storeCancelOutput: ports.storeCancel,
		returnLeaseOutput: ports.returnLease, resolvedOutput: ports.resolved,
		outcomeOutput: ports.outcome, pending: make(map[string]pendingResolution),
		byReply: make(map[string]string), terminal: make(map[string]struct{}),
		preCanceled: make(map[string]string),
	}
}
