package speech

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	defaultFrameDurationMS = 100
	maximumFrameDurationMS = 1000
	shutdownCleanupTimeout = 250 * time.Millisecond
)

type PlaybackConfig struct {
	Sink            string `json:"sink"`
	FrameDurationMS int    `json:"frame_duration_ms,omitempty"`
	MaxChunkBytes   int    `json:"max_chunk_bytes,omitempty"`
	CancelMemory    int    `json:"cancel_memory,omitempty"`
}

func decodePlaybackConfig(source json.RawMessage) (PlaybackConfig, error) {
	var config PlaybackConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return PlaybackConfig{}, err
	}
	config.Sink = strings.TrimSpace(config.Sink)
	if config.Sink == "" {
		return PlaybackConfig{}, errors.New("playback config requires a sink reference")
	}
	if config.FrameDurationMS == 0 {
		config.FrameDurationMS = defaultFrameDurationMS
	}
	if config.MaxChunkBytes == 0 {
		config.MaxChunkBytes = defaultMaxChunkBytes
	}
	if config.CancelMemory == 0 {
		config.CancelMemory = defaultCancelMemory
	}
	if config.FrameDurationMS < 1 || config.FrameDurationMS > maximumFrameDurationMS {
		return PlaybackConfig{}, fmt.Errorf("frame_duration_ms must be between 1 and %d", maximumFrameDurationMS)
	}
	if config.MaxChunkBytes < 2 || config.MaxChunkBytes > maximumBoundBytes {
		return PlaybackConfig{}, fmt.Errorf("max_chunk_bytes must be between 2 and %d", maximumBoundBytes)
	}
	if config.CancelMemory < 1 || config.CancelMemory > maximumCancelMemory {
		return PlaybackConfig{}, fmt.Errorf("cancel_memory must be between 1 and %d", maximumCancelMemory)
	}
	return config, nil
}

type playbackFactory struct{}

func (playbackFactory) Descriptor() element.Descriptor { return PlaybackDescriptor() }

func (playbackFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodePlaybackConfig(source)
	return err
}

func (playbackFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodePlaybackConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("speech.Playback %s config: %w", mount.InstanceID, err)
	}
	sinkService, _, found := mount.Services.Lookup(PlaybackSinkRegistryService)
	if !found {
		return nil, fmt.Errorf("speech.Playback %s has no sink registry service", mount.InstanceID)
	}
	sinkRegistry, ok := sinkService.(*PlaybackSinkRegistry)
	if !ok || sinkRegistry == nil {
		return nil, fmt.Errorf("playback sink registry service has type %T", sinkService)
	}
	entry, err := sinkRegistry.resolve(config.Sink)
	if err != nil {
		return nil, err
	}
	sink, err := entry.factory()
	if err != nil {
		return nil, fmt.Errorf("create playback sink %q: %w", config.Sink, err)
	}
	if err := verifySink(config.Sink, entry.descriptor, sink); err != nil {
		return nil, errors.Join(err, closeResource(sink))
	}
	if err := mount.Lifecycle.Defer("close-playback-sink", func(context.Context) error {
		return closeResource(sink)
	}); err != nil {
		return nil, errors.Join(err, closeResource(sink))
	}
	ledgerService, _, found := mount.Services.Lookup(IrreversibilityLedgerService)
	if !found {
		return nil, fmt.Errorf("speech.Playback %s has no irreversibility ledger service", mount.InstanceID)
	}
	ledger, ok := ledgerService.(*action.Ledger)
	if !ok || ledger == nil {
		return nil, fmt.Errorf("irreversibility ledger service has type %T", ledgerService)
	}
	scheduler := clock.Scheduler(clock.NewSystem())
	if schedulerService, _, found := mount.Services.Lookup(PlaybackSchedulerService); found {
		var valid bool
		scheduler, valid = schedulerService.(clock.Scheduler)
		if !valid || scheduler == nil {
			return nil, fmt.Errorf("playback scheduler service has type %T", schedulerService)
		}
	}
	audioInput, err := mount.Ports.Input("audio")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	statusOutput, err := mount.Ports.Output("status")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	resolvedOutput, err := mount.Ports.Output("resolved")
	if err != nil {
		return nil, err
	}
	reservedOutput, err := mount.Ports.Output("reserved")
	if err != nil {
		return nil, err
	}
	begunOutput, err := mount.Ports.Output("begun")
	if err != nil {
		return nil, err
	}
	textOutput, err := mount.Ports.Output("text_committed")
	if err != nil {
		return nil, err
	}
	audioOutput, err := mount.Ports.Output("audio_emitted")
	if err != nil {
		return nil, err
	}
	endedOutput, err := mount.Ports.Output("ended")
	if err != nil {
		return nil, err
	}
	releasedOutput, err := mount.Ports.Output("released")
	if err != nil {
		return nil, err
	}
	return &playbackRunner{
		instance: mount.InstanceID, config: config, sink: sink, ledger: ledger,
		scheduler: scheduler, sinkReference: config.Sink, sinkDescriptor: entry.descriptor,
		audioInput: audioInput, cancelInput: cancelInput,
		statusOutput: statusOutput, outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		reservedOutput: reservedOutput, begunOutput: begunOutput, textOutput: textOutput,
		audioOutput: audioOutput, endedOutput: endedOutput, releasedOutput: releasedOutput,
		resolution:     mount.Resolution,
		pendingCancels: newCancellationMemory(config.CancelMemory),
	}, nil
}

type audioCommand struct {
	envelope element.Envelope
	frame    AudioFrame
	valid    bool
}

type activePlayback struct {
	cause            element.Envelope
	utterance        action.Utterance
	expectedOffset   uint64
	sampleRate       uint32
	nextSendNS       uint64
	playedNS         uint64
	receiptSequence  uint64
	lastReceiptID    string
	emittingReported bool
	reserved         bool
	begun            bool
}

type playResult struct {
	err error
}

type playbackRunner struct {
	instance       string
	config         PlaybackConfig
	sink           PlaybackSink
	ledger         *action.Ledger
	scheduler      clock.Scheduler
	sinkReference  string
	sinkDescriptor v1.Descriptor
	audioInput     element.InputPort
	cancelInput    element.InputPort
	statusOutput   element.OutputPort
	outcomeOutput  element.OutputPort
	resolvedOutput element.OutputPort
	reservedOutput element.OutputPort
	begunOutput    element.OutputPort
	textOutput     element.OutputPort
	audioOutput    element.OutputPort
	endedOutput    element.OutputPort
	releasedOutput element.OutputPort
	resolution     element.ResolutionReporter
	pendingCancels *cancellationMemory
	active         *activePlayback
	discardID      string
}

func (runner *playbackRunner) Run(parent context.Context) (runErr error) {
	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	actual, err := liveSinkDescriptor(runner.sinkReference, runner.sinkDescriptor, runner.sink)
	if err != nil {
		return fmt.Errorf("resolve playback sink %q before readiness: %w", runner.sinkReference, err)
	}
	if err := reportPlaybackLiveResolution(runner.resolution, actual); err != nil {
		return fmt.Errorf("attest playback sink %q: %w", runner.sinkReference, err)
	}
	if err := runner.publishResolution(ctx); err != nil {
		return err
	}
	work := make(chan audioCommand)
	interrupts := make(chan element.Envelope)
	failures := make(chan error, 2)
	var receivers sync.WaitGroup
	receivers.Add(2)
	go receiveAudio(ctx, runner.audioInput, work, failures, &receivers)
	go receiveInterrupts(ctx, runner.cancelInput, interrupts, failures, &receivers)
	defer func() {
		if runner.active != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), shutdownCleanupTimeout)
			runErr = errors.Join(runErr,
				runner.finishCancelled(cleanupCtx, runner.active, "playback stopped"))
			cleanupCancel()
		}
		stop(nil)
		receivers.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case interrupt := <-interrupts:
			if err := runner.handleIdleCancel(ctx, interrupt); err != nil {
				return err
			}
		case command := <-work:
			if err := runner.handleAudio(ctx, command, interrupts, failures); err != nil {
				return err
			}
		}
	}
}

func (runner *playbackRunner) handleAudio(
	ctx context.Context, command audioCommand, interrupts <-chan element.Envelope, failures <-chan error,
) error {
	if !command.valid {
		return runner.publishPlaybackTerminal(ctx, command.envelope, Transition{
			Stage: StagePlayback, State: StateRefused,
			Reason: fmt.Sprintf("audio payload has type %T", command.envelope.Payload),
		}, PlaybackOutcome{
			Kind: OutcomeRefused, Code: "invalid_payload",
			Message: fmt.Sprintf("audio payload has type %T", command.envelope.Payload),
		})
	}
	frame := command.frame
	if err := validateAudioFrame(frame, runner.config.MaxChunkBytes); err != nil {
		if runner.active != nil && frame.UtteranceID == runner.active.utterance.ID {
			active := runner.active
			runner.active = nil
			runner.discardID = frame.UtteranceID
			return runner.finishFailed(ctx, active, "invalid_audio_frame", err)
		}
		return runner.publishPlaybackTerminal(ctx, command.envelope, Transition{
			UtteranceID: frame.UtteranceID, Stage: StagePlayback, State: StateRefused,
			Reason: err.Error(),
		}, PlaybackOutcome{
			UtteranceID: frame.UtteranceID, Kind: OutcomeRefused,
			Code: "invalid_audio_frame", Message: err.Error(),
		})
	}
	if runner.discardID != "" {
		if frame.UtteranceID != runner.discardID {
			return runner.publishPlaybackTerminal(ctx, command.envelope, Transition{
				UtteranceID: frame.UtteranceID, Stage: StagePlayback, State: StateRefused,
				Reason: fmt.Sprintf("waiting for terminal frame for cancelled utterance %q", runner.discardID),
			}, PlaybackOutcome{
				UtteranceID: frame.UtteranceID, Kind: OutcomeRefused,
				Code: "interleaved_utterance", Message: "audio streams require an explicit arbiter",
			})
		}
		if frame.Kind == AudioEnd {
			runner.discardID = ""
		}
		return nil
	}

	switch frame.Kind {
	case AudioBegin:
		return runner.begin(ctx, command.envelope, frame)
	case AudioChunk:
		if runner.active == nil || runner.active.utterance.ID != frame.UtteranceID {
			return runner.unexpectedFrame(ctx, command.envelope, frame, "audio chunk has no matching begin")
		}
		return runner.play(ctx, command, interrupts, failures)
	case AudioEnd:
		if runner.active == nil || runner.active.utterance.ID != frame.UtteranceID {
			return runner.unexpectedFrame(ctx, command.envelope, frame, "audio end has no matching begin")
		}
		active := runner.active
		runner.active = nil
		switch frame.Terminal.Kind {
		case OutcomeSucceeded:
			return runner.finishPlayed(ctx, active)
		case OutcomeCancelled:
			return runner.finishCancelled(ctx, active, frame.Terminal.Message)
		default:
			return runner.finishFailed(ctx, active,
				firstNonempty(frame.Terminal.Code, "synthesis_failed"),
				errors.New(firstNonempty(frame.Terminal.Message, "synthesis failed")))
		}
	default:
		return runner.unexpectedFrame(ctx, command.envelope, frame, "unknown audio frame kind")
	}
}

func (runner *playbackRunner) begin(
	ctx context.Context, cause element.Envelope, frame AudioFrame,
) error {
	if runner.active != nil {
		return runner.unexpectedFrame(ctx, cause, frame,
			fmt.Sprintf("utterance %q is already active; insert an explicit arbiter", runner.active.utterance.ID))
	}
	// Reaching this Prepared input is the graph's explicit authorization to
	// speak. SpeechAuthority is retained as resolution provenance only; legacy
	// values such as "silent" must not smuggle a provider-owned routing policy
	// back into a graph that deliberately connected deliberative output here.
	authority := firstNonempty(strings.TrimSpace(frame.SpeechAuthority), "graph")
	commitment := action.Commitment{
		ID: frame.Utterance.ID, Kind: action.KindSpeech,
		AssistantItemIDs: frame.Utterance.AssistantItemIDs,
		SourceRevision:   frame.Utterance.SourceRevision, Phase: frame.Utterance.Phase,
		SpeechAuthority: authority,
	}
	if err := runner.ledger.Prepare(commitment); err != nil {
		runner.discardID = frame.UtteranceID
		return runner.publishPlaybackTerminal(ctx, cause, Transition{
			UtteranceID: frame.UtteranceID, Stage: StagePlayback, State: StateRefused,
			Reason: err.Error(),
		}, PlaybackOutcome{
			UtteranceID: frame.UtteranceID, Kind: OutcomeRefused,
			Code: "ledger_prepare_failed", Message: err.Error(),
		})
	}
	if reason, cancelled := runner.pendingCancels.take(frame.UtteranceID); cancelled {
		_, _ = runner.ledger.Cancel(frame.UtteranceID, reason)
		commitment, _ := runner.ledger.Lookup(frame.UtteranceID)
		runner.discardID = frame.UtteranceID
		return runner.publishPlaybackTerminal(ctx, cause, Transition{
			UtteranceID: frame.UtteranceID, Stage: StagePlayback, State: StateCancelled,
			Reason: reason, LedgerState: commitment.State,
		}, PlaybackOutcome{
			UtteranceID: frame.UtteranceID, Kind: OutcomeCancelled,
			Code: "cancelled_before_commitment", Message: reason,
		})
	}
	if err := runner.ledger.Queue(frame.UtteranceID); err != nil {
		runner.discardID = frame.UtteranceID
		_, _ = runner.ledger.Cancel(frame.UtteranceID, err.Error())
		return runner.publishPlaybackTerminal(ctx, cause, Transition{
			UtteranceID: frame.UtteranceID, Stage: StagePlayback, State: StateFailed,
			Reason: err.Error(),
		}, PlaybackOutcome{
			UtteranceID: frame.UtteranceID, Kind: OutcomeFailed,
			Code: "ledger_queue_failed", Message: err.Error(),
		})
	}
	active := &activePlayback{cause: cause.Clone(), utterance: frame.Utterance}
	if reserving, ok := runner.sink.(action.SpeechReservationSink); ok {
		if err := reserving.Reserve(frame.Utterance); err != nil {
			runner.discardID = frame.UtteranceID
			_, _ = runner.ledger.Cancel(frame.UtteranceID, "sink reservation failed")
			return runner.publishPlaybackTerminal(ctx, cause, Transition{
				UtteranceID: frame.UtteranceID, Stage: StagePlayback, State: StateFailed,
				Reason: err.Error(),
			}, PlaybackOutcome{
				UtteranceID: frame.UtteranceID, Kind: OutcomeFailed,
				Code: "sink_reservation_failed", Message: err.Error(),
			})
		}
		active.reserved = true
		if err := runner.publishReceipt(ctx, active, runner.reservedOutput, PlaybackReserved,
			action.Frame{}, action.Outcome{}); err != nil {
			reserving.CancelReservation(frame.Utterance)
			_, _ = runner.ledger.Cancel(frame.UtteranceID, "publish sink reservation receipt failed")
			return err
		}
	}
	if err := runner.sink.Begin(ctx, frame.Utterance); err != nil {
		runner.discardID = frame.UtteranceID
		var receiptErr error
		if active.reserved {
			runner.sink.(action.SpeechReservationSink).CancelReservation(frame.Utterance)
			receiptErr = runner.publishReceipt(ctx, active, runner.releasedOutput, PlaybackReleased,
				action.Frame{}, action.Outcome{Reason: "sink begin failed"})
		}
		_, _ = runner.ledger.Cancel(frame.UtteranceID, "sink begin failed")
		terminalErr := runner.publishPlaybackTerminal(ctx, cause, Transition{
			UtteranceID: frame.UtteranceID, Stage: StagePlayback, State: StateFailed,
			Reason: err.Error(),
		}, PlaybackOutcome{
			UtteranceID: frame.UtteranceID, Kind: OutcomeFailed,
			Code: "sink_begin_failed", Message: err.Error(),
		})
		return errors.Join(receiptErr, terminalErr)
	}
	active.begun = true
	if err := runner.publishReceipt(ctx, active, runner.begunOutput, PlaybackBegun,
		action.Frame{}, action.Outcome{}); err != nil {
		_, _ = runner.ledger.Cancel(frame.UtteranceID, "publish sink begin receipt failed")
		return errors.Join(err, runner.endSink(ctx, active, action.Outcome{Reason: err.Error()}))
	}
	if err := runner.publishReceipt(ctx, active, runner.textOutput, PlaybackTextCommitted,
		action.Frame{}, action.Outcome{}); err != nil {
		_, _ = runner.ledger.Cancel(frame.UtteranceID, "publish sink text receipt failed")
		return errors.Join(err, runner.endSink(ctx, active, action.Outcome{Reason: err.Error()}))
	}
	runner.active = active
	commitment, _ = runner.ledger.Lookup(frame.UtteranceID)
	return runner.publishTransition(ctx, cause, Transition{
		UtteranceID: frame.UtteranceID, Stage: StagePlayback,
		State: StateQueued, LedgerState: commitment.State,
	})
}

func (runner *playbackRunner) play(
	ctx context.Context, command audioCommand, interrupts <-chan element.Envelope, failures <-chan error,
) error {
	active := runner.active
	operationCtx, cancel := context.WithCancelCause(ctx)
	resultChannel := make(chan playResult, 1)
	go func() { resultChannel <- playResult{err: runner.playChunk(operationCtx, active, command.frame.Chunk)} }()
	for {
		select {
		case <-ctx.Done():
			cancel(context.Cause(ctx))
			<-resultChannel
			return nil
		case err := <-failures:
			cancel(err)
			<-resultChannel
			return err
		case interrupt := <-interrupts:
			request, valid := cancelPayload(interrupt.Payload)
			if !valid {
				if err := runner.publishCancelReply(ctx, interrupt, "", OutcomeRefused,
					false, 0, "invalid_payload", fmt.Sprintf("cancel payload has type %T", interrupt.Payload)); err != nil {
					cancel(err)
					<-resultChannel
					return err
				}
				continue
			}
			target := cancelTarget(interrupt, request)
			if target != active.utterance.ID {
				if target != "" {
					runner.pendingCancels.remember(target, request.Reason)
				}
				if err := runner.publishCancelReply(ctx, interrupt, target, OutcomeIgnored,
					false, 0, "scope_not_active", fmt.Sprintf("active utterance is %q", active.utterance.ID)); err != nil {
					cancel(err)
					<-resultChannel
					return err
				}
				continue
			}
			reason := firstNonempty(strings.TrimSpace(request.Reason), "cancelled")
			cancel(errors.New(reason))
			<-resultChannel
			runner.active = nil
			runner.discardID = active.utterance.ID
			return runner.finishCancelled(ctx, active, reason)
		case result := <-resultChannel:
			cancel(nil)
			if result.err != nil {
				runner.active = nil
				runner.discardID = active.utterance.ID
				return runner.finishFailed(ctx, active, "playback_error", result.err)
			}
			return nil
		}
	}
}

func (runner *playbackRunner) playChunk(
	ctx context.Context, active *activePlayback, chunk v1.SpeechChunk,
) error {
	if active.sampleRate == 0 {
		active.sampleRate = chunk.SampleRateHz
	} else if active.sampleRate != chunk.SampleRateHz {
		return fmt.Errorf("audio sample rate changed from %d to %d", active.sampleRate, chunk.SampleRateHz)
	}
	if chunk.SampleOffset != active.expectedOffset {
		return fmt.Errorf("audio chunk offset is %d, want %d", chunk.SampleOffset, active.expectedOffset)
	}
	frameBytes := uint64(chunk.SampleRateHz) * uint64(runner.config.FrameDurationMS) * 2 / 1000
	if frameBytes == 0 || frameBytes > uint64(math.MaxInt) {
		return errors.New("paced audio frame size is invalid")
	}
	if frameBytes%2 != 0 {
		frameBytes--
	}
	if frameBytes == 0 {
		return errors.New("paced audio frame contains no complete PCM16 sample")
	}
	remaining := chunk.PCM16LE
	for len(remaining) > 0 {
		size := int(frameBytes)
		if size > len(remaining) {
			size = len(remaining)
		}
		payload := remaining[:size]
		remaining = remaining[size:]
		final := chunk.Final && len(remaining) == 0
		if err := runner.waitUntil(ctx, active.nextSendNS); err != nil {
			return err
		}
		durationNS := uint64(size/2) * uint64(time.Second) / uint64(chunk.SampleRateHz)
		if durationNS == 0 || durationNS > uint64(math.MaxInt64) {
			return errors.New("paced audio frame duration is invalid")
		}
		if err := runner.ledger.Emit(active.utterance.ID); err != nil {
			return err
		}
		emitted := action.Frame{
			PCM16LE: append([]byte(nil), payload...), SampleRateHz: chunk.SampleRateHz,
			Duration: time.Duration(durationNS), Final: final,
		}
		if err := runner.sink.Audio(ctx, active.utterance, emitted); err != nil {
			return err
		}
		if err := runner.publishReceipt(ctx, active, runner.audioOutput, PlaybackAudioEmitted,
			emitted, action.Outcome{}); err != nil {
			return err
		}
		if durationNS > math.MaxUint64-active.playedNS {
			return errors.New("played duration overflow")
		}
		active.playedNS += durationNS
		now := runner.scheduler.NowNS()
		if active.nextSendNS < now {
			active.nextSendNS = now
		}
		if durationNS > math.MaxUint64-active.nextSendNS {
			return errors.New("playback deadline overflow")
		}
		active.nextSendNS += durationNS
		active.expectedOffset += uint64(size / 2)
		if !active.emittingReported {
			active.emittingReported = true
			commitment, _ := runner.ledger.Lookup(active.utterance.ID)
			if err := runner.publishTransition(ctx, active.cause, Transition{
				UtteranceID: active.utterance.ID, Stage: StagePlayback,
				State: StateEmitting, CrossedBoundary: true,
				PlayedNS: active.playedNS, LedgerState: commitment.State,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (runner *playbackRunner) waitUntil(ctx context.Context, deadline uint64) error {
	if deadline == 0 {
		return nil
	}
	now := runner.scheduler.NowNS()
	if deadline <= now {
		return nil
	}
	delay := deadline - now
	if delay > uint64(math.MaxInt64) {
		return errors.New("playback wait exceeds supported duration")
	}
	fired := make(chan struct{})
	var once sync.Once
	timer := runner.scheduler.AfterFunc(time.Duration(delay), func() { once.Do(func() { close(fired) }) })
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-fired:
		return nil
	}
}

func (runner *playbackRunner) finishPlayed(ctx context.Context, active *activePlayback) error {
	playedMS := active.playedNS / uint64(time.Millisecond)
	ledgerErr := runner.ledger.Complete(active.utterance.ID, playedMS)
	sinkErr := runner.endSink(ctx, active, action.Outcome{Completed: true, PlayedMS: playedMS})
	if err := errors.Join(ledgerErr, sinkErr); err != nil {
		return runner.publishPlaybackTerminal(ctx, active.cause, Transition{
			UtteranceID: active.utterance.ID, Stage: StagePlayback,
			State: StateFailed, CrossedBoundary: active.playedNS > 0,
			PlayedNS: active.playedNS, Reason: err.Error(),
		}, PlaybackOutcome{
			UtteranceID: active.utterance.ID, Kind: OutcomeFailed,
			CrossedBoundary: active.playedNS > 0, PlayedNS: active.playedNS,
			Code: "completion_failed", Message: err.Error(),
		})
	}
	commitment, _ := runner.ledger.Lookup(active.utterance.ID)
	return runner.publishPlaybackTerminal(ctx, active.cause, Transition{
		UtteranceID: active.utterance.ID, Stage: StagePlayback,
		State: StatePlayed, CrossedBoundary: active.playedNS > 0,
		PlayedNS: active.playedNS, LedgerState: commitment.State,
	}, PlaybackOutcome{
		UtteranceID: active.utterance.ID, Kind: OutcomeSucceeded,
		CrossedBoundary: active.playedNS > 0, PlayedNS: active.playedNS,
	})
}

func (runner *playbackRunner) finishCancelled(
	ctx context.Context, active *activePlayback, reason string,
) error {
	reason = firstNonempty(strings.TrimSpace(reason), "cancelled")
	crossed, ledgerErr := runner.ledger.Cancel(active.utterance.ID, reason)
	sinkErr := runner.endSink(ctx, active, action.Outcome{
		Completed: false, PlayedMS: active.playedNS / uint64(time.Millisecond), Reason: reason,
	})
	message := reason
	if err := errors.Join(ledgerErr, sinkErr); err != nil {
		message = errors.Join(errors.New(reason), err).Error()
	}
	commitment, _ := runner.ledger.Lookup(active.utterance.ID)
	code := "cancelled_before_commitment"
	if crossed {
		code = "cancelled_after_commitment"
	}
	return runner.publishPlaybackTerminal(ctx, active.cause, Transition{
		UtteranceID: active.utterance.ID, Stage: StagePlayback, State: StateCancelled,
		CrossedBoundary: crossed, PlayedNS: active.playedNS,
		Reason: message, LedgerState: commitment.State,
	}, PlaybackOutcome{
		UtteranceID: active.utterance.ID, Kind: OutcomeCancelled,
		CrossedBoundary: crossed, PlayedNS: active.playedNS, Code: code, Message: message,
	})
}

func (runner *playbackRunner) finishFailed(
	ctx context.Context, active *activePlayback, code string, cause error,
) error {
	crossed, ledgerErr := runner.ledger.Cancel(active.utterance.ID, cause.Error())
	sinkErr := runner.endSink(ctx, active, action.Outcome{
		Completed: false, PlayedMS: active.playedNS / uint64(time.Millisecond), Reason: cause.Error(),
	})
	message := errors.Join(cause, ledgerErr, sinkErr).Error()
	commitment, _ := runner.ledger.Lookup(active.utterance.ID)
	return runner.publishPlaybackTerminal(ctx, active.cause, Transition{
		UtteranceID: active.utterance.ID, Stage: StagePlayback, State: StateFailed,
		CrossedBoundary: crossed, PlayedNS: active.playedNS,
		Reason: message, LedgerState: commitment.State,
	}, PlaybackOutcome{
		UtteranceID: active.utterance.ID, Kind: OutcomeFailed,
		CrossedBoundary: crossed, PlayedNS: active.playedNS, Code: code, Message: message,
	})
}

func (runner *playbackRunner) endSink(
	ctx context.Context, active *activePlayback, outcome action.Outcome,
) error {
	if !active.begun {
		return nil
	}
	active.begun = false
	if err := runner.sink.End(ctx, active.utterance, outcome); err != nil {
		return err
	}
	if err := runner.publishReceipt(ctx, active, runner.endedOutput, PlaybackEnded,
		action.Frame{}, outcome); err != nil {
		return err
	}
	if active.reserved {
		return runner.publishReceipt(ctx, active, runner.releasedOutput, PlaybackReleased,
			action.Frame{}, outcome)
	}
	return nil
}

func (runner *playbackRunner) handleIdleCancel(ctx context.Context, envelope element.Envelope) error {
	request, valid := cancelPayload(envelope.Payload)
	if !valid {
		return runner.publishCancelReply(ctx, envelope, "", OutcomeRefused,
			false, 0, "invalid_payload", fmt.Sprintf("cancel payload has type %T", envelope.Payload))
	}
	target := cancelTarget(envelope, request)
	if target == "" {
		return runner.publishCancelReply(ctx, envelope, "", OutcomeRefused,
			false, 0, "missing_utterance_id", "cancel requires an utterance ID or cancellation scope")
	}
	if runner.active != nil && target == runner.active.utterance.ID {
		active := runner.active
		runner.active = nil
		runner.discardID = target
		return runner.finishCancelled(ctx, active, request.Reason)
	}
	runner.pendingCancels.remember(target, request.Reason)
	return runner.publishCancelReply(ctx, envelope, target, OutcomeIgnored,
		false, 0, "pending_cancel", "cancellation retained for bounded future admission")
}

func (runner *playbackRunner) unexpectedFrame(
	ctx context.Context, cause element.Envelope, frame AudioFrame, message string,
) error {
	return runner.publishPlaybackTerminal(ctx, cause, Transition{
		UtteranceID: frame.UtteranceID, Stage: StagePlayback,
		State: StateRefused, Reason: message,
	}, PlaybackOutcome{
		UtteranceID: frame.UtteranceID, Kind: OutcomeRefused,
		Code: "invalid_framing", Message: message,
	})
}

func (runner *playbackRunner) publishTransition(
	ctx context.Context, cause element.Envelope, transition Transition,
) error {
	envelope := childEnvelope(cause, transitionType,
		fmt.Sprintf("%s:%s:%s", cause.ItemID, transition.Stage, transition.State), transition.UtteranceID)
	envelope.Payload = transition
	_, err := runner.statusOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *playbackRunner) publishReceipt(
	ctx context.Context, active *activePlayback, output element.OutputPort,
	kind PlaybackReceiptKind, frame action.Frame, outcome action.Outcome,
) error {
	if active == nil || output == nil || active.utterance.ID == "" {
		return errors.New("publish playback receipt: missing active effect or output")
	}
	active.receiptSequence++
	itemID := fmt.Sprintf("%s:playback-receipt:%06d:%s",
		active.cause.ItemID, active.receiptSequence, kind)
	envelope := childEnvelope(active.cause, playbackReceiptType, itemID, active.utterance.ID)
	if active.lastReceiptID != "" && !slices.Contains(envelope.CausalParents, active.lastReceiptID) {
		envelope.CausalParents = append(envelope.CausalParents, active.lastReceiptID)
	}
	utterance := active.utterance
	utterance.AssistantItemIDs = slices.Clone(active.utterance.AssistantItemIDs)
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	envelope.Payload = PlaybackReceipt{
		Kind: kind, Sequence: active.receiptSequence, Utterance: utterance,
		Frame: frame, Outcome: outcome,
	}
	delivery, err := output.Broadcast(ctx, envelope)
	if err != nil {
		return fmt.Errorf("publish playback %s receipt: %w", kind, err)
	}
	if lanes := len(output.Lanes()); delivery.Delivered != lanes || delivery.Dropped != 0 {
		return fmt.Errorf("publish playback %s receipt delivered %d/%d and dropped %d lanes",
			kind, delivery.Delivered, lanes, delivery.Dropped)
	}
	active.lastReceiptID = itemID
	return nil
}

func (runner *playbackRunner) publishPlaybackTerminal(
	ctx context.Context, cause element.Envelope, transition Transition, outcome PlaybackOutcome,
) error {
	if err := runner.publishTransition(ctx, cause, transition); err != nil {
		return err
	}
	envelope := childEnvelope(cause, playbackOutcomeType,
		cause.ItemID+":playback-outcome", outcome.UtteranceID)
	envelope.Payload = outcome
	_, err := runner.outcomeOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *playbackRunner) publishCancelReply(
	ctx context.Context, cause element.Envelope, target string, kind OutcomeKind,
	crossed bool, playedNS uint64, code, message string,
) error {
	envelope := childEnvelope(cause, playbackOutcomeType,
		cause.ItemID+":playback-outcome", target)
	envelope.Payload = PlaybackOutcome{
		UtteranceID: target, Kind: kind, CrossedBoundary: crossed,
		PlayedNS: playedNS, Code: code, Message: message,
	}
	_, err := runner.outcomeOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *playbackRunner) publishResolution(ctx context.Context) error {
	selected := make(map[string]bool, len(runner.sinkDescriptor.Capabilities))
	for capability, enabled := range runner.sinkDescriptor.Capabilities {
		selected[string(capability)] = enabled
	}
	resolution := SinkResolution{
		Reference:  runner.sinkReference,
		Descriptor: cloneDescriptor(runner.sinkDescriptor), Selected: selected,
	}
	_, err := runner.resolvedOutput.Broadcast(ctx, element.Envelope{
		Type: sinkResolutionType, ItemID: runner.instance + "-resolved", Payload: resolution,
	})
	return err
}

func receiveAudio(
	ctx context.Context, input element.InputPort, output chan<- audioCommand,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			sendFailure(ctx, failures, err)
			return
		}
		frame, valid := audioFramePayload(envelope.Payload)
		select {
		case output <- audioCommand{envelope: envelope, frame: frame, valid: valid}:
		case <-ctx.Done():
			return
		}
	}
}

func audioFramePayload(payload any) (AudioFrame, bool) {
	switch typed := payload.(type) {
	case AudioFrame:
		return cloneAudioFrame(typed), true
	case *AudioFrame:
		if typed != nil {
			return cloneAudioFrame(*typed), true
		}
	}
	return AudioFrame{}, false
}

func validateAudioFrame(frame AudioFrame, maxChunkBytes int) error {
	frame.UtteranceID = strings.TrimSpace(frame.UtteranceID)
	if frame.UtteranceID == "" {
		return errors.New("audio frame requires an utterance ID")
	}
	switch frame.Kind {
	case AudioBegin:
		if frame.Utterance.ID != frame.UtteranceID {
			return errors.New("audio begin utterance metadata does not match its stream ID")
		}
		if strings.TrimSpace(frame.Utterance.Text) == "" {
			return errors.New("audio begin requires utterance text")
		}
	case AudioChunk:
		if err := frame.Chunk.Validate(); err != nil {
			return fmt.Errorf("audio chunk: %w", err)
		}
		if frame.Chunk.CandidateID != frame.UtteranceID {
			return errors.New("audio chunk candidate does not match its stream ID")
		}
		if len(frame.Chunk.PCM16LE) > maxChunkBytes {
			return fmt.Errorf("audio chunk exceeds %d bytes", maxChunkBytes)
		}
	case AudioEnd:
		if frame.Terminal.UtteranceID != frame.UtteranceID {
			return errors.New("audio end terminal result does not match its stream ID")
		}
		switch frame.Terminal.Kind {
		case OutcomeSucceeded, OutcomeCancelled, OutcomeFailed:
		default:
			return fmt.Errorf("audio end has invalid terminal outcome %q", frame.Terminal.Kind)
		}
	default:
		return fmt.Errorf("unknown audio frame kind %q", frame.Kind)
	}
	return nil
}
