package cascade_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync"
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
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// callingFloor asks for a tool call on every revision, which is what a model
// that has just seen a phone menu does.
type callingFloor struct{}

func (callingFloor) Name() string      { return "calling" }
func (callingFloor) EngineOwned() bool { return true }

func (callingFloor) Endpoint(decision interaction.Context) interaction.EndpointDecision {
	if decision.Revision.Empty() {
		return interaction.EndpointDecision{}
	}
	return interaction.EndpointDecision{Act: interaction.ActActSilently, Reason: "the menu named the option"}
}

func (callingFloor) Holder(session.Snapshot) interaction.Holder { return interaction.HolderNobody }

// compositeSilentFloor reproduces the meeting case: the interaction policy
// starts a silent visual act from a partial, then ordinary endpoint silence
// still commits the user's complete request.
type compositeSilentFloor struct{}

func (compositeSilentFloor) Name() string      { return "composite-silent" }
func (compositeSilentFloor) EngineOwned() bool { return true }
func (compositeSilentFloor) Holder(session.Snapshot) interaction.Holder {
	return interaction.HolderNobody
}
func (compositeSilentFloor) Endpoint(decision interaction.Context) interaction.EndpointDecision {
	return interaction.EndpointDecision{
		Act:   interaction.ActActSilently,
		Ended: decision.Revision.Final || decision.Revision.SilenceNS >= uint64(500*time.Millisecond),
	}
}

// Acting twice on one stretch of speech is acting twice for one reason, and a
// revision boundary is not a new reason. A recorded menu is one utterance that
// produces a revision every few hundred milliseconds, and one act per revision
// was nine key presses in a single call - a person doing that lands three
// menus deep.
func TestOneSilentActPerStretchOfSpeech(t *testing.T) {
	var mu sync.Mutex
	var acts []string
	model, err := interaction.NewInteractionModel(silentDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Floor = callingFloor{}
	// actSilently runs only where the interaction model owns the decision,
	// which is the arrangement this rule belongs to.
	policies.Interaction = model
	policies.ShadowInteraction = func(record interaction.ShadowDecision) {
		if record.Predicates["where"] != "interject" && record.Predicates["where"] != "act-silently" {
			return
		}
		mu.Lock()
		acts = append(acts, record.Act)
		mu.Unlock()
	}
	runtime, _ := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &scriptedASR{
				partials: []string{
					"press one for billing",
					"press one for billing press two for order status",
					"press one for billing press two for order status press three",
				},
				final: "press one for billing press two for order status press three",
			}, nil
		},
		Fast: newFast(), Slow: newSlow(), Policies: policies,
	}, binding.Settings{})

	for index := 0; index < 8; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	// A second utterance, which is what the recogniser produces every few
	// seconds out of one recorded menu - and what the prefix test alone
	// cannot tell from a genuinely new prompt.
	for index := 0; index < 8; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(acts) > 0
	}, "the act was never taken at all")
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	taken := 0
	for _, act := range acts {
		if act == "acted" || act == "refused" {
			taken++
		}
	}
	if taken > 1 {
		t.Fatalf("acted %d times in three seconds: %q", taken, acts)
	}
}

func TestSilentComputerActUsesTheBoundedVisualActor(t *testing.T) {
	model, err := interaction.NewInteractionModel(visualDirectDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Floor = callingFloor{}
	policies.Interaction = model

	dispatched := make(chan trajectory.ToolCall, 1)
	reflex := newVisualReflex([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "silent-visual-click", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":40,"y":30}`),
		},
	}})
	config := visualReflexVideoConfig()
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{
			partials: []string{"click the acknowledge alert button now"},
			final:    "click the acknowledge alert button now",
		}, nil
	}
	config.Policies = policies
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
	runtime, _ := startSession(t, config, binding.Settings{})
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	for range 8 {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	select {
	case call := <-dispatched:
		if call.Name != computeruse.Click {
			t.Fatalf("unexpected silent visual action: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("silent computer act did not reach the visual actor")
	}

	reflex.mu.Lock()
	request := reflex.requests[0]
	reflex.mu.Unlock()
	if !strings.Contains(request.Invocation.Instruction, "click the acknowledge alert button now") {
		t.Fatalf("visual actor missed the live request: %q", request.Invocation.Instruction)
	}
}

func TestNoScreenAuthorityCannotReachVisualActorThroughSilentAct(t *testing.T) {
	model, err := interaction.NewInteractionModel(visualNoneDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Floor = callingFloor{}
	policies.Interaction = model

	reflex := newVisualReflex([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "unauthorized-prefix-click", Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":40,"y":30}`),
		},
	}})
	slow := newSlow()
	config := visualReflexVideoConfig()
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{partials: []string{"Wait, go back"}}, nil
	}
	config.Policies = policies
	config.VisualReflex = reflex
	config.Slow = slow
	runtime, _ := startSession(t, config, binding.Settings{})
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	for range 3 {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	waitFor(t, func() bool { return slow.invocations() > 0 }, "no-screen silent act did not fall back to nonvisual cognition")
	time.Sleep(100 * time.Millisecond)
	if got := reflex.invocations(); got != 0 {
		t.Fatalf("incomplete no-screen prefix reached the visual actor %d times", got)
	}
}

func TestCompositeSilentVisualWaitResumesSpeechAndStillMonitors(t *testing.T) {
	model, err := interaction.NewInteractionModel(visualDirectDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Floor = compositeSilentFloor{}
	policies.Interaction = model
	policies.Deferral = interaction.NewDuplexDeferral(interaction.DeferralOptions{
		AllowWhileUserSpeaking: true,
	})

	dispatched := make(chan trajectory.ToolCall, 1)
	reflex := newVisualReflex(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "WAIT"}},
		[]continuation.Event{{
			Kind: continuation.EventToolCall,
			ToolCall: &trajectory.ToolCall{
				CallID: "later-alert-click", Name: computeruse.Click,
				Arguments: json.RawMessage(`{"source":"screen","x":320,"y":240}`),
			},
		}},
	)
	fast := newFast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "Here is the project overview.",
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
	config.Policies = policies
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
	}, "initial screen was not retained for the silent visual actor")
	// Retention is the canonical append; let its observer-only event-loop turn
	// finish before arming the monitor so this fixture's old frame cannot race
	// the deliberately scripted response for the later changed frame.
	time.Sleep(50 * time.Millisecond)

	// Drive only the live partial first. No canonical user observation exists
	// yet, so any voice turn here must have come from CompositeResume rather
	// than the ordinary endpoint path.
	pushAudio(t, runtime, tone(2400, 8000), 3)
	spokenPresentation := func() bool {
		for _, text := range sink.spokenTexts() {
			if text == "Here is the project overview." {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for !spokenPresentation() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !spokenPresentation() {
		t.Fatalf("the visual WAIT consumed the independent presentation obligation: reflex=%d fast=%d slow=%d status=%+v trajectory=%+v",
			reflex.invocations(), fast.invocations(), config.Slow.(*scriptedProvider).invocations(), runtime.Status(), runtime.Trajectory().Items)
	}
	fast.mu.Lock()
	voiceRequests := append([]continuation.Request(nil), fast.requests...)
	fast.mu.Unlock()
	compositeCause := false
	for _, request := range voiceRequests {
		if strings.Contains(request.Invocation.Instruction, "independent nonvisual obligation") {
			compositeCause = true
			break
		}
	}
	if !compositeCause {
		t.Fatalf("the resumed voice turn lost its composite cause across %d requests", len(voiceRequests))
	}
	// Commit the complete request before testing autonomous visual monitoring:
	// observer evidence is deliberately inert until user authority is present.
	pushAudio(t, runtime, silence(2400), 8)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
				return true
			}
		}
		return false
	}, "the completed composite request did not arm later visual monitoring")
	// The visual observer deliberately preserves its adaptive cadence and
	// change gate. Wait one cadence, then send a genuinely changed screen.
	time.Sleep(350 * time.Millisecond)
	if err := runtime.Video(context.Background(), alertScreenFrame(t)); err != nil {
		t.Fatalf("alert video: %v", err)
	}
	select {
	case call := <-dispatched:
		if call.CallID != "later-alert-click" {
			t.Fatalf("unexpected later visual action: %+v", call)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resuming speech disabled the later silent visual action")
	}
}

func TestCompositeResumeKeepsAPureSilentMonitorSilent(t *testing.T) {
	model, err := interaction.NewInteractionModel(visualDirectDecider{})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Floor = compositeSilentFloor{}
	policies.Interaction = model
	policies.Deferral = interaction.NewDuplexDeferral(interaction.DeferralOptions{
		AllowWhileUserSpeaking: true,
	})
	fast := newFast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "<wait>",
	}})
	config := visualReflexVideoConfig()
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{
			partials: []string{"silently acknowledge an alert if it appears"},
			final:    "silently acknowledge an alert if it appears",
		}, nil
	}
	config.Fast = fast
	config.Slow = newSlow()
	config.Policies = policies
	config.VisualReflex = newVisualReflex([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "WAIT",
	}})
	config.Tools = []action.ToolSpec{{
		Name: computeruse.Click, Description: "click the current screen",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmNever,
		Target: "browser", Dispatcher: action.DispatcherFunc(func(
			_ context.Context, call trajectory.ToolCall,
		) (trajectory.ToolResult, error) {
			t.Fatalf("absent alert dispatched %+v", call)
			return trajectory.ToolResult{}, nil
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
	pushAudio(t, runtime, tone(2400, 8000), 3)
	deadline := time.Now().Add(3 * time.Second)
	for fast.invocations() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fast.invocations() == 0 {
		t.Fatalf("pure silent request never reached the resume policy: reflex=%d status=%+v trajectory=%+v",
			config.VisualReflex.(*scriptedProvider).invocations(), runtime.Status(), runtime.Trajectory().Items)
	}
	time.Sleep(100 * time.Millisecond)
	if spoken := sink.spokenTexts(); len(spoken) != 0 {
		t.Fatalf("pure silent monitoring became an announcement: %q", spoken)
	}
}

func alertScreenFrame(t *testing.T) perception.Frame {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			pixel := color.RGBA{R: 40, G: 40, B: 40, A: 255}
			if x > 180 && x < 460 && y > 150 && y < 330 {
				pixel = color.RGBA{R: 230, G: 20, B: 20, A: 255}
			}
			canvas.Set(x, y, pixel)
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, nil); err != nil {
		t.Fatalf("encode alert frame: %v", err)
	}
	return perception.Frame{
		Kind: perception.FrameImage, Source: "screen", MIMEType: "image/jpeg",
		Width: 640, Height: 480, CapturedNS: uint64(time.Now().UnixNano()), Image: buffer.Bytes(),
	}
}

// silentDecider always stays silent; the floor above is what asks for the act.
type silentDecider struct{}

func (silentDecider) Name() string { return "silent" }

func (silentDecider) Decide(
	_ context.Context, request interaction.Decision,
) (interaction.Outcome, error) {
	for index, option := range request.Options {
		if option == string(interaction.ActStaySilent) {
			return interaction.Outcome{Index: index}, nil
		}
	}
	return interaction.Outcome{Index: 0}, nil
}

type visualNoneDecider struct{}

func (visualNoneDecider) Name() string { return "visual-none" }

func (visualNoneDecider) Decide(
	_ context.Context, request interaction.Decision,
) (interaction.Outcome, error) {
	want := string(interaction.ActStaySilent)
	for _, option := range request.Options {
		if option == string(interaction.VisualIntentNone) {
			want = option
			break
		}
	}
	for index, option := range request.Options {
		if option == want {
			return interaction.Outcome{Index: index, Option: option}, nil
		}
	}
	return interaction.Outcome{}, nil
}
