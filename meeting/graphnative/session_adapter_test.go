package graphnative

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/perception"
)

func TestMeetingSessionAdapterPreservesExternalMediaClock(t *testing.T) {
	audio := newTestOutput("audio", modelelements.AudioInputType())
	video := newTestOutput("video", modelelements.VideoInputType())
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-clock-test",
		ports:     meetingAdapterPorts{audio: audio, video: video},
		frameRate: 5_000, videoCaptured: make(map[string]uint64),
	}

	audioFrame := perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone", CapturedNS: 101,
		PCM16LE: []byte{0, 0}, SampleRateHz: 24_000,
	}
	if err := adapter.Audio(context.Background(), audioFrame); err != nil {
		t.Fatal(err)
	}
	audioEnvelope := outputEnvelope(t, audio)
	assertMeetingExternalClock(t, audioEnvelope, audioFrame.CapturedNS)
	audioPayload, ok := audioEnvelope.Payload.(acousticelements.InputFrame)
	if !ok || audioPayload.Frame.CapturedNS != audioEnvelope.CaptureNS {
		t.Fatalf("Meeting audio clock payload=%T %+v envelope=%+v",
			audioEnvelope.Payload, audioPayload, audioEnvelope)
	}

	videoFrame := perception.Frame{
		Kind: perception.FrameImage, Source: "screen", CapturedNS: 202,
		Image: []byte{1}, MIMEType: "image/png", Width: 1, Height: 1, Index: 1,
	}
	if err := adapter.Video(context.Background(), videoFrame); err != nil {
		t.Fatal(err)
	}
	videoEnvelope := outputEnvelope(t, video)
	assertMeetingExternalClock(t, videoEnvelope, videoFrame.CapturedNS)
	videoPayload, ok := videoEnvelope.Payload.(modelelements.VideoInputFrame)
	if !ok || videoPayload.Frame.CapturedNS != videoEnvelope.CaptureNS {
		t.Fatalf("Meeting video clock payload=%T %+v envelope=%+v",
			videoEnvelope.Payload, videoPayload, videoEnvelope)
	}
}

func assertMeetingExternalClock(t testing.TB, envelope element.Envelope, want uint64) {
	t.Helper()
	if envelope.CaptureNS != want || envelope.CaptureNS == 0 {
		t.Fatalf("Meeting media capture clock = %d, want %d", envelope.CaptureNS, want)
	}
}

type blockedMeetingSpeechSink struct {
	beginEntered chan struct{}
	releaseBegin chan struct{}
	began        atomic.Bool
	ended        atomic.Bool
	turnEnded    atomic.Bool
}

func (*blockedMeetingSpeechSink) TurnBegin(context.Context) error { return nil }
func (sink *blockedMeetingSpeechSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	if !sink.ended.Load() {
		return errors.New("turn ended before speech")
	}
	sink.turnEnded.Store(true)
	return nil
}
func (*blockedMeetingSpeechSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }
func (*blockedMeetingSpeechSink) Transcript(context.Context, legacy.TranscriptEvent) error {
	return nil
}
func (*blockedMeetingSpeechSink) Observation(context.Context, perception.Observation) error {
	return nil
}
func (sink *blockedMeetingSpeechSink) SpeechBegin(context.Context, action.Utterance) error {
	close(sink.beginEntered)
	<-sink.releaseBegin
	sink.began.Store(true)
	return nil
}
func (sink *blockedMeetingSpeechSink) SpeechText(context.Context, action.Utterance, string) error {
	if !sink.began.Load() {
		return errors.New("text overtook speech begin")
	}
	return nil
}
func (*blockedMeetingSpeechSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (sink *blockedMeetingSpeechSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	sink.ended.Store(true)
	return nil
}
func (*blockedMeetingSpeechSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (*blockedMeetingSpeechSink) Failed(context.Context, legacy.ErrorEvent)             {}

func TestMeetingSessionAdapterOrdersCrossPortSpeechLifecycle(t *testing.T) {
	sink := &blockedMeetingSpeechSink{
		beginEntered: make(chan struct{}), releaseBegin: make(chan struct{}),
	}
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-speech-order", sink: sink,
		activeSpeech: make(map[string]*meetingAdapterSpeech),
	}
	runID := "run-1"
	utterance := action.Utterance{ID: "speech-1", Text: "Hello."}
	beginDone := make(chan error, 1)
	go func() {
		beginDone <- adapter.publishAudio(context.Background(), element.Envelope{
			RunID: runID, Payload: speechelements.AudioFrame{
				Kind: speechelements.AudioBegin, UtteranceID: utterance.ID, Utterance: utterance,
			},
		})
	}()
	<-sink.beginEntered
	textDone := make(chan error, 1)
	go func() {
		textDone <- adapter.publishText(context.Background(), element.Envelope{
			RunID: runID, Payload: cognitionelements.PreparedTextDelta{
				Boundary: cognitionelements.TextChunk, Index: 1, Text: "Hello.",
			},
		})
	}()
	select {
	case err := <-textDone:
		t.Fatalf("text did not wait for SpeechBegin: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(sink.releaseBegin)
	if err := <-beginDone; err != nil {
		t.Fatal(err)
	}
	if err := <-textDone; err != nil {
		t.Fatal(err)
	}

	outcomeDone := make(chan error, 1)
	go func() {
		outcomeDone <- adapter.publishForegroundOutcome(context.Background(), element.Envelope{
			RunID: runID, Payload: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
			},
		})
	}()
	select {
	case err := <-outcomeDone:
		t.Fatalf("outcome did not wait for SpeechEnd: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := adapter.publishAudio(context.Background(), element.Envelope{
		RunID: runID, Payload: speechelements.AudioFrame{
			Kind: speechelements.AudioEnd, UtteranceID: utterance.ID,
			Terminal: speechelements.SynthesisOutcome{
				UtteranceID: utterance.ID, Kind: speechelements.OutcomeSucceeded,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-outcomeDone; err != nil {
		t.Fatal(err)
	}
	if !sink.turnEnded.Load() {
		t.Fatal("foreground outcome did not close the turn after speech")
	}
}

var _ legacy.Sink = (*blockedMeetingSpeechSink)(nil)
