package acoustic

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

type admissionRunner struct {
	instance   string
	config     resolvedAdmissionConfig
	ports      admissionPorts
	resolution element.ResolutionReporter

	gate               *coreperception.EnergyGate
	policyReady        bool
	mode               EndpointMode
	policySequence     uint64
	streamID           string
	source             string
	sampleRate         uint32
	pending            *EndpointCandidate
	speechActive       bool
	hasAdmitted        bool
	framesSeen         uint64
	framesAdmitted     uint64
	lastAdmittedItemID string
	gateGeneration     uint64
	stateSequence      uint64
	candidateSeq       uint64
	canceled           boundedSet
	terminal           boundedSet
}

func newAdmissionRunner(
	instance string, config resolvedAdmissionConfig, ports admissionPorts,
	resolution element.ResolutionReporter,
) *admissionRunner {
	return &admissionRunner{
		instance: instance, config: config, ports: ports, resolution: resolution,
		canceled: newBoundedSet(config.cancellationMemory), terminal: newBoundedSet(config.terminalMemory),
	}
}

func (runner *admissionRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := reportBuiltIn(
		runner.resolution, admissionRuntimeID, admissionImplementationRevision,
	); err != nil {
		return fmt.Errorf("attest acoustic.EnergyAdmission runtime: %w", err)
	}

	events := make(chan receivedInput)
	interrupts := make(chan element.Envelope)
	failures := make(chan error, 4)
	audioPermits := make(chan struct{}, 1)
	var receivers sync.WaitGroup
	receivers.Add(4)
	go receivePermittedAudio(ctx, runner.ports.audio, audioPermits, events, failures, &receivers)
	go receiveInputs(ctx, "policy", runner.ports.policy, events, failures, &receivers)
	go receiveInputs(ctx, "command", runner.ports.command, events, failures, &receivers)
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
			runner.grantAudio(audioPermits)
		case input := <-events:
			if err := runner.drainInterrupts(ctx, interrupts); err != nil {
				return err
			}
			var err error
			switch input.kind {
			case "audio":
				err = runner.handleAudio(ctx, input.envelope)
			case "policy":
				err = runner.handlePolicy(ctx, input.envelope)
			case "command":
				err = runner.handleCommand(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown EnergyAdmission input %q", input.kind)
			}
			if err != nil {
				return err
			}
			runner.grantAudio(audioPermits)
		}
	}
}

func (runner *admissionRunner) drainInterrupts(
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

func (runner *admissionRunner) grantAudio(permits chan<- struct{}) {
	if !runner.policyReady || runner.pending != nil {
		return
	}
	select {
	case permits <- struct{}{}:
	default:
	}
}

func (runner *admissionRunner) handlePolicy(ctx context.Context, envelope element.Envelope) error {
	state, valid := policyStatePayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "policy", Code: "invalid_payload",
			Message: fmt.Sprintf("policy payload has type %T", envelope.Payload),
		})
	}
	if !state.Mode.valid() || state.Sequence == 0 {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "policy", Code: "invalid_policy_state",
			Message: "policy state requires a valid mode and positive sequence",
		})
	}
	if runner.policyReady && state.Sequence <= runner.policySequence {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeIgnored, Operation: "policy", Code: "stale_policy_state",
			Message: fmt.Sprintf("policy sequence %d is not newer than %d", state.Sequence, runner.policySequence),
		})
	}
	if runner.streamID != "" && runner.policyReady && state.Mode != runner.mode {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "policy", StreamID: runner.streamID,
			Code:    "policy_change_during_stream",
			Message: fmt.Sprintf("cannot change endpoint mode from %s to %s during an active stream", runner.mode, state.Mode),
		})
	}
	runner.policyReady = true
	runner.mode = state.Mode
	runner.policySequence = state.Sequence
	if err := runner.publishState(ctx, envelope); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
		Kind: OutcomeSucceeded, Operation: "policy", Code: "policy_applied",
	})
}

func (runner *admissionRunner) handleAudio(ctx context.Context, envelope element.Envelope) error {
	input, valid := inputFramePayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "audio", Code: "invalid_payload",
			Message: fmt.Sprintf("audio payload has type %T", envelope.Payload),
		})
	}
	if err := validateIdentifier("stream ID", input.StreamID, true); err != nil {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "audio", Code: "missing_stream_id",
			Message: err.Error(),
		})
	}
	if runner.canceled.Has(input.StreamID) {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeCanceled, Operation: "audio", StreamID: input.StreamID,
			Code: "canceled_before_audio", Message: "the stream was canceled before this audio arrived",
		})
	}
	if runner.terminal.Has(input.StreamID) {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeIgnored, Operation: "audio", StreamID: input.StreamID,
			Code: "stream_terminal", Message: "late audio for a terminal stream was ignored",
		})
	}
	if !runner.policyReady {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "audio", StreamID: input.StreamID,
			Code: "policy_not_ready", Message: "endpoint policy state has not arrived",
		})
	}
	if err := runner.validateFrame(input.Frame); err != nil {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "audio", StreamID: input.StreamID,
			Code: "invalid_frame", Message: err.Error(),
		})
	}
	if runner.streamID != "" && input.StreamID != runner.streamID {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "audio", StreamID: input.StreamID,
			Code: "stream_in_progress", Message: fmt.Sprintf("stream %q is active", runner.streamID),
		})
	}
	if runner.streamID == "" {
		runner.streamID = input.StreamID
		runner.source = input.Frame.Source
		runner.sampleRate = input.Frame.SampleRateHz
		if err := runner.replaceGate(input.Frame.SampleRateHz); err != nil {
			runner.clearStream()
			return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
				Kind: OutcomeFailed, Operation: "audio", StreamID: input.StreamID,
				Code: "gate_create_failed", Message: err.Error(),
			})
		}
	} else {
		if input.Frame.Source != runner.source {
			return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
				Kind: OutcomeRefused, Operation: "audio", StreamID: input.StreamID,
				Code: "source_changed", Message: fmt.Sprintf("source changed from %q to %q", runner.source, input.Frame.Source),
			})
		}
		if input.Frame.SampleRateHz != runner.sampleRate {
			if runner.hasAdmitted || runner.speechActive || runner.pending != nil {
				return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
					Kind: OutcomeRefused, Operation: "audio", StreamID: input.StreamID,
					Code:    "sample_rate_change_active",
					Message: fmt.Sprintf("cannot change sample rate from %d to %d after admission", runner.sampleRate, input.Frame.SampleRateHz),
				})
			}
			if err := runner.replaceGate(input.Frame.SampleRateHz); err != nil {
				return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
					Kind: OutcomeFailed, Operation: "audio", StreamID: input.StreamID,
					Code: "gate_rebuild_failed", Message: err.Error(),
				})
			}
			runner.sampleRate = input.Frame.SampleRateHz
		}
	}

	saturatingIncrement(&runner.framesSeen)
	result, err := runner.gate.Push(input.Frame.PCM16LE)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeFailed, Operation: "audio", StreamID: input.StreamID,
			Code: "gate_push_failed", Message: err.Error(),
		})
	}
	if result.Started {
		runner.speechActive = true
		if err := runner.publishActivity(ctx, envelope, SpeechActivity{
			Kind: SpeechStarted, StreamID: runner.streamID, Source: runner.source,
			AtNS:         firstNonzero(input.Frame.CapturedNS, envelope.CaptureNS, envelope.ReceiveNS),
			SampleRateHz: runner.sampleRate, AudioStartMS: result.AudioStartMS,
		}); err != nil {
			return err
		}
	}
	admitted := result.Audio
	if runner.mode == EndpointManual {
		admitted = input.Frame.PCM16LE
	}
	if len(admitted) != 0 {
		frame := input.Frame
		frame.PCM16LE = append([]byte(nil), admitted...)
		batch := perceptionelements.AudioBatch{
			StreamID: runner.streamID, Frames: []coreperception.Frame{frame},
		}
		admittedEnvelope := derivedEnvelope(envelope, admittedAudioType, ":admitted", batch)
		if _, err := runner.ports.admitted.Broadcast(ctx, admittedEnvelope); err != nil {
			return err
		}
		runner.lastAdmittedItemID = admittedEnvelope.ItemID
		runner.hasAdmitted = true
		saturatingIncrement(&runner.framesAdmitted)
	}
	if result.Stopped {
		sequence := saturatingIncrement(&runner.candidateSeq)
		candidate := EndpointCandidate{
			ID:       candidateIdentifier(runner.instance, runner.streamID, sequence),
			StreamID: runner.streamID, Source: runner.source,
			DetectedNS: firstNonzero(input.Frame.CapturedNS, envelope.CaptureNS, envelope.ReceiveNS),
			AudioEndMS: result.AudioEndMS, SilenceNS: result.SilenceNS,
			SampleRateHz: runner.sampleRate, Sequence: sequence,
		}
		runner.pending = &candidate
		if _, err := runner.ports.candidate.Broadcast(ctx,
			derivedEnvelope(envelope, candidateType, ":candidate", candidate)); err != nil {
			return err
		}
	}
	if err := runner.publishState(ctx, envelope); err != nil {
		return err
	}
	outcome := AdmissionOutcome{
		Kind: OutcomeSucceeded, Operation: "audio", StreamID: runner.streamID,
		Code: "observed",
	}
	if len(admitted) != 0 {
		outcome.Code = "admitted"
	}
	if runner.pending != nil {
		outcome.Kind, outcome.Code, outcome.CandidateID = OutcomePending, "endpoint_candidate", runner.pending.ID
	}
	return runner.publishOutcome(ctx, envelope, outcome)
}

func (runner *admissionRunner) validateFrame(frame coreperception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.Kind != coreperception.FrameAudio {
		return errors.New("acoustic admission requires an audio frame")
	}
	if err := validateIdentifier("audio source", frame.Source, true); err != nil {
		return err
	}
	if len(frame.PCM16LE) > runner.config.maxFrameBytes {
		return fmt.Errorf("audio frame has %d bytes, maximum is %d", len(frame.PCM16LE), runner.config.maxFrameBytes)
	}
	if frame.SampleRateHz > runner.config.maxSampleRateHz {
		return fmt.Errorf("sample rate %d exceeds maximum %d", frame.SampleRateHz, runner.config.maxSampleRateHz)
	}
	if runner.config.source != "" && frame.Source != runner.config.source {
		return fmt.Errorf("source %q does not match configured source %q", frame.Source, runner.config.source)
	}
	return nil
}

func (runner *admissionRunner) replaceGate(rate uint32) error {
	gate, err := coreperception.NewEnergyGate(runner.config.gate, rate)
	if err != nil {
		return err
	}
	runner.gate = gate
	saturatingIncrement(&runner.gateGeneration)
	return nil
}

func (runner *admissionRunner) handleCommand(ctx context.Context, envelope element.Envelope) error {
	command, valid := gateCommandPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "command", Code: "invalid_payload",
			Message: fmt.Sprintf("command payload has type %T", envelope.Payload),
		})
	}
	if err := validateIdentifier("command stream ID", command.StreamID, false); err != nil {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "command", Code: "invalid_stream_id", Message: err.Error(),
		})
	}
	if err := validateIdentifier("command candidate ID", command.CandidateID, false); err != nil {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "command", StreamID: command.StreamID,
			Code: "invalid_candidate_id", Message: err.Error(),
		})
	}
	if command.StreamID != "" && runner.canceled.Has(command.StreamID) {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeCanceled, Operation: "command", StreamID: command.StreamID,
			CandidateID: command.CandidateID, Code: "stream_canceled",
		})
	}
	if command.StreamID != "" && runner.terminal.Has(command.StreamID) {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeIgnored, Operation: "command", StreamID: command.StreamID,
			CandidateID: command.CandidateID, Code: "stream_terminal",
		})
	}
	if command.StreamID != "" && runner.streamID != "" && command.StreamID != runner.streamID {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeIgnored, Operation: "command", StreamID: command.StreamID,
			CandidateID: command.CandidateID, Code: "scope_not_active",
			Message: fmt.Sprintf("active stream is %q", runner.streamID),
		})
	}
	switch command.Action {
	case GateClose, GateReopen:
		if runner.pending == nil {
			return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
				Kind: OutcomeIgnored, Operation: "command", StreamID: firstNonempty(command.StreamID, runner.streamID),
				CandidateID: command.CandidateID, Code: "no_pending_candidate",
			})
		}
		if command.CandidateID == "" || command.CandidateID != runner.pending.ID {
			return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
				Kind: OutcomeIgnored, Operation: "command", StreamID: runner.streamID,
				CandidateID: command.CandidateID, Code: "stale_candidate",
				Message: fmt.Sprintf("pending candidate is %q", runner.pending.ID),
			})
		}
		if command.Action == GateReopen {
			runner.gate.Reopen()
			candidateID := runner.pending.ID
			runner.pending = nil
			if err := runner.publishState(ctx, envelope); err != nil {
				return err
			}
			return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
				Kind: OutcomeSucceeded, Operation: "command", StreamID: runner.streamID,
				CandidateID: candidateID, Code: "reopened",
			})
		}
		return runner.closeStream(ctx, envelope, runner.pending.AudioEndMS, runner.pending.ID, "closed")
	case GateForceClose:
		if runner.streamID == "" || !runner.hasAdmitted {
			return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
				Kind: OutcomeRefused, Operation: "command", StreamID: firstNonempty(command.StreamID, runner.streamID),
				CandidateID: command.CandidateID, Code: "empty_stream",
				Message: "cannot commit an empty audio stream",
			})
		}
		if command.CandidateID != "" && (runner.pending == nil || command.CandidateID != runner.pending.ID) {
			return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
				Kind: OutcomeIgnored, Operation: "command", StreamID: runner.streamID,
				CandidateID: command.CandidateID, Code: "stale_candidate",
			})
		}
		endMS := 0
		candidateID := command.CandidateID
		if runner.pending != nil {
			endMS, candidateID = runner.pending.AudioEndMS, runner.pending.ID
		} else if runner.gate != nil {
			endMS, _ = runner.gate.ForceStop()
		}
		return runner.closeStream(ctx, envelope, endMS, candidateID, "force_closed")
	default:
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "command", StreamID: command.StreamID,
			CandidateID: command.CandidateID, Code: "invalid_action",
			Message: fmt.Sprintf("unknown gate action %q", command.Action),
		})
	}
}

func (runner *admissionRunner) closeStream(
	ctx context.Context, cause element.Envelope, endMS int, candidateID, code string,
) error {
	streamID, source, rate := runner.streamID, runner.source, runner.sampleRate
	if runner.lastAdmittedItemID == "" {
		return errors.New("acoustic admission cannot close a stream without exact admitted-audio evidence")
	}
	if runner.speechActive {
		if err := runner.publishActivity(ctx, cause, SpeechActivity{
			Kind: SpeechStopped, StreamID: streamID, Source: source,
			AtNS:         firstNonzero(cause.CaptureNS, cause.ReceiveNS),
			SampleRateHz: rate, AudioEndMS: endMS,
		}); err != nil {
			return err
		}
	}
	flush := perceptionelements.Flush{
		StreamID: streamID, AfterItemID: runner.lastAdmittedItemID,
	}
	flushEnvelope := derivedEnvelope(cause, audioFlushType, ":flush", flush)
	flushEnvelope.CausalParents = appendUnique(
		flushEnvelope.CausalParents, runner.lastAdmittedItemID,
	)
	if _, err := runner.ports.endpoint.Broadcast(ctx, flushEnvelope); err != nil {
		return err
	}
	runner.terminal.Add(streamID)
	runner.clearStream()
	if err := runner.publishState(ctx, cause); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, AdmissionOutcome{
		Kind: OutcomeSucceeded, Operation: "command", StreamID: streamID,
		CandidateID: candidateID, Code: code,
	})
}

func (runner *admissionRunner) handleCancel(ctx context.Context, envelope element.Envelope) error {
	request, valid := cancelPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "cancel", StreamID: envelopeScope(envelope),
			Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload),
		})
	}
	streamID := firstNonempty(request.StreamID, envelopeScope(envelope), runner.streamID)
	if err := validateIdentifier("cancel stream ID", streamID, true); err != nil {
		return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
			Kind: OutcomeRefused, Operation: "cancel", Code: "invalid_stream_id", Message: err.Error(),
		})
	}
	runner.canceled.Add(streamID)
	runner.terminal.Add(streamID)
	if streamID == runner.streamID {
		runner.clearStream()
	}
	if err := runner.publishState(ctx, envelope); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, AdmissionOutcome{
		Kind: OutcomeCanceled, Operation: "cancel", StreamID: streamID,
		Code: "canceled", Message: boundedReason(request.Reason),
	})
}

func (runner *admissionRunner) clearStream() {
	runner.gate = nil
	runner.streamID = ""
	runner.source = ""
	runner.sampleRate = 0
	runner.pending = nil
	runner.speechActive = false
	runner.hasAdmitted = false
	runner.framesSeen = 0
	runner.framesAdmitted = 0
	runner.lastAdmittedItemID = ""
	runner.gateGeneration = 0
}

func (runner *admissionRunner) publishActivity(
	ctx context.Context, cause element.Envelope, activity SpeechActivity,
) error {
	suffix := ":activity:" + string(activity.Kind)
	_, err := runner.ports.activity.Broadcast(ctx, derivedEnvelope(cause, activityType, suffix, activity))
	return err
}

func (runner *admissionRunner) publishState(ctx context.Context, cause element.Envelope) error {
	sequence := saturatingIncrement(&runner.stateSequence)
	state := AdmissionState{
		Sequence: sequence, Phase: runner.phase(), Mode: runner.mode,
		PolicySequence: runner.policySequence, StreamID: runner.streamID, Source: runner.source,
		SampleRateHz: runner.sampleRate, SpeechActive: runner.speechActive,
		FramesSeen: runner.framesSeen, FramesAdmitted: runner.framesAdmitted,
		GateGeneration:        runner.gateGeneration,
		CancellationsRetained: runner.canceled.Len(), TerminalsRetained: runner.terminal.Len(),
	}
	if runner.pending != nil {
		state.CandidateID = runner.pending.ID
	}
	var envelope element.Envelope
	if cause.ItemID == "" {
		envelope = generatedEnvelope(runner.instance, sequence, admissionStateType, "state", state)
	} else {
		envelope = derivedEnvelope(cause, admissionStateType, fmt.Sprintf(":state:%d", sequence), state)
	}
	_, err := runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *admissionRunner) phase() AdmissionPhase {
	switch {
	case !runner.policyReady:
		return AdmissionAwaitingPolicy
	case runner.pending != nil:
		return AdmissionAwaiting
	case runner.streamID == "":
		return AdmissionIdle
	case runner.speechActive:
		return AdmissionSpeaking
	default:
		return AdmissionListening
	}
}

func (runner *admissionRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome AdmissionOutcome,
) error {
	if cause.ItemID == "" {
		return errors.New("admission outcome requires a causal envelope")
	}
	_, err := runner.ports.outcome.Broadcast(ctx,
		derivedEnvelope(cause, admissionOutcomeType, ":outcome", outcome))
	return err
}

func firstNonzero(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

var _ element.Runnable = (*admissionRunner)(nil)
