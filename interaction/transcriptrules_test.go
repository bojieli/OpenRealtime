package interaction_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// A transcript-event policy is what decides whether the agent may speak on a
// partial transcript or only on a final one, and its rules are validated once
// at construction. Three of those refusals had no coverage, so a policy could
// have been built with no instruction, with a single act -- which is not a
// choice at all -- or with the same act offered twice.
func validTranscriptOptions() interaction.TranscriptEventOptions {
	return interaction.TranscriptEventOptions{
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
	}
}

func TestTranscriptEventRulesRefuseUnusableActSets(t *testing.T) {
	t.Parallel()
	decider := &transcriptDecider{}
	if _, err := interaction.NewTranscriptEventPolicy(decider, validTranscriptOptions()); err != nil {
		t.Fatalf("valid options = %v, want accepted", err)
	}
	for _, test := range []struct {
		name string
		edit func(*interaction.TranscriptEventOptions)
		want string
	}{
		{
			name: "partial rules carry no instruction",
			edit: func(options *interaction.TranscriptEventOptions) {
				options.Partial.Instruction = "   "
			},
			want: "partial transcript rules require an instruction",
		},
		{
			name: "final rules carry no instruction",
			edit: func(options *interaction.TranscriptEventOptions) {
				options.Final.Instruction = ""
			},
			want: "final transcript rules require an instruction",
		},
		{
			// One act is not a decision. A policy offered a single option has
			// nothing to decide and would always return it.
			name: "partial rules offer a single act",
			edit: func(options *interaction.TranscriptEventOptions) {
				options.Partial.Acts = []interaction.Act{interaction.ActStaySilent}
			},
			want: "partial transcript rules require at least two acts",
		},
		{
			name: "final rules offer no acts",
			edit: func(options *interaction.TranscriptEventOptions) {
				options.Final.Acts = nil
			},
			want: "final transcript rules require at least two acts",
		},
		{
			name: "partial rules repeat an act",
			edit: func(options *interaction.TranscriptEventOptions) {
				options.Partial.Acts = []interaction.Act{
					interaction.ActStaySilent, interaction.ActInterrupt, interaction.ActStaySilent,
				}
			},
			want: "act",
		},
		{
			// Answering is a final-transcript act: a partial transcript is not
			// a turn yet, so a policy that could answer on one would speak
			// before the person had finished.
			name: "partial rules offer an act reserved for a final transcript",
			edit: func(options *interaction.TranscriptEventOptions) {
				options.Partial.Acts = []interaction.Act{
					interaction.ActStaySilent, interaction.ActAnswer,
				}
			},
			want: "is not valid for a partial transcript event",
		},
		{
			name: "final rules offer an act reserved for a partial transcript",
			edit: func(options *interaction.TranscriptEventOptions) {
				options.Final.Acts = []interaction.Act{
					interaction.ActStaySilent, interaction.ActSpeakThrough,
				}
			},
			want: "is not valid for a final transcript event",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := validTranscriptOptions()
			test.edit(&options)
			_, err := interaction.NewTranscriptEventPolicy(decider, options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("policy error = %v, want one containing %q", err, test.want)
			}
		})
	}

	if _, err := interaction.NewTranscriptEventPolicy(nil, validTranscriptOptions()); err == nil ||
		!strings.Contains(err.Error(), "requires a decider") {
		t.Fatalf("policy without a decider = %v, want a refusal", err)
	}
}
