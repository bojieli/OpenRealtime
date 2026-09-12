package graphs_test

import (
	"context"
	"errors"
	"strings"
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

// Recognition commits partials and endpoints for both halves of the setup.
// The extractor must see one utterance, then its standing rule must govern the
// next animal and produce an actual count through the production speech graph.
func TestScenarioConversationSplitStandingInstructionIsNotRepeatedAsHistory(t *testing.T) {
	base := newScenarioProfileFixture(t)
	config := base.pluginConfig()
	config.SemanticAdmission.Rules = "Follow the standing counting instruction."
	asr := &scenarioAddressingASRControl{turns: []string{
		"Count the animals out loud as I mention them.", "And say nothing else.",
		"A capybara wandered over.",
	}}
	policy := &scenarioSplitStandingPolicy{
		scenarioCountAdmissionPolicy: scenarioCountAdmissionPolicy{
			descriptor: config.Policy.Descriptor, primary: coreinteraction.ChoiceSpeak,
			primaryConfidence: 0.99,
		},
		extraction: make(chan string, 4),
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
	instrumentScenarioAddressingFactory(t, &launchConfig, "policy.SemanticAdmission", recording, "decision")
	launched, err := graphlaunch.New(context.Background(), launchConfig)
	if err != nil {
		t.Fatal(err)
	}
	sink := &scenarioQueuedCancelSink{scenarioAddressingSink: newScenarioAddressingSink(), audio: make(chan string, 8)}
	const sessionID = "split-standing-history"
	settings := legacy.Settings{Instruction: "Follow the user's standing counting instruction.",
		Voice: config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate}
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{SessionID: sessionID, Sink: sink, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := runtime.Close(ctx, errors.New("split standing history test complete")); err != nil {
			t.Error(err)
		}
	})
	if err := runtime.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	var clock scenarioAddressingAudioClock
	for index := 0; index < 2; index++ {
		stream := scenarioAddressingStreamID(sessionID, uint64(index+1))
		driveScenarioAddressingTurn(t, runtime, sink.scenarioAddressingSink, &clock, stream, asr.turns[index])
		decisionEnvelope := recording.await(t, "semantic_admission.decision", func(envelope element.Envelope) bool {
			decision, ok := envelope.Payload.(policyelements.SemanticDecision)
			return ok && decision.StreamID == stream && decision.Event == coreinteraction.TranscriptFinal
		})
		decision := decisionEnvelope.Payload.(policyelements.SemanticDecision)
		if !decision.Choice.Idle() || decision.StandingAfter != index {
			t.Fatalf("split setup acquired speech or lost its completed rule: %+v", decision)
		}
	}
	evidence := receiveScenarioAddressing(t, policy.extraction, "split instruction extraction")
	if strings.Count(evidence, asr.turns[0]) != 1 || strings.Count(evidence, asr.turns[1]) != 1 ||
		!strings.Contains(evidence, `They just said: "`+asr.turns[0]+" "+asr.turns[1]+`"`) {
		t.Fatalf("the production extractor received duplicated or incomplete setup: %s", evidence)
	}
	if model.calls.Load() != 0 || tts.plans.Load() != 0 {
		t.Fatal("the standing setup produced a premature count")
	}
	driveScenarioAddressingTurn(t, runtime, sink.scenarioAddressingSink, &clock, scenarioAddressingStreamID(sessionID, 3), asr.turns[2])
	if got := receiveScenarioAddressing(t, sink.audio, "count after split setup"); got != "One." {
		t.Fatalf("count audio = %q", got)
	}
	if model.calls.Load() != 1 || tts.plans.Load() != 1 {
		t.Fatalf("split setup produced duplicate counts: model=%d synthesis=%d", model.calls.Load(), tts.plans.Load())
	}
}

type scenarioSplitStandingPolicy struct {
	scenarioCountAdmissionPolicy
	extraction chan string
}

func (policy *scenarioSplitStandingPolicy) Generate(ctx context.Context, prompt, evidence string, maximum int) (string, error) {
	if prompt == coreinteraction.ExtractionInstruction {
		if !strings.Contains(evidence, "And say nothing else.") || strings.Contains(evidence, "A capybara") {
			return "none", nil
		}
		select {
		case policy.extraction <- evidence:
		case <-ctx.Done():
			return "", context.Cause(ctx)
		}
	}
	return policy.scenarioCountAdmissionPolicy.Generate(ctx, prompt, evidence, maximum)
}
