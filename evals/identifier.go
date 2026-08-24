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

// IdentifierCases freeze the reasoner at the moment it has to act on something
// a person spelled out loud.
//
// A recogniser writes an identifier the way it was said, so "A-B-C-one-two-
// three" reaches the trajectory as "AB, C,1,2,3" or "a b c one two three". The
// separators are the recogniser's, the characters are the caller's, and the
// difference between removing the first and inventing the second is somebody
// else's order.
//
// These are graded strictly on purpose. There is one right value here, unlike
// the hand-off boundary where several actions are reasonable: a lookup against
// the wrong record is not a partial success.
func IdentifierCases() []Case {
	spoken := func(name, heard, want string) Case {
		return Case{
			Name: name, Decision: DecisionIdentifier,
			Context: heard + "\x1fwant=" + want,
			Accept:  []Action{ActionDraft},
			Forbid:  []Action{ActionWrongID},
			Note:    "the separators are the recogniser's; the characters are the caller's",
		}
	}
	return []Case{
		spoken("comma-grouped", "Could you track my order? The order I D is AB, C,1,2,3.", "ABC123"),
		spoken("spaced-letters", "Where is my package, order number X, Y, Z 88?", "XYZ88"),
		spoken("words-for-digits", "Track order B B one nine for me please.", "BB19"),
		spoken("dashed", "My order is Q-Q-4-1, can you check it?", "QQ41"),
		spoken("run-together", "Can you look up order kk zero two?", "KK02"),
		spoken("mixed-case", "The order id is m four four seven seven.", "M4477"),
		spoken("digits-only", "Order seven seven eight eight, has it shipped?", "7788"),
		spoken("with-filler", "It is, um, order A-B-1-2, I think. Could you check?", "AB12"),
	}
}

// IdentifierRunner asks one reasoner to act on a spoken identifier.
type IdentifierRunner struct {
	Provider continuation.Provider
	Label    string
}

func (runner IdentifierRunner) Name() string       { return runner.Label }
func (runner IdentifierRunner) Decision() Decision { return DecisionIdentifier }

func (runner IdentifierRunner) Observe(ctx context.Context, item Case) Observation {
	heard, want, _ := strings.Cut(item.Context, "\x1fwant=")
	started := time.Now()
	request := continuation.Request{
		Descriptor: runner.Provider.Descriptor(),
		Invocation: continuation.Invocation{
			Instruction:     cognition.Compose("", cognition.SlowInstruction),
			Tools:           []continuation.ToolDefinition{trackOrderTool()},
			MaxOutputTokens: 512,
		},
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{{
			ID: "observation-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
			Content: heard, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		}}},
	}

	var got string
	var called bool
	_, err := runner.Provider.Continue(ctx, request, func(event continuation.Event) error {
		if event.Kind == continuation.EventToolCall && event.ToolCall != nil {
			called = true
			var arguments struct {
				OrderID string `json:"order_id"`
			}
			_ = json.Unmarshal(event.ToolCall.Arguments, &arguments)
			got = arguments.OrderID
		}
		return nil
	})

	var actions []Action
	switch {
	case !called:
		actions = append(actions, ActionSilent)
	case strings.EqualFold(strings.TrimSpace(got), want):
		actions = append(actions, ActionDraft)
	default:
		actions = append(actions, ActionWrongID)
	}
	return Observation{
		Actions: actions, Text: "order_id=" + got + " want=" + want,
		Elapsed: time.Since(started), Err: err,
	}
}

func trackOrderTool() continuation.ToolDefinition {
	schema := `{"type":"object","properties":{"order_id":{"type":"string",` +
		`"description":"The order identifier exactly as the caller gave it."}},` +
		`"required":["order_id"]}`
	return continuation.ToolDefinition{
		Name:        "track_order",
		Description: "Look up the delivery status of an order by its identifier.",
		Parameters:  json.RawMessage(schema),
	}
}
