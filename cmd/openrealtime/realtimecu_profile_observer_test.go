package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type realtimeCUKeyframeRetainerFixture struct {
	references []trajectory.MediaRef
	payloads   [][]byte
}

func (fixture *realtimeCUKeyframeRetainerFixture) Retain(
	reference trajectory.MediaRef, payload []byte,
) (trajectory.MediaRef, error) {
	reference.Handle = fmt.Sprintf("keyframe-%d", len(fixture.references)+1)
	reference.Bytes = len(payload)
	fixture.references = append(fixture.references, reference)
	fixture.payloads = append(fixture.payloads, slices.Clone(payload))
	return reference, nil
}

func TestRealtimeCULocalObserverAttachesExactChangedScreenAndCameraKeyframes(t *testing.T) {
	config := realtimeCULocalObserverConfig{
		ASRProvider: realtimeCULocalASRProvider, ASRModel: realtimeCULocalASRModel,
		ASRBaseURL: realtimeCULocalASRURL, VideoMode: realtimeCUAttachedKeyframeMode,
		AttachKeyframes: true, ExternalCadence: true, ChangeThreshold: 0.02,
		Gate: perception.DefaultGateConfig(),
	}
	retainer := &realtimeCUKeyframeRetainerFixture{}
	observer, err := newRealtimeCULocalObserver(context.Background(), config, retainer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := observer.Close(); closeErr != nil {
			t.Errorf("close keyframe observer: %v", closeErr)
		}
	})
	for index, source := range []string{"screen", "camera"} {
		payload := realtimeCUProfileJPEG(t, color.RGBA{R: uint8(40 + index*160), G: 20, B: 200, A: 255})
		observations, err := observer.Video(context.Background(), perception.Frame{
			Kind: perception.FrameImage, Source: source, CapturedNS: uint64(index + 1),
			MIMEType: "image/jpeg", Image: payload, Width: 32, Height: 24,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(observations) != 1 || len(observations[0].Media) != 1 ||
			observations[0].Source != source || observations[0].Media[0].Source != source ||
			observations[0].Media[0].Handle == "" ||
			observations[0].Text != "Current "+source+" visual evidence is attached as a keyframe." {
			t.Fatalf("%s keyframe observation = %+v", source, observations)
		}
		if len(retainer.payloads) != index+1 || !bytes.Equal(retainer.payloads[index], payload) ||
			retainer.references[index].CapturedNS != uint64(index+1) ||
			retainer.references[index].Width != 32 || retainer.references[index].Height != 24 {
			t.Fatalf("%s retained keyframe = refs %+v payloads %d", source, retainer.references, len(retainer.payloads))
		}
	}
}

func TestRealtimeCUProductionVisualThresholdAdmitsMeasuredSmallTransitions(t *testing.T) {
	config := realtimeCULocalObserverConfig{
		ASRProvider: realtimeCULocalASRProvider, ASRModel: realtimeCULocalASRModel,
		ASRBaseURL: realtimeCULocalASRURL, VideoMode: realtimeCUAttachedKeyframeMode,
		AttachKeyframes: true, ExternalCadence: true,
		ChangeThreshold: realtimeCUVisualChangeThreshold,
		Gate:            perception.DefaultGateConfig(),
	}
	retainer := &realtimeCUKeyframeRetainerFixture{}
	observer, err := newRealtimeCULocalObserver(context.Background(), config, retainer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := observer.Close(); closeErr != nil {
			t.Errorf("close production-threshold observer: %v", closeErr)
		}
	})
	observe := func(capturedNS uint64, changedPixels int) []perception.Observation {
		t.Helper()
		observations, observeErr := observer.Video(context.Background(), perception.Frame{
			Kind: perception.FrameImage, Source: "screen", CapturedNS: capturedNS,
			MIMEType: "image/png", Image: realtimeCUProfilePNG(t, changedPixels),
			Width: 32, Height: 32,
		})
		if observeErr != nil {
			t.Fatal(observeErr)
		}
		return observations
	}
	if observations := observe(1, 0); len(observations) != 1 {
		t.Fatalf("initial keyframe observations = %+v", observations)
	}
	// Ten of the 32x32 signature cells is 0.9766%, below the production
	// threshold. Eleven is 1.0742%, matching the small target transitions
	// measured in the retained candidate run and must therefore be admitted.
	if observations := observe(2, 10); len(observations) != 0 {
		t.Fatalf("sub-threshold keyframe observations = %+v", observations)
	}
	if observations := observe(3, 11); len(observations) != 1 {
		t.Fatalf("measured small-transition observations = %+v", observations)
	}
	if len(retainer.payloads) != 2 {
		t.Fatalf("retained keyframes = %d, want initial plus admitted transition", len(retainer.payloads))
	}
}

func TestRealtimeCULocalObserverRefusesUnretainedOrNarratedVideoModes(t *testing.T) {
	valid := realtimeCULocalObserverConfig{
		ASRProvider: realtimeCULocalASRProvider, ASRModel: realtimeCULocalASRModel,
		ASRBaseURL: realtimeCULocalASRURL, VideoMode: realtimeCUAttachedKeyframeMode,
		AttachKeyframes: true, ExternalCadence: true, ChangeThreshold: 0.02,
		Gate: perception.DefaultGateConfig(),
	}
	if _, err := newRealtimeCULocalObserver(context.Background(), valid, nil); err == nil ||
		!strings.Contains(err.Error(), "session media retainer") {
		t.Fatalf("nil retainer error = %v", err)
	}
	invalid := valid
	invalid.VideoMode = "dedicated-narrator"
	if _, err := newRealtimeCULocalObserver(context.Background(), invalid, &realtimeCUKeyframeRetainerFixture{}); err == nil ||
		!strings.Contains(err.Error(), "attached-keyframe contract") {
		t.Fatalf("narration fallback error = %v", err)
	}
}

func realtimeCUProfileJPEG(t *testing.T, fill color.RGBA) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			frame.SetRGBA(x, y, fill)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func realtimeCUProfilePNG(t *testing.T, changedPixels int) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for index := 0; index < changedPixels; index++ {
		frame.SetRGBA(index%32, index/32, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

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
