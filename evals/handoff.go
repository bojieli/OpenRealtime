package evals

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// HandOffCases freeze the boundary every tool call in the cascade depends on.
//
// The voice is the only phase the user hears and the only one that cannot act,
// so a turn it declares finished is a turn where nothing happens. These cases
// ask what it does with one user utterance and nothing else.
//
// The acceptable sets are wider than they look. Asking for a detail that is
// genuinely absent is a real answer, and so is handing on without asking; what
// is never right is finishing a turn that promised work, or stating a delivery
// status nobody supplied.
func HandOffCases() []Case {
	needsWork := func(name, utterance, note string) Case {
		return Case{
			Name: name, Decision: DecisionHandOff, Context: utterance,
			Accept: []Action{ActionHandOn, ActionDraft},
			Forbid: []Action{ActionFabricate, ActionSilent},
			Note:   note,
		}
	}
	selfContained := func(name, utterance, note string) Case {
		return Case{
			Name: name, Decision: DecisionHandOff, Context: utterance,
			Accept: []Action{ActionFinish},
			Forbid: []Action{ActionFabricate, ActionSilent},
			Note:   note,
		}
	}
	return []Case{
		needsWork("track-with-id", "Could you track my order for me? The order ID is ABC123.",
			"everything needed is present, so asking is a wasted turn"),
		needsWork("track-spoken-id", "Where's my package? The order number is XYZ88.", ""),
		needsWork("cancel-subscription", "I'd like to cancel my subscription please.",
			"confirmation may be required, but the work still has to start"),
		needsWork("refund-status", "Has my refund come through yet? It was for order 7788.", ""),
		needsWork("change-address", "Can you change the delivery address on order QQ41 to my work address?",
			"the new address is missing, so asking is acceptable here"),
		needsWork("late-delivery", "I ordered something last week and it hasn't arrived. Order BB19.", ""),
		needsWork("chase-order", "My package is late. Can you look into it? It's order number M4477.", ""),
		needsWork("did-it-ship", "I want to know if order KK02 shipped.", ""),
		needsWork("vague-problem", "I have a problem with my order and I'm not sure what to do.",
			"no identifier, so asking is the good answer - finishing is not"),
		needsWork("vague-account", "Something's wrong with my account, can you help?", ""),

		selfContained("greeting", "Hello?", ""),
		selfContained("thanks", "Thanks, that's all I needed.", ""),
		selfContained("general-knowledge", "What's the capital of France?",
			"no lookup of this user's data, so the voice can finish it"),
		selfContained("repeat", "Sorry, could you say that again?", ""),
		selfContained("gratitude", "Great, thank you very much.", ""),
	}
}

// fabricationHints are claims about the user's own data that the voice cannot
// have. They are matched case-insensitively against what it said.
//
// A substring test is crude and deliberately so: it is a veto, and a veto that
// needs a model to adjudicate it is a veto that fails open on the day the
// judge is unavailable. Anything it misses is caught by the end-to-end layer.
var fabricationHints = []string{
	"is on its way", "has shipped", "was delivered", "is out for delivery",
	"has been refunded", "was refunded", "is being delivered", "arrived on",
	"is scheduled to arrive", "has been cancelled", "was cancelled successfully",
}

// HandOffRunner asks one fast provider what it does with a turn.
type HandOffRunner struct {
	Provider continuation.Provider
	Store    *trajectory.Store
	Label    string
}

func (runner HandOffRunner) Name() string       { return runner.Label }
func (runner HandOffRunner) Decision() Decision { return DecisionHandOff }

// Observe runs the real fast instruction through the real continuation runner,
// so what is measured is what ships rather than a reconstruction of it.
func (runner HandOffRunner) Observe(ctx context.Context, item Case) Observation {
	started := time.Now()
	request := continuation.Request{
		Descriptor: runner.Provider.Descriptor(),
		Invocation: continuation.Invocation{
			Instruction:     cognition.Compose("", cognition.FastInstruction),
			Capabilities:    handOffCapabilities(),
			MaxOutputTokens: 96,
		},
	}
	request.Trajectory = trajectory.Snapshot{Items: []trajectory.Item{{
		ID: "observation-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
		Content: item.Context, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
	}}}

	var spoken strings.Builder
	var drafted bool
	_, err := runner.Provider.Continue(ctx, request, func(event continuation.Event) error {
		switch event.Kind {
		case continuation.EventAssistantDelta:
			spoken.WriteString(event.Text)
		case continuation.EventToolCall:
			drafted = true
		}
		return nil
	})
	text, finished := continuation.StripMarkers(spoken.String())
	return Observation{
		Actions: handOffActions(text, finished, drafted),
		Text:    text, Elapsed: time.Since(started), Err: err,
	}
}

func handOffActions(text string, finished, drafted bool) []Action {
	var actions []Action
	if drafted {
		actions = append(actions, ActionDraft)
	}
	if finished {
		actions = append(actions, ActionFinish)
	} else {
		actions = append(actions, ActionHandOn)
	}
	if strings.Contains(text, "?") {
		actions = append(actions, ActionAsk)
	}
	if strings.TrimSpace(text) == "" && !drafted {
		actions = append(actions, ActionSilent)
	}
	lowered := strings.ToLower(text)
	for _, hint := range fabricationHints {
		if strings.Contains(lowered, hint) {
			actions = append(actions, ActionFabricate)
			break
		}
	}
	return actions
}

func handOffCapabilities() []continuation.Capability {
	return []continuation.Capability{
		{Name: "track_order", Description: "Look up the delivery status of an order by its identifier.", Available: true, ExecutionPhase: "slow"},
		{Name: "check_refund_status", Description: "Look up whether a refund has been issued for an order.", Available: true, ExecutionPhase: "slow"},
		{Name: "cancel_subscription", Description: "Cancel the caller's active subscription.", Available: true, ExecutionPhase: "slow", ConfirmationRequired: true},
		{Name: "update_address", Description: "Change the delivery address on an order.", Available: true, ExecutionPhase: "slow", ConfirmationRequired: true},
	}
}

var _ = json.Marshal
