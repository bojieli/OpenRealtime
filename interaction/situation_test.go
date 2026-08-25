package interaction_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// An act must never be offered with no way to perform it.
func TestAvailableActsOnlyOffersCallToolWhenAToolExists(t *testing.T) {
	bare := interaction.Situation{}.AvailableActs()
	for _, act := range bare {
		if act == interaction.ActCallTool {
			t.Fatal("call-tool offered with no tool named; the phone-menu case failed exactly this way")
		}
	}
	armed := interaction.Situation{Tools: []string{"press_key(digit)"}}.AvailableActs()
	if !contains(armed, interaction.ActCallTool) {
		t.Fatal("a tool exists and call-tool was not offered")
	}
}

// An agent that is not speaking cannot keep speaking, and one mid-sentence is
// not choosing whether to start.
func TestAvailableActsDependOnWhetherTheAgentIsSpeaking(t *testing.T) {
	silent := interaction.Situation{}.AvailableActs()
	if contains(silent, interaction.ActKeepSpeaking) || contains(silent, interaction.ActStopSpeaking) {
		t.Fatalf("a silent agent was offered an act about its own speech: %v", silent)
	}
	talking := interaction.Situation{AgentSpeaking: true}.AvailableActs()
	if contains(talking, interaction.ActStaySilent) || contains(talking, interaction.ActAnswer) {
		t.Fatalf("a speaking agent was offered an act about starting: %v", talking)
	}
}

// Collapsing these hid every completed utterance the instant its speaker
// stopped: a model told a turn had ended and never told what the turn said.
func TestRenderKeepsWhatWasHeardAfterTheSpeakerStops(t *testing.T) {
	block := interaction.Situation{
		Heard: "I'd like to cancel my subscription", Silence: "1800ms",
	}.Render()
	if !strings.Contains(block, "cancel my subscription") {
		t.Fatalf("a finished utterance vanished when its speaker stopped:\n%s", block)
	}
	if !strings.Contains(block, "stopped speaking 1800ms ago") {
		t.Fatalf("the render did not say they had stopped:\n%s", block)
	}
	live := interaction.Situation{Speaker: "other", Speaking: true, Heard: "press 2 for"}.Render()
	if !strings.Contains(live, "other: speaking right now") {
		t.Fatalf("a live speaker was not reported as speaking:\n%s", live)
	}
}

// Truncating the conversation window must never silently repeal a policy, so
// pins are rendered outside it.
func TestRenderPutsStandingInstructionsAheadOfTheWindow(t *testing.T) {
	block := interaction.Situation{
		Pins:   []string{"count them out loud (2m ago)"},
		Recent: []string{"user: anyway, as I was saying"},
	}.Render()
	pin := strings.Index(block, "count them out loud")
	window := strings.Index(block, "as I was saying")
	if pin < 0 || window < 0 {
		t.Fatalf("pin or window missing:\n%s", block)
	}
	if pin > window {
		t.Fatalf("a standing instruction was rendered inside the window that truncates:\n%s", block)
	}
}

func TestInstructionCarriesEveryActItOffers(t *testing.T) {
	for _, act := range []interaction.Act{
		interaction.ActStaySilent, interaction.ActSpeakThrough, interaction.ActAnswer,
		interaction.ActInterrupt, interaction.ActCallTool,
		interaction.ActKeepSpeaking, interaction.ActStopSpeaking,
	} {
		if !strings.Contains(interaction.Instruction, string(act)) {
			t.Fatalf("act %q is choosable and never described in the instruction", act)
		}
	}
}

func contains(acts []interaction.Act, want interaction.Act) bool {
	for _, act := range acts {
		if act == want {
			return true
		}
	}
	return false
}

// A decision about when to speak often turns on something only the deployment
// knows. Correcting somebody mid-sentence never worked without this, because
// the fact being corrected against was never in front of the decision.
func TestRenderCarriesTheAgentContract(t *testing.T) {
	block := interaction.Situation{
		Contract: "The deadline the client gave is the third of the month.",
		Recent:   []string{"user: so we ship by the thirteenth"},
	}.Render()
	if !strings.Contains(block, "third of the month") {
		t.Fatalf("the contract never reached the decision:\n%s", block)
	}
	contract := strings.Index(block, "third of the month")
	window := strings.Index(block, "thirteenth")
	if contract > window {
		t.Fatalf("the contract was rendered after the window it should outlive:\n%s", block)
	}
	if strings.Contains(interaction.Situation{}.Render(), "What this agent is for") {
		t.Fatal("an empty contract still announced a section")
	}
}
