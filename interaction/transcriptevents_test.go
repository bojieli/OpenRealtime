package interaction_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

type transcriptDecider struct {
	decisions []interaction.Decision
}

func (decider *transcriptDecider) Name() string { return "event-test" }

func (decider *transcriptDecider) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	decider.decisions = append(decider.decisions, decision)
	choice := string(interaction.ActStaySilent)
	if decision.Prompt == "partial rules" {
		choice = string(interaction.ActSpeakThrough)
	}
	for index, option := range decision.Options {
		if option == choice {
			return interaction.Outcome{Index: index, Option: option}, nil
		}
	}
	return interaction.Outcome{Option: decision.Options[0]}, nil
}

func TestTranscriptEventsUseDifferentRulesEvidenceAndActs(t *testing.T) {
	decider := &transcriptDecider{}
	policy, err := interaction.NewTranscriptEventPolicy(decider, interaction.TranscriptEventOptions{
		Partial: interaction.TranscriptEventRules{
			Instruction: "partial rules",
			Acts: []interaction.Act{
				interaction.ActStaySilent, interaction.ActSpeakThrough, interaction.ActInterrupt,
			},
		},
		Final: interaction.TranscriptEventRules{
			Instruction: "final rules",
			Acts:        []interaction.Act{interaction.ActStaySilent, interaction.ActAnswer},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	partialState := interaction.Situation{
		Speaker: "user", Speaking: true, Heard: "please translate this as I",
	}
	act, _, err := policy.Decide(context.Background(), interaction.TranscriptPartial, partialState)
	if err != nil {
		t.Fatal(err)
	}
	if act != interaction.ActSpeakThrough {
		t.Fatalf("partial rules chose %q", act)
	}
	finalState := interaction.Situation{Speaker: "user", Heard: "what time is it"}
	act, _, err = policy.Decide(context.Background(), interaction.TranscriptFinal, finalState)
	if err != nil {
		t.Fatal(err)
	}
	if act != interaction.ActStaySilent {
		t.Fatalf("final rules chose %q", act)
	}
	if len(decider.decisions) != 2 {
		t.Fatalf("got %d decisions, want a partial and a final", len(decider.decisions))
	}
	partial, final := decider.decisions[0], decider.decisions[1]
	if partial.Prompt != "partial rules" || final.Prompt != "final rules" {
		t.Fatalf("event rules crossed: partial=%q final=%q", partial.Prompt, final.Prompt)
	}
	if !strings.Contains(partial.Evidence, "transcript event: partial") ||
		strings.Contains(partial.Evidence, "transcript event: final") {
		t.Fatalf("partial event is not explicitly marked:\n%s", partial.Evidence)
	}
	if !strings.Contains(final.Evidence, "transcript event: final") ||
		strings.Contains(final.Evidence, "transcript event: partial") {
		t.Fatalf("final event is not explicitly marked:\n%s", final.Evidence)
	}
	if strings.Join(partial.Options, ",") != "listen,speak-through,interrupt" {
		t.Fatalf("partial acts = %v", partial.Options)
	}
	if strings.Join(final.Options, ",") != "listen,answer" {
		t.Fatalf("final acts = %v", final.Options)
	}
	if got := policy.AllowedActs(interaction.TranscriptPartial); len(got) != 3 ||
		strings.Join([]string{string(got[0]), string(got[1]), string(got[2])}, ",") !=
			"listen,speak-through,interrupt" {
		t.Fatalf("reported partial boundary = %v", got)
	}
}

func TestTranscriptEventPolicyDoesNotChangeTheOrdinarySituation(t *testing.T) {
	rendered := (interaction.Situation{Heard: "hello"}).Render()
	if strings.Contains(rendered, "transcript event:") {
		t.Fatalf("the existing observation space changed without selecting the policy:\n%s", rendered)
	}
}

func TestTranscriptEventsUseTheOverlapClassifierForOrdinaryActiveOutput(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		heard    string
		evidence interaction.OverlapEvidence
		want     interaction.Act
	}{
		{
			name: "floor taking opener", heard: "Hold on, what about tomorrow",
			evidence: interaction.OverlapDirected, want: interaction.ActStopSpeaking,
		},
		{
			name: "other addressee", heard: "Maria, could you close the window",
			evidence: interaction.OverlapSide, want: interaction.ActKeepSpeaking,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			decider := &recordingDecider{answer: string(testCase.evidence), confidence: 0.9}
			policy, err := interaction.NewTranscriptEventPolicy(
				decider, activeOutputTranscriptOptions(),
			)
			if err != nil {
				t.Fatal(err)
			}
			state := interaction.Situation{
				AgentSpeaking: true,
				AgentSaying:   "The answer that is currently audible.",
				Speaker:       "user",
				Speaking:      true,
				Heard:         testCase.heard,
			}
			act, outcome, err := policy.Decide(
				context.Background(), interaction.TranscriptPartial, state,
			)
			if err != nil {
				t.Fatal(err)
			}
			if act != testCase.want || outcome.Option != string(testCase.want) {
				t.Fatalf("active-output act = %q, outcome=%+v; want %q", act, outcome, testCase.want)
			}
			decisions := decider.decisions()
			if len(decisions) != 1 ||
				!strings.HasPrefix(decisions[0].Prompt, "An agent is speaking.") ||
				!strings.Contains(decisions[0].Evidence, testCase.heard) {
				t.Fatalf("active-output classifier decisions = %+v", decisions)
			}
		})
	}
}

func TestTranscriptEventsValidateBackchannelsBeforeKeepingActiveOutput(t *testing.T) {
	decider := &sequencedDecider{answers: []string{
		string(interaction.OverlapBackchannel), "valid_backchannel",
	}}
	policy, err := interaction.NewTranscriptEventPolicy(decider, activeOutputTranscriptOptions())
	if err != nil {
		t.Fatal(err)
	}
	act, _, err := policy.Decide(context.Background(), interaction.TranscriptPartial, interaction.Situation{
		AgentSpeaking: true, AgentSaying: "A longer answer.",
		Speaker: "user", Speaking: true, Heard: "mhmm yeah",
	})
	if err != nil {
		t.Fatal(err)
	}
	if act != interaction.ActKeepSpeaking {
		t.Fatalf("validated backchannel act = %q", act)
	}
	decisions := decider.decisions()
	if len(decisions) != 2 ||
		!strings.Contains(decisions[1].Prompt, "Validate a proposed listener backchannel") {
		t.Fatalf("backchannel validation decisions = %+v", decisions)
	}
}

func TestTranscriptEventsFallBackForAmbiguousOrProtectedActiveOutput(t *testing.T) {
	t.Run("ambiguous", func(t *testing.T) {
		decider := &sequencedDecider{answers: []string{
			string(interaction.OverlapAmbiguous), string(interaction.ActKeepSpeaking),
		}}
		policy, err := interaction.NewTranscriptEventPolicy(decider, activeOutputTranscriptOptions())
		if err != nil {
			t.Fatal(err)
		}
		act, _, err := policy.Decide(context.Background(), interaction.TranscriptPartial, interaction.Situation{
			AgentSpeaking: true, AgentSaying: "A longer answer.",
			Speaker: "user", Speaking: true, Heard: "The",
		})
		if err != nil {
			t.Fatal(err)
		}
		if act != interaction.ActKeepSpeaking {
			t.Fatalf("ambiguous fallback act = %q", act)
		}
		decisions := decider.decisions()
		if len(decisions) != 2 || decisions[1].Prompt != "partial rules" {
			t.Fatalf("ambiguous fallback decisions = %+v", decisions)
		}
	})

	t.Run("protected same stream", func(t *testing.T) {
		decider := &recordingDecider{answer: string(interaction.ActKeepSpeaking)}
		policy, err := interaction.NewTranscriptEventPolicy(decider, activeOutputTranscriptOptions())
		if err != nil {
			t.Fatal(err)
		}
		act, _, err := policy.Decide(context.Background(), interaction.TranscriptPartial, interaction.Situation{
			AgentSpeaking: true, AgentOutputProtected: true,
			AgentSaying: "The correction that was deliberately started.",
			Speaker:     "user", Speaking: true, Heard: "which gives us",
		})
		if err != nil {
			t.Fatal(err)
		}
		if act != interaction.ActKeepSpeaking {
			t.Fatalf("protected-stream act = %q", act)
		}
		decisions := decider.decisions()
		if len(decisions) != 1 || decisions[0].Prompt != "partial rules" {
			t.Fatalf("protected stream left the event policy: %+v", decisions)
		}
	})
}

func activeOutputTranscriptOptions() interaction.TranscriptEventOptions {
	return interaction.TranscriptEventOptions{
		Partial: interaction.TranscriptEventRules{
			Instruction: "partial rules",
			Acts: []interaction.Act{
				interaction.ActStaySilent, interaction.ActSpeakThrough,
				interaction.ActInterrupt, interaction.ActKeepSpeaking,
				interaction.ActStopSpeaking,
			},
		},
		Final: interaction.TranscriptEventRules{
			Instruction: "final rules",
			Acts: []interaction.Act{
				interaction.ActStaySilent, interaction.ActAnswer,
				interaction.ActKeepSpeaking, interaction.ActStopSpeaking,
			},
		},
	}
}

func TestTranscriptEventRulesRejectActsFromTheOtherObservationSpace(t *testing.T) {
	decider := &transcriptDecider{}
	base := interaction.TranscriptEventOptions{
		Partial: interaction.TranscriptEventRules{
			Instruction: "partial", Acts: []interaction.Act{interaction.ActStaySilent, interaction.ActSpeakThrough},
		},
		Final: interaction.TranscriptEventRules{
			Instruction: "final", Acts: []interaction.Act{interaction.ActStaySilent, interaction.ActAnswer},
		},
	}
	invalidPartial := base
	invalidPartial.Partial.Acts = []interaction.Act{interaction.ActStaySilent, interaction.ActAnswer}
	if _, err := interaction.NewTranscriptEventPolicy(decider, invalidPartial); err == nil {
		t.Fatal("answer must not be available on a provisional transcript")
	}
	invalidFinal := base
	invalidFinal.Final.Acts = []interaction.Act{interaction.ActStaySilent, interaction.ActInterrupt}
	if _, err := interaction.NewTranscriptEventPolicy(decider, invalidFinal); err == nil {
		t.Fatal("interrupt must not be available after the utterance is final")
	}
}
