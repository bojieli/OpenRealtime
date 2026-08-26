package cascade_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func newVisualReflex(turns ...[]continuation.Event) *scriptedProvider {
	return &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "visual-reflex", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityExecute,
			SpeechAuthority: continuation.SpeechAuthoritySilent, Vision: true,
		},
		turns: turns,
	}
}

func visualReflexVideoConfig() cascade.Config {
	config := videoConfig(nil)
	config.Observers = []perception.Factory{perception.VideoFactory(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: "A dialog is open."}, AttachKeyframes: true,
	})}
	return config
}

func TestVoiceOnlySessionNeverInvokesAConfiguredVisualReflex(t *testing.T) {
	reflex := newVisualReflex([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "WAIT"}})
	runtime, _ := startSession(t, cascade.Config{
		Fast: newFast(), Slow: newSlow(), VisualReflex: reflex,
	}, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
				return true
			}
		}
		return false
	}, "voice observation did not commit")
	time.Sleep(100 * time.Millisecond)
	if reflex.invocations() != 0 {
		t.Fatalf("voice-only input invoked the visual reflex %d times", reflex.invocations())
	}
	if runtime.Status().Reflex != "" {
		t.Fatalf("voice-only session instantiated the visual reflex: %+v", runtime.Status())
	}
}

func TestSessionObserverSelectionConstructsAndRemovesTheVisualReflex(t *testing.T) {
	config := visualReflexVideoConfig()
	config.DefaultObservers = []string{"audio"}
	config.VisualReflex = newVisualReflex()
	runtime, _ := startSession(t, config, binding.Settings{})
	if runtime.Status().Reflex != "" {
		t.Fatalf("audio-only selection instantiated a reflex: %+v", runtime.Status())
	}
	if err := runtime.Update(context.Background(), binding.Settings{
		Observers: []string{"audio", "video"}, Gate: perception.DefaultGateConfig(),
	}); err != nil {
		t.Fatalf("enable video: %v", err)
	}
	if runtime.Status().Reflex != "test/visual-reflex" {
		t.Fatalf("video selection did not instantiate the reflex: %+v", runtime.Status())
	}
	if err := runtime.Update(context.Background(), binding.Settings{
		Observers: []string{"audio"}, Gate: perception.DefaultGateConfig(),
	}); err != nil {
		t.Fatalf("disable video: %v", err)
	}
	if runtime.Status().Reflex != "" {
		t.Fatalf("returning to audio retained the reflex: %+v", runtime.Status())
	}
}

func TestVisualReflexActsFromCompactCurrentContextBeforeTheSlowLane(t *testing.T) {
	dispatched := make(chan trajectory.ToolCall, 1)
	reflex := newVisualReflex([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "visual-click-1", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":40,"y":30}`),
		},
	}})
	config := visualReflexVideoConfig()
	config.VisualReflex = reflex
	config.Tools = []action.ToolSpec{{
		Name: computeruse.Click, Description: "click the current screen",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			dispatched <- call
			return trajectory.ToolResult{CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`)}, nil
		}),
	}}
	runtime, _ := startSession(t, config, binding.Settings{})
	if runtime.Status().Reflex != "test/visual-reflex" {
		t.Fatalf("visual session did not report its independent reflex: %+v", runtime.Status())
	}
	speak(t, runtime, 3)
	waitFor(t, func() bool { return config.Fast.(*scriptedProvider).invocations() > 0 }, "voice turn did not finish")
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	select {
	case call := <-dispatched:
		if call.Name != computeruse.Click {
			t.Fatalf("unexpected action: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("visual reflex action was not dispatched")
	}

	reflex.mu.Lock()
	request := reflex.requests[0]
	reflex.mu.Unlock()
	if len(request.Invocation.Tools) != 1 || request.Invocation.Tools[0].Name != computeruse.Click {
		t.Fatalf("visual reflex received the wrong authority surface: %+v", request.Invocation.Tools)
	}
	// Current task, current screen, and the runtime instruction are the whole
	// provider prefix. The voice and slow output from the first turn stay out.
	if len(request.Trajectory.Items) != 3 {
		t.Fatalf("visual reflex received non-compact history: %+v", request.Trajectory.Items)
	}
	for _, item := range request.Trajectory.Items {
		if item.Kind == trajectory.KindAssistant || item.Kind == trajectory.KindReasoning || item.Kind == trajectory.KindToolResult {
			t.Fatalf("visual reflex received slow/conversational history: %#v", item)
		}
	}
}

func TestMalformedVisualReflexFallsBackToTheExistingSlowLane(t *testing.T) {
	reflex := newVisualReflex([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "maybe click it"}})
	config := visualReflexVideoConfig()
	config.VisualReflex = reflex
	config.Tools = []action.ToolSpec{{
		Name: computeruse.Click, Description: "click",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			t.Fatalf("malformed reflex dispatched %+v", call)
			return trajectory.ToolResult{}, nil
		}),
	}}
	runtime, _ := startSession(t, config, binding.Settings{})
	speak(t, runtime, 3)
	slow := config.Slow.(*scriptedProvider)
	waitFor(t, func() bool { return slow.invocations() > 0 }, "initial slow turn did not finish")
	before := slow.invocations()
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	waitFor(t, func() bool { return slow.invocations() > before }, "malformed reflex did not fall back to slow")
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindToolCall && item.ToolCall != nil && strings.HasPrefix(item.ToolCall.CallID, "visual-") {
			t.Fatalf("malformed reflex committed a call: %#v", item)
		}
	}
}

type blockingVisualReflex struct{ descriptor continuation.Descriptor }

func (provider blockingVisualReflex) Descriptor() continuation.Descriptor { return provider.descriptor }
func (provider blockingVisualReflex) Continue(
	ctx context.Context, _ continuation.Request, _ continuation.Emit,
) (continuation.Completion, error) {
	<-ctx.Done()
	return continuation.Completion{}, ctx.Err()
}

func TestTimedOutVisualReflexFallsBackToTheExistingSlowLane(t *testing.T) {
	descriptor := newVisualReflex().descriptor
	config := visualReflexVideoConfig()
	config.VisualReflex = blockingVisualReflex{descriptor: descriptor}
	config.VisualReflexTimeout = 10 * time.Millisecond
	config.Tools = []action.ToolSpec{{
		Name: computeruse.Click, Description: "click",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			t.Fatalf("timed-out reflex dispatched %+v", call)
			return trajectory.ToolResult{}, nil
		}),
	}}
	runtime, _ := startSession(t, config, binding.Settings{})
	speak(t, runtime, 3)
	slow := config.Slow.(*scriptedProvider)
	waitFor(t, func() bool { return slow.invocations() > 0 }, "initial slow turn did not finish")
	before := slow.invocations()
	started := time.Now()
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	waitFor(t, func() bool { return slow.invocations() > before }, "timed-out reflex did not fall back to slow")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("reflex timeout delayed fallback by %s", elapsed)
	}
}

func TestVisualReflexActionStillRequiresDeclaredAuthorization(t *testing.T) {
	var executed atomic.Int32
	audited := make(chan action.Record, 1)
	reflex := newVisualReflex([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "visual-click-denied", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":40,"y":30}`),
		},
	}})
	config := visualReflexVideoConfig()
	config.VisualReflex = reflex
	config.ActionAudit = func(record action.Record) { audited <- record }
	config.Tools = []action.ToolSpec{{
		Name: computeruse.Click, Description: "click",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmAlways,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			executed.Add(1)
			return trajectory.ToolResult{CallID: call.CallID, Name: call.Name}, nil
		}),
	}}
	runtime, _ := startSession(t, config, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool { return config.Fast.(*scriptedProvider).invocations() > 0 }, "voice turn did not finish")
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	select {
	case record := <-audited:
		if record.Executed || !strings.Contains(record.Error, "confirmed") {
			t.Fatalf("unexpected authorization record: %+v", record)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("visual reflex refusal was not audited")
	}
	if executed.Load() != 0 {
		t.Fatal("visual reflex bypassed declared confirmation")
	}
}
