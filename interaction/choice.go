package interaction

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

// Choice is what the interaction policy decides at one instant.
//
// Two questions, one token. Should the agent's voice be invoked now - and,
// only while the agent is already speaking, should the output it is producing
// stop? Everything else is somebody else's decision: what to say, whether to
// say anything at all, and whether to call a tool belong to the voice model,
// which can read the content. The policy reads the situation.
//
// This replaces a seven-act vocabulary whose members mostly named the same
// outcome. answer, speak-through and interrupt all reached the runtime as
// "invoke the voice model"; the distinctions between them existed only in the
// prompt, and a model shown three names for one thing spent its decision
// choosing between the names. The runtime's own dispatch had four cases. This
// is that dispatch, written down as the contract instead of discovered in it.
type Choice struct {
	// Speaking records whether the agent had voice output active when this
	// was decided. It is what makes the token exact: the same two answers
	// spell "listen" while the floor is free and "keep" while it is in use,
	// and a Choice that did not know which it was could not be read back.
	Speaking bool
	// Speak invokes the voice model now, on any event: a provisional
	// transcript, a settled one, a frame, a quiet clock that a standing
	// policy made into evidence.
	Speak bool
	// Stop yields the floor by cancelling the output the agent is producing.
	// It requires Speaking; there is nothing to stop otherwise.
	Stop bool
}

// The tokens a policy model may return. They are the option strings sent for
// constrained decoding, so a model cannot answer with anything else, and they
// are the text form of a Choice, so the same string reads back exactly.
const (
	ChoiceListen    = "listen"
	ChoiceSpeak     = "speak"
	ChoiceKeep      = "keep"
	ChoiceStop      = "stop"
	ChoiceKeepSpeak = "keep+speak"
	ChoiceStopSpeak = "stop+speak"
)

// ChoiceOptions is the exact option set for one instant. It is derived from
// state and never configured: while the floor is free there are two answers,
// and while the agent is speaking there are four, because the two questions
// are independent - a running count falling due mid-sentence is keep+speak,
// and somebody cutting in with a new request is stop+speak.
func ChoiceOptions(speaking bool) []string {
	if speaking {
		return []string{ChoiceKeep, ChoiceStop, ChoiceKeepSpeak, ChoiceStopSpeak}
	}
	return []string{ChoiceListen, ChoiceSpeak}
}

// ChoiceOptionsFor is the exact option set for one situation: the full set
// for its speaking state, on every event.
//
// A provisional transcript is offered speak as well as listen. Whether a
// half-sentence justifies speaking is the policy's judgement - the rules say
// listen unless a standing instruction has come due - and it is asked on
// every partial rather than short-circuited by the runtime. An earlier version
// withheld speak on partials unless a pin was in force, which saved a model
// call per partial and made the whole of live counting depend on the
// pinboard: measured, one lost pin left an agent unable to count for the rest
// of the story, because nothing was ever asked.
func ChoiceOptionsFor(state Situation) []string {
	return ChoiceOptions(state.AgentSpeaking)
}

// ParseChoice reads a token the policy returned. The token must be one that
// was offered for this instant; a token from the other state is a decision
// about a situation that did not exist.
func ParseChoice(token string, speaking bool) (Choice, error) {
	choice, err := parseChoiceToken(strings.TrimSpace(token))
	if err != nil {
		return Choice{}, err
	}
	if choice.Speaking != speaking {
		return Choice{}, fmt.Errorf(
			"interaction choice %q belongs to the other state: the agent was%s speaking",
			token, map[bool]string{true: "", false: " not"}[speaking],
		)
	}
	return choice, nil
}

func parseChoiceToken(token string) (Choice, error) {
	switch token {
	case ChoiceListen:
		return Choice{}, nil
	case ChoiceSpeak:
		return Choice{Speak: true}, nil
	case ChoiceKeep:
		return Choice{Speaking: true}, nil
	case ChoiceStop:
		return Choice{Speaking: true, Stop: true}, nil
	case ChoiceKeepSpeak:
		return Choice{Speaking: true, Speak: true}, nil
	case ChoiceStopSpeak:
		return Choice{Speaking: true, Speak: true, Stop: true}, nil
	}
	return Choice{}, fmt.Errorf("interaction choice %q is not one of the exact options", token)
}

// Token is the text form of the choice.
func (choice Choice) Token() string {
	switch {
	case !choice.Speaking && !choice.Speak:
		return ChoiceListen
	case !choice.Speaking:
		return ChoiceSpeak
	case choice.Stop && choice.Speak:
		return ChoiceStopSpeak
	case choice.Stop:
		return ChoiceStop
	case choice.Speak:
		return ChoiceKeepSpeak
	}
	return ChoiceKeep
}

// Validate rejects the one combination that names nothing: stopping output
// that was not there.
func (choice Choice) Validate() error {
	if choice.Stop && !choice.Speaking {
		return errors.New("interaction choice stops output while the agent was not speaking")
	}
	return nil
}

// Idle says the choice asks the runtime to do nothing: no invocation and no
// cancellation. It is the answer to "did anything happen", which is not the
// same question as "was the agent speaking".
func (choice Choice) Idle() bool {
	return !choice.Speak && !choice.Stop
}

// MarshalText encodes the choice as its token, so JSON carries "stop+speak"
// rather than three booleans somebody has to combine.
func (choice Choice) MarshalText() ([]byte, error) {
	if err := choice.Validate(); err != nil {
		return nil, err
	}
	return []byte(choice.Token()), nil
}

// UnmarshalText reads a token back. A struct that round-trips through its
// own text is one that traces and tests can write by hand.
func (choice *Choice) UnmarshalText(text []byte) error {
	parsed, err := parseChoiceToken(strings.TrimSpace(string(text)))
	if err != nil {
		return err
	}
	*choice = parsed
	return nil
}

// ChoiceInstruction is the rules a policy model reads with the situation. It
// is written around the two questions and carries the worked examples that
// were measured against the twelve scenarios; a deployment can replace it,
// and the room profile passes it explicitly so the file it ran with is the
// one recorded.
//
//go:embed choice_rules.txt
var ChoiceInstruction string
