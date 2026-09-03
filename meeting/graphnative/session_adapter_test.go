package graphnative

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	flowelements "github.com/bojieli/OpenRealtime/elements/flow"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
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
	if err := adapter.publishText(context.Background(), element.Envelope{
		RunID: runID, Payload: cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextBegin, Index: 0,
		},
	}); err != nil {
		t.Fatal(err)
	}
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
	if err := adapter.publishText(context.Background(), element.Envelope{
		RunID: runID, Payload: cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextEnd, Index: 2,
		},
	}); err != nil {
		t.Fatal(err)
	}

	outcomeDone := make(chan error, 1)
	go func() {
		outcomeDone <- adapter.publishForegroundOutcome(context.Background(), element.Envelope{
			RunID: runID, Payload: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
				ProviderReference: ForegroundDeploymentReference,
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

func TestMeetingSessionAdapterConsumesNoOpChunkAndTerminalText(t *testing.T) {
	sink := &orderedMeetingSink{done: make(chan struct{})}
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-terminal-text", sink: sink,
		activeSpeech: make(map[string]*meetingAdapterSpeech), completedRuns: make(map[string]struct{}),
	}
	const runID = "terminal-text-run"
	utterance := action.Utterance{ID: "terminal-text-speech"}
	if err := adapter.publishAudio(context.Background(), element.Envelope{
		RunID: runID, Payload: speechelements.AudioFrame{
			Kind: speechelements.AudioBegin, UtteranceID: utterance.ID, Utterance: utterance,
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []cognitionelements.PreparedTextDelta{
		{Boundary: cognitionelements.TextBegin, Index: 0},
		{Boundary: cognitionelements.TextChunk, Index: 1},
		{Boundary: cognitionelements.TextEnd, Index: 2, Text: "terminal text"},
	} {
		if err := adapter.publishText(context.Background(), element.Envelope{
			RunID: runID, Payload: delta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if events, _, _ := sink.snapshot(); !slices.Equal(events,
		[]string{"turn_begin", "speech_begin", "text:terminal text"}) {
		t.Fatalf("safe terminal text events = %v", events)
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
			ProviderReference: ForegroundDeploymentReference,
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
				ProviderReference: ForegroundDeploymentReference,
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
				ProviderReference: ForegroundDeploymentReference,
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

type orderedMeetingSink struct {
	mu       sync.Mutex
	events   []string
	outcomes []legacy.TurnOutcome
	failed   []legacy.ErrorEvent
	done     chan struct{}
	doneOnce sync.Once
}

func (sink *orderedMeetingSink) record(value string) {
	sink.mu.Lock()
	sink.events = append(sink.events, value)
	sink.mu.Unlock()
}

func (sink *orderedMeetingSink) snapshot() ([]string, []legacy.TurnOutcome, []legacy.ErrorEvent) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]string(nil), sink.events...),
		append([]legacy.TurnOutcome(nil), sink.outcomes...),
		append([]legacy.ErrorEvent(nil), sink.failed...)
}

func (sink *orderedMeetingSink) TurnBegin(context.Context) error {
	sink.record("turn_begin")
	return nil
}
func (sink *orderedMeetingSink) TurnEnd(_ context.Context, outcome legacy.TurnOutcome) error {
	sink.mu.Lock()
	sink.events = append(sink.events, "turn_end")
	sink.outcomes = append(sink.outcomes, outcome)
	sink.mu.Unlock()
	if sink.done != nil {
		sink.doneOnce.Do(func() { close(sink.done) })
	}
	return nil
}
func (*orderedMeetingSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }
func (*orderedMeetingSink) Transcript(context.Context, legacy.TranscriptEvent) error {
	return nil
}
func (*orderedMeetingSink) Observation(context.Context, perception.Observation) error { return nil }
func (sink *orderedMeetingSink) SpeechBegin(context.Context, action.Utterance) error {
	sink.record("speech_begin")
	return nil
}
func (sink *orderedMeetingSink) SpeechText(_ context.Context, _ action.Utterance, text string) error {
	sink.record("text:" + text)
	return nil
}
func (*orderedMeetingSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (sink *orderedMeetingSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	sink.record("speech_end")
	return nil
}
func (sink *orderedMeetingSink) ToolCalls(_ context.Context, event legacy.ToolCallEvent) error {
	callID := ""
	if len(event.Calls) != 0 {
		callID = event.Calls[0].CallID
	}
	sink.record("tool:" + callID)
	return nil
}
func (sink *orderedMeetingSink) Failed(_ context.Context, event legacy.ErrorEvent) {
	sink.mu.Lock()
	sink.events = append(sink.events, "failed:"+event.Code)
	sink.failed = append(sink.failed, event)
	sink.mu.Unlock()
}

var _ legacy.Sink = (*orderedMeetingSink)(nil)

type gatedMeetingInput struct {
	element.InputPort
	gate <-chan struct{}
	once sync.Once
}

type notifyingMeetingInput struct {
	element.InputPort
	received chan struct{}
	once     sync.Once
}

func (input *notifyingMeetingInput) Receive(ctx context.Context) (element.Envelope, error) {
	envelope, err := input.InputPort.Receive(ctx)
	if err == nil {
		input.once.Do(func() { close(input.received) })
	}
	return envelope, err
}

func (input *notifyingMeetingInput) ReceiveAny(ctx context.Context) (element.Envelope, string, error) {
	envelope, err := input.Receive(ctx)
	return envelope, input.Name(), err
}

func (input *gatedMeetingInput) Receive(ctx context.Context) (element.Envelope, error) {
	var waitErr error
	input.once.Do(func() {
		select {
		case <-input.gate:
		case <-ctx.Done():
			waitErr = context.Cause(ctx)
		}
	})
	if waitErr != nil {
		return element.Envelope{}, waitErr
	}
	return input.InputPort.Receive(ctx)
}

func (input *gatedMeetingInput) ReceiveAny(ctx context.Context) (element.Envelope, string, error) {
	envelope, err := input.Receive(ctx)
	return envelope, input.Name(), err
}

func TestMeetingSessionAdapterDrainsPrequeuedCrossPortResponseInSourceOrder(t *testing.T) {
	sink := &orderedMeetingSink{done: make(chan struct{})}
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-prequeued-order", sink: sink,
		activeSpeech:  make(map[string]*meetingAdapterSpeech),
		completedRuns: make(map[string]struct{}),
	}
	runID := "prequeued-run"
	utteranceID := "foreground_segment:" + runID + ":speech:1"
	coordinator := &meetingAdapterResponseCoordinator{runs: make(map[string]*meetingAdapterCoordinatedRun)}
	accept := func(name, source string, payload any) {
		t.Helper()
		envelope := element.Envelope{
			ItemID: "coordinated-" + name, SessionID: adapter.sessionID,
			SourceID: source, RunID: runID, Payload: payload,
		}
		if err := adapter.acceptCoordinatedResponse(context.Background(), coordinator,
			meetingAdapterResponseEvent{name: name, envelope: envelope}); err != nil {
			t.Fatalf("accept %s: %v", name, err)
		}
	}

	// Every non-audio terminal overtakes the graph TTS boundary. None may close
	// the response, and safe text remains buffered until AudioBegin proves that
	// the independently synthesized utterance contains exactly those bytes.
	accept("foreground_outcome", ForegroundDeploymentReference, cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
		ProviderReference: ForegroundDeploymentReference,
	})
	accept("foreground_segmentation_outcome", ForegroundDeploymentReference,
		interactionelements.SegmentationOutcome{
			Kind: interactionelements.OutcomeIgnored, RunID: runID, Code: "already_terminal",
		})
	accept("foreground_safe_result", ForegroundDeploymentReference, cognitionelements.Result{
		RunID: runID, ProviderReference: ForegroundDeploymentReference,
		AssistantText: "Ordered.",
	})
	accept("foreground_segmentation_outcome", ForegroundDeploymentReference,
		interactionelements.SegmentationOutcome{
			Kind: interactionelements.OutcomeCompleted, RunID: runID, Segments: 1,
		})
	synthesis := speechelements.SynthesisOutcome{
		UtteranceID: utteranceID, Kind: speechelements.OutcomeSucceeded,
		Chunks: 1, AudioBytes: 4,
	}
	accept("foreground_synthesis_outcome", utteranceID, synthesis)
	for _, delta := range []cognitionelements.PreparedTextDelta{
		{Boundary: cognitionelements.TextBegin, Index: 0},
		{Boundary: cognitionelements.TextChunk, Index: 1, Text: "Ordered."},
		{Boundary: cognitionelements.TextEnd, Index: 2},
	} {
		accept("prepared_text", ForegroundDeploymentReference, delta)
	}
	if events, _, _ := sink.snapshot(); len(events) != 0 {
		t.Fatalf("graph terminals escaped before TTS audio: %v", events)
	}
	accept("prepared_audio", utteranceID, speechelements.AudioFrame{
		Kind: speechelements.AudioBegin, UtteranceID: utteranceID,
		Utterance: action.Utterance{ID: utteranceID, Text: "Ordered."},
	})
	accept("prepared_audio", utteranceID, speechelements.AudioFrame{
		Kind: speechelements.AudioChunk, UtteranceID: utteranceID,
		Chunk: v1.SpeechChunk{
			ChunkID: "coordinated-audio", CandidateID: utteranceID,
			SampleRateHz: 24_000, PCM16LE: []byte{9, 0, 8, 0}, Final: true,
		},
	})
	accept("prepared_audio", utteranceID, speechelements.AudioFrame{
		Kind: speechelements.AudioEnd, UtteranceID: utteranceID, Terminal: synthesis,
	})

	want := []string{"turn_begin", "speech_begin", "text:Ordered.", "speech_end", "turn_end"}
	if events, _, _ := sink.snapshot(); !slices.Equal(events, want) {
		t.Fatalf("coordinated response events = %v, want %v", events, want)
	}
	select {
	case <-sink.done:
	default:
		t.Fatal("coordinated graph TTS terminal did not close the turn")
	}
}

func TestMountedMeetingSessionAdapterOrdersTypedGraphBoundariesBeforeTerminal(t *testing.T) {
	types := map[string]element.Type{
		"prepared_audio":                  speechelements.AudioType(),
		"prepared_text":                   interactionelements.SafePreparedTextType(),
		"foreground_safe_result":          interactionelements.SafeModelResultType(),
		"foreground_segmentation_outcome": interactionelements.SegmentationOutcomeType(),
		"foreground_synthesis_outcome":    speechelements.SynthesisOutcomeType(),
		"foreground_outcome":              modelelements.OutcomeType(),
	}
	descriptor := flowelements.TeeDescriptor()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	graph := ir.Graph{FormatVersion: ir.FormatVersion, ID: "meeting-response-order-fixture", Revision: 1}
	for boundary, valueType := range types {
		nodeID := "relay_" + boundary
		lane := "boundary:" + boundary
		graph.Nodes = append(graph.Nodes, ir.Node{
			ID: nodeID, Element: identity,
			Ports: []ir.Port{
				{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One,
					Required: true, LossAllowed: true},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.Variadic,
					Required: true, MinConnections: 1, LossAllowed: true, Lanes: []string{lane}},
			},
			Reaction: descriptor.Reaction,
		})
		graph.Boundaries = append(graph.Boundaries,
			ir.Boundary{Name: "source_" + boundary, Direction: ir.InputBoundary,
				Endpoint: ir.Endpoint{Node: nodeID, Port: "in"}, Type: valueType},
			ir.Boundary{Name: boundary, Direction: ir.OutputBoundary,
				Endpoint: ir.Endpoint{Node: nodeID, Port: "out", Lane: lane}, Type: valueType},
		)
	}
	frozen, err := ir.Freeze(graph)
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := flowelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{Graph: frozen, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mountedDone := make(chan error, 1)
	go func() { mountedDone <- mounted.Run(ctx) }()

	runID := "mounted-response-run"
	utteranceID := "foreground_segment:" + runID + ":speech:1"
	utterance := action.Utterance{ID: utteranceID, Text: "Mounted order."}
	send := func(boundary string, sequence uint64, payload any) {
		port, portErr := mounted.Ingress("source_" + boundary)
		if portErr != nil {
			t.Fatal(portErr)
		}
		sourceID := ForegroundDeploymentReference
		if boundary == "prepared_audio" || boundary == "foreground_synthesis_outcome" {
			sourceID = utteranceID
		}
		result, sendErr := port.Broadcast(context.Background(), element.Envelope{
			Type: types[boundary], ItemID: fmt.Sprintf("mounted-response-%d", sequence),
			SessionID: "mounted-session", SourceID: sourceID,
			RunID: runID, Sequence: sequence, Payload: payload,
		})
		if sendErr != nil || result.Delivered != 1 || result.Dropped != 0 {
			t.Fatalf("send mounted %s = %+v, %v", boundary, result, sendErr)
		}
	}
	send("prepared_audio", 1, speechelements.AudioFrame{
		Kind: speechelements.AudioBegin, UtteranceID: utterance.ID, Utterance: utterance,
	})
	send("prepared_text", 2, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	})
	send("prepared_text", 3, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextChunk, Index: 1, Text: "Mounted order.",
	})
	send("prepared_text", 4, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: 2,
	})
	send("prepared_audio", 5, speechelements.AudioFrame{
		Kind: speechelements.AudioEnd, UtteranceID: utterance.ID,
		Terminal: speechelements.SynthesisOutcome{
			UtteranceID: utterance.ID, Kind: speechelements.OutcomeSucceeded,
		},
	})
	send("foreground_safe_result", 6, cognitionelements.Result{
		RunID: runID, ProviderReference: ForegroundDeploymentReference,
		AssistantText: "Mounted order.",
	})
	send("foreground_segmentation_outcome", 7, interactionelements.SegmentationOutcome{
		Kind: interactionelements.OutcomeCompleted, RunID: runID, Segments: 1,
	})
	send("foreground_segmentation_outcome", 8, interactionelements.SegmentationOutcome{
		Kind: interactionelements.OutcomeIgnored, RunID: runID, Code: "already_terminal",
	})
	send("foreground_synthesis_outcome", 9, speechelements.SynthesisOutcome{
		UtteranceID: utteranceID, Kind: speechelements.OutcomeSucceeded,
	})
	send("foreground_outcome", 10, cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
		ProviderReference: ForegroundDeploymentReference,
	})

	audio, err := mounted.Egress("prepared_audio")
	if err != nil {
		t.Fatal(err)
	}
	text, err := mounted.Egress("prepared_text")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := mounted.Egress("foreground_outcome")
	if err != nil {
		t.Fatal(err)
	}
	safeResult, err := mounted.Egress("foreground_safe_result")
	if err != nil {
		t.Fatal(err)
	}
	segmentation, err := mounted.Egress("foreground_segmentation_outcome")
	if err != nil {
		t.Fatal(err)
	}
	synthesis, err := mounted.Egress("foreground_synthesis_outcome")
	if err != nil {
		t.Fatal(err)
	}
	audioGate, textReceived, outcomeReceived := make(chan struct{}), make(chan struct{}), make(chan struct{})
	sink := &orderedMeetingSink{done: make(chan struct{})}
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "mounted-session", sink: sink,
		ports: meetingAdapterPorts{outputs: map[string]element.InputPort{
			"prepared_audio":                  &gatedMeetingInput{InputPort: audio, gate: audioGate},
			"prepared_text":                   &notifyingMeetingInput{InputPort: text, received: textReceived},
			"foreground_safe_result":          safeResult,
			"foreground_segmentation_outcome": segmentation,
			"foreground_synthesis_outcome":    synthesis,
			"foreground_outcome":              &notifyingMeetingInput{InputPort: outcome, received: outcomeReceived},
		}},
		activeSpeech: make(map[string]*meetingAdapterSpeech), completedRuns: make(map[string]struct{}),
	}
	adapterDone := make(chan error, 1)
	go func() { adapterDone <- adapter.Run(ctx) }()
	for name, received := range map[string]<-chan struct{}{"text": textReceived, "outcome": outcomeReceived} {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatalf("mounted %s boundary was not drained", name)
		}
	}
	if events, _, _ := sink.snapshot(); len(events) != 0 {
		t.Fatalf("mounted later boundary escaped before AudioBegin: %v", events)
	}
	close(audioGate)
	select {
	case <-sink.done:
	case <-time.After(2 * time.Second):
		t.Fatal("mounted ordered response did not terminalize")
	}
	want := []string{"turn_begin", "speech_begin", "text:Mounted order.", "speech_end", "turn_end"}
	if events, _, _ := sink.snapshot(); !slices.Equal(events, want) {
		t.Fatalf("mounted response events = %v, want %v", events, want)
	}
	cancel()
	if err := <-adapterDone; err != nil {
		t.Fatal(err)
	}
	if err := <-mountedDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMeetingSessionAdapterRejectsCompletedRunReplayWithoutOpeningAnotherResponse(t *testing.T) {
	sink := &orderedMeetingSink{}
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-run-tombstone", sink: sink,
		activeSpeech: make(map[string]*meetingAdapterSpeech), completedRuns: make(map[string]struct{}),
	}
	runID := "terminal-run"
	proposal := element.Envelope{RunID: runID, Payload: cognitionelements.ToolProposal{
		Call: trajectory.ToolCall{CallID: "first-call", Name: "computer.click_normalized"},
	}}
	if err := adapter.publishTool(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	terminal := element.Envelope{RunID: runID, Payload: cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
		ProviderReference: ForegroundDeploymentReference,
	}}
	if err := adapter.publishForegroundOutcome(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	if err := adapter.publishTool(context.Background(), proposal); err == nil {
		t.Fatal("completed run replay was admitted")
	}
	if err := adapter.publishForegroundOutcome(context.Background(), terminal); err == nil {
		t.Fatal("completed terminal replay was admitted")
	}
	events, _, _ := sink.snapshot()
	if want := []string{"turn_begin", "tool:first-call", "turn_end"}; !slices.Equal(events, want) {
		t.Fatalf("events after completed-run replay = %v, want %v", events, want)
	}
}

func TestMeetingSessionAdapterRequiresExactForegroundOutcomeIdentity(t *testing.T) {
	valid := cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: "envelope-run",
		ProviderReference: ForegroundDeploymentReference,
	}
	for _, test := range []struct {
		name     string
		envelope string
		change   func(*cognitionelements.Outcome)
	}{
		{name: "mismatched run", envelope: "envelope-run", change: func(value *cognitionelements.Outcome) {
			value.RunID = "payload-run"
		}},
		{name: "empty payload run", envelope: "envelope-run", change: func(value *cognitionelements.Outcome) {
			value.RunID = ""
		}},
		{name: "empty envelope run", envelope: "", change: func(*cognitionelements.Outcome) {}},
		{name: "noncanonical envelope run", envelope: " envelope-run", change: func(value *cognitionelements.Outcome) {
			value.RunID = " envelope-run"
		}},
		{name: "wrong operation", envelope: "envelope-run", change: func(value *cognitionelements.Outcome) {
			value.Operation = "summarize"
		}},
		{name: "wrong provider", envelope: "envelope-run", change: func(value *cognitionelements.Outcome) {
			value.ProviderReference = "meeting.foreground.unreviewed"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := &orderedMeetingSink{}
			adapter := &meetingSessionAdapter{
				ctx: context.Background(), sessionID: "meeting-outcome-identity", sink: sink,
				activeSpeech: make(map[string]*meetingAdapterSpeech), completedRuns: make(map[string]struct{}),
			}
			outcome := valid
			test.change(&outcome)
			err := adapter.publishForegroundOutcome(context.Background(), element.Envelope{
				RunID: test.envelope, Payload: outcome,
			})
			if err == nil {
				t.Fatal("non-exact foreground outcome identity was accepted")
			}
			if events, _, _ := sink.snapshot(); len(events) != 0 {
				t.Fatalf("non-exact outcome crossed response boundary: %v", events)
			}
		})
	}
}

func TestMeetingSessionAdapterGroupedOutcomeUsesAdmissionOrderAndPreservesReason(t *testing.T) {
	for _, test := range []struct {
		name      string
		first     cognitionelements.Outcome
		second    cognitionelements.Outcome
		want      legacy.TurnOutcome
		wantFails int
	}{
		{
			name: "completion inversion preserves first admitted incomplete",
			first: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeCanceled, Operation: "generate",
				ProviderReference: ForegroundDeploymentReference,
				Code:              legacy.TurnIncompleteTokens, Message: "token ceiling reached",
			},
			second: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeSucceeded, Operation: "generate",
				ProviderReference: ForegroundDeploymentReference,
			},
			want: legacy.TurnOutcome{
				Incomplete: true, Reason: legacy.TurnIncompleteTokens, Detail: "token ceiling reached",
			},
		},
		{
			name: "two incomplete outcomes retain admission precedence",
			first: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeCanceled, Operation: "generate",
				ProviderReference: ForegroundDeploymentReference,
				Code:              legacy.TurnIncompleteTokens, Message: "first incomplete",
			},
			second: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeRefused, Operation: "generate",
				ProviderReference: ForegroundDeploymentReference,
				Code:              "content_filter", Message: "second incomplete",
			},
			want: legacy.TurnOutcome{
				Incomplete: true, Reason: legacy.TurnIncompleteTokens, Detail: "first incomplete",
			},
			wantFails: 1,
		},
		{
			name: "failed then successful remains incomplete",
			first: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeFailed, Operation: "generate",
				ProviderReference: ForegroundDeploymentReference,
				Code:              "provider_failed", Message: "provider failed",
			},
			second: cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeSucceeded, Operation: "generate",
				ProviderReference: ForegroundDeploymentReference,
			},
			want:      legacy.TurnOutcome{Incomplete: true, Detail: "provider failed"},
			wantFails: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := &orderedMeetingSink{}
			adapter := &meetingSessionAdapter{
				ctx: context.Background(), sessionID: "meeting-grouped-outcome", sink: sink,
				activeSpeech: make(map[string]*meetingAdapterSpeech), completedRuns: make(map[string]struct{}),
			}
			for index, runID := range []string{"admitted-first", "admitted-second"} {
				if err := adapter.publishTool(context.Background(), element.Envelope{
					RunID: runID, Payload: cognitionelements.ToolProposal{Call: trajectory.ToolCall{
						CallID: fmt.Sprintf("call-%d", index), Name: "computer.click_normalized",
					}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			second := test.second
			second.RunID = "admitted-second"
			if err := adapter.publishForegroundOutcome(context.Background(), element.Envelope{
				RunID: second.RunID, Payload: second,
			}); err != nil {
				t.Fatal(err)
			}
			first := test.first
			first.RunID = "admitted-first"
			if err := adapter.publishForegroundOutcome(context.Background(), element.Envelope{
				RunID: first.RunID, Payload: first,
			}); err != nil {
				t.Fatal(err)
			}
			_, outcomes, failures := sink.snapshot()
			if len(outcomes) != 1 || outcomes[0] != test.want {
				t.Fatalf("grouped terminal = %+v, want %+v", outcomes, test.want)
			}
			if len(failures) != test.wantFails {
				t.Fatalf("failure events = %+v, want %d", failures, test.wantFails)
			}
		})
	}
}

func TestMeetingSessionAdapterBoundsResponseGroupAndSessionIdentities(t *testing.T) {
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-adapter-bounds", sink: &orderedMeetingSink{},
		activeSpeech: make(map[string]*meetingAdapterSpeech), completedRuns: make(map[string]struct{}),
	}
	for index := 0; index < maximumMeetingAdapterRuns; index++ {
		runID := fmt.Sprintf("bounded-run-%d", index)
		if err := adapter.ensureTurn(context.Background(), runID); err != nil {
			t.Fatalf("admit response run %d: %v", index, err)
		}
	}
	if err := adapter.ensureTurn(context.Background(), "bounded-run-overflow"); err == nil {
		t.Fatal("response group admitted a run beyond its bound")
	}

	limited := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-identity-bound", sink: &orderedMeetingSink{},
		activeSpeech:  make(map[string]*meetingAdapterSpeech),
		completedRuns: make(map[string]struct{}, maximumMeetingAdapterRunIDs),
	}
	for index := 0; index < maximumMeetingAdapterRunIDs; index++ {
		limited.completedRuns[fmt.Sprintf("completed-%d", index)] = struct{}{}
	}
	if err := limited.ensureTurn(context.Background(), "never-admitted"); err == nil {
		t.Fatal("session admitted a run after completed identity bound")
	}
}

func TestMeetingSessionAdapterCancelTargetsSharedResponseWhileVoiceAndVisualRunsOverlap(t *testing.T) {
	cancelPort := newTestOutput("cancel", modelelements.CancelType())
	releaseSpeech := make(chan struct{})
	close(releaseSpeech)
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-shared-cancel", sink: &blockedMeetingSpeechSink{
			beginEntered: make(chan struct{}, 1), releaseBegin: releaseSpeech,
		},
		ports:        meetingAdapterPorts{cancel: cancelPort},
		activeSpeech: make(map[string]*meetingAdapterSpeech), completedRuns: make(map[string]struct{}),
	}
	voiceRun, visualRun := "voice-draining-run", "visual-active-run"
	utterance := action.Utterance{ID: "cancel-speech", Text: "Still playing."}
	if err := adapter.publishAudio(context.Background(), element.Envelope{
		RunID: voiceRun, Payload: speechelements.AudioFrame{
			Kind: speechelements.AudioBegin, UtteranceID: utterance.ID, Utterance: utterance,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.publishTool(context.Background(), element.Envelope{
		RunID: visualRun, Payload: cognitionelements.ToolProposal{Call: trajectory.ToolCall{
			CallID: "visual-active-call", Name: "computer.click_normalized",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Cancel(context.Background(), "cancel the shared response"); err != nil {
		t.Fatal(err)
	}
	envelope := outputEnvelope(t, cancelPort)
	cancel, ok := envelope.Payload.(cognitionelements.Cancel)
	if !ok || cancel.RunID != voiceRun || envelope.RunID != voiceRun ||
		cancel.Reason != "cancel the shared response" {
		t.Fatalf("shared response cancellation = envelope %+v payload %+v", envelope, cancel)
	}
}

func TestMeetingSessionAdapterPartialCoordinatedRunCancellationAndSourceValidation(t *testing.T) {
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-response-gap", sink: &orderedMeetingSink{},
		activeSpeech: make(map[string]*meetingAdapterSpeech), completedRuns: make(map[string]struct{}),
	}
	events := make(chan meetingAdapterResponseEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- adapter.publishCoordinatedResponses(ctx, events) }()
	events <- meetingAdapterResponseEvent{name: "foreground_outcome", envelope: element.Envelope{
		Type: modelelements.OutcomeType(), ItemID: "gap-terminal", SessionID: adapter.sessionID,
		SourceID: ForegroundDeploymentReference, RunID: "gap-run", Sequence: 2,
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: "gap-run",
			ProviderReference: ForegroundDeploymentReference,
		},
	}}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("response gap did not unblock on session cancellation")
	}
	if observed, _, _ := adapter.sink.(*orderedMeetingSink).snapshot(); len(observed) != 0 {
		t.Fatalf("partial terminal crossed the sink: %v", observed)
	}

	coordinator := &meetingAdapterResponseCoordinator{runs: make(map[string]*meetingAdapterCoordinatedRun)}
	bad := meetingAdapterResponseEvent{name: "foreground_outcome", envelope: element.Envelope{
		Type: modelelements.OutcomeType(), ItemID: "foreign-source", SessionID: adapter.sessionID,
		SourceID: "untrusted.foreground", RunID: "foreign-run", Sequence: 1,
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: "foreign-run",
			ProviderReference: ForegroundDeploymentReference,
		},
	}}
	if err := adapter.acceptCoordinatedResponse(context.Background(), coordinator, bad); err == nil {
		t.Fatal("foreign response authority was accepted")
	}
	for index := 0; index < maximumMeetingAdapterRuns; index++ {
		coordinator.runs[fmt.Sprintf("occupied-%d", index)] = &meetingAdapterCoordinatedRun{}
	}
	bad.envelope.SourceID = ForegroundDeploymentReference
	if err := adapter.acceptCoordinatedResponse(context.Background(), coordinator, bad); err == nil {
		t.Fatal("response coordinator admitted a run beyond its bound")
	}
}
