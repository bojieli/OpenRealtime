package graphs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
)

// A standing count fires on the partial that names the animal, while the
// person is still talking, and the setup utterance that established the rule
// says nothing. There is one decision per event and it is the policy's alone:
// the guard stages that used to recover a missed count are gone, so a policy
// that listens on the animal simply produces no count.
func TestScenarioConversationStandingCountFiresOnThePartial(t *testing.T) {
	for _, test := range []struct {
		name       string
		primary    string
		wantSpeech bool
	}{
		{"policy speaks on the animal", coreinteraction.ChoiceSpeak, true},
		{"policy listens on the animal", coreinteraction.ChoiceListen, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newScenarioProfileFixture(t)
			config := base.pluginConfig()
			config.SemanticAdmission.Rules = "Count only when the standing trigger arrives."
			asr := &scenarioAddressingASRControl{turns: []string{
				"Count the animals out loud as I mention them and say nothing else.",
				"A capybara wandered over and sat down next to me.",
			}}
			policy := &scenarioCountAdmissionPolicy{
				descriptor: config.Policy.Descriptor, primary: test.primary, primaryConfidence: 0.99,
			}
			model := &scenarioCountAdmissionModel{descriptor: config.Model.Descriptor}
			tts := &scenarioAddressingTTSControl{}
			config.ASR.Factory = func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
				return &scenarioAddressingASR{control: asr, descriptor: config.ASR.Descriptor}, nil
			}
			config.Policy.Factory = func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) { return policy, nil }
			config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) { return model, nil }
			config.TTS.Factory = func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
				return &scenarioAddressingTTS{control: tts, descriptor: config.TTS.Descriptor}, nil
			}
			launchConfig, err := graphs.ScenarioConversationLaunchConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			recording := newScenarioAddressingGraphRecorder()
			instrumentScenarioAddressingFactory(t, &launchConfig, "policy.SemanticAdmission", recording, "decision", "voice_committed", "outcome")
			instrumentScenarioAddressingFactory(t, &launchConfig, "policy.SessionInvocation", recording, "outcome")
			instrumentScenarioAddressingFactory(t, &launchConfig, "cognition.TextModel", recording, "outcome")
			launched, err := graphlaunch.New(context.Background(), launchConfig)
			if err != nil {
				t.Fatal(err)
			}
			sink := &scenarioQueuedCancelSink{scenarioAddressingSink: newScenarioAddressingSink(), audio: make(chan string, 8)}
			const sessionID = "count-admission"
			settings := legacy.Settings{Instruction: "Follow the user's standing counting instruction.",
				Voice: config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate}
			runtime, err := launched.Binding.Start(context.Background(), legacy.Options{SessionID: sessionID, Sink: sink, Settings: settings})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := runtime.Close(ctx, errors.New("count admission test complete")); err != nil {
					t.Error(err)
				}
			})
			if err := runtime.Update(context.Background(), settings); err != nil {
				t.Fatal(err)
			}
			var clock scenarioAddressingAudioClock
			driveScenarioAddressingTurn(t, runtime, sink.scenarioAddressingSink, &clock, scenarioAddressingStreamID(sessionID, 1), asr.turns[0])
			setup := recording.await(t, "semantic_admission.decision", func(e element.Envelope) bool {
				d, ok := e.Payload.(policyelements.SemanticDecision)
				return ok && d.StandingAfter == 1
			}).Payload.(policyelements.SemanticDecision)
			if !setup.Choice.Idle() || setup.StandingPinned != 1 {
				t.Fatalf("the setup utterance was not pinned silently: %+v", setup)
			}
			if model.calls.Load() != 0 || tts.plans.Load() != 0 {
				t.Fatal("standing-policy setup acquired speech authority")
			}
			animalStream := scenarioAddressingStreamID(sessionID, 2)
			for i := 0; i < 3; i++ {
				sendScenarioCountAudio(t, runtime, &clock, false)
			}
			partial := recording.await(t, "semantic_admission.decision", func(e element.Envelope) bool {
				d, ok := e.Payload.(policyelements.SemanticDecision)
				return ok && d.StreamID == animalStream && d.Event == coreinteraction.TranscriptPartial
			}).Payload.(policyelements.SemanticDecision)
			if partial.Choice.Speak != test.wantSpeech || partial.DecisionStage != "policy" ||
				partial.StandingBefore != 1 || partial.SpokeOver != test.wantSpeech {
				t.Fatalf("partial decision = %+v; want speak=%v while the person is still talking", partial, test.wantSpeech)
			}
			for i := 0; i < 5; i++ {
				sendScenarioCountAudio(t, runtime, &clock, true)
			}
			final := recording.await(t, "semantic_admission.decision", func(e element.Envelope) bool {
				d, ok := e.Payload.(policyelements.SemanticDecision)
				return ok && d.StreamID == animalStream && d.Event == coreinteraction.TranscriptFinal
			}).Payload.(policyelements.SemanticDecision)
			recording.await(t, "semantic_admission.outcome", func(e element.Envelope) bool {
				o, ok := e.Payload.(policyelements.SemanticAdmissionOutcome)
				return ok && o.StreamID == animalStream && o.SourceRevision == final.SourceRevision
			})
			if test.wantSpeech {
				if text := receiveScenarioAddressing(t, sink.audio, "first animal count at the audio sink"); text != "One." {
					t.Fatalf("audible count = %q", text)
				}
				// The final of the same words must not count the animal twice:
				// output was triggered by an earlier revision of this stream,
				// and the fixture keeps it rather than speaking again.
				if model.calls.Load() != 1 || tts.plans.Load() != 1 {
					t.Fatalf("provider/TTS calls = %d/%d, want the partial's count only", model.calls.Load(), tts.plans.Load())
				}
			} else {
				if final.Choice.Speak {
					t.Fatalf("a listening policy spoke on the final: %+v", final)
				}
				if model.calls.Load() != 0 || tts.plans.Load() != 0 || len(recording.records("semantic_admission.voice_committed")) != 0 {
					t.Fatal("a policy that listened acquired speech authority")
				}
			}
		})
	}
}

// sendScenarioCountAudio pushes one 100 ms frame of tone or silence. The fake
// recogniser reports a partial on every frame and the final once the gate has
// heard enough silence to endpoint.
func sendScenarioCountAudio(
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
		t.Fatalf("send count audio frame %d: %v", clock.index, err)
	}
}

type scenarioCountAdmissionPolicy struct {
	descriptor        policyelements.SemanticDeciderDescriptor
	primary           string
	primaryConfidence float64

	mu       sync.Mutex
	answered map[string]bool
}

func (*scenarioCountAdmissionPolicy) Name() string { return "count-admission-policy" }
func (p *scenarioCountAdmissionPolicy) Descriptor() policyelements.SemanticDeciderDescriptor {
	return p.descriptor
}

// Decide speaks when the animal is in the current words and the script says
// to, and never otherwise. While the agent is already speaking it keeps the
// output running: a second count for the same animal is what "output was
// triggered by an earlier revision of this stream" exists to prevent.
func (p *scenarioCountAdmissionPolicy) Decide(_ context.Context, d coreinteraction.Decision) (coreinteraction.Outcome, error) {
	current := scenarioAddressingCurrentEvidence(d.Evidence)
	animal := strings.Contains(current, "A capybara")
	// The rule the real policy follows: a partial that already led the agent
	// to speak for this occurrence makes its final listen. The fixture keeps
	// the exact words it answered; the fake recogniser repeats them on every
	// revision, so a later mention reads as different words.
	p.mu.Lock()
	counted := p.answered[current]
	p.mu.Unlock()
	speaking := d.Speaking
	wanted, confidence := coreinteraction.ChoiceListen, 0.99
	switch {
	case speaking:
		wanted = coreinteraction.ChoiceKeep
	case animal && !counted && p.primary != "":
		wanted, confidence = p.primary, p.primaryConfidence
		if wanted == coreinteraction.ChoiceSpeak {
			p.mu.Lock()
			if p.answered == nil {
				p.answered = map[string]bool{}
			}
			p.answered[current] = true
			p.mu.Unlock()
		}
	}
	outcome, err := scenarioAnswer(d, wanted)
	outcome.Confidence = confidence
	return outcome, err
}

func (*scenarioCountAdmissionPolicy) Generate(_ context.Context, prompt, _ string, _ int) (string, error) {
	switch prompt {
	case coreinteraction.ExtractionInstruction:
		return "pin conversation count the animals out loud as they mention them and say nothing else", nil
	case coreinteraction.StandingPolicyGroundingInstruction, coreinteraction.CountingInstruction, coreinteraction.RestrictingInstruction, coreinteraction.ArrivedInstruction:
		return "yes", nil
	case coreinteraction.ScopeInstruction:
		return "standing", nil
	default:
		return "none", nil
	}
}

type scenarioCountAdmissionModel struct {
	descriptor continuation.Descriptor
	calls      atomic.Int32
}

func (m *scenarioCountAdmissionModel) Descriptor() continuation.Descriptor { return m.descriptor }
func (m *scenarioCountAdmissionModel) Continue(_ context.Context, _ continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	m.calls.Add(1)
	return scenarioEndpointSpeak(emit, "One.")
}
