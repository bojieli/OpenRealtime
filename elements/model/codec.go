package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// EncodedPayload is the complete language-neutral body of one element frame.
// JSON, binary data, and authenticated media metadata are kept together so an
// adapter cannot accidentally derive inconsistent wire evidence in two calls.
type EncodedPayload struct {
	JSON   json.RawMessage
	Binary []byte
	Media  *sidecar.MediaFrameMetadata
}

func (payload EncodedPayload) clone() EncodedPayload {
	payload.JSON = slices.Clone(payload.JSON)
	payload.Binary = slices.Clone(payload.Binary)
	payload.Media = cloneMedia(payload.Media)
	return payload
}

// PayloadCodec separates language-neutral wire representation from Go-native
// values. Unknown types remain OpaquePayload; registered types reconstruct an
// exact graph-native value.
type PayloadCodec interface {
	Encode(element.Type, any) (EncodedPayload, error)
	Decode(element.Type, EncodedPayload) (any, error)
	Supports(element.Type) bool
}

type OpaquePayload struct {
	JSON   json.RawMessage
	Binary []byte
	Media  *sidecar.MediaFrameMetadata
}

type PayloadFactory func() any

// PayloadAdapter owns reconstruction for one exact graph type. Nil hooks use
// strict JSON plus the generic BinaryPayload*/MediaPayload* interfaces.
type PayloadAdapter struct {
	Factory       PayloadFactory
	MarshalJSON   func(any) (json.RawMessage, error)
	ExtractBinary func(any) ([]byte, *sidecar.MediaFrameMetadata, error)
	AttachBinary  func(any, []byte, *sidecar.MediaFrameMetadata) error
	payloadType   reflect.Type
}

type BinaryPayloadSource interface{ BinaryPayload() []byte }

type MediaPayloadSource interface {
	MediaFrameMetadata() *sidecar.MediaFrameMetadata
}

type MediaPayloadReceiver interface {
	SetMediaFrameMetadata(sidecar.MediaFrameMetadata) error
}

// JSONCodec provides deterministic strict JSON transport and optional exact
// Go reconstruction. It owns returned buffers and is safe for concurrent use.
type JSONCodec struct {
	mu       sync.RWMutex
	adapters map[string]PayloadAdapter
}

// NewJSONCodec returns an empty codec. NewStandardJSONCodec installs the
// repository's graph-native mappings for production use.
func NewJSONCodec() *JSONCodec {
	return &JSONCodec{adapters: make(map[string]PayloadAdapter)}
}

func (codec *JSONCodec) Register(valueType element.Type, factory PayloadFactory) error {
	return codec.RegisterAdapter(valueType, PayloadAdapter{Factory: factory})
}

func (codec *JSONCodec) RegisterAdapter(valueType element.Type, adapter PayloadAdapter) error {
	if codec == nil {
		return errors.New("register external model payload codec: nil codec")
	}
	if err := valueType.ValidateConcretePort(); err != nil {
		return fmt.Errorf("register external model payload codec: %w", err)
	}
	if adapter.Factory == nil {
		return errors.New("register external model payload codec: nil factory")
	}
	probe := adapter.Factory()
	if probe == nil {
		return errors.New("external model payload factory returned nil")
	}
	reflected := reflect.ValueOf(probe)
	if reflected.Kind() != reflect.Pointer || reflected.IsNil() {
		return fmt.Errorf("external model payload factory must return a non-nil pointer, got %T", probe)
	}
	adapter.payloadType = reflected.Type()
	key := valueType.String()
	codec.mu.Lock()
	defer codec.mu.Unlock()
	if codec.adapters == nil {
		codec.adapters = make(map[string]PayloadAdapter)
	}
	if _, duplicate := codec.adapters[key]; duplicate {
		return fmt.Errorf("external model payload codec for %s is already registered", key)
	}
	codec.adapters[key] = adapter
	return nil
}

func (codec *JSONCodec) Supports(valueType element.Type) bool {
	if codec == nil {
		return false
	}
	codec.mu.RLock()
	defer codec.mu.RUnlock()
	_, found := codec.adapters[valueType.String()]
	return found
}

// Registered is retained as a readable alias for deployment code that wants
// to inspect the immutable codec registry before installing it as a service.
func (codec *JSONCodec) Registered(valueType element.Type) bool { return codec.Supports(valueType) }

func (codec *JSONCodec) Encode(valueType element.Type, payload any) (EncodedPayload, error) {
	if codec == nil {
		return EncodedPayload{}, errors.New("external model payload codec is nil")
	}
	if err := valueType.ValidateConcretePort(); err != nil {
		return EncodedPayload{}, err
	}
	adapter, registered := codec.adapter(valueType)
	if opaque, ok := opaquePayload(payload); ok {
		encoded := EncodedPayload{JSON: opaque.JSON, Binary: opaque.Binary, Media: opaque.Media}
		if err := validateEncodedPayload(valueType, encoded); err != nil {
			return EncodedPayload{}, fmt.Errorf("opaque external model payload: %w", err)
		}
		if registered {
			return EncodedPayload{}, fmt.Errorf(
				"encode %s payload: registered graph-native types cannot bypass their adapter with an opaque payload",
				valueType.String(),
			)
		}
		return encoded.clone(), nil
	}
	if registered && !compatiblePayloadType(payload, adapter.payloadType) {
		return EncodedPayload{}, fmt.Errorf("encode %s payload: got %T, want %s or %s",
			valueType.String(), payload, adapter.payloadType, adapter.payloadType.Elem())
	}
	var data json.RawMessage
	var err error
	if adapter.MarshalJSON != nil {
		data, err = adapter.MarshalJSON(payload)
	} else {
		data, err = json.Marshal(payload)
	}
	if err != nil {
		return EncodedPayload{}, fmt.Errorf("encode %s payload: %w", valueType.String(), err)
	}

	var binary []byte
	var media *sidecar.MediaFrameMetadata
	if adapter.ExtractBinary != nil {
		binary, media, err = adapter.ExtractBinary(payload)
	} else {
		binary, media, err = genericBinaryPayload(payload)
	}
	if err != nil {
		return EncodedPayload{}, fmt.Errorf("encode %s binary payload: %w", valueType.String(), err)
	}
	encoded := EncodedPayload{JSON: data, Binary: binary, Media: media}
	if err := validateEncodedPayload(valueType, encoded); err != nil {
		return EncodedPayload{}, fmt.Errorf("encode %s payload: %w", valueType.String(), err)
	}
	return encoded.clone(), nil
}

func compatiblePayloadType(payload any, expected reflect.Type) bool {
	if payload == nil || expected == nil {
		return false
	}
	actual := reflect.TypeOf(payload)
	if actual != expected && (expected.Kind() != reflect.Pointer || actual != expected.Elem()) {
		return false
	}
	return !reflectedNil(payload)
}

func (codec *JSONCodec) Decode(valueType element.Type, encoded EncodedPayload) (any, error) {
	if codec == nil {
		return nil, errors.New("external model payload codec is nil")
	}
	if err := valueType.ValidateConcretePort(); err != nil {
		return nil, err
	}
	if err := validateEncodedPayload(valueType, encoded); err != nil {
		return nil, fmt.Errorf("external model frame: %w", err)
	}
	adapter, registered := codec.adapter(valueType)
	if !registered {
		encoded = encoded.clone()
		return OpaquePayload{JSON: encoded.JSON, Binary: encoded.Binary, Media: encoded.Media}, nil
	}

	value := adapter.Factory()
	if value == nil {
		return nil, fmt.Errorf("external model payload factory for %s returned nil", valueType.String())
	}
	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Pointer || reflected.IsNil() {
		return nil, fmt.Errorf("external model payload factory for %s returned non-pointer %T",
			valueType.String(), value)
	}
	if reflected.Type() != adapter.payloadType {
		return nil, fmt.Errorf("external model payload factory for %s changed type from %s to %s",
			valueType.String(), adapter.payloadType, reflected.Type())
	}
	data := encoded.JSON
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return nil, fmt.Errorf("decode %s payload: %w", valueType.String(), err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode %s payload: trailing JSON value", valueType.String())
		}
		return nil, fmt.Errorf("decode %s payload: %w", valueType.String(), err)
	}

	if adapter.AttachBinary != nil {
		if err := adapter.AttachBinary(value, slices.Clone(encoded.Binary), cloneMedia(encoded.Media)); err != nil {
			return nil, fmt.Errorf("decode %s binary payload: %w", valueType.String(), err)
		}
	} else if err := attachGenericBinaryPayload(value, encoded.Binary, encoded.Media); err != nil {
		return nil, fmt.Errorf("decode %s binary payload: %w", valueType.String(), err)
	}
	return value, nil
}

func (codec *JSONCodec) adapter(valueType element.Type) (PayloadAdapter, bool) {
	codec.mu.RLock()
	defer codec.mu.RUnlock()
	adapter, found := codec.adapters[valueType.String()]
	return adapter, found
}

func validateEncodedPayload(valueType element.Type, payload EncodedPayload) error {
	if len(payload.JSON) > sidecar.MaxElementJSONBytes || len(payload.Binary) > sidecar.MaxElementBinaryBytes {
		return errors.New("payload exceeds protocol-v4 limits")
	}
	if len(payload.JSON) != 0 {
		if err := strictjson.Validate(payload.JSON); err != nil {
			return fmt.Errorf("payload contains non-strict JSON: %w", err)
		}
	}
	kind, mediaType, err := sidecar.MediaKindForType(valueType)
	if err != nil {
		return err
	}
	if payload.Media != nil {
		if err := payload.Media.Validate(); err != nil {
			return fmt.Errorf("media metadata: %w", err)
		}
		if len(payload.Binary) == 0 {
			return errors.New("media metadata requires a binary body")
		}
		if !mediaType || payload.Media.Kind != kind {
			return fmt.Errorf("media metadata kind %s is incompatible with graph type %s",
				payload.Media.Kind, valueType.String())
		}
	}
	if mediaType && len(payload.Binary) != 0 && payload.Media == nil {
		return fmt.Errorf("binary media type %s requires explicit frame metadata", valueType.String())
	}
	return nil
}

func genericBinaryPayload(payload any) ([]byte, *sidecar.MediaFrameMetadata, error) {
	var binary []byte
	if source, ok := payload.(BinaryPayloadSource); ok {
		if reflectedNil(source) {
			return nil, nil, errors.New("binary payload source is nil")
		}
		binary = slices.Clone(source.BinaryPayload())
	}
	media, err := mediaFrameMetadataOf(payload)
	return binary, media, err
}

func attachGenericBinaryPayload(value any, binary []byte, media *sidecar.MediaFrameMetadata) error {
	if len(binary) != 0 {
		receiver, ok := value.(interface{ SetBinaryPayload([]byte) error })
		if !ok || reflectedNil(receiver) {
			return fmt.Errorf("decoded payload type %T cannot accept binary data", value)
		}
		if err := receiver.SetBinaryPayload(slices.Clone(binary)); err != nil {
			return err
		}
	}
	if media == nil {
		return nil
	}
	receiver, ok := value.(MediaPayloadReceiver)
	if !ok || reflectedNil(receiver) {
		return fmt.Errorf("decoded media payload type %T cannot accept media frame metadata", value)
	}
	return receiver.SetMediaFrameMetadata(*media)
}

func opaquePayload(payload any) (OpaquePayload, bool) {
	var opaque OpaquePayload
	switch typed := payload.(type) {
	case OpaquePayload:
		opaque = typed
	case *OpaquePayload:
		if typed == nil {
			return OpaquePayload{}, false
		}
		opaque = *typed
	default:
		return OpaquePayload{}, false
	}
	opaque.JSON = slices.Clone(opaque.JSON)
	opaque.Binary = slices.Clone(opaque.Binary)
	opaque.Media = cloneMedia(opaque.Media)
	return opaque, true
}

func cloneMedia(source *sidecar.MediaFrameMetadata) *sidecar.MediaFrameMetadata {
	if source == nil {
		return nil
	}
	media := *source
	return &media
}

func mediaFrameMetadataOf(payload any) (*sidecar.MediaFrameMetadata, error) {
	if opaque, ok := opaquePayload(payload); ok {
		if opaque.Media == nil {
			return nil, nil
		}
		if err := opaque.Media.Validate(); err != nil {
			return nil, err
		}
		return cloneMedia(opaque.Media), nil
	}
	source, ok := payload.(MediaPayloadSource)
	if !ok {
		return nil, nil
	}
	if reflectedNil(source) {
		return nil, errors.New("media payload source is nil")
	}
	declared := source.MediaFrameMetadata()
	if declared == nil {
		return nil, nil
	}
	if err := declared.Validate(); err != nil {
		return nil, err
	}
	return cloneMedia(declared), nil
}

func attachMediaFrameMetadata(payload any, source *sidecar.MediaFrameMetadata) (any, error) {
	if source == nil {
		return payload, nil
	}
	media := cloneMedia(source)
	if err := media.Validate(); err != nil {
		return nil, err
	}
	switch typed := payload.(type) {
	case OpaquePayload:
		typed.Media = media
		return typed, nil
	case *OpaquePayload:
		if typed == nil {
			return nil, errors.New("cannot attach media metadata to nil opaque payload")
		}
		copy := *typed
		copy.JSON = slices.Clone(typed.JSON)
		copy.Binary = slices.Clone(typed.Binary)
		copy.Media = media
		return &copy, nil
	}
	receiver, ok := payload.(MediaPayloadReceiver)
	if !ok {
		return nil, fmt.Errorf("decoded media payload type %T cannot accept media frame metadata", payload)
	}
	if reflectedNil(receiver) {
		return nil, fmt.Errorf("decoded media payload type %T is nil", payload)
	}
	if err := receiver.SetMediaFrameMetadata(*media); err != nil {
		return nil, err
	}
	return payload, nil
}

var _ PayloadCodec = (*JSONCodec)(nil)
