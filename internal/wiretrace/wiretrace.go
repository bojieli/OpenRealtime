// Package wiretrace materializes validated OpenAI wire events into a causal trace.
package wiretrace

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/trace"
)

type Event struct {
	Key       string
	Parents   []string
	AtNS      uint64
	Order     uint64
	Direction openaiwire.Direction
	Profile   openaiwire.Profile
	Message   json.RawMessage
}

func Marshal(value map[string]any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func Materialize(events []Event, sessionID string, validator *openaiwire.Validator) ([]trace.Record, error) {
	if sessionID == "" || validator == nil {
		return nil, fmt.Errorf("wire trace requires a session ID and validator")
	}
	events = slices.Clone(events)
	sort.SliceStable(events, func(left, right int) bool {
		if events[left].AtNS != events[right].AtNS {
			return events[left].AtNS < events[right].AtNS
		}
		return events[left].Order < events[right].Order
	})
	state := trace.NewState()
	records := make([]trace.Record, 0, len(events))
	for sequence, event := range events {
		message, err := openaiwire.Decode(event.Message)
		if err != nil {
			return nil, fmt.Errorf("event %s: %w", event.Key, err)
		}
		if err := validator.Validate(event.Profile, event.Direction, message); err != nil {
			return nil, fmt.Errorf("event %s: %w", event.Key, err)
		}
		record := trace.Record{
			SchemaVersion: trace.SchemaVersion, TraceID: event.Key, SessionID: sessionID,
			Sequence: uint64(sequence), MonotonicNS: event.AtNS, Direction: event.Direction,
			Profile: event.Profile, CausalParentIDs: slices.Clone(event.Parents), Message: slices.Clone(event.Message),
		}
		if err := state.Accept(record); err != nil {
			return nil, fmt.Errorf("event %s: %w", event.Key, err)
		}
		records = append(records, record)
	}
	return records, nil
}
