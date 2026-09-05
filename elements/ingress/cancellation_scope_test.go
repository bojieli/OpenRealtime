package ingress

import (
	"context"
	"fmt"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
)

func TestIngressCancellationKeepsContentStreamAndSessionScopesDistinct(t *testing.T) {
	for _, test := range []struct {
		name                     string
		cancel                   ContentCancel
		session, content, stream string
		want                     OutcomeKind
	}{
		{"stream-is-not-content", ContentCancel{StreamID: "shared-id"}, "session-a", "shared-id", "fresh", OutcomeSucceeded},
		{"content-is-not-stream", ContentCancel{ContentID: "shared-id"}, "session-a", "fresh", "shared-id", OutcomeSucceeded},
		{"stream-other-session", ContentCancel{StreamID: "shared-id"}, "session-b", "fresh", "shared-id", OutcomeSucceeded},
		{"content-other-session", ContentCancel{ContentID: "shared-id"}, "session-b", "shared-id", "fresh", OutcomeSucceeded},
		{"content-same-session", ContentCancel{ContentID: "shared-id"}, "session-a", "shared-id", "fresh", OutcomeCanceled},
		{"stream-same-session", ContentCancel{StreamID: "shared-id"}, "session-a", "fresh", "shared-id", OutcomeCanceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ports := newIngressTestPorts()
			runner := newIngressTestRunner(ports)
			if err := runner.cancel(t.Context(), element.Envelope{
				ItemID: "cancel", SessionID: "session-a", Payload: test.cancel,
			}); err != nil {
				t.Fatal(err)
			}
			if err := runner.text(t.Context(), element.Envelope{
				ItemID: "request", SessionID: test.session, Payload: UserText{
					ContentID: test.content, StreamID: test.stream, Text: "hello", Revision: 1,
				},
			}); err != nil {
				t.Fatal(err)
			}
			if outcome := ports.outcome.last().Payload.(Outcome); outcome.Kind != test.want {
				t.Fatalf("cancellation scope = %+v, want %s", outcome, test.want)
			}
			wantObservations := 1
			if test.want == OutcomeCanceled {
				wantObservations = 0
			}
			if len(ports.observations.values) != wantObservations {
				t.Fatalf("consumer observations = %d, want %d", len(ports.observations.values), wantObservations)
			}
		})
	}
}

func TestIngressStreamCancellationDuringRetentionSurvivesCompletion(t *testing.T) {
	for _, address := range []string{"stream", "content-and-stream", "stream-with-run-envelope"} {
		t.Run(address, func(t *testing.T) {
			ports := newIngressTestPorts()
			runner := newIngressTestRunner(ports)
			if err := runner.attachment(t.Context(), "image", ingressImageEnvelope("initial", "session-a", "image-content", "withdrawn")); err != nil {
				t.Fatal(err)
			}
			retain := ports.retain.last()
			cancel := element.Envelope{ItemID: "cancel", SessionID: "session-a"}
			request := ContentCancel{StreamID: "withdrawn"}
			if address == "content-and-stream" {
				request.ContentID = "image-content"
			}
			if address == "stream-with-run-envelope" {
				cancel.RunID = "unrelated-envelope-run"
			}
			cancel.Payload = request
			if err := runner.cancel(t.Context(), cancel); err != nil {
				t.Fatal(err)
			}
			if outcome := ports.outcome.last().Payload.(Outcome); outcome.Code != "cancel_forwarded" {
				t.Fatalf("pending cancellation = %+v", outcome)
			}
			if forwarded := ports.storeCancel.last().Payload.(mediaelements.StoreCancel); forwarded.RequestID != retain.ItemID {
				t.Fatalf("wrong retain canceled: %+v", forwarded)
			}
			if err := runner.retained(t.Context(), element.Envelope{
				ItemID: retain.ItemID + ":retained", SessionID: "session-a", CausalParents: []string{retain.ItemID},
				Payload: mediaelements.RetainResult{Kind: mediaelements.ResultCanceled},
			}); err != nil {
				t.Fatal(err)
			}
			for revision := uint64(2); revision <= 3; revision++ {
				if err := runner.text(t.Context(), element.Envelope{
					ItemID: fmt.Sprintf("late-%d", revision), SessionID: "session-a",
					Payload: UserText{StreamID: "withdrawn", Text: "late revision", Revision: revision},
				}); err != nil {
					t.Fatal(err)
				}
				if outcome := ports.outcome.last().Payload.(Outcome); outcome.Kind != OutcomeCanceled {
					t.Fatalf("revision %d restarted canceled stream: %+v", revision, outcome)
				}
			}
			if len(ports.observations.values) != 0 || len(ports.handles.values) != 0 {
				t.Fatal("canceled stream published content")
			}
		})
	}
}

func TestIngressCancellationMemoryIsBoundedAndContentCancellationIsConsumed(t *testing.T) {
	ports := newIngressTestPorts()
	runner := newIngressTestRunner(ports)
	runner.config.TerminalMemory = 2
	for _, stream := range []string{"evicted", "retained-a", "retained-b"} {
		if err := runner.cancel(context.Background(), element.Envelope{ItemID: "cancel-" + stream, Payload: ContentCancel{StreamID: stream}}); err != nil {
			t.Fatal(err)
		}
	}
	for index, stream := range []string{"evicted", "retained-a", "retained-a", "retained-b"} {
		if err := runner.text(t.Context(), element.Envelope{ItemID: fmt.Sprintf("request-%d", index), Payload: UserText{StreamID: stream, Text: "text", Revision: uint64(index + 1)}}); err != nil {
			t.Fatal(err)
		}
		want := OutcomeCanceled
		if index == 0 {
			want = OutcomeSucceeded
		}
		if outcome := ports.outcome.last().Payload.(Outcome); outcome.Kind != want {
			t.Fatalf("stream %s = %+v, want %s", stream, outcome, want)
		}
	}
	if len(runner.preCanceled) != 2 {
		t.Fatalf("retained cancellation count = %d", len(runner.preCanceled))
	}
	if err := runner.cancel(t.Context(), element.Envelope{ItemID: "cancel-content", Payload: ContentCancel{ContentID: "one-content"}}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := runner.text(t.Context(), element.Envelope{ItemID: fmt.Sprintf("content-%d", index), Payload: UserText{ContentID: "one-content", StreamID: "fresh", Text: "text", Revision: uint64(index + 1)}}); err != nil {
			t.Fatal(err)
		}
		want := OutcomeCanceled
		if index == 1 {
			want = OutcomeSucceeded
		}
		if outcome := ports.outcome.last().Payload.(Outcome); outcome.Kind != want {
			t.Fatalf("content request %d = %+v, want %s", index, outcome, want)
		}
	}
}

func TestIngressExplicitStreamCancellationIgnoresUnrelatedRunAddress(t *testing.T) {
	ports := newIngressTestPorts()
	runner := newIngressTestRunner(ports)
	if err := runner.attachment(t.Context(), "image", ingressImageEnvelope("pending", "session-a", "unrelated-content", "unrelated-stream")); err != nil {
		t.Fatal(err)
	}
	if err := runner.cancel(t.Context(), element.Envelope{
		ItemID: "cancel", SessionID: "session-a", RunID: "unrelated-content",
		Payload: ContentCancel{StreamID: "withdrawn"},
	}); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(Outcome); outcome.Code != "cancel_recorded" || len(ports.storeCancel.values) != 0 {
		t.Fatalf("stream cancellation touched unrelated pending work: %+v", outcome)
	}
	for revision := uint64(1); revision <= 2; revision++ {
		if err := runner.text(t.Context(), element.Envelope{
			ItemID: fmt.Sprintf("late-%d", revision), SessionID: "session-a",
			Payload: UserText{StreamID: "withdrawn", Text: "late", Revision: revision},
		}); err != nil {
			t.Fatal(err)
		}
		if outcome := ports.outcome.last().Payload.(Outcome); outcome.Kind != OutcomeCanceled {
			t.Fatalf("explicit stream cancellation lost to envelope run: %+v", outcome)
		}
	}
}

func TestIngressPendingCancellationDoesNotCrossSessions(t *testing.T) {
	for _, request := range []ContentCancel{{StreamID: "image-stream"}, {ContentID: "image-content"}} {
		ports := newIngressTestPorts()
		runner := newIngressTestRunner(ports)
		if err := runner.attachment(t.Context(), "image", ingressImageEnvelope("pending", "session-a", "image-content", "image-stream")); err != nil {
			t.Fatal(err)
		}
		if err := runner.cancel(t.Context(), element.Envelope{ItemID: "foreign-cancel", SessionID: "session-b", Payload: request}); err != nil {
			t.Fatal(err)
		}
		if outcome := ports.outcome.last().Payload.(Outcome); outcome.Code != "cancel_recorded" || len(ports.storeCancel.values) != 0 {
			t.Fatalf("foreign cancellation touched pending work: %+v", outcome)
		}
	}
}

func TestIngressCombinedCancellationKeepsStreamInOneSlotMemory(t *testing.T) {
	ports := newIngressTestPorts()
	runner := newIngressTestRunner(ports)
	runner.config.TerminalMemory = 1
	if err := runner.cancel(t.Context(), element.Envelope{
		ItemID: "cancel", Payload: ContentCancel{ContentID: "content", StreamID: "withdrawn"},
	}); err != nil {
		t.Fatal(err)
	}
	for revision := uint64(1); revision <= 2; revision++ {
		if err := runner.text(t.Context(), element.Envelope{
			ItemID:  fmt.Sprintf("late-%d", revision),
			Payload: UserText{ContentID: "content", StreamID: "withdrawn", Text: "late", Revision: revision},
		}); err != nil {
			t.Fatal(err)
		}
		if outcome := ports.outcome.last().Payload.(Outcome); outcome.Kind != OutcomeCanceled {
			t.Fatalf("combined cancellation lost its stream: %+v", outcome)
		}
	}
}
