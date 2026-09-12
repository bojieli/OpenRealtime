package scenarioconversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type contentRequest struct {
	pending *pendingContent
	port    element.OutputPort
	payload any
}

func (session *session) Update(ctx context.Context, settings legacy.Settings) error {
	if err := usableContext(ctx, "update scenario conversation session"); err != nil {
		return err
	}
	settings = legacy.CloneSettings(settings)
	invocation, err := session.invocationForSettings(settings)
	if err != nil {
		return err
	}
	invocationBytes, err := json.Marshal(invocation)
	if err != nil {
		return fmt.Errorf("fingerprint scenario conversation invocation: %w", err)
	}
	invocationHash := sha256.Sum256(invocationBytes)
	invocationDigest := "sha256:" + hex.EncodeToString(invocationHash[:])

	// Revisions are allocated before crossing the graph boundary. A caller
	// whose context expires after delivery must not cause the next update to
	// replay the same revision against graph-owned state.
	session.updateMu.Lock()
	defer session.updateMu.Unlock()
	session.settingsMu.Lock()
	session.settingsRevision++
	revision := session.settingsRevision
	session.settingsMu.Unlock()

	itemID, sequence := session.nextEnvelopeIdentity("update")
	pending := &pendingOperation{
		operation: "update", revision: revision, digest: invocationDigest,
		result: make(chan operationAck, 1),
	}
	if err := session.registerOperation(itemID, pending); err != nil {
		return err
	}
	if err := sendExact(ctx, session.ports.update, element.Envelope{
		Type: session.ports.update.Type(), ItemID: itemID, SessionID: session.sessionID,
		SourceID: "gateway", OpportunityID: itemID, Sequence: sequence,
		TraceID: itemID, CancellationScope: session.sessionID,
		Payload: policyelements.SessionInvocationUpdate{
			Revision: revision, Invocation: invocation, Contract: contractForSettings(settings),
		},
	}, "send scenario conversation session update"); err != nil {
		session.removeOperation(itemID, pending)
		return err
	}
	if _, err := session.awaitOperation(ctx, itemID, pending); err != nil {
		return err
	}
	session.settingsMu.Lock()
	session.settings = legacy.CloneSettings(settings)
	session.settingsMu.Unlock()
	session.bundle.presentation.Update(settings)
	return nil
}

// contractForSettings is the session's own instruction to the agent, with
// the room's default when the client gave none.
func contractForSettings(settings legacy.Settings) string {
	if strings.TrimSpace(settings.Instruction) == "" {
		return "You are a realtime assistant in a conversation room. Follow the user's instructions about when to speak. You can hear audio and see images they explicitly share. Answer briefly."
	}
	return settings.Instruction
}

func (session *session) invocationForSettings(settings legacy.Settings) (continuation.Invocation, error) {
	if err := validateInitialSettings(settings, session.config); err != nil {
		return continuation.Invocation{}, err
	}
	if !utf8.ValidString(settings.Instruction) ||
		len(settings.Instruction) > maximumAdapterTextBytes {
		return continuation.Invocation{}, fmt.Errorf(
			"scenario conversation instruction must be non-empty valid UTF-8 no larger than %d bytes",
			maximumAdapterTextBytes,
		)
	}
	instruction := contractForSettings(settings)
	if policy := session.config.ContinuationInstruction; policy != "" {
		const separator = "\n\n"
		if len(instruction) > maximumAdapterTextBytes-len(separator)-len(policy) {
			return continuation.Invocation{}, fmt.Errorf(
				"scenario conversation composed instruction must be no larger than %d bytes",
				maximumAdapterTextBytes,
			)
		}
		instruction += separator + policy
	}
	declared := make(map[string]ToolDeclaration, len(session.config.Tools))
	for _, tool := range session.config.Tools {
		declared[tool.Name] = tool
	}
	seen := make(map[string]struct{}, len(settings.Tools))
	tools := make([]continuation.ToolDefinition, 0, len(settings.Tools))
	capabilities := make([]continuation.Capability, 0, len(settings.Tools))
	for _, selected := range settings.Tools {
		if selected.Dispatcher != nil {
			return continuation.Invocation{}, fmt.Errorf(
				"scenario conversation tool %q carries a transport-local dispatcher", selected.Name,
			)
		}
		configured, found := declared[selected.Name]
		if !found {
			return continuation.Invocation{}, fmt.Errorf(
				"scenario conversation tool %q is not selected by the application", selected.Name,
			)
		}
		if _, duplicate := seen[selected.Name]; duplicate {
			return continuation.Invocation{}, fmt.Errorf(
				"scenario conversation tool %q is repeated", selected.Name,
			)
		}
		if !sameToolDeclaration(selected, configured) {
			return continuation.Invocation{}, fmt.Errorf(
				"scenario conversation tool %q drifted from its exact application declaration", selected.Name,
			)
		}
		seen[selected.Name] = struct{}{}
		tools = append(tools, continuation.ToolDefinition{
			Name: configured.Name, Description: configured.Description,
			Parameters: slices.Clone(configured.Parameters), Background: configured.Background,
		})
		capabilities = append(capabilities, continuation.Capability{
			Name: configured.Name, Description: configured.Description, Available: true,
			ExecutionPhase:       string(session.config.Model.Descriptor.Phase),
			ConfirmationRequired: configured.Confirm != "never",
		})
	}
	return continuation.Invocation{
		Instruction: instruction, Capabilities: capabilities, Tools: tools,
		MaxOutputTokens: session.config.MaxOutputTokens,
	}, nil
}

func (session *session) Audio(ctx context.Context, frame perception.Frame) error {
	if err := usableContext(ctx, "send scenario conversation audio"); err != nil {
		return err
	}
	if frame.Kind != perception.FrameAudio || frame.Source != SourceMicrophone {
		return errors.New("scenario conversation audio input must be a microphone audio frame")
	}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("scenario conversation microphone frame: %w", err)
	}
	if frame.CapturedNS == 0 {
		return errors.New("scenario conversation microphone frame requires a positive capture timestamp")
	}
	if len(frame.PCM16LE) > maximumAdapterAudioBytes || frame.SampleRateHz > maximumAdapterSampleRate {
		return errors.New("scenario conversation microphone frame exceeds the exact graph audio bounds")
	}
	frame = cloneFrame(frame)
	select {
	case <-session.audioReady:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	session.audioSendMu.Lock()
	defer session.audioSendMu.Unlock()
	session.audioMu.Lock()
	if session.audioCaptured != 0 && frame.CapturedNS <= session.audioCaptured {
		session.audioMu.Unlock()
		return errors.New("scenario conversation microphone capture timestamps must strictly increase")
	}
	streamID := session.audioStreamID(session.audioStream)
	session.audioMu.Unlock()
	itemID, sequence := session.nextEnvelopeIdentity("audio")
	pending := &pendingAudio{
		itemID: itemID, streamID: streamID, result: make(chan error, 1),
	}
	session.audioMu.Lock()
	if len(session.audioOps) >= maximumAdapterMemory {
		session.audioMu.Unlock()
		return errors.New("scenario conversation pending audio bound reached")
	}
	session.audioOps[itemID] = pending
	session.audioMu.Unlock()
	defer func() {
		session.audioMu.Lock()
		if session.audioOps[itemID] == pending {
			delete(session.audioOps, itemID)
		}
		session.audioMu.Unlock()
	}()
	if err := sendExact(ctx, session.ports.audio, element.Envelope{
		Type: session.ports.audio.Type(), ItemID: itemID, SessionID: session.sessionID,
		SourceID: streamID, OpportunityID: streamID, Sequence: sequence,
		CaptureNS: frame.CapturedNS, TraceID: itemID, CancellationScope: streamID,
		Payload: acousticelements.InputFrame{StreamID: streamID, Frame: frame},
	}, "send scenario conversation audio"); err != nil {
		return err
	}
	session.audioMu.Lock()
	session.audioCaptured = frame.CapturedNS
	session.audioMu.Unlock()
	select {
	case err := <-pending.result:
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Video enters the same bounded retained-image and trajectory path as an image
// attachment. The same explicit evaluation used by the visual scenario lets
// the interaction policy decide whether new visual evidence warrants speech.
// One sample per source per second bounds work independently of preview FPS.
func (session *session) Video(ctx context.Context, frame perception.Frame) error {
	if err := usableContext(ctx, "send scenario conversation video"); err != nil {
		return err
	}
	if !session.config.Model.Descriptor.Vision {
		return legacy.ErrUnsupported
	}
	if frame.Kind != perception.FrameImage || (frame.Source != "camera" && frame.Source != "screen" && frame.Source != "browser") {
		return errors.New("scenario video requires a camera, screen, or browser image frame")
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.CapturedNS == 0 {
		return errors.New("scenario video requires a capture timestamp")
	}
	session.videoMu.Lock()
	defer session.videoMu.Unlock()
	if session.videoCaptured == nil {
		session.videoCaptured = make(map[string]uint64)
	}
	previous := session.videoCaptured[frame.Source]
	if previous != 0 && frame.CapturedNS <= previous {
		return errors.New("video capture timestamps must increase per source")
	}
	if previous != 0 && frame.CapturedNS-previous < 1_000_000_000 {
		return nil
	}
	session.videoCaptured[frame.Source] = frame.CapturedNS
	if err := session.text(ctx, legacy.TextInput{
		ItemID: session.nextItemID("video-" + frame.Source), Role: "user",
		Images: []legacy.Image{{Bytes: frame.Image, MIMEType: frame.MIMEType, Width: frame.Width, Height: frame.Height}},
	}, "Current shared "+frame.Source+" frame.", frame.CapturedNS); err != nil {
		return err
	}
	// Visual updates remain context while speech is in flight. An explicit
	// create cannot execute keep-speaking/stop-speaking controls; those are
	// owned by the graph's concurrent audio/interaction lane.
	err := session.CreateResponse(ctx)
	var refusal *semanticAdmissionError
	if errors.As(err, &refusal) && refusal.outcome.Kind == policyelements.SemanticAdmissionRefused &&
		refusal.outcome.Code == "speech_in_flight" {
		return nil // The frame was retained; the graph owns in-flight speech control.
	}
	return err
}

func (session *session) Text(ctx context.Context, input legacy.TextInput) error {
	return session.text(ctx, input, "", 0)
}

func (session *session) text(ctx context.Context, input legacy.TextInput, caption string, capturedNS uint64) error {
	if err := usableContext(ctx, "send scenario conversation message"); err != nil {
		return err
	}
	input = cloneTextInput(input)
	if !canonicalIdentity(input.ItemID) || input.Role != "user" {
		return errors.New("scenario conversation message requires a canonical item ID and user role")
	}
	text := strings.TrimSpace(input.Text)
	if text == "" && len(input.Images) == 0 {
		return errors.New("scenario conversation message requires text or a still-image attachment")
	}
	if len(input.Text) > maximumAdapterTextBytes || !utf8.ValidString(input.Text) {
		return fmt.Errorf("scenario conversation message text must be valid UTF-8 no larger than %d bytes",
			maximumAdapterTextBytes)
	}
	if len(input.Images) != 0 && !session.config.Model.Descriptor.Vision {
		return errors.New("scenario conversation selected model does not admit still-image attachments")
	}
	if len(input.Images) > session.config.Media.MaxItems {
		return errors.New("scenario conversation message images exceed the exact retained-media item bound")
	}
	totalImageBytes := 0
	for index := range input.Images {
		image := &input.Images[index]
		canonicalMIME, mimeErr := canonicalMIMEType(image.MIMEType)
		if mimeErr != nil || len(image.Bytes) == 0 || len(image.Bytes) > maximumAdapterImageBytes ||
			len(image.Bytes) > session.config.Media.MaxItemBytes || image.Width <= 0 || image.Height <= 0 {
			return fmt.Errorf("scenario conversation image %d has invalid bytes, MIME type, or dimensions", index)
		}
		image.MIMEType = canonicalMIME
		totalImageBytes += len(image.Bytes)
		if totalImageBytes > session.config.Media.MaxBytes {
			return errors.New("scenario conversation message images exceed the exact retained-media byte bound")
		}
	}
	if err := session.rememberMessage(input.ItemID); err != nil {
		return err
	}

	requests := make([]contentRequest, 0, 1+len(input.Images))
	if text != "" {
		requestID := session.contentIdentifier(input.ItemID, "text", 0)
		streamID := session.contentIdentifier(input.ItemID, "text-stream", 0)
		requests = append(requests, contentRequest{
			pending: &pendingContent{
				requestID: requestID, streamID: streamID, result: make(chan error, 1),
				handleReady: true,
			},
			port: session.ports.text,
			payload: ingresselements.UserText{
				ContentID: requestID, StreamID: streamID, Text: input.Text,
				StableText: input.Text, Revision: 1, Final: true,
			},
		})
	}
	for index, image := range input.Images {
		requestID := session.contentIdentifier(input.ItemID, "image", index)
		streamID := session.contentIdentifier(input.ItemID, "image-stream", index)
		digest := sha256.Sum256(image.Bytes)
		requests = append(requests, contentRequest{
			pending: &pendingContent{
				requestID: requestID, streamID: streamID, result: make(chan error, 1),
				requiresHandle: true,
			},
			port: session.ports.image,
			payload: ingresselements.UserImage{
				ContentID: requestID, StreamID: streamID, MIMEType: strings.TrimSpace(image.MIMEType),
				Content: slices.Clone(image.Bytes), SHA256: "sha256:" + hex.EncodeToString(digest[:]),
				Width: image.Width, Height: image.Height, SourceRevision: 1, Caption: caption, CapturedNS: capturedNS,
			},
		})
	}
	if err := session.registerContent(requests); err != nil {
		return err
	}
	defer session.finishContent(requests)
	for _, request := range requests {
		_, sequence := session.nextEnvelopeIdentity("content")
		if err := sendExact(ctx, request.port, element.Envelope{
			Type: request.port.Type(), ItemID: request.pending.requestID, SessionID: session.sessionID,
			SourceID: request.pending.streamID, OpportunityID: input.ItemID, Sequence: sequence,
			TraceID: request.pending.requestID, CancellationScope: request.pending.streamID,
			Payload: request.payload,
		}, "send scenario conversation message content"); err != nil {
			return err
		}
	}
	var joined error
	for _, request := range requests {
		select {
		case err := <-request.pending.result:
			joined = errors.Join(joined, err)
		case <-ctx.Done():
			return errors.Join(joined, context.Cause(ctx))
		}
	}
	return joined
}

func (session *session) ToolResult(ctx context.Context, result trajectory.ToolResult) error {
	if err := usableContext(ctx, "return scenario conversation tool result"); err != nil {
		return err
	}
	result = cloneToolResult(result)
	if err := validateToolResult(result); err != nil {
		return err
	}
	session.activityMu.Lock()
	active, found := session.calls[result.CallID]
	session.activityMu.Unlock()
	if !found || active.call.Name != result.Name || !canonicalIdentity(active.runID) {
		return fmt.Errorf("scenario conversation client result %q has no exact emitted action", result.CallID)
	}
	itemID, sequence := session.nextEnvelopeIdentity("tool_result")
	pending := &pendingOperation{
		operation: "tool_result", generation: active.runID, callID: result.CallID,
		name: result.Name, digest: actionelements.ClientToolResultDigest(result),
		result: make(chan operationAck, 1),
	}
	if err := session.registerOperation(itemID, pending); err != nil {
		return err
	}
	if err := sendExact(ctx, session.ports.toolResult, element.Envelope{
		Type: session.ports.toolResult.Type(), ItemID: itemID, SessionID: session.sessionID,
		RunID: active.runID, SourceID: "gateway", OpportunityID: result.CallID,
		Sequence: sequence, TraceID: itemID, CancellationScope: active.runID,
		Payload: result,
	}, "send scenario conversation client tool result"); err != nil {
		session.removeOperation(itemID, pending)
		return err
	}
	_, err := session.awaitOperation(ctx, itemID, pending)
	return err
}

func (*session) CommitAudio(context.Context) error { return legacy.ErrUnsupported }

func (session *session) CreateResponse(ctx context.Context) error {
	if err := usableContext(ctx, "create scenario conversation response"); err != nil {
		return err
	}
	committedContext, err := session.responseCreateContext(ctx)
	if err != nil {
		return fmt.Errorf("bind scenario conversation response creation: %w", err)
	}
	itemID, sequence := session.nextEnvelopeIdentity("create")
	pending := &pendingOperation{operation: "create", result: make(chan operationAck, 1)}
	if err := session.registerOperation(itemID, pending); err != nil {
		return err
	}
	if err := sendExact(ctx, session.ports.create, element.Envelope{
		Type: session.ports.create.Type(), ItemID: itemID, SessionID: session.sessionID,
		SourceID: "gateway", OpportunityID: itemID, Sequence: sequence,
		TraceID: itemID, CancellationScope: session.sessionID,
		Payload: policyelements.ResponseCreate{
			ResponseID: itemID, ExpectedContextVersion: &committedContext.Prefix.Version,
			ExpectedContextItemID: committedContext.StateItemID,
			CommittedContext:      &committedContext,
		},
	}, "send scenario conversation response creation"); err != nil {
		session.removeOperation(itemID, pending)
		return err
	}
	_, err = session.awaitOperation(ctx, itemID, pending)
	return err
}

// responseCreateContext establishes a causal barrier between the durable
// trajectory store and the independently scheduled model-context lane. The
// policy trigger then carries this exact state identity, so cognition waits
// for the matching snapshot instead of sampling whichever snapshot it last
// happened to process.
func (session *session) responseCreateContext(
	ctx context.Context,
) (stateelements.CommittedContext, error) {
	required := session.bundle.store.Snapshot().Version
	for {
		session.contentMu.Lock()
		if session.snapshotItemID != "" && session.snapshotVersion >= required {
			version, itemID := session.snapshotVersion, session.snapshotItemID
			session.contentMu.Unlock()
			current := session.bundle.store.Snapshot()
			prefix, err := trajectory.IdentifyPrefix(current, version)
			if err != nil {
				return stateelements.CommittedContext{}, err
			}
			return stateelements.CommittedContext{Prefix: prefix, StateItemID: itemID}, nil
		}
		changed := session.snapshotChanged
		session.contentMu.Unlock()
		select {
		case <-ctx.Done():
			return stateelements.CommittedContext{}, context.Cause(ctx)
		case <-changed:
		}
	}
}

func (session *session) Cancel(ctx context.Context, reason string) error {
	if err := usableContext(ctx, "cancel scenario conversation response"); err != nil {
		return err
	}
	reason = boundedAdapterReason(reason)
	session.activityMu.Lock()
	generations := make([]string, 0, len(session.active))
	runs := make(map[string]struct{}, len(session.active)+len(session.pendingSpeech)+len(session.speechRuns)+len(session.calls))
	for generationID := range session.active {
		generations = append(generations, generationID)
		runs[generationID] = struct{}{}
	}
	for runID := range session.pendingSpeech {
		runs[runID] = struct{}{}
	}
	calls := make([]activeClientCall, 0, len(session.calls))
	for _, call := range session.calls {
		calls = append(calls, call)
		runs[call.runID] = struct{}{}
	}
	playback := make([]playbackReceiptState, 0, len(session.playback))
	for _, state := range session.playback {
		if state.active && !state.terminal {
			playback = append(playback, state)
			runs[state.runID] = struct{}{}
		}
	}
	for runID, remaining := range session.speechRuns {
		if remaining > 0 {
			runs[runID] = struct{}{}
		}
	}
	session.activityMu.Unlock()
	if len(runs) == 0 && len(generations) == 0 && len(calls) == 0 && len(playback) == 0 {
		return nil
	}
	sort.Strings(generations)
	sort.Slice(calls, func(left, right int) bool {
		if calls[left].runID != calls[right].runID {
			return calls[left].runID < calls[right].runID
		}
		return calls[left].call.CallID < calls[right].call.CallID
	})
	sort.Slice(playback, func(left, right int) bool {
		return playback[left].utterance.ID < playback[right].utterance.ID
	})
	runIDs := make([]string, 0, len(runs))
	for runID := range runs {
		runIDs = append(runIDs, runID)
	}
	sort.Strings(runIDs)

	// Revoke the graph's complete queued speech horizon before waiting for a
	// policy acknowledgement. Segmentation retains exact utterance IDs after
	// TextEnd; direct playback/TTS interrupts cover an utterance whose receipt
	// has already crossed the presentation boundary.
	var cancelErr error
	segmentationParents := make(map[string]string, len(runIDs))
	for _, runID := range runIDs {
		itemID, sequence := session.nextEnvelopeIdentity("segmentation_cancel")
		err := sendExact(ctx, session.ports.segmentationCancel, element.Envelope{
			Type: session.ports.segmentationCancel.Type(), ItemID: itemID,
			SessionID: session.sessionID, RunID: runID, Sequence: sequence,
			TraceID: itemID, CancellationScope: runID,
			Payload: cognitionelements.Cancel{RunID: runID, Reason: reason},
		}, "send scenario conversation segmentation cancellation")
		if err == nil {
			segmentationParents[runID] = itemID
		}
		cancelErr = errors.Join(cancelErr, err)
	}
	for _, state := range playback {
		for _, target := range []struct {
			kind string
			port element.OutputPort
		}{
			{kind: "playback_cancel", port: session.ports.playbackCancel},
			{kind: "tts_cancel", port: session.ports.ttsCancel},
		} {
			itemID, sequence := session.nextEnvelopeIdentity(target.kind)
			envelope := element.Envelope{
				Type: target.port.Type(), ItemID: itemID, SessionID: session.sessionID,
				RunID: state.runID, OpportunityID: state.utterance.ID,
				Sequence: sequence, TraceID: itemID,
				CancellationScope: state.utterance.ID,
				Payload: speechelements.Cancel{
					UtteranceID: state.utterance.ID, Reason: reason,
				},
			}
			if parent := segmentationParents[state.runID]; parent != "" {
				envelope.CausalParents = []string{parent}
			}
			cancelErr = errors.Join(cancelErr, sendExact(ctx, target.port, envelope,
				"send scenario conversation "+strings.ReplaceAll(target.kind, "_", " ")))
		}
	}
	for _, generationID := range generations {
		cancelErr = errors.Join(cancelErr, session.cancelGeneration(ctx, generationID, reason))
	}
	for _, active := range calls {
		itemID, sequence := session.nextEnvelopeIdentity("action_cancel")
		cancelErr = errors.Join(cancelErr, sendExact(ctx, session.ports.actionCancel, element.Envelope{
			Type: session.ports.actionCancel.Type(), ItemID: itemID, SessionID: session.sessionID,
			RunID: active.runID, OpportunityID: active.call.CallID,
			Sequence: sequence, TraceID: itemID,
			CancellationScope: active.runID,
			Payload:           actionelements.Interrupt{CallID: active.call.CallID, Reason: reason},
		}, "send scenario conversation action cancellation"))
	}
	return cancelErr
}

func (session *session) cancelGeneration(ctx context.Context, generationID, reason string) error {
	itemID, sequence := session.nextEnvelopeIdentity("generation_cancel")
	pending := &pendingOperation{
		operation: "cancel", generation: generationID, result: make(chan operationAck, 1),
	}
	if err := session.registerOperation(itemID, pending); err != nil {
		return err
	}
	if err := sendExact(ctx, session.ports.generationCancel, element.Envelope{
		Type: session.ports.generationCancel.Type(), ItemID: itemID, SessionID: session.sessionID,
		RunID: generationID, Sequence: sequence, TraceID: itemID,
		CancellationScope: generationID,
		Payload:           policyelements.GenerationCancel{GenerationID: generationID, Reason: reason},
	}, "send scenario conversation policy cancellation"); err != nil {
		session.removeOperation(itemID, pending)
		return err
	}
	modelItemID, modelSequence := session.nextEnvelopeIdentity("model_cancel")
	if err := sendExact(ctx, session.ports.modelCancel, element.Envelope{
		Type: session.ports.modelCancel.Type(), ItemID: modelItemID, SessionID: session.sessionID,
		RunID: generationID, Sequence: modelSequence, TraceID: modelItemID,
		CancellationScope: generationID,
		CausalParents:     []string{itemID}, Payload: cognitionelements.Cancel{RunID: generationID, Reason: reason},
	}, "send scenario conversation model cancellation"); err != nil {
		session.removeOperation(itemID, pending)
		return err
	}
	_, err := session.awaitOperation(ctx, itemID, pending)
	return err
}

func (*session) Truncate(context.Context, legacy.Truncation) error { return legacy.ErrUnsupported }

func (session *session) Trajectory() trajectory.Snapshot { return session.bundle.store.Snapshot() }

func (session *session) Close(_ context.Context, cause error) error {
	session.closeOnce.Do(func() {
		if cause == nil {
			cause = errors.New("scenario conversation session closed")
		}
		session.closeErr = session.bundle.Close(cause)
		session.audioReadyOnce.Do(func() { close(session.audioReady) })
		session.audioMu.Lock()
		audio := make([]*pendingAudio, 0, len(session.audioOps))
		for key, pending := range session.audioOps {
			delete(session.audioOps, key)
			audio = append(audio, pending)
		}
		session.audioMu.Unlock()
		for _, pending := range audio {
			select {
			case pending.result <- cause:
			default:
			}
		}
		session.operationMu.Lock()
		operations := make([]*pendingOperation, 0, len(session.pendingOps))
		for key, pending := range session.pendingOps {
			delete(session.pendingOps, key)
			operations = append(operations, pending)
		}
		session.operationMu.Unlock()
		for _, pending := range operations {
			select {
			case pending.result <- operationAck{err: cause}:
			default:
			}
		}
		session.contentMu.Lock()
		contents := make([]*pendingContent, 0, len(session.contentAcks))
		for key, pending := range session.contentAcks {
			delete(session.contentAcks, key)
			contents = append(contents, pending)
		}
		session.contentMu.Unlock()
		for _, pending := range contents {
			select {
			case pending.result <- cause:
			default:
			}
		}
	})
	return session.closeErr
}

func (session *session) registerOperation(itemID string, pending *pendingOperation) error {
	session.operationMu.Lock()
	defer session.operationMu.Unlock()
	if len(session.pendingOps) >= maximumAdapterMemory {
		return errors.New("scenario conversation pending operation bound reached")
	}
	if session.pendingOps[itemID] != nil {
		return fmt.Errorf("scenario conversation operation %q is already pending", itemID)
	}
	pending.requestID = itemID
	session.pendingOps[itemID] = pending
	return nil
}

func (session *session) removeOperation(itemID string, pending *pendingOperation) {
	session.operationMu.Lock()
	if session.pendingOps[itemID] == pending {
		delete(session.pendingOps, itemID)
	}
	session.operationMu.Unlock()
}

func (session *session) awaitOperation(
	ctx context.Context, itemID string, pending *pendingOperation,
) (string, error) {
	select {
	case result := <-pending.result:
		return result.generationID, result.err
	case <-ctx.Done():
		session.operationMu.Lock()
		if session.pendingOps[itemID] == pending {
			if pending.completed {
				session.operationMu.Unlock()
				result := <-pending.result
				return result.generationID, result.err
			}
			pending.abandoned = true
		}
		session.operationMu.Unlock()
		return "", context.Cause(ctx)
	}
}

func (session *session) rememberMessage(itemID string) error {
	session.contentMu.Lock()
	defer session.contentMu.Unlock()
	if _, duplicate := session.seenContent[itemID]; duplicate {
		return fmt.Errorf("scenario conversation message item %q is duplicated", itemID)
	}
	session.seenContent[itemID] = struct{}{}
	session.seenContentOrder = append(session.seenContentOrder, itemID)
	for len(session.seenContentOrder) > maximumAdapterMemory {
		oldest := session.seenContentOrder[0]
		session.seenContentOrder = session.seenContentOrder[1:]
		delete(session.seenContent, oldest)
	}
	return nil
}

func (session *session) registerContent(requests []contentRequest) error {
	session.contentMu.Lock()
	defer session.contentMu.Unlock()
	if len(session.contentAcks)+len(requests) > maximumAdapterMemory {
		return errors.New("scenario conversation pending content bound reached")
	}
	for _, request := range requests {
		if session.contentAcks[request.pending.streamID] != nil {
			return fmt.Errorf("scenario conversation content stream %q is already pending", request.pending.streamID)
		}
	}
	for _, request := range requests {
		session.contentAcks[request.pending.streamID] = request.pending
	}
	return nil
}

func (session *session) finishContent(requests []contentRequest) {
	session.contentMu.Lock()
	defer session.contentMu.Unlock()
	for _, request := range requests {
		if session.contentAcks[request.pending.streamID] == request.pending {
			delete(session.contentAcks, request.pending.streamID)
		}
		if _, exists := session.terminalContent[request.pending.streamID]; !exists {
			session.terminalContent[request.pending.streamID] = struct{}{}
			session.terminalContentIDs = append(session.terminalContentIDs, request.pending.streamID)
		}
		if _, exists := session.terminalRequests[request.pending.requestID]; !exists {
			session.terminalRequests[request.pending.requestID] = struct{}{}
			session.terminalRequestIDs = append(session.terminalRequestIDs, request.pending.requestID)
		}
	}
	for len(session.terminalContentIDs) > maximumAdapterMemory {
		oldest := session.terminalContentIDs[0]
		session.terminalContentIDs = session.terminalContentIDs[1:]
		delete(session.terminalContent, oldest)
	}
	for len(session.terminalRequestIDs) > maximumAdapterMemory {
		oldest := session.terminalRequestIDs[0]
		session.terminalRequestIDs = session.terminalRequestIDs[1:]
		delete(session.terminalRequests, oldest)
	}
}

func (session *session) nextEnvelopeIdentity(kind string) (string, uint64) {
	sequence := session.sequence.Add(1)
	return fmt.Sprintf("sc_%s_%d", kind, sequence), sequence
}

func (session *session) audioStreamID(revision uint64) string {
	return session.derivedIdentifier("audio", session.sessionID, fmt.Sprint(revision))
}

func (session *session) contentIdentifier(itemID, kind string, index int) string {
	return session.derivedIdentifier(kind, session.sessionID, itemID, fmt.Sprint(index))
}

func (*session) derivedIdentifier(kind string, values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprintf(hash, "%d:", len(value))
		_, _ = hash.Write([]byte(value))
	}
	return kind + ":sha256:" + hex.EncodeToString(hash.Sum(nil))
}
