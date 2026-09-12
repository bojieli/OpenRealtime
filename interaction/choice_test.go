package interaction

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every token offered for a state parses back to a choice whose token is the
// same string, and the two states never offer the same token.
func TestChoiceTokensRoundTrip(t *testing.T) {
	seen := map[string]bool{}
	for _, speaking := range []bool{false, true} {
		options := ChoiceOptions(speaking)
		if len(options) != map[bool]int{false: 2, true: 4}[speaking] {
			t.Fatalf("speaking=%v offers %v", speaking, options)
		}
		for _, token := range options {
			if seen[token] {
				t.Fatalf("token %q is offered in both states", token)
			}
			seen[token] = true
			choice, err := ParseChoice(token, speaking)
			if err != nil {
				t.Fatal(err)
			}
			if choice.Speaking != speaking || choice.Token() != token {
				t.Fatalf("%q parsed to %+v, token %q", token, choice, choice.Token())
			}
			if err := choice.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A token from the other state is refused: it decides a situation that did
// not exist, and accepting it would let "stop" through with nothing to stop.
func TestChoiceRefusesTokenFromTheOtherState(t *testing.T) {
	if _, err := ParseChoice(ChoiceStop, false); err == nil ||
		!strings.Contains(err.Error(), "not speaking") {
		t.Fatalf("stop while idle was accepted: %v", err)
	}
	if _, err := ParseChoice(ChoiceListen, true); err == nil {
		t.Fatal("listen while speaking was accepted")
	}
	if _, err := ParseChoice("answer", false); err == nil {
		t.Fatal("a legacy act name was accepted as a choice")
	}
}

func TestChoiceMeaning(t *testing.T) {
	stopSpeak, _ := ParseChoice(ChoiceStopSpeak, true)
	if !stopSpeak.Stop || !stopSpeak.Speak || stopSpeak.Idle() {
		t.Fatalf("stop+speak = %+v", stopSpeak)
	}
	keep, _ := ParseChoice(ChoiceKeep, true)
	if !keep.Idle() || keep.Stop || keep.Speak {
		t.Fatalf("keep = %+v", keep)
	}
	if (Choice{Stop: true}).Validate() == nil {
		t.Fatal("a stop with the floor free validated")
	}
}

// JSON carries the token, not three booleans somebody has to combine, and
// reads it back to the same value.
func TestChoiceJSONIsTheToken(t *testing.T) {
	type payload struct {
		Choice Choice `json:"choice"`
	}
	encoded, err := json.Marshal(payload{Choice: Choice{Speaking: true, Speak: true}})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"choice":"keep+speak"}` {
		t.Fatalf("encoded %s", encoded)
	}
	var decoded payload
	if err := json.Unmarshal([]byte(`{"choice":"stop"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Choice != (Choice{Speaking: true, Stop: true}) {
		t.Fatalf("decoded %+v", decoded.Choice)
	}
	if _, err := json.Marshal(payload{Choice: Choice{Stop: true}}); err == nil {
		t.Fatal("an invalid choice encoded")
	}
}

// The instruction is the preamble every step question shares. It explains
// the evidence in words a small model can use and names no act, because the
// model returns yes or no and the runtime composes the choice.
func TestChoiceInstructionExplainsTheEvidenceAndNamesNoAct(t *testing.T) {
	for _, legacy := range []string{"speak-through", "act-silently", "keep-speaking", "stop-speaking", "-> answer", "-> interrupt", "Choose exactly one"} {
		if strings.Contains(ChoiceInstruction, legacy) {
			t.Fatalf("instruction still names %q", legacy)
		}
	}
	for _, term := range []string{"partial", "final", "new words since the last step", "standing instruction", "yes or no"} {
		if !strings.Contains(ChoiceInstruction, term) {
			t.Fatalf("instruction never explains %q", term)
		}
	}
	if strings.Count(ChoiceInstruction, "\n") > 16 {
		t.Fatalf("instruction has grown past a dozen lines:\n%s", ChoiceInstruction)
	}
}

// Every choice round-trips through the questions: answering each question
// the way a choice would composes that same choice.
func TestQuestionsComposeEveryChoice(t *testing.T) {
	for _, speaking := range []bool{false, true} {
		for _, token := range ChoiceOptions(speaking) {
			choice, err := ParseChoice(token, speaking)
			if err != nil {
				t.Fatal(err)
			}
			answers := map[string]bool{}
			for _, question := range []string{QuestionStop, QuestionOccurrence, QuestionRequest} {
				answers[question] = AnswerFor(question, choice) == AnswerYes
			}
			if composed := ComposeChoice(speaking, answers); composed != choice {
				t.Fatalf("%s composed as %+v, want %+v", token, composed, choice)
			}
		}
	}
	if !ComposeChoice(false, map[string]bool{QuestionStop: true}).Idle() {
		t.Fatal("a stop answer while nobody is speaking must not stop anything")
	}
	if DescribeAnswers([]AskedQuestion{{Question: QuestionStop, Answer: AnswerNo}, {Question: QuestionOccurrence, Answer: AnswerYes}}) != "stop=no occurrence=yes" {
		t.Fatal("answers do not describe themselves")
	}
}

func TestRenderForChoiceEndsWithTheExactOptions(t *testing.T) {
	idle := Situation{Heard: "what time is it", Speaker: "user"}
	if rendered := idle.RenderForChoice(); !strings.HasSuffix(rendered, "Choose exactly one: listen, speak") ||
		strings.Contains(rendered, "Available acts") {
		t.Fatalf("idle render:\n%s", rendered)
	}
	speaking := Situation{AgentSpeaking: true, AgentSaying: "One.", Heard: "then a heron"}
	if rendered := speaking.RenderForChoice(); !strings.HasSuffix(rendered, "Choose exactly one: keep, stop, keep+speak, stop+speak") {
		t.Fatalf("speaking render:\n%s", rendered)
	}
}
