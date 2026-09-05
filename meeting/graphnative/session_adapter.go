package graphnative

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	defaultMeetingVideoRateMilliHz = 5_000
	maximumMeetingAdapterTextBytes = 1 << 20
	maximumMeetingAdapterReason    = 1 << 10
	maximumMeetingAdapterRuns      = 64
	maximumMeetingAdapterRunIDs    = 65_536
	maximumMeetingResponseEvents   = 1 << 20
	maximumMeetingResponsePending  = 512
	maximumMeetingCoordinatedAudio = 64 << 20
)

// SessionAdapterConfig is the resource-free executable contribution for the
// stable Realtime-to-Meeting-graph boundary. Provider selection belongs to
// the independently registered graph dependencies; this adapter contains only
// protocol translation and the exact video cadence declared on model.External.
type SessionAdapterConfig struct {
	FrameRateMilliHz int
}

// SessionAdapterFactory constructs the concrete Meeting Assistant adapter.
// It is suitable for AdapterPluginConfig.Factory and acquires no provider or
// listener resource until NativeBinding starts a session.
func SessionAdapterFactory(config SessionAdapterConfig) graphbinding.AdapterFactory {
	if config.FrameRateMilliHz == 0 {
		config.FrameRateMilliHz = defaultMeetingVideoRateMilliHz
	}
	return func(
		ctx context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		profile graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		return newMeetingSessionAdapter(ctx, mounted, options, profile, config, trajectory.NewStore())
	}
}

func (plugin *SessionPlugin) sessionAdapterFactory() graphbinding.AdapterFactory {
	config := plugin.config.Adapter
	if config.FrameRateMilliHz == 0 {
		config.FrameRateMilliHz = defaultMeetingVideoRateMilliHz
	}
	return func(
		ctx context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		profile graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		store, err := plugin.coordinator.take(options.SessionID)
		if err != nil {
			return nil, err
		}
		return newMeetingSessionAdapter(ctx, mounted, options, profile, config, store)
	}
}

type meetingAdapterPorts struct {
	update, audio, video, text, toolResult element.OutputPort
	commit, create, cancel, truncate       element.OutputPort
	outputs                                map[string]element.InputPort
}

type meetingAdapterSpeech struct {
	utterance action.Utterance
	ready     chan struct{}
	beginErr  error
	done      chan struct{}
	textOpen  bool
	textEnd   bool
	textNext  uint64
}

// meetingAdapterTurn is one open Realtime response group. A graph may finish
// cognition for one run and admit a silent visual-action run while the first
// run's speech is still draining. The graph run IDs remain independently
// addressable, but every overlapping run contributes output items to the same
// wire response and the response closes only after all of them are terminal.
type meetingAdapterTurn struct {
	ready     chan struct{}
	done      chan struct{}
	beginErr  error
	closing   bool
	runs      map[string]struct{}
	completed map[string]legacy.TurnOutcome
	order     []string
}

type meetingAdapterResponseEvent struct {
	name     string
	envelope element.Envelope
}

type meetingAdapterCoordinatedRun struct {
	textBegun  bool
	textEnded  bool
	textNext   uint64
	text       strings.Builder
	textFrames []element.Envelope

	audioBegun       bool
	audioEnded       bool
	audioUtteranceID string
	audioFrames      []element.Envelope
	audioBytes       int
	introPublished   bool
	audioPublished   int

	tools          []element.Envelope
	toolsPublished bool
	toolsRejected  bool
	safeResult     *cognitionelements.Result

	segmentation            *interactionelements.SegmentationOutcome
	segmentationSourceSeen  bool
	segmentationSuccessSeen bool
	synthesis               *speechelements.SynthesisOutcome
	foreground              *element.Envelope
}

type meetingAdapterResponseCoordinator struct {
	runs map[string]*meetingAdapterCoordinatedRun
}

type meetingSessionAdapter struct {
	ctx       context.Context
	sessionID string
	sink      legacy.Sink
	profile   graphbinding.SessionAdapterProfile
	ports     meetingAdapterPorts
	frameRate int
	store     *trajectory.Store
	sequence  atomic.Uint64

	videoMu       sync.Mutex
	videoCaptured map[string]uint64
	turnMu        sync.Mutex
	turn          *meetingAdapterTurn
	activeSpeech  map[string]*meetingAdapterSpeech
	completedRuns map[string]struct{}
	// segmentationCancellationCutoffs are exact run tombstones established
	// before an internal SegmentPreparedText model-cancel request is re-entered
	// at the graph's cancel ingress. A positive cutoff is the trusted,
	// session-scoped foreground response sequence that caused cancellation;
	// zero means no source-order proof exists (for example, a timeout).
	segmentationCancellationCutoffs map[string]uint64
	closed                          atomic.Bool
}

func newMeetingSessionAdapter(
	ctx context.Context, mounted *graphruntime.Mounted, options legacy.Options,
	profile graphbinding.SessionAdapterProfile, config SessionAdapterConfig, store *trajectory.Store,
) (*meetingSessionAdapter, error) {
	if ctx == nil || mounted == nil || options.Sink == nil || store == nil {
		return nil, errors.New("create meeting session adapter: context, mounted graph, sink, and trajectory store are required")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := profile.ValidateGraph(mounted.Graph()); err != nil {
		return nil, fmt.Errorf("create meeting session adapter profile: %w", err)
	}
	if mounted.Graph().ID != GraphID {
		return nil, fmt.Errorf("create meeting session adapter: graph ID %q, want %q", mounted.Graph().ID, GraphID)
	}
	if config.FrameRateMilliHz <= 0 || config.FrameRateMilliHz > 1_000_000 {
		return nil, errors.New("create meeting session adapter: video frame rate must be between 1 and 1000000 millihertz")
	}
	ports, err := bindMeetingAdapterPorts(mounted, profile)
	if err != nil {
		return nil, err
	}
	sessionID := strings.TrimSpace(options.SessionID)
	if sessionID == "" {
		return nil, errors.New("create meeting session adapter: canonical session ID is required")
	}
	return &meetingSessionAdapter{
		ctx: ctx, sessionID: sessionID, sink: options.Sink, profile: profile.Clone(), ports: ports,
		frameRate: config.FrameRateMilliHz, store: store,
		videoCaptured: make(map[string]uint64), activeSpeech: make(map[string]*meetingAdapterSpeech),
		completedRuns: make(map[string]struct{}), segmentationCancellationCutoffs: make(map[string]uint64),
	}, nil
}

func bindMeetingAdapterPorts(
	mounted *graphruntime.Mounted, profile graphbinding.SessionAdapterProfile,
) (meetingAdapterPorts, error) {
	result := meetingAdapterPorts{outputs: make(map[string]element.InputPort)}
	inputs := map[graphbinding.AdapterOperation]*element.OutputPort{
		graphbinding.AdapterInputUpdate:         &result.update,
		graphbinding.AdapterInputAudio:          &result.audio,
		graphbinding.AdapterInputVideo:          &result.video,
		graphbinding.AdapterInputText:           &result.text,
		graphbinding.AdapterInputToolResult:     &result.toolResult,
		graphbinding.AdapterInputCommitAudio:    &result.commit,
		graphbinding.AdapterInputCreateResponse: &result.create,
		graphbinding.AdapterInputCancel:         &result.cancel,
		graphbinding.AdapterInputTruncate:       &result.truncate,
	}
	for _, mapping := range profile.Boundaries {
		if target := inputs[mapping.Operation]; target != nil {
			port, err := mounted.Ingress(mapping.Boundary)
			if err != nil {
				return meetingAdapterPorts{}, fmt.Errorf("bind meeting adapter input %s: %w", mapping.Operation, err)
			}
			*target = port
			continue
		}
		port, err := mounted.Egress(mapping.Boundary)
		if err != nil {
			return meetingAdapterPorts{}, fmt.Errorf("bind meeting adapter output %s: %w", mapping.Operation, err)
		}
		result.outputs[mapping.Boundary] = port
	}
	for operation, target := range inputs {
		if *target == nil {
			return meetingAdapterPorts{}, fmt.Errorf("bind meeting adapter: profile omits required operation %s", operation)
		}
	}
	for _, boundary := range []string{
		"foreground_safe_result", "foreground_model_cancel_request", "foreground_segmentation_outcome",
		"foreground_synthesis_outcome",
	} {
		port, err := mounted.Egress(boundary)
		if err != nil {
			return meetingAdapterPorts{}, fmt.Errorf("bind meeting adapter internal output %s: %w", boundary, err)
		}
		result.outputs[boundary] = port
	}
	return result, nil
}

// foregroundSessionSettings is the exact gateway update carried over the
// graph's tool.Catalog state boundary. The graph owns only routing; the
// external foreground deployment owns validation of its selected voice stack.
type foregroundSessionSettings struct {
	Settings legacy.Settings `json:"settings"`
}

func (session *meetingSessionAdapter) Update(ctx context.Context, settings legacy.Settings) error {
	if err := session.usable(ctx, "update meeting session"); err != nil {
		return err
	}
	settings = legacy.CloneSettings(settings)
	return session.send(ctx, session.ports.update, "update", "", foregroundSessionSettings{Settings: settings})
}

func (session *meetingSessionAdapter) Audio(ctx context.Context, frame perception.Frame) error {
	if err := session.usable(ctx, "send meeting audio"); err != nil {
		return err
	}
	if err := frame.Validate(); err != nil || frame.Kind != perception.FrameAudio {
		if err == nil {
			err = errors.New("frame is not audio")
		}
		return fmt.Errorf("send meeting audio: %w", err)
	}
	streamID := canonicalMediaStream(frame.Source, "microphone")
	return session.sendCaptured(ctx, session.ports.audio, "audio", streamID, frame.CapturedNS,
		acousticelements.InputFrame{StreamID: streamID, Frame: cloneMeetingFrame(frame)})
}

func (session *meetingSessionAdapter) Video(ctx context.Context, frame perception.Frame) error {
	if err := session.usable(ctx, "send meeting video"); err != nil {
		return err
	}
	if err := frame.Validate(); err != nil || frame.Kind != perception.FrameImage {
		if err == nil {
			err = errors.New("frame is not an image")
		}
		return fmt.Errorf("send meeting video: %w", err)
	}
	streamID := canonicalMediaStream(frame.Source, "screen")
	frameRate := session.frameRate
	session.videoMu.Lock()
	previous := session.videoCaptured[streamID]
	if previous != 0 {
		if frame.CapturedNS <= previous {
			session.videoMu.Unlock()
			return errors.New("send meeting video: capture timestamps must strictly increase per stream")
		}
		delta := frame.CapturedNS - previous
		if delta != 0 {
			measured := int(1_000_000_000_000 / delta)
			if measured > 0 && measured <= 1_000_000 {
				frameRate = measured
			}
		}
	}
	session.videoCaptured[streamID] = frame.CapturedNS
	session.videoMu.Unlock()
	return session.sendCaptured(ctx, session.ports.video, "video", streamID, frame.CapturedNS,
		modelelements.VideoInputFrame{
			StreamID: streamID, Frame: cloneMeetingFrame(frame), FrameRateMilliHz: frameRate,
		})
}

func (session *meetingSessionAdapter) Text(ctx context.Context, input legacy.TextInput) error {
	if err := session.usable(ctx, "send meeting text"); err != nil {
		return err
	}
	if len(input.Images) != 0 {
		return errors.New("send meeting text: still-image attachments use the composable video input")
	}
	if strings.TrimSpace(input.ItemID) == "" || strings.TrimSpace(input.Text) == "" ||
		!utf8.ValidString(input.Text) || len(input.Text) > maximumMeetingAdapterTextBytes {
		return errors.New("send meeting text: canonical item ID and bounded UTF-8 text are required")
	}
	role := strings.TrimSpace(input.Role)
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "system" {
		return fmt.Errorf("send meeting text: unsupported role %q", role)
	}
	payload := ContextInjection{Role: role, Source: "gateway.text", RunID: input.ItemID, Text: input.Text}
	return session.sendWithID(ctx, session.ports.text, input.ItemID, input.ItemID, payload)
}

func (session *meetingSessionAdapter) ToolResult(ctx context.Context, result trajectory.ToolResult) error {
	if err := session.usable(ctx, "send meeting tool result"); err != nil {
		return err
	}
	result.Output = slices.Clone(result.Output)
	if strings.TrimSpace(result.CallID) == "" || strings.TrimSpace(result.Name) == "" {
		return errors.New("send meeting tool result: call ID and name are required")
	}
	return session.send(ctx, session.ports.toolResult, "tool_result", result.CallID, result)
}

func (session *meetingSessionAdapter) CommitAudio(ctx context.Context) error {
	if err := session.usable(ctx, "commit meeting audio"); err != nil {
		return err
	}
	return session.send(ctx, session.ports.commit, "commit_audio", "", acousticelements.AudioCommit{
		Reason: "client committed input audio",
	})
}

func (session *meetingSessionAdapter) CreateResponse(ctx context.Context) error {
	if err := session.usable(ctx, "create meeting response"); err != nil {
		return err
	}
	return session.send(ctx, session.ports.create, "create_response", "", cognitionelements.Generate{
		Invocation: session.defaultInvocation(),
	})
}

func (session *meetingSessionAdapter) Cancel(ctx context.Context, reason string) error {
	if err := session.usable(ctx, "cancel meeting response"); err != nil {
		return err
	}
	reason = boundedMeetingReason(reason)
	session.turnMu.Lock()
	runID := session.activeTurnLocked()
	session.turnMu.Unlock()
	return session.send(ctx, session.ports.cancel, "cancel", runID, cognitionelements.Cancel{
		RunID: runID, Reason: reason,
	})
}

func (session *meetingSessionAdapter) Truncate(ctx context.Context, truncation legacy.Truncation) error {
	if err := session.usable(ctx, "truncate meeting response"); err != nil {
		return err
	}
	if strings.TrimSpace(truncation.ItemID) == "" || truncation.AudioEndMS < 0 {
		return errors.New("truncate meeting response: item ID and non-negative endpoint are required")
	}
	return session.send(ctx, session.ports.truncate, "truncate", truncation.ItemID, truncation)
}

func (session *meetingSessionAdapter) Trajectory() trajectory.Snapshot {
	if session == nil || session.store == nil {
		return trajectory.Snapshot{}
	}
	return session.store.Snapshot()
}

func (session *meetingSessionAdapter) Close(context.Context, error) error {
	session.closed.Store(true)
	return nil
}

func (session *meetingSessionAdapter) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run meeting session adapter: nil context")
	}
	if len(session.ports.outputs) == 0 {
		return errors.New("run meeting session adapter: no graph outputs")
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	failures := make(chan error, len(session.ports.outputs)+1)
	responseEvents := make(chan meetingAdapterResponseEvent, maximumMeetingResponsePending)
	var wait sync.WaitGroup
	reportFailure := func(err error) {
		if err == nil || context.Cause(runCtx) != nil {
			return
		}
		select {
		case failures <- err:
		case <-runCtx.Done():
		}
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		reportFailure(session.publishCoordinatedResponses(runCtx, responseEvents))
	}()
	for name, input := range session.ports.outputs {
		name, input := name, input
		wait.Add(1)
		go func() {
			defer wait.Done()
			reportFailure(session.drain(runCtx, name, input, responseEvents))
		}()
	}
	var runErr error
	select {
	case <-runCtx.Done():
	case runErr = <-failures:
		cancel(runErr)
	}
	wait.Wait()
	return runErr
}

func (session *meetingSessionAdapter) drain(
	ctx context.Context, name string, input element.InputPort,
	responseEvents chan<- meetingAdapterResponseEvent,
) error {
	for {
		envelope, err := input.Receive(ctx)
		if err != nil {
			if context.Cause(ctx) != nil || errors.Is(err, graphruntime.ErrChannelClosed) {
				return nil
			}
			return fmt.Errorf("receive meeting graph output %s: %w", name, err)
		}
		if envelope.SessionID != "" && envelope.SessionID != session.sessionID {
			return fmt.Errorf("meeting graph output %s crossed session boundary", name)
		}
		if name == "foreground_model_cancel_request" {
			if err := session.relayForegroundModelCancel(ctx, envelope); err != nil {
				return err
			}
			continue
		}
		if meetingResponseBoundary(name) {
			select {
			case responseEvents <- meetingAdapterResponseEvent{name: name, envelope: envelope.Clone()}:
			case <-ctx.Done():
				return nil
			}
			continue
		}
		if err := session.publish(ctx, name, envelope); err != nil {
			return err
		}
	}
}

func meetingResponseBoundary(name string) bool {
	switch name {
	case "prepared_text", "prepared_audio", "tool_proposals", "foreground_safe_result",
		"foreground_segmentation_outcome", "foreground_synthesis_outcome", "foreground_outcome":
		return true
	default:
		return false
	}
}

func (session *meetingSessionAdapter) segmentationCancellationRejects(
	name string, envelope element.Envelope,
) bool {
	if name != "tool_proposals" && name != "foreground_safe_result" {
		return false
	}
	session.turnMu.Lock()
	defer session.turnMu.Unlock()
	cutoff, found := session.segmentationCancellationCutoffs[envelope.RunID]
	if !found {
		return false
	}
	// A safe result is produced only after the foreground source terminal and
	// can never precede a segmentation cancellation. Tool proposals are emitted
	// on an independent graph edge, so only their trusted, positive response
	// sequence can prove that they were already buffered before the positive
	// sequence which caused cancellation.
	return name != "tool_proposals" || cutoff == 0 || cutoff > maximumMeetingResponseEvents ||
		envelope.Sequence == 0 || envelope.Sequence >= cutoff
}

// relayForegroundModelCancel is the explicit causal break between
// SegmentPreparedText and model.External. Connecting those nodes directly
// creates foreground.text_out -> quarantine -> segment -> foreground.cancel
// feedback, which the graph compiler correctly refuses. Crossing the mounted
// adapter boundary makes the asynchronous handoff inspectable while retaining
// every bit of the segmenter's run address and causal provenance.
func (session *meetingSessionAdapter) relayForegroundModelCancel(
	ctx context.Context, envelope element.Envelope,
) error {
	cancel, ok := meetingCognitionCancel(envelope.Payload)
	runID := strings.TrimSpace(envelope.RunID)
	if !ok || !canonicalText(runID) || len(runID) > sidecar.MaxElementIdentifierBytes ||
		cancel.RunID != runID || strings.TrimSpace(envelope.CancellationScope) != runID {
		return fmt.Errorf("meeting foreground model cancellation has inexact run address %q / %q / %q",
			envelope.RunID, cancel.RunID, envelope.CancellationScope)
	}
	if envelope.SessionID != session.sessionID || !strings.HasSuffix(envelope.ItemID, ":model-cancel") {
		return errors.New("meeting foreground model cancellation lacks exact session or item provenance")
	}
	if session.ports.cancel == nil || !envelope.Type.Equal(session.ports.cancel.Type()) {
		return errors.New("meeting foreground model cancellation does not match the cancel ingress type")
	}

	session.turnMu.Lock()
	if session.segmentationCancellationCutoffs == nil {
		session.segmentationCancellationCutoffs = make(map[string]uint64)
	}
	if _, duplicate := session.segmentationCancellationCutoffs[runID]; duplicate {
		session.turnMu.Unlock()
		return fmt.Errorf("meeting segmentation requested model cancellation twice for run %q", runID)
	}
	if len(session.segmentationCancellationCutoffs) >= maximumMeetingAdapterRunIDs {
		session.turnMu.Unlock()
		return errors.New("meeting segmentation cancellation identity limit reached")
	}
	cutoff := uint64(0)
	if envelope.SourceID == ForegroundDeploymentReference &&
		envelope.Sequence <= maximumMeetingResponseEvents {
		cutoff = envelope.Sequence
	}
	session.segmentationCancellationCutoffs[runID] = cutoff
	session.turnMu.Unlock()

	relayed := envelope.Clone()
	relayed.Payload = cancel
	delivery, err := session.ports.cancel.Broadcast(ctx, relayed)
	if err != nil {
		return fmt.Errorf("relay Meeting foreground model cancellation: %w", err)
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return fmt.Errorf("Meeting foreground model cancellation delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func (session *meetingSessionAdapter) publishCoordinatedResponses(
	ctx context.Context, events <-chan meetingAdapterResponseEvent,
) error {
	coordinator := meetingAdapterResponseCoordinator{
		runs: make(map[string]*meetingAdapterCoordinatedRun),
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			if err := session.acceptCoordinatedResponse(ctx, &coordinator, event); err != nil {
				return err
			}
		}
	}
}

func (session *meetingSessionAdapter) acceptCoordinatedResponse(
	ctx context.Context, coordinator *meetingAdapterResponseCoordinator,
	event meetingAdapterResponseEvent,
) error {
	if coordinator == nil || coordinator.runs == nil {
		return errors.New("meeting response coordinator is not initialized")
	}
	if !meetingResponseBoundary(event.name) {
		return fmt.Errorf("meeting response coordinator received non-response boundary %q", event.name)
	}
	envelope := event.envelope
	runID := strings.TrimSpace(envelope.RunID)
	if !canonicalText(runID) || len(runID) > sidecar.MaxElementIdentifierBytes {
		return fmt.Errorf("meeting response boundary %s has a non-canonical run ID", event.name)
	}
	if session.segmentationCancellationRejects(event.name, envelope) {
		return fmt.Errorf("meeting foreground emitted %s for run %q after segmentation cancellation",
			event.name, runID)
	}
	session.turnMu.Lock()
	_, completed := session.completedRuns[runID]
	session.turnMu.Unlock()
	if completed {
		return session.acceptLateCoordinatedAudit(event)
	}
	run := coordinator.runs[runID]
	created := false
	if run == nil {
		if len(coordinator.runs) >= maximumMeetingAdapterRuns {
			return errors.New("meeting response coordinator run limit reached")
		}
		run = &meetingAdapterCoordinatedRun{}
		coordinator.runs[runID] = run
		created = true
	}
	if err := session.recordCoordinatedResponse(run, event); err != nil {
		if created {
			delete(coordinator.runs, runID)
		}
		return err
	}
	finished, err := session.flushCoordinatedResponse(ctx, runID, run)
	if err != nil {
		return err
	}
	if finished {
		delete(coordinator.runs, runID)
	}
	return nil
}

func (session *meetingSessionAdapter) recordCoordinatedResponse(
	run *meetingAdapterCoordinatedRun, event meetingAdapterResponseEvent,
) error {
	if run == nil {
		return errors.New("meeting response coordinator has no run state")
	}
	envelope := event.envelope.Clone()
	foregroundSource := func() error {
		if envelope.SourceID != ForegroundDeploymentReference {
			return fmt.Errorf("meeting response boundary %s has source %q, want %q",
				event.name, envelope.SourceID, ForegroundDeploymentReference)
		}
		return nil
	}
	switch event.name {
	case "prepared_text":
		if err := foregroundSource(); err != nil {
			return err
		}
		delta, ok := meetingPreparedText(envelope.Payload)
		if !ok {
			return fmt.Errorf("meeting prepared text has payload %T", envelope.Payload)
		}
		switch delta.Boundary {
		case cognitionelements.TextBegin:
			if run.textBegun || run.textEnded || delta.Index != 0 || delta.Text != "" || delta.Interrupted {
				return errors.New("meeting safe text begin must be empty, uninterrupted, and index zero")
			}
			run.textBegun, run.textNext = true, 1
		case cognitionelements.TextChunk:
			if !run.textBegun || run.textEnded || delta.Index != run.textNext || delta.Interrupted {
				return errors.New("meeting safe text chunk is missing its exact active predecessor")
			}
			run.textNext++
		case cognitionelements.TextEnd:
			if !run.textBegun || run.textEnded || delta.Index != run.textNext {
				return errors.New("meeting safe text end is missing its exact active predecessor")
			}
			run.textNext++
			run.textEnded = true
		default:
			return fmt.Errorf("meeting safe text has unknown boundary %q", delta.Boundary)
		}
		if run.text.Len()+len(delta.Text) > maximumMeetingAdapterTextBytes {
			return fmt.Errorf("meeting safe text exceeds %d bytes", maximumMeetingAdapterTextBytes)
		}
		run.text.WriteString(delta.Text)
		envelope.Payload = delta
		run.textFrames = append(run.textFrames, envelope)
	case "prepared_audio":
		frame, ok := meetingPreparedAudio(envelope.Payload)
		if !ok {
			return fmt.Errorf("meeting prepared audio has payload %T", envelope.Payload)
		}
		utteranceID := strings.TrimSpace(frame.UtteranceID)
		if !canonicalText(utteranceID) || envelope.SourceID != utteranceID {
			return errors.New("meeting graph TTS audio lacks its exact utterance source")
		}
		wantUtteranceID := "foreground_segment:" + envelope.RunID + ":speech:1"
		if utteranceID != wantUtteranceID {
			return fmt.Errorf("meeting graph TTS utterance %q, want %q", utteranceID, wantUtteranceID)
		}
		switch frame.Kind {
		case speechelements.AudioBegin:
			if run.audioBegun || run.audioEnded || frame.Utterance.ID != utteranceID ||
				strings.TrimSpace(frame.Utterance.Text) == "" {
				return errors.New("meeting graph TTS emitted an invalid or duplicate audio begin")
			}
			run.audioBegun, run.audioUtteranceID = true, utteranceID
		case speechelements.AudioChunk:
			if !run.audioBegun || run.audioEnded || utteranceID != run.audioUtteranceID {
				return errors.New("meeting graph TTS emitted audio outside its exact utterance")
			}
			if err := frame.Chunk.Validate(); err != nil || frame.Chunk.CandidateID != utteranceID {
				if err == nil {
					err = errors.New("audio candidate does not match the graph utterance")
				}
				return fmt.Errorf("meeting graph TTS chunk: %w", err)
			}
			if len(frame.Chunk.PCM16LE) > maximumMeetingCoordinatedAudio-run.audioBytes {
				return fmt.Errorf("meeting graph TTS audio exceeds %d buffered bytes", maximumMeetingCoordinatedAudio)
			}
			run.audioBytes += len(frame.Chunk.PCM16LE)
			frame.Chunk.PCM16LE = slices.Clone(frame.Chunk.PCM16LE)
		case speechelements.AudioEnd:
			if !run.audioBegun || run.audioEnded || utteranceID != run.audioUtteranceID ||
				frame.Terminal.UtteranceID != utteranceID {
				return errors.New("meeting graph TTS emitted an invalid audio terminal")
			}
			run.audioEnded = true
		default:
			return fmt.Errorf("meeting graph TTS audio has unknown kind %q", frame.Kind)
		}
		envelope.Payload = frame
		run.audioFrames = append(run.audioFrames, envelope)
	case "tool_proposals":
		if err := foregroundSource(); err != nil {
			return err
		}
		proposal, ok := meetingToolProposal(envelope.Payload)
		if !ok {
			return fmt.Errorf("meeting tool proposal has payload %T", envelope.Payload)
		}
		if len(run.tools) >= maximumMeetingResponseEvents {
			return errors.New("meeting response tool proposal limit reached")
		}
		proposal.Call.Arguments = slices.Clone(proposal.Call.Arguments)
		envelope.Payload = proposal
		run.tools = append(run.tools, envelope)
	case "foreground_safe_result":
		if err := foregroundSource(); err != nil {
			return err
		}
		result, ok := meetingSafeResult(envelope.Payload)
		if !ok {
			return fmt.Errorf("meeting safe result has payload %T", envelope.Payload)
		}
		if run.safeResult != nil || result.RunID != envelope.RunID {
			return errors.New("meeting safe result is duplicated or names a different run")
		}
		copy := result
		copy.Outputs = clonePreparedOutputs(result.Outputs)
		copy.ToolProposals = cloneForegroundProposals(result.ToolProposals)
		run.safeResult = &copy
	case "foreground_segmentation_outcome":
		outcome, ok := meetingSegmentationOutcome(envelope.Payload)
		if !ok || outcome.RunID != envelope.RunID {
			return fmt.Errorf("meeting segmentation outcome has invalid payload %T", envelope.Payload)
		}
		if outcome.Kind == interactionelements.OutcomeIgnored {
			if outcome.Code != "stream_end_authoritative" && outcome.Code != "already_terminal" {
				return fmt.Errorf("meeting segmentation ignored source terminal with code %q", outcome.Code)
			}
			run.segmentationSourceSeen = true
			if outcome.Code == "stream_end_authoritative" {
				run.segmentationSuccessSeen = true
			}
			break
		}
		switch outcome.Kind {
		case interactionelements.OutcomeCompleted, interactionelements.OutcomeCanceled,
			interactionelements.OutcomeFailed, interactionelements.OutcomeRefused:
		default:
			return fmt.Errorf("meeting segmentation emitted unexpected outcome %q", outcome.Kind)
		}
		if run.segmentation != nil {
			return errors.New("meeting segmentation emitted two terminal outcomes")
		}
		copy := outcome
		run.segmentation = &copy
		if outcome.Kind != interactionelements.OutcomeCompleted {
			run.segmentationSourceSeen = true
		}
	case "foreground_synthesis_outcome":
		outcome, ok := meetingSynthesisOutcome(envelope.Payload)
		if !ok || !canonicalText(outcome.UtteranceID) || envelope.SourceID != outcome.UtteranceID {
			return fmt.Errorf("meeting synthesis outcome has invalid payload %T", envelope.Payload)
		}
		if outcome.Kind == speechelements.OutcomeIgnored {
			break
		}
		wantUtteranceID := "foreground_segment:" + envelope.RunID + ":speech:1"
		if run.synthesis != nil || outcome.UtteranceID != wantUtteranceID ||
			(run.audioUtteranceID != "" && outcome.UtteranceID != run.audioUtteranceID) {
			return errors.New("meeting synthesis terminal is duplicate or names a different utterance")
		}
		copy := outcome
		run.synthesis = &copy
	case "foreground_outcome":
		if err := foregroundSource(); err != nil {
			return err
		}
		outcome, ok := meetingCognitionOutcome(envelope.Payload)
		if !ok || outcome.RunID != envelope.RunID || outcome.Operation != "generate" ||
			outcome.ProviderReference != ForegroundDeploymentReference {
			return fmt.Errorf("meeting foreground terminal has invalid payload %T", envelope.Payload)
		}
		if run.foreground != nil {
			return errors.New("meeting foreground emitted two terminal outcomes")
		}
		copy := envelope.Clone()
		copy.Payload = outcome
		run.foreground = &copy
	default:
		return fmt.Errorf("meeting response coordinator received undeclared boundary %q", event.name)
	}
	return nil
}

func (session *meetingSessionAdapter) flushCoordinatedResponse(
	ctx context.Context, runID string, run *meetingAdapterCoordinatedRun,
) (bool, error) {
	if run.textEnded && run.audioBegun && !run.introPublished {
		begin, ok := meetingPreparedAudio(run.audioFrames[0].Payload)
		if !ok || strings.TrimSpace(run.text.String()) != begin.Utterance.Text {
			return false, errors.New("meeting graph TTS utterance text differs from quarantined SafePreparedText")
		}
		if err := session.publishAudio(ctx, run.audioFrames[0]); err != nil {
			return false, err
		}
		for _, text := range run.textFrames {
			if err := session.publishText(ctx, text); err != nil {
				return false, err
			}
		}
		run.introPublished = true
		run.audioPublished = 1
	}
	if run.introPublished {
		for run.audioPublished < len(run.audioFrames) {
			if err := session.publishAudio(ctx, run.audioFrames[run.audioPublished]); err != nil {
				return false, err
			}
			run.audioPublished++
		}
	}

	zeroSpeech := meetingSafeResultProvesNoSpeechWithoutTextStream(run)
	if zeroSpeech && (run.audioBegun || run.audioEnded || len(run.audioFrames) != 0 || run.synthesis != nil) {
		return false, errors.New("meeting zero-speech safe result nevertheless produced graph TTS output")
	}
	segmentationSucceeded := run.segmentation != nil &&
		run.segmentation.Kind == interactionelements.OutcomeCompleted
	if run.segmentation == nil && zeroSpeech && run.segmentationSuccessSeen {
		segmentationSucceeded = true
	}
	segmentationFailed := run.segmentation != nil &&
		run.segmentation.Kind != interactionelements.OutcomeCompleted
	if segmentationFailed {
		// A failed speech-safety boundary invalidates the entire response. Tool
		// proposals remain quarantined even if they raced ahead on their
		// independent foreground edge before model cancellation was relayed.
		run.toolsRejected = true
	}
	resultReady := run.safeResult != nil && (run.textEnded || zeroSpeech)
	if resultReady {
		if run.safeResult.AssistantText != run.text.String() {
			return false, errors.New("meeting safe result differs from quarantined SafePreparedText")
		}
		if len(run.tools) > len(run.safeResult.ToolProposals) {
			return false, errors.New("meeting emitted more tool proposals than its safe result records")
		}
		for index, tool := range run.tools {
			proposal, _ := meetingToolProposal(tool.Payload)
			if !reflect.DeepEqual(proposal, run.safeResult.ToolProposals[index]) {
				return false, fmt.Errorf("meeting tool proposal %d differs from its safe result", index)
			}
		}
		if len(run.tools) == len(run.safeResult.ToolProposals) && segmentationSucceeded &&
			!run.toolsPublished && !run.toolsRejected {
			for _, tool := range run.tools {
				if err := session.publishTool(ctx, tool); err != nil {
					return false, err
				}
			}
			run.toolsPublished = true
		}
	}

	if run.foreground == nil {
		return false, nil
	}
	foreground, _ := meetingCognitionOutcome(run.foreground.Payload)
	if run.segmentation == nil {
		// A tool-only foreground run opens no prepared-text stream. In that one
		// case SegmentPreparedText can only acknowledge that the successful
		// source terminal must not overtake a stream; it has no active stream to
		// close and therefore emits no separate completed outcome. The
		// quarantined safe result is the authoritative proof that there is no
		// assistant text to synthesize.
		if foreground.Kind != cognitionelements.OutcomeSucceeded || !zeroSpeech ||
			!run.segmentationSuccessSeen {
			return false, nil
		}
	} else if !run.segmentationSourceSeen {
		return false, nil
	}
	if foreground.Kind == cognitionelements.OutcomeSucceeded {
		if !segmentationFailed && (!resultReady || !run.toolsPublished) {
			return false, nil
		}
	} else if !run.toolsPublished && !run.toolsRejected {
		if run.safeResult != nil && !resultReady {
			return false, nil
		}
		if !segmentationSucceeded {
			return false, nil
		}
		for _, tool := range run.tools {
			if err := session.publishTool(ctx, tool); err != nil {
				return false, err
			}
		}
		run.toolsPublished = true
	}

	internalCode, internalMessage := "", ""
	if run.segmentation == nil {
		// The zero-speech proof above is the terminal segmentation state.
	} else {
		switch run.segmentation.Kind {
		case interactionelements.OutcomeCompleted:
			safeText := strings.TrimSpace(run.text.String())
			switch run.segmentation.Segments {
			case 0:
				if safeText != "" || run.audioBegun || run.synthesis != nil {
					return false, errors.New("meeting empty segmentation produced text or graph TTS output")
				}
			case 1:
				if safeText == "" || !run.audioEnded || run.synthesis == nil ||
					!run.introPublished || run.audioPublished != len(run.audioFrames) {
					return false, nil
				}
				end, ok := meetingPreparedAudio(run.audioFrames[len(run.audioFrames)-1].Payload)
				if !ok || end.Kind != speechelements.AudioEnd ||
					!reflect.DeepEqual(end.Terminal, *run.synthesis) {
					return false, errors.New("meeting graph TTS audio and synthesis terminals differ")
				}
				if run.synthesis.Kind != speechelements.OutcomeSucceeded {
					internalCode = firstNonemptyMeeting(run.synthesis.Code, "synthesis_failed")
					internalMessage = firstNonemptyMeeting(run.synthesis.Message, "graph TTS did not complete")
				}
			default:
				return false, fmt.Errorf("meeting segmentation produced %d speech segments, want at most one",
					run.segmentation.Segments)
			}
		default:
			if run.audioBegun || run.synthesis != nil {
				return false, errors.New("meeting failed segmentation nevertheless produced graph TTS output")
			}
			internalCode = firstNonemptyMeeting(run.segmentation.Code, "segmentation_failed")
			internalMessage = firstNonemptyMeeting(run.segmentation.Message, "safe speech segmentation failed")
		}
	}

	terminal := run.foreground.Clone()
	if foreground.Kind == cognitionelements.OutcomeSucceeded && internalCode != "" {
		foreground.Kind = cognitionelements.OutcomeFailed
		foreground.Code, foreground.Message = internalCode, internalMessage
		terminal.Payload = foreground
	}
	if err := session.publishForegroundOutcome(ctx, terminal); err != nil {
		return false, err
	}
	return true, nil
}

func meetingSafeResultProvesNoSpeechWithoutTextStream(run *meetingAdapterCoordinatedRun) bool {
	if run == nil || run.safeResult == nil || run.safeResult.AssistantText != "" ||
		run.textBegun || run.textEnded || len(run.textFrames) != 0 {
		return false
	}
	for _, output := range run.safeResult.Outputs {
		if output.Kind == cognitionelements.PreparedAssistant {
			return false
		}
	}
	return true
}

func (session *meetingSessionAdapter) acceptLateCoordinatedAudit(
	event meetingAdapterResponseEvent,
) error {
	switch event.name {
	case "foreground_segmentation_outcome":
		outcome, ok := meetingSegmentationOutcome(event.envelope.Payload)
		if ok && outcome.Kind == interactionelements.OutcomeIgnored {
			return nil
		}
	case "foreground_synthesis_outcome":
		outcome, ok := meetingSynthesisOutcome(event.envelope.Payload)
		if ok && outcome.Kind == speechelements.OutcomeIgnored {
			return nil
		}
	}
	return fmt.Errorf("meeting graph emitted %s after run %q completed", event.name, event.envelope.RunID)
}

func firstNonemptyMeeting(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (session *meetingSessionAdapter) publish(
	ctx context.Context, name string, envelope element.Envelope,
) error {
	switch name {
	case "activity":
		activity, ok := meetingActivity(envelope.Payload)
		if !ok {
			return fmt.Errorf("meeting activity output has payload %T", envelope.Payload)
		}
		return session.sink.Activity(ctx, activity)
	case "transcript":
		observation, ok := meetingObservation(envelope.Payload)
		if !ok {
			return fmt.Errorf("meeting transcript output has payload %T", envelope.Payload)
		}
		return session.sink.Transcript(ctx, legacy.TranscriptEvent{
			ItemID: envelope.ItemID, Text: observation.Text, Final: observation.Final,
		})
	case "observations":
		observation, ok := meetingObservation(envelope.Payload)
		if !ok {
			return fmt.Errorf("meeting observation output has payload %T", envelope.Payload)
		}
		return session.sink.Observation(ctx, observation)
	case "prepared_text":
		return session.publishText(ctx, envelope)
	case "prepared_audio":
		return session.publishAudio(ctx, envelope)
	case "tool_proposals":
		return session.publishTool(ctx, envelope)
	case "foreground_outcome":
		return session.publishForegroundOutcome(ctx, envelope)
	case "background_outcome":
		return session.publishBackgroundOutcome(ctx, envelope)
	default:
		return fmt.Errorf("meeting adapter received undeclared graph output %q", name)
	}
}

func (session *meetingSessionAdapter) publishText(
	ctx context.Context, envelope element.Envelope,
) error {
	delta, ok := meetingPreparedText(envelope.Payload)
	if !ok {
		return fmt.Errorf("meeting prepared text has payload %T", envelope.Payload)
	}
	runID := envelope.RunID
	session.turnMu.Lock()
	state := session.activeSpeech[runID]
	if state == nil {
		session.turnMu.Unlock()
		return errors.New("meeting prepared text arrived outside an active utterance")
	}
	switch delta.Boundary {
	case cognitionelements.TextBegin:
		if state.textOpen || state.textEnd || delta.Index != 0 || delta.Text != "" || delta.Interrupted {
			session.turnMu.Unlock()
			return errors.New("meeting safe text begin must be empty, uninterrupted, and index zero")
		}
		state.textOpen = true
		state.textNext = 1
		session.turnMu.Unlock()
		return nil
	case cognitionelements.TextChunk:
		if !state.textOpen || state.textEnd || delta.Index != state.textNext || delta.Interrupted {
			session.turnMu.Unlock()
			return errors.New("meeting safe text chunk is missing its exact active predecessor")
		}
		state.textNext++
	case cognitionelements.TextEnd:
		if !state.textOpen || state.textEnd || delta.Index != state.textNext {
			session.turnMu.Unlock()
			return errors.New("meeting safe text end is missing its exact active predecessor")
		}
		state.textNext++
		state.textEnd = true
	default:
		session.turnMu.Unlock()
		return fmt.Errorf("meeting safe text has unknown boundary %q", delta.Boundary)
	}
	session.turnMu.Unlock()
	if delta.Text == "" {
		return nil
	}
	utterance, err := session.speech(ctx, runID)
	if err != nil {
		return fmt.Errorf("meeting prepared text arrived outside an active utterance: %w", err)
	}
	return session.sink.SpeechText(ctx, utterance, delta.Text)
}

func (session *meetingSessionAdapter) publishAudio(
	ctx context.Context, envelope element.Envelope,
) error {
	frame, ok := meetingPreparedAudio(envelope.Payload)
	if !ok {
		return fmt.Errorf("meeting prepared audio has payload %T", envelope.Payload)
	}
	runID := envelope.RunID
	if runID == "" {
		runID = frame.UtteranceID
	}
	switch frame.Kind {
	case speechelements.AudioBegin:
		if err := session.ensureTurn(ctx, runID); err != nil {
			return err
		}
		utterance := frame.Utterance
		if strings.TrimSpace(utterance.ID) == "" {
			utterance.ID = frame.UtteranceID
		}
		state := &meetingAdapterSpeech{
			utterance: utterance, ready: make(chan struct{}), done: make(chan struct{}),
		}
		session.turnMu.Lock()
		if _, duplicate := session.activeSpeech[runID]; duplicate {
			session.turnMu.Unlock()
			return errors.New("meeting graph opened the same utterance twice")
		}
		session.activeSpeech[runID] = state
		session.turnMu.Unlock()
		state.beginErr = session.sink.SpeechBegin(ctx, utterance)
		close(state.ready)
		return state.beginErr
	case speechelements.AudioChunk:
		utterance, err := session.speech(ctx, runID)
		if err != nil {
			return fmt.Errorf("meeting graph emitted audio outside an active utterance: %w", err)
		}
		duration := time.Duration(0)
		if frame.Chunk.SampleRateHz > 0 {
			duration = time.Duration(len(frame.Chunk.PCM16LE)/2) * time.Second /
				time.Duration(frame.Chunk.SampleRateHz)
		}
		return session.sink.SpeechAudio(ctx, utterance, action.Frame{
			PCM16LE: slices.Clone(frame.Chunk.PCM16LE), SampleRateHz: frame.Chunk.SampleRateHz,
			Duration: duration, Final: frame.Chunk.Final,
		})
	case speechelements.AudioEnd:
		session.turnMu.Lock()
		textIncomplete := session.activeSpeech[runID] != nil &&
			session.activeSpeech[runID].textOpen && !session.activeSpeech[runID].textEnd
		session.turnMu.Unlock()
		if textIncomplete {
			return errors.New("meeting graph ended audio before its safe text terminal")
		}
		state, found := session.takeSpeech(runID)
		if !found {
			return errors.New("meeting graph ended unknown utterance")
		}
		completed := frame.Terminal.Kind == speechelements.OutcomeSucceeded
		err := session.sink.SpeechEnd(ctx, state.utterance, action.Outcome{
			Completed: completed, Reason: frame.Terminal.Message,
		})
		session.finishSpeech(runID, state)
		return err
	default:
		return fmt.Errorf("meeting prepared audio has unknown kind %q", frame.Kind)
	}
}

func (session *meetingSessionAdapter) publishTool(
	ctx context.Context, envelope element.Envelope,
) error {
	proposal, ok := meetingToolProposal(envelope.Payload)
	if !ok {
		return fmt.Errorf("meeting tool proposal has payload %T", envelope.Payload)
	}
	if err := session.ensureTurn(ctx, envelope.RunID); err != nil {
		return err
	}
	return session.sink.ToolCalls(ctx, legacy.ToolCallEvent{
		InvocationID: envelope.RunID, Calls: []trajectory.ToolCall{proposal.Call},
	})
}

func (session *meetingSessionAdapter) publishForegroundOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	outcome, ok := meetingCognitionOutcome(envelope.Payload)
	if !ok {
		return fmt.Errorf("meeting foreground outcome has payload %T", envelope.Payload)
	}
	runID := envelope.RunID
	if !canonicalText(runID) || outcome.RunID != runID {
		return fmt.Errorf("meeting foreground outcome run ID %q does not match envelope %q",
			outcome.RunID, runID)
	}
	if outcome.Operation != "generate" {
		return fmt.Errorf("meeting foreground outcome operation %q is not generate", outcome.Operation)
	}
	if outcome.ProviderReference != ForegroundDeploymentReference {
		return fmt.Errorf("meeting foreground outcome provider %q does not match %q",
			outcome.ProviderReference, ForegroundDeploymentReference)
	}
	if err := session.ensureTurn(ctx, runID); err != nil {
		return err
	}
	session.turnMu.Lock()
	speech := session.activeSpeech[runID]
	session.turnMu.Unlock()
	if speech != nil {
		select {
		case <-speech.done:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	terminal := legacy.TurnOutcome{}
	switch outcome.Kind {
	case cognitionelements.OutcomeSucceeded, cognitionelements.OutcomeIgnored:
	case cognitionelements.OutcomeCanceled:
		terminal.Incomplete, terminal.Reason, terminal.Detail =
			true, meetingIncompleteReason(outcome.Code), outcome.Message
	case cognitionelements.OutcomeRefused, cognitionelements.OutcomeFailed:
		terminal.Incomplete, terminal.Reason, terminal.Detail =
			true, meetingIncompleteReason(outcome.Code), outcome.Message
		session.sink.Failed(ctx, legacy.ErrorEvent{Code: outcome.Code, Message: outcome.Message})
	default:
		return fmt.Errorf("meeting foreground outcome has unknown kind %q", outcome.Kind)
	}
	return session.finishTurn(ctx, runID, terminal)
}

func meetingIncompleteReason(code string) string {
	switch strings.TrimSpace(code) {
	case legacy.TurnIncompleteTokens:
		return legacy.TurnIncompleteTokens
	case "content_filter":
		return "content_filter"
	default:
		return ""
	}
}

func (session *meetingSessionAdapter) publishBackgroundOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	outcome, ok := meetingBackgroundOutcome(envelope.Payload)
	if !ok {
		return fmt.Errorf("meeting background outcome has payload %T", envelope.Payload)
	}
	debug, ok := session.sink.(legacy.DebugSink)
	if !ok {
		return nil
	}
	return debug.Debug(ctx, legacy.DebugEvent{
		Category: string(openrealtime.DebugGraph), Name: "meeting.background_injection", Phase: "slow",
		CorrelationID: outcome.RunID, Message: outcome.Message,
		Attributes: map[string]any{"kind": outcome.Kind, "bytes": outcome.Bytes, "code": outcome.Code},
	})
}

func (session *meetingSessionAdapter) ensureTurn(ctx context.Context, runID string) error {
	if !canonicalText(runID) || len(runID) > sidecar.MaxElementIdentifierBytes {
		return errors.New("meeting graph output requires a run ID")
	}
	for {
		session.turnMu.Lock()
		if session.completedRuns == nil {
			session.completedRuns = make(map[string]struct{})
		}
		if _, completed := session.completedRuns[runID]; completed {
			session.turnMu.Unlock()
			return fmt.Errorf("meeting graph reused completed run %q", runID)
		}
		turn := session.turn
		if turn == nil {
			if len(session.completedRuns) >= maximumMeetingAdapterRunIDs {
				session.turnMu.Unlock()
				return errors.New("meeting adapter completed-run identity limit reached")
			}
			turn = &meetingAdapterTurn{
				ready: make(chan struct{}), done: make(chan struct{}),
				runs:      map[string]struct{}{runID: {}},
				completed: make(map[string]legacy.TurnOutcome), order: []string{runID},
			}
			session.turn = turn
			session.turnMu.Unlock()

			err := session.sink.TurnBegin(ctx)
			session.turnMu.Lock()
			turn.beginErr = err
			close(turn.ready)
			if err != nil {
				if session.turn == turn {
					session.turn = nil
				}
				close(turn.done)
			}
			session.turnMu.Unlock()
			return err
		}
		if _, completed := turn.completed[runID]; completed {
			session.turnMu.Unlock()
			return fmt.Errorf("meeting graph emitted output after run %q ended", runID)
		}
		if turn.closing {
			done := turn.done
			session.turnMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		if _, active := turn.runs[runID]; active {
			ready := turn.ready
			session.turnMu.Unlock()
			select {
			case <-ready:
				return turn.beginErr
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		ready := turn.ready
		session.turnMu.Unlock()
		select {
		case <-ready:
			if turn.beginErr != nil {
				return turn.beginErr
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		}

		session.turnMu.Lock()
		if session.turn != turn || turn.closing {
			session.turnMu.Unlock()
			continue
		}
		if _, completed := turn.completed[runID]; completed {
			session.turnMu.Unlock()
			return fmt.Errorf("meeting graph emitted output after run %q ended", runID)
		}
		if _, active := turn.runs[runID]; !active {
			if len(turn.order) >= maximumMeetingAdapterRuns {
				session.turnMu.Unlock()
				return errors.New("meeting adapter response run limit reached")
			}
			if len(session.completedRuns)+len(turn.order) >= maximumMeetingAdapterRunIDs {
				session.turnMu.Unlock()
				return errors.New("meeting adapter run identity limit reached")
			}
			turn.runs[runID] = struct{}{}
			turn.order = append(turn.order, runID)
		}
		session.turnMu.Unlock()
		return nil
	}
}

func (session *meetingSessionAdapter) finishTurn(
	ctx context.Context, runID string, outcome legacy.TurnOutcome,
) error {
	session.turnMu.Lock()
	turn := session.turn
	if turn == nil {
		session.turnMu.Unlock()
		return fmt.Errorf("meeting graph ended inactive run %q", runID)
	}
	if _, active := turn.runs[runID]; !active {
		session.turnMu.Unlock()
		return fmt.Errorf("meeting graph ended inactive run %q", runID)
	}
	delete(turn.runs, runID)
	turn.completed[runID] = outcome
	if len(turn.runs) != 0 {
		session.turnMu.Unlock()
		return nil
	}
	turn.closing = true
	terminal := legacy.TurnOutcome{}
	for _, completedRunID := range turn.order {
		candidate := turn.completed[completedRunID]
		if candidate.Incomplete {
			terminal = candidate
			break
		}
	}
	if session.completedRuns == nil {
		session.completedRuns = make(map[string]struct{}, len(turn.completed))
	}
	for completedRunID := range turn.completed {
		session.completedRuns[completedRunID] = struct{}{}
	}
	session.turnMu.Unlock()

	err := session.sink.TurnEnd(ctx, terminal)
	session.turnMu.Lock()
	if session.turn == turn {
		session.turn = nil
	}
	close(turn.done)
	session.turnMu.Unlock()
	return err
}

func (session *meetingSessionAdapter) activeTurnLocked() string {
	if session.turn == nil || session.turn.closing {
		return ""
	}
	for _, runID := range session.turn.order {
		if _, active := session.turn.runs[runID]; active {
			return runID
		}
	}
	return ""
}

func (session *meetingSessionAdapter) speech(
	ctx context.Context, runID string,
) (action.Utterance, error) {
	session.turnMu.Lock()
	state := session.activeSpeech[runID]
	session.turnMu.Unlock()
	if state == nil {
		return action.Utterance{}, errors.New("unknown utterance")
	}
	select {
	case <-state.ready:
	case <-ctx.Done():
		return action.Utterance{}, context.Cause(ctx)
	}
	if state.beginErr != nil {
		return action.Utterance{}, state.beginErr
	}
	return state.utterance, nil
}

func (session *meetingSessionAdapter) takeSpeech(runID string) (*meetingAdapterSpeech, bool) {
	session.turnMu.Lock()
	defer session.turnMu.Unlock()
	state, found := session.activeSpeech[runID]
	return state, found
}

func (session *meetingSessionAdapter) finishSpeech(runID string, state *meetingAdapterSpeech) {
	session.turnMu.Lock()
	if state != nil && session.activeSpeech[runID] == state {
		close(state.done)
		delete(session.activeSpeech, runID)
	}
	session.turnMu.Unlock()
}

func (session *meetingSessionAdapter) send(
	ctx context.Context, port element.OutputPort, kind, runID string, payload any,
) error {
	sequence := session.sequence.Add(1)
	kind = strings.ReplaceAll(strings.TrimSpace(kind), "_", "-")
	itemID := fmt.Sprintf("meeting-%s-%d", kind, sequence)
	return session.sendEnvelope(ctx, port, itemID, runID, sequence, 0, payload)
}

func (session *meetingSessionAdapter) sendCaptured(
	ctx context.Context, port element.OutputPort, kind, runID string, capturedNS uint64, payload any,
) error {
	sequence := session.sequence.Add(1)
	kind = strings.ReplaceAll(strings.TrimSpace(kind), "_", "-")
	itemID := fmt.Sprintf("meeting-%s-%d", kind, sequence)
	return session.sendEnvelope(ctx, port, itemID, runID, sequence, capturedNS, payload)
}

func (session *meetingSessionAdapter) sendWithID(
	ctx context.Context, port element.OutputPort, itemID, runID string, payload any,
) error {
	sequence := session.sequence.Add(1)
	return session.sendEnvelope(ctx, port, itemID, runID, sequence, 0, payload)
}

func (session *meetingSessionAdapter) sendEnvelope(
	ctx context.Context, port element.OutputPort, itemID, runID string, sequence, capturedNS uint64,
	payload any,
) error {
	envelope := element.Envelope{
		Type: port.Type(), ItemID: itemID, SessionID: session.sessionID,
		SourceID: "gateway", OpportunityID: itemID, RunID: runID, Sequence: sequence,
		CaptureNS: capturedNS, TraceID: itemID, CancellationScope: session.sessionID,
		Payload: payload,
	}
	delivery, err := port.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return fmt.Errorf("meeting adapter input %s delivered %d and dropped %d lanes",
			port.Name(), delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func (session *meetingSessionAdapter) defaultInvocation() continuation.Invocation {
	return continuation.Invocation{
		Instruction:     "Continue the current meeting turn.",
		MaxOutputTokens: session.profile.Capabilities.MaxOutputTokens,
	}
}

func (session *meetingSessionAdapter) usable(ctx context.Context, operation string) error {
	if ctx == nil {
		return fmt.Errorf("%s: nil context", operation)
	}
	if session.closed.Load() {
		return fmt.Errorf("%s: adapter is closed", operation)
	}
	return context.Cause(ctx)
}

func canonicalMediaStream(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
		return fallback
	}
	return value
}

func cloneMeetingFrame(frame perception.Frame) perception.Frame {
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	frame.Image = slices.Clone(frame.Image)
	return frame
}

func boundedMeetingReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" || !utf8.ValidString(reason) {
		return "client canceled response"
	}
	if len(reason) <= maximumMeetingAdapterReason {
		return reason
	}
	reason = reason[:maximumMeetingAdapterReason]
	for !utf8.ValidString(reason) {
		reason = reason[:len(reason)-1]
	}
	return reason
}

func meetingActivity(payload any) (legacy.ActivityEvent, bool) {
	switch value := payload.(type) {
	case legacy.ActivityEvent:
		return value, true
	case *legacy.ActivityEvent:
		if value != nil {
			return *value, true
		}
	}
	return legacy.ActivityEvent{}, false
}

func meetingObservation(payload any) (perception.Observation, bool) {
	switch value := payload.(type) {
	case perception.Observation:
		return value, true
	case *perception.Observation:
		if value != nil {
			return *value, true
		}
	}
	return perception.Observation{}, false
}

func meetingPreparedText(payload any) (cognitionelements.PreparedTextDelta, bool) {
	switch value := payload.(type) {
	case cognitionelements.PreparedTextDelta:
		return value, true
	case *cognitionelements.PreparedTextDelta:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.PreparedTextDelta{}, false
}

func meetingPreparedAudio(payload any) (speechelements.AudioFrame, bool) {
	switch value := payload.(type) {
	case speechelements.AudioFrame:
		return value, true
	case *speechelements.AudioFrame:
		if value != nil {
			return *value, true
		}
	}
	return speechelements.AudioFrame{}, false
}

func meetingToolProposal(payload any) (cognitionelements.ToolProposal, bool) {
	switch value := payload.(type) {
	case cognitionelements.ToolProposal:
		return value, true
	case *cognitionelements.ToolProposal:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.ToolProposal{}, false
}

func meetingCognitionCancel(payload any) (cognitionelements.Cancel, bool) {
	switch value := payload.(type) {
	case cognitionelements.Cancel:
		return value, true
	case *cognitionelements.Cancel:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Cancel{}, false
}

func meetingSafeResult(payload any) (cognitionelements.Result, bool) {
	switch value := payload.(type) {
	case cognitionelements.Result:
		return value, true
	case *cognitionelements.Result:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Result{}, false
}

func meetingSegmentationOutcome(payload any) (interactionelements.SegmentationOutcome, bool) {
	switch value := payload.(type) {
	case interactionelements.SegmentationOutcome:
		return value, true
	case *interactionelements.SegmentationOutcome:
		if value != nil {
			return *value, true
		}
	}
	return interactionelements.SegmentationOutcome{}, false
}

func meetingSynthesisOutcome(payload any) (speechelements.SynthesisOutcome, bool) {
	switch value := payload.(type) {
	case speechelements.SynthesisOutcome:
		return value, true
	case *speechelements.SynthesisOutcome:
		if value != nil {
			return *value, true
		}
	}
	return speechelements.SynthesisOutcome{}, false
}

func meetingCognitionOutcome(payload any) (cognitionelements.Outcome, bool) {
	switch value := payload.(type) {
	case cognitionelements.Outcome:
		return value, true
	case *cognitionelements.Outcome:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Outcome{}, false
}

func meetingBackgroundOutcome(payload any) (BackgroundInjectionOutcome, bool) {
	switch value := payload.(type) {
	case BackgroundInjectionOutcome:
		return value, true
	case *BackgroundInjectionOutcome:
		if value != nil {
			return *value, true
		}
	}
	return BackgroundInjectionOutcome{}, false
}

var _ graphbinding.SessionAdapter = (*meetingSessionAdapter)(nil)
