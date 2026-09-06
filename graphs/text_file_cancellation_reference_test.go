package graphs_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Cancellation names a whole content stream. Its first delayed hypothesis
// must not consume cancellation and let the final hypothesis invoke the model.
func TestTextFileCognitionCanceledStreamCannotReactivateOnFinalRevision(t *testing.T) {
	reference := loadTextFileCognitionReference(t)
	provider := &textFileReferenceProvider{descriptor: textFileReferenceDescriptor()}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("deployment.text-file", provider.descriptor,
		func() (continuation.Provider, error) { return provider, nil }); err != nil {
		t.Fatal(err)
	}
	store := trajectory.NewStore()
	services := graphruntime.NewServiceSet()
	for name, value := range map[string]any{
		cognitionelements.ProviderRegistryService: providers,
		stateelements.TrajectoryStoreService: &stateelements.TrajectoryStoreServiceValue{
			Store: store, SessionID: "canceled-content-session",
		},
	} {
		if _, err := services.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var now atomic.Uint64
	mounted, err := graphruntime.Mount(t.Context(), graphruntime.Config{
		Graph: reference.bound.Graph, Values: reference.bound.Values,
		Registry: registry, Services: services, Now: func() uint64 { return now.Add(1) },
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("text/file graph shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("text/file graph did not stop")
		}
	})
	for _, boundary := range reference.bound.Graph.Boundaries {
		if boundary.Direction != ir.OutputBoundary {
			continue
		}
		switch boundary.Name {
		case "activation_outcome", "prepared_text", "model_result", "model_commit_outcome":
		default:
			textFileDrain(t, ctx, mounted, boundary.Name)
		}
	}
	textFileInstallInvocation(t, mounted, "canceled-content-session", 1, "Answer from committed participant text and resolve retained files when relevant.")
	textFileSend(t, mounted, "activation_cancel", element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "cancel-content-stream",
		SessionID: "canceled-content-session", Payload: policyelements.GenerationCancel{
			StreamID: "withdrawn-request", Reason: "request withdrawn",
		},
	})
	nextActivation := func() policyelements.SessionInvocationOutcome {
		t.Helper()
		return textFileReceive(t, mounted, "activation_outcome").Payload.(policyelements.SessionInvocationOutcome)
	}
	if outcome := nextActivation(); outcome.Code != "cancel_recorded" {
		t.Fatalf("cancellation = %+v", outcome)
	}
	for revision := uint64(1); revision <= 3; revision++ {
		textFileSend(t, mounted, "text", element.Envelope{
			Type: ingresselements.UserTextType(), ItemID: fmt.Sprintf("withdrawn-input-%d", revision),
			SessionID: "canceled-content-session", SourceID: "participant",
			Payload: ingresselements.UserText{
				ContentID: "withdrawn-message", StreamID: "withdrawn-request",
				Text: "Read the report.", Revision: revision, Supersedes: revision - 1,
				Final: revision == 3,
			},
		})
		if outcome := nextActivation(); outcome.Kind != policyelements.SessionInvocationCanceled || outcome.StreamID != "withdrawn-request" {
			t.Fatalf("canceled content revision %d reactivated cognition: %+v", revision, outcome)
		}
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("canceled content invoked the provider %d times", calls)
	}
	textFileSend(t, mounted, "text", element.Envelope{
		Type: ingresselements.UserTextType(), ItemID: "replacement-input",
		SessionID: "canceled-content-session", SourceID: "participant",
		Payload: ingresselements.UserText{
			ContentID: "replacement-message", StreamID: "replacement-request",
			Text: "Read the report I send next.", Revision: 1, Final: true,
		},
	})
	if outcome := nextActivation(); outcome.Kind != policyelements.SessionInvocationEmitted || outcome.StreamID != "replacement-request" {
		t.Fatalf("replacement activation = %+v", outcome)
	}
	var answer string
	for range 3 {
		delta := textFileReceive(t, mounted, "prepared_text").Payload.(cognitionelements.PreparedTextDelta)
		answer += delta.Text
	}
	result := textFileReceive(t, mounted, "model_result").Payload.(cognitionelements.Result)
	if answer != "Text received; send the retained report." || result.AssistantText != answer || provider.calls.Load() != 1 {
		t.Fatalf("replacement response: text=%q result=%+v calls=%d", answer, result, provider.calls.Load())
	}
	for {
		outcome := textFileReceive(t, mounted, "model_commit_outcome").Payload.(interactionelements.ModelCommitOutcome)
		if outcome.Kind == interactionelements.ModelCommitted {
			break
		}
		if outcome.Kind != interactionelements.ModelIgnored {
			t.Fatalf("replacement response did not commit: %+v", outcome)
		}
	}
	assistants := 0
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindAssistant {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("canonical assistant responses = %d, want only the replacement response", assistants)
	}
}
