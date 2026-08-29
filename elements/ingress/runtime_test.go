package ingress

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TestIngressIdentifiersAreExactAndCancellationReasonsAreBounded(t *testing.T) {
	if value, err := canonicalID("content-1", "content_id"); err != nil || value != "content-1" {
		t.Fatalf("canonical ID = %q, %v", value, err)
	}
	for _, value := range []string{" content-1", "content 1", "content-1\n", strings.Repeat("x", maximumIdentifierBytes+1)} {
		if _, err := canonicalID(value, "content_id"); err == nil {
			t.Fatalf("noncanonical ID %q was accepted", value)
		}
	}
	reason := boundedReason(strings.Repeat("r", maximumReasonBytes+100))
	if len(reason) > maximumReasonBytes || !strings.HasSuffix(reason, "…") {
		t.Fatalf("bounded reason has %d bytes", len(reason))
	}
}

func TestIngressRequiresExactRetentionCorrelationAndCompleteHandle(t *testing.T) {
	ports := newIngressTestPorts()
	runner := newIngressTestRunner(ports)
	cause := ingressImageEnvelope("image-request", "session-a", "image-content", "image-stream")
	if err := runner.attachment(context.Background(), "image", cause); err != nil {
		t.Fatal(err)
	}
	retainEnvelope := ports.retain.last()
	request := retainEnvelope.Payload.(mediaelements.RetainRequest)
	handle := mediaelements.AttachmentHandle{
		Handle: "media-test", Capability: "capability-test",
		AttachmentID: request.AttachmentID, Kind: request.Kind, Name: request.Name,
		MIMEType: request.MIMEType, SHA256: request.SHA256, Bytes: len(request.Content),
		Source: request.Source, Scope: request.Scope, SourceRevision: request.SourceRevision,
		CapturedNS: request.CapturedNS, Width: request.Width, Height: request.Height,
	}
	validResult := mediaelements.RetainResult{Kind: mediaelements.ResultSucceeded, Handle: handle}
	expectedReplyID := retainEnvelope.ItemID + ":retained"

	malformed := []struct {
		name     string
		envelope element.Envelope
		code     string
	}{
		{
			name: "wrong item ID",
			envelope: element.Envelope{Type: mediaelements.RetainResultType(), ItemID: retainEnvelope.ItemID + ":other",
				SessionID: cause.SessionID, CausalParents: []string{retainEnvelope.ItemID}, Payload: validResult},
			code: "unknown_reply",
		},
		{
			name: "extra immediate parent",
			envelope: element.Envelope{Type: mediaelements.RetainResultType(), ItemID: expectedReplyID,
				SessionID: cause.SessionID, CausalParents: []string{retainEnvelope.ItemID, "ambiguous"}, Payload: validResult},
			code: "invalid_reply_parent",
		},
		{
			name: "cross session",
			envelope: element.Envelope{Type: mediaelements.RetainResultType(), ItemID: expectedReplyID,
				SessionID: "session-b", CausalParents: []string{retainEnvelope.ItemID}, Payload: validResult},
			code: "cross_session_reply",
		},
		{
			name: "wrong payload",
			envelope: element.Envelope{Type: mediaelements.RetainResultType(), ItemID: expectedReplyID,
				SessionID: cause.SessionID, CausalParents: []string{retainEnvelope.ItemID}, Payload: "not a result"},
			code: "invalid_retention_reply",
		},
	}
	for _, test := range malformed {
		t.Run(test.name, func(t *testing.T) {
			if err := runner.retained(context.Background(), test.envelope); err != nil {
				t.Fatal(err)
			}
			if outcome := ports.outcome.last().Payload.(Outcome); outcome.Code != test.code {
				t.Fatalf("outcome = %+v", outcome)
			}
			assertIngressPending(t, runner, retainEnvelope.ItemID, cause.ItemID, cause.Payload.(UserImage).ContentID)
		})
	}

	mutations := []struct {
		name   string
		mutate func(*mediaelements.AttachmentHandle)
	}{
		{"empty capability", func(value *mediaelements.AttachmentHandle) { value.Capability = "" }},
		{"digest", func(value *mediaelements.AttachmentHandle) { value.SHA256 = "sha256:" + strings.Repeat("0", 64) }},
		{"source revision", func(value *mediaelements.AttachmentHandle) { value.SourceRevision++ }},
		{"name", func(value *mediaelements.AttachmentHandle) { value.Name = "other.png" }},
		{"dimensions", func(value *mediaelements.AttachmentHandle) { value.Width++ }},
	}
	for _, mutation := range mutations {
		t.Run("altered "+mutation.name, func(t *testing.T) {
			altered := handle
			mutation.mutate(&altered)
			if err := runner.retained(context.Background(), element.Envelope{
				Type: mediaelements.RetainResultType(), ItemID: expectedReplyID,
				SessionID: cause.SessionID, CausalParents: []string{retainEnvelope.ItemID},
				Payload: mediaelements.RetainResult{Kind: mediaelements.ResultSucceeded, Handle: altered},
			}); err != nil {
				t.Fatal(err)
			}
			if outcome := ports.outcome.last().Payload.(Outcome); outcome.Code != "retained_handle_mismatch" {
				t.Fatalf("outcome = %+v", outcome)
			}
			assertIngressPending(t, runner, retainEnvelope.ItemID, cause.ItemID, cause.Payload.(UserImage).ContentID)
		})
	}

	if err := runner.retained(context.Background(), element.Envelope{
		Type: mediaelements.RetainResultType(), ItemID: expectedReplyID,
		SessionID: cause.SessionID, CausalParents: []string{retainEnvelope.ItemID}, Payload: validResult,
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.pending) != 0 || len(runner.pendingByReply) != 0 || len(runner.pendingByContent) != 0 {
		t.Fatalf("valid reply left pending state: pending=%+v replies=%+v content=%+v", runner.pending, runner.pendingByReply, runner.pendingByContent)
	}
	if outcome := ports.outcome.last().Payload.(Outcome); outcome.Kind != OutcomeSucceeded || outcome.Handle != handle.Handle {
		t.Fatalf("valid outcome = %+v", outcome)
	}
}

func TestIngressDoesNotOverwritePendingContentAndOutcomesStayUnique(t *testing.T) {
	ports := newIngressTestPorts()
	runner := newIngressTestRunner(ports)
	first := ingressImageEnvelope("first-request", "session-a", "same-content", "first-stream")
	if err := runner.attachment(context.Background(), "image", first); err != nil {
		t.Fatal(err)
	}
	firstRetainID := ports.retain.last().ItemID
	second := ingressImageEnvelope("second-request", "session-a", "same-content", "second-stream")
	if err := runner.attachment(context.Background(), "image", second); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(Outcome); outcome.Code != "content_pending" {
		t.Fatalf("content collision outcome = %+v", outcome)
	}
	if runner.pendingByContent["same-content"] != firstRetainID || len(runner.pending) != 1 {
		t.Fatalf("content index was overwritten: %+v pending=%+v", runner.pendingByContent, runner.pending)
	}

	unknown := element.Envelope{
		Type: mediaelements.RetainResultType(), ItemID: "late-reply", SessionID: "session-a",
		Payload: mediaelements.RetainResult{Kind: mediaelements.ResultIgnored},
	}
	if err := runner.retained(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	firstOutcomeID := ports.outcome.last().ItemID
	if err := runner.retained(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	secondOutcomeID := ports.outcome.last().ItemID
	if firstOutcomeID == secondOutcomeID {
		t.Fatalf("duplicate late outcomes reused item ID %q", firstOutcomeID)
	}
}

func assertIngressPending(t *testing.T, runner *userContentRunner, retainID, causeID, contentID string) {
	t.Helper()
	if len(runner.pending) != 1 || runner.pendingByItem[causeID] != retainID ||
		runner.pendingByContent[contentID] != retainID {
		t.Fatalf("pending state was consumed or drifted: pending=%+v item=%+v content=%+v", runner.pending, runner.pendingByItem, runner.pendingByContent)
	}
}

func ingressImageEnvelope(itemID, sessionID, contentID, streamID string) element.Envelope {
	return element.Envelope{
		Type: userImageType, ItemID: itemID, SessionID: sessionID, SourceID: streamID,
		CaptureNS: 100,
		Payload: UserImage{
			ContentID: contentID, StreamID: streamID, MIMEType: "image/png",
			Content: []byte("image"), Width: 8, Height: 6, SourceRevision: 1,
		},
	}
}

type ingressTestClock struct{ now uint64 }

func (clock ingressTestClock) NowNS() uint64 { return clock.now }

type ingressCaptureOutput struct {
	name   string
	typeOf element.Type
	mu     sync.Mutex
	values []element.Envelope
}

func (output *ingressCaptureOutput) Name() string            { return output.name }
func (output *ingressCaptureOutput) Type() element.Type      { return output.typeOf.Clone() }
func (output *ingressCaptureOutput) Lanes() []element.Sender { return nil }
func (output *ingressCaptureOutput) Broadcast(_ context.Context, envelope element.Envelope) (element.SendResult, error) {
	output.mu.Lock()
	output.values = append(output.values, envelope.Clone())
	output.mu.Unlock()
	return element.SendResult{Delivered: 1}, nil
}
func (output *ingressCaptureOutput) last() element.Envelope {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.values[len(output.values)-1]
}

type ingressTestPorts struct {
	retain, release, storeCancel, observations, handles, outcome *ingressCaptureOutput
}

func newIngressTestPorts() *ingressTestPorts {
	return &ingressTestPorts{
		retain:       &ingressCaptureOutput{name: "retain", typeOf: mediaelements.RetainRequestType()},
		release:      &ingressCaptureOutput{name: "release", typeOf: mediaelements.ReleaseRequestType()},
		storeCancel:  &ingressCaptureOutput{name: "store_cancel", typeOf: mediaelements.StoreCancelType()},
		observations: &ingressCaptureOutput{name: "observations", typeOf: observationType},
		handles:      &ingressCaptureOutput{name: "handles", typeOf: attachmentHandleType},
		outcome:      &ingressCaptureOutput{name: "outcome", typeOf: ingressOutcomeType},
	}
}

func newIngressTestRunner(ports *ingressTestPorts) *userContentRunner {
	return &userContentRunner{
		instance: "content", config: UserContentConfig{
			Observer: "client", TextSource: "text", AttachmentSource: "message",
			RetentionScope: "session", MaxTextBytes: 1024, MaxMetadataBytes: 4096,
			MaxInputBytes: 1024, MaxPending: 8, MaxPendingBytes: 8192,
			MaxStreams: 16, TerminalMemory: 32,
		},
		clock: ingressTestClock{now: 10}, sequences: graphruntime.NewSequenceAllocator(),
		ports: userContentPorts{
			retain: ports.retain, release: ports.release, storeCancel: ports.storeCancel,
			observations: ports.observations, handles: ports.handles, outcome: ports.outcome,
		},
		pending: make(map[string]pendingContent), pendingByReply: make(map[string]string),
		pendingByContent: make(map[string]string), pendingByItem: make(map[string]string),
		pendingByStream: make(map[string]string), streams: make(map[string]revisionState),
		terminal: make(map[string]struct{}), preCanceled: make(map[string]string),
	}
}
