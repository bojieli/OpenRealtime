package graphnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	maximumForegroundFrames          = 512
	maximumForegroundPendingTriggers = 64
	maximumForegroundPendingInputs   = 512
	maximumForegroundTranscriptKeys  = 4096
	maximumForegroundDrainingRuns    = 64
	maximumForegroundIdentities      = 65_536
	foregroundCloseTimeout           = 10 * time.Second
	foregroundContextWait            = 5 * time.Second
	foregroundTurnWait               = 30 * time.Second
)

var expectedForegroundPorts = map[string]element.Direction{
	"audio": element.Input, "video": element.Input, "context": element.Input,
	"text": element.Input, "tools": element.Input, "tool_result": element.Input,
	"trigger": element.Input, "commit": element.Input, "cancel": element.Input,
	"truncate":   element.Input,
	"transcript": element.Output, "text_out": element.Output, "audio_out": element.Output,
	"result": element.Output, "tool_proposal": element.Output,
	"interaction_act": element.Output, "state": element.Output,
	"activity": element.Output, "outcome": element.Output,
}

type pendingForegroundTrigger struct {
	message sidecar.Message
	payload cognitionelements.Generate
}

type pendingForegroundInput struct {
	message sidecar.Message
	payload any
}

type foregroundRun struct {
	id                 string
	invocation         continuation.Invocation
	context            trajectory.Snapshot
	parentIDs          []string
	startedNS          uint64
	outputs            []cognitionelements.PreparedOutput
	assistant          strings.Builder
	proposals          []cognitionelements.ToolProposal
	completion         continuation.Completion
	failed             *legacy.ErrorEvent
	utterances         map[string]*foregroundUtterance
	turnEnded          bool
	turnOutcome        legacy.TurnOutcome
	contextReady       bool
	contextErr         error
	requiredTranscript string
	requiredEventID    string
}

type foregroundUtterance struct {
	value        action.Utterance
	sampleOffset uint64
	textOpened   bool
	audioOpened  bool
	textIndex    uint64
	done         bool
}

type foregroundSession struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	plugin   ForegroundPlugin
	options  legacy.Options
	ready    sidecar.Message
	contract *sidecar.ElementSessionContract
	codec    modelelements.PayloadCodec
	frames   chan sidecar.Message
	runtime  legacy.Runtime
	sequence atomic.Uint64
	// responseMu serializes every response-bearing callback through the exact
	// order in which it is published to model.External. responseSequence is a
	// session-scoped, gap-free stream carried by Envelope.Sequence across the
	// otherwise independently queued typed output ports. The Realtime adapter
	// uses it to reconstruct Begin -> content -> End -> terminal without a
	// scheduler-dependent cross-port race.
	responseMu         sync.Mutex
	responseSequence   uint64
	responseCallbackMu sync.Mutex
	closeOnce          sync.Once
	closeErr           error
	// Cascade may finish cognition for one turn while its action plane is still
	// synthesizing that turn's reserved utterance. A later visual or transcript
	// trigger must be allowed to start then: blocking it behind playback defeats
	// concurrent-I/O and misses deadline-sensitive Meeting actions. The adapter
	// still serializes overlapping cognition spans, and routes asynchronous
	// speech by the globally unique utterance ID to the exact draining graph run.
	turnAdmission chan struct{}

	sendMu  sync.Mutex
	seen    map[string]struct{}
	pending []pendingForegroundTrigger
	// model.External forwards each selected input port independently. Hold
	// ordinary inputs until the gateway's initial settings frame has actually
	// reached the provider runtime; otherwise audio can overtake session.update.
	configured   bool
	initialQueue []pendingForegroundInput

	mu                  sync.Mutex
	settings            legacy.Settings
	contextSnapshot     trajectory.Snapshot
	contextChanged      chan struct{}
	pendingRunIDs       []pendingForegroundTrigger
	active              *foregroundRun
	draining            map[string]*foregroundRun
	utteranceRuns       map[string]*foregroundRun
	seenRunIDs          map[string]struct{}
	seenUtteranceIDs    map[string]struct{}
	runChanged          chan struct{}
	transcriptRevision  uint64
	transcriptByItem    map[string]uint64
	lastFinalTranscript string
	lastFinalRevision   uint64
	lastFinalEventID    string
	boundFinalRevision  uint64
	err                 error
}

func (plugin *SessionPlugin) newForegroundSession(
	ctx context.Context, hello sidecar.Message, options legacy.Options,
) (modelelements.Session, error) {
	if plugin == nil {
		return nil, errors.New("create meeting foreground session: nil plugin")
	}
	if ctx == nil {
		return nil, errors.New("create meeting foreground session: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := validateForegroundHello(hello); err != nil {
		return nil, fmt.Errorf("create meeting foreground session: %w", err)
	}
	ready, contract, err := foregroundReady(plugin.config.Foreground, hello)
	if err != nil {
		return nil, fmt.Errorf("create meeting foreground readiness: %w", err)
	}
	sessionCtx, cancel := context.WithCancelCause(ctx)
	session := &foregroundSession{
		ctx: sessionCtx, cancel: cancel, plugin: plugin.config.Foreground,
		options: cloneLegacyOptions(options), ready: ready, contract: contract,
		codec: modelelements.NewStandardJSONCodec(), frames: make(chan sidecar.Message, maximumForegroundFrames),
		seen: make(map[string]struct{}), settings: legacy.CloneSettings(options.Settings),
		contextChanged: make(chan struct{}), runChanged: make(chan struct{}),
		turnAdmission: make(chan struct{}, 1), transcriptByItem: make(map[string]uint64),
		draining: make(map[string]*foregroundRun), utteranceRuns: make(map[string]*foregroundRun),
		seenRunIDs: make(map[string]struct{}), seenUtteranceIDs: make(map[string]struct{}),
	}
	session.turnAdmission <- struct{}{}
	binding, err := plugin.config.Foreground.Factory(sessionCtx, cloneLegacyOptions(options))
	if err != nil {
		cancel(err)
		return nil, fmt.Errorf("create meeting foreground binding: %w", err)
	}
	if err := validateForegroundBinding(plugin.config.Foreground, binding); err != nil {
		cancel(err)
		return nil, err
	}
	runtimeOptions := cloneLegacyOptions(options)
	runtimeOptions.Settings = meetingForegroundTextOnlySettings(runtimeOptions.Settings)
	runtimeOptions.Sink = session
	runtime, err := binding.Start(sessionCtx, runtimeOptions)
	if err != nil {
		cancel(err)
		return nil, fmt.Errorf("start meeting foreground binding: %w", err)
	}
	if reflectedMeetingNil(runtime) {
		cancel(errors.New("meeting foreground binding returned nil runtime"))
		return nil, errors.New("start meeting foreground binding: nil runtime")
	}
	session.runtime = runtime
	return session, nil
}

func validateForegroundBinding(plugin ForegroundPlugin, binding legacy.Binding) error {
	if reflectedMeetingNil(binding) {
		return errors.New("meeting foreground factory returned nil binding")
	}
	if binding.Name() != plugin.BindingName {
		return fmt.Errorf("meeting foreground binding name %q, want %q", binding.Name(), plugin.BindingName)
	}
	if !reflect.DeepEqual(binding.Ownership(), plugin.Ownership) {
		return errors.New("meeting foreground binding ownership drifted")
	}
	actual := binding.Capabilities()
	actual.Observers = slices.Clone(actual.Observers)
	expected := plugin.Capabilities
	expected.Observers = slices.Clone(expected.Observers)
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("meeting foreground binding capabilities drifted")
	}
	return nil
}

func validateForegroundHello(hello sidecar.Message) error {
	if err := hello.Validate(); err != nil {
		return err
	}
	if hello.Type != sidecar.TypeHello || hello.Version != sidecar.VersionElementGraph {
		return errors.New("meeting foreground requires a protocol-v4 Hello")
	}
	if hello.ElementDescriptor == nil {
		return errors.New("meeting foreground Hello has no element descriptor")
	}
	identity, err := hello.ElementDescriptor.Identity()
	if err != nil {
		return err
	}
	want, err := modelelements.StandardDescriptor().Identity()
	if err != nil {
		return err
	}
	if identity != want {
		return fmt.Errorf("meeting foreground descriptor is %+v, want %+v", identity, want)
	}
	selected := make(map[string]element.Direction, len(hello.SelectedPorts))
	for _, port := range hello.SelectedPorts {
		wantDirection, found := expectedForegroundPorts[port.Name]
		if !found || wantDirection != port.Direction {
			return fmt.Errorf("meeting foreground selected unexpected %s port %q", port.Direction, port.Name)
		}
		if _, duplicate := selected[port.Name]; duplicate {
			return fmt.Errorf("meeting foreground selected port %q twice", port.Name)
		}
		selected[port.Name] = port.Direction
	}
	if len(selected) != len(expectedForegroundPorts) {
		return fmt.Errorf("meeting foreground selected %d/%d exact ports", len(selected), len(expectedForegroundPorts))
	}
	for name := range expectedForegroundPorts {
		if _, found := selected[name]; !found {
			return fmt.Errorf("meeting foreground omitted required port %q", name)
		}
	}
	for _, requirement := range hello.RequiredCapabilities {
		switch requirement.Name {
		case "model.concurrent-io":
			if requirement.Contract != "" {
				return errors.New("meeting foreground concurrent-I/O capability contract must be empty")
			}
		case ForegroundCausalOrderingCapability:
			if requirement.Contract != ForegroundCausalOrderingContract {
				return errors.New("meeting foreground causal-ordering contract drifted")
			}
		default:
			return fmt.Errorf("meeting foreground cannot prove undeclared capability %q", requirement.Name)
		}
	}
	return nil
}

func foregroundReady(
	plugin ForegroundPlugin, hello sidecar.Message,
) (sidecar.Message, *sidecar.ElementSessionContract, error) {
	runtime := sidecarArtifact(plugin.RuntimeArtifact)
	provider := sidecarArtifact(plugin.ProviderArtifact)
	wire := sidecarArtifact(plugin.WireAdapterArtifact)
	for label, artifact := range map[string]sidecar.ArtifactIdentity{
		"runtime": runtime, "provider": provider, "wire adapter": wire,
	} {
		if err := artifact.Validate(); err != nil {
			return sidecar.Message{}, nil, fmt.Errorf("meeting foreground %s: %w", label, err)
		}
	}
	descriptor := hello.ElementDescriptor.Clone()
	ready := sidecar.Message{
		Type: sidecar.TypeReady, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &descriptor, AppliedConfigDigest: sidecar.ElementConfigDigest(hello.ElementConfig),
		RuntimeArtifact: runtime,
	}
	for _, selection := range hello.SelectedPorts {
		if len(selection.Formats) == 0 {
			return sidecar.Message{}, nil, fmt.Errorf("meeting foreground port %s offers no wire format", selection.Name)
		}
		ready.NegotiatedPorts = append(ready.NegotiatedPorts, sidecar.PortNegotiation{
			Name: selection.Name, Direction: selection.Direction, Format: sidecar.CloneWireFormats(selection.Formats)[0],
		})
		adapter := wire
		ready.ResolvedCapabilities = append(ready.ResolvedCapabilities, sidecar.CapabilityIdentity{
			Name:     sidecar.PortCapabilityName(selection.Direction, selection.Name),
			Contract: selection.Type.String(), Provider: provider, Adapter: &adapter,
		})
	}
	for _, requirement := range hello.RequiredCapabilities {
		ready.ResolvedCapabilities = append(ready.ResolvedCapabilities, sidecar.CapabilityIdentity{
			Name: requirement.Name, Contract: requirement.Contract, Provider: provider,
		})
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		return sidecar.Message{}, nil, err
	}
	return ready.Clone(), contract, nil
}

func (session *foregroundSession) Ready() sidecar.Message { return session.ready.Clone() }

func (session *foregroundSession) Frames() <-chan sidecar.Message { return session.frames }

func (session *foregroundSession) Err() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.err
}

func (session *foregroundSession) Send(message sidecar.Message) error {
	if session == nil {
		return errors.New("send meeting foreground frame: nil session")
	}
	session.sendMu.Lock()
	defer session.sendMu.Unlock()
	if err := context.Cause(session.ctx); err != nil {
		return err
	}
	message = message.Clone()
	if err := session.contract.ValidateEngineFrame(message); err != nil {
		return fmt.Errorf("send meeting foreground frame: %w", err)
	}
	if message.Type == sidecar.TypeBye {
		return session.Close()
	}
	if message.Envelope == nil {
		return errors.New("send meeting foreground frame: missing envelope")
	}
	payload, err := session.codec.Decode(message.Envelope.Type, modelelements.EncodedPayload{
		JSON: message.Envelope.JSON, Binary: message.Payload, Media: message.Envelope.Media,
	})
	if err != nil {
		return fmt.Errorf("decode meeting foreground input %s: %w", message.Port, err)
	}
	if !session.configured && message.Port != "tools" && message.Port != "cancel" {
		if len(session.initialQueue) >= maximumForegroundPendingInputs {
			return errors.New("meeting foreground initial configuration queue is full")
		}
		session.initialQueue = append(session.initialQueue, pendingForegroundInput{
			message: message.Clone(), payload: payload,
		})
		return nil
	}
	if err := session.applyDecodedInput(message, payload); err != nil {
		return err
	}
	if message.Port != "tools" || session.configured {
		return nil
	}
	session.configured = true
	queued := session.initialQueue
	session.initialQueue = nil
	slices.SortStableFunc(queued, func(left, right pendingForegroundInput) int {
		leftSequence, rightSequence := left.message.Envelope.Sequence, right.message.Envelope.Sequence
		switch {
		case leftSequence == 0 || rightSequence == 0 || leftSequence == rightSequence:
			return 0
		case leftSequence < rightSequence:
			return -1
		default:
			return 1
		}
	})
	for _, pending := range queued {
		if err := session.applyDecodedInput(pending.message, pending.payload); err != nil {
			return err
		}
	}
	return nil
}

func (session *foregroundSession) applyDecodedInput(message sidecar.Message, payload any) error {
	if message.Port == "trigger" {
		generate, ok := foregroundGenerate(payload)
		if !ok {
			return fmt.Errorf("meeting foreground trigger has payload %T", payload)
		}
		if session.waitsForInjection(message) {
			if len(session.pending) >= maximumForegroundPendingTriggers {
				return errors.New("meeting foreground causal trigger queue is full")
			}
			session.pending = append(session.pending, pendingForegroundTrigger{message: message, payload: generate})
			return nil
		}
		if err := session.applyTrigger(message, generate); err != nil {
			return err
		}
	} else if err := session.applyInput(message, payload); err != nil {
		return err
	}
	if itemID := strings.TrimSpace(message.Envelope.ItemID); itemID != "" {
		session.seen[itemID] = struct{}{}
	}
	return session.drainCausalTriggers()
}

func (session *foregroundSession) waitsForInjection(message sidecar.Message) bool {
	if message.Envelope == nil {
		return false
	}
	for _, parent := range message.Envelope.CausalParents {
		if !strings.HasSuffix(parent, "/meeting-background-injection") {
			continue
		}
		if _, seen := session.seen[parent]; !seen {
			return true
		}
	}
	return false
}

func (session *foregroundSession) drainCausalTriggers() error {
	for index := 0; index < len(session.pending); {
		pending := session.pending[index]
		if session.waitsForInjection(pending.message) {
			index++
			continue
		}
		session.pending = append(session.pending[:index], session.pending[index+1:]...)
		if err := session.applyTrigger(pending.message, pending.payload); err != nil {
			return err
		}
		if pending.message.Envelope != nil && pending.message.Envelope.ItemID != "" {
			session.seen[pending.message.Envelope.ItemID] = struct{}{}
		}
	}
	return nil
}

func (session *foregroundSession) applyInput(message sidecar.Message, payload any) error {
	ctx := session.ctx
	switch message.Port {
	case "audio":
		input, ok := foregroundAudio(payload)
		if !ok {
			return fmt.Errorf("meeting foreground audio has payload %T", payload)
		}
		return session.runtime.Audio(ctx, cloneMeetingFrame(input.Frame))
	case "video":
		input, ok := foregroundVideo(payload)
		if !ok {
			return fmt.Errorf("meeting foreground video has payload %T", payload)
		}
		return session.runtime.Video(ctx, cloneMeetingFrame(input.Frame))
	case "context":
		snapshot, ok := foregroundContext(payload)
		if !ok {
			return fmt.Errorf("meeting foreground context has payload %T", payload)
		}
		return session.updateContext(snapshot)
	case "tools":
		var update foregroundSessionSettings
		if err := decodeForegroundOpaque(payload, &update); err != nil {
			return fmt.Errorf("meeting foreground tools update: %w", err)
		}
		settings := legacy.CloneSettings(update.Settings)
		if err := session.runtime.Update(ctx, meetingForegroundTextOnlySettings(settings)); err != nil {
			return err
		}
		session.mu.Lock()
		session.settings = settings
		session.mu.Unlock()
		return nil
	case "text":
		var injection ContextInjection
		if err := decodeForegroundOpaque(payload, &injection); err != nil {
			return fmt.Errorf("meeting foreground context injection: %w", err)
		}
		if err := validateContextInjection(injection); err != nil {
			return err
		}
		role := injection.Role
		if role != "user" {
			role = "system"
		}
		return session.runtime.Text(ctx, legacy.TextInput{
			ItemID: injection.RunID, Role: role, Text: injection.Text,
		})
	case "tool_result":
		result, ok := foregroundToolResult(payload)
		if !ok {
			return fmt.Errorf("meeting foreground tool result has payload %T", payload)
		}
		result.Output = slices.Clone(result.Output)
		return session.runtime.ToolResult(ctx, result)
	case "commit":
		if _, ok := foregroundAudioCommit(payload); !ok {
			return fmt.Errorf("meeting foreground commit has payload %T", payload)
		}
		return session.runtime.CommitAudio(ctx)
	case "cancel":
		cancel, ok := foregroundCancel(payload)
		if !ok {
			return fmt.Errorf("meeting foreground cancel has payload %T", payload)
		}
		return session.runtime.Cancel(ctx, cancel.Reason)
	case "truncate":
		truncation, ok := foregroundTruncation(payload)
		if !ok {
			return fmt.Errorf("meeting foreground truncation has payload %T", payload)
		}
		return session.runtime.Truncate(ctx, truncation)
	default:
		return fmt.Errorf("meeting foreground received unsupported input port %q", message.Port)
	}
}

// meetingForegroundTextOnlySettings is the deployment boundary between the
// legacy foreground's cognition callbacks and graph-owned synthesis. Keep the
// client settings for tool/instruction policy, but never let a session update
// re-enable the legacy binding's native audio action plane.
func meetingForegroundTextOnlySettings(source legacy.Settings) legacy.Settings {
	result := legacy.CloneSettings(source)
	result.Modalities = []string{"text"}
	return result
}

func (session *foregroundSession) applyTrigger(
	message sidecar.Message, generate cognitionelements.Generate,
) error {
	if err := continuation.ValidateInvocation(generate.Invocation, session.plugin.Descriptor); err != nil {
		return fmt.Errorf("meeting foreground trigger invocation: %w", err)
	}
	runID := ""
	if message.Envelope != nil {
		runID = strings.TrimSpace(message.Envelope.RunID)
	}
	if runID == "" {
		runID = session.newIdentifier("run")
	}
	message = message.Clone()
	message.Envelope.RunID = runID
	session.mu.Lock()
	if len(session.pendingRunIDs) >= maximumForegroundPendingTriggers {
		session.mu.Unlock()
		return errors.New("meeting foreground pending run queue is full")
	}
	session.pendingRunIDs = append(session.pendingRunIDs, pendingForegroundTrigger{
		message: message, payload: generate,
	})
	session.mu.Unlock()
	if err := session.runtime.CreateResponse(session.ctx); err != nil {
		session.mu.Lock()
		for index := len(session.pendingRunIDs) - 1; index >= 0; index-- {
			candidate := session.pendingRunIDs[index]
			if candidate.message.Envelope != nil && candidate.message.Envelope.RunID == runID {
				session.pendingRunIDs = append(session.pendingRunIDs[:index], session.pendingRunIDs[index+1:]...)
				break
			}
		}
		session.mu.Unlock()
		return err
	}
	return nil
}

func (session *foregroundSession) updateContext(snapshot trajectory.Snapshot) error {
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return errors.New("meeting foreground context version does not match its item count")
	}
	items := make([]trajectory.Item, len(snapshot.Items))
	for index := range snapshot.Items {
		items[index] = cloneTrajectoryItemForMeeting(snapshot.Items[index])
	}
	snapshot.Items = items
	if len(snapshot.Items) == 0 {
		snapshot.Items = nil
	}
	session.mu.Lock()
	if snapshot.Version < session.contextSnapshot.Version {
		session.mu.Unlock()
		return errors.New("meeting foreground context moved backwards")
	}
	if snapshot.Version == session.contextSnapshot.Version &&
		!reflect.DeepEqual(snapshot, session.contextSnapshot) {
		session.mu.Unlock()
		return errors.New("meeting foreground context changed without advancing its version")
	}
	if snapshot.Version > session.contextSnapshot.Version {
		session.contextSnapshot = snapshot
		close(session.contextChanged)
		session.contextChanged = make(chan struct{})
	}
	session.mu.Unlock()
	return nil
}

func (session *foregroundSession) Close() error {
	if session == nil {
		return nil
	}
	session.closeOnce.Do(func() {
		cause := errors.New("meeting foreground session closed")
		session.cancel(cause)
		if session.runtime != nil {
			ctx, cancel := context.WithTimeout(context.Background(), foregroundCloseTimeout)
			session.closeErr = session.runtime.Close(ctx, cause)
			cancel()
		}
		session.mu.Lock()
		if session.err == nil {
			session.err = session.closeErr
		}
		session.mu.Unlock()
	})
	return session.closeErr
}

func (session *foregroundSession) TurnBegin(ctx context.Context) error {
	if err := session.outputUsable(ctx, "begin meeting foreground turn"); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, foregroundTurnWait)
	defer cancel()
	select {
	case <-session.turnAdmission:
		defer func() { session.turnAdmission <- struct{}{} }()
	case <-waitCtx.Done():
		return fmt.Errorf("begin meeting foreground turn admission: %w", context.Cause(waitCtx))
	case <-session.ctx.Done():
		return context.Cause(session.ctx)
	}
	for {
		session.mu.Lock()
		if session.active == nil {
			if len(session.draining) >= maximumForegroundDrainingRuns {
				session.mu.Unlock()
				return errors.New("begin meeting foreground turn: draining run limit reached")
			}
			break
		}
		changed := session.runChanged
		session.mu.Unlock()
		select {
		case <-changed:
		case <-waitCtx.Done():
			return fmt.Errorf("begin meeting foreground turn after active run: %w", context.Cause(waitCtx))
		case <-session.ctx.Done():
			return context.Cause(session.ctx)
		}
	}
	runID := ""
	invocation := continuation.Invocation{}
	var parents []string
	if len(session.pendingRunIDs) != 0 {
		pending := session.pendingRunIDs[0]
		session.pendingRunIDs = session.pendingRunIDs[1:]
		if pending.message.Envelope != nil {
			runID = strings.TrimSpace(pending.message.Envelope.RunID)
			parents = append(parents, pending.message.Envelope.ItemID)
			parents = append(parents, pending.message.Envelope.CausalParents...)
		}
		invocation = cloneForegroundInvocation(pending.payload.Invocation)
	}
	if runID == "" {
		runID = session.newIdentifier("run")
	}
	if strings.TrimSpace(invocation.Instruction) == "" {
		invocation = session.defaultForegroundInvocationLocked()
	}
	if err := continuation.ValidateInvocation(invocation, session.plugin.Descriptor); err != nil {
		session.mu.Unlock()
		return fmt.Errorf("begin meeting foreground turn: %w", err)
	}
	if _, duplicate := session.seenRunIDs[runID]; duplicate {
		session.mu.Unlock()
		return fmt.Errorf("begin meeting foreground turn: duplicate run ID %q", runID)
	}
	if len(session.seenRunIDs) >= maximumForegroundIdentities {
		session.mu.Unlock()
		return errors.New("begin meeting foreground turn: run identity limit reached")
	}
	session.seenRunIDs[runID] = struct{}{}
	requiredTranscript := ""
	requiredEventID := ""
	if session.lastFinalRevision > session.boundFinalRevision {
		requiredTranscript = session.lastFinalTranscript
		requiredEventID = session.lastFinalEventID
		session.boundFinalRevision = session.lastFinalRevision
	}
	session.active = &foregroundRun{
		id: runID, invocation: invocation,
		context:   cloneForegroundSnapshot(session.contextSnapshot),
		parentIDs: canonicalForegroundParents(parents), startedNS: foregroundNowNS(),
		utterances:         make(map[string]*foregroundUtterance),
		requiredTranscript: requiredTranscript,
		requiredEventID:    requiredEventID,
	}
	session.mu.Unlock()
	return nil
}

func (session *foregroundSession) TurnEnd(
	ctx context.Context, outcome legacy.TurnOutcome,
) error {
	if err := session.outputUsable(ctx, "end meeting foreground turn"); err != nil {
		return err
	}
	// Establish the cognition-end marker behind any response callback that has
	// already entered. Release it while waiting for the final transcript so
	// paced SpeechEnd can continue draining this run.
	session.responseCallbackMu.Lock()
	session.mu.Lock()
	if session.active == nil {
		session.mu.Unlock()
		session.responseCallbackMu.Unlock()
		return errors.New("end meeting foreground turn: no active run")
	}
	if session.active.turnEnded {
		session.mu.Unlock()
		session.responseCallbackMu.Unlock()
		return errors.New("end meeting foreground turn: run already ended")
	}
	run := session.active
	run.turnEnded = true
	run.turnOutcome = outcome
	atCognitionEnd := cloneForegroundSnapshot(session.contextSnapshot)
	session.mu.Unlock()
	session.responseCallbackMu.Unlock()

	// The graph may admit another run while this run's speech drains. Freeze
	// the authoritative context before releasing that admission boundary so a
	// later turn can never be stamped into this run with hindsight. A final ASR
	// transcript may still be crossing the trajectory boundary; when it is,
	// wait only for its exact output event identity and retain the prefix through
	// that observation rather than the mutable snapshot visible at playback end.
	frozen, contextErr := session.contextAtCognitionEnd(
		ctx, run.requiredTranscript, run.requiredEventID, atCognitionEnd,
	)

	// Serialize the terminal transition behind any response-bearing callback
	// that already owns a run. In particular, a multi-call ToolCalls callback
	// must publish all of its proposals before this run's outcome fence.
	session.responseCallbackMu.Lock()
	defer session.responseCallbackMu.Unlock()
	session.mu.Lock()
	run.context = frozen
	run.contextErr = contextErr
	run.contextReady = true
	// TurnEnd brackets provider cognition, not paced playback. Once it arrives,
	// all future non-speech callbacks belong to the next run. Keep this run in a
	// separately addressed draining set until every reserved utterance ends.
	session.active = nil
	close(session.runChanged)
	session.runChanged = make(chan struct{})
	if !foregroundRunSpeechComplete(run) {
		session.draining[run.id] = run
	}
	run = session.takeFinalRunLocked(run)
	session.mu.Unlock()
	if run == nil {
		return nil
	}
	return session.finalizeForegroundRun(ctx, run)
}

func (session *foregroundSession) Activity(
	ctx context.Context, activity legacy.ActivityEvent,
) error {
	if err := session.outputUsable(ctx, "publish meeting foreground activity"); err != nil {
		return err
	}
	return session.emit(ctx, "activity", modelelements.ActivityType(), "", nil, activity)
}

func (session *foregroundSession) Transcript(
	ctx context.Context, event legacy.TranscriptEvent,
) error {
	if err := session.outputUsable(ctx, "publish meeting foreground transcript"); err != nil {
		return err
	}
	text := strings.TrimSpace(event.Text)
	if text == "" || !utf8.ValidString(text) || len(text) > maximumMeetingAdapterTextBytes {
		return errors.New("publish meeting foreground transcript: bounded UTF-8 text is required")
	}
	key := strings.TrimSpace(event.ItemID)
	if !canonicalText(key) {
		key = "microphone"
	}
	eventID := session.newIdentifier("transcript-event")
	session.mu.Lock()
	session.transcriptRevision++
	revision := session.transcriptRevision
	supersedes := session.transcriptByItem[key]
	if len(session.transcriptByItem) >= maximumForegroundTranscriptKeys && supersedes == 0 {
		clear(session.transcriptByItem)
	}
	session.transcriptByItem[key] = revision
	if event.Final {
		session.lastFinalTranscript = text
		session.lastFinalRevision = revision
		session.lastFinalEventID = eventID
	}
	session.mu.Unlock()
	observation := perception.Observation{
		Text: text, Observer: "meeting.foreground.asr", Source: "microphone",
		Authority: trajectory.AuthorityUser, Revision: revision, Supersedes: supersedes,
		Provisional: !event.Final, Final: event.Final, OccurredNS: foregroundNowNS(),
	}
	if event.Final {
		observation.StableText = text
	}
	return session.emitWithIdentity(ctx, "transcript", modelelements.TranscriptType(), "", nil,
		eventID, "microphone", key, observation)
}

func (session *foregroundSession) Observation(
	ctx context.Context, observation perception.Observation,
) error {
	if err := session.outputUsable(ctx, "accept meeting foreground observation"); err != nil {
		return err
	}
	if err := observation.Validate(); err != nil {
		return fmt.Errorf("accept meeting foreground observation: %w", err)
	}
	// The foreground receives pixels directly for its own fast/reflex policy.
	// Persistent screen narration is independently owned by the graph's
	// perception.VisualObserver and must not be committed a second time here.
	return nil
}

func (session *foregroundSession) SpeechReserved(
	ctx context.Context, utterance action.Utterance,
) error {
	session.responseCallbackMu.Lock()
	defer session.responseCallbackMu.Unlock()
	if err := session.outputUsable(ctx, "reserve meeting foreground speech"); err != nil {
		return err
	}
	if !canonicalText(utterance.ID) {
		return errors.New("reserve meeting foreground speech: canonical utterance ID is required")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.active == nil || session.active.turnEnded {
		return errors.New("reserve meeting foreground speech outside an active turn")
	}
	if _, duplicate := session.active.utterances[utterance.ID]; duplicate {
		return errors.New("reserve meeting foreground speech: duplicate utterance")
	}
	if _, duplicate := session.utteranceRuns[utterance.ID]; duplicate {
		return errors.New("reserve meeting foreground speech: duplicate utterance")
	}
	if _, duplicate := session.seenUtteranceIDs[utterance.ID]; duplicate {
		return errors.New("reserve meeting foreground speech: reused utterance ID")
	}
	if len(session.seenUtteranceIDs) >= maximumForegroundIdentities {
		return errors.New("reserve meeting foreground speech: utterance identity limit reached")
	}
	session.active.utterances[utterance.ID] = &foregroundUtterance{value: cloneForegroundUtterance(utterance)}
	session.utteranceRuns[utterance.ID] = session.active
	session.seenUtteranceIDs[utterance.ID] = struct{}{}
	return nil
}

func (session *foregroundSession) SpeechReservationCancelled(
	_ context.Context, utterance action.Utterance,
) {
	session.responseCallbackMu.Lock()
	defer session.responseCallbackMu.Unlock()
	session.mu.Lock()
	run := session.utteranceRuns[utterance.ID]
	if run == nil {
		session.mu.Unlock()
		return
	}
	pending := run.utterances[utterance.ID]
	if pending != nil && !pending.audioOpened && !pending.textOpened {
		pending.done = true
	}
	final := session.takeFinalRunLocked(run)
	session.mu.Unlock()
	if final != nil {
		if err := session.finalizeForegroundRun(session.ctx, final); err != nil {
			session.fail(err)
		}
	}
}

func (session *foregroundSession) SpeechBegin(
	ctx context.Context, utterance action.Utterance,
) error {
	session.responseCallbackMu.Lock()
	defer session.responseCallbackMu.Unlock()
	if err := session.outputUsable(ctx, "begin meeting foreground speech"); err != nil {
		return err
	}
	if !canonicalText(utterance.ID) || strings.TrimSpace(utterance.Text) == "" {
		return errors.New("begin meeting foreground speech: canonical ID and text are required")
	}
	session.mu.Lock()
	run := session.utteranceRuns[utterance.ID]
	if run == nil {
		if session.active == nil || session.active.turnEnded {
			session.mu.Unlock()
			return errors.New("begin meeting foreground speech outside an active turn")
		}
		run = session.active
	}
	pending := run.utterances[utterance.ID]
	created := false
	if pending == nil {
		if _, duplicate := session.utteranceRuns[utterance.ID]; duplicate {
			session.mu.Unlock()
			return errors.New("begin meeting foreground speech: duplicate utterance")
		}
		if _, duplicate := session.seenUtteranceIDs[utterance.ID]; duplicate {
			session.mu.Unlock()
			return errors.New("begin meeting foreground speech: reused utterance ID")
		}
		if len(session.seenUtteranceIDs) >= maximumForegroundIdentities {
			session.mu.Unlock()
			return errors.New("begin meeting foreground speech: utterance identity limit reached")
		}
		pending = &foregroundUtterance{value: cloneForegroundUtterance(utterance)}
		created = true
	}
	if pending.audioOpened || pending.textOpened || pending.done {
		session.mu.Unlock()
		return errors.New("begin meeting foreground speech: utterance already opened")
	}
	parents := slices.Clone(run.parentIDs)
	session.mu.Unlock()
	begin := speechelements.AudioFrame{
		Kind: speechelements.AudioBegin, UtteranceID: utterance.ID,
		Utterance:       cloneForegroundUtterance(utterance),
		SpeechAuthority: string(session.plugin.Descriptor.EffectiveSpeechAuthority()),
	}
	if err := session.emitResponse(ctx, run, "audio_out", modelelements.PreparedAudioType(), parents, begin); err != nil {
		return err
	}
	if err := session.emitResponse(ctx, run, "text_out", modelelements.PreparedTextType(), parents,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0}); err != nil {
		return session.failResponsePublication("begin meeting foreground speech after audio begin", err)
	}
	session.mu.Lock()
	if created {
		if run.utterances[utterance.ID] != nil || session.utteranceRuns[utterance.ID] != nil {
			session.mu.Unlock()
			return session.failResponsePublication("commit meeting foreground speech begin",
				errors.New("utterance state changed during publication"))
		}
		run.utterances[utterance.ID] = pending
		session.utteranceRuns[utterance.ID] = run
		session.seenUtteranceIDs[utterance.ID] = struct{}{}
	} else if run.utterances[utterance.ID] != pending || session.utteranceRuns[utterance.ID] != run {
		session.mu.Unlock()
		return session.failResponsePublication("commit meeting foreground speech begin",
			errors.New("reserved utterance state changed during publication"))
	}
	pending.value = cloneForegroundUtterance(utterance)
	pending.audioOpened = true
	pending.textOpened = true
	session.mu.Unlock()
	return nil
}

func (session *foregroundSession) SpeechText(
	ctx context.Context, utterance action.Utterance, text string,
) error {
	session.responseCallbackMu.Lock()
	defer session.responseCallbackMu.Unlock()
	if err := session.outputUsable(ctx, "publish meeting foreground speech text"); err != nil {
		return err
	}
	if text == "" || !utf8.ValidString(text) || len(text) > maximumMeetingAdapterTextBytes {
		return errors.New("publish meeting foreground speech text: bounded UTF-8 delta is required")
	}
	session.mu.Lock()
	run := session.utteranceRuns[utterance.ID]
	if run == nil {
		session.mu.Unlock()
		return errors.New("publish meeting foreground speech text outside a retained turn")
	}
	pending := run.utterances[utterance.ID]
	if pending == nil || !pending.textOpened || pending.done {
		session.mu.Unlock()
		return errors.New("publish meeting foreground speech text outside an open utterance")
	}
	index := pending.textIndex + 1
	parents := slices.Clone(run.parentIDs)
	session.mu.Unlock()
	if err := session.emitResponse(ctx, run, "text_out", modelelements.PreparedTextType(), parents,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextChunk, Index: index, Text: text}); err != nil {
		return err
	}
	session.mu.Lock()
	if run.utterances[utterance.ID] != pending || pending.done || pending.textIndex+1 != index {
		session.mu.Unlock()
		return session.failResponsePublication("commit meeting foreground speech text",
			errors.New("utterance text state changed during publication"))
	}
	pending.textIndex = index
	run.assistant.WriteString(text)
	run.outputs = append(run.outputs, cognitionelements.PreparedOutput{
		Kind: cognitionelements.PreparedAssistant, Text: text,
	})
	session.mu.Unlock()
	return nil
}

func (session *foregroundSession) SpeechAudio(
	ctx context.Context, utterance action.Utterance, frame action.Frame,
) error {
	session.responseCallbackMu.Lock()
	defer session.responseCallbackMu.Unlock()
	if err := session.outputUsable(ctx, "publish meeting foreground speech audio"); err != nil {
		return err
	}
	if len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 || frame.SampleRateHz == 0 {
		return errors.New("publish meeting foreground speech audio: PCM16 and sample rate are required")
	}
	session.mu.Lock()
	run := session.utteranceRuns[utterance.ID]
	if run == nil {
		session.mu.Unlock()
		return errors.New("publish meeting foreground speech audio outside a retained turn")
	}
	pending := run.utterances[utterance.ID]
	if pending == nil || !pending.audioOpened || pending.done {
		session.mu.Unlock()
		return errors.New("publish meeting foreground speech audio outside an open utterance")
	}
	samples := uint64(len(frame.PCM16LE) / 2)
	if samples > ^uint64(0)-pending.sampleOffset {
		session.mu.Unlock()
		return errors.New("publish meeting foreground speech audio: sample offset overflow")
	}
	offset := pending.sampleOffset
	parents := slices.Clone(run.parentIDs)
	session.mu.Unlock()
	chunk := speechelements.AudioFrame{
		Kind: speechelements.AudioChunk, UtteranceID: utterance.ID,
		SpeechAuthority: string(session.plugin.Descriptor.EffectiveSpeechAuthority()),
		Chunk: v1.SpeechChunk{
			ChunkID: session.newIdentifier("audio"), CandidateID: utterance.ID,
			SampleOffset: offset, SampleRateHz: frame.SampleRateHz,
			PCM16LE: slices.Clone(frame.PCM16LE), Final: frame.Final,
		},
	}
	if err := session.emitResponse(ctx, run, "audio_out", modelelements.PreparedAudioType(), parents, chunk); err != nil {
		return err
	}
	session.mu.Lock()
	if run.utterances[utterance.ID] != pending || pending.done || pending.sampleOffset != offset {
		session.mu.Unlock()
		return session.failResponsePublication("commit meeting foreground speech audio",
			errors.New("utterance audio state changed during publication"))
	}
	pending.sampleOffset += samples
	session.mu.Unlock()
	return nil
}

func (session *foregroundSession) SpeechEnd(
	ctx context.Context, utterance action.Utterance, outcome action.Outcome,
) error {
	session.responseCallbackMu.Lock()
	defer session.responseCallbackMu.Unlock()
	if err := session.outputUsable(ctx, "end meeting foreground speech"); err != nil {
		return err
	}
	session.mu.Lock()
	owner := session.utteranceRuns[utterance.ID]
	if owner == nil {
		session.mu.Unlock()
		return errors.New("end meeting foreground speech outside a retained turn")
	}
	pending := owner.utterances[utterance.ID]
	if pending == nil || pending.done || !pending.audioOpened || !pending.textOpened {
		session.mu.Unlock()
		return errors.New("end meeting foreground speech: unknown or closed utterance")
	}
	parents := slices.Clone(owner.parentIDs)
	textIndex := pending.textIndex + 1
	session.mu.Unlock()
	if err := session.emitResponse(ctx, owner, "text_out", modelelements.PreparedTextType(), parents,
		cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextEnd, Index: textIndex, Interrupted: !outcome.Completed,
		}); err != nil {
		return err
	}
	kind := speechelements.OutcomeSucceeded
	if !outcome.Completed {
		kind = speechelements.OutcomeCancelled
	}
	if err := session.emitResponse(ctx, owner, "audio_out", modelelements.PreparedAudioType(), parents,
		speechelements.AudioFrame{
			Kind: speechelements.AudioEnd, UtteranceID: utterance.ID,
			SpeechAuthority: string(session.plugin.Descriptor.EffectiveSpeechAuthority()),
			Terminal: speechelements.SynthesisOutcome{
				UtteranceID: utterance.ID, Kind: kind, Message: outcome.Reason,
			},
		}); err != nil {
		return session.failResponsePublication("end meeting foreground speech after text end", err)
	}
	session.mu.Lock()
	if owner.utterances[utterance.ID] != pending || pending.done ||
		!pending.audioOpened || !pending.textOpened {
		session.mu.Unlock()
		return session.failResponsePublication("commit meeting foreground speech end",
			errors.New("utterance state changed during publication"))
	}
	pending.done = true
	run := session.takeFinalRunLocked(owner)
	session.mu.Unlock()
	if run != nil {
		return session.finalizeForegroundRun(ctx, run)
	}
	return nil
}

func (session *foregroundSession) ToolCalls(
	ctx context.Context, event legacy.ToolCallEvent,
) error {
	session.responseCallbackMu.Lock()
	defer session.responseCallbackMu.Unlock()
	if err := session.outputUsable(ctx, "publish meeting foreground tool calls"); err != nil {
		return err
	}
	if len(event.Calls) == 0 {
		return errors.New("publish meeting foreground tool calls: at least one call is required")
	}
	session.mu.Lock()
	if session.active == nil || session.active.turnEnded {
		session.mu.Unlock()
		return errors.New("publish meeting foreground tool calls outside an active turn")
	}
	parents := slices.Clone(session.active.parentIDs)
	settings := legacy.CloneSettings(session.settings)
	proposals := make([]cognitionelements.ToolProposal, len(event.Calls))
	for index, call := range event.Calls {
		call.Arguments = slices.Clone(call.Arguments)
		declared := slices.ContainsFunc(settings.Tools, func(tool action.ToolSpec) bool {
			return tool.Name == call.Name
		})
		proposal := cognitionelements.ToolProposal{
			Call: call, Declared: declared,
			ProviderAuthority: session.plugin.Descriptor.EffectiveToolAuthority(),
		}
		proposals[index] = proposal
	}
	run := session.active
	session.mu.Unlock()
	for index, proposal := range proposals {
		if err := session.emitResponse(ctx, run, "tool_proposal", modelelements.ToolProposalType(), parents, proposal); err != nil {
			if index != 0 {
				return session.failResponsePublication("publish meeting foreground tool proposal batch", err)
			}
			return err
		}
	}
	session.mu.Lock()
	if session.active != run || run.turnEnded {
		session.mu.Unlock()
		return session.failResponsePublication("commit meeting foreground tool proposals",
			errors.New("active run changed during publication"))
	}
	for _, proposal := range proposals {
		copy := proposal
		run.outputs = append(run.outputs, cognitionelements.PreparedOutput{
			Kind: cognitionelements.PreparedTool, Proposal: &copy,
		})
		run.proposals = append(run.proposals, proposal)
	}
	if event.Usage != nil {
		run.completion.Usage = *event.Usage
	}
	session.mu.Unlock()
	return nil
}

func (session *foregroundSession) Failed(_ context.Context, event legacy.ErrorEvent) {
	session.mu.Lock()
	if session.active != nil {
		copy := event
		session.active.failed = &copy
		session.mu.Unlock()
		return
	}
	session.mu.Unlock()
	session.fail(fmt.Errorf("meeting foreground runtime failed: %s: %s", event.Code, event.Message))
}

func foregroundRunSpeechComplete(run *foregroundRun) bool {
	if run == nil {
		return false
	}
	for _, utterance := range run.utterances {
		if !utterance.done {
			return false
		}
	}
	return true
}

func (session *foregroundSession) takeFinalRunLocked(run *foregroundRun) *foregroundRun {
	if run == nil || !run.turnEnded || !run.contextReady || !foregroundRunSpeechComplete(run) {
		return nil
	}
	if session.active == run {
		session.active = nil
		close(session.runChanged)
		session.runChanged = make(chan struct{})
	}
	if session.draining[run.id] == run {
		delete(session.draining, run.id)
	}
	for utteranceID := range run.utterances {
		if session.utteranceRuns[utteranceID] == run {
			delete(session.utteranceRuns, utteranceID)
		}
	}
	return run
}

func (session *foregroundSession) finalizeForegroundRun(
	ctx context.Context, run *foregroundRun,
) error {
	if run == nil {
		return errors.New("finalize meeting foreground run: nil run")
	}
	snapshot := cloneForegroundSnapshot(run.context)
	err := run.contextErr
	if err != nil {
		outcome := cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeFailed, Operation: "generate", RunID: run.id,
			ProviderReference: ForegroundDeploymentReference, Code: "context_sync", Message: err.Error(),
			StartedNS: run.startedNS, FinishedNS: foregroundNowNS(),
		}
		outcome.DurationNS = foregroundDuration(run.startedNS, outcome.FinishedNS)
		if err := session.emitResponse(ctx, run, "outcome", modelelements.OutcomeType(), run.parentIDs, outcome); err != nil {
			return session.failResponsePublication("finalize meeting foreground context failure", err)
		}
		return nil
	}
	completion := run.completion
	if completion.StopReason == "" {
		completion.StopReason = strings.TrimSpace(run.turnOutcome.Reason)
	}
	if completion.StopReason == "" {
		completion.StopReason = "stop"
	}
	result := cognitionelements.Result{
		RunID: run.id, ProviderReference: ForegroundDeploymentReference,
		Descriptor: session.plugin.Descriptor, ContextVersion: snapshot.Version,
		Invocation: cloneForegroundInvocation(run.invocation),
		Outputs:    clonePreparedOutputs(run.outputs), AssistantText: run.assistant.String(),
		ToolProposals: cloneForegroundProposals(run.proposals), Completion: completion,
		Interrupted: run.turnOutcome.Incomplete || run.failed != nil,
	}
	if len(snapshot.Items) != 0 {
		result.ContextTailID = snapshot.Items[len(snapshot.Items)-1].ID
	}
	if err := session.emit(ctx, "result", modelelements.ResultType(), run.id, run.parentIDs, result); err != nil {
		return session.failResponsePublication("publish meeting foreground result", err)
	}
	kind := cognitionelements.OutcomeSucceeded
	code, message := "", strings.TrimSpace(run.turnOutcome.Detail)
	if run.failed != nil {
		kind, code, message = cognitionelements.OutcomeFailed, run.failed.Code, run.failed.Message
	} else if run.turnOutcome.Incomplete {
		kind, code = cognitionelements.OutcomeCanceled, strings.TrimSpace(run.turnOutcome.Reason)
		if message == "" {
			message = run.turnOutcome.Reason
		}
	}
	finished := foregroundNowNS()
	outcome := cognitionelements.Outcome{
		Kind: kind, Operation: "generate", RunID: run.id,
		ProviderReference: ForegroundDeploymentReference, ContextVersion: snapshot.Version,
		Code: code, Message: message, StartedNS: run.startedNS, FinishedNS: finished,
		DurationNS: foregroundDuration(run.startedNS, finished),
	}
	if err := session.emitResponse(ctx, run, "outcome", modelelements.OutcomeType(), run.parentIDs, outcome); err != nil {
		return session.failResponsePublication("publish meeting foreground terminal outcome", err)
	}
	return nil
}

func (session *foregroundSession) contextAtCognitionEnd(
	ctx context.Context, requiredTranscript, requiredEventID string,
	atCognitionEnd trajectory.Snapshot,
) (trajectory.Snapshot, error) {
	if requiredEventID == "" {
		return cloneForegroundSnapshot(atCognitionEnd), nil
	}
	if strings.TrimSpace(requiredTranscript) == "" || !canonicalText(requiredEventID) {
		return cloneForegroundSnapshot(atCognitionEnd),
			errors.New("final transcript has no canonical text or event identity")
	}
	if _, found := foregroundRequiredTranscriptIndex(
		atCognitionEnd, requiredTranscript, requiredEventID,
	); found {
		return cloneForegroundSnapshot(atCognitionEnd), nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, foregroundContextWait)
	defer cancel()
	for {
		session.mu.Lock()
		snapshot := cloneForegroundSnapshot(session.contextSnapshot)
		changed := session.contextChanged
		session.mu.Unlock()
		if index, found := foregroundRequiredTranscriptIndex(
			snapshot, requiredTranscript, requiredEventID,
		); found {
			prefix := trajectory.Snapshot{
				Version: uint64(index + 1), Items: snapshot.Items[:index+1],
			}
			return cloneForegroundSnapshot(prefix), nil
		}
		select {
		case <-waitCtx.Done():
			return trajectory.Snapshot{}, fmt.Errorf(
				"committed context did not bind final transcript before deadline: %w", context.Cause(waitCtx))
		case <-changed:
		}
	}
}

func foregroundRequiredTranscriptIndex(
	snapshot trajectory.Snapshot, requiredTranscript, requiredEventID string,
) (int, bool) {
	for index, item := range snapshot.Items {
		if item.Kind == trajectory.KindObservation && item.Content == requiredTranscript &&
			item.Event != nil && item.Event.EventID == requiredEventID &&
			item.Event.Type == "meeting.foreground.asr.endpoint" &&
			item.Event.Source == "meeting.foreground.asr" && item.Event.Channel == "microphone" &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			return index, true
		}
	}
	return 0, false
}

func (session *foregroundSession) emit(
	ctx context.Context, port string, valueType element.Type, runID string,
	parents []string, payload any,
) error {
	return session.emitWithIdentity(ctx, port, valueType, runID, parents, "", "", "", payload)
}

// emitResponse assigns one gap-free sequence to the complete Realtime-facing
// response stream before publishing it onto model.External's typed ports. The
// lock is intentionally held through the sidecar enqueue: allocating sequence
// N and then allowing N+1 to enter the shared frame channel first would merely
// move the scheduler race to the provider boundary.
func (session *foregroundSession) emitResponse(
	ctx context.Context, run *foregroundRun, port string, valueType element.Type,
	parents []string, payload any,
) error {
	if run == nil || !canonicalText(run.id) {
		return errors.New("emit meeting foreground response: canonical run is required")
	}
	session.responseMu.Lock()
	defer session.responseMu.Unlock()
	if session.responseSequence >= maximumMeetingResponseEvents {
		return fmt.Errorf("emit meeting foreground response: exceeds %d events", maximumMeetingResponseEvents)
	}
	next := session.responseSequence + 1
	if err := session.emitWithSequence(
		ctx, port, valueType, run.id, parents, "", "", "", next, payload,
	); err != nil {
		return err
	}
	session.responseSequence = next
	return nil
}

func (session *foregroundSession) emitWithIdentity(
	ctx context.Context, port string, valueType element.Type, runID string,
	parents []string, itemID, sourceID, opportunityID string, payload any,
) error {
	return session.emitWithSequence(
		ctx, port, valueType, runID, parents, itemID, sourceID, opportunityID, 0, payload,
	)
}

func (session *foregroundSession) emitWithSequence(
	ctx context.Context, port string, valueType element.Type, runID string,
	parents []string, itemID, sourceID, opportunityID string, responseSequence uint64, payload any,
) error {
	if err := session.outputUsable(ctx, "emit meeting foreground frame"); err != nil {
		return err
	}
	encoded, err := session.codec.Encode(valueType, payload)
	if err != nil {
		return fmt.Errorf("encode meeting foreground %s: %w", port, err)
	}
	identitySequence := session.sequence.Add(1)
	if itemID == "" {
		itemID = fmt.Sprintf("meeting-foreground-%s-%d", strings.ReplaceAll(port, "_", "-"), identitySequence)
	}
	if sourceID == "" {
		sourceID = ForegroundDeploymentReference
	}
	if opportunityID == "" {
		opportunityID = itemID
	}
	envelopeSequence := identitySequence
	if responseSequence != 0 {
		envelopeSequence = responseSequence
	}
	envelope := element.Envelope{
		Type: valueType.Clone(), ItemID: itemID, SessionID: session.options.SessionID,
		SourceID: sourceID, OpportunityID: opportunityID,
		RunID: runID, Sequence: envelopeSequence, TraceID: itemID,
		CancellationScope: session.options.SessionID,
		CausalParents:     canonicalForegroundParents(parents), Payload: payload,
	}
	wire := sidecar.FromEnvelope(envelope, encoded.JSON)
	wire.Media = encoded.Media
	message := sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: port, Envelope: &wire,
		Payload: encoded.Binary, PayloadBytes: len(encoded.Binary),
	}
	if err := session.contract.ValidateSidecarFrame(message); err != nil {
		return fmt.Errorf("conform meeting foreground %s: %w", port, err)
	}
	select {
	case session.frames <- message:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-session.ctx.Done():
		return context.Cause(session.ctx)
	}
}

func (session *foregroundSession) outputUsable(ctx context.Context, operation string) error {
	if session == nil || ctx == nil {
		return fmt.Errorf("%s: session and context are required", operation)
	}
	if err := context.Cause(session.ctx); err != nil {
		return err
	}
	return context.Cause(ctx)
}

func (session *foregroundSession) fail(err error) {
	if err == nil {
		return
	}
	session.mu.Lock()
	if session.err == nil {
		session.err = err
	}
	session.mu.Unlock()
	session.cancel(err)
}

func (session *foregroundSession) failResponsePublication(operation string, err error) error {
	if err == nil {
		return nil
	}
	wrapped := fmt.Errorf("%s: %w", operation, err)
	session.fail(wrapped)
	return wrapped
}

func (session *foregroundSession) defaultForegroundInvocationLocked() continuation.Invocation {
	instruction := strings.TrimSpace(session.settings.Instruction)
	if instruction == "" {
		instruction = "Respond concisely and helpfully to the current meeting turn."
	}
	tools := make([]continuation.ToolDefinition, len(session.settings.Tools))
	for index, tool := range session.settings.Tools {
		tools[index] = continuation.ToolDefinition{
			Name: tool.Name, Description: tool.Description,
			Parameters: slices.Clone(tool.Parameters), Background: tool.Background,
		}
	}
	return continuation.Invocation{
		Instruction: instruction, Tools: tools,
		MaxOutputTokens: session.plugin.Capabilities.MaxOutputTokens,
	}
}

func cloneForegroundInvocation(source continuation.Invocation) continuation.Invocation {
	result := source
	result.Capabilities = slices.Clone(source.Capabilities)
	result.Tools = slices.Clone(source.Tools)
	for index := range result.Tools {
		result.Tools[index].Parameters = slices.Clone(source.Tools[index].Parameters)
	}
	return result
}

func cloneForegroundSnapshot(source trajectory.Snapshot) trajectory.Snapshot {
	result := trajectory.Snapshot{Version: source.Version, Items: make([]trajectory.Item, len(source.Items))}
	for index := range source.Items {
		result.Items[index] = cloneTrajectoryItemForMeeting(source.Items[index])
	}
	if len(result.Items) == 0 {
		result.Items = nil
	}
	return result
}

func clonePreparedOutputs(source []cognitionelements.PreparedOutput) []cognitionelements.PreparedOutput {
	result := make([]cognitionelements.PreparedOutput, len(source))
	for index, output := range source {
		result[index] = output
		if output.Proposal != nil {
			proposal := cloneForegroundProposal(*output.Proposal)
			result[index].Proposal = &proposal
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func cloneForegroundProposals(source []cognitionelements.ToolProposal) []cognitionelements.ToolProposal {
	result := make([]cognitionelements.ToolProposal, len(source))
	for index := range source {
		result[index] = cloneForegroundProposal(source[index])
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func cloneForegroundProposal(source cognitionelements.ToolProposal) cognitionelements.ToolProposal {
	result := source
	result.Call.Arguments = slices.Clone(source.Call.Arguments)
	return result
}

func cloneForegroundUtterance(source action.Utterance) action.Utterance {
	result := source
	result.AssistantItemIDs = slices.Clone(source.AssistantItemIDs)
	return result
}

func canonicalForegroundParents(source []string) []string {
	result := make([]string, 0, len(source))
	seen := make(map[string]struct{}, len(source))
	for _, parent := range source {
		parent = strings.TrimSpace(parent)
		if parent == "" || len(parent) > sidecar.MaxElementIdentifierBytes ||
			strings.ContainsAny(parent, "\x00\r\n") {
			continue
		}
		if _, duplicate := seen[parent]; duplicate {
			continue
		}
		seen[parent] = struct{}{}
		result = append(result, parent)
		if len(result) == sidecar.MaxEnvelopeParents {
			break
		}
	}
	return result
}

func foregroundNowNS() uint64 {
	now := time.Now().UnixNano()
	if now <= 0 {
		return 1
	}
	return uint64(now)
}

func foregroundDuration(started, finished uint64) uint64 {
	if finished <= started {
		return 0
	}
	return finished - started
}

func (session *foregroundSession) newIdentifier(kind string) string {
	return fmt.Sprintf("meeting-foreground-%s-%d", kind, session.sequence.Add(1))
}

func validateContextInjection(injection ContextInjection) error {
	if !canonicalText(injection.Role) || !canonicalText(injection.Source) ||
		!canonicalText(injection.RunID) || strings.TrimSpace(injection.Text) == "" ||
		!utf8.ValidString(injection.Text) || len(injection.Text) > maximumMeetingAdapterTextBytes {
		return errors.New("meeting foreground context injection is not canonical or exceeds its bound")
	}
	switch injection.Role {
	case "user", "system", "background":
		return nil
	default:
		return fmt.Errorf("meeting foreground context injection has unsupported role %q", injection.Role)
	}
}

func decodeForegroundOpaque(payload any, target any) error {
	opaque, ok := payload.(modelelements.OpaquePayload)
	if !ok {
		if pointer, pointerOK := payload.(*modelelements.OpaquePayload); pointerOK && pointer != nil {
			opaque = *pointer
			ok = true
		}
	}
	if !ok || len(opaque.Binary) != 0 || opaque.Media != nil {
		return fmt.Errorf("payload has type %T or an unexpected binary lane", payload)
	}
	decoder := json.NewDecoder(bytes.NewReader(opaque.JSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func sidecarArtifact(source inspect.ArtifactIdentity) sidecar.ArtifactIdentity {
	return sidecar.ArtifactIdentity{ID: source.ID, Revision: source.Revision, Digest: source.Digest}
}

func foregroundAudio(payload any) (acousticelements.InputFrame, bool) {
	switch value := payload.(type) {
	case acousticelements.InputFrame:
		return value, true
	case *acousticelements.InputFrame:
		if value != nil {
			return *value, true
		}
	}
	return acousticelements.InputFrame{}, false
}

func foregroundVideo(payload any) (modelelements.VideoInputFrame, bool) {
	switch value := payload.(type) {
	case modelelements.VideoInputFrame:
		return value, true
	case *modelelements.VideoInputFrame:
		if value != nil {
			return *value, true
		}
	}
	return modelelements.VideoInputFrame{}, false
}

func foregroundContext(payload any) (trajectory.Snapshot, bool) {
	switch value := payload.(type) {
	case trajectory.Snapshot:
		return value, true
	case *trajectory.Snapshot:
		if value != nil {
			return *value, true
		}
	}
	return trajectory.Snapshot{}, false
}

func foregroundToolResult(payload any) (trajectory.ToolResult, bool) {
	switch value := payload.(type) {
	case trajectory.ToolResult:
		return value, true
	case *trajectory.ToolResult:
		if value != nil {
			return *value, true
		}
	}
	return trajectory.ToolResult{}, false
}

func foregroundGenerate(payload any) (cognitionelements.Generate, bool) {
	switch value := payload.(type) {
	case cognitionelements.Generate:
		return value, true
	case *cognitionelements.Generate:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Generate{}, false
}

func foregroundAudioCommit(payload any) (acousticelements.AudioCommit, bool) {
	switch value := payload.(type) {
	case acousticelements.AudioCommit:
		return value, true
	case *acousticelements.AudioCommit:
		if value != nil {
			return *value, true
		}
	}
	return acousticelements.AudioCommit{}, false
}

func foregroundCancel(payload any) (cognitionelements.Cancel, bool) {
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

func foregroundTruncation(payload any) (legacy.Truncation, bool) {
	switch value := payload.(type) {
	case legacy.Truncation:
		return value, true
	case *legacy.Truncation:
		if value != nil {
			return *value, true
		}
	}
	return legacy.Truncation{}, false
}

func cloneTrajectoryItemForMeeting(source trajectory.Item) trajectory.Item {
	result := source
	result.CausalParentIDs = slices.Clone(source.CausalParentIDs)
	result.ProviderState = slices.Clone(source.ProviderState)
	if source.ToolCall != nil {
		value := *source.ToolCall
		value.Arguments = slices.Clone(source.ToolCall.Arguments)
		result.ToolCall = &value
	}
	if source.ToolCallDerivation != nil {
		value := *source.ToolCallDerivation
		value.Rewrites = slices.Clone(source.ToolCallDerivation.Rewrites)
		result.ToolCallDerivation = &value
	}
	if source.ToolResult != nil {
		value := *source.ToolResult
		value.Output = slices.Clone(source.ToolResult.Output)
		result.ToolResult = &value
	}
	if source.Observation != nil {
		value := *source.Observation
		value.Media = slices.Clone(source.Observation.Media)
		result.Observation = &value
	}
	return result
}

var _ modelelements.Session = (*foregroundSession)(nil)
var _ legacy.Sink = (*foregroundSession)(nil)
var _ legacy.SpeechReservationSink = (*foregroundSession)(nil)
