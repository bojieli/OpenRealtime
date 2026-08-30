package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/perception"
)

// endpointingAudioObserver adds an acoustic endpoint to a provider-neutral
// perception observer. Realtime-CU receives protocol audio frames rather than
// a binding-owned turn-commit callback, so batch recognizers must be finalized
// when the selected acoustic gate observes terminal silence. Streaming
// recognizers keep emitting their typed partial revisions through the same
// inner observer.
type endpointingAudioObserver struct {
	mu         sync.Mutex
	name       string
	source     string
	gateConfig perception.GateConfig
	provider   func() (v1.PerceptionProvider, error)
	cadence    time.Duration
	gate       *perception.EnergyGate
	gateRateHz uint32
	inner      *perception.AudioObserver
	closed     bool
}

type endpointingAudioObserverConfig struct {
	Name     string
	Source   string
	Gate     perception.GateConfig
	Provider func() (v1.PerceptionProvider, error)
	Cadence  time.Duration
}

func newEndpointingAudioObserver(config endpointingAudioObserverConfig) (*endpointingAudioObserver, error) {
	if strings.TrimSpace(config.Name) == "" || config.Name != strings.TrimSpace(config.Name) ||
		strings.TrimSpace(config.Source) == "" || config.Source != strings.TrimSpace(config.Source) {
		return nil, errors.New("Realtime-CU endpointing observer requires a canonical name and source")
	}
	if config.Provider == nil {
		return nil, errors.New("Realtime-CU endpointing observer requires an ASR factory")
	}
	// Validate configuration before allocating the provider. The live gate is
	// instantiated lazily from the negotiated frame rate so this observer does
	// not impose a transport-specific resampling policy.
	if _, err := perception.NewEnergyGate(config.Gate, 24_000); err != nil {
		return nil, fmt.Errorf("Realtime-CU endpointing gate: %w", err)
	}
	inner, err := perception.NewAudioObserver(perception.AudioConfig{
		Provider: config.Provider, Name: config.Name, Source: config.Source, Cadence: config.Cadence,
	})
	if err != nil {
		return nil, err
	}
	return &endpointingAudioObserver{
		name: config.Name, source: config.Source, gateConfig: config.Gate,
		provider: config.Provider, cadence: config.Cadence, inner: inner,
	}, nil
}

func (observer *endpointingAudioObserver) Name() string { return observer.name }

func (observer *endpointingAudioObserver) Cadence() time.Duration { return observer.cadence }

func (observer *endpointingAudioObserver) Accepts(frame perception.Frame) bool {
	return frame.Kind == perception.FrameAudio && frame.Source == observer.source
}

// The acoustic decision is stateful and therefore belongs in Observe. Gate is
// intentionally a cheap structural admission predicate, matching the core
// perception.Observer contract.
func (observer *endpointingAudioObserver) Gate(frame perception.Frame) bool {
	return observer.Accepts(frame) && len(frame.PCM16LE) != 0
}

func (observer *endpointingAudioObserver) Observe(
	ctx context.Context, frames []perception.Frame,
) ([]perception.Observation, error) {
	if ctx == nil {
		return nil, errors.New("observe Realtime-CU endpointing audio: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.closed {
		return nil, errors.New("Realtime-CU endpointing audio observer is closed")
	}
	var observations []perception.Observation
	for _, frame := range frames {
		if !observer.Accepts(frame) {
			return nil, errors.New("Realtime-CU endpointing audio observer received an incompatible frame")
		}
		if err := frame.Validate(); err != nil {
			return nil, err
		}
		if observer.gate == nil {
			gate, err := perception.NewEnergyGate(observer.gateConfig, frame.SampleRateHz)
			if err != nil {
				return nil, err
			}
			observer.gate = gate
			observer.gateRateHz = frame.SampleRateHz
		} else if frame.SampleRateHz != observer.gateRateHz {
			return nil, fmt.Errorf(
				"Realtime-CU endpointing audio rate changed within an utterance: %d Hz after %d Hz",
				frame.SampleRateHz, observer.gateRateHz,
			)
		}
		admitted, err := observer.gate.Push(frame.PCM16LE)
		if err != nil {
			return nil, err
		}
		if len(admitted.Audio) != 0 {
			admittedFrame := frame
			admittedFrame.PCM16LE = admitted.Audio
			partial, err := observer.inner.Observe(ctx, []perception.Frame{admittedFrame})
			if err != nil {
				return nil, err
			}
			observations = append(observations, partial...)
		}
		if admitted.Stopped {
			final, err := observer.finalizeLocked(ctx, frame.CapturedNS)
			observations = append(observations, final...)
			if err != nil {
				return observations, err
			}
		}
	}
	return observations, nil
}

func (observer *endpointingAudioObserver) Flush(ctx context.Context) ([]perception.Observation, error) {
	if ctx == nil {
		return nil, errors.New("flush Realtime-CU endpointing audio: nil context")
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.closed {
		return nil, errors.New("Realtime-CU endpointing audio observer is closed")
	}
	return observer.finalizeLocked(ctx, 0)
}

// finalizeLocked makes terminal cleanup unconditional. A failed batch
// recognizer cannot leave its provider or acoustic state attached to the next
// utterance, and a manual flush has the same lifecycle as a silence endpoint.
func (observer *endpointingAudioObserver) finalizeLocked(
	ctx context.Context, occurredNS uint64,
) ([]perception.Observation, error) {
	final, flushErr := observer.inner.Flush(ctx)
	closeErr := observer.inner.Close()
	observer.gate = nil
	observer.gateRateHz = 0
	for index := range final {
		if final[index].OccurredNS == 0 && occurredNS != 0 {
			final[index].OccurredNS = occurredNS
		}
	}
	return final, errors.Join(flushErr, closeErr)
}

func (observer *endpointingAudioObserver) Reset() {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.closed {
		return
	}
	observer.inner.Reset()
	observer.gate = nil
	observer.gateRateHz = 0
}

func (observer *endpointingAudioObserver) Close() error {
	observer.mu.Lock()
	if observer.closed {
		observer.mu.Unlock()
		return nil
	}
	observer.closed = true
	observer.gate = nil
	observer.gateRateHz = 0
	inner := observer.inner
	observer.mu.Unlock()
	return inner.Close()
}

var _ perception.Observer = (*endpointingAudioObserver)(nil)
var _ io.Closer = (*endpointingAudioObserver)(nil)
