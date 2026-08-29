package cascade_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
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

type heldFirstVisualReflex struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	mu      sync.Mutex
	seen    []continuation.Request
}

func (reflex *heldFirstVisualReflex) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "held-visual-reflex", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent, Vision: true,
	}
}

func (reflex *heldFirstVisualReflex) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	call := reflex.calls.Add(1)
	reflex.mu.Lock()
	reflex.seen = append(reflex.seen, request)
	reflex.mu.Unlock()
	if call == 1 {
		select {
		case reflex.started <- struct{}{}:
		default:
		}
		select {
		case <-reflex.release:
		case <-ctx.Done():
			return continuation.Completion{}, context.Cause(ctx)
		}
		if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "coalesced-open", Name: computeruse.ClickElement,
			Arguments: json.RawMessage(`{"source":"screen","element_id":"3","_openrealtime_continue":true}`),
		}}); err != nil {
			return continuation.Completion{}, err
		}
	} else {
		if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "coalesced-share", Name: computeruse.ClickElement,
			Arguments: json.RawMessage(`{"source":"screen","element_id":"4","_openrealtime_continue":false}`),
		}}); err != nil {
			return continuation.Completion{}, err
		}
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

type visualDirectDecider struct{}

func (visualDirectDecider) Name() string { return "visual-direct" }
func (visualDirectDecider) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	want := string(interaction.ActStaySilent)
	for _, option := range decision.Options {
		if option == string(interaction.VisualIntentDirect) {
			want = option
			break
		}
	}
	for index, option := range decision.Options {
		if option == want {
			return interaction.Outcome{Index: index, Option: option}, nil
		}
	}
	return interaction.Outcome{}, nil
}

type visualMonitorDecider struct{}

func (visualMonitorDecider) Name() string { return "visual-monitor" }
func (visualMonitorDecider) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	want := string(interaction.ActStaySilent)
	for _, option := range decision.Options {
		if option == string(interaction.VisualIntentMonitor) {
			want = option
			break
		}
	}
	for index, option := range decision.Options {
		if option == want {
			return interaction.Outcome{Index: index, Option: option}, nil
		}
	}
	return interaction.Outcome{}, nil
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
	dispatched := make(chan trajectory.ToolCall, 2)
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

func TestSelectedVisualReflexExclusivelyOwnsImmediateComputerEffects(t *testing.T) {
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "done"}})
	slow.descriptor.Vision = true
	config := visualReflexVideoConfig()
	config.VisualReflex = newVisualReflex()
	config.Slow = slow
	config.Tools = []action.ToolSpec{
		{Name: computeruse.ClickNormalized, Description: "click the current screen", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "meeting.read_review", Description: "read the review", Parameters: json.RawMessage(`{"type":"object"}`), Background: true},
	}
	runtime, _ := startSession(t, config, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool { return slow.invocations() > 0 }, "slow cognition did not run")

	slow.mu.Lock()
	request := slow.requests[0]
	slow.mu.Unlock()
	var names []string
	for _, tool := range request.Invocation.Tools {
		names = append(names, tool.Name)
	}
	if slices.Contains(names, computeruse.ClickNormalized) {
		t.Fatalf("vision-capable slow lane retained coordinate-action authority: %v", names)
	}
	if !slices.Contains(names, "meeting.read_review") {
		t.Fatalf("filter removed the slow lane's semantic tool: %v", names)
	}
	for _, tool := range request.Invocation.Tools {
		if tool.Name == "meeting.read_review" && !tool.Background {
			t.Fatal("the cognition catalog dropped typed background execution policy")
		}
	}
}

func TestMixedUserAndVisualBatchPreservesActionAndVoiceBranches(t *testing.T) {
	dispatched := make(chan trajectory.ToolCall, 1)
	reflex := newVisualReflex([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "mixed-alert-click", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":320,"y":240}`),
		},
	}})
	fast := newFast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "Here is the launch overview.",
	}})
	config := visualReflexVideoConfig()
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{
			partials: []string{"present the overview and acknowledge an alert if it appears"},
			final:    "present the overview and acknowledge an alert if it appears",
		}, nil
	}
	config.Fast = fast
	config.Slow = newSlow()
	config.VisualReflex = reflex
	config.Tools = []action.ToolSpec{{
		Name: computeruse.Click, Description: "click the current screen",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			dispatched <- call
			return trajectory.ToolResult{
				CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`),
			}, nil
		}),
	}}
	runtime, sink := startSession(t, config, binding.Settings{})
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("initial video: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
				return true
			}
		}
		return false
	}, "initial screen was not retained")

	// The visual event commits while the user still owns the floor, so the
	// default duplex gate queues it. Endpoint silence then contributes the user
	// observation and the coordinator presents both at one safe point.
	pushAudio(t, runtime, tone(2400, 8000), 3)
	time.Sleep(350 * time.Millisecond)
	if err := runtime.Video(context.Background(), alertScreenFrame(t)); err != nil {
		t.Fatalf("alert video: %v", err)
	}
	pushAudio(t, runtime, silence(2400), 8)

	select {
	case call := <-dispatched:
		if call.CallID != "mixed-alert-click" {
			t.Fatalf("unexpected mixed-batch action: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mixed user+visual batch lost its visual action")
	}
	waitFor(t, func() bool {
		for _, text := range sink.spokenTexts() {
			if text == "Here is the launch overview." {
				return true
			}
		}
		return false
	}, "mixed user+visual batch lost its presentation obligation")
	fast.mu.Lock()
	requests := append([]continuation.Request(nil), fast.requests...)
	fast.mu.Unlock()
	if len(requests) == 0 || !strings.Contains(requests[0].Invocation.Instruction, "independent nonvisual obligation") {
		t.Fatalf("mixed batch did not identify its independent branch: %+v", requests)
	}
	foundActionResult := false
	for _, item := range requests[0].Trajectory.Items {
		if item.Kind == trajectory.KindToolResult && item.ToolResult != nil &&
			item.ToolResult.CallID == "mixed-alert-click" {
			foundActionResult = true
			break
		}
	}
	if !foundActionResult {
		t.Fatal("mixed-batch voice did not resume from the post-action trajectory")
	}
}

func TestDirectVisionReflexKeepsGroundedComputerEffectsFromTextOnlySlow(t *testing.T) {
	config := visualReflexVideoConfig()
	config.VisualReflex = newVisualReflex()
	slow := config.Slow.(*scriptedProvider)
	config.Tools = []action.ToolSpec{
		{
			Name: computeruse.ClickElement, Description: "click a visible mark",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Target: "browser",
		},
		{
			Name: "lookup_project", Description: "look up project facts",
			Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
			Target: "knowledge",
		},
	}
	runtime, _ := startSession(t, config, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool { return slow.invocations() > 0 }, "slow cognition did not run")
	slow.mu.Lock()
	request := slow.requests[0]
	slow.mu.Unlock()
	for _, tool := range request.Invocation.Tools {
		if tool.Name == computeruse.ClickElement {
			t.Fatal("text-only slow cognition received a visually grounded effector")
		}
	}
	foundKnowledge := false
	for _, tool := range request.Invocation.Tools {
		foundKnowledge = foundKnowledge || tool.Name == "lookup_project"
	}
	if !foundKnowledge {
		t.Fatalf("narrowing visual effectors also removed nonvisual tools: %+v", request.Invocation.Tools)
	}
}

func TestUserNavigationInvokesSilentVisualActorAgainstRetainedFrame(t *testing.T) {
	dispatched := make(chan trajectory.ToolCall, 1)
	reflex := newVisualReflex([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "retained-frame-nav", Name: computeruse.ClickElement,
			Arguments: json.RawMessage(`{"source":"screen","element_id":"3"}`),
		},
	}})
	config := visualReflexVideoConfig()
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{final: "switch to the risks slide now"}, nil
	}
	config.VisualReflex = reflex
	config.Tools = []action.ToolSpec{{
		Name: computeruse.ClickElement, Description: "click a visible mark",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			dispatched <- call
			return trajectory.ToolResult{CallID: call.CallID, Name: call.Name}, nil
		}),
	}}
	runtime, _ := startSession(t, config, binding.Settings{})
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
				return true
			}
		}
		return false
	}, "retained screen was not committed")
	speak(t, runtime, 3)
	select {
	case call := <-dispatched:
		if call.CallID != "retained-frame-nav" {
			t.Fatalf("unexpected retained-frame action: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("explicit navigation did not invoke the visual actor on the retained frame")
	}
}

func TestLivePartialCanDispatchOnePixelGroundedMicroTurnBeforeTheEndpoint(t *testing.T) {
	model, err := interaction.NewInteractionModel(visualDirectDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Interaction = model
	dispatched := make(chan trajectory.ToolCall, 1)
	reflex := newVisualReflex([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "live-partial-navigation", Name: computeruse.ClickElement,
			Arguments: json.RawMessage(`{"source":"screen","element_id":"3","_openrealtime_continue":false}`),
		},
	}})
	config := visualReflexVideoConfig()
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{
			partials: []string{"switch to", "switch to the risks slide now"},
			final:    "switch to the risks slide now",
		}, nil
	}
	config.Policies = policies
	config.VisualReflex = reflex
	config.Tools = []action.ToolSpec{{
		Name: computeruse.ClickElement, Description: "click a visible mark",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			dispatched <- call
			return trajectory.ToolResult{
				CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`),
			}, nil
		}),
	}}
	runtime, _ := startSession(t, config, binding.Settings{})
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
				return true
			}
		}
		return false
	}, "retained screen was not committed")
	// No silence follows these frames, so the acoustic endpoint cannot be the
	// source of the action. The 200 ms trigger over ASR revisions is.
	pushAudio(t, runtime, tone(2400, 8000), 3)
	select {
	case call := <-dispatched:
		if call.CallID != "live-partial-navigation" ||
			strings.Contains(string(call.Arguments), "_openrealtime_continue") {
			t.Fatalf("unexpected live visual action: %+v", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("live partial did not dispatch before the endpoint")
	}
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			t.Fatal("action waited for a canonical endpoint observation")
		}
	}
}

func TestLiveVisualLaneCoalescesToTheNewestRevisionWhileBusy(t *testing.T) {
	model, err := interaction.NewInteractionModel(visualDirectDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Interaction = model
	reflex := &heldFirstVisualReflex{
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-reflex.release:
		default:
			close(reflex.release)
		}
	})
	dispatched := make(chan trajectory.ToolCall, 1)
	config := visualReflexVideoConfig()
	config.ASRCadence = 100 * time.Millisecond
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{partials: []string{
			"open the launch review", "open the launch review and share your screen",
		}}, nil
	}
	config.Policies = policies
	config.VisualReflex = reflex
	config.Tools = []action.ToolSpec{{
		Name: computeruse.ClickElement, Description: "click a visible mark",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			dispatched <- call
			return trajectory.ToolResult{
				CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`),
			}, nil
		}),
	}}
	runtime, _ := startSession(t, config, binding.Settings{})
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
				return true
			}
		}
		return false
	}, "retained screen was not committed")

	pushAudio(t, runtime, tone(2400, 8000), 3)
	select {
	case <-reflex.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first visual micro-turn did not start")
	}
	// Let the fixed trigger cadence open again, then deliver the last ASR
	// revision while the first VLM request is deliberately still occupied.
	time.Sleep(220 * time.Millisecond)
	pushAudio(t, runtime, tone(2400, 8000), 3)
	close(reflex.release)

	select {
	case call := <-dispatched:
		if call.CallID != "coalesced-open" ||
			strings.Contains(string(call.Arguments), "_openrealtime_continue") {
			t.Fatalf("unexpected first action: %+v", call)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first visual action did not finish")
	}
	select {
	case call := <-dispatched:
		t.Fatalf("coalesced revision acted on stale pre-action pixels: %+v", call)
	case <-time.After(100 * time.Millisecond):
	}
	if reflex.calls.Load() != 1 {
		t.Fatalf("visual lane grounded %d requests before a fresh frame, want 1", reflex.calls.Load())
	}
	if err := runtime.Video(context.Background(), alertScreenFrame(t)); err != nil {
		t.Fatalf("fresh video: %v", err)
	}
	select {
	case call := <-dispatched:
		if call.CallID != "coalesced-share" ||
			strings.Contains(string(call.Arguments), "_openrealtime_continue") {
			t.Fatalf("unexpected coalesced action: %+v", call)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("newest ASR revision was not replanned from the fresh frame; calls=%d",
			reflex.calls.Load())
	}
	if reflex.calls.Load() != 2 {
		t.Fatalf("visual lane ran %d requests, want one action per fresh frame", reflex.calls.Load())
	}
	reflex.mu.Lock()
	seen := append([]continuation.Request(nil), reflex.seen...)
	reflex.mu.Unlock()
	if len(seen) != 2 || !strings.Contains(seen[1].Invocation.Instruction, "share your screen") {
		t.Fatalf("coalesced request did not carry the newest words: %+v", seen)
	}
}

func TestArmedDirectVisionCannotBeDiscardedByATextStaySilentDecision(t *testing.T) {
	model, err := interaction.NewInteractionModel(silentDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Interaction = model

	dispatched := make(chan trajectory.ToolCall, 1)
	reflex := newVisualReflex(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "WAIT"}},
		[]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "alert-after-stay-silent", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":320,"y":240}`),
		}}},
	)
	config := visualReflexVideoConfig()
	config.Policies = policies
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
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("initial video: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
				return true
			}
		}
		return false
	}, "initial screen was not retained")

	// A committed user turn arms the monitor. Its retained-frame reflex call
	// consumes the first scripted WAIT.
	speak(t, runtime, 3)
	waitFor(t, func() bool { return reflex.invocations() >= 1 }, "user intent did not arm the visual actor")
	time.Sleep(350 * time.Millisecond)
	if err := runtime.Video(context.Background(), alertScreenFrame(t)); err != nil {
		t.Fatalf("alert video: %v", err)
	}
	select {
	case call := <-dispatched:
		if call.CallID != "alert-after-stay-silent" {
			t.Fatalf("unexpected action: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("text stay-silent decision discarded evidence before the direct visual actor")
	}
}

func TestMonitorAuthorityStartsVoiceBeforeGroundingTheNextFrame(t *testing.T) {
	model, err := interaction.NewInteractionModel(visualMonitorDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Interaction = model

	dispatched := make(chan trajectory.ToolCall, 1)
	reflex := newVisualReflex([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "alert-on-next-frame", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":320,"y":240}`),
		},
	}})
	config := visualReflexVideoConfig()
	config.Policies = policies
	config.VisualReflex = reflex
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{final: "Present the overview and if an alert appears acknowledge it."}, nil
	}
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
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("initial video: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
				return true
			}
		}
		return false
	}, "initial screen was not retained")

	speak(t, runtime, 3)
	fast := config.Fast.(*scriptedProvider)
	waitFor(t, func() bool { return fast.invocations() > 0 }, "monitoring request did not start the fast voice")
	if got := reflex.invocations(); got != 0 {
		t.Fatalf("retained pre-condition frame consumed %d visual calls before voice", got)
	}

	if err := runtime.Video(context.Background(), alertScreenFrame(t)); err != nil {
		t.Fatalf("alert video: %v", err)
	}
	select {
	case call := <-dispatched:
		if call.CallID != "alert-on-next-frame" {
			t.Fatalf("unexpected action: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("armed monitor did not ground the next direct-pixel frame")
	}
	if got := reflex.invocations(); got != 1 {
		t.Fatalf("monitor grounded %d visual frames, want exactly the new frame", got)
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
