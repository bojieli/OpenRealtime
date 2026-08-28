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
		if act == interaction.ActActSilently {
			t.Fatal("call-tool offered with no tool named; the phone-menu case failed exactly this way")
		}
	}
	armed := interaction.Situation{Tools: []string{"press_key(digit)"}}.AvailableActs()
	if !contains(armed, interaction.ActActSilently) {
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

func TestAvailableActsRespectExecutorCapabilities(t *testing.T) {
	state := interaction.Situation{
		Speaker: "user", Speaking: true, Heard: "one more thing",
		AllowedActs: []interaction.Act{interaction.ActStaySilent, interaction.ActInterrupt},
	}
	acts := state.AvailableActs()
	if contains(acts, interaction.ActSpeakThrough) {
		t.Fatal("a model without concurrent I/O must not be offered speak-through")
	}
	if !contains(acts, interaction.ActInterrupt) || !contains(acts, interaction.ActStaySilent) {
		t.Fatalf("supported acts were lost: %v", acts)
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
		interaction.ActInterrupt, interaction.ActActSilently,
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

// Answering means the floor is free. Offered while somebody is audibly using
// it, a model reaches for it whenever something is worth saying, and the
// runtime then refuses it - so the thing worth saying is never said at all.
func TestAvailableActsWithholdAnswerWhileSomebodyIsSpeaking(t *testing.T) {
	live := interaction.Situation{Speaker: "user", Speaking: true, Heard: "and then a heron landed"}
	if contains(live.AvailableActs(), interaction.ActAnswer) {
		t.Fatalf("answer was offered over an active speaker: %v", live.AvailableActs())
	}
	for _, act := range []interaction.Act{interaction.ActSpeakThrough, interaction.ActInterrupt} {
		if !contains(live.AvailableActs(), act) {
			t.Fatalf("%s must stay available while somebody is speaking", act)
		}
	}
	stopped := interaction.Situation{Heard: "and then a heron landed", Silence: "900ms"}
	if !contains(stopped.AvailableActs(), interaction.ActAnswer) {
		t.Fatal("answer was withheld from a floor nobody was using")
	}
}

// TestNothingNewAndEverythingNewDoNotLookTheSame is the regression for a
// scenario that went from counting too much to counting nothing at all. The
// line was omitted when HeardSince was empty and again when it equalled Heard,
// so "the agent has already answered all of this" and "the agent has not
// spoken during any of this" rendered identically, and a rule that told the
// model to judge a standing instruction against what was new could not be
// followed.
func TestNothingNewAndEverythingNewDoNotLookTheSame(t *testing.T) {
	nothingNew := interaction.Situation{
		Heard: "a capybara wandered over and sat down next to me", HeardSince: "",
	}.Render()
	allNew := interaction.Situation{
		Heard:      "a capybara wandered over and sat down next to me",
		HeardSince: "a capybara wandered over and sat down next to me",
	}.Render()
	if nothingNew == allNew {
		t.Fatalf("two opposite situations render the same:\n%s", nothingNew)
	}
	if !strings.Contains(nothingNew, "nothing has been said since the agent last spoke") {
		t.Fatalf("a situation with nothing new does not say so:\n%s", nothingNew)
	}
	if !strings.Contains(allNew, "all of this is new since the agent last spoke") {
		t.Fatalf("a situation where everything is new does not say so:\n%s", allNew)
	}
}

// Told to say nothing but the thing they asked for, an ordinary reply is not
// an act the person has left available. Measured, an agent under "count the
// animals and say nothing else" answered the pause at the end of a sentence
// with no animal in it - twice - because somebody stopping mid-story is an
// overwhelming case for a reply, and the standing instruction was one line
// against it. What they did ask for is still sayable: speak-through stays.
func TestAnswerIsWithdrawnByAPolicyThatForbidsEverythingElse(t *testing.T) {
	state := interaction.Situation{
		Pins:       []string{"count the animals out loud as they mention them, and say nothing else"},
		Restricted: true,
	}
	var sawAnswer, sawSpeakThrough bool
	for _, act := range state.AvailableActs() {
		switch act {
		case interaction.ActAnswer:
			sawAnswer = true
		case interaction.ActSpeakThrough:
			sawSpeakThrough = true
		}
	}
	if sawAnswer {
		t.Fatal("an ordinary reply was offered under a policy that forbids everything else")
	}
	if !sawSpeakThrough {
		t.Fatal("the thing they did ask for has to stay sayable")
	}
}

// A final event can recover a required simultaneous act that missed its live
// window. The event policy still decides whether the current words contain the
// standing condition; filtering answer here made its explicit catch-up rule
// impossible to carry out under "say nothing else".
func TestFinalTranscriptCanCatchUpARestrictedStandingAct(t *testing.T) {
	state := interaction.Situation{
		TranscriptEvent: interaction.TranscriptFinal,
		Pins:            []string{"count the animals out loud as they mention them, and say nothing else"},
		Restricted:      true,
		AllowedActs:     []interaction.Act{interaction.ActStaySilent, interaction.ActAnswer},
	}
	for _, act := range state.AvailableActs() {
		if act == interaction.ActAnswer {
			return
		}
	}
	t.Fatal("a final transcript cannot recover the required restricted act")
}

// And without such a policy it is offered as before, since the floor is free.
func TestAnswerSurvivesAPolicyThatOnlyNamesSomethingToDo(t *testing.T) {
	state := interaction.Situation{Pins: []string{"count the animals out loud as they mention them"}}
	for _, act := range state.AvailableActs() {
		if act == interaction.ActAnswer {
			return
		}
	}
	t.Fatal("answer should be available when nothing forbids it")
}
