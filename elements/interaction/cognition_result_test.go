package interaction

import (
	"strings"
	"testing"

	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
)

// validateCognitionResult is the check that a model's result is internally
// coherent before any of it is committed to the canonical record. Six of its
// eight refusals had no coverage, so a result whose ordered outputs disagreed
// with its own aggregate text, or whose context version and tail contradicted
// each other, would have been committed as history.
func TestCognitionResultRefusesIncoherentContextAndOutputs(t *testing.T) {
	t.Parallel()
	coherent := modelResult("run-1", 3, "tail-item", false, false)
	if err := validateCognitionResult(coherent); err != nil {
		t.Fatalf("coherent result = %v, want accepted", err)
	}
	if err := validateCognitionResult(modelResult("run-2", 3, "tail-item", false, true)); err != nil {
		t.Fatalf("coherent tool result = %v, want accepted", err)
	}

	for _, test := range []struct {
		name string
		edit func(*cognitionelements.Result)
		want string
	}{
		{
			name: "empty context names a tail item",
			edit: func(result *cognitionelements.Result) {
				result.ContextVersion = 0
				result.ContextTailID = "tail-item"
			},
			want: "empty context cannot name a tail item",
		},
		{
			name: "non-empty context has no tail item",
			edit: func(result *cognitionelements.Result) { result.ContextTailID = "  " },
			want: "non-empty context requires its tail item ID",
		},
		{
			name: "reasoning output carries a proposal",
			edit: func(result *cognitionelements.Result) {
				proposal := toolProposalFixture()
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedReasoning, Text: "thinking", Proposal: &proposal,
				}}
			},
			want: "reasoning output 0 requires text and no proposal",
		},
		{
			name: "reasoning output carries no text",
			edit: func(result *cognitionelements.Result) {
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedReasoning,
				}}
			},
			want: "reasoning output 0 requires text and no proposal",
		},
		{
			name: "assistant output carries a proposal",
			edit: func(result *cognitionelements.Result) {
				proposal := toolProposalFixture()
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedAssistant, Text: "hello", Proposal: &proposal,
				}}
				result.AssistantText = "hello"
			},
			want: "assistant output 0 requires text and no proposal",
		},
		{
			name: "assistant output carries no text",
			edit: func(result *cognitionelements.Result) {
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedAssistant,
				}}
			},
			want: "assistant output 0 requires text and no proposal",
		},
		{
			name: "tool output carries no proposal",
			edit: func(result *cognitionelements.Result) {
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedTool,
				}}
			},
			want: "tool output 0 requires a proposal and no text",
		},
		{
			name: "tool output also carries text",
			edit: func(result *cognitionelements.Result) {
				proposal := toolProposalFixture()
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedTool, Text: "hello", Proposal: &proposal,
				}}
			},
			want: "tool output 0 requires a proposal and no text",
		},
		{
			name: "output kind is not one of the prepared kinds",
			edit: func(result *cognitionelements.Result) {
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedOutputKind("speech"), Text: "hello",
				}}
			},
			want: "unknown kind",
		},
		{
			// The aggregate is what downstream reads; the ordered outputs are
			// what actually happened. If they disagree, one of them is a lie
			// and the record must not take either.
			name: "aggregate assistant text disagrees with the ordered outputs",
			edit: func(result *cognitionelements.Result) {
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedAssistant, Text: "hello",
				}}
				result.AssistantText = "goodbye"
			},
			want: "disagree with aggregate assistant or reasoning text",
		},
		{
			name: "aggregate reasoning text disagrees with the ordered outputs",
			edit: func(result *cognitionelements.Result) {
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedReasoning, Text: "because",
				}}
				result.ReasoningText = "for another reason"
			},
			want: "disagree with aggregate assistant or reasoning text",
		},
		{
			name: "aggregate tool proposals disagree with the ordered outputs",
			edit: func(result *cognitionelements.Result) {
				proposal := toolProposalFixture()
				result.Outputs = []cognitionelements.PreparedOutput{{
					Kind: cognitionelements.PreparedTool, Proposal: &proposal,
				}}
				result.AssistantText = ""
				result.ReasoningText = ""
				result.ToolProposals = nil
			},
			want: "disagree with aggregate tool proposals",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := modelResult("run-x", 3, "tail-item", false, false)
			test.edit(&result)
			err := validateCognitionResult(result)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("result error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func toolProposalFixture() cognitionelements.ToolProposal {
	return modelResult("proposal-run", 3, "tail-item", false, true).ToolProposals[0]
}
