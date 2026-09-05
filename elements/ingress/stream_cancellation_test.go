package ingress_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
)

func TestMultimodalStreamCancellationSurvivesLaterRevisions(t *testing.T) {
	for _, modality := range []string{"text", "image", "file", "attachment"} {
		t.Run(modality, func(t *testing.T) {
			mounted, done, cancel := mountReference(t, referenceValues(t))
			defer stopReference(t, mounted, done, cancel)
			_ = receive(t, egress(t, mounted, "retention_metrics"))
			outcomes := egress(t, mounted, "ingress_outcome")
			send(t, ingress(t, mounted, "content_cancel"), element.Envelope{
				Type: ingresselements.ContentCancelType(), ItemID: "cancel-stream", SessionID: "session-a",
				Payload: ingresselements.ContentCancel{StreamID: "withdrawn", Reason: "request withdrawn"},
			})
			if outcome := receive(t, outcomes).Payload.(ingresselements.Outcome); outcome.Code != "cancel_recorded" {
				t.Fatalf("cancel receipt = %+v", outcome)
			}
			for revision := uint64(1); revision <= 3; revision++ {
				envelope := element.Envelope{ItemID: fmt.Sprintf("late-%d", revision), SessionID: "session-a"}
				switch modality {
				case "text":
					envelope.Type = ingresselements.UserTextType()
					envelope.Payload = ingresselements.UserText{
						ContentID: envelope.ItemID, StreamID: "withdrawn", Text: "withdrawn text",
						Revision: revision, Final: revision == 3,
					}
				case "image":
					envelope.Type = ingresselements.UserImageType()
					envelope.Payload = ingresselements.UserImage{
						ContentID: envelope.ItemID, StreamID: "withdrawn", MIMEType: "image/png",
						Content: []byte("image"), Width: 1, Height: 1, SourceRevision: revision,
					}
				case "file":
					envelope.Type = ingresselements.UserFileType()
					envelope.Payload = ingresselements.UserFile{
						ContentID: envelope.ItemID, StreamID: "withdrawn", MIMEType: "text/plain",
						Content: []byte("file"), Name: "report.txt", SourceRevision: revision,
					}
				case "attachment":
					envelope.Type = ingresselements.UserAttachmentType()
					envelope.Payload = ingresselements.UserAttachment{
						ContentID: envelope.ItemID, StreamID: "withdrawn", MIMEType: "application/octet-stream",
						Content: []byte("attachment"), SourceRevision: revision,
					}
				}
				send(t, ingress(t, mounted, modality), envelope)
				if outcome := receive(t, outcomes).Payload.(ingresselements.Outcome); outcome.Kind != ingresselements.OutcomeCanceled || outcome.SourceRevision != revision {
					t.Fatalf("canceled %s revision %d = %+v", modality, revision, outcome)
				}
			}
			for _, control := range []struct{ session, content, stream string }{
				{"session-a", "fresh-content", "replacement"},
				{"session-b", "other-session-content", "withdrawn"},
				{"session-a", "withdrawn", "content-is-not-stream"},
			} {
				send(t, ingress(t, mounted, "text"), element.Envelope{
					Type: ingresselements.UserTextType(), ItemID: "fresh-" + control.content,
					SessionID: control.session, Payload: ingresselements.UserText{
						ContentID: control.content, StreamID: control.stream, Text: "fresh text", Revision: 1, Final: true,
					},
				})
				if outcome := receive(t, outcomes).Payload.(ingresselements.Outcome); outcome.Kind != ingresselements.OutcomeSucceeded {
					t.Fatalf("fresh request was suppressed: %+v", outcome)
				}
				observation := receive(t, egress(t, mounted, "observations"))
				if observation.SessionID != control.session || observation.SourceID != control.stream {
					t.Fatalf("unexpected observation reached the consumer: %+v", observation)
				}
			}
			for _, name := range []string{"observations", "handles", "retention_outcome", "retention_metrics"} {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
				unexpected, err := egress(t, mounted, name).Receive(ctx)
				cancel()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("canceled content reached %s: %+v, %v", name, unexpected, err)
				}
			}
		})
	}
}
