package acoustic

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
)

type endpointRunner struct {
	instance   string
	config     resolvedEndpointConfig
	ports      endpointPorts
	resolution element.ResolutionReporter

	pending       *EndpointCandidate
	earlyVerdict  *retainedVerdict
	deadlineNS    uint64
	lastTickNS    uint64
	hasTick       bool
	stateSequence uint64
	canceled      boundedSet
	terminal      boundedSet
}

type retainedVerdict struct {
	verdict  EndpointVerdict
	envelope element.Envelope
}

func newEndpointRunner(
	instance string, config resolvedEndpointConfig, ports endpointPorts,
	resolution element.ResolutionReporter,
) *endpointRunner {
	return &endpointRunner{
		instance: instance, config: config, ports: ports, resolution: resolution,
		canceled: newBoundedSet(config.cancellationMemory), terminal: newBoundedSet(config.terminalMemory),
	}
}

func (runner *endpointRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := reportBuiltIn(runner.resolution, endpointRuntimeID); err != nil {
		return fmt.Errorf("attest acoustic.EndpointPolicy runtime: %w", err)
	}
	events := make(chan receivedInput)
	interrupts := make(chan element.Envelope)
	failures := make(chan error, 5)
	var receivers sync.WaitGroup
	receivers.Add(5)
	go receiveInputs(ctx, "candidate", runner.ports.candidate, events, failures, &receivers)
	go receiveInputs(ctx, "tick", runner.ports.tick, events, failures, &receivers)
	go receiveInputs(ctx, "commit", runner.ports.commit, events, failures, &receivers)
	go receiveInputs(ctx, "verdict", runner.ports.verdict, events, failures, &receivers)
	go receiveInterrupts(ctx, runner.ports.cancel, interrupts, failures, &receivers)
	defer func() {
		cancel(nil)
		receivers.Wait()
	}()
	if err := runner.publishState(ctx, element.Envelope{}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case interrupt := <-interrupts:
			if err := runner.handleCancel(ctx, interrupt); err != nil {
				return err
			}
		case input := <-events:
			if err := runner.drainInterrupts(ctx, interrupts); err != nil {
				return err
			}
			var err error
			switch input.kind {
			case "candidate":
				err = runner.handleCandidate(ctx, input.envelope)
			case "tick":
				err = runner.handleTick(ctx, input.envelope)
			case "commit":
				err = runner.handleCommit(ctx, input.envelope)
			case "verdict":
				err = runner.handleVerdict(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown EndpointPolicy input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *endpointRunner) drainInterrupts(
	ctx context.Context, interrupts <-chan element.Envelope,
) error {
	for {
		select {
		case interrupt := <-interrupts:
			if err := runner.handleCancel(ctx, interrupt); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (runner *endpointRunner) handleCandidate(ctx context.Context, envelope element.Envelope) error {
	candidate, valid := candidatePayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "candidate", Code: "invalid_payload",
			Message: fmt.Sprintf("candidate payload has type %T", envelope.Payload),
		})
	}
	identifierErr := errors.Join(
		validateIdentifier("candidate ID", candidate.ID, true),
		validateIdentifier("candidate stream ID", candidate.StreamID, true),
		validateIdentifier("candidate source", candidate.Source, true),
	)
	if identifierErr != nil || candidate.Sequence == 0 || candidate.SampleRateHz == 0 {
		message := "candidate requires canonical bounded IDs, a positive sequence, and a sample rate"
		if identifierErr != nil {
			message = identifierErr.Error()
		}
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "candidate", StreamID: candidate.StreamID,
			CandidateID: candidate.ID, Code: "invalid_candidate",
			Message: message,
		})
	}
	if runner.canceled.Has(candidate.StreamID) {
		if runner.earlyVerdict != nil && runner.earlyVerdict.verdict.CandidateID == candidate.ID {
			runner.earlyVerdict = nil
		}
		runner.terminal.Add(candidate.ID)
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeCanceled, Operation: "candidate", StreamID: candidate.StreamID,
			CandidateID: candidate.ID, Code: "canceled_before_candidate",
			Message: "the stream was canceled before this candidate arrived",
		})
	}
	if runner.terminal.Has(candidate.ID) {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeIgnored, Operation: "candidate", StreamID: candidate.StreamID,
			CandidateID: candidate.ID, Code: "candidate_terminal",
		})
	}
	if runner.pending != nil {
		code := "candidate_in_progress"
		if runner.pending.ID == candidate.ID {
			code = "duplicate_candidate"
		}
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeIgnored, Operation: "candidate", StreamID: candidate.StreamID,
			CandidateID: candidate.ID, Code: code,
			Message: fmt.Sprintf("pending candidate is %q", runner.pending.ID),
		})
	}
	if runner.config.mode == EndpointExternal && runner.earlyVerdict != nil {
		retained := *runner.earlyVerdict
		runner.earlyVerdict = nil
		if retained.verdict.CandidateID == candidate.ID &&
			(retained.verdict.StreamID == "" || retained.verdict.StreamID == candidate.StreamID) {
			cause := retained.envelope.Clone()
			originalVerdictID := cause.ItemID
			cause.ItemID += ":candidate-arrived"
			cause.CausalParents = appendUnique(cause.CausalParents, originalVerdictID)
			cause.CausalParents = appendUnique(cause.CausalParents, envelope.ItemID)
			return runner.resolveCandidate(ctx, cause, candidate, retained.verdict.Action, "verdict", "external_verdict")
		}
		runner.terminal.Add(retained.verdict.CandidateID)
		if err := runner.publishOutcomeWithSuffix(ctx, retained.envelope, EndpointOutcome{
			Kind: OutcomeIgnored, Operation: "verdict", StreamID: retained.verdict.StreamID,
			CandidateID: retained.verdict.CandidateID, Action: retained.verdict.Action,
			Code:    "superseded_by_candidate",
			Message: fmt.Sprintf("arriving candidate is %q", candidate.ID),
		}, ":superseded"); err != nil {
			return err
		}
	}
	switch runner.config.mode {
	case EndpointAutomatic:
		return runner.resolveCandidate(ctx, envelope, candidate, GateClose, "candidate", "automatic_close")
	case EndpointManual:
		return runner.resolveCandidate(ctx, envelope, candidate, GateReopen, "candidate", "manual_reopen")
	case EndpointExternal:
		runner.pending = &candidate
		if err := runner.publishState(ctx, envelope); err != nil {
			return err
		}
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomePending, Operation: "candidate", StreamID: candidate.StreamID,
			CandidateID: candidate.ID, Code: "awaiting_verdict_or_tick",
		})
	default:
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeFailed, Operation: "candidate", StreamID: candidate.StreamID,
			CandidateID: candidate.ID, Code: "invalid_runtime_mode",
		})
	}
}

func (runner *endpointRunner) handleVerdict(ctx context.Context, envelope element.Envelope) error {
	verdict, valid := verdictPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "verdict", Code: "invalid_payload",
			Message: fmt.Sprintf("verdict payload has type %T", envelope.Payload),
		})
	}
	if err := errors.Join(
		validateIdentifier("verdict candidate ID", verdict.CandidateID, true),
		validateIdentifier("verdict stream ID", verdict.StreamID, false),
	); err != nil {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "verdict", StreamID: verdict.StreamID,
			CandidateID: verdict.CandidateID, Code: "invalid_verdict", Message: err.Error(),
		})
	}
	if runner.config.mode != EndpointExternal {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "verdict", StreamID: verdict.StreamID,
			CandidateID: verdict.CandidateID, Action: verdict.Action,
			Code: "verdict_not_enabled", Message: "verdicts are accepted only in external mode",
		})
	}
	if verdict.CandidateID == "" || !verdict.Action.endpointVerdict() {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "verdict", StreamID: verdict.StreamID,
			CandidateID: verdict.CandidateID, Action: verdict.Action,
			Code: "invalid_verdict", Message: "verdict requires a candidate ID and close or reopen action",
		})
	}
	if runner.pending == nil {
		code := "no_pending_candidate"
		if runner.terminal.Has(verdict.CandidateID) {
			code = "candidate_terminal"
			return runner.publishOutcome(ctx, envelope, EndpointOutcome{
				Kind: OutcomeIgnored, Operation: "verdict", StreamID: verdict.StreamID,
				CandidateID: verdict.CandidateID, Action: verdict.Action, Code: code,
			})
		}
		if runner.earlyVerdict != nil {
			if runner.earlyVerdict.verdict.CandidateID == verdict.CandidateID {
				code = "duplicate_verdict"
			} else {
				code = "verdict_in_progress"
			}
			return runner.publishOutcome(ctx, envelope, EndpointOutcome{
				Kind: OutcomeIgnored, Operation: "verdict", StreamID: verdict.StreamID,
				CandidateID: verdict.CandidateID, Action: verdict.Action, Code: code,
			})
		}
		runner.earlyVerdict = &retainedVerdict{verdict: verdict, envelope: envelope.Clone()}
		if err := runner.publishState(ctx, envelope); err != nil {
			return err
		}
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomePending, Operation: "verdict", StreamID: verdict.StreamID,
			CandidateID: verdict.CandidateID, Action: verdict.Action,
			Code: "awaiting_candidate",
		})
	}
	if verdict.CandidateID != runner.pending.ID ||
		(verdict.StreamID != "" && verdict.StreamID != runner.pending.StreamID) {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeIgnored, Operation: "verdict", StreamID: verdict.StreamID,
			CandidateID: verdict.CandidateID, Action: verdict.Action,
			Code: "stale_candidate", Message: fmt.Sprintf("pending candidate is %q", runner.pending.ID),
		})
	}
	candidate := *runner.pending
	return runner.resolveCandidate(ctx, envelope, candidate, verdict.Action, "verdict", "external_verdict")
}

func (runner *endpointRunner) handleTick(ctx context.Context, envelope element.Envelope) error {
	tick, valid := tickPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "tick", Code: "invalid_payload",
			Message: fmt.Sprintf("tick payload has type %T", envelope.Payload),
		})
	}
	if runner.config.mode != EndpointExternal {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeIgnored, Operation: "tick", Code: "tick_not_required",
		})
	}
	if runner.hasTick && tick.NowNS < runner.lastTickNS {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeIgnored, Operation: "tick", Code: "stale_tick",
			Message: fmt.Sprintf("tick time %d is older than %d", tick.NowNS, runner.lastTickNS),
		})
	}
	runner.lastTickNS, runner.hasTick = tick.NowNS, true
	if runner.pending == nil {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeIgnored, Operation: "tick", Code: "no_pending_candidate",
		})
	}
	if runner.deadlineNS == 0 {
		runner.deadlineNS = saturatingAdd(tick.NowNS, runner.config.timeoutNS)
		if err := runner.publishState(ctx, envelope); err != nil {
			return err
		}
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomePending, Operation: "tick", StreamID: runner.pending.StreamID,
			CandidateID: runner.pending.ID, Code: "deadline_started",
		})
	}
	if tick.NowNS < runner.deadlineNS {
		if err := runner.publishState(ctx, envelope); err != nil {
			return err
		}
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomePending, Operation: "tick", StreamID: runner.pending.StreamID,
			CandidateID: runner.pending.ID, Code: "deadline_not_reached",
		})
	}
	candidate := *runner.pending
	return runner.resolveCandidate(ctx, envelope, candidate, runner.config.fallback, "tick", "deadline_fallback")
}

func (runner *endpointRunner) handleCommit(ctx context.Context, envelope element.Envelope) error {
	commit, valid := commitPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "commit", Code: "invalid_payload",
			Message: fmt.Sprintf("commit payload has type %T", envelope.Payload),
		})
	}
	if err := validateIdentifier("commit stream ID", commit.StreamID, false); err != nil {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "commit", Code: "invalid_stream_id", Message: err.Error(),
		})
	}
	if runner.config.mode != EndpointManual {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "commit", StreamID: commit.StreamID,
			Code: "commit_not_enabled", Message: "manual commit is accepted only in manual mode",
		})
	}
	if commit.StreamID != "" && runner.canceled.Has(commit.StreamID) {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeCanceled, Operation: "commit", StreamID: commit.StreamID,
			Code: "canceled_before_commit",
		})
	}
	command := GateCommand{
		Action: GateForceClose, StreamID: commit.StreamID, Reason: boundedReason(commit.Reason),
	}
	if runner.pending != nil && (commit.StreamID == "" || commit.StreamID == runner.pending.StreamID) {
		command.StreamID = runner.pending.StreamID
		command.CandidateID = runner.pending.ID
		runner.terminal.Add(runner.pending.ID)
		runner.pending, runner.deadlineNS = nil, 0
	}
	if _, err := runner.ports.command.Broadcast(ctx,
		derivedEnvelope(envelope, gateCommandType, ":force-close", command)); err != nil {
		return err
	}
	if err := runner.publishState(ctx, envelope); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, EndpointOutcome{
		Kind: OutcomeSucceeded, Operation: "commit", StreamID: command.StreamID,
		CandidateID: command.CandidateID, Action: GateForceClose, Code: "force_close_emitted",
	})
}

func (runner *endpointRunner) resolveCandidate(
	ctx context.Context, cause element.Envelope, candidate EndpointCandidate,
	action GateAction, operation, code string,
) error {
	command := GateCommand{
		Action: action, CandidateID: candidate.ID, StreamID: candidate.StreamID, Reason: code,
	}
	if _, err := runner.ports.command.Broadcast(ctx,
		derivedEnvelope(cause, gateCommandType, ":command", command)); err != nil {
		return err
	}
	runner.terminal.Add(candidate.ID)
	if runner.pending != nil && runner.pending.ID == candidate.ID {
		runner.pending, runner.deadlineNS = nil, 0
	}
	if err := runner.publishState(ctx, cause); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, EndpointOutcome{
		Kind: OutcomeSucceeded, Operation: operation, StreamID: candidate.StreamID,
		CandidateID: candidate.ID, Action: action, Code: code,
	})
}

func (runner *endpointRunner) handleCancel(ctx context.Context, envelope element.Envelope) error {
	request, valid := cancelPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "cancel", StreamID: envelopeScope(envelope),
			Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload),
		})
	}
	pendingStream := ""
	if runner.pending != nil {
		pendingStream = runner.pending.StreamID
	}
	streamID := firstNonempty(request.StreamID, envelopeScope(envelope), pendingStream)
	if err := validateIdentifier("cancel stream ID", streamID, true); err != nil {
		return runner.publishOutcome(ctx, envelope, EndpointOutcome{
			Kind: OutcomeRefused, Operation: "cancel", Code: "invalid_stream_id", Message: err.Error(),
		})
	}
	runner.canceled.Add(streamID)
	candidateID := ""
	if runner.pending != nil && runner.pending.StreamID == streamID {
		candidateID = runner.pending.ID
		runner.terminal.Add(candidateID)
		runner.pending, runner.deadlineNS = nil, 0
	}
	if runner.earlyVerdict != nil &&
		(runner.earlyVerdict.verdict.StreamID == streamID || runner.earlyVerdict.verdict.StreamID == "") {
		candidateID = firstNonempty(candidateID, runner.earlyVerdict.verdict.CandidateID)
		runner.terminal.Add(runner.earlyVerdict.verdict.CandidateID)
		runner.earlyVerdict = nil
	}
	if err := runner.publishState(ctx, envelope); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, EndpointOutcome{
		Kind: OutcomeCanceled, Operation: "cancel", StreamID: streamID,
		CandidateID: candidateID, Code: "canceled", Message: boundedReason(request.Reason),
	})
}

func (runner *endpointRunner) publishState(ctx context.Context, cause element.Envelope) error {
	sequence := saturatingIncrement(&runner.stateSequence)
	state := EndpointPolicyState{
		Sequence: sequence, Mode: runner.config.mode, DeadlineNS: runner.deadlineNS,
		LastTickNS: runner.lastTickNS,
		Fallback:   runner.config.fallback, CancellationsRetained: runner.canceled.Len(),
		TerminalsRetained: runner.terminal.Len(),
	}
	if runner.pending != nil {
		state.PendingCandidateID = runner.pending.ID
		state.PendingStreamID = runner.pending.StreamID
	}
	if runner.earlyVerdict != nil {
		state.PendingVerdictID = runner.earlyVerdict.verdict.CandidateID
		state.PendingVerdictStreamID = runner.earlyVerdict.verdict.StreamID
	}
	var envelope element.Envelope
	if cause.ItemID == "" {
		envelope = generatedEnvelope(runner.instance, sequence, policyStateType, "state", state)
	} else {
		envelope = derivedEnvelope(cause, policyStateType, fmt.Sprintf(":state:%d", sequence), state)
	}
	_, err := runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *endpointRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome EndpointOutcome,
) error {
	return runner.publishOutcomeWithSuffix(ctx, cause, outcome, ":outcome")
}

func (runner *endpointRunner) publishOutcomeWithSuffix(
	ctx context.Context, cause element.Envelope, outcome EndpointOutcome, suffix string,
) error {
	if cause.ItemID == "" {
		return errors.New("endpoint outcome requires a causal envelope")
	}
	_, err := runner.ports.outcome.Broadcast(ctx,
		derivedEnvelope(cause, endpointOutcomeType, suffix, outcome))
	return err
}

var _ element.Runnable = (*endpointRunner)(nil)
