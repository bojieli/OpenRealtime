package interaction_test

import (
	"context"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
)

type overlapAnswers struct{ act interaction.Act }

func (answers overlapAnswers) Name() string { return "overlap-answers" }

func (answers overlapAnswers) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	return interaction.Outcome{Option: string(answers.act)}, nil
}

type boundedOverlapAnswers struct{}

func (boundedOverlapAnswers) Name() string { return "bounded-overlap" }

func (boundedOverlapAnswers) DecisionTimeout() time.Duration { return 250 * time.Millisecond }

func (boundedOverlapAnswers) Decide(
	ctx context.Context, _ interaction.Decision,
) (interaction.Outcome, error) {
	select {
	case <-ctx.Done():
		return interaction.Outcome{}, ctx.Err()
	case <-time.After(175 * time.Millisecond):
		return interaction.Outcome{Option: string(interaction.ActStopSpeaking)}, nil
	}
}

func TestActBargeInUsesTheWholeActVocabulary(t *testing.T) {
	model, err := interaction.NewInteractionModel(overlapAnswers{act: interaction.ActStopSpeaking})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := interaction.NewActBargeIn(model, interaction.ActBargeInOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state := interaction.Situation{
		AgentSpeaking: true, AgentSaying: "the answer so far",
		Speaking: true, Heard: "no, that is wrong",
	}
	decision := policy.Decide(interaction.BargeInInput{Context: interaction.Context{
		Duplex: session.Snapshot{AgentSpeaking: true, UserSpeaking: true}, Situation: &state,
	}})
	if !decision.Cancel {
		t.Fatalf("stop-speaking did not cancel output: %+v", decision)
	}
}

func TestActBargeInKeepsSpeakingAtBareAcousticOnset(t *testing.T) {
	model, err := interaction.NewInteractionModel(overlapAnswers{act: interaction.ActStopSpeaking})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := interaction.NewActBargeIn(model, interaction.ActBargeInOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// With no transcript there is no decidable evidence. The model wrapper must
	// return inertia without calling its decider, which is keep-speaking here.
	state := interaction.Situation{AgentSpeaking: true, Speaking: true}
	decision := policy.Decide(interaction.BargeInInput{Context: interaction.Context{
		Duplex: session.Snapshot{AgentSpeaking: true, UserSpeaking: true}, Situation: &state,
	}})
	if decision.Cancel {
		t.Fatalf("bare acoustic onset cancelled speech: %+v", decision)
	}
}

func TestActBargeInUsesTheAttestedModelDeadline(t *testing.T) {
	model, err := interaction.NewInteractionModel(boundedOverlapAnswers{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := interaction.NewActBargeIn(model, interaction.ActBargeInOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state := interaction.Situation{AgentSpeaking: true, Speaking: true, Heard: "stop"}
	decision := policy.Decide(interaction.BargeInInput{Context: interaction.Context{
		Duplex: session.Snapshot{AgentSpeaking: true, UserSpeaking: true}, Situation: &state,
	}})
	if !decision.Cancel {
		t.Fatalf("the wrapper silently shortened the attested 250ms deadline: %+v", decision)
	}
}
