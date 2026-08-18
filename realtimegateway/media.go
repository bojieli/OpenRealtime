package realtimegateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/preparation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type utteranceState struct {
	itemID            string
	provider          v1.PerceptionProvider
	preparation       *preparedTurn
	frameIndex        uint64
	sourceSample      uint64
	sampleRate        uint32
	lastText          string
	providerRuns      uint64
	providerFailures  uint64
	providerElapsedNS uint64
	finalizeRuns      uint64
	finalizeFailures  uint64
	finalizeElapsedNS uint64
}

func (session *session) mediaLoop() {
	defer session.wait.Done()
	var utterance *utteranceState
	for {
		select {
		case <-session.ctx.Done():
			closePreparedTurn(utterance, context.Cause(session.ctx))
			return
		case command := <-session.media:
			if command.Start {
				if utterance != nil {
					session.sendError("asr_state_error", "new utterance started before the prior utterance finalized")
					continue
				}
				provider, err := session.config.PerceptionFactory()
				if err != nil {
					session.sendError("asr_provider_error", err.Error())
					continue
				}
				prepared, err := session.cognitive.NewPreparation()
				if err != nil {
					session.sendError("preparation_error", err.Error())
					continue
				}
				utterance = &utteranceState{
					itemID: command.ItemID, provider: provider, preparation: prepared,
					sampleRate: command.SampleRate,
				}
			}
			if utterance == nil {
				session.sendError("asr_state_error", "received speech audio without an active utterance")
				continue
			}
			if err := session.advanceUtterance(utterance, command); err != nil {
				session.sendError("asr_provider_error", err.Error())
				closePreparedTurn(utterance, err)
				utterance = nil
				continue
			}
			if command.Final {
				utterance = nil
			}
		}
	}
}

func (session *session) advanceUtterance(utterance *utteranceState, command mediaCommand) error {
	if command.SampleRate != utterance.sampleRate {
		return errors.New("audio sample rate changed within an utterance")
	}
	frame := v1.AudioFrame{
		Index: utterance.frameIndex, SampleOffset: utterance.sourceSample,
		SampleRateHz: command.SampleRate, PCM16LE: command.PCM16,
	}
	revisions, err := utterance.provider.PushFrame(session.ctx, frame)
	session.config.RuntimeMetrics.asrInputFrames.Add(1)
	session.recordProviderAdvances(utterance)
	if err != nil {
		return err
	}
	utterance.frameIndex++
	utterance.sourceSample += uint64(len(command.PCM16) / 2)
	for _, revision := range revisions {
		if err := session.observeRevision(utterance, revision, false); err != nil {
			return err
		}
	}
	if !command.Final {
		return nil
	}
	final, err := utterance.provider.Finalize(session.ctx, utterance.sourceSample)
	session.recordProviderAdvances(utterance)
	if err != nil {
		return err
	}
	session.config.RuntimeMetrics.asrFinalizations.Add(1)
	return session.observeRevision(utterance, final, true)
}

type providerInvocationCounter interface {
	ProviderInvocationCount() uint64
}

type providerRuntimeMetrics interface {
	ProviderRuntimeMetrics() asrbuffer.ProviderMetrics
}

func (session *session) recordProviderAdvances(utterance *utteranceState) {
	counter, ok := utterance.provider.(providerInvocationCounter)
	if !ok {
		return
	}
	current := counter.ProviderInvocationCount()
	if current < utterance.providerRuns {
		return
	}
	session.config.RuntimeMetrics.asrProviderAdvances.Add(current - utterance.providerRuns)
	utterance.providerRuns = current
	detailed, ok := utterance.provider.(providerRuntimeMetrics)
	if !ok {
		return
	}
	metrics := detailed.ProviderRuntimeMetrics()
	if metrics.AdvanceFailures >= utterance.providerFailures {
		session.config.RuntimeMetrics.asrProviderFailures.Add(metrics.AdvanceFailures - utterance.providerFailures)
		utterance.providerFailures = metrics.AdvanceFailures
	}
	if metrics.AdvanceElapsedNS >= utterance.providerElapsedNS {
		session.config.RuntimeMetrics.asrProviderElapsedNS.Add(metrics.AdvanceElapsedNS - utterance.providerElapsedNS)
		utterance.providerElapsedNS = metrics.AdvanceElapsedNS
	}
	updateMaximum(&session.config.RuntimeMetrics.asrProviderMaxElapsedNS, metrics.AdvanceMaxElapsedNS)
	if metrics.FinalizeInvocations >= utterance.finalizeRuns {
		session.config.RuntimeMetrics.asrFinalizeAttempts.Add(metrics.FinalizeInvocations - utterance.finalizeRuns)
		utterance.finalizeRuns = metrics.FinalizeInvocations
	}
	if metrics.FinalizeFailures >= utterance.finalizeFailures {
		session.config.RuntimeMetrics.asrFinalizeFailures.Add(metrics.FinalizeFailures - utterance.finalizeFailures)
		utterance.finalizeFailures = metrics.FinalizeFailures
	}
	if metrics.FinalizeElapsedNS >= utterance.finalizeElapsedNS {
		session.config.RuntimeMetrics.asrFinalizeElapsedNS.Add(metrics.FinalizeElapsedNS - utterance.finalizeElapsedNS)
		utterance.finalizeElapsedNS = metrics.FinalizeElapsedNS
	}
	updateMaximum(&session.config.RuntimeMetrics.asrFinalizeMaxElapsedNS, metrics.FinalizeMaxElapsedNS)
}

func (session *session) observeRevision(utterance *utteranceState, revision v1.PerceptionRevision, final bool) error {
	text := revision.StableText + revision.UnstableText
	revisionID := session.sourceRev.Add(1)
	if strings.TrimSpace(text) != "" && text != utterance.lastText {
		if err := utterance.preparation.Observe(session.ctx, revisionID, text); err != nil && !errors.Is(err, preparation.ErrClosed) {
			return fmt.Errorf("prepare transcript revision: %w", err)
		}
		utterance.lastText = text
	}
	if !final {
		return nil
	}
	if strings.TrimSpace(text) != "" {
		// Final stability is a distinct scheduler opportunity even when Qwen's
		// text bytes equal the last partial revision. The semantic fingerprint
		// coalesces it without a second model request.
		if err := utterance.preparation.Observe(session.ctx, revisionID, text); err != nil && !errors.Is(err, preparation.ErrClosed) {
			return fmt.Errorf("prepare final transcript: %w", err)
		}
		chain, _, err := utterance.preparation.Commit(revisionID, text)
		if err != nil {
			return fmt.Errorf("commit prepared continuation: %w", err)
		}
		session.cognitive.AttachPreparation(revisionID, chain)
	} else {
		closePreparedTurn(utterance, errors.New("empty final transcript"))
	}
	if err := session.send(event("conversation.item.created", session.nextID("event"), map[string]any{
		"previous_item_id": nil, "item": userAudioItem(utterance.itemID, text),
	})); err != nil {
		return err
	}
	durationSeconds := float64(utterance.sourceSample) / float64(utterance.sampleRate)
	if err := session.send(event("conversation.item.input_audio_transcription.completed", session.nextID("event"), map[string]any{
		"item_id": utterance.itemID, "content_index": 0, "transcript": text,
		"usage": map[string]any{"type": "duration", "seconds": durationSeconds},
	})); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if _, err := session.coordinator.Submit(eventloop.Event{
		Type: "asr.endpoint", Source: "qwen3-asr", Channel: "voice",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		OccurredNS: uint64(time.Since(session.origin)), SourceRevision: revisionID,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: text,
		CorrelationID: utterance.itemID,
	}); err != nil {
		return err
	}
	session.signalCognition()
	return nil
}

func closePreparedTurn(utterance *utteranceState, cause error) {
	if utterance == nil || utterance.preparation == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = utterance.preparation.manager.Close(ctx, cause)
}
