package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	scenarioSupersessionSession = "scenario-supersession-session"
	scenarioSupersessionPartial = "Press 2 for order"
	scenarioSupersessionFinal   = "Press 2 for order status."
	scenarioSupersessionCallID  = "call_press_key_supersession_1"
)

// TestScenarioConversationNewerFinalCancelsSlowPartialSilentGeneration covers
// the production graph race observed on the recorded-menu scenario. A policy
// is intentionally made to over-admit an unfinished partial, and the first
// silent model call remains blocked forever unless the newer canonical final
// reaches cognition.TextModel's exact cancellation port. The final can reach
// the client tool boundary only after that cancellation frees the model's
// single-concurrency lane.
func TestScenarioConversationNewerFinalCancelsSlowPartialSilentGeneration(t *testing.T) {
	base := newScenarioProfileFixture(t)
	config := base.pluginConfig()
	config.SemanticAdmission = scenarioconversation.SemanticAdmissionSelection{
		VerifySilentAction: true, MinimumActivationConfidence: 0.7, StandingMemory: 8,
		TranscriptEvents: &policyelements.SemanticTranscriptEventConfig{
			Partial: policyelements.SemanticTranscriptEventRules{
				Instruction: "Classify the live menu partial.", TimeoutMS: 1_000,
				Acts: []coreinteraction.Act{
					coreinteraction.ActStaySilent, coreinteraction.ActActSilently,
				},
			},
			Final: policyelements.SemanticTranscriptEventRules{
				Instruction: "Classify the final menu transcript.", TimeoutMS: 1_000,
				Acts: []coreinteraction.Act{
					coreinteraction.ActStaySilent, coreinteraction.ActActSilently,
				},
			},
		},
	}
	parameters := json.RawMessage(
		`{"type":"object","properties":{"digit":{"type":"string"}},"required":["digit"]}`,
	)
	config.Tools = []scenarioconversation.ToolDeclaration{{
		Name: "press_key", Description: "Send a keypad tone on the open call.",
		Parameters: parameters, Confirm: legacyaction.ConfirmNever,
	}}

	asr := &scenarioSupersessionASR{descriptor: config.ASR.Descriptor}
	policy := &scenarioSupersessionPolicy{descriptor: config.Policy.Descriptor}
	silent := newScenarioSupersessionModel(config.SilentModel.Descriptor, true)
	voice := newScenarioSupersessionModel(config.Model.Descriptor, false)
	tts := &scenarioAddressingTTSControl{}
	config.ASR.Factory = func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
		return asr, nil
	}
	config.Policy.Factory = func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
		return policy, nil
	}
	config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
		return voice, nil
	}
	config.SilentModel.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
		return silent, nil
	}
	config.TTS.Factory = func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
		return &scenarioAddressingTTS{control: tts, descriptor: config.TTS.Descriptor}, nil
	}

	launchConfig, err := graphs.ScenarioConversationLaunchConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	recording := newScenarioAddressingGraphRecorder()
	instrumentScenarioAddressingFactory(t, &launchConfig, "policy.SemanticAdmission", recording,
		"silent_committed", "silent_cancel", "outcome")
	instrumentScenarioAddressingFactory(t, &launchConfig, "cognition.TextModel", recording,
		"outcome")
	launched, err := graphlaunch.New(context.Background(), launchConfig)
	if err != nil {
		t.Fatal(err)
	}

	sink := newScenarioSupersessionSink()
	settings := legacy.Settings{
		Instruction: "Call support and choose the order-status menu option without speaking.",
		Tools: []legacyaction.ToolSpec{{
			Name: "press_key", Description: "Send a keypad tone on the open call.",
			Parameters: slices.Clone(parameters), Confirm: legacyaction.ConfirmNever,
		}},
		Voice: config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate,
	}
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		SessionID: scenarioSupersessionSession, Sink: sink, Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := runtime.Close(ctx, errors.New("supersession graph test complete")); closeErr != nil {
			t.Errorf("close scenario supersession runtime: %v", closeErr)
		}
	})
	if err := runtime.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	var clock scenarioAddressingAudioClock
	for index := 0; index < 3; index++ {
		sendScenarioSupersessionAudio(t, runtime, &clock, false)
	}
	firstRunID := receiveScenarioAddressing(t, silent.started, "blocked partial silent generation")
	if firstRunID == "" {
		t.Fatal("partial silent generation has no run ID")
	}

	deadlineStarted := time.Now()
	for index := 0; index < 5; index++ {
		sendScenarioSupersessionAudio(t, runtime, &clock, true)
	}
	awaitScenarioSupersessionFinal(t, sink)
	canceledRunID := receiveScenarioAddressing(t, silent.canceled, "partial generation cancellation")
	if canceledRunID != firstRunID {
		t.Fatalf("canceled silent run = %q, want blocked partial run %q", canceledRunID, firstRunID)
	}
	toolEvent := receiveScenarioAddressing(t, sink.toolCalls, "final-revision press_key call")
	if elapsed := time.Since(deadlineStarted); elapsed > 3*time.Second {
		t.Fatalf("corrected final took %s to reach press_key after endpointing", elapsed)
	}
	if toolEvent.InvocationID == "" || toolEvent.InvocationID == firstRunID || len(toolEvent.Calls) != 1 {
		t.Fatalf("final-revision tool event = %+v", toolEvent)
	}
	call := toolEvent.Calls[0]
	if call.CallID != scenarioSupersessionCallID || call.Name != "press_key" ||
		string(call.Arguments) != `{"digit":"2"}` {
		t.Fatalf("final-revision tool call = %+v", call)
	}

	cancelEnvelope := recording.await(t, "semantic_admission.silent_cancel", func(envelope element.Envelope) bool {
		cancel, ok := envelope.Payload.(cognitionelements.Cancel)
		return ok && cancel.RunID == firstRunID
	})
	if cancelEnvelope.RunID != firstRunID || cancelEnvelope.CancellationScope != firstRunID {
		t.Fatalf("graph silent cancellation address = %+v, want %q", cancelEnvelope, firstRunID)
	}
	if !recording.any("silent_model.outcome", func(envelope element.Envelope) bool {
		outcome, ok := envelope.Payload.(cognitionelements.Outcome)
		return ok && outcome.RunID == firstRunID && outcome.Kind == cognitionelements.OutcomeCanceled
	}) {
		t.Fatalf("silent model has no canceled outcome for %q: %+v",
			firstRunID, recording.records("silent_model.outcome"))
	}

	requests := silent.snapshotRequests()
	if len(requests) != 2 {
		t.Fatalf("silent model requests = %d, want exactly partial and final", len(requests))
	}
	assertScenarioSupersessionRequest(t, requests[0], scenarioSupersessionPartial, 1, false)
	assertScenarioSupersessionRequest(t, requests[1], scenarioSupersessionFinal, 2, true)
	if requests[0].InvocationID != firstRunID || requests[1].InvocationID != toolEvent.InvocationID {
		t.Fatalf("model/tool run identities drifted: first=%q second=%q tool=%q",
			requests[0].InvocationID, requests[1].InvocationID, toolEvent.InvocationID)
	}
	if voice.invocations.Load() != 0 || tts.plans.Load() != 0 {
		t.Fatalf("silent menu action leaked to voice path: voice invocations=%d TTS plans=%d",
			voice.invocations.Load(), tts.plans.Load())
	}
	sink.scenarioAddressingSink.mu.Lock()
	failures := slices.Clone(sink.scenarioAddressingSink.failures)
	turnBegins, turnEnds := sink.scenarioAddressingSink.turnBegins, sink.scenarioAddressingSink.turnEnds
	sink.scenarioAddressingSink.mu.Unlock()
	if len(failures) != 0 || turnBegins != 1 || turnEnds != 1 {
		t.Fatalf("client lifecycle after supersession: failures=%+v turns=%d/%d",
			failures, turnBegins, turnEnds)
	}
}

type scenarioSupersessionASR struct {
	descriptor v1.Descriptor
	partial    atomic.Bool
}

func (provider *scenarioSupersessionASR) Descriptor() v1.Descriptor { return provider.descriptor }

func (provider *scenarioSupersessionASR) PushFrame(
	_ context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	if !provider.partial.CompareAndSwap(false, true) {
		return nil, nil
	}
	return []v1.PerceptionRevision{{
		RevisionID: 1, SourceSample: frame.SampleOffset, StableText: scenarioSupersessionPartial,
	}}, nil
}

func (*scenarioSupersessionASR) Finalize(
	_ context.Context, sample uint64,
) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{
		RevisionID: 2, SourceSample: sample, StableText: scenarioSupersessionFinal, Final: true,
	}, nil
}

type scenarioSupersessionPolicy struct {
	descriptor policyelements.SemanticDeciderDescriptor
}

func (*scenarioSupersessionPolicy) Name() string { return "scenario-supersession-policy" }

func (policy *scenarioSupersessionPolicy) Descriptor() policyelements.SemanticDeciderDescriptor {
	return policy.descriptor
}

func (*scenarioSupersessionPolicy) Decide(
	_ context.Context, decision coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	wanted := string(coreinteraction.ActActSilently)
	if slices.Contains(decision.Options, "action-ready") {
		// Deliberately reproduce the incorrect verifier verdict that originally
		// let the unfinished "Press 2 for order" reach Gemini.
		wanted = "action-ready"
	}
	index := slices.Index(decision.Options, wanted)
	if index < 0 {
		return coreinteraction.Outcome{}, fmt.Errorf(
			"scenario supersession policy has no deterministic answer for %v", decision.Options,
		)
	}
	return coreinteraction.Outcome{
		Index: index, Option: wanted, Confidence: 0.99, Measured: true,
	}, nil
}

func (*scenarioSupersessionPolicy) Generate(context.Context, string, string, int) (string, error) {
	return "none", nil
}

type scenarioSupersessionModel struct {
	descriptor  continuation.Descriptor
	blockFirst  bool
	invocations atomic.Int32
	started     chan string
	canceled    chan string
	mu          sync.Mutex
	requests    []continuation.Request
}

func newScenarioSupersessionModel(
	descriptor continuation.Descriptor, blockFirst bool,
) *scenarioSupersessionModel {
	return &scenarioSupersessionModel{
		descriptor: descriptor, blockFirst: blockFirst,
		started: make(chan string, 1), canceled: make(chan string, 1),
	}
}

func (provider *scenarioSupersessionModel) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (provider *scenarioSupersessionModel) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	provider.requests = append(provider.requests, request)
	provider.mu.Unlock()
	invocation := provider.invocations.Add(1)
	if !provider.blockFirst {
		return continuation.Completion{}, errors.New("supersession test unexpectedly invoked voice model")
	}
	switch invocation {
	case 1:
		provider.started <- request.InvocationID
		<-ctx.Done()
		provider.canceled <- request.InvocationID
		return continuation.Completion{}, context.Cause(ctx)
	case 2:
		call := trajectory.ToolCall{
			CallID: scenarioSupersessionCallID, Name: "press_key",
			Arguments: json.RawMessage(`{"digit":"2"}`),
		}
		if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
			return continuation.Completion{}, err
		}
		return continuation.Completion{StopReason: "tool_call"}, nil
	default:
		return continuation.Completion{}, fmt.Errorf(
			"unexpected supersession model invocation %d", invocation,
		)
	}
}

func (provider *scenarioSupersessionModel) snapshotRequests() []continuation.Request {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return slices.Clone(provider.requests)
}

type scenarioSupersessionSink struct {
	*scenarioAddressingSink
	toolCalls chan legacy.ToolCallEvent
}

func newScenarioSupersessionSink() *scenarioSupersessionSink {
	return &scenarioSupersessionSink{
		scenarioAddressingSink: newScenarioAddressingSink(),
		toolCalls:              make(chan legacy.ToolCallEvent, 2),
	}
}

func (sink *scenarioSupersessionSink) ToolCalls(
	_ context.Context, event legacy.ToolCallEvent,
) error {
	copy := event
	copy.Calls = make([]trajectory.ToolCall, len(event.Calls))
	for index, call := range event.Calls {
		copy.Calls[index] = call
		copy.Calls[index].Arguments = slices.Clone(call.Arguments)
	}
	sink.toolCalls <- copy
	return nil
}

func sendScenarioSupersessionAudio(
	t *testing.T, runtime legacy.Runtime, clock *scenarioAddressingAudioClock, silence bool,
) {
	t.Helper()
	clock.index++
	clock.nowNS += uint64(100 * time.Millisecond)
	pcm := scenarioEndpointPCM(2_400)
	if silence {
		pcm = make([]byte, 4_800)
	}
	frame := perception.Frame{
		Kind: perception.FrameAudio, Source: scenarioconversation.SourceMicrophone,
		CapturedNS: clock.nowNS, Index: clock.index, PCM16LE: pcm,
		SampleRateHz: 24_000, SampleOffset: clock.sample,
	}
	clock.sample += 2_400
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Audio(ctx, frame); err != nil {
		t.Fatalf("send supersession audio frame %d: %v", clock.index, err)
	}
}

func awaitScenarioSupersessionFinal(t *testing.T, sink *scenarioSupersessionSink) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-sink.transcripts:
			if event.Final {
				if event.Text != scenarioSupersessionFinal {
					t.Fatalf("supersession final transcript = %q", event.Text)
				}
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for supersession final transcript")
		}
	}
}

func assertScenarioSupersessionRequest(
	t *testing.T, request continuation.Request, text string, revision uint64, final bool,
) {
	t.Helper()
	var current *trajectory.Item
	for index := range request.Trajectory.Items {
		item := &request.Trajectory.Items[index]
		if item.Kind == trajectory.KindObservation && item.Producer.Phase == trajectory.PhaseUser {
			current = item
		}
	}
	wantEventSuffix := ".revision"
	if final {
		wantEventSuffix = ".endpoint"
	}
	if current == nil || current.Content != text || current.SourceRevision != revision ||
		current.Event == nil || !strings.HasSuffix(current.Event.Type, wantEventSuffix) {
		t.Fatalf("model request current observation = %+v, want %q revision=%d final=%t",
			current, text, revision, final)
	}
	if final && current.Event.SupersedesRevision != 1 {
		t.Fatalf("final model request does not supersede revision 1: %+v", current.Event)
	}
}

var (
	_ v1.PerceptionProvider          = (*scenarioSupersessionASR)(nil)
	_ policyelements.SemanticDecider = (*scenarioSupersessionPolicy)(nil)
	_ continuation.Provider          = (*scenarioSupersessionModel)(nil)
	_ legacy.Sink                    = (*scenarioSupersessionSink)(nil)
)
