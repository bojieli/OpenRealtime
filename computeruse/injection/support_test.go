package injection_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"sync"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
)

type staticASR struct{ text string }

func (staticASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "static", Version: "1", Capabilities: v1.Capabilities{}}
}

func (staticASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}

func (asr staticASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 1, StableText: asr.text, Final: true}, nil
}

type scriptedProvider struct {
	descriptor continuation.Descriptor
	mu         sync.Mutex
	turns      [][]continuation.Event
	calls      int
}

func (provider *scriptedProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scriptedProvider) Continue(
	_ context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
	var events []continuation.Event
	if index < len(provider.turns) {
		events = provider.turns[index]
	}
	provider.mu.Unlock()
	for _, event := range events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

type toneSpeech struct{}

func (toneSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "tone", Version: "1", Capabilities: v1.Capabilities{}}
}

func (toneSpeech) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("unused")
}

func (toneSpeech) Stream(_ context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	return emit(v1.SpeechChunk{
		ChunkID: "c", CandidateID: plan.CandidateID, SampleRateHz: 24_000,
		PCM16LE: make([]byte, 4800), Final: true,
	})
}

type silentSink struct{}

func (silentSink) Activity(context.Context, binding.ActivityEvent) error      { return nil }
func (silentSink) Transcript(context.Context, binding.TranscriptEvent) error  { return nil }
func (silentSink) Observation(context.Context, perception.Observation) error  { return nil }
func (silentSink) SpeechBegin(context.Context, action.Utterance) error        { return nil }
func (silentSink) SpeechText(context.Context, action.Utterance, string) error { return nil }
func (silentSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (silentSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error { return nil }
func (silentSink) ToolCalls(context.Context, binding.ToolCallEvent) error            { return nil }
func (silentSink) Failed(context.Context, binding.ErrorEvent)                        {}

func screenFrame() perception.Frame {
	canvas := image.NewGray(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			canvas.SetGray(x, y, color.Gray{Y: uint8((x + y) % 255)})
		}
	}
	var buffer bytes.Buffer
	_ = jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 80})
	return perception.Frame{
		Kind: perception.FrameImage, Source: "screen", Image: buffer.Bytes(),
		MIMEType: "image/jpeg", Width: 320, Height: 240,
	}
}
