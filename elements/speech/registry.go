package speech

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

type TTSProviderFactory func() (v1.SpeechProvider, error)

type ttsProviderEntry struct {
	descriptor v1.Descriptor
	factory    TTSProviderFactory
}

// TTSProviderRegistry is deployment-owned provider resolution. Graph source
// contains only the symbolic reference selected in the separate values
// artifact; credentials and concrete clients never enter topology or Graph IR.
type TTSProviderRegistry struct {
	mu      sync.RWMutex
	entries map[string]ttsProviderEntry
}

func NewTTSProviderRegistry() *TTSProviderRegistry {
	return &TTSProviderRegistry{entries: make(map[string]ttsProviderEntry)}
}

func (registry *TTSProviderRegistry) Register(
	reference string, descriptor v1.Descriptor, factory TTSProviderFactory,
) error {
	if registry == nil {
		return errors.New("register TTS provider: nil registry")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("register TTS provider: empty reference")
	}
	if err := descriptor.Validate(); err != nil {
		return fmt.Errorf("register TTS provider %q: %w", reference, err)
	}
	if factory == nil {
		return fmt.Errorf("register TTS provider %q: nil factory", reference)
	}
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]ttsProviderEntry)
	}
	if _, duplicate := registry.entries[reference]; duplicate {
		return fmt.Errorf("TTS provider %q is already registered", reference)
	}
	registry.entries[reference] = ttsProviderEntry{descriptor: descriptor, factory: factory}
	return nil
}

func (registry *TTSProviderRegistry) resolve(reference string) (ttsProviderEntry, error) {
	if registry == nil {
		return ttsProviderEntry{}, errors.New("TTS provider registry is nil")
	}
	registry.mu.RLock()
	entry, found := registry.entries[strings.TrimSpace(reference)]
	registry.mu.RUnlock()
	if !found {
		return ttsProviderEntry{}, fmt.Errorf("TTS provider %q is not registered", reference)
	}
	entry.descriptor.Capabilities = maps.Clone(entry.descriptor.Capabilities)
	return entry, nil
}

// PlaybackSink extends the proven action-plane sink contract with a live
// descriptor. A legacy sink can be adapted explicitly; the graph never treats
// a deployment assertion as proof of what resource was actually mounted.
type PlaybackSink interface {
	action.SpeechSink
	Descriptor() v1.Descriptor
}

type PlaybackSinkFactory func() (PlaybackSink, error)

type playbackSinkEntry struct {
	descriptor v1.Descriptor
	factory    PlaybackSinkFactory
}

type PlaybackSinkRegistry struct {
	mu      sync.RWMutex
	entries map[string]playbackSinkEntry
}

func NewPlaybackSinkRegistry() *PlaybackSinkRegistry {
	return &PlaybackSinkRegistry{entries: make(map[string]playbackSinkEntry)}
}

func (registry *PlaybackSinkRegistry) Register(
	reference string, descriptor v1.Descriptor, factory PlaybackSinkFactory,
) error {
	if registry == nil {
		return errors.New("register playback sink: nil registry")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("register playback sink: empty reference")
	}
	if err := descriptor.Validate(); err != nil {
		return fmt.Errorf("register playback sink %q: %w", reference, err)
	}
	if factory == nil {
		return fmt.Errorf("register playback sink %q: nil factory", reference)
	}
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]playbackSinkEntry)
	}
	if _, duplicate := registry.entries[reference]; duplicate {
		return fmt.Errorf("playback sink %q is already registered", reference)
	}
	registry.entries[reference] = playbackSinkEntry{descriptor: descriptor, factory: factory}
	return nil
}

func (registry *PlaybackSinkRegistry) resolve(reference string) (playbackSinkEntry, error) {
	if registry == nil {
		return playbackSinkEntry{}, errors.New("playback sink registry is nil")
	}
	registry.mu.RLock()
	entry, found := registry.entries[strings.TrimSpace(reference)]
	registry.mu.RUnlock()
	if !found {
		return playbackSinkEntry{}, fmt.Errorf("playback sink %q is not registered", reference)
	}
	entry.descriptor.Capabilities = maps.Clone(entry.descriptor.Capabilities)
	return entry, nil
}

func verifyProvider(reference string, expected v1.Descriptor, provider v1.SpeechProvider) error {
	if provider == nil || reflectedNil(provider) {
		return fmt.Errorf("TTS provider %q factory returned nil", reference)
	}
	actual := provider.Descriptor()
	if err := actual.Validate(); err != nil {
		return fmt.Errorf("TTS provider %q returned an invalid descriptor: %w", reference, err)
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("TTS provider %q descriptor drifted: registered %+v, live %+v",
			reference, expected, actual)
	}
	_, streaming := provider.(v1.StreamingSpeechProvider)
	if actual.Capabilities.Has(v1.CapabilityStreamingOutput) != streaming {
		return fmt.Errorf("TTS provider %q streaming_output capability is %t but live interface support is %t",
			reference, actual.Capabilities.Has(v1.CapabilityStreamingOutput), streaming)
	}
	if !actual.Capabilities.Has(v1.CapabilityPCM16Output) {
		return fmt.Errorf("TTS provider %q does not resolve to the PCM16 output required by speech.AudioFrame",
			reference)
	}
	return nil
}

func verifySink(reference string, expected v1.Descriptor, sink PlaybackSink) error {
	if sink == nil || reflectedNil(sink) {
		return fmt.Errorf("playback sink %q factory returned nil", reference)
	}
	actual := sink.Descriptor()
	if err := actual.Validate(); err != nil {
		return fmt.Errorf("playback sink %q returned an invalid descriptor: %w", reference, err)
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("playback sink %q descriptor drifted: registered %+v, live %+v",
			reference, expected, actual)
	}
	if !actual.Capabilities.Has(v1.CapabilityStreamingInput) {
		return fmt.Errorf("playback sink %q does not accept streaming audio frames", reference)
	}
	return nil
}

func cloneDescriptor(descriptor v1.Descriptor) v1.Descriptor {
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	return descriptor
}

func closeResource(resource any) error {
	if resource == nil || reflectedNil(resource) {
		return nil
	}
	if closer, ok := resource.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func reflectedNil(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
