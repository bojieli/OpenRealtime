package cognition_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

func TestCompactVisualProjectionKeepsTheLatestFastComputerActionAndResult(t *testing.T) {
	items := []trajectory.Item{
		{ID: "task", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "acknowledge the alert"},
		{ID: "old-call", Kind: trajectory.KindToolCall, Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
			ToolCall: &trajectory.ToolCall{CallID: "old", Name: "computer.click_element"}},
		{ID: "call", Kind: trajectory.KindToolCall, Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
			ToolCall: &trajectory.ToolCall{CallID: "current", Name: "computer.click_element"}},
		{ID: "result", Kind: trajectory.KindToolResult,
			ToolResult: &trajectory.ToolResult{CallID: "current", Name: "computer.click_element"}},
		visualObservation("screen", 2, trajectory.AuthorityObserver, "screen", "current-screen"),
	}
	projected, err := cognition.CompactVisualProjection(trajectory.Snapshot{Items: items})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	want := []string{"task", "call", "result", "screen"}
	if len(projected.Items) != len(want) {
		t.Fatalf("projection did not retain latest action state: %+v", projected.Items)
	}
	for index, item := range projected.Items {
		if item.ID != want[index] {
			t.Fatalf("projection item %d = %q, want %q", index, item.ID, want[index])
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
			if provider.seen == nil || len(provider.seen.Invocation.Tools) != 1 ||
				len(provider.seen.Invocation.Capabilities) == 0 || provider.seen.Invocation.MaxOutputTokens != 96 {
				t.Fatalf("reflex did not receive the narrow invocation: %#v", provider.seen)
			}
		})
	}
}

func TestVisualReflexCapabilityManifestMatchesItsOfferedTools(t *testing.T) {
	provider := reflexProvider(continuation.Event{
		Kind: continuation.EventAssistantDelta, Text: "ABSTAIN",
	})
	store := trajectory.NewStore()
	seed(t, store)
	tools := []continuation.ToolDefinition{
		{Name: "computer.click", Description: "click", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "analyze_report", Description: "analyze", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slowProvider(), Catalog: listedCatalog{tools: tools},
		VisualReflex: &cognition.VisualReflexConfig{
			Provider: provider,
			ToolFilter: func(tool continuation.ToolDefinition) bool {
				return tool.Name == "computer.click"
			},
		},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if provider.seen == nil || len(provider.seen.Invocation.Tools) != 1 ||
		provider.seen.Invocation.Tools[0].Name != "computer.click" ||
		len(provider.seen.Invocation.Capabilities) != 1 ||
		provider.seen.Invocation.Capabilities[0].Name != "computer.click" {
		t.Fatalf("visual reflex invocation escaped its role surface: %#v", provider.seen)
	}
}

func TestVisualReflexCurrentTaskReplacesAStaleCanonicalUserCommand(t *testing.T) {
	provider := reflexProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "WAIT"})
	engine, _ := reflexEngine(t, provider, time.Second)
	_, err := engine.RunVisualReflex(context.Background(), cognition.Request{
		SourceRevision: 2,
		VisualTask:     "Wait, go back to Overview.",
		Heard:          "Wait, go back to Overview.",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if provider.seen == nil {
		t.Fatal("visual provider did not receive a request")
	}
	userTasks := 0
	for _, item := range provider.seen.Trajectory.Items {
		if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			userTasks++
			if item.Content != "Wait, go back to Overview." {
				t.Fatalf("stale canonical user command competed with reconstructed task: %+v", item)
			}
		}
	}
	if userTasks != 1 {
		t.Fatalf("visual projection retained %d current user tasks, want one", userTasks)
	}
	if !strings.Contains(provider.seen.Invocation.Instruction, "Wait, go back to Overview.") {
		t.Fatalf("current reconstructed task was not supplied: %q", provider.seen.Invocation.Instruction)
	}
}

func TestVisualReflexExtractsPrivateReplanStateBeforeCommittingTheEffect(t *testing.T) {
	provider := reflexProvider(continuation.Event{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "call-continue", Name: "get_balance",
			Arguments: json.RawMessage(`{"account":"A1","_openrealtime_continue":true,"_openrealtime_target":"Summary"}`),
		},
	})
	engine, store := reflexEngine(t, provider, time.Second)
	outcome, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if outcome.Kind != cognition.VisualReflexAct || !outcome.Continue || outcome.Target != "Summary" {
		t.Fatalf("typed action state = kind %q continue %t target %q", outcome.Kind, outcome.Continue, outcome.Target)
	}
	if provider.seen == nil || len(provider.seen.Invocation.Tools) != 1 {
		t.Fatalf("provider missed action schema: %#v", provider.seen)
	}
	var schema map[string]any
	if err := json.Unmarshal(provider.seen.Invocation.Tools[0].Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, declared := properties["_openrealtime_continue"]; !declared {
		t.Fatalf("model-only action schema omitted controller state: %s", provider.seen.Invocation.Tools[0].Parameters)
	}
	if _, declared := properties["_openrealtime_target"]; !declared {
		t.Fatalf("model-only action schema omitted grounded target evidence: %s", provider.seen.Invocation.Tools[0].Parameters)
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind != trajectory.KindToolCall || item.ToolCall == nil {
			continue
		}
		if strings.Contains(string(item.ToolCall.Arguments), "_openrealtime_continue") {
			t.Fatalf("private controller state leaked into canonical effect arguments: %s", item.ToolCall.Arguments)
		}
		if string(item.ToolCall.Arguments) != `{"account":"A1"}` {
			t.Fatalf("clean effect arguments = %s", item.ToolCall.Arguments)
		}
	}
}

func TestVisualReflexStripsPrivateReplanStateDecoderAliases(t *testing.T) {
	for _, key := range []string{"openrealtime_continue", " _openrealtime_continue"} {
		t.Run(key, func(t *testing.T) {
			provider := reflexProvider(continuation.Event{
				Kind: continuation.EventToolCall,
				ToolCall: &trajectory.ToolCall{
					CallID: "call-alias", Name: "get_balance",
					Arguments: json.RawMessage(`{"account":"A1","` + key + `":true}`),
				},
			})
			engine, store := reflexEngine(t, provider, time.Second)
			outcome, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if outcome.Kind != cognition.VisualReflexAct || !outcome.Continue {
				t.Fatalf("typed action state = kind %q continue %t", outcome.Kind, outcome.Continue)
			}
			for _, item := range store.Snapshot().Items {
				if item.Kind == trajectory.KindToolCall && item.ToolCall != nil &&
					string(item.ToolCall.Arguments) != `{"account":"A1"}` {
					t.Fatalf("decoder alias crossed into effect arguments: %s", item.ToolCall.Arguments)
				}
			}
		})
	}
}

func TestVisualReflexExtractsQwenClickTargetAlias(t *testing.T) {
	provider := reflexProvider(continuation.Event{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "click-alias", Name: "computer.click_normalized",
			Arguments: json.RawMessage(`{"source":"screen","x":350,"y":810,"label":"Overview","_openrealtime_continue":false}`),
		},
	})
	store := trajectory.NewStore()
	seed(t, store)
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slowProvider(), Catalog: listedCatalog{tools: []continuation.ToolDefinition{{
			Name: "computer.click_normalized", Description: "click a visible point in normalized image coordinates",
			Parameters: json.RawMessage(`{"type":"object","properties":{"source":{"type":"string"},"x":{"type":"number"},"y":{"type":"number"}},"required":["source","x","y"]}`),
		}}},
		VisualReflex: &cognition.VisualReflexConfig{
			Provider: provider, Timeout: time.Second,
			ToolFilter: func(tool continuation.ToolDefinition) bool { return true },
		},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	outcome, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if outcome.Target != "Overview" {
		t.Fatalf("Qwen click label alias target = %q", outcome.Target)
	}
	if provider.seen == nil || len(provider.seen.Invocation.Tools) != 1 {
		t.Fatalf("provider missed click schema: %#v", provider.seen)
	}
	var schema map[string]any
	if err := json.Unmarshal(provider.seen.Invocation.Tools[0].Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, declared := properties["label"]; !declared {
		t.Fatalf("click schema omitted decoder-native private label: %s", provider.seen.Invocation.Tools[0].Parameters)
	}
	required, _ := schema["required"].([]any)
	if !slices.Contains(required, any("label")) {
		t.Fatalf("click schema did not require private label: %s", provider.seen.Invocation.Tools[0].Parameters)
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall && item.ToolCall != nil {
			if got := string(item.ToolCall.Arguments); got != `{"source":"screen","x":350,"y":810}` {
				t.Fatalf("private label alias leaked into click effect: %s", got)
			}
		}
	}
}

func TestVisualReflexReceivesTheLiveUncommittedUtterance(t *testing.T) {
	provider := reflexProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "WAIT"})
	engine, _ := reflexEngine(t, provider, time.Second)
	_, err := engine.RunVisualReflex(context.Background(), cognition.Request{
		SourceRevision: 1, Heard: "click the acknowledge alert button now",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if provider.seen == nil || !strings.Contains(
		provider.seen.Invocation.Instruction, "click the acknowledge alert button now",
	) {
		t.Fatalf("visual actor did not receive live speech: %#v", provider.seen)
	}
}

func TestVisualReflexReceivesTheLiveSessionInstruction(t *testing.T) {
	provider := reflexProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "WAIT"})
	store := trajectory.NewStore()
	seed(t, store)
	engine, err := cognition.New(cognition.Config{
		Store: store, Fast: fastProvider(), Slow: slowProvider(), Catalog: catalog{},
		AgentInstruction: "Never share the screen unless the user explicitly requests it.",
		VisualReflex: &cognition.VisualReflexConfig{
			Provider: provider, Timeout: time.Second,
			ToolFilter: func(tool continuation.ToolDefinition) bool { return tool.Name == "get_balance" },
		},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if _, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1}); err != nil {
		t.Fatalf("run initial instruction: %v", err)
	}
	if provider.seen == nil || !strings.Contains(provider.seen.Invocation.Instruction, "Never share the screen") {
		t.Fatalf("visual actor missed the initial session contract: %#v", provider.seen)
	}

	engine.SetAgentInstruction("Acknowledge a visible deployment alert immediately.")
	if _, err := engine.RunVisualReflex(context.Background(), cognition.Request{SourceRevision: 1}); err != nil {
		t.Fatalf("run updated instruction: %v", err)
	}
	if !strings.Contains(provider.seen.Invocation.Instruction, "Acknowledge a visible deployment alert immediately") ||
		strings.Contains(provider.seen.Invocation.Instruction, "Never share the screen") {
		t.Fatalf("visual actor did not receive the replaced session contract: %q", provider.seen.Invocation.Instruction)
	}
}

func TestVisualReflexReceivesTypedWorkInFlight(t *testing.T) {
	provider := reflexProvider(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "WAIT"})
	engine, _ := reflexEngine(t, provider, time.Second)
	_, err := engine.RunVisualReflex(context.Background(), cognition.Request{
		SourceRevision: 1, Heard: "analyze the review", InFlight: []string{"meeting.analyze_launch_review"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if provider.seen == nil || !strings.Contains(
		provider.seen.Invocation.Instruction, "Work already in flight: meeting.analyze_launch_review",
	) {
		t.Fatalf("visual actor did not receive in-flight work: %#v", provider.seen)
	}
}

func TestCompactVisualProjectionRetainsSeveralRecentActionChunks(t *testing.T) {
	snapshot := trajectory.Snapshot{Version: 8, Items: []trajectory.Item{
		{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "open the review, then share"},
		{ID: "call-open", Kind: trajectory.KindToolCall, InvocationID: "open", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, ToolCall: &trajectory.ToolCall{CallID: "open-1", Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":100,"y":800}`)}},
		{ID: "result-open", Kind: trajectory.KindToolResult, InvocationID: "open", Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &trajectory.ToolResult{CallID: "open-1", Name: "computer.click_normalized", Output: json.RawMessage(`{"ok":true}`)}},
		{ID: "frame", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseObserver}, Content: "new screen", Observation: &trajectory.ObservationMeta{Observer: "video", Authority: trajectory.AuthorityObserver, Media: []trajectory.MediaRef{{Handle: "frame", MIMEType: "image/jpeg", Source: "screen"}}}},
		{ID: "call-share", Kind: trajectory.KindToolCall, InvocationID: "share", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, ToolCall: &trajectory.ToolCall{CallID: "share-1", Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":100,"y":600}`)}},
		{ID: "result-share", Kind: trajectory.KindToolResult, InvocationID: "share", Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &trajectory.ToolResult{CallID: "share-1", Name: "computer.click_normalized", Output: json.RawMessage(`{"ok":true}`)}},
		{ID: "assistant", Kind: trajectory.KindAssistant, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "not visual state"},
		{ID: "reasoning", Kind: trajectory.KindReasoning, Producer: trajectory.Producer{Phase: trajectory.PhaseSlow}, Content: "not visual state"},
	}}
	projected, err := cognition.CompactVisualProjection(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range projected.Items {
		seen[item.ID] = true
	}
	for _, required := range []string{"user", "call-open", "result-open", "frame", "call-share", "result-share"} {
		if !seen[required] {
			t.Fatalf("projection dropped recent action state %q: %+v", required, projected.Items)
		}
	}
	if seen["assistant"] || seen["reasoning"] {
		t.Fatalf("projection retained conversational history: %+v", projected.Items)
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
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindToolCall || item.Kind == trajectory.KindToolProposal {
			t.Fatalf("filtered visual tool entered shared action state: %#v", item)
		}
	}
	if provider.seen == nil || len(provider.seen.Invocation.Capabilities) != 1 ||
		provider.seen.Invocation.Capabilities[0].Name != "get_balance" {
		t.Fatalf("visual reflex saw capabilities outside its offered tools: %#v", provider.seen)
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
