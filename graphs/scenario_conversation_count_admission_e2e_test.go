package graphs_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
)

// The retained event-count failures chose answer with probability 0.651 after
// independently grounding condition-met at 0.999. A complete partial at 0.679
// had correctly acquired no speech authority. The final must recover that
// missed standing trigger just as it does when the primary policy says listen.
func TestScenarioConversationGroundedCountSurvivesUncertainPrimaryAnswer(t *testing.T) {
	for _, test := range []struct {
		name, primary, activation               string
		primaryConfidence, activationConfidence float64
		wantSpeech                              bool
	}{
		{"uncertain answer", "answer", "condition-met", 0.651, 0.999, true},
		{"confident answer", "answer", "condition-met", 0.989, 0.999, true},
		{"primary listen", "listen", "condition-met", 0.9, 0.999, true},
		{"condition not met", "answer", "wait", 0.651, 0.999, false},
		{"uncertain condition", "answer", "condition-met", 0.651, 0.6, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newScenarioProfileFixture(t)
			config := base.pluginConfig()
			config.SemanticAdmission.TranscriptEvents = &policyelements.SemanticTranscriptEventConfig{
				Partial: policyelements.SemanticTranscriptEventRules{
					Instruction: "Count only when the standing trigger arrives.", TimeoutMS: 1000,
					Acts: []coreinteraction.Act{coreinteraction.ActStaySilent, coreinteraction.ActSpeakThrough},
				},
				Final: policyelements.SemanticTranscriptEventRules{
					Instruction: "Recover an unannounced animal count.", TimeoutMS: 1000,
					Acts: []coreinteraction.Act{coreinteraction.ActStaySilent, coreinteraction.ActAnswer},
				},
			}
			asr := &scenarioAddressingASRControl{turns: []string{
				"Count the animals out loud as I mention them and say nothing else.",
				"A capybara wandered over and sat down next to me.",
			}}
			policy := &scenarioCountAdmissionPolicy{
				descriptor: config.Policy.Descriptor, primary: test.primary, activation: test.activation,
				primaryConfidence: test.primaryConfidence, activationConfidence: test.activationConfidence,
			}
			model := &scenarioCountAdmissionModel{descriptor: config.Model.Descriptor}
			tts := &scenarioAddressingTTSControl{}
			config.ASR.Factory = func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
				return &scenarioAddressingASR{control: asr, descriptor: config.ASR.Descriptor}, nil
			}
			config.Policy.Factory = func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) { return policy, nil }
			config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) { return model, nil }
			config.SilentModel.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
				return &scenarioCountAdmissionModel{descriptor: config.SilentModel.Descriptor}, nil
			}
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
			recording.await(t, "semantic_admission.decision", func(e element.Envelope) bool {
				d, ok := e.Payload.(policyelements.SemanticDecision)
				return ok && d.StandingAfter == 1 && d.DecisionStage == "standing_coverage"
			})
			animalStream := scenarioAddressingStreamID(sessionID, 2)
			for i := 0; i < 3; i++ {
				sendScenarioSupersessionAudio(t, runtime, &clock, false)
			}
			recording.await(t, "semantic_admission.decision", func(e element.Envelope) bool {
				d, ok := e.Payload.(policyelements.SemanticDecision)
				return ok && d.StreamID == animalStream && d.DecisionStage == "confidence_guard"
			})
			if model.calls.Load() != 0 || tts.plans.Load() != 0 {
				t.Fatal("uncertain partial or standing-policy setup acquired speech authority")
			}
			for i := 0; i < 5; i++ {
				sendScenarioSupersessionAudio(t, runtime, &clock, true)
			}
			final := recording.await(t, "semantic_admission.decision", func(e element.Envelope) bool {
				d, ok := e.Payload.(policyelements.SemanticDecision)
				return ok && d.StreamID == animalStream && d.Activation != ""
			})
			decision := final.Payload.(policyelements.SemanticDecision)
			wantAct := coreinteraction.ActStaySilent
			if test.wantSpeech {
				wantAct = coreinteraction.ActAnswer
			}
			if decision.Act != wantAct {
				t.Fatalf("final decision = %+v; want %s (provider calls=%d)", decision, wantAct, model.calls.Load())
			}
			if test.wantSpeech && test.primaryConfidence < 0.7 &&
				(decision.DecisionStage != "voice_activation" || !decision.Measured ||
					decision.Confidence != test.activationConfidence || decision.StandingBefore != 1) {
				t.Fatalf("recovered count did not retain its independent standing-trigger authority: %+v", decision)
			}
			recording.await(t, "semantic_admission.outcome", func(e element.Envelope) bool {
				o, ok := e.Payload.(policyelements.SemanticAdmissionOutcome)
				return ok && o.DecisionItemID == final.ItemID
			})
			if test.wantSpeech {
				if text := receiveScenarioAddressing(t, sink.audio, "first animal count at the audio sink"); text != "One." {
					t.Fatalf("audible count = %q", text)
				}
				if model.calls.Load() != 1 || tts.plans.Load() != 1 {
					t.Fatalf("provider/TTS calls = %d/%d", model.calls.Load(), tts.plans.Load())
				}
			} else if model.calls.Load() != 0 || tts.plans.Load() != 0 || len(recording.records("semantic_admission.voice_committed")) != 0 {
				t.Fatal("unverified condition acquired speech authority")
			}
		})
	}
}

type scenarioCountAdmissionPolicy struct {
	descriptor                              policyelements.SemanticDeciderDescriptor
	primary, activation                     string
	primaryConfidence, activationConfidence float64
}

func (*scenarioCountAdmissionPolicy) Name() string { return "count-admission-policy" }
func (p *scenarioCountAdmissionPolicy) Descriptor() policyelements.SemanticDeciderDescriptor {
	return p.descriptor
}
func (p *scenarioCountAdmissionPolicy) Decide(_ context.Context, d coreinteraction.Decision) (coreinteraction.Outcome, error) {
	animal := strings.Contains(scenarioAddressingCurrentEvidence(d.Evidence), "A capybara")
	wanted, confidence := "listen", 0.99
	switch {
	case slices.Contains(d.Options, "condition-met"):
		wanted = "wait"
		if animal {
			wanted, confidence = p.activation, p.activationConfidence
		}
	case slices.Contains(d.Options, "covered"):
		wanted = "covered"
	case slices.Contains(d.Options, "direct-request"):
		wanted = "wait"
	case slices.Contains(d.Options, "speak-through"):
		if animal {
			wanted, confidence = "speak-through", 0.679
		}
	case slices.Contains(d.Options, "answer"):
		if animal {
			wanted, confidence = p.primary, p.primaryConfidence
		}
	}
	index := slices.Index(d.Options, wanted)
	if index < 0 {
		return coreinteraction.Outcome{}, fmt.Errorf("no scripted choice %q in %v", wanted, d.Options)
	}
	return coreinteraction.Outcome{Index: index, Option: wanted, Confidence: confidence, Measured: true}, nil
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
