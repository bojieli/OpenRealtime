package realtimegateway

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type speechJob struct {
	ID           string
	Epoch        uint64
	FastEpoch    uint64
	Phase        trajectory.Phase
	Text         string
	AssistantIDs []string
	Usage        continuation.Usage
}

type speechScheduler struct {
	session   *session
	provider  v1.StreamingSpeechProvider
	queue     chan speechJob
	epoch     atomic.Uint64
	fastEpoch atomic.Uint64

	mu           sync.Mutex
	activeCancel context.CancelCauseFunc
	activePhase  trajectory.Phase
	outstanding  map[string]speechJob
}

var errFastSpeechSuperseded = errors.New("fast speech superseded by authoritative slow continuation")

func newSpeechScheduler(session *session, provider v1.StreamingSpeechProvider) *speechScheduler {
	return &speechScheduler{
		session: session, provider: provider, queue: make(chan speechJob, 128),
		outstanding: make(map[string]speechJob),
	}
}

func (scheduler *speechScheduler) Enqueue(job speechJob) error {
	job.Text = strings.TrimSpace(job.Text)
	if job.Text == "" {
		return errors.New("Fish speech job requires non-empty text")
	}
	job.ID = scheduler.session.nextID("speech")
	job.Epoch = scheduler.epoch.Load()
	job.FastEpoch = scheduler.fastEpoch.Load()
	job.AssistantIDs = slices.Clone(job.AssistantIDs)
	scheduler.mu.Lock()
	scheduler.outstanding[job.ID] = job
	scheduler.mu.Unlock()
	select {
	case scheduler.queue <- job:
		return nil
	case <-scheduler.session.ctx.Done():
		return context.Cause(scheduler.session.ctx)
	default:
		scheduler.mu.Lock()
		delete(scheduler.outstanding, job.ID)
		scheduler.mu.Unlock()
		return errors.New("Fish speech queue is full")
	}
}

func (scheduler *speechScheduler) Interrupt(cause error) []string {
	scheduler.epoch.Add(1)
	scheduler.mu.Lock()
	cancel := scheduler.activeCancel
	seen := make(map[string]struct{})
	var assistantIDs []string
	for _, job := range scheduler.outstanding {
		for _, id := range job.AssistantIDs {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			assistantIDs = append(assistantIDs, id)
		}
	}
	clear(scheduler.outstanding)
	scheduler.mu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
	return assistantIDs
}

// SupersedeFast preserves the already played fast prefix while invalidating
// all unplayed provisional fast media. Slow assistant output and authoritative
// tool calls cross a semantic safe point, so they must not wait behind stale
// fast audio. No transcript text or task-specific rule is inspected.
func (scheduler *speechScheduler) SupersedeFast() []string {
	scheduler.fastEpoch.Add(1)
	scheduler.mu.Lock()
	cancel := scheduler.activeCancel
	if scheduler.activePhase != trajectory.PhaseFast {
		cancel = nil
	}
	seen := make(map[string]struct{})
	var assistantIDs []string
	for id, job := range scheduler.outstanding {
		if job.Phase != trajectory.PhaseFast {
			continue
		}
		delete(scheduler.outstanding, id)
		for _, assistantID := range job.AssistantIDs {
			if _, duplicate := seen[assistantID]; duplicate {
				continue
			}
			seen[assistantID] = struct{}{}
			assistantIDs = append(assistantIDs, assistantID)
		}
	}
	scheduler.mu.Unlock()
	if cancel != nil {
		cancel(errFastSpeechSuperseded)
	}
	return assistantIDs
}

func (scheduler *speechScheduler) Run() {
	defer scheduler.session.wait.Done()
	for {
		select {
		case <-scheduler.session.ctx.Done():
			return
		case job := <-scheduler.queue:
			if job.Epoch != scheduler.epoch.Load() ||
				(job.Phase == trajectory.PhaseFast && job.FastEpoch != scheduler.fastEpoch.Load()) {
				scheduler.mu.Lock()
				delete(scheduler.outstanding, job.ID)
				scheduler.mu.Unlock()
				continue
			}
			ctx, cancel := context.WithCancelCause(scheduler.session.ctx)
			scheduler.mu.Lock()
			scheduler.activeCancel = cancel
			scheduler.activePhase = job.Phase
			scheduler.mu.Unlock()
			publishErr := scheduler.session.publishSpeech(ctx, job)
			if publishErr != nil && context.Cause(ctx) == nil {
				scheduler.session.sendError("tts_provider_error", publishErr.Error())
			}
			cancel(nil)
			scheduler.mu.Lock()
			scheduler.activeCancel = nil
			scheduler.activePhase = ""
			delete(scheduler.outstanding, job.ID)
			scheduler.mu.Unlock()
		}
	}
}

func (session *session) publishSpeech(ctx context.Context, job speechJob) error {
	responseID := session.nextID("resp")
	itemID := session.nextID("item")
	session.settingsMu.RLock()
	outputFormat := session.settings.OutputFormat
	voice := session.settings.Voice
	session.settingsMu.RUnlock()
	session.wireAssistantMu.Lock()
	session.wireAssistantItems[itemID] = slices.Clone(job.AssistantIDs)
	session.wireAssistantMu.Unlock()
	if err := session.send(event("response.created", session.nextID("event"), map[string]any{
		"response": responseObject(responseID, "in_progress", session.conversationID, nil, nil, outputFormat, voice),
	})); err != nil {
		return err
	}
	inProgress := assistantItem(itemID, "in_progress", "")
	if err := session.send(event("response.output_item.added", session.nextID("event"), map[string]any{
		"response_id": responseID, "output_index": 0, "item": inProgress,
	})); err != nil {
		return err
	}
	if err := session.send(event("response.content_part.added", session.nextID("event"), map[string]any{
		"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "audio", "transcript": ""},
	})); err != nil {
		return err
	}
	if err := session.send(event("response.output_audio_transcript.delta", session.nextID("event"), map[string]any{
		"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
		"delta": job.Text,
	})); err != nil {
		return err
	}

	var encoder *outputEncoder
	var sourceRate uint32
	var nextAudioSend time.Time
	wireFrameBytes, err := encodedAudioFrameBytes(outputFormat, 100*time.Millisecond)
	if err != nil {
		return err
	}
	var wireBuffer []byte
	emitAudio := func(encoded []byte) error {
		if err := waitForAudioBudget(ctx, nextAudioSend); err != nil {
			return err
		}
		if err := session.send(event("response.output_audio.delta", session.nextID("event"), map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
			"delta": base64.StdEncoding.EncodeToString(encoded),
		})); err != nil {
			return err
		}
		duration, err := encodedAudioDuration(outputFormat, len(encoded))
		if err != nil {
			return err
		}
		now := time.Now()
		if nextAudioSend.Before(now) {
			nextAudioSend = now
		}
		nextAudioSend = nextAudioSend.Add(duration)
		return nil
	}
	streamErr := session.speech.provider.Stream(ctx, v1.SpeechPlan{
		CandidateID: job.ID, Text: job.Text,
	}, func(chunk v1.SpeechChunk) error {
		if sourceRate == 0 {
			sourceRate = chunk.SampleRateHz
			var err error
			encoder, err = newOutputEncoder(outputFormat, sourceRate)
			if err != nil {
				return err
			}
		}
		if chunk.SampleRateHz != sourceRate {
			return errors.New("Fish Audio changed sample rate within one response")
		}
		encoded, err := encoder.Push(chunk.PCM16LE, chunk.Final)
		if err != nil {
			return err
		}
		if len(encoded) == 0 {
			return nil
		}
		wireBuffer = append(wireBuffer, encoded...)
		for len(wireBuffer) >= wireFrameBytes {
			if err := emitAudio(wireBuffer[:wireFrameBytes]); err != nil {
				return err
			}
			wireBuffer = wireBuffer[wireFrameBytes:]
		}
		if chunk.Final && len(wireBuffer) > 0 {
			if err := emitAudio(wireBuffer); err != nil {
				return err
			}
			wireBuffer = nil
		}
		return nil
	})
	if streamErr == nil && context.Cause(ctx) != nil {
		streamErr = context.Cause(ctx)
	}
	if streamErr == nil && sourceRate == 0 {
		streamErr = errors.New("Fish Audio returned no audio chunks")
	}
	if streamErr == nil && len(wireBuffer) > 0 {
		streamErr = emitAudio(wireBuffer)
		wireBuffer = nil
	}
	status := "completed"
	itemStatus := "completed"
	if streamErr != nil {
		status = "cancelled"
		itemStatus = "incomplete"
	}
	terminalItem := assistantItem(itemID, itemStatus, job.Text)
	terminalEvents := []map[string]any{
		event("response.output_audio.done", session.nextID("event"), map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
		}),
		event("response.output_audio_transcript.done", session.nextID("event"), map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
			"transcript": job.Text,
		}),
		event("response.content_part.done", session.nextID("event"), map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "audio", "transcript": job.Text},
		}),
		event("response.output_item.done", session.nextID("event"), map[string]any{
			"response_id": responseID, "output_index": 0, "item": terminalItem,
		}),
	}
	for _, terminal := range terminalEvents {
		if err := session.send(terminal); err != nil {
			return errors.Join(streamErr, err)
		}
	}
	usage := job.Usage
	done := responseObject(responseID, status, session.conversationID, []map[string]any{terminalItem}, &usage, outputFormat, voice)
	if status == "cancelled" {
		reason := "turn_detected"
		if errors.Is(context.Cause(ctx), errFastSpeechSuperseded) {
			reason = "client_cancelled"
		}
		done["status_details"] = map[string]any{"type": "cancelled", "reason": reason}
	}
	if err := session.send(event("response.done", session.nextID("event"), map[string]any{"response": done})); err != nil {
		return errors.Join(streamErr, err)
	}
	return streamErr
}

func waitForAudioBudget(ctx context.Context, deadline time.Time) error {
	if deadline.IsZero() {
		return nil
	}
	delay := time.Until(deadline)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func encodedAudioDuration(format audioFormat, bytes int) (time.Duration, error) {
	if bytes <= 0 {
		return 0, errors.New("encoded audio must be non-empty")
	}
	rate, err := format.sampleRate()
	if err != nil {
		return 0, err
	}
	samples := bytes
	if format.Type == formatPCM16 {
		if bytes%2 != 0 {
			return 0, errors.New("encoded PCM audio must contain complete samples")
		}
		samples /= 2
	}
	return time.Duration(samples) * time.Second / time.Duration(rate), nil
}

func encodedAudioFrameBytes(format audioFormat, duration time.Duration) (int, error) {
	if duration <= 0 {
		return 0, errors.New("encoded audio frame duration must be positive")
	}
	rate, err := format.sampleRate()
	if err != nil {
		return 0, err
	}
	samples := int(time.Duration(rate) * duration / time.Second)
	if samples <= 0 {
		return 0, errors.New("encoded audio frame must contain at least one sample")
	}
	if format.Type == formatPCM16 {
		return samples * 2, nil
	}
	return samples, nil
}

func (session *session) publishToolResponse(calls []trajectory.ToolCall, usage *continuation.Usage) error {
	responseID := session.nextID("resp")
	session.settingsMu.RLock()
	outputFormat := session.settings.OutputFormat
	voice := session.settings.Voice
	session.settingsMu.RUnlock()
	if err := session.send(event("response.created", session.nextID("event"), map[string]any{
		"response": responseObject(responseID, "in_progress", session.conversationID, nil, nil, outputFormat, voice),
	})); err != nil {
		return err
	}
	output := make([]map[string]any, 0, len(calls))
	for index, call := range calls {
		itemID := session.nextID("item")
		inProgress := functionCallItem(itemID, "in_progress", call)
		if err := session.send(event("response.output_item.added", session.nextID("event"), map[string]any{
			"response_id": responseID, "output_index": index, "item": inProgress,
		})); err != nil {
			return err
		}
		if err := session.send(event("response.function_call_arguments.delta", session.nextID("event"), map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": index,
			"call_id": call.CallID, "delta": string(call.Arguments),
		})); err != nil {
			return err
		}
		if err := session.send(event("response.function_call_arguments.done", session.nextID("event"), map[string]any{
			"response_id": responseID, "item_id": itemID, "output_index": index,
			"call_id": call.CallID, "name": call.Name, "arguments": string(call.Arguments),
		})); err != nil {
			return err
		}
		completed := functionCallItem(itemID, "completed", call)
		if err := session.send(event("response.output_item.done", session.nextID("event"), map[string]any{
			"response_id": responseID, "output_index": index, "item": completed,
		})); err != nil {
			return err
		}
		output = append(output, completed)
	}
	done := responseObject(responseID, "completed", session.conversationID, output, usage, outputFormat, voice)
	return session.send(event("response.done", session.nextID("event"), map[string]any{"response": done}))
}
