package interaction_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

func TestParsePinReadsEachForm(t *testing.T) {
	for _, testCase := range []struct {
		answer string
		kind   string
		text   string
		scope  interaction.Scope
	}{
		{"none", "none", "", ""},
		{"pin conversation count them out loud", "pin", "count them out loud", interaction.ScopeConversation},
		{"pin turn do not reply until they finish", "pin", "do not reply until they finish", interaction.ScopeTurn},
		{"revoke tell them when the kettle boils", "revoke", "tell them when the kettle boils", ""},
	} {
		kind, pinned, ok := interaction.ParsePin(testCase.answer)
		if !ok || kind != testCase.kind {
			t.Fatalf("%q parsed as (%q, %v)", testCase.answer, kind, ok)
		}
		if pinned.Text != testCase.text || pinned.Scope != testCase.scope {
			t.Fatalf("%q gave %+v", testCase.answer, pinned)
		}
	}
}

// A pass that invents a policy nobody set is worse than one that misses.
func TestParsePinRefusesToGuess(t *testing.T) {
	for _, answer := range []string{"", "maybe pin this?", "I think they set a policy", "pin"} {
		if _, _, ok := interaction.ParsePin(answer); ok {
			t.Fatalf("%q was accepted as an answer", answer)
		}
	}
}

// A revocation is only recognisable against the thing it lifts.
func TestRenderForExtractionShowsWhatIsInForce(t *testing.T) {
	block := interaction.RenderForExtraction([]interaction.StandingInstruction{
		{Text: "tell them when the kettle has boiled", Scope: interaction.ScopeConversation},
	}, nil, "never mind about the kettle")
	if !strings.Contains(block, "tell them when the kettle has boiled") {
		t.Fatalf("the policy being lifted was not shown:\n%s", block)
	}
	if !strings.Contains(block, "never mind about the kettle") {
		t.Fatalf("the utterance was not shown:\n%s", block)
	}
	empty := interaction.RenderForExtraction(nil, nil, "hello")
	if !strings.Contains(empty, "No policies") {
		t.Fatalf("an empty policy list must say so rather than omit the section:\n%s", empty)
	}
}

// A policy that names a delay waits on something that has not happened yet,
// and is standing by construction. It must not be asked about, because the
// delay is lifted out of the text before anything else reads it: "if I go
// quiet for fifteen seconds, ask whether I'm still there" arrives as "ask
// whether they are still there", which reads as a request about this moment.
func TestADelayedPolicyStandsWithoutBeingAsked(t *testing.T) {
	kind, instruction, ok := interaction.ParsePin(
		"pin turn after 15s ask whether they are still there")
	if !ok || kind != "pin" {
		t.Fatalf("the pin did not parse: %q %v", kind, ok)
	}
	if instruction.After == 0 {
		t.Fatal("the delay was not read out of the policy")
	}
}
