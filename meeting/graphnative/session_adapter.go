package graphnative

import (
	"context"
	"errors"
	"fmt"
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
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	defaultMeetingVideoRateMilliHz = 5_000
	maximumMeetingAdapterTextBytes = 1 << 20
	maximumMeetingAdapterReason    = 1 << 10
)

// SessionAdapterConfig is the resource-free executable contribution for the
// stable Realtime-to-Meeting-graph boundary. Provider selection belongs to
// the independently registered graph dependencies; this adapter contains only
// protocol translation and the exact video cadence declared on model.External.
type SessionAdapterConfig struct {
	FrameRateMilliHz int
	Status           legacy.Status
}

// SessionAdapterFactory constructs the concrete Meeting Assistant adapter.
// It is suitable for AdapterPluginConfig.Factory and acquires no provider or
// listener resource until NativeBinding starts a session.
func SessionAdapterFactory(config SessionAdapterConfig) graphbinding.AdapterFactory {
	config.Status.Observers = slices.Clone(config.Status.Observers)
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
	config.Status.Observers = slices.Clone(plugin.config.Adapter.Status.Observers)
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
}

type meetingSessionAdapter struct {
	ctx       context.Context
	sessionID string
	sink      legacy.Sink
	profile   graphbinding.SessionAdapterProfile
	ports     meetingAdapterPorts
	status    legacy.Status
	frameRate int
	store     *trajectory.Store
	sequence  atomic.Uint64

	videoMu       sync.Mutex
	videoCaptured map[string]uint64
	turnMu        sync.Mutex
	activeTurn    string
	activeSpeech  map[string]*meetingAdapterSpeech
	closed        atomic.Bool
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
	status := config.Status
	status.Observers = slices.Clone(config.Status.Observers)
	return &meetingSessionAdapter{
		ctx: ctx, sessionID: sessionID, sink: options.Sink, profile: profile.Clone(), ports: ports,
		status: status, frameRate: config.FrameRateMilliHz, store: store,
		videoCaptured: make(map[string]uint64), activeSpeech: make(map[string]*meetingAdapterSpeech),
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
	runID := session.activeTurn
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

func (session *meetingSessionAdapter) Status() legacy.Status {
	status := session.status
	status.Observers = slices.Clone(session.status.Observers)
	return status
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
	failures := make(chan error, len(session.ports.outputs))
	var wait sync.WaitGroup
	for name, input := range session.ports.outputs {
		name, input := name, input
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := session.drain(runCtx, name, input); err != nil && context.Cause(runCtx) == nil {
				select {
				case failures <- err:
				case <-runCtx.Done():
				}
			}
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
		if err := session.publish(ctx, name, envelope); err != nil {
			return err
		}
	}
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
	if delta.Boundary != cognitionelements.TextChunk {
		return nil
	}
	utterance, err := session.speech(ctx, envelope.RunID)
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
	if runID == "" {
		runID = outcome.RunID
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
		terminal.Incomplete, terminal.Detail = true, outcome.Message
	case cognitionelements.OutcomeRefused, cognitionelements.OutcomeFailed:
		terminal.Incomplete, terminal.Detail = true, outcome.Message
		session.sink.Failed(ctx, legacy.ErrorEvent{Code: outcome.Code, Message: outcome.Message})
	default:
		return fmt.Errorf("meeting foreground outcome has unknown kind %q", outcome.Kind)
	}
	if err := session.sink.TurnEnd(ctx, terminal); err != nil {
		return err
	}
	session.turnMu.Lock()
	if session.activeTurn == runID {
		session.activeTurn = ""
	}
	session.turnMu.Unlock()
	return nil
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
		Category: "graph", Name: "meeting.background_injection", Phase: "slow",
		CorrelationID: outcome.RunID, Message: outcome.Message,
		Attributes: map[string]any{"kind": outcome.Kind, "bytes": outcome.Bytes, "code": outcome.Code},
	})
}

func (session *meetingSessionAdapter) ensureTurn(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("meeting graph output requires a run ID")
	}
	session.turnMu.Lock()
	if session.activeTurn == runID {
		session.turnMu.Unlock()
		return nil
	}
	if session.activeTurn != "" {
		active := session.activeTurn
		session.turnMu.Unlock()
		return fmt.Errorf("meeting graph opened run %q while %q is active", runID, active)
	}
	session.activeTurn = runID
	session.turnMu.Unlock()
	if err := session.sink.TurnBegin(ctx); err != nil {
		session.turnMu.Lock()
		if session.activeTurn == runID {
			session.activeTurn = ""
		}
		session.turnMu.Unlock()
		return err
	}
	return nil
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
