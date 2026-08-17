package livebench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

type wireRecorder struct {
	mu     sync.Mutex
	origin time.Time
	events []WireEvent
}

func newWireRecorder(origin time.Time) *wireRecorder {
	return &wireRecorder{origin: origin}
}

func (recorder *wireRecorder) add(direction, eventType, providerEventID string, payload json.RawMessage, audio []byte) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	event := WireEvent{
		Sequence: uint64(len(recorder.events)), MonotonicNS: time.Since(recorder.origin).Nanoseconds(),
		Direction: direction, Type: eventType, ProviderEventID: providerEventID,
	}
	if len(payload) > 0 {
		event.Payload = append(json.RawMessage(nil), payload...)
	}
	if len(audio) > 0 {
		digest := sha256.Sum256(audio)
		event.AudioBytes = len(audio)
		event.AudioSHA256 = hex.EncodeToString(digest[:])
	}
	recorder.events = append(recorder.events, event)
}

func (recorder *wireRecorder) snapshot() []WireEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	result := make([]WireEvent, len(recorder.events))
	copy(result, recorder.events)
	return result
}

func compactPayload(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
