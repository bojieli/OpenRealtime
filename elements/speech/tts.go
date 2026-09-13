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

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	defaultMaxTextBytes  = 64 << 10
	defaultMaxChunkBytes = 1 << 20
	defaultMaxAudioBytes = 64 << 20
	defaultCancelMemory  = 256
	maximumBoundBytes    = 64 << 20
	maximumCancelMemory  = 4096
)

type TTSConfig struct {
	Provider      string `json:"provider"`
	MaxTextBytes  int    `json:"max_text_bytes,omitempty"`
	MaxChunkBytes int    `json:"max_chunk_bytes,omitempty"`
	MaxAudioBytes int    `json:"max_audio_bytes,omitempty"`
	CancelMemory  int    `json:"cancel_memory,omitempty"`
}

func decodeTTSConfig(source json.RawMessage) (TTSConfig, error) {
	var config TTSConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return TTSConfig{}, err
	}
	config.Provider = strings.TrimSpace(config.Provider)
	if config.Provider == "" {
		return TTSConfig{}, errors.New("TTS config requires a provider reference")
	}
	if config.MaxTextBytes == 0 {
		config.MaxTextBytes = defaultMaxTextBytes
	}
	if config.MaxChunkBytes == 0 {
		config.MaxChunkBytes = defaultMaxChunkBytes
	}
	if config.MaxAudioBytes == 0 {
		config.MaxAudioBytes = defaultMaxAudioBytes
	}
	if config.CancelMemory == 0 {
		config.CancelMemory = defaultCancelMemory
	}
	if config.MaxTextBytes < 1 || config.MaxTextBytes > maximumBoundBytes {
		return TTSConfig{}, fmt.Errorf("max_text_bytes must be between 1 and %d", maximumBoundBytes)
	}
	if config.MaxChunkBytes < 2 || config.MaxChunkBytes > maximumBoundBytes {
		return TTSConfig{}, fmt.Errorf("max_chunk_bytes must be between 2 and %d", maximumBoundBytes)
	}
	if config.MaxAudioBytes < 2 || config.MaxAudioBytes > maximumBoundBytes {
		return TTSConfig{}, fmt.Errorf("max_audio_bytes must be between 2 and %d", maximumBoundBytes)
	}
	if config.MaxAudioBytes < config.MaxChunkBytes {
		return TTSConfig{}, errors.New("max_audio_bytes must be at least max_chunk_bytes")
	}
	if config.CancelMemory < 1 || config.CancelMemory > maximumCancelMemory {
		return TTSConfig{}, fmt.Errorf("cancel_memory must be between 1 and %d", maximumCancelMemory)
	}
	return config, nil
}

type ttsFactory struct{}

func (ttsFactory) Descriptor() element.Descriptor { return TTSDescriptor() }

func (ttsFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeTTSConfig(source)
	return err
}

func (ttsFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeTTSConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("speech.TTS %s config: %w", mount.InstanceID, err)
	}
	service, _, found := mount.Services.Lookup(TTSProviderRegistryService)
	if !found {
		return nil, fmt.Errorf("speech.TTS %s has no provider registry service", mount.InstanceID)
	}
	registry, ok := service.(*TTSProviderRegistry)
	if !ok || registry == nil {
		return nil, fmt.Errorf("TTS provider registry service has type %T", service)
	}
	entry, err := registry.resolve(config.Provider)
	if err != nil {
		return nil, err
	}
	provider, err := entry.factory()
	if err != nil {
		return nil, fmt.Errorf("create TTS provider %q: %w", config.Provider, err)
	}
	if err := verifyProvider(config.Provider, entry.descriptor, provider); err != nil {
		return nil, errors.Join(err, closeResource(provider))
	}
	if err := mount.Lifecycle.Defer("close-tts-provider", func(context.Context) error {
		return closeResource(provider)
	}); err != nil {
		return nil, errors.Join(err, closeResource(provider))
	}
	textInput, err := mount.Ports.Input("text")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	audioOutput, err := mount.Ports.Output("audio")
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
	return &ttsRunner{
		instance: mount.InstanceID, config: config, provider: provider,
		providerReference: config.Provider, providerDescriptor: entry.descriptor,
		textInput: textInput, cancelInput: cancelInput, audioOutput: audioOutput,
		statusOutput: statusOutput, outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		resolution:     mount.Resolution,
		pendingCancels: newCancellationMemory(config.CancelMemory),
		cancelledRuns:  newCancellationMemory(config.CancelMemory),
	}, nil
}

type ttsCommand struct {
	envelope element.Envelope
	segment  TextSegment
	valid    bool
}

type ttsResult struct {
	outcome SynthesisOutcome
	err     error
}

var errSynthesisCancelled = errors.New("speech synthesis cancelled")

type ttsRunner struct {
	instance           string
	config             TTSConfig
	provider           v1.SpeechProvider
	providerReference  string
	providerDescriptor v1.Descriptor
	textInput          element.InputPort
	cancelInput        element.InputPort
	audioOutput        element.OutputPort
	statusOutput       element.OutputPort
	outcomeOutput      element.OutputPort
	resolvedOutput     element.OutputPort
	resolution         element.ResolutionReporter
	pendingCancels     *cancellationMemory
	// cancelledRuns remembers runs a cancel named; every later segment of
	// such a run is dropped on arrival.
	cancelledRuns *cancellationMemory
}

func (runner *ttsRunner) Run(parent context.Context) error {
	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	actual, err := liveProviderDescriptor(
		runner.providerReference, runner.providerDescriptor, runner.provider,
	)
	if err != nil {
		return fmt.Errorf("resolve TTS provider %q before readiness: %w", runner.providerReference, err)
	}
	if err := reportTTSLiveResolution(runner.resolution, actual); err != nil {
		return fmt.Errorf("attest TTS provider %q: %w", runner.providerReference, err)
	}
	if err := runner.publishResolution(ctx); err != nil {
		return err
	}
	work := make(chan ttsCommand)
	interrupts := make(chan element.Envelope)
	failures := make(chan error, 2)
	var receivers sync.WaitGroup
	receivers.Add(2)
	go receiveText(ctx, runner.textInput, work, failures, &receivers)
	go receiveInterrupts(ctx, runner.cancelInput, interrupts, failures, &receivers)
	defer func() {
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
			if err := runner.rememberIdleCancel(ctx, interrupt); err != nil {
				return err
			}
		case command := <-work:
			if err := runner.handle(ctx, command, interrupts, failures); err != nil {
				return err
			}
		}
	}
}

func (runner *ttsRunner) handle(
	ctx context.Context, command ttsCommand, interrupts <-chan element.Envelope, failures <-chan error,
) error {
	if !command.valid {
		return runner.publishSynthesisTerminal(ctx, command.envelope, Transition{
			Stage: StageSynthesis, State: StateRefused,
			Reason: fmt.Sprintf("text payload has type %T", command.envelope.Payload),
		}, SynthesisOutcome{
			Kind: OutcomeRefused, Code: "invalid_payload",
			Message: fmt.Sprintf("text payload has type %T", command.envelope.Payload),
		}, false)
	}
	command.segment.ID = strings.TrimSpace(command.segment.ID)
	command.segment.Text = strings.TrimSpace(command.segment.Text)
	if err := command.segment.validate(runner.config.MaxTextBytes); err != nil {
		return runner.publishSynthesisTerminal(ctx, command.envelope, Transition{
			UtteranceID: command.segment.ID, Stage: StageSynthesis, State: StateRefused,
			Reason: err.Error(),
		}, SynthesisOutcome{
			UtteranceID: command.segment.ID, Kind: OutcomeRefused,
			Code: "invalid_segment", Message: err.Error(),
		}, false)
	}
	pendingReason, pending := runner.cancelledRuns.peek(command.envelope.RunID)
	if !pending {
		pendingReason, pending = runner.pendingCancels.take(command.segment.ID)
	}
	if pending {
		return runner.publishSynthesisTerminal(ctx, command.envelope, Transition{
			UtteranceID: command.segment.ID, Stage: StageSynthesis,
			State: StateCancelled, Reason: pendingReason,
		}, SynthesisOutcome{
			UtteranceID: command.segment.ID, Kind: OutcomeCancelled,
			Code: "cancelled_before_synthesis", Message: pendingReason,
		}, false)
	}
	if err := runner.publishTransition(ctx, command.envelope, Transition{
		UtteranceID: command.segment.ID, Stage: StageSynthesis, State: StateGenerating,
	}); err != nil {
		return err
	}
	if err := runner.publishAudio(ctx, command.envelope, 0, AudioFrame{
		Kind: AudioBegin, UtteranceID: command.segment.ID,
		Utterance: command.segment.utterance(), SpeechAuthority: command.segment.SpeechAuthority,
	}); err != nil {
		return err
	}

	operationCtx, cancel := context.WithCancelCause(ctx)
	resultChannel := make(chan ttsResult, 1)
	go func() { resultChannel <- runner.synthesize(operationCtx, command) }()
	cancelled := false
	cancelReason := ""
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
					"invalid_payload", fmt.Sprintf("cancel payload has type %T", interrupt.Payload)); err != nil {
					cancel(err)
					<-resultChannel
					return err
				}
				continue
			}
			target := cancelTarget(interrupt, request)
			if runID := strings.TrimSpace(request.RunID); runID != "" {
				runner.cancelledRuns.remember(runID, request.Reason)
			}
			if target != command.segment.ID && !cancelsRun(request, command.envelope.RunID) {
				if target != "" {
					runner.pendingCancels.remember(target, request.Reason)
				}
				if err := runner.publishCancelReply(ctx, interrupt, target, OutcomeIgnored,
					"scope_not_active", fmt.Sprintf("active utterance is %q", command.segment.ID)); err != nil {
					cancel(err)
					<-resultChannel
					return err
				}
				continue
			}
			cancelled = true
			cancelReason = firstNonempty(strings.TrimSpace(request.Reason), "cancelled")
			cancel(fmt.Errorf("%w: %s", errSynthesisCancelled, cancelReason))
		case result := <-resultChannel:
			cancel(nil)
			if cancelled || errors.Is(result.err, context.Canceled) || errors.Is(result.err, errSynthesisCancelled) {
				result.outcome.Kind = OutcomeCancelled
				result.outcome.Code = "cancelled"
				result.outcome.Message = firstNonempty(cancelReason, errorString(result.err))
			} else if result.err != nil {
				result.outcome.Kind = OutcomeFailed
				result.outcome.Code = "provider_error"
				result.outcome.Message = result.err.Error()
			} else {
				result.outcome.Kind = OutcomeSucceeded
			}
			state := StateGenerated
			switch result.outcome.Kind {
			case OutcomeCancelled:
				state = StateCancelled
			case OutcomeFailed:
				state = StateFailed
			}
			return runner.publishSynthesisTerminal(ctx, command.envelope, Transition{
				UtteranceID: command.segment.ID, Stage: StageSynthesis,
				State: state, Reason: result.outcome.Message,
			}, result.outcome, true)
		}
	}
}

func (runner *ttsRunner) synthesize(ctx context.Context, command ttsCommand) ttsResult {
	outcome := SynthesisOutcome{UtteranceID: command.segment.ID}
	var expectedOffset uint64
	var sampleRate uint32
	finalSeen := false
	plan := v1.SpeechPlan{CandidateID: command.segment.ID, Text: command.segment.Text}
	consume := func(chunk v1.SpeechChunk) error {
		if err := chunk.Validate(); err != nil {
			return fmt.Errorf("invalid provider chunk: %w", err)
		}
		if chunk.CandidateID != command.segment.ID {
			return fmt.Errorf("provider chunk candidate %q does not match utterance %q",
				chunk.CandidateID, command.segment.ID)
		}
		if len(chunk.PCM16LE) > runner.config.MaxChunkBytes {
			return fmt.Errorf("provider chunk exceeds %d bytes", runner.config.MaxChunkBytes)
		}
		if finalSeen {
			return errors.New("provider emitted audio after its final chunk")
		}
		if sampleRate == 0 {
			sampleRate = chunk.SampleRateHz
		} else if chunk.SampleRateHz != sampleRate {
			return fmt.Errorf("provider changed sample rate from %d to %d",
				sampleRate, chunk.SampleRateHz)
		}
		if chunk.SampleOffset != expectedOffset {
			return fmt.Errorf("provider chunk offset is %d, want %d", chunk.SampleOffset, expectedOffset)
		}
		end, err := chunk.EndSample()
		if err != nil {
			return err
		}
		expectedOffset = end
		if chunk.Final {
			finalSeen = true
		}
		chunk.PCM16LE = slices.Clone(chunk.PCM16LE)
		if uint64(len(chunk.PCM16LE)) > math.MaxUint64-outcome.AudioBytes {
			return errors.New("provider audio byte count overflow")
		}
		if outcome.AudioBytes+uint64(len(chunk.PCM16LE)) > uint64(runner.config.MaxAudioBytes) {
			return fmt.Errorf("provider audio exceeds %d bytes", runner.config.MaxAudioBytes)
		}
		outcome.Chunks++
		outcome.AudioBytes += uint64(len(chunk.PCM16LE))
		return runner.publishAudio(ctx, command.envelope, outcome.Chunks, AudioFrame{
			Kind: AudioChunk, UtteranceID: command.segment.ID, Chunk: chunk,
		})
	}
	var err error
	if streaming, ok := runner.provider.(v1.StreamingSpeechProvider); ok {
		err = streaming.Stream(ctx, plan, consume)
	} else {
		var chunks []v1.SpeechChunk
		chunks, err = runner.provider.Synthesize(ctx, plan)
		if err == nil {
			for _, chunk := range chunks {
				if consumeErr := consume(chunk); consumeErr != nil {
					err = consumeErr
					break
				}
			}
		}
	}
	if err == nil && outcome.Chunks == 0 {
		err = errors.New("speech provider returned no audio")
	}
	return ttsResult{outcome: outcome, err: err}
}

func (runner *ttsRunner) rememberIdleCancel(ctx context.Context, envelope element.Envelope) error {
	request, valid := cancelPayload(envelope.Payload)
	if !valid {
		return runner.publishCancelReply(ctx, envelope, "", OutcomeRefused,
			"invalid_payload", fmt.Sprintf("cancel payload has type %T", envelope.Payload))
	}
	target := cancelTarget(envelope, request)
	if target == "" {
		return runner.publishCancelReply(ctx, envelope, "", OutcomeRefused,
			"missing_utterance_id", "cancel requires an utterance ID or cancellation scope")
	}
	if runID := strings.TrimSpace(request.RunID); runID != "" {
		runner.cancelledRuns.remember(runID, request.Reason)
		return runner.publishCancelReply(ctx, envelope, runID, OutcomeIgnored,
			"pending_cancel", "run cancellation retained for every utterance of the run")
	}
	runner.pendingCancels.remember(target, request.Reason)
	return runner.publishCancelReply(ctx, envelope, target, OutcomeIgnored,
		"pending_cancel", "cancellation retained for bounded future admission")
}

func (runner *ttsRunner) publishSynthesisTerminal(
	ctx context.Context, cause element.Envelope, transition Transition,
	outcome SynthesisOutcome, audioStarted bool,
) error {
	if audioStarted {
		if err := runner.publishAudio(ctx, cause, outcome.Chunks+1, AudioFrame{
			Kind: AudioEnd, UtteranceID: outcome.UtteranceID, Terminal: outcome,
		}); err != nil {
			return err
		}
	}
	if err := runner.publishTransition(ctx, cause, transition); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, outcome)
}

func (runner *ttsRunner) publishAudio(
	ctx context.Context, cause element.Envelope, sequence uint64, frame AudioFrame,
) error {
	envelope := childEnvelope(cause, audioType,
		fmt.Sprintf("%s:audio:%06d", cause.ItemID, sequence), frame.UtteranceID)
	envelope.Payload = cloneAudioFrame(frame)
	_, err := runner.audioOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *ttsRunner) publishTransition(
	ctx context.Context, cause element.Envelope, transition Transition,
) error {
	envelope := childEnvelope(cause, transitionType,
		fmt.Sprintf("%s:%s:%s", cause.ItemID, transition.Stage, transition.State), transition.UtteranceID)
	envelope.Payload = transition
	_, err := runner.statusOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *ttsRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome SynthesisOutcome,
) error {
	envelope := childEnvelope(cause, synthesisOutcomeType, cause.ItemID+":synthesis-outcome", outcome.UtteranceID)
	envelope.Payload = outcome
	_, err := runner.outcomeOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *ttsRunner) publishCancelReply(
	ctx context.Context, cause element.Envelope, target string, kind OutcomeKind, code, message string,
) error {
	return runner.publishOutcome(ctx, cause, SynthesisOutcome{
		UtteranceID: target, Kind: kind, Code: code, Message: message,
	})
}

func (runner *ttsRunner) publishResolution(ctx context.Context) error {
	selected := make(map[string]bool, len(runner.providerDescriptor.Capabilities))
	for capability, enabled := range runner.providerDescriptor.Capabilities {
		selected[string(capability)] = enabled
	}
	resolution := ProviderResolution{
		Reference:  runner.providerReference,
		Descriptor: cloneDescriptor(runner.providerDescriptor), Selected: selected,
	}
	_, err := runner.resolvedOutput.Broadcast(ctx, element.Envelope{
		Type: providerResolutionType, ItemID: runner.instance + "-resolved", Payload: resolution,
	})
	return err
}

func receiveText(
	ctx context.Context, input element.InputPort, output chan<- ttsCommand,
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
		segment, valid := textSegmentPayload(envelope.Payload)
		select {
		case output <- ttsCommand{envelope: envelope, segment: segment, valid: valid}:
		case <-ctx.Done():
			return
		}
	}
}

func textSegmentPayload(payload any) (TextSegment, bool) {
	var segment TextSegment
	switch typed := payload.(type) {
	case TextSegment:
		segment = typed
	case *TextSegment:
		if typed == nil {
			return TextSegment{}, false
		}
		segment = *typed
	default:
		return TextSegment{}, false
	}
	segment.AssistantItemIDs = slices.Clone(segment.AssistantItemIDs)
	return segment, true
}

func cancelPayload(payload any) (Cancel, bool) {
	switch typed := payload.(type) {
	case Cancel:
		return typed, true
	case *Cancel:
		if typed != nil {
			return *typed, true
		}
	}
	return Cancel{}, false
}

func cloneAudioFrame(frame AudioFrame) AudioFrame {
	frame.Utterance.AssistantItemIDs = slices.Clone(frame.Utterance.AssistantItemIDs)
	frame.Chunk.PCM16LE = slices.Clone(frame.Chunk.PCM16LE)
	return frame
}
