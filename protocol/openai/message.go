// Package openai implements the OpenAI Realtime wire protocol boundary.
//
// It preserves complete JSON messages, validates them against a pinned subset
// of OpenAI's official OpenAPI 3.1 specification, and classifies every event by
// protocol profile and direction. Unknown events can be retained for forward
// compatible proxying without being mistaken for conformant known events.
package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

type EventType string

type Direction string

const (
	DirectionClient Direction = "client"
	DirectionServer Direction = "server"
)

type Profile string

const (
	ProfileRealtime      Profile = "realtime"
	ProfileTranscription Profile = "transcription"
	ProfileTranslation   Profile = "translation"
	ProfileBeta          Profile = "beta"
)

type Definition struct {
	Profile   Profile   `json:"profile"`
	Direction Direction `json:"direction"`
	Type      EventType `json:"type"`
	SchemaRef string    `json:"schema_ref"`
}

type Message struct {
	eventID *string
	typeID  EventType
	raw     json.RawMessage
}

type header struct {
	EventID *string   `json:"event_id"`
	Type    EventType `json:"type"`
}

func Decode(data []byte) (Message, error) {
	var envelope header
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Message{}, fmt.Errorf("decode event envelope: %w", err)
	}
	if envelope.Type == "" {
		return Message{}, errors.New("event type must not be empty")
	}
	raw := bytes.Clone(data)
	return Message{eventID: envelope.EventID, typeID: envelope.Type, raw: raw}, nil
}

func (message Message) Type() EventType {
	return message.typeID
}

func (message Message) EventID() (string, bool) {
	if message.eventID == nil {
		return "", false
	}
	return *message.eventID, true
}

func (message Message) Raw() []byte {
	return bytes.Clone(message.raw)
}

func (message Message) Unmarshal(target any) error {
	if target == nil {
		return errors.New("target must not be nil")
	}
	return json.Unmarshal(message.raw, target)
}

func Lookup(profile Profile, direction Direction, eventType EventType) (Definition, bool) {
	definition, ok := definitionIndex[registryKey{profile, direction, eventType}]
	return definition, ok
}

func Definitions() []Definition {
	result := make([]Definition, len(generatedDefinitions))
	copy(result, generatedDefinitions)
	return result
}

type registryKey struct {
	profile   Profile
	direction Direction
	eventType EventType
}

var definitionIndex = func() map[registryKey]Definition {
	result := make(map[registryKey]Definition, len(generatedDefinitions))
	for _, definition := range generatedDefinitions {
		key := registryKey{definition.Profile, definition.Direction, definition.Type}
		if _, exists := result[key]; exists {
			panic("duplicate generated OpenAI Realtime event definition")
		}
		result[key] = definition
	}
	return result
}()
