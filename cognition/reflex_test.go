package cognition_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func reflexProvider(events ...continuation.Event) *scriptedProvider {
	return &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "test-reflex", Model: "vision", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityExecute,
			SpeechAuthority: continuation.SpeechAuthoritySilent, Vision: true,
		},
		events: events,
	}
}

func reflexEngine(t *testing.T, provider continuation.Provider, timeout time.Duration) (*cognition.Engine, *trajectory.Store) {
	t.Helper()
	store := trajectory.NewStore()
	seed(t, store)
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slowProvider(), Catalog: catalog{},
		VisualReflex: &cognition.VisualReflexConfig{
			Provider: provider, Timeout: timeout,
			ToolFilter: func(tool continuation.ToolDefinition) bool { return tool.Name == "get_balance" },
		},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine, store
}

func TestVoiceOnlyEngineDoesNotInstantiateTheVisualReflex(t *testing.T) {
	engine, err := cognition.New(cognition.Config{
		Store: trajectory.NewStore(), Fast: fastProvider(), Slow: slowProvider(),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, enabled := engine.VisualReflexDescriptor(); enabled {
		t.Fatal("the zero-value voice configuration instantiated a visual reflex")
	}
	if _, err := engine.RunVisualReflex(context.Background(), cognition.Request{}); !errors.Is(err, cognition.ErrVisualReflexDisabled) {
		t.Fatalf("disabled reflex returned %v", err)
	}
}

func TestCompactVisualProjectionKeepsOnlyCurrentTaskAndMediaPerSource(t *testing.T) {
	items := []trajectory.Item{
		visualObservation("old-screen", 1, trajectory.AuthorityObserver, "screen", "old"),
		{ID: "old-user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "old request"},
		{ID: "assistant", Kind: trajectory.KindAssistant, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "history"},
		{ID: "task", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "click submit"},
		visualObservation("screen", 2, trajectory.AuthorityObserver, "screen", "current-screen"),
		visualObservation("camera", 3, trajectory.AuthorityObserver, "camera", "current-camera"),
	}
	projected, err := cognition.CompactVisualProjection(trajectory.Snapshot{Version: 99, Items: items})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if projected.Version != 99 || len(projected.Items) != 3 {
		t.Fatalf("unexpected compact projection: %#v", projected)
	}
	want := []string{"task", "screen", "camera"}
	for index, item := range projected.Items {
		if item.ID != want[index] {
			t.Fatalf("projection item %d = %q, want %q", index, item.ID, want[index])
		}
		if len(item.CausalParentIDs) != 0 || len(item.ProviderState) != 0 {
			t.Fatalf("compact item retained unrelated continuation state: %#v", item)
		}
	}
}

func visualObservation(id string, revision uint64, authority trajectory.Authority, source, handle string) trajectory.Item {
	phase := trajectory.PhaseObserver
	if authority == trajectory.AuthorityUser {
		phase = trajectory.PhaseUser
	}
	return trajectory.Item{
		ID: id, Kind: trajectory.KindObservation, SourceRevision: revision,
		Producer: trajectory.Producer{Phase: phase}, Content: id,
		Observation: &trajectory.ObservationMeta{
			Observer: "video", Source: source, Authority: authority,
			Media: []trajectory.MediaRef{{Handle: handle, MIMEType: "image/jpeg", Source: source}},
		},
	}
}

func TestVisualReflexReturnsDistinctTypedOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		events []continuation.Event
		want   cognition.VisualReflexKind
	}{
		{
			name: "act", want: cognition.VisualReflexAct,
			events: []continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "call-1", Name: "get_balance", Arguments: json.RawMessage(`{}`),
			}}},
		},
		{name: "wait", want: cognition.VisualReflexWait, events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "WAIT"}}},
		{name: "abstain", want: cognition.VisualReflexAbstain, events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "ABSTAIN"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := reflexProvider(test.events...)
			engine, store := reflexEngine(t, provider, time.Second)
			outcome, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if outcome.Kind != test.want {
				t.Fatalf("got %q, want %q", outcome.Kind, test.want)
			}
			calls := 0
			assistants := 0
			for _, item := range store.Snapshot().Items {
				if item.Kind == trajectory.KindToolCall {
					calls++
				}
				if item.Kind == trajectory.KindAssistant {
					assistants++
				}
			}
			if calls != map[cognition.VisualReflexKind]int{cognition.VisualReflexAct: 1}[test.want] {
				t.Fatalf("%s committed %d calls", test.want, calls)
			}
			if assistants != 0 {
				t.Fatalf("control output leaked into shared assistant state")
			}
			if provider.seen == nil || len(provider.seen.Invocation.Tools) != 1 || provider.seen.Invocation.MaxOutputTokens != 48 {
				t.Fatalf("reflex did not receive the narrow invocation: %#v", provider.seen)
			}
		})
	}
}

func TestMalformedVisualReflexSafelyAbstainsWithoutACommittedCall(t *testing.T) {
	provider := reflexProvider(
		continuation.Event{Kind: continuation.EventAssistantDelta, Text: "I think "},
		continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call-1", Name: "get_balance", Arguments: json.RawMessage(`{}`),
		}},
	)
	engine, store := reflexEngine(t, provider, time.Second)
	outcome, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1})
	if !errors.Is(err, cognition.ErrMalformedVisualReflex) || outcome.Kind != cognition.VisualReflexAbstain {
		t.Fatalf("malformed output got outcome=%q err=%v", outcome.Kind, err)
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			t.Fatalf("malformed reflex committed an executable call: %#v", item)
		}
	}
}

func TestVisualReflexCannotPromoteAnUnfilteredTool(t *testing.T) {
	provider := reflexProvider(continuation.Event{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "call-1", Name: "transfer_funds", Arguments: json.RawMessage(`{}`),
		},
	})
	engine, store := reflexEngine(t, provider, time.Second)
	outcome, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1})
	if !errors.Is(err, cognition.ErrMalformedVisualReflex) || outcome.Kind != cognition.VisualReflexAbstain {
		t.Fatalf("filtered call got outcome=%q err=%v", outcome.Kind, err)
	}
	proposal := false
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			t.Fatalf("filtered tool became executable: %#v", item)
		}
		proposal = proposal || item.Kind == trajectory.KindToolProposal
	}
	if !proposal {
		t.Fatal("the rejected model output was not retained as a diagnosable proposal")
	}
}

type blockingReflexProvider struct{ descriptor continuation.Descriptor }

func (provider blockingReflexProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}
func (provider blockingReflexProvider) Continue(ctx context.Context, _ continuation.Request, _ continuation.Emit) (continuation.Completion, error) {
	<-ctx.Done()
	return continuation.Completion{}, ctx.Err()
}

func TestVisualReflexHasAHardTimeout(t *testing.T) {
	provider := blockingReflexProvider{descriptor: reflexProvider().descriptor}
	engine, store := reflexEngine(t, provider, 10*time.Millisecond)
	started := time.Now()
	outcome, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1})
	if !errors.Is(err, context.DeadlineExceeded) || outcome.Kind != cognition.VisualReflexAbstain {
		t.Fatalf("timeout got outcome=%q err=%v", outcome.Kind, err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("hard timeout took %s", elapsed)
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall {
			t.Fatal("timed-out reflex committed an action")
		}
	}
}
