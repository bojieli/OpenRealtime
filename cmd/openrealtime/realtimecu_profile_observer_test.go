package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/perception"
)

type endpointingASRFixture struct {
	frames      int
	closed      bool
	finalizeErr error
}

func (fixture *endpointingASRFixture) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "fixture", Version: "v1"}
}
func (fixture *endpointingASRFixture) PushFrame(
	context.Context, v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	fixture.frames++
	return nil, nil
}
func (fixture *endpointingASRFixture) Finalize(
	context.Context, uint64,
) (v1.PerceptionRevision, error) {
	if fixture.finalizeErr != nil {
		return v1.PerceptionRevision{}, fixture.finalizeErr
	}
	return v1.PerceptionRevision{
		RevisionID: 1, SourceSample: 1, StableText: "click submit", Final: true,
	}, nil
}

func TestEndpointingAudioObserverFlushAlwaysStartsNextUtteranceClean(t *testing.T) {
	want := errors.New("fixture finalize failed")
	var providers []*endpointingASRFixture
	observer, err := newEndpointingAudioObserver(endpointingAudioObserverConfig{
		Name: "fixture", Source: "microphone",
		Gate: perception.GateConfig{
			Threshold: 0, PrefixPaddingMS: 0, SilenceDurationMS: 20, SpeechDurationMS: 20,
		},
		Provider: func() (v1.PerceptionProvider, error) {
			provider := &endpointingASRFixture{}
			if len(providers) == 0 {
				provider.finalizeErr = want
			}
			providers = append(providers, provider)
			return provider, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone", CapturedNS: 1,
		SampleRateHz: 8_000, PCM16LE: make([]byte, 320),
	}
	for index := range frame.PCM16LE {
		if index%2 == 0 {
			binary.LittleEndian.PutUint16(frame.PCM16LE[index:], uint16(2_000))
		}
	}
	if _, err := observer.Observe(context.Background(), []perception.Frame{frame}); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Flush(context.Background()); !errors.Is(err, want) {
		t.Fatalf("first Flush() error = %v", err)
	}
	if len(providers) != 1 || !providers[0].closed {
		t.Fatalf("failed provider was not closed: %+v", providers)
	}
	frame.CapturedNS++
	if _, err := observer.Observe(context.Background(), []perception.Frame{frame}); err != nil {
		t.Fatal(err)
	}
	observations, err := observer.Flush(context.Background())
	if err != nil || len(observations) != 1 || observations[0].Text != "click submit" {
		t.Fatalf("clean second Flush() = %+v, %v", observations, err)
	}
	if len(providers) != 2 || !providers[1].closed {
		t.Fatalf("second provider lifecycle = %+v", providers)
	}
}
func (fixture *endpointingASRFixture) Close() error { fixture.closed = true; return nil }

func TestEndpointingAudioObserverFinalizesBatchRecognizerOnNegotiatedRateSilence(t *testing.T) {
	for _, sampleRateHz := range []uint32{8_000, 24_000} {
		t.Run(fmt.Sprint(sampleRateHz), func(t *testing.T) {
			testEndpointingAudioObserverFinalizesBatchRecognizerOnSilence(t, sampleRateHz)
		})
	}
}

func testEndpointingAudioObserverFinalizesBatchRecognizerOnSilence(t *testing.T, sampleRateHz uint32) {
	var providers []*endpointingASRFixture
	observer, err := newEndpointingAudioObserver(endpointingAudioObserverConfig{
		Name: "fixture", Source: "microphone",
		Gate: perception.GateConfig{
			Threshold: 0, PrefixPaddingMS: 0, SilenceDurationMS: 20, SpeechDurationMS: 20,
		},
		Cadence: time.Millisecond,
		Provider: func() (v1.PerceptionProvider, error) {
			provider := &endpointingASRFixture{}
			providers = append(providers, provider)
			return provider, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pcm := func(value int16) []byte {
		payload := make([]byte, int(sampleRateHz)*20/1000*2)
		for index := 0; index < len(payload); index += 2 {
			binary.LittleEndian.PutUint16(payload[index:], uint16(value))
		}
		return payload
	}
	frames := []perception.Frame{
		{Kind: perception.FrameAudio, Source: "microphone", CapturedNS: 1, SampleRateHz: sampleRateHz, PCM16LE: pcm(2000)},
		{Kind: perception.FrameAudio, Source: "microphone", CapturedNS: 2, SampleRateHz: sampleRateHz, PCM16LE: pcm(0)},
	}
	var observations []perception.Observation
	for _, frame := range frames {
		current, err := observer.Observe(context.Background(), []perception.Frame{frame})
		if err != nil {
			t.Fatal(err)
		}
		observations = append(observations, current...)
	}
	if len(observations) != 1 || observations[0].Text != "click submit" ||
		!observations[0].Final || observations[0].OccurredNS != 2 {
		t.Fatalf("endpoint observations = %+v", observations)
	}
	if len(providers) != 1 || providers[0].frames == 0 || !providers[0].closed {
		t.Fatalf("provider lifecycle = %+v", providers)
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
}
