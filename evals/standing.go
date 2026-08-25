package evals

import (
	"context"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Actions at the extraction boundary.
const (
	ActNoPolicy        Action = "none"
	ActPinTurn         Action = "pin-turn"
	ActPinConversation Action = "pin-conversation"
	ActRevoke          Action = "revoke"
	ActUnrecognisedPin Action = "unrecognised"
)

// StandingInstructionCases freeze the pass that decides what gets pinned.
//
// Two distinctions carry the suite, and both are ones a careless pass gets
// wrong in a way nobody notices for weeks.
//
// A task is not a policy. "Finish the report by Friday" and "stop me if I get
// a date wrong" have the same grammatical shape and nothing else in common: a
// pass that pins the first fills the policy list with entries that never apply
// and never expire, and every one of them is prompt the deciding model has to
// read on every tick forever.
//
// Scope is not decoration. "Don't cut me off" governs a conversation and "wait,
// I have more to say" governs a few seconds. Give the second the lifetime of
// the first and the agent is permanently mute, months later, for reasons
// nobody will connect to the sentence that caused it.
func StandingInstructionCases() []Case {
	said := func(name, utterance, note string, accept []Action, forbid ...Action) Case {
		return inForce(nil, name, utterance, note, accept, forbid...)
	}
	standing := func(text string, scope interaction.Scope) interaction.StandingInstruction {
		return interaction.StandingInstruction{Text: text, Scope: scope}
	}
	policy := []Action{ActPinConversation}
	forThisTurn := []Action{ActPinTurn}
	nothing := []Action{ActNoPolicy}
	return []Case{
		// --- ordinary speech sets no policy at all ---
		said("plain-request", "Can you look up my last order?", "",
			nothing, ActPinConversation, ActPinTurn),
		said("plain-question", "What's the weather going to do this afternoon?", "",
			nothing, ActPinConversation, ActPinTurn),
		said("plain-answer", "It's the blue one, second from the left.", "",
			nothing, ActPinConversation, ActPinTurn),

		// --- a task instruction has the shape of a policy and is not one ---
		said("task-deadline", "Finish the report by Friday and send it to Dana.",
			"work to be done, not a rule about when to speak",
			nothing, ActPinConversation, ActPinTurn),
		said("task-style", "From now on give me shorter answers.",
			"'from now on' makes it sound standing; it governs style, not timing",
			nothing, ActPinConversation, ActPinTurn),
		said("task-detail", "Always include the order number when you tell me about a delivery.",
			"about content, not about when to speak",
			nothing, ActPinConversation, ActPinTurn),

		// --- policies that govern the whole conversation ---
		said("count-as-i-go", "Count the animals out loud as I mention them.",
			"the demo case: a policy that only a model reading the dialogue can honour",
			policy, ActNoPolicy),
		said("dont-cut-me-off", "Don't cut me off while I'm talking.", "",
			policy, ActNoPolicy),
		said("tell-me-immediately", "Tell me the moment the build finishes, even if I'm mid-sentence.", "",
			policy, ActNoPolicy),
		said("watch-posture", "Let me know if I start slouching.",
			"a visual condition, announced whenever it happens",
			policy, ActNoPolicy),
		said("translate-continuously", "Translate everything he says into English as he goes.",
			"the simultaneous-speech case: a policy never to yield the floor",
			policy, ActNoPolicy),
		said("stop-me-on-price", "Stop me if I quote a price under fifty.", "",
			policy, ActNoPolicy),
		said("stay-quiet-until-asked", "Just listen for now, don't say anything until I ask.", "",
			policy, ActNoPolicy),

		// --- policies that govern only the current turn ---
		said("let-me-finish", "Let me finish.",
			"about this sentence, not about every future one",
			forThisTurn, ActPinConversation, ActNoPolicy),
		said("more-to-say", "Wait, I have more to say.", "",
			forThisTurn, ActPinConversation, ActNoPolicy),
		said("thinking-out-loud", "I'm going to think out loud for a minute, don't answer yet.",
			"explicitly bounded: a minute, not forever",
			forThisTurn, ActPinConversation, ActNoPolicy),

		// --- lifting a policy that was set earlier ---
		inForce([]interaction.StandingInstruction{
			standing("do not interrupt them while they are talking", interaction.ScopeConversation),
		}, "permission-to-interrupt", "Okay, you can interrupt me now.",
			"a revocation is only visible against the thing it lifts",
			[]Action{ActRevoke}, ActNoPolicy, ActPinConversation),
		inForce([]interaction.StandingInstruction{
			standing("tell them when the kettle has boiled", interaction.ScopeConversation),
		}, "never-mind-kettle", "Actually never mind about the kettle, you don't need to tell me.", "",
			[]Action{ActRevoke}, ActPinConversation),

		inForce([]interaction.StandingInstruction{
			standing("do not interrupt them while they are talking", interaction.ScopeConversation),
		}, "adds-a-second-policy", "Also, tell me if the courier turns up.",
			"a new policy alongside an existing one is not a revocation of it",
			policy, ActRevoke, ActNoPolicy),

		// --- an immediate command is obeyed, not pinned ---
		said("stop-now", "Stop.",
			"over the moment it is obeyed; pinning it would mute the agent for good",
			nothing, ActPinConversation),
		said("hold-on", "Hold on.",
			"defensible either way - a request for a beat, or a rule for this turn",
			[]Action{ActNoPolicy, ActPinTurn}, ActPinConversation),
	}
}

// inForce builds a case where policies are already standing. Case carries the
// rendered block rather than the parts, so what a case asserts is exactly what
// a model was shown.
func inForce(
	existing []interaction.StandingInstruction, name, utterance, note string,
	accept []Action, forbid ...Action,
) Case {
	return Case{
		Name: name, Decision: DecisionStandingInstruction,
		Context: interaction.RenderForExtraction(existing, utterance),
		Accept:  accept, Forbid: forbid, Note: note,
	}
}

// StandingInstructionRunner asks one model what should be pinned.
type StandingInstructionRunner struct {
	Provider continuation.Provider
	Label    string
}

func (runner StandingInstructionRunner) Name() string       { return runner.Label }
func (runner StandingInstructionRunner) Decision() Decision { return DecisionStandingInstruction }

func (runner StandingInstructionRunner) Observe(ctx context.Context, item Case) Observation {
	started := time.Now()
	request := continuation.Request{
		Descriptor: runner.Provider.Descriptor(),
		Invocation: continuation.Invocation{
			Instruction: interaction.ExtractionInstruction, MaxOutputTokens: 128,
		},
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{{
			ID: "utterance", Kind: trajectory.KindObservation, MonotonicNS: 1,
			Content:  "They said: \"" + item.Context + "\"",
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		}}},
	}
	var said strings.Builder
	_, err := runner.Provider.Continue(ctx, request, func(event continuation.Event) error {
		if event.Kind == continuation.EventAssistantDelta {
			said.WriteString(event.Text)
		}
		return nil
	})
	text, _ := continuation.StripMarkers(said.String())
	return Observation{
		Actions: []Action{pinAction(text)},
		Text:    strings.TrimSpace(text), Elapsed: time.Since(started), Err: err,
	}
}

func pinAction(text string) Action {
	kind, pinned, ok := interaction.ParsePin(text)
	if !ok {
		return ActUnrecognisedPin
	}
	switch kind {
	case "none":
		return ActNoPolicy
	case "revoke":
		return ActRevoke
	case "pin":
		if pinned.Scope == interaction.ScopeTurn {
			return ActPinTurn
		}
		return ActPinConversation
	}
	return ActUnrecognisedPin
}
