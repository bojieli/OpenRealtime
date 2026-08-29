package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
)

// PayloadCodec separates language-neutral wire representation from Go-native
// payload values. A remote-to-remote path may retain OpaquePayload, while an
// explicit native adapter registers a constructor for the exact protocol type.
type PayloadCodec interface {
	Encode(element.Type, any) (json.RawMessage, []byte, error)
	Decode(element.Type, json.RawMessage, []byte) (any, error)
}

// OpaquePayload preserves a payload whose Go representation is intentionally
// unresolved. It remains type-safe at the graph envelope level and can cross
// another language-neutral boundary without an accidental map[string]any
// reinterpretation.
type OpaquePayload struct {
	JSON   json.RawMessage
	Binary []byte
}

type PayloadFactory func() any

// JSONCodec provides deterministic default JSON transport plus optional exact
// Go reconstruction. It owns returned buffers and is safe for concurrent use.
type JSONCodec struct {
	mu        sync.RWMutex
	factories map[string]PayloadFactory
}

func NewJSONCodec() *JSONCodec {
	return &JSONCodec{factories: make(map[string]PayloadFactory)}
}

// Register associates one complete temporal protocol type with a fresh
// pointer/value constructor used by Decode.
func (codec *JSONCodec) Register(valueType element.Type, factory PayloadFactory) error {
	if codec == nil {
		return errors.New("register external model payload codec: nil codec")
	}
	if err := valueType.ValidateConcretePort(); err != nil {
		return fmt.Errorf("register external model payload codec: %w", err)
	}
	if factory == nil {
		return errors.New("register external model payload codec: nil factory")
	}
	probe := factory()
	if probe == nil {
		return errors.New("external model payload factory returned nil")
	}
	reflected := reflect.ValueOf(probe)
	if reflected.Kind() != reflect.Pointer || reflected.IsNil() {
		return fmt.Errorf("external model payload factory must return a non-nil pointer, got %T", probe)
	}
	key := valueType.String()
	codec.mu.Lock()
	defer codec.mu.Unlock()
	if codec.factories == nil {
		codec.factories = make(map[string]PayloadFactory)
	}
	if _, duplicate := codec.factories[key]; duplicate {
		return fmt.Errorf("external model payload codec for %s is already registered", key)
	}
	codec.factories[key] = factory
	return nil
}

func (codec *JSONCodec) Encode(
	valueType element.Type, payload any,
) (json.RawMessage, []byte, error) {
	if codec == nil {
		return nil, nil, errors.New("external model payload codec is nil")
	}
	if err := valueType.ValidateConcretePort(); err != nil {
		return nil, nil, err
	}
	if opaque, ok := opaquePayload(payload); ok {
		if len(opaque.JSON) != 0 && !json.Valid(opaque.JSON) {
			return nil, nil, errors.New("opaque external model payload contains invalid JSON")
		}
		return slices.Clone(opaque.JSON), slices.Clone(opaque.Binary), nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("encode %s payload: %w", valueType.String(), err)
	}
	return encoded, nil, nil
}

func (codec *JSONCodec) Decode(
	valueType element.Type, data json.RawMessage, binary []byte,
) (any, error) {
	if codec == nil {
		return nil, errors.New("external model payload codec is nil")
	}
	if err := valueType.ValidateConcretePort(); err != nil {
		return nil, err
	}
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	if !json.Valid(data) {
		return nil, errors.New("external model frame contains invalid JSON")
	}
	codec.mu.RLock()
	factory := codec.factories[valueType.String()]
	codec.mu.RUnlock()
	if factory == nil {
		return OpaquePayload{JSON: slices.Clone(data), Binary: slices.Clone(binary)}, nil
	}
	value := factory()
	if value == nil {
		return nil, fmt.Errorf("external model payload factory for %s returned nil", valueType.String())
	}
	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Pointer || reflected.IsNil() {
		return nil, fmt.Errorf("external model payload factory for %s returned non-pointer %T",
			valueType.String(), value)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return nil, fmt.Errorf("decode %s payload: %w", valueType.String(), err)
	}
	if len(binary) != 0 {
		if receiver, ok := value.(interface{ SetBinaryPayload([]byte) error }); ok {
			if err := receiver.SetBinaryPayload(slices.Clone(binary)); err != nil {
				return nil, fmt.Errorf("decode %s binary payload: %w", valueType.String(), err)
			}
		} else {
			return nil, fmt.Errorf("decoded %s payload type %T cannot accept binary data", valueType.String(), value)
		}
	}
	return value, nil
}

func opaquePayload(payload any) (OpaquePayload, bool) {
	switch typed := payload.(type) {
	case OpaquePayload:
		return typed, true
	case *OpaquePayload:
		if typed != nil {
			return *typed, true
		}
	}
	return OpaquePayload{}, false
}

var _ PayloadCodec = (*JSONCodec)(nil)
