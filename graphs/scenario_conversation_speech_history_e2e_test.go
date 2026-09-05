package graphs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A model can answer a partial while its matching final, or a later unrelated
// observation, commits. Rejecting fresh action authority must not erase speech
// that reaches the listener or strand its playback-completion receipt.
func TestScenarioConversationPreservesAudibleCountAfterContextAdvances(t *testing.T) {
	for _, newerRoom := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching final", true: "later room observation"}[newerRoom], func(t *testing.T) {
			base := newScenarioProfileFixture(t)
			config := base.pluginConfig()
			config.SemanticAdmission.TranscriptEvents = &policyelements.SemanticTranscriptEventConfig{
				Partial: policyelements.SemanticTranscriptEventRules{Instruction: "Count the current animal.", TimeoutMS: 1000, Acts: []coreinteraction.Act{coreinteraction.ActStaySilent, coreinteraction.ActSpeakThrough, coreinteraction.ActKeepSpeaking}},
				Final:   policyelements.SemanticTranscriptEventRules{Instruction: "Count the current animal.", TimeoutMS: 1000, Acts: []coreinteraction.Act{coreinteraction.ActStaySilent, coreinteraction.ActAnswer, coreinteraction.ActKeepSpeaking}},
			}
			asr := &scenarioAddressingASRControl{turns: []string{"Count the animals out loud as I mention them and say nothing else.", "A capybara wandered over and sat down next to me.", "The water was calm."}}
			if !newerRoom {
				asr.turns = asr.turns[:2]
			}
			asr.turns = append(asr.turns, "A capybara joined the first one.")
			policy := &scenarioSpeechHistoryPolicy{scenarioCountAdmissionPolicy: scenarioCountAdmissionPolicy{descriptor: config.Policy.Descriptor, primary: "answer", activation: "condition-met", primaryConfidence: 0.989, activationConfidence: 0.999}}
			model := &scenarioSpeechHistoryModel{descriptor: config.Model.Descriptor, started: make(chan continuation.Request, 1), release: make(chan struct{})}
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
			instrumentScenarioAddressingFactory(t, &launchConfig, "policy.SemanticAdmission", recording, "decision")
			instrumentScenarioAddressingFactory(t, &launchConfig, "interaction.ModelResultCommit", recording, "outcome")
			launched, err := graphlaunch.New(t.Context(), launchConfig)
			if err != nil {
				t.Fatal(err)
			}
			sink := &scenarioSpeechHistorySink{scenarioQueuedCancelSink: &scenarioQueuedCancelSink{scenarioAddressingSink: newScenarioAddressingSink(), audio: make(chan string, 8)}, ended: make(chan legacy.TurnOutcome, 8)}
			const sessionID = "speech-history"
			settings := legacy.Settings{Instruction: "Follow the user's standing count.", Voice: config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate}
			runtime, err := launched.Binding.Start(t.Context(), legacy.Options{SessionID: sessionID, Sink: sink, Settings: settings})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := runtime.Close(ctx, errors.New("history test complete")); err != nil {
					t.Error(err)
				}
			})
			if err = runtime.Update(t.Context(), settings); err != nil {
				t.Fatal(err)
			}
			var clock scenarioAddressingAudioClock
			driveScenarioAddressingTurn(t, runtime, sink.scenarioAddressingSink, &clock, scenarioAddressingStreamID(sessionID, 1), asr.turns[0])
			recording.await(t, "semantic_admission.decision", func(e element.Envelope) bool {
				d, ok := e.Payload.(policyelements.SemanticDecision)
				return ok && d.StandingAfter == 1 && d.DecisionStage == "standing_coverage"
			})
			for i := 0; i < 3; i++ {
				sendScenarioSupersessionAudio(t, runtime, &clock, false)
			}
			request := receiveScenarioAddressing(t, model.started, "partial-trigger model invocation")
			for i := 0; i < 5; i++ {
				sendScenarioSupersessionAudio(t, runtime, &clock, true)
			}
			awaitSpeechHistoryObservation(t, runtime, asr.turns[1])
			if newerRoom {
				for i := 0; i < 8; i++ {
					sendScenarioSupersessionAudio(t, runtime, &clock, i >= 3)
				}
				awaitSpeechHistoryObservation(t, runtime, asr.turns[2])
			}
			if runtime.Trajectory().Version <= request.Trajectory.Version {
				t.Fatal("fixture did not advance canonical context during model work")
			}
			close(model.release)
			if text := receiveScenarioAddressing(t, sink.audio, "count at actual audio boundary"); text != "One." {
				t.Fatal(text)
			}
			outcome := recording.await(t, "model_result_commit.outcome", func(e element.Envelope) bool {
				o, ok := e.Payload.(interactionelements.ModelCommitOutcome)
				return ok && o.Kind != interactionelements.ModelIgnored
			})
			committed := outcome.Payload.(interactionelements.ModelCommitOutcome)
			if committed.Kind != interactionelements.ModelSpeechRetained || committed.Code != "stale_speech_history" {
				t.Fatalf("speech did not survive its strict freshness refusal: %+v", committed)
			}
			// Ignore empty setup turns. The exact utterance below must finish cleanly.
			end := receiveScenarioAddressing(t, sink.ended, "completed count turn")
			if end.Incomplete {
				t.Fatalf("count ended incomplete: %+v", end)
			}
			snapshot := runtime.Trajectory()
			visibility := trajectory.AssistantVisibility(snapshot)
			found := false
			for _, item := range snapshot.Items {
				if item.InvocationID != committed.RunID {
					continue
				}
				if item.Kind == trajectory.KindToolProposal || item.Kind == trajectory.KindToolCall {
					t.Fatal("stale proposal became canonical")
				}
				if item.Kind == trajectory.KindAssistant && item.Content == "One." {
					if visibility[item.ID] != trajectory.VisibilityPlayed {
						t.Fatalf("retained count not visible as played: %s", visibility[item.ID])
					}
					found = true
				}
			}
			if !found {
				t.Fatal("audible count missing from canonical history")
			}
			for i := 0; i < 3; i++ {
				sendScenarioSupersessionAudio(t, runtime, &clock, false)
			}
			next := receiveScenarioAddressing(t, model.started, "next provider context")
			nextVisibility := trajectory.AssistantVisibility(next.Trajectory)
			found = false
			for _, item := range next.Trajectory.Items {
				if item.InvocationID == committed.RunID && item.Kind == trajectory.KindAssistant &&
					item.Content == "One." && nextVisibility[item.ID] == trajectory.VisibilityPlayed {
					found = true
				}
			}
			if !found {
				t.Fatal("next provider invocation omitted the played count")
			}
		})
	}
}

func awaitSpeechHistoryObservation(t *testing.T, runtime legacy.Runtime, text string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for !scenarioEndpointHasFinalAudio(runtime.Trajectory(), text) {
		select {
		case <-deadline.C:
			t.Fatal("newer observation did not commit")
		case <-poll.C:
		}
	}
}

type scenarioSpeechHistoryPolicy struct{ scenarioCountAdmissionPolicy }

func (p *scenarioSpeechHistoryPolicy) Decide(ctx context.Context, d coreinteraction.Decision) (coreinteraction.Outcome, error) {
	result, err := p.scenarioCountAdmissionPolicy.Decide(ctx, d)
	if err == nil && result.Option == "speak-through" {
		result.Confidence = 0.989
	}
	return result, err
}

type scenarioSpeechHistoryModel struct {
	descriptor continuation.Descriptor
	started    chan continuation.Request
	release    chan struct{}
}

func (m *scenarioSpeechHistoryModel) Descriptor() continuation.Descriptor { return m.descriptor }
func (m *scenarioSpeechHistoryModel) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	select {
	case m.started <- request:
	case <-ctx.Done():
		return continuation.Completion{}, context.Cause(ctx)
	}
	select {
	case <-m.release:
	case <-ctx.Done():
		return continuation.Completion{}, context.Cause(ctx)
	}
	return scenarioEndpointSpeak(emit, "One.")
}

type scenarioSpeechHistorySink struct {
	*scenarioQueuedCancelSink
	ended chan legacy.TurnOutcome
}

func (s *scenarioSpeechHistorySink) TurnEnd(ctx context.Context, outcome legacy.TurnOutcome) error {
	s.mu.Lock()
	hasSpeech := s.speechBegins > 0
	s.mu.Unlock()
	if hasSpeech {
		s.ended <- outcome
	}
	return s.scenarioAddressingSink.TurnEnd(ctx, outcome)
}
