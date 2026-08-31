package cascade

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type manualObserverTurnDeferral struct{}

func (manualObserverTurnDeferral) Name() string                         { return "test-client-driven+silent-observers" }
func (manualObserverTurnDeferral) Conditions() []session.TransitionKind { return nil }
func (manualObserverTurnDeferral) Admit(waiting interaction.Waiting) (bool, string) {
	if waiting.AutonomousObservation || waiting.Requested {
		return true, ""
	}
	return false, "waiting for response.create"
}
func (manualObserverTurnDeferral) ConsumesResponseRequest(waiting interaction.Waiting) bool {
	return !waiting.AutonomousObservation
}

type manualObserverTurnRollout struct {
	observerOnce sync.Once
	observerSeen chan struct{}
}

func (*manualObserverTurnRollout) Name() string { return "test-user-only-response" }
func (rollout *manualObserverTurnRollout) Plan(input interaction.RolloutInput) []interaction.Step {
	if input.Cause.AutonomousObservation {
		rollout.observerOnce.Do(func() { close(rollout.observerSeen) })
		return nil
	}
	if input.Cause.Observation {
		return []interaction.Step{{Kind: interaction.StepFast, Reason: "answer exact user turn"}}
	}
	return nil
}

type manualObserverTurnPerception struct{ revisions atomic.Uint64 }

func (*manualObserverTurnPerception) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "test-manual-perception", Version: "1"}
}
func (*manualObserverTurnPerception) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}
func (provider *manualObserverTurnPerception) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	revision := provider.revisions.Add(1)
	return v1.PerceptionRevision{RevisionID: revision, StableText: "answer the user now", Final: true}, nil
}

type manualObserverTurnProvider struct {
	descriptor continuation.Descriptor
	calls      atomic.Int64
}

func (provider *manualObserverTurnProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}
func (provider *manualObserverTurnProvider) Continue(
	ctx context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.calls.Add(1)
	if provider.descriptor.EffectiveSpeechAuthority() == continuation.SpeechAuthorityVoice {
		if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "Done."}); err != nil {
			return continuation.Completion{}, err
		}
	}
	return continuation.Completion{StopReason: "stop"}, context.Cause(ctx)
}

type manualObserverTurnSpeech struct{}

func (manualObserverTurnSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "test-manual-speech", Version: "1"}
}
func (manualObserverTurnSpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, nil
}
func (manualObserverTurnSpeech) Stream(context.Context, v1.SpeechPlan, func(v1.SpeechChunk) error) error {
	return nil
}

type manualObserverTurnSink struct {
	begins atomic.Int64
	ends   atomic.Int64
}

func (sink *manualObserverTurnSink) TurnBegin(context.Context) error {
	sink.begins.Add(1)
	return nil
}
func (sink *manualObserverTurnSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	sink.ends.Add(1)
	return nil
}
func (*manualObserverTurnSink) Activity(context.Context, legacy.ActivityEvent) error      { return nil }
func (*manualObserverTurnSink) Transcript(context.Context, legacy.TranscriptEvent) error  { return nil }
func (*manualObserverTurnSink) Observation(context.Context, perception.Observation) error { return nil }
func (*manualObserverTurnSink) SpeechBegin(context.Context, action.Utterance) error       { return nil }
func (*manualObserverTurnSink) SpeechText(context.Context, action.Utterance, string) error {
	return nil
}
func (*manualObserverTurnSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (*manualObserverTurnSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}
func (*manualObserverTurnSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (*manualObserverTurnSink) Failed(context.Context, legacy.ErrorEvent)             {}

func TestManualObserverWithoutResponseTurnPreservesTurnScopedPolicy(t *testing.T) {
	rollout := &manualObserverTurnRollout{observerSeen: make(chan struct{})}
	policies := interaction.Defaults()
	policies.Rollout = rollout
	fast := &manualObserverTurnProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort:          continuation.EffortMinimal,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}}
	slow := &manualObserverTurnProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort:          continuation.EffortHigh,
		ToolAuthority:   continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}}
	perceptionProvider := &manualObserverTurnPerception{}
	bind, err := New(Config{
		Perception: func() (v1.PerceptionProvider, error) { return perceptionProvider, nil },
		Observers: []perception.Factory{perception.VideoFactory(perception.VideoConfig{
			Narrator: perception.StaticNarrator{Text: "A deployment screen is visible."},
		})},
		Fast: fast, Slow: slow, Speech: manualObserverTurnSpeech{}, Policies: policies,
		ManualDeferral: manualObserverTurnDeferral{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &manualObserverTurnSink{}
	started, err := bind.Start(t.Context(), legacy.Options{
		Sink: sink, SessionID: "manual-observer-turn-policy",
		Settings: legacy.Settings{ManualTurns: true, Modalities: []string{"text"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = started.Close(context.Background(), nil) })
	runtime, ok := started.(*runtime)
	if !ok {
		t.Fatalf("manual observer runtime has type %T", started)
	}

	const turnPolicy = "wait for the response-authorized user turn"
	runtime.pinboard.Pin(interaction.StandingInstruction{Text: turnPolicy, Scope: interaction.ScopeTurn})
	if err := runtime.CreateResponse(t.Context()); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Video(t.Context(), perception.Frame{
		Kind: perception.FrameImage, Source: "screen", MIMEType: "image/jpeg",
		Width: 16, Height: 16, CapturedNS: uint64(time.Now().UnixNano()), Image: encoded.Bytes(),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rollout.observerSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("observer-only batch did not reach the silent rollout")
	}
	waitManualObserverTurn(t, func() bool { return !runtime.coordinator.Active() }, "observer-only run stayed active")
	if got := runtime.pinboard.InForce(); len(got) != 1 || got[0].Text != turnPolicy {
		t.Fatalf("observer-only zero-step run expired turn policy: %+v", got)
	}
	if fast.calls.Load() != 0 || sink.begins.Load() != 0 || sink.ends.Load() != 0 {
		t.Fatalf("observer-only run opened response: fast=%d begin=%d end=%d",
			fast.calls.Load(), sink.begins.Load(), sink.ends.Load())
	}

	if err := runtime.Audio(t.Context(), perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
		PCM16LE: bytes.Repeat([]byte{1, 0}, 2_400),
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.CommitAudio(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitManualObserverTurn(t, func() bool {
		return fast.calls.Load() == 1 && sink.ends.Load() == 1 && !runtime.coordinator.Active()
	}, "response-authorized user batch did not finish one response turn")
	if got := runtime.pinboard.InForce(); len(got) != 0 {
		t.Fatalf("completed user response retained turn policy: %+v", got)
	}
}

func waitManualObserverTurn(t *testing.T, ready func() bool, message string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !ready() {
		select {
		case <-deadline:
			t.Fatal(message)
		case <-time.After(time.Millisecond):
		}
	}
}
