package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
)

type speechArbiterFactory struct{}

var (
	_ element.Factory         = speechArbiterFactory{}
	_ element.ConfigValidator = speechArbiterFactory{}
)

func (speechArbiterFactory) Descriptor() element.Descriptor { return SpeechArbiterDescriptor() }

func (speechArbiterFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeArbiterConfig(source)
	return err
}

func (speechArbiterFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeArbiterConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("interaction.SpeechArbiter %s config: %w", mount.InstanceID, err)
	}
	textInput, err := mount.Ports.Input("text")
	if err != nil {
		return nil, err
	}
	terminalInput, err := mount.Ports.Input("terminal")
	if err != nil {
		return nil, err
	}
	selectionInput, err := mount.Ports.Input("selection")
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
	selectedOutput, err := mount.Ports.Output("selected")
	if err != nil {
		return nil, err
	}
	cancelOutput, err := mount.Ports.Output("cancel_upstream")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &speechArbiterRunner{
		instance:       mount.InstanceID,
		config:         config,
		textInput:      textInput,
		terminalInput:  terminalInput,
		selectionInput: selectionInput,
		timeoutInput:   timeoutInput,
		cancelInput:    cancelInput,
		selectedOutput: selectedOutput,
		cancelOutput:   cancelOutput,
		outcomeOutput:  outcomeOutput,
		runs:           make(map[string]*arbitratedRun, config.MaxPendingRuns),
		resolution:     mount.Resolution,
	}, nil
}

type arbiterInputKind uint8

const (
	arbiterTextInput arbiterInputKind = iota
	arbiterTerminalInput
	arbiterSelectionInput
	arbiterTimeoutInput
	arbiterCancelInput
)

type arbiterInput struct {
	kind     arbiterInputKind
	lane     string
	envelope element.Envelope
}

type arbitratedRun struct {
	id          string
	lane        string
	expected    uint64
	started     bool
	ended       bool
	discarding  bool
	selected    bool
	queued      bool
	outputOpen  bool
	outputClose bool
	lastOutput  string
	buffer      []element.Envelope
	bufferBytes int
}

type speechArbiterRunner struct {
	instance string
	config   SpeechArbiterConfig

	textInput      element.InputPort
	terminalInput  element.InputPort
	selectionInput element.InputPort
	timeoutInput   element.InputPort
	cancelInput    element.InputPort

	selectedOutput element.OutputPort
	cancelOutput   element.OutputPort
	outcomeOutput  element.OutputPort

	runs           map[string]*arbitratedRun
	active         string
	queue          []string
	bufferedBytes  int
	bufferedDeltas int
	resolution     element.ResolutionReporter
}

func (runner *speechArbiterRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := reportInteractionResolution(runner.resolution, SpeechArbiterDescriptor()); err != nil {
		return err
	}
	inputs := make(chan arbiterInput)
	failures := make(chan error, 5)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		kind     arbiterInputKind
		port     element.InputPort
		variadic bool
	}{
		{arbiterTextInput, runner.textInput, true},
		{arbiterTerminalInput, runner.terminalInput, true},
		{arbiterSelectionInput, runner.selectionInput, false},
		{arbiterTimeoutInput, runner.timeoutInput, false},
		{arbiterCancelInput, runner.cancelInput, false},
	} {
		receivers.Add(1)
		go receiveArbiterInput(ctx, source.kind, source.port, source.variadic,
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
			case arbiterTextInput:
				err = runner.acceptText(ctx, input)
			case arbiterTerminalInput:
				err = runner.acceptTerminal(ctx, input.envelope)
			case arbiterSelectionInput:
				err = runner.acceptSelection(ctx, input.envelope)
			case arbiterTimeoutInput:
				err = runner.acceptTimeout(ctx, input.envelope)
			case arbiterCancelInput:
				err = runner.acceptCancel(ctx, input.envelope)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *speechArbiterRunner) acceptText(
	ctx context.Context, input arbiterInput,
) error {
	envelope := input.envelope
	delta, valid := preparedTextPayload(envelope.Payload)
	runID := strings.TrimSpace(envelope.RunID)
	if !valid {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: runID, Code: "invalid_payload",
			Message: fmt.Sprintf("prepared text payload has type %T", envelope.Payload),
		})
	}
	if runID == "" {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, Code: "missing_run_id",
			Message: "prepared text requires an envelope run ID",
		})
	}
	if len(delta.Text) > runner.config.MaxDeltaBytes {
		return runner.failStream(ctx, envelope, runID, "delta_too_large",
			fmt.Sprintf("prepared text delta exceeds %d bytes", runner.config.MaxDeltaBytes))
	}
	run := runner.runs[runID]
	if run == nil {
		if len(runner.runs) >= runner.config.MaxPendingRuns {
			if err := runner.publishUpstreamCancel(ctx, envelope, runID, "arbiter run capacity exceeded"); err != nil {
				return err
			}
			return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
				Kind: OutcomeRefused, RunID: runID, Code: "run_capacity",
				Message: fmt.Sprintf("arbiter retains at most %d runs", runner.config.MaxPendingRuns),
			})
		}
		run = &arbitratedRun{id: runID, lane: input.lane}
		runner.runs[runID] = run
	}
	if run.discarding {
		if delta.Boundary == cognitionelements.TextEnd {
			runner.removeRun(runID)
		}
		return nil
	}
	if run.lane == "" {
		run.lane = input.lane
	} else if run.lane != input.lane {
		return runner.failStream(ctx, envelope, runID, "multiple_source_lanes",
			fmt.Sprintf("run %q arrived on lanes %q and %q", runID, run.lane, input.lane))
	}
	if err := runner.validateRunDelta(run, delta); err != nil {
		return runner.failStream(ctx, envelope, runID, "invalid_framing", err.Error())
	}
	if run.selected && runner.active == runID {
		if err := runner.forwardDelta(ctx, run, envelope, delta); err != nil {
			return err
		}
		if delta.Boundary == cognitionelements.TextEnd {
			return runner.finishSelected(ctx, envelope, run, OutcomeCompleted, "", "")
		}
		return nil
	}
	if runner.bufferedDeltas >= runner.config.MaxBufferedDeltas ||
		len(delta.Text) > runner.config.MaxBufferedBytes-runner.bufferedBytes {
		return runner.failStream(ctx, envelope, runID, "buffer_capacity",
			fmt.Sprintf("arbiter buffer exceeds %d deltas or %d bytes",
				runner.config.MaxBufferedDeltas, runner.config.MaxBufferedBytes))
	}
	run.buffer = append(run.buffer, clonePreparedEnvelope(envelope, delta))
	run.bufferBytes += len(delta.Text)
	runner.bufferedBytes += len(delta.Text)
	runner.bufferedDeltas++
	return nil
}

func (runner *speechArbiterRunner) validateRunDelta(
	run *arbitratedRun, delta cognitionelements.PreparedTextDelta,
) error {
	if run.ended {
		return errors.New("prepared text arrived after its end")
	}
	if !run.started {
		if err := validatePreparedDelta(delta, cognitionelements.TextBegin, 0); err != nil {
			return err
		}
		run.started = true
		run.expected = 1
		return nil
	}
	if delta.Index != run.expected {
		return fmt.Errorf("prepared text index is %d, want %d", delta.Index, run.expected)
	}
	run.expected++
	switch delta.Boundary {
	case cognitionelements.TextChunk:
		if delta.Text == "" || delta.Interrupted {
			return errors.New("a prepared text delta requires text and cannot be terminal")
		}
	case cognitionelements.TextEnd:
		if delta.Text != "" {
			return errors.New("prepared text end cannot carry text")
		}
		run.ended = true
	case cognitionelements.TextBegin:
		return errors.New("prepared run emitted a second begin")
	default:
		return fmt.Errorf("unknown prepared text boundary %q", delta.Boundary)
	}
	return nil
}

func (runner *speechArbiterRunner) acceptSelection(
	ctx context.Context, envelope element.Envelope,
) error {
	selection, valid := selectionPayload(envelope.Payload)
	if !valid {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: "invalid_payload", Message: fmt.Sprintf("selection payload has type %T", envelope.Payload),
		})
	}
	target, err := runAddress(selection.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: runAddressCode(err), Message: err.Error(),
		})
	}
	if selection.Mode != SelectionQueue && selection.Mode != SelectionPreempt {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: target, Code: "invalid_selection_mode",
			Message: fmt.Sprintf("selection mode must be %q or %q", SelectionQueue, SelectionPreempt),
		})
	}
	run := runner.runs[target]
	if run != nil && run.discarding {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: target, Code: "run_terminal",
			Message: "a canceled or failed run cannot be selected",
		})
	}
	if run == nil {
		if len(runner.runs) >= runner.config.MaxPendingRuns {
			return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
				Kind: OutcomeRefused, RunID: target, Code: "run_capacity",
				Message: fmt.Sprintf("arbiter retains at most %d runs", runner.config.MaxPendingRuns),
			})
		}
		run = &arbitratedRun{id: target}
		runner.runs[target] = run
	}
	if runner.active == target {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeIgnored, RunID: target, Code: "already_selected",
		})
	}
	if selection.Mode == SelectionQueue && runner.active != "" {
		if run.queued {
			return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
				Kind: OutcomeIgnored, RunID: target, Code: "already_queued",
				QueuePosition: runner.queuePosition(target),
			})
		}
		run.queued = true
		runner.queue = append(runner.queue, target)
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeQueued, RunID: target, QueuePosition: len(runner.queue),
		})
	}
	replaced := ""
	if runner.active != "" {
		replaced = runner.active
		if err := runner.preemptActive(ctx, envelope, target); err != nil {
			return err
		}
	}
	runner.removeFromQueue(target)
	return runner.selectRun(ctx, envelope, run, replaced)
}

func (runner *speechArbiterRunner) selectRun(
	ctx context.Context, cause element.Envelope, run *arbitratedRun, replaced string,
) error {
	runner.active = run.id
	run.selected = true
	run.queued = false
	if err := runner.publishArbitrationOutcome(ctx, cause, ArbitrationOutcome{
		Kind: OutcomeSelected, RunID: run.id, ReplacedRunID: replaced,
	}); err != nil {
		return err
	}
	return runner.flushSelected(ctx, cause, run)
}

func (runner *speechArbiterRunner) flushSelected(
	ctx context.Context, cause element.Envelope, run *arbitratedRun,
) error {
	buffer := run.buffer
	runner.bufferedBytes -= run.bufferBytes
	runner.bufferedDeltas -= len(buffer)
	run.buffer = nil
	run.bufferBytes = 0
	for _, envelope := range buffer {
		delta, _ := preparedTextPayload(envelope.Payload)
		if err := runner.forwardDelta(ctx, run, envelope, delta); err != nil {
			return err
		}
		if delta.Boundary == cognitionelements.TextEnd {
			return runner.finishSelected(ctx, envelope, run, OutcomeCompleted, "", "")
		}
	}
	return nil
}

func (runner *speechArbiterRunner) forwardDelta(
	ctx context.Context, run *arbitratedRun, source element.Envelope,
	delta cognitionelements.PreparedTextDelta,
) error {
	envelope := clonePreparedEnvelope(source, delta)
	envelope.Type = preparedTextType
	envelope.RunID = run.id
	if run.lastOutput != "" {
		envelope.CausalParents = appendUniqueString(envelope.CausalParents, run.lastOutput)
	}
	if err := broadcastInteraction(ctx, runner.selectedOutput, envelope); err != nil {
		return err
	}
	run.lastOutput = envelope.ItemID
	if delta.Boundary == cognitionelements.TextBegin {
		run.outputOpen = true
	}
	if delta.Boundary == cognitionelements.TextEnd {
		run.outputClose = true
	}
	return nil
}

func (runner *speechArbiterRunner) preemptActive(
	ctx context.Context, cause element.Envelope, replacement string,
) error {
	current := runner.runs[runner.active]
	if current == nil {
		runner.active = ""
		return nil
	}
	if err := runner.closeSelected(ctx, cause, current); err != nil {
		return err
	}
	if err := runner.publishUpstreamCancel(ctx, cause, current.id,
		"preempted by explicit selection of "+replacement); err != nil {
		return err
	}
	current.selected = false
	current.queued = false
	current.discarding = true
	runner.active = ""
	runner.clearBuffer(current)
	return runner.publishArbitrationOutcome(ctx, cause, ArbitrationOutcome{
		Kind: OutcomePreempted, RunID: current.id, ReplacedRunID: replacement,
		Code: "explicit_preemption",
	})
}

func (runner *speechArbiterRunner) closeSelected(
	ctx context.Context, cause element.Envelope, run *arbitratedRun,
) error {
	if !run.outputOpen || run.outputClose {
		return nil
	}
	envelope := cause.Clone()
	envelope.Type = preparedTextType
	envelope.ItemID = cause.ItemID + ":synthetic-end:" + run.id
	envelope.RunID = run.id
	envelope.Sequence = run.expected + 1
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, run.lastOutput)
	envelope.Payload = cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: run.expected, Interrupted: true,
	}
	if err := broadcastInteraction(ctx, runner.selectedOutput, envelope); err != nil {
		return err
	}
	run.lastOutput = envelope.ItemID
	run.outputClose = true
	return nil
}

func (runner *speechArbiterRunner) finishSelected(
	ctx context.Context, cause element.Envelope, run *arbitratedRun,
	kind OutcomeKind, code, message string,
) error {
	runID := run.id
	runner.active = ""
	runner.removeRun(runID)
	if err := runner.publishArbitrationOutcome(ctx, cause, ArbitrationOutcome{
		Kind: kind, RunID: runID, Code: code, Message: message,
	}); err != nil {
		return err
	}
	return runner.promoteNext(ctx, cause)
}

func (runner *speechArbiterRunner) promoteNext(
	ctx context.Context, cause element.Envelope,
) error {
	for runner.active == "" && len(runner.queue) > 0 {
		nextID := runner.queue[0]
		runner.queue = runner.queue[1:]
		run := runner.runs[nextID]
		if run == nil || run.discarding {
			continue
		}
		if err := runner.selectRun(ctx, cause, run, ""); err != nil {
			return err
		}
	}
	return nil
}

func (runner *speechArbiterRunner) failStream(
	ctx context.Context, cause element.Envelope, runID, code, message string,
) error {
	run := runner.runs[runID]
	if run == nil {
		if len(runner.runs) < runner.config.MaxPendingRuns {
			run = &arbitratedRun{id: runID, discarding: true}
			runner.runs[runID] = run
		}
	} else {
		wasActive := runner.active == runID
		if wasActive {
			if err := runner.closeSelected(ctx, cause, run); err != nil {
				return err
			}
			runner.active = ""
		}
		runner.removeFromQueue(runID)
		runner.clearBuffer(run)
		run.selected = false
		run.queued = false
		if run.ended {
			// The terminal delta itself can be the item that exceeds a delta
			// count. It has still closed the input framing, so retaining a
			// discard tombstone would consume capacity forever.
			runner.removeRun(runID)
		} else {
			run.discarding = true
		}
	}
	if err := runner.publishUpstreamCancel(ctx, cause, runID, message); err != nil {
		return err
	}
	if err := runner.publishArbitrationOutcome(ctx, cause, ArbitrationOutcome{
		Kind: OutcomeFailed, RunID: runID, Code: code, Message: message,
	}); err != nil {
		return err
	}
	if runner.active == "" {
		return runner.promoteNext(ctx, cause)
	}
	return nil
}

func (runner *speechArbiterRunner) acceptCancel(
	ctx context.Context, envelope element.Envelope,
) error {
	request, valid := modelCancelPayload(envelope.Payload)
	if !valid {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload),
		})
	}
	target, err := runAddress(request.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: runAddressCode(err), Message: err.Error(),
		})
	}
	reason := firstNonemptyString(strings.TrimSpace(request.Reason), "canceled")
	return runner.terminateRun(ctx, envelope, target, OutcomeCanceled, "canceled", reason, true)
}

func (runner *speechArbiterRunner) acceptTimeout(
	ctx context.Context, envelope element.Envelope,
) error {
	timeout, valid := timeoutPayload(envelope.Payload)
	if !valid {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: "invalid_payload", Message: fmt.Sprintf("timeout payload has type %T", envelope.Payload),
		})
	}
	target, err := runAddress(timeout.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: runAddressCode(err), Message: err.Error(),
		})
	}
	reason := firstNonemptyString(strings.TrimSpace(timeout.Reason), "deadline expired")
	return runner.terminateRun(ctx, envelope, target, OutcomeFailed, "timeout", reason, true)
}

func (runner *speechArbiterRunner) acceptTerminal(
	ctx context.Context, envelope element.Envelope,
) error {
	outcome, valid := modelOutcomePayload(envelope.Payload)
	if !valid {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: "invalid_payload", Message: fmt.Sprintf("terminal payload has type %T", envelope.Payload),
		})
	}
	target, err := runAddress(outcome.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeRefused, RunID: strings.TrimSpace(envelope.RunID),
			Code: runAddressCode(err), Message: err.Error(),
		})
	}
	if outcome.Kind == cognitionelements.OutcomeSucceeded {
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeIgnored, RunID: target, Code: "stream_end_authoritative",
			Message: "successful source terminal does not overtake prepared text framing",
		})
	}
	// A preempted run is retained only until its source supplies authoritative
	// terminal framing. A model that was canceled before opening its text
	// stream has no TextEnd to send, so its non-success model outcome is the
	// only terminal signal that can release the discard tombstone. Keeping it
	// here would consume bounded run capacity for the rest of the mount.
	if run := runner.runs[target]; run != nil && run.discarding {
		runner.removeRun(target)
		return runner.publishArbitrationOutcome(ctx, envelope, ArbitrationOutcome{
			Kind: OutcomeIgnored, RunID: target, Code: "discard_terminal",
			Message: "source terminal released a preempted run",
		})
	}
	kind := OutcomeFailed
	if outcome.Kind == cognitionelements.OutcomeCanceled {
		kind = OutcomeCanceled
	}
	reason := firstNonemptyString(strings.TrimSpace(outcome.Message), string(outcome.Kind))
	return runner.terminateRun(ctx, envelope, target, kind,
		"source_"+string(outcome.Kind), reason, false)
}

func (runner *speechArbiterRunner) terminateRun(
	ctx context.Context, cause element.Envelope, runID string,
	kind OutcomeKind, code, message string, cancelUpstream bool,
) error {
	run := runner.runs[runID]
	if run != nil && run.discarding {
		return runner.publishArbitrationOutcome(ctx, cause, ArbitrationOutcome{
			Kind: OutcomeIgnored, RunID: runID, Code: "already_terminal",
		})
	}
	if run == nil {
		if len(runner.runs) >= runner.config.MaxPendingRuns {
			return runner.publishArbitrationOutcome(ctx, cause, ArbitrationOutcome{
				Kind: OutcomeRefused, RunID: runID, Code: "run_capacity",
				Message: fmt.Sprintf("arbiter retains at most %d runs", runner.config.MaxPendingRuns),
			})
		}
		run = &arbitratedRun{id: runID}
		runner.runs[runID] = run
	}
	wasActive := runner.active == runID
	if wasActive {
		if err := runner.closeSelected(ctx, cause, run); err != nil {
			return err
		}
		runner.active = ""
	}
	runner.removeFromQueue(runID)
	runner.clearBuffer(run)
	run.selected = false
	run.queued = false
	if run.ended {
		runner.removeRun(runID)
	} else {
		run.discarding = true
	}
	if cancelUpstream {
		if err := runner.publishUpstreamCancel(ctx, cause, runID, message); err != nil {
			return err
		}
	}
	if err := runner.publishArbitrationOutcome(ctx, cause, ArbitrationOutcome{
		Kind: kind, RunID: runID, Code: code, Message: message,
	}); err != nil {
		return err
	}
	if wasActive {
		return runner.promoteNext(ctx, cause)
	}
	return nil
}

func (runner *speechArbiterRunner) publishUpstreamCancel(
	ctx context.Context, cause element.Envelope, runID, reason string,
) error {
	envelope := cause.Clone()
	envelope.Type = modelCancelType
	envelope.ItemID = cause.ItemID + ":cancel-upstream:" + runID
	envelope.RunID = runID
	envelope.CancellationScope = runID
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = cognitionelements.Cancel{RunID: runID, Reason: strings.TrimSpace(reason)}
	return broadcastInteraction(ctx, runner.cancelOutput, envelope)
}

func (runner *speechArbiterRunner) publishArbitrationOutcome(
	ctx context.Context, cause element.Envelope, outcome ArbitrationOutcome,
) error {
	outcome.BufferedRuns = len(runner.runs)
	outcome.BufferedBytes = runner.bufferedBytes
	envelope := cause.Clone()
	envelope.Type = arbitrationOutcomeType
	envelope.ItemID = cause.ItemID + ":arbitration-outcome:" +
		string(outcome.Kind) + ":" + outcome.RunID + ":" + strconv.Itoa(len(runner.queue))
	envelope.RunID = outcome.RunID
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	return broadcastInteraction(ctx, runner.outcomeOutput, envelope)
}

func (runner *speechArbiterRunner) clearBuffer(run *arbitratedRun) {
	if run == nil {
		return
	}
	runner.bufferedBytes -= run.bufferBytes
	runner.bufferedDeltas -= len(run.buffer)
	run.buffer = nil
	run.bufferBytes = 0
}

func (runner *speechArbiterRunner) removeRun(runID string) {
	run := runner.runs[runID]
	if run == nil {
		return
	}
	runner.clearBuffer(run)
	delete(runner.runs, runID)
	runner.removeFromQueue(runID)
	if runner.active == runID {
		runner.active = ""
	}
}

func (runner *speechArbiterRunner) removeFromQueue(runID string) {
	for index, queued := range runner.queue {
		if queued != runID {
			continue
		}
		copy(runner.queue[index:], runner.queue[index+1:])
		runner.queue = runner.queue[:len(runner.queue)-1]
		break
	}
	if run := runner.runs[runID]; run != nil {
		run.queued = false
	}
}

func (runner *speechArbiterRunner) queuePosition(runID string) int {
	for index, queued := range runner.queue {
		if queued == runID {
			return index + 1
		}
	}
	return 0
}

func receiveArbiterInput(
	ctx context.Context, kind arbiterInputKind, input element.InputPort, variadic bool,
	output chan<- arbiterInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		var envelope element.Envelope
		var lane string
		var err error
		if variadic {
			envelope, lane, err = input.ReceiveAny(ctx)
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
		case output <- arbiterInput{kind: kind, lane: lane, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func selectionPayload(payload any) (SpeechSelection, bool) {
	switch typed := payload.(type) {
	case SpeechSelection:
		return typed, true
	case *SpeechSelection:
		if typed != nil {
			return *typed, true
		}
	}
	return SpeechSelection{}, false
}

func clonePreparedEnvelope(
	envelope element.Envelope, delta cognitionelements.PreparedTextDelta,
) element.Envelope {
	copy := envelope.Clone()
	copy.Payload = delta
	return copy
}
