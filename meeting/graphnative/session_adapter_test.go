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
	"github.com/bojieli/OpenRealtime/trajectory"
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
	beginEntered     chan struct{}
	releaseBegin     chan struct{}
	turnBeginEntered chan struct{}
	releaseTurnBegin chan struct{}
	requireSpeechEnd bool
	began            atomic.Bool
	ended            atomic.Bool
	turnEnded        atomic.Bool
	turnBegins       atomic.Int32
	turnEnds         atomic.Int32
	toolCalls        atomic.Int32
}

func (sink *blockedMeetingSpeechSink) TurnBegin(context.Context) error {
	sink.turnBegins.Add(1)
	if sink.turnBeginEntered != nil {
		select {
		case sink.turnBeginEntered <- struct{}{}:
		default:
		}
	}
	if sink.releaseTurnBegin != nil {
		<-sink.releaseTurnBegin
	}
	return nil
}
func (sink *blockedMeetingSpeechSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	if sink.requireSpeechEnd && !sink.ended.Load() {
		return errors.New("turn ended before speech")
	}
	sink.turnEnds.Add(1)
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
func (sink *blockedMeetingSpeechSink) ToolCalls(context.Context, legacy.ToolCallEvent) error {
	sink.toolCalls.Add(1)
	return nil
}
func (*blockedMeetingSpeechSink) Failed(context.Context, legacy.ErrorEvent) {}

func TestMeetingSessionAdapterOrdersCrossPortSpeechLifecycle(t *testing.T) {
	sink := &blockedMeetingSpeechSink{
		beginEntered: make(chan struct{}), releaseBegin: make(chan struct{}), requireSpeechEnd: true,
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

func TestMeetingSessionAdapterGroupsVisualRunWhileSpeechDrains(t *testing.T) {
	releaseSpeech := make(chan struct{})
	close(releaseSpeech)
	sink := &blockedMeetingSpeechSink{
		beginEntered: make(chan struct{}, 1), releaseBegin: releaseSpeech, requireSpeechEnd: true,
	}
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-overlap", sink: sink,
		activeSpeech: make(map[string]*meetingAdapterSpeech),
	}
	voiceRun, visualRun := "foreground-run-voice", "foreground-run-visual"
	utterance := action.Utterance{ID: "speech-overlap", Text: "The conversion rate is 18.4 percent."}
	if err := adapter.publishAudio(context.Background(), element.Envelope{
		RunID: voiceRun, Payload: speechelements.AudioFrame{
			Kind: speechelements.AudioBegin, UtteranceID: utterance.ID, Utterance: utterance,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.publishTool(context.Background(), element.Envelope{
		RunID: visualRun, Payload: cognitionelements.ToolProposal{Call: trajectory.ToolCall{
			CallID: "visual-call-1", Name: "computer.click_normalized",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.publishForegroundOutcome(context.Background(), element.Envelope{
		RunID: visualRun, Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: visualRun,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := sink.turnBegins.Load(); got != 1 {
		t.Fatalf("overlapping graph runs opened %d Realtime responses, want 1", got)
	}
	if got := sink.toolCalls.Load(); got != 1 {
		t.Fatalf("visual calls = %d, want 1", got)
	}
	if got := sink.turnEnds.Load(); got != 0 {
		t.Fatalf("visual run closed response while voice run was active: %d", got)
	}
	if err := adapter.ensureTurn(context.Background(), visualRun); err == nil {
		t.Fatal("completed visual run was admitted into the same response again")
	}

	voiceOutcome := make(chan error, 1)
	go func() {
		voiceOutcome <- adapter.publishForegroundOutcome(context.Background(), element.Envelope{
			RunID: voiceRun, Payload: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: voiceRun,
			},
		})
	}()
	select {
	case err := <-voiceOutcome:
		t.Fatalf("voice outcome did not wait for its speech: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := adapter.publishAudio(context.Background(), element.Envelope{
		RunID: voiceRun, Payload: speechelements.AudioFrame{
			Kind: speechelements.AudioEnd, UtteranceID: utterance.ID,
			Terminal: speechelements.SynthesisOutcome{
				UtteranceID: utterance.ID, Kind: speechelements.OutcomeSucceeded,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-voiceOutcome; err != nil {
		t.Fatal(err)
	}
	if got := sink.turnEnds.Load(); got != 1 {
		t.Fatalf("terminal response boundaries = %d, want 1", got)
	}
}

func TestMeetingSessionAdapterOrdersConcurrentOutputAfterTurnBegin(t *testing.T) {
	turnBeginEntered := make(chan struct{}, 1)
	releaseTurnBegin := make(chan struct{})
	sink := &blockedMeetingSpeechSink{
		turnBeginEntered: turnBeginEntered, releaseTurnBegin: releaseTurnBegin,
	}
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-turn-order", sink: sink,
		activeSpeech: make(map[string]*meetingAdapterSpeech),
	}
	publish := func(runID, callID string) error {
		return adapter.publishTool(context.Background(), element.Envelope{
			RunID: runID, Payload: cognitionelements.ToolProposal{Call: trajectory.ToolCall{
				CallID: callID, Name: "computer.click_normalized",
			}},
		})
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- publish("run-first", "call-first") }()
	<-turnBeginEntered
	go func() { second <- publish("run-second", "call-second") }()
	select {
	case err := <-second:
		t.Fatalf("second output overtook TurnBegin: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := sink.toolCalls.Load(); got != 0 {
		t.Fatalf("tool outputs before TurnBegin completed = %d", got)
	}
	close(releaseTurnBegin)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-first", "run-second"} {
		if err := adapter.publishForegroundOutcome(context.Background(), element.Envelope{
			RunID: runID, Payload: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := sink.turnBegins.Load(); got != 1 {
		t.Fatalf("TurnBegin count = %d, want 1", got)
	}
	if got := sink.toolCalls.Load(); got != 2 {
		t.Fatalf("tool output count = %d, want 2", got)
	}
	if got := sink.turnEnds.Load(); got != 1 {
		t.Fatalf("TurnEnd count = %d, want 1", got)
	}
}

var _ legacy.Sink = (*blockedMeetingSpeechSink)(nil)
