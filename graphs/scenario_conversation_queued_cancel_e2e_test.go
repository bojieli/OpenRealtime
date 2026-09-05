package graphs_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
)

// Preparation completes while the first sentence is still being synthesized.
// A spoken stop must revoke every queued sentence through the shipped graph,
// including after the user's speech has ended. A later request must still play.
func TestScenarioConversationStopRevokesCompletedQueuedSpeechAndAllowsNewTurn(t *testing.T) {
	base := newScenarioProfileFixture(t)
	config := base.pluginConfig()
	config.SemanticAdmission.TranscriptEvents = &policyelements.SemanticTranscriptEventConfig{
		Partial: policyelements.SemanticTranscriptEventRules{
			Instruction: "Wait for the final request.", TimeoutMS: 1000,
			Acts: []coreinteraction.Act{coreinteraction.ActStaySilent, coreinteraction.ActSpeakThrough},
		},
		Final: policyelements.SemanticTranscriptEventRules{
			Instruction: "Answer or stop the current speech.", TimeoutMS: 1000,
			Acts: []coreinteraction.Act{coreinteraction.ActStaySilent, coreinteraction.ActAnswer, coreinteraction.ActStopSpeaking},
		},
	}
	asr := &scenarioAddressingASRControl{turns: []string{"Count to eight.", "Hold on a moment.", "Resume now."}}
	policy := &scenarioQueuedCancelPolicy{scenarioAddressingPolicyDecider{
		control: newScenarioAddressingPolicyControl(), descriptor: config.Policy.Descriptor,
	}}
	model := &scenarioQueuedCancelModel{descriptor: config.Model.Descriptor}
	tts := &scenarioQueuedCancelTTS{
		descriptor: config.TTS.Descriptor, started: make(chan string, 1),
		canceled: make(chan struct{}), release: make(chan struct{}),
	}
	t.Cleanup(func() { tts.unblock() })
	config.ASR.Factory = func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
		return &scenarioAddressingASR{control: asr, descriptor: config.ASR.Descriptor}, nil
	}
	config.Policy.Factory = func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) { return policy, nil }
	config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) { return model, nil }
	config.SilentModel.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
		return &scenarioQueuedCancelModel{descriptor: config.SilentModel.Descriptor}, nil
	}
	config.TTS.Factory = func(context.Context, legacy.Options) (v1.SpeechProvider, error) { return tts, nil }
	launchConfig, err := graphs.ScenarioConversationLaunchConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	recording := newScenarioAddressingGraphRecorder()
	instrumentScenarioAddressingFactory(t, &launchConfig, "interaction.SegmentPreparedText", recording, "outcome", "speech_cancel")
	instrumentScenarioAddressingFactory(t, &launchConfig, "interaction.OverlapBargeIn", recording, "segmentation_cancel", "state")
	instrumentScenarioAddressingFactory(t, &launchConfig, "speech.TTS", recording, "outcome")
	launched, err := graphlaunch.New(context.Background(), launchConfig)
	if err != nil {
		t.Fatal(err)
	}
	sink := &scenarioQueuedCancelSink{scenarioAddressingSink: newScenarioAddressingSink(), audio: make(chan string, 32)}
	settings := legacy.Settings{Instruction: "Follow the user's count and stop requests.", Voice: config.TTS.Voice,
		Modalities: []string{"audio"}, Gate: config.Gate}
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{SessionID: "queued-cancel", Sink: sink, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tts.unblock()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := runtime.Close(ctx, errors.New("queued cancellation test complete")); err != nil {
			t.Error(err)
		}
	})
	if err := runtime.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	var clock scenarioAddressingAudioClock
	driveScenarioAddressingTurn(t, runtime, sink.scenarioAddressingSink, &clock, scenarioAddressingStreamID("queued-cancel", 1), asr.turns[0])
	firstID := receiveScenarioAddressing(t, tts.started, "first count synthesis")
	if got := receiveScenarioAddressing(t, sink.audio, "first audible count"); got != "One." {
		t.Fatalf("first audio = %q", got)
	}
	completed := recording.await(t, "segment.outcome", func(envelope element.Envelope) bool {
		outcome, ok := envelope.Payload.(interactionelements.SegmentationOutcome)
		return ok && outcome.Kind == interactionelements.OutcomeCompleted && outcome.Segments == 8
	})
	runID := completed.RunID
	// Ensure the overlap controller has consumed preparation completion before
	// the stop request, rather than winning a race against TextEnd.
	recording.await(t, "overlap_barge_in.state", func(envelope element.Envelope) bool {
		state, ok := envelope.Payload.(interactionelements.OverlapState)
		return ok && slices.Contains(envelope.CausalParents, completed.ItemID) && state.ActiveSegmentations == 0
	})
	driveScenarioAddressingTurn(t, runtime, sink.scenarioAddressingSink, &clock, scenarioAddressingStreamID("queued-cancel", 2), asr.turns[1])
	receiveScenarioAddressing(t, tts.canceled, "active synthesis cancellation")
	recording.await(t, "segment.outcome", func(envelope element.Envelope) bool {
		outcome, ok := envelope.Payload.(interactionelements.SegmentationOutcome)
		return ok && outcome.RunID == runID && outcome.Code == "canceled_after_stream" && outcome.Segments == 8
	})
	// The provider is taking time to finish cancellation. During that cleanup,
	// every future utterance must acquire an exact pending synthesis cancel.
	cancels := recording.records("segment.speech_cancel")
	if len(cancels) != 8 {
		t.Fatalf("queued speech cancellations = %d, want 8", len(cancels))
	}
	for _, envelope := range cancels {
		request := envelope.Payload.(speechelements.Cancel)
		if request.UtteranceID == firstID {
			continue
		}
		recording.await(t, "tts.outcome", func(envelope element.Envelope) bool {
			outcome, ok := envelope.Payload.(speechelements.SynthesisOutcome)
			return ok && outcome.UtteranceID == request.UtteranceID && outcome.Code == "scope_not_active"
		})
	}
	tts.unblock()
	recording.await(t, "overlap_barge_in.state", func(envelope element.Envelope) bool {
		state, ok := envelope.Payload.(interactionelements.OverlapState)
		return ok && state.Canceled > 0 && !state.UserSpeaking && !state.AgentOutput.Active
	})
	select {
	case text := <-sink.audio:
		t.Fatalf("canceled queued speech reached the sink: %q", text)
	default:
	}
	driveScenarioAddressingTurn(t, runtime, sink.scenarioAddressingSink, &clock, scenarioAddressingStreamID("queued-cancel", 3), asr.turns[2])
	if text := receiveScenarioAddressing(t, sink.audio, "new authorized speech"); text != "Resumed." {
		t.Fatalf("new speech = %q", text)
	}
	if model.calls.Load() != 2 || tts.calls.Load() != 2 {
		t.Fatalf("generation/synthesis calls = %d/%d, want initial and resumed only", model.calls.Load(), tts.calls.Load())
	}
}

type scenarioQueuedCancelPolicy struct {
	scenarioAddressingPolicyDecider
}

func (policy *scenarioQueuedCancelPolicy) Decide(ctx context.Context, decision coreinteraction.Decision) (coreinteraction.Outcome, error) {
	wanted := ""
	if slices.Contains(decision.Options, string(coreinteraction.ActStaySilent)) && strings.Contains(decision.Prompt, "Wait for the final request.") {
		wanted = string(coreinteraction.ActStaySilent)
	} else if slices.Contains(decision.Options, string(coreinteraction.ActStopSpeaking)) && strings.Contains(scenarioAddressingCurrentEvidence(decision.Evidence), "Hold on a moment.") {
		wanted = string(coreinteraction.ActStopSpeaking)
	}
	if wanted != "" {
		return coreinteraction.Outcome{Index: slices.Index(decision.Options, wanted), Option: wanted, Confidence: 0.99, Measured: true}, nil
	}
	return policy.scenarioAddressingPolicyDecider.Decide(ctx, decision)
}

type scenarioQueuedCancelModel struct {
	descriptor continuation.Descriptor
	calls      atomic.Int32
}

func (model *scenarioQueuedCancelModel) Descriptor() continuation.Descriptor { return model.descriptor }
func (model *scenarioQueuedCancelModel) Continue(_ context.Context, _ continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	if model.calls.Add(1) == 1 {
		return scenarioEndpointSpeak(emit, "One. Two. Three. Four. Five. Six. Seven. Eight.")
	}
	return scenarioEndpointSpeak(emit, "Resumed.")
}

type scenarioQueuedCancelTTS struct {
	descriptor        v1.Descriptor
	calls             atomic.Int32
	started           chan string
	canceled, release chan struct{}
	once              sync.Once
}

func (tts *scenarioQueuedCancelTTS) unblock()                  { tts.once.Do(func() { close(tts.release) }) }
func (tts *scenarioQueuedCancelTTS) Descriptor() v1.Descriptor { return tts.descriptor }
func (tts *scenarioQueuedCancelTTS) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := tts.Stream(ctx, plan, func(chunk v1.SpeechChunk) error { chunks = append(chunks, chunk); return nil })
	return chunks, err
}
func (tts *scenarioQueuedCancelTTS) Stream(ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	first := tts.calls.Add(1) == 1
	if err := emit(v1.SpeechChunk{ChunkID: plan.CandidateID + ":audio", CandidateID: plan.CandidateID,
		SampleRateHz: 24000, PCM16LE: scenarioEndpointPCM(2400), Final: !first}); err != nil {
		return err
	}
	if !first {
		return nil
	}
	tts.started <- plan.CandidateID
	select {
	case <-ctx.Done():
		close(tts.canceled)
	case <-tts.release:
		return fmt.Errorf("test stopped before cancellation")
	}
	select {
	case <-tts.release:
		return context.Cause(ctx)
	case <-time.After(5 * time.Second):
		return fmt.Errorf("cancellation cleanup timed out")
	}
}

type scenarioQueuedCancelSink struct {
	*scenarioAddressingSink
	audio chan string
}

func (sink *scenarioQueuedCancelSink) SpeechAudio(ctx context.Context, utterance action.Utterance, frame action.Frame) error {
	sink.audio <- utterance.Text
	return sink.scenarioAddressingSink.SpeechAudio(ctx, utterance, frame)
}
