package interaction_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestPolicyConversationDistinguishesPlayedWordsFromDrafts(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		visibility              trajectory.Visibility
		mark                    *spoken.Mark
		wantSpeech, wantUnheard string
	}{
		{"partial", trajectory.VisibilityPlayed, &spoken.Mark{Spoken: "Find the order", Cut: "number", Pending: "number and purchase email.", Measured: true}, "Find the order", "number and purchase email."},
		{"nothing-played", trajectory.VisibilityPlayed, &spoken.Mark{Pending: "Find the order number and purchase email.", Measured: true}, "", "Find the order number and purchase email."},
		{"canceled", trajectory.VisibilityCancelled, nil, "", "Find the order number and purchase email."},
		{"prepared", trajectory.VisibilityPrepared, nil, "", "Find the order number and purchase email."},
		{"queued", trajectory.VisibilityQueued, nil, "", "Find the order number and purchase email."},
		{"played", trajectory.VisibilityPlayed, &spoken.Mark{Spoken: "Find the order number and purchase email.", Measured: true}, "Find the order number and purchase email.", ""},
		{"unmeasured-text", trajectory.VisibilityPlayed, nil, "Find the order number and purchase email.", ""},
		{"legacy", "", nil, "Find the order number and purchase email.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := []trajectory.Item{
				{ID: "user", Kind: trajectory.KindObservation, Content: "Explain the refund process.", Producer: trajectory.Producer{Phase: trajectory.PhaseUser}},
				{ID: "answer", Kind: trajectory.KindAssistant, Content: "Find the order number and purchase email.", Visibility: tc.visibility},
			}
			if tc.mark != nil {
				items = append(items, trajectory.Item{ID: "played", Kind: trajectory.KindAssistantState, AssistantState: &trajectory.AssistantState{AssistantItemID: "answer", Visibility: tc.visibility, Heard: tc.mark}})
			}
			for name, lines := range map[string][]string{"window": (&interaction.Window{}).Lines(items), "recent": interaction.RecentLines(items, 8)} {
				joined := strings.Join(lines, "\n")
				var speech []string
				for _, line := range strings.Split(joined, "\n") {
					if strings.HasPrefix(line, "agent: ") {
						speech = append(speech, strings.TrimPrefix(line, "agent: "))
					}
				}
				if strings.Join(speech, " ") != tc.wantSpeech {
					t.Fatalf("%s reports unheard words as speech: %q", name, joined)
				}
				if tc.wantUnheard != "" && !strings.Contains(joined, tc.wantUnheard) {
					t.Fatalf("%s lost the draft needed for continuation: %q", name, joined)
				}
			}
		})
	}
}

func TestPolicyConversationAppliesLaterPlaybackCorrectionsBeforeTruncation(t *testing.T) {
	items := []trajectory.Item{
		{ID: "user", Kind: trajectory.KindObservation, Content: strings.Repeat("Older conversation. ", 20)},
		{ID: "answer", Kind: trajectory.KindAssistant, Content: "One. Two. Three.", Visibility: trajectory.VisibilityPrepared},
	}
	window := &interaction.Window{LowerBudget: 25, UpperBudget: 40}
	for _, tc := range []struct {
		id, want   string
		visibility trajectory.Visibility
		mark       *spoken.Mark
	}{
		{"queued", "prepared (not yet said): One. Two. Three.", trajectory.VisibilityQueued, nil},
		{"server-boundary", "agent: One. Two.\nprepared (not heard): Three.", trajectory.VisibilityPlayed, &spoken.Mark{Spoken: "One. Two.", Pending: "Three."}},
		{"client-correction", "agent: One.\nplayback stopped during: \"Two\"\nprepared (not heard): Two. Three.", trajectory.VisibilityPlayed, &spoken.Mark{Spoken: "One.", Cut: "Two", Pending: "Two. Three.", Measured: true}},
		{"complete", "agent: One. Two. Three.", trajectory.VisibilityPlayed, &spoken.Mark{Spoken: "One. Two. Three.", Measured: true}},
	} {
		items = append(items, trajectory.Item{ID: tc.id, Kind: trajectory.KindAssistantState, AssistantState: &trajectory.AssistantState{
			AssistantItemID: "answer", Visibility: tc.visibility, Heard: tc.mark,
		}})
		for name, lines := range map[string][]string{"window": window.Lines(items), "recent": interaction.RecentLines(items, 1)} {
			if len(lines) != 1 || lines[0] != tc.want {
				t.Fatalf("%s %s lost the latest playback boundary after truncation: %q", tc.id, name, lines)
			}
		}
	}
}

func TestPolicyConversationKeepsSilentOutputSeparateFromPlayback(t *testing.T) {
	items := []trajectory.Item{
		{ID: "background", Kind: trajectory.KindAssistant, Content: "The balance is 412 pounds.", Producer: trajectory.Producer{SpeechAuthority: "silent"}},
		{ID: "state", Kind: trajectory.KindAssistantState, AssistantState: &trajectory.AssistantState{
			AssistantItemID: "background", Visibility: trajectory.VisibilityPrepared,
			Heard: &spoken.Mark{Pending: "The balance is 412 pounds."},
		}},
	}
	for name, lines := range map[string][]string{"window": (&interaction.Window{}).Lines(items), "recent": interaction.RecentLines(items, 1)} {
		if len(lines) != 1 || lines[0] != "background (not said out loud): The balance is 412 pounds." {
			t.Fatalf("%s treated silent reasoning as a speech draft: %q", name, lines)
		}
	}
}

func TestPlaybackProjectionPreservesWindowPartialCompaction(t *testing.T) {
	items := []trajectory.Item{
		{ID: "partial", Kind: trajectory.KindObservation, Content: "A capybara walked"},
		{ID: "reasoning", Kind: trajectory.KindReasoning, Content: "Wait for the final request."},
		{ID: "final", Kind: trajectory.KindObservation, Content: "A capybara walked past."},
		{ID: "answer", Kind: trajectory.KindAssistant, Content: "One.", Visibility: trajectory.VisibilityPlayed},
	}
	lines := (&interaction.Window{}).Lines(items)
	if len(lines) != 2 || lines[0] != "user: A capybara walked past." || lines[1] != "agent: One." {
		t.Fatalf("playback projection made one animal look like two mentions: %q", lines)
	}
}
