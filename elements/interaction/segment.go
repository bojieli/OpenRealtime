package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/bojieli/OpenRealtime/adapters/bysentence"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	"github.com/bojieli/OpenRealtime/elements/speech"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
)

type segmentPreparedTextFactory struct{}

var (
	_ element.Factory         = segmentPreparedTextFactory{}
	_ element.ConfigValidator = segmentPreparedTextFactory{}
)

func (segmentPreparedTextFactory) Descriptor() element.Descriptor {
	return SegmentPreparedTextDescriptor()
}

func (segmentPreparedTextFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeSegmentConfig(source)
	return err
}

func (segmentPreparedTextFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeSegmentConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("interaction.SegmentPreparedText %s config: %w", mount.InstanceID, err)
	}
	textInput, err := mount.Ports.Input("text")
	if err != nil {
		return nil, err
	}
	terminalInput, err := mount.Ports.Input("terminal")
	if err != nil {
		return nil, err
	}
	timeoutInput, err := mount.Ports.Input("timeout")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	segmentsOutput, err := mount.Ports.Output("segments")
	if err != nil {
		return nil, err
	}
	modelCancelOutput, err := mount.Ports.Output("model_cancel")
	if err != nil {
		return nil, err
	}
	speechCancelOutput, err := mount.Ports.Output("speech_cancel")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &segmentPreparedTextRunner{
		instance:           mount.InstanceID,
		config:             config,
		textInput:          textInput,
		terminalInput:      terminalInput,
		timeoutInput:       timeoutInput,
		cancelInput:        cancelInput,
		segmentsOutput:     segmentsOutput,
		modelCancelOutput:  modelCancelOutput,
		speechCancelOutput: speechCancelOutput,
		outcomeOutput:      outcomeOutput,
		terminalRuns:       make(map[string]struct{}, config.TerminalMemory),
		terminalSegments:   make(map[string][]string, config.TerminalMemory),
		resolution:         mount.Resolution,
	}, nil
}

type segmentInputKind uint8

const (
	segmentTextInput segmentInputKind = iota
	segmentTerminalInput
	segmentTimeoutInput
	segmentCancelInput
)

type segmentInput struct {
	kind     segmentInputKind
	envelope element.Envelope
}

type preparedRun struct {
	id         string
	begin      element.Envelope
	expected   uint64
	spokeOver  bool
	buffer     string
	totalBytes int
	segments   []string
	// silenced records that a control token asked this run to say nothing.
	// It is a property of the run, not of one delta: the token can arrive in
	// the last chunk, after earlier prose has already been buffered.
	silenced bool
}

type segmentPreparedTextRunner struct {
	instance string
	config   SegmentPreparedTextConfig

	textInput     element.InputPort
	terminalInput element.InputPort
	timeoutInput  element.InputPort
	cancelInput   element.InputPort

	segmentsOutput     element.OutputPort
	modelCancelOutput  element.OutputPort
	speechCancelOutput element.OutputPort
	outcomeOutput      element.OutputPort

	active           *preparedRun
	terminalRuns     map[string]struct{}
	terminalSegments map[string][]string
	terminalOrder    []string
	resolution       element.ResolutionReporter
}

func (runner *segmentPreparedTextRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := liveidentity.Report(runner.resolution, liveidentity.Artifact{ID: "builtin://openrealtime/elements/interaction.SegmentPreparedText", Revision: "implementation:3"}, nil); err != nil {
		return err
	}
	inputs := make(chan segmentInput)
	failures := make(chan error, 4)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		kind     segmentInputKind
		port     element.InputPort
		variadic bool
	}{
		{segmentTextInput, runner.textInput, false},
		{segmentTerminalInput, runner.terminalInput, true},
		{segmentTimeoutInput, runner.timeoutInput, false},
		{segmentCancelInput, runner.cancelInput, false},
	} {
		receivers.Add(1)
		go receiveSegmentInput(ctx, source.kind, source.port, source.variadic,
			inputs, failures, &receivers)
	}
	defer func() {
		cancel(nil)
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			cancel(err)
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case segmentTextInput:
				err = runner.acceptText(ctx, input.envelope)
			case segmentTerminalInput:
				err = runner.acceptTerminal(ctx, input.envelope)
			case segmentTimeoutInput:
				err = runner.acceptTimeout(ctx, input.envelope)
			case segmentCancelInput:
				err = runner.acceptCancel(ctx, input.envelope)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *segmentPreparedTextRunner) acceptText(
	ctx context.Context, envelope element.Envelope,
) error {
	delta, valid := preparedTextPayload(envelope.Payload)
	runID := strings.TrimSpace(envelope.RunID)
	if !valid {
		return runner.refuseText(ctx, envelope, runID, "invalid_payload",
			fmt.Sprintf("prepared text payload has type %T", envelope.Payload))
	}
	if runID == "" {
		return runner.refuseText(ctx, envelope, "", "missing_run_id",
			"prepared text requires an envelope run ID")
	}
	if runner.isTerminal(runID) {
		return nil
	}
	if runner.active == nil {
		if err := validateSafePreparedDelta(delta, 0, false); err != nil {
			return runner.refuseText(ctx, envelope, runID, "invalid_framing", err.Error())
		}
		runner.active = &preparedRun{
			id: runID, begin: envelope.Clone(), expected: 1, spokeOver: delta.SpokeOver,
		}
		return nil
	}
	if runner.active.id != runID {
		return runner.refuseText(ctx, envelope, runID, "interleaved_run",
			fmt.Sprintf("run %q arrived while run %q was open", runID, runner.active.id))
	}
	run := runner.active
	if delta.SpokeOver != run.spokeOver {
		return runner.failActive(ctx, envelope, OutcomeFailed, "inconsistent_spoke_over",
			"prepared text spoke_over changed within one framed stream", true)
	}
	if err := validateSafePreparedDelta(delta, run.expected, true); err != nil {
		code := "invalid_framing"
		if delta.Index != run.expected {
			code = "invalid_index"
		}
		return runner.failActive(ctx, envelope, OutcomeFailed, code, err.Error(), true)
	}
	run.expected++
	switch delta.Boundary {
	case cognitionelements.TextChunk:
		_, err := runner.appendSafeText(ctx, envelope, run, delta.Text)
		return err
	case cognitionelements.TextEnd:
		active, err := runner.appendSafeText(ctx, envelope, run, delta.Text)
		if err != nil || !active {
			return err
		}
		if delta.Interrupted {
			return runner.failActive(ctx, envelope, OutcomeFailed, "source_interrupted",
				"prepared text source ended interrupted", false)
		}
		if run.silenced {
			// Reported, not absorbed. A turn that chose to be silent and a
			// turn that produced nothing are different facts, and only the
			// second is worth investigating.
			run.buffer = ""
			return runner.completeSilenced(ctx, envelope)
		}
		if strings.TrimSpace(run.buffer) != "" {
			if len(strings.TrimSpace(run.buffer)) > runner.config.MaxSegmentBytes {
				return runner.failActive(ctx, envelope, OutcomeFailed, "segment_too_large",
					fmt.Sprintf("safe speech segment exceeds %d bytes", runner.config.MaxSegmentBytes), false)
			}
			if err := runner.publishSegment(ctx, envelope, strings.TrimSpace(run.buffer)); err != nil {
				return err
			}
			run.buffer = ""
		}
		return runner.completeActive(ctx, envelope)
	case cognitionelements.TextBegin:
		return runner.failActive(ctx, envelope, OutcomeFailed, "duplicate_begin",
			"prepared run emitted a second begin", true)
	default:
		return runner.failActive(ctx, envelope, OutcomeFailed, "invalid_boundary",
			fmt.Sprintf("unknown prepared text boundary %q", delta.Boundary), true)
	}
}

func (runner *segmentPreparedTextRunner) appendSafeText(
	ctx context.Context, cause element.Envelope, run *preparedRun, text string,
) (bool, error) {
	// Before anything measures, buffers, or splits this text. A control token
	// is an instruction to the runtime, never a word to be spoken, so it is
	// removed on the way in and the request it carried is remembered on the
	// run. <wait> asks for silence, and the safe reading of a token whose
	// entire purpose is silence is silence: a run that carried one publishes
	// no further speech, rather than speaking the prose around it.
	//
	// Extracted from the pending buffer together with this delta, not from
	// the delta alone: a streaming provider hands the token over in pieces
	// - "<", "wait", ">" - and no piece is the token. Measured, a menu turn
	// that answered with the token alone was pronounced "wait" to the
	// recording. The pieces gather at the end of the buffer, because nothing
	// in them ends a sentence, so the buffer is where the token reassembles.
	if len(text) > runner.config.MaxRunBytes-run.totalBytes {
		return false, runner.failActive(ctx, cause, OutcomeFailed, "run_too_large",
			fmt.Sprintf("prepared run exceeds %d bytes", runner.config.MaxRunBytes), true)
	}
	run.totalBytes += len(text)
	combined, silencing := extractControlTokens(run.buffer + text)
	if silencing {
		run.silenced = true
	}
	run.buffer = combined
	if combined == "" {
		return true, nil
	}
	if err := runner.releaseSafeSegments(ctx, cause); err != nil {
		return runner.active == run, err
	}
	return runner.active == run, nil
}

func validatePreparedDelta(
	delta cognitionelements.PreparedTextDelta, boundary cognitionelements.TextBoundary, index uint64,
) error {
	if delta.Boundary != boundary || delta.Index != index {
		return fmt.Errorf("prepared stream must start with begin index 0, got %q index %d",
			delta.Boundary, delta.Index)
	}
	if delta.Text != "" || delta.Interrupted {
		return errors.New("prepared text begin cannot carry text or an interrupted marker")
	}
	return nil
}

// validateSafePreparedDelta defines the framing of the nominal safe stream.
// Unlike raw provider text, a chunk may be an intentional no-op and an end may
// carry bytes that the quarantine could not release until it saw the terminal.
func validateSafePreparedDelta(
	delta cognitionelements.PreparedTextDelta, expected uint64, started bool,
) error {
	if !started {
		return validatePreparedDelta(delta, cognitionelements.TextBegin, 0)
	}
	if delta.Index != expected {
		return fmt.Errorf("prepared text index is %d, want %d", delta.Index, expected)
	}
	switch delta.Boundary {
	case cognitionelements.TextChunk:
		if delta.Interrupted {
			return errors.New("safe prepared text chunk cannot be interrupted")
		}
	case cognitionelements.TextEnd:
		// Terminal text and Interrupted are both part of the safe framing.
	case cognitionelements.TextBegin:
		return errors.New("safe prepared run emitted a second begin")
	default:
		return fmt.Errorf("unknown safe prepared text boundary %q", delta.Boundary)
	}
	return nil
}

func (runner *segmentPreparedTextRunner) releaseSafeSegments(
	ctx context.Context, cause element.Envelope,
) error {
	if runner.active.silenced {
		// The run asked for silence. Holding the buffer rather than splitting
		// it keeps the prose around the token unspoken too.
		return nil
	}
	for {
		pieces := bysentence.SplitWithClauseMinimum(runner.active.buffer,
			runner.config.MinimumRunes, runner.config.MinimumClauseRunes)
		if len(pieces) < 2 {
			break
		}
		if len(pieces[0]) > runner.config.MaxSegmentBytes {
			return runner.failActive(ctx, cause, OutcomeFailed, "segment_too_large",
				fmt.Sprintf("safe speech segment exceeds %d bytes", runner.config.MaxSegmentBytes), true)
		}
		if err := runner.publishSegment(ctx, cause, pieces[0]); err != nil {
			return err
		}
		// The batch splitter trims both ends of its remainder. In a live
		// stream the trailing whitespace belongs before the next delta;
		// dropping it turns "return label " + "to" into "return labelto".
		original := runner.active.buffer
		trailing := original[len(strings.TrimRightFunc(original, unicode.IsSpace)):]
		runner.active.buffer = pieces[1] + trailing
	}
	if len(runner.active.buffer) > runner.config.MaxSegmentBytes {
		return runner.failActive(ctx, cause, OutcomeFailed, "segment_too_large",
			fmt.Sprintf("no safe boundary within %d bytes", runner.config.MaxSegmentBytes), true)
	}
	return nil
}

func (runner *segmentPreparedTextRunner) publishSegment(
	ctx context.Context, cause element.Envelope, text string,
) error {
	run := runner.active
	if len(run.segments) >= runner.config.MaxSegments {
		return runner.failActive(ctx, cause, OutcomeFailed, "too_many_segments",
			fmt.Sprintf("prepared run exceeds %d speech segments", runner.config.MaxSegments), true)
	}
	sequence := len(run.segments) + 1
	utteranceID := runner.instance + ":" + run.id + ":speech:" + strconv.Itoa(sequence)
	run.segments = append(run.segments, utteranceID)
	envelope := cause.Clone()
	envelope.Type = textSegmentType
	envelope.ItemID = cause.ItemID + ":speech:" + strconv.Itoa(sequence)
	envelope.RunID = run.id
	envelope.Sequence = uint64(sequence)
	envelope.CancellationScope = utteranceID
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, run.begin.ItemID)
	envelope.Payload = speech.TextSegment{
		ID: utteranceID, Text: text, SpokeOver: run.spokeOver,
		Producer: firstNonemptyString(cause.SourceID, run.begin.SourceID),
		// SpeechAuthority intentionally remains empty. Graph routing grants
		// speech; a provider's legacy silent label is provenance, not policy.
	}
	return broadcastInteraction(ctx, runner.segmentsOutput, envelope)
}

func (runner *segmentPreparedTextRunner) completeActive(
	ctx context.Context, cause element.Envelope,
) error {
	run := runner.active
	runner.active = nil
	runner.rememberTerminalSegments(run.id, run.segments)
	return runner.publishSegmentationOutcome(ctx, cause, SegmentationOutcome{
		Kind: OutcomeCompleted, RunID: run.id, Segments: len(run.segments),
		BufferedBytes: run.totalBytes,
	})
}

// completeSilenced ends a run that carried a control token asking for silence.
func (runner *segmentPreparedTextRunner) completeSilenced(
	ctx context.Context, cause element.Envelope,
) error {
	run := runner.active
	runner.active = nil
	runner.rememberTerminalSegments(run.id, run.segments)
	return runner.publishSegmentationOutcome(ctx, cause, SegmentationOutcome{
		Kind: OutcomeCompleted, RunID: run.id, Segments: len(run.segments),
		BufferedBytes: run.totalBytes, Code: "control_token_silence",
		Message: "a control token in the prepared text asked this run to say nothing",
	})
}

func (runner *segmentPreparedTextRunner) failActive(
	ctx context.Context, cause element.Envelope, kind OutcomeKind, code, message string,
	cancelModel bool,
) error {
	run := runner.active
	if run == nil {
		return errors.New("segment prepared text has no active run")
	}
	runner.active = nil
	runner.rememberTerminal(run.id)
	// Revoke already-issued utterances first. A blocked model-control edge must
	// never delay the more urgent request to stop audio that may reach a user.
	if err := runner.publishSpeechCancels(ctx, cause, run.id, run.segments, message); err != nil {
		return err
	}
	if cancelModel {
		if err := runner.publishModelCancel(ctx, cause, run.id, message); err != nil {
			return err
		}
	}
	return runner.publishSegmentationOutcome(ctx, cause, SegmentationOutcome{
		Kind: kind, RunID: run.id, Segments: len(run.segments),
		BufferedBytes: run.totalBytes, Code: code, Message: message,
	})
}

func (runner *segmentPreparedTextRunner) refuseText(
	ctx context.Context, cause element.Envelope, runID, code, message string,
) error {
	if runID != "" {
		runner.rememberTerminal(runID)
		if err := runner.publishModelCancel(ctx, cause, runID, message); err != nil {
			return err
		}
	}
	return runner.publishSegmentationOutcome(ctx, cause, SegmentationOutcome{
		Kind: OutcomeRefused, RunID: runID, Code: code, Message: message,
	})
}

func (runner *segmentPreparedTextRunner) acceptCancel(
	ctx context.Context, envelope element.Envelope,
) error {
	request, valid := modelCancelPayload(envelope.Payload)
	if !valid {
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload),
		})
	}
	target, err := runAddress(request.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil {
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: runAddressCode(err), Message: err.Error(),
		})
	}
	if runner.active != nil && runner.active.id == target {
		return runner.failActive(ctx, envelope, OutcomeCanceled, "canceled",
			firstNonemptyString(strings.TrimSpace(request.Reason), "canceled"), true)
	}
	if runner.isTerminal(target) {
		segments := runner.takeTerminalSegments(target)
		if len(segments) != 0 {
			reason := firstNonemptyString(strings.TrimSpace(request.Reason), "canceled")
			if err := runner.publishSpeechCancels(ctx, envelope, target, segments, reason); err != nil {
				return err
			}
			return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
				Kind: OutcomeCanceled, RunID: target, Segments: len(segments),
				Code: "canceled_after_stream", Message: reason,
			})
		}
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeIgnored, RunID: target, Code: "already_terminal",
		})
	}
	runner.rememberTerminal(target)
	if err := runner.publishModelCancel(ctx, envelope, target, request.Reason); err != nil {
		return err
	}
	return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
		Kind: OutcomeCanceled, RunID: target, Code: "canceled_before_stream",
		Message: strings.TrimSpace(request.Reason),
	})
}

func (runner *segmentPreparedTextRunner) acceptTimeout(
	ctx context.Context, envelope element.Envelope,
) error {
	timeout, valid := timeoutPayload(envelope.Payload)
	if !valid {
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: "invalid_payload", Message: fmt.Sprintf("timeout payload has type %T", envelope.Payload),
		})
	}
	target, err := runAddress(timeout.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil {
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: runAddressCode(err), Message: err.Error(),
		})
	}
	reason := firstNonemptyString(strings.TrimSpace(timeout.Reason), "deadline expired")
	if runner.active != nil && runner.active.id == target {
		return runner.failActive(ctx, envelope, OutcomeFailed, "timeout", reason, true)
	}
	if runner.isTerminal(target) {
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeIgnored, RunID: target, Code: "already_terminal",
		})
	}
	runner.rememberTerminal(target)
	if err := runner.publishModelCancel(ctx, envelope, target, reason); err != nil {
		return err
	}
	return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
		Kind: OutcomeFailed, RunID: target, Code: "timeout", Message: reason,
	})
}

func (runner *segmentPreparedTextRunner) acceptTerminal(
	ctx context.Context, envelope element.Envelope,
) error {
	outcome, valid := modelOutcomePayload(envelope.Payload)
	if !valid {
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: "invalid_payload", Message: fmt.Sprintf("terminal payload has type %T", envelope.Payload),
		})
	}
	target, err := runAddress(outcome.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil {
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: runAddressCode(err), Message: err.Error(),
		})
	}
	if outcome.Kind == cognitionelements.OutcomeSucceeded {
		// TextEnd is the ordered stream terminal. Success can race ahead on a
		// separate edge, so it must not discard already-enqueued text.
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeIgnored, RunID: target, Code: "stream_end_authoritative",
			Message: "successful source terminal does not overtake prepared text framing",
		})
	}
	kind := OutcomeFailed
	if outcome.Kind == cognitionelements.OutcomeCanceled {
		kind = OutcomeCanceled
	}
	message := firstNonemptyString(strings.TrimSpace(outcome.Message), string(outcome.Kind))
	if runner.active != nil && runner.active.id == target {
		return runner.failActive(ctx, envelope, kind, "source_"+string(outcome.Kind), message, false)
	}
	if runner.isTerminal(target) {
		return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
			Kind: OutcomeIgnored, RunID: target, Code: "already_terminal",
		})
	}
	runner.rememberTerminal(target)
	return runner.publishSegmentationOutcome(ctx, envelope, SegmentationOutcome{
		Kind: kind, RunID: target, Code: "source_" + string(outcome.Kind), Message: message,
	})
}

func (runner *segmentPreparedTextRunner) publishModelCancel(
	ctx context.Context, cause element.Envelope, runID, reason string,
) error {
	envelope := cause.Clone()
	envelope.Type = modelCancelType
	envelope.ItemID = cause.ItemID + ":model-cancel"
	envelope.RunID = runID
	envelope.CancellationScope = runID
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = cognitionelements.Cancel{RunID: runID, Reason: strings.TrimSpace(reason)}
	return broadcastInteraction(ctx, runner.modelCancelOutput, envelope)
}

func (runner *segmentPreparedTextRunner) publishSpeechCancels(
	ctx context.Context, cause element.Envelope, runID string, utteranceIDs []string, reason string,
) error {
	for index, utteranceID := range utteranceIDs {
		envelope := cause.Clone()
		envelope.Type = speechCancelType
		envelope.ItemID = cause.ItemID + ":speech-cancel:" + strconv.Itoa(index+1)
		envelope.RunID = runID
		envelope.Sequence = uint64(index + 1)
		envelope.CancellationScope = utteranceID
		envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
		envelope.Payload = speech.Cancel{UtteranceID: utteranceID, Reason: strings.TrimSpace(reason)}
		if err := broadcastInteraction(ctx, runner.speechCancelOutput, envelope); err != nil {
			return err
		}
	}
	return nil
}

func (runner *segmentPreparedTextRunner) publishSegmentationOutcome(
	ctx context.Context, cause element.Envelope, outcome SegmentationOutcome,
) error {
	envelope := cause.Clone()
	envelope.Type = segmentationOutcomeType
	envelope.ItemID = cause.ItemID + ":segmentation-outcome"
	envelope.RunID = outcome.RunID
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	return broadcastInteraction(ctx, runner.outcomeOutput, envelope)
}

func (runner *segmentPreparedTextRunner) rememberTerminal(runID string) {
	if runID == "" {
		return
	}
	if _, exists := runner.terminalRuns[runID]; exists {
		return
	}
	if len(runner.terminalOrder) == runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		delete(runner.terminalRuns, oldest)
		delete(runner.terminalSegments, oldest)
		copy(runner.terminalOrder, runner.terminalOrder[1:])
		runner.terminalOrder = runner.terminalOrder[:len(runner.terminalOrder)-1]
	}
	runner.terminalRuns[runID] = struct{}{}
	runner.terminalOrder = append(runner.terminalOrder, runID)
}

func (runner *segmentPreparedTextRunner) rememberTerminalSegments(
	runID string, utteranceIDs []string,
) {
	runner.rememberTerminal(runID)
	if runID == "" || len(utteranceIDs) == 0 {
		return
	}
	runner.terminalSegments[runID] = append([]string(nil), utteranceIDs...)
}

func (runner *segmentPreparedTextRunner) takeTerminalSegments(runID string) []string {
	utteranceIDs := runner.terminalSegments[runID]
	delete(runner.terminalSegments, runID)
	return utteranceIDs
}

func (runner *segmentPreparedTextRunner) isTerminal(runID string) bool {
	_, exists := runner.terminalRuns[runID]
	return exists
}

func receiveSegmentInput(
	ctx context.Context, kind segmentInputKind, input element.InputPort, variadic bool,
	output chan<- segmentInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		var envelope element.Envelope
		var err error
		if variadic {
			envelope, _, err = input.ReceiveAny(ctx)
		} else {
			envelope, err = input.Receive(ctx)
		}
		if terminalInteractionReceive(ctx, err) {
			return
		}
		if err != nil {
			sendInteractionFailure(ctx, failures, err)
			return
		}
		select {
		case output <- segmentInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func preparedTextPayload(payload any) (cognitionelements.PreparedTextDelta, bool) {
	switch typed := payload.(type) {
	case cognitionelements.PreparedTextDelta:
		return typed, true
	case *cognitionelements.PreparedTextDelta:
		if typed != nil {
			return *typed, true
		}
	}
	return cognitionelements.PreparedTextDelta{}, false
}

func modelCancelPayload(payload any) (cognitionelements.Cancel, bool) {
	switch typed := payload.(type) {
	case cognitionelements.Cancel:
		return typed, true
	case *cognitionelements.Cancel:
		if typed != nil {
			return *typed, true
		}
	}
	return cognitionelements.Cancel{}, false
}

func modelOutcomePayload(payload any) (cognitionelements.Outcome, bool) {
	switch typed := payload.(type) {
	case cognitionelements.Outcome:
		return typed, true
	case *cognitionelements.Outcome:
		if typed != nil {
			return *typed, true
		}
	}
	return cognitionelements.Outcome{}, false
}

func timeoutPayload(payload any) (Timeout, bool) {
	switch typed := payload.(type) {
	case Timeout:
		return typed, true
	case *Timeout:
		if typed != nil {
			return *typed, true
		}
	}
	return Timeout{}, false
}

func runAddressCode(err error) string {
	if strings.Contains(err.Error(), "required") {
		return "missing_run_id"
	}
	return "conflicting_run_id"
}

func terminalInteractionReceive(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}

func sendInteractionFailure(ctx context.Context, failures chan<- error, err error) {
	select {
	case failures <- err:
	case <-ctx.Done():
	}
}

func broadcastInteraction(
	ctx context.Context, output element.OutputPort, envelope element.Envelope,
) error {
	result, err := output.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if result.Delivered == 0 && len(output.Lanes()) != 0 {
		return fmt.Errorf("interaction output %s was not delivered", output.Name())
	}
	return nil
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func firstNonemptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// controlTokens never reach a synthesiser. They are the model's channel for
// telling the runtime what to do, and a synthesiser handed one reads it out:
// measured at a phone menu, "Pressing the key for order status. <wait>" was
// spoken to a recording that could not hear it. Extracting them here rather
// than at each producer means one place decides, whichever model wrote the
// text and whichever binding is running.
var controlTokens = []string{coreinteraction.WaitToken}

// extractControlTokens removes every control token from one delta and reports
// whether any was present.
func extractControlTokens(text string) (string, bool) {
	found := false
	for _, token := range controlTokens {
		if !strings.Contains(text, token) {
			continue
		}
		found = true
		text = strings.ReplaceAll(text, token, "")
	}
	return text, found
}
