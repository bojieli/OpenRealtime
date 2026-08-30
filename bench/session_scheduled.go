package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maximumScheduledEventBytes = 64 << 20
	maximumScheduledIdentity   = 256
)

// SessionScheduledCapture is one authored event after it was successfully
// submitted to the session transport. EventJSON is the immutable canonical
// JSON value handed to that same Send call, not a later reconstruction of the
// source map. The callback owns EventJSON and may retain or mutate it without
// changing the already-sent event.
//
// AtMS and Name are the closed harness cue identity copied from ScheduledEvent.
// EventType is the exact top-level protocol event type decoded during
// preflight. EventJSON is bounded to maximumScheduledEventBytes before a
// connection or session is opened.
type SessionScheduledCapture struct {
	AtMS      int
	Name      string
	EventType string
	EventJSON []byte
}

type preparedScheduledEvent struct {
	AtMS      int
	Name      string
	EventType string
	EventJSON json.RawMessage
}

func prepareScheduledEvents(source []ScheduledEvent) ([]preparedScheduledEvent, error) {
	if len(source) == 0 {
		return nil, nil
	}
	prepared := make([]preparedScheduledEvent, 0, len(source))
	seen := make(map[string]struct{}, len(source))
	for index, event := range source {
		if event.AtMS < 0 {
			return nil, fmt.Errorf("scheduled event %d has a negative cue", index)
		}
		if !canonicalScheduledIdentity(event.Name) {
			return nil, fmt.Errorf("scheduled event %d has a non-canonical identity", index)
		}
		if _, duplicate := seen[event.Name]; duplicate {
			return nil, fmt.Errorf("scheduled event identity %q is repeated", event.Name)
		}
		seen[event.Name] = struct{}{}
		encoded, err := json.Marshal(event.Event)
		if err != nil {
			return nil, fmt.Errorf("encode scheduled event %q: %w", event.Name, err)
		}
		if err := validateScheduledEventBytes(event.Name, len(encoded)); err != nil {
			return nil, err
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(encoded, &envelope); err != nil ||
			!canonicalScheduledIdentity(envelope.Type) {
			return nil, fmt.Errorf("scheduled event %q has no canonical protocol type", event.Name)
		}
		prepared = append(prepared, preparedScheduledEvent{
			AtMS: event.AtMS, Name: event.Name, EventType: envelope.Type,
			EventJSON: slices.Clone(encoded),
		})
	}
	sort.SliceStable(prepared, func(left, right int) bool {
		return prepared[left].AtMS < prepared[right].AtMS
	})
	return prepared, nil
}

func validateScheduledEventBytes(name string, size int) error {
	if size < 1 || size > maximumScheduledEventBytes {
		return fmt.Errorf(
			"scheduled event %q has %d encoded bytes; expected 1..%d",
			name, size, maximumScheduledEventBytes,
		)
	}
	return nil
}

func emitScheduledCapture(
	ctx context.Context,
	capture func(SessionScheduledCapture) error,
	event preparedScheduledEvent,
) error {
	if capture == nil {
		return context.Cause(ctx)
	}
	copy := SessionScheduledCapture{
		AtMS: event.AtMS, Name: event.Name, EventType: event.EventType,
		EventJSON: slices.Clone(event.EventJSON),
	}
	captureErr := capture(copy)
	if cause := context.Cause(ctx); cause != nil {
		return errors.Join(cause, captureErr)
	}
	return captureErr
}

func canonicalScheduledIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) ||
		len(value) > maximumScheduledIdentity || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}
