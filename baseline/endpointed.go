// Package baseline implements the conventional endpointed B0 reference condition.
package baseline

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/engine"
	"github.com/bojieli/OpenRealtime/internal/audio"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/replay"
	"github.com/bojieli/OpenRealtime/trace"
)

const nanosecondsPerMillisecond = uint64(1_000_000)

type Delay struct {
	BaseNS   uint64 `json:"base_ns"`
	JitterNS uint64 `json:"jitter_ns"`
}

type TimingModel struct {
	StreamingRevision  Delay `json:"streaming_revision"`
	PerceptionFinalize Delay `json:"perception_finalize"`
	PerceptionQueue    Delay `json:"perception_queue"`
	Cognition          Delay `json:"cognition"`
	CognitionQueue     Delay `json:"cognition_queue"`
	SpeechFirstChunk   Delay `json:"speech_first_chunk"`
	PlaybackQueue      Delay `json:"playback_queue"`
}

func DefaultTimingModel() TimingModel {
	ms := func(base, jitter uint64) Delay {
		return Delay{BaseNS: base * nanosecondsPerMillisecond, JitterNS: jitter * nanosecondsPerMillisecond}
	}
	return TimingModel{
		StreamingRevision:  ms(30, 5),
		PerceptionFinalize: ms(60, 12),
		PerceptionQueue:    ms(3, 1),
		Cognition:          ms(80, 20),
		CognitionQueue:     ms(4, 1),
		SpeechFirstChunk:   ms(55, 10),
		PlaybackQueue:      ms(2, 1),
	}
}

type Config struct {
	FixturePath string
	Manifest    reference.Manifest
	Trials      uint64
	Seed        uint64
	FrameMS     uint32
	Timing      TimingModel
}

type StageDurations struct {
	PerceptionFinalizeNS uint64 `json:"perception_finalize_ns"`
	PerceptionQueueNS    uint64 `json:"perception_queue_ns"`
	CognitionNS          uint64 `json:"cognition_ns"`
	CognitionQueueNS     uint64 `json:"cognition_queue_ns"`
	SpeechFirstChunkNS   uint64 `json:"speech_first_chunk_ns"`
	PlaybackQueueNS      uint64 `json:"playback_queue_ns"`
}

func (stages StageDurations) Sum() uint64 {
	return stages.PerceptionFinalizeNS + stages.PerceptionQueueNS +
		stages.CognitionNS + stages.CognitionQueueNS +
		stages.SpeechFirstChunkNS + stages.PlaybackQueueNS
}

type Trial struct {
	Index                     uint64         `json:"index"`
	Seed                      uint64         `json:"seed"`
	EndpointNS                uint64         `json:"endpoint_ns"`
	FirstOutputPlaybackNS     uint64         `json:"first_output_playback_ns"`
	ObservedResponseLatencyNS uint64         `json:"observed_response_latency_ns"`
	ReconciledStageSumNS      uint64         `json:"reconciled_stage_sum_ns"`
	ReconciliationErrorNS     uint64         `json:"reconciliation_error_ns"`
	Stages                    StageDurations `json:"stages"`
	Trace                     []trace.Record `json:"-"`
}

type Distribution struct {
	Count uint64 `json:"count"`
	MinNS uint64 `json:"min_ns"`
	P50NS uint64 `json:"p50_ns"`
	P90NS uint64 `json:"p90_ns"`
	P95NS uint64 `json:"p95_ns"`
	P99NS uint64 `json:"p99_ns"`
	MaxNS uint64 `json:"max_ns"`
}

type Report struct {
	SchemaVersion string                  `json:"schema_version"`
	Condition     string                  `json:"condition"`
	TimingMode    string                  `json:"timing_mode"`
	EvidenceScope string                  `json:"evidence_scope"`
	FixtureSHA256 string                  `json:"fixture_sha256"`
	Seed          uint64                  `json:"seed"`
	FrameMS       uint32                  `json:"frame_ms"`
	TimingModel   TimingModel             `json:"timing_model"`
	Adapters      map[string]string       `json:"adapters"`
	Trials        []Trial                 `json:"trials"`
	Distributions map[string]Distribution `json:"distributions"`
}

type sampledTiming struct {
	streamingRevision uint64
	stages            StageDurations
}

type scheduledEvent struct {
	key       string
	parents   []string
	atNS      uint64
	order     uint64
	direction openaiwire.Direction
	profile   openaiwire.Profile
	message   json.RawMessage
}

type inputFixture struct {
	records     []trace.Record
	frames      []engine.AudioFrame
	endpointNS  uint64
	sampleCount uint64
}

func Run(ctx context.Context, config Config) (Report, error) {
	if config.FixturePath == "" || config.Trials == 0 || config.FrameMS == 0 {
		return Report{}, errors.New("fixture path, positive trials, and positive frame duration are required")
	}
	digest, err := audio.HashFile(config.FixturePath)
	if err != nil {
		return Report{}, err
	}
	fixtureHash := hex.EncodeToString(digest[:])
	if fixtureHash != config.Manifest.FixtureSHA256 {
		return Report{}, fmt.Errorf("fixture SHA-256 %s does not match manifest %s", fixtureHash, config.Manifest.FixtureSHA256)
	}
	input, err := loadInputFixture(config.FixturePath, config.FrameMS)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		SchemaVersion: "0.1.0",
		Condition:     "B0_endpointed_reference",
		TimingMode:    "deterministic_simulation",
		EvidenceScope: "instrumentation_only_no_provider_latency_or_quality",
		FixtureSHA256: fixtureHash,
		Seed:          config.Seed,
		FrameMS:       config.FrameMS,
		TimingModel:   config.Timing,
		Adapters: map[string]string{
			"perception": "reference.manifest_perception",
			"cognition":  "reference.fixed_cognition",
			"speech":     "reference.signal_speech",
		},
		Trials: make([]Trial, 0, config.Trials),
	}
	validator := openaiwire.NewValidator()
	for trialIndex := range config.Trials {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		trialSeed := config.Seed + trialIndex
		random := rand.New(rand.NewPCG(trialSeed, trialSeed^0x9e3779b97f4a7c15))
		timing, err := sampleTiming(config.Timing, random)
		if err != nil {
			return Report{}, err
		}
		trial, err := runTrial(ctx, trialIndex, trialSeed, config.Manifest, input, timing, validator)
		if err != nil {
			return Report{}, fmt.Errorf("trial %d: %w", trialIndex, err)
		}
		report.Trials = append(report.Trials, trial)
	}
	report.Distributions = summarize(report.Trials)
	return report, nil
}

func loadInputFixture(path string, frameMS uint32) (inputFixture, error) {
	var eventOutput, traceOutput bytes.Buffer
	summary, err := replay.WAV(path, replay.Options{
		SessionID: "m1-input", FrameDurationMS: frameMS, Events: &eventOutput, Trace: &traceOutput,
	})
	if err != nil {
		return inputFixture{}, err
	}
	result := inputFixture{endpointNS: summary.DurationNS, sampleCount: summary.InputSampleCount}
	var sampleOffset uint64
	_, err = trace.Read(bytes.NewReader(traceOutput.Bytes()), func(record trace.Record) error {
		result.records = append(result.records, record)
		message, err := openaiwire.Decode(record.Message)
		if err != nil {
			return err
		}
		if message.Type() != openaiwire.EventInputAudioBufferAppend {
			return nil
		}
		var payload struct {
			Audio string `json:"audio"`
		}
		if err := message.Unmarshal(&payload); err != nil {
			return err
		}
		pcm, err := base64.StdEncoding.DecodeString(payload.Audio)
		if err != nil {
			return err
		}
		result.frames = append(result.frames, engine.AudioFrame{
			Index: uint64(len(result.frames)), SampleOffset: sampleOffset,
			SampleRateHz: 24_000, PCM16LE: pcm,
		})
		sampleOffset += uint64(len(pcm) / 2)
		return nil
	})
	if err != nil {
		return inputFixture{}, err
	}
	if sampleOffset != summary.InputSampleCount {
		return inputFixture{}, errors.New("input trace sample count does not match replay summary")
	}
	return result, nil
}

func sampleTiming(model TimingModel, random *rand.Rand) (sampledTiming, error) {
	values := []*Delay{
		&model.StreamingRevision, &model.PerceptionFinalize, &model.PerceptionQueue,
		&model.Cognition, &model.CognitionQueue, &model.SpeechFirstChunk, &model.PlaybackQueue,
	}
	for _, delay := range values {
		if delay.JitterNS > delay.BaseNS {
			return sampledTiming{}, errors.New("timing jitter must not exceed its base duration")
		}
	}
	return sampledTiming{
		streamingRevision: jitter(model.StreamingRevision, random),
		stages: StageDurations{
			PerceptionFinalizeNS: jitter(model.PerceptionFinalize, random),
			PerceptionQueueNS:    jitter(model.PerceptionQueue, random),
			CognitionNS:          jitter(model.Cognition, random),
			CognitionQueueNS:     jitter(model.CognitionQueue, random),
			SpeechFirstChunkNS:   jitter(model.SpeechFirstChunk, random),
			PlaybackQueueNS:      jitter(model.PlaybackQueue, random),
		},
	}, nil
}

func jitter(delay Delay, random *rand.Rand) uint64 {
	if delay.JitterNS == 0 {
		return delay.BaseNS
	}
	width := delay.JitterNS*2 + 1
	offset := random.Uint64N(width)
	return delay.BaseNS - delay.JitterNS + offset
}

func runTrial(
	ctx context.Context,
	index uint64,
	seed uint64,
	manifest reference.Manifest,
	input inputFixture,
	timing sampledTiming,
	validator *openaiwire.Validator,
) (Trial, error) {
	perception := reference.NewPerception(manifest)
	cognition := reference.NewCognition(manifest.ResponseText)
	speech := reference.NewSpeech(100)
	if !perception.Capabilities()[engine.CapabilityStreamingInput] {
		return Trial{}, errors.New("M1 perception adapter must support streaming input")
	}

	schedule := make([]scheduledEvent, 0, len(input.records)+len(manifest.Cues)+12)
	var order uint64
	for _, record := range input.records {
		schedule = append(schedule, scheduledEvent{
			key: record.TraceID, parents: slices.Clone(record.CausalParentIDs), atNS: record.MonotonicNS,
			order: order, direction: record.Direction, profile: record.Profile, message: slices.Clone(record.Message),
		})
		order++
	}
	var revisions []engine.PerceptionRevision
	for _, frame := range input.frames {
		produced, err := perception.PushFrame(ctx, frame)
		if err != nil {
			return Trial{}, err
		}
		for _, revision := range produced {
			revisions = append(revisions, revision)
			frameIndex := frameForSample(input.frames, revision.SourceSample)
			message, err := marshalEvent(map[string]any{
				"event_id": fmt.Sprintf("event_m1_%02d_revision_%04d", index, revision.RevisionID),
				"type":     openaiwire.EventConversationItemInputAudioTranscriptionDelta,
				"item_id":  manifest.ItemID, "content_index": 0, "delta": revision.Delta,
			})
			if err != nil {
				return Trial{}, err
			}
			schedule = append(schedule, scheduledEvent{
				key:     fmt.Sprintf("trial_%02d_revision_%04d", index, revision.RevisionID),
				parents: []string{input.records[frameIndex].TraceID},
				atNS:    revision.SourceSample*1_000_000_000/24_000 + timing.streamingRevision,
				order:   order, direction: openaiwire.DirectionServer, profile: openaiwire.ProfileRealtime,
				message: message,
			})
			order++
		}
	}
	finalRevision, err := perception.Finalize(ctx, input.sampleCount)
	if err != nil {
		return Trial{}, err
	}
	candidate, err := cognition.Respond(ctx, finalRevision)
	if err != nil {
		return Trial{}, err
	}
	chunks, err := speech.Synthesize(ctx, engine.SpeechPlan{CandidateID: candidate.CandidateID, Text: candidate.Text})
	if err != nil {
		return Trial{}, err
	}
	if len(chunks) != 1 || !chunks[0].Final {
		return Trial{}, errors.New("reference speech adapter must return one final chunk")
	}

	endpointKey := input.records[len(input.records)-1].TraceID
	lastRevisionKey := endpointKey
	if len(revisions) > 0 {
		lastRevisionKey = fmt.Sprintf("trial_%02d_revision_%04d", index, revisions[len(revisions)-1].RevisionID)
	}
	stage := timing.stages
	perceptionDoneNS := input.endpointNS + stage.PerceptionFinalizeNS
	responseCreatedNS := perceptionDoneNS + stage.PerceptionQueueNS
	cognitionDoneNS := responseCreatedNS + stage.CognitionNS
	speechRequestedNS := cognitionDoneNS + stage.CognitionQueueNS
	firstChunkNS := speechRequestedNS + stage.SpeechFirstChunkNS
	firstOutputPlaybackNS := firstChunkNS + stage.PlaybackQueueNS
	responseDoneNS := firstOutputPlaybackNS + chunks[0].DurationNS()

	responseID := fmt.Sprintf("resp_m1_%02d", index)
	assistantItemID := fmt.Sprintf("item_m1_assistant_%02d", index)
	chain := endpointKey
	add := func(key string, atNS uint64, value map[string]any) error {
		message, err := marshalEvent(value)
		if err != nil {
			return err
		}
		parents := []string{chain}
		if key == fmt.Sprintf("trial_%02d_perception_done", index) && lastRevisionKey != endpointKey {
			parents = append(parents, lastRevisionKey)
		}
		schedule = append(schedule, scheduledEvent{
			key: key, parents: parents, atNS: atNS, order: order,
			direction: openaiwire.DirectionServer, profile: openaiwire.ProfileRealtime, message: message,
		})
		order++
		chain = key
		return nil
	}

	if err := add(fmt.Sprintf("trial_%02d_perception_done", index), perceptionDoneNS, map[string]any{
		"event_id": fmt.Sprintf("event_m1_%02d_transcript_done", index),
		"type":     openaiwire.EventConversationItemInputAudioTranscriptionCompleted,
		"item_id":  manifest.ItemID, "content_index": 0, "transcript": finalRevision.StableText,
		"usage": map[string]any{"type": "duration", "seconds": float64(input.endpointNS) / 1_000_000_000},
	}); err != nil {
		return Trial{}, err
	}
	if err := add(fmt.Sprintf("trial_%02d_response_created", index), responseCreatedNS, map[string]any{
		"event_id": fmt.Sprintf("event_m1_%02d_response_created", index),
		"type":     openaiwire.EventResponseCreated,
		"response": map[string]any{"id": responseID, "object": "realtime.response", "status": "in_progress", "output": []any{}},
	}); err != nil {
		return Trial{}, err
	}
	if err := add(fmt.Sprintf("trial_%02d_output_item", index), responseCreatedNS, map[string]any{
		"event_id": fmt.Sprintf("event_m1_%02d_output_item", index),
		"type":     openaiwire.EventResponseOutputItemAdded, "response_id": responseID, "output_index": 0,
		"item": map[string]any{"id": assistantItemID, "object": "realtime.item", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	}); err != nil {
		return Trial{}, err
	}
	if err := add(fmt.Sprintf("trial_%02d_content_part", index), responseCreatedNS, map[string]any{
		"event_id": fmt.Sprintf("event_m1_%02d_content_part", index),
		"type":     openaiwire.EventResponseContentPartAdded, "response_id": responseID,
		"item_id": assistantItemID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "audio", "transcript": candidate.Text},
	}); err != nil {
		return Trial{}, err
	}
	if err := add(fmt.Sprintf("trial_%02d_transcript", index), cognitionDoneNS, map[string]any{
		"event_id": fmt.Sprintf("event_m1_%02d_transcript", index),
		"type":     openaiwire.EventResponseOutputAudioTranscriptDone, "response_id": responseID,
		"item_id": assistantItemID, "output_index": 0, "content_index": 0, "transcript": candidate.Text,
	}); err != nil {
		return Trial{}, err
	}
	if err := add(fmt.Sprintf("trial_%02d_audio_delta", index), firstChunkNS, map[string]any{
		"event_id": fmt.Sprintf("event_m1_%02d_audio_delta", index),
		"type":     openaiwire.EventResponseOutputAudioDelta, "response_id": responseID,
		"item_id": assistantItemID, "output_index": 0, "content_index": 0,
		"delta": base64.StdEncoding.EncodeToString(chunks[0].PCM16LE),
	}); err != nil {
		return Trial{}, err
	}
	if err := add(fmt.Sprintf("trial_%02d_playback_started", index), firstOutputPlaybackNS, map[string]any{
		"event_id": fmt.Sprintf("event_m1_%02d_playback_started", index),
		"type":     openaiwire.EventOutputAudioBufferStarted, "response_id": responseID,
	}); err != nil {
		return Trial{}, err
	}
	for suffix, value := range []map[string]any{
		{"event_id": fmt.Sprintf("event_m1_%02d_audio_done", index), "type": openaiwire.EventResponseOutputAudioDone, "response_id": responseID, "item_id": assistantItemID, "output_index": 0, "content_index": 0},
		{"event_id": fmt.Sprintf("event_m1_%02d_content_done", index), "type": openaiwire.EventResponseContentPartDone, "response_id": responseID, "item_id": assistantItemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "audio", "transcript": candidate.Text}},
		{"event_id": fmt.Sprintf("event_m1_%02d_item_done", index), "type": openaiwire.EventResponseOutputItemDone, "response_id": responseID, "output_index": 0, "item": map[string]any{"id": assistantItemID, "object": "realtime.item", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_audio", "transcript": candidate.Text}}}},
		{"event_id": fmt.Sprintf("event_m1_%02d_response_done", index), "type": openaiwire.EventResponseDone, "response": map[string]any{"id": responseID, "object": "realtime.response", "status": "completed", "output": []any{}}},
		{"event_id": fmt.Sprintf("event_m1_%02d_playback_stopped", index), "type": openaiwire.EventOutputAudioBufferStopped, "response_id": responseID},
	} {
		if err := add(fmt.Sprintf("trial_%02d_done_%02d", index, suffix), responseDoneNS, value); err != nil {
			return Trial{}, err
		}
	}

	records, err := materialize(schedule, fmt.Sprintf("m1-trial-%02d", index), validator)
	if err != nil {
		return Trial{}, err
	}
	observed := firstOutputPlaybackNS - input.endpointNS
	sum := stage.Sum()
	return Trial{
		Index: index, Seed: seed, EndpointNS: input.endpointNS,
		FirstOutputPlaybackNS: firstOutputPlaybackNS, ObservedResponseLatencyNS: observed,
		ReconciledStageSumNS: sum, ReconciliationErrorNS: absoluteDifference(observed, sum),
		Stages: stage, Trace: records,
	}, nil
}

func frameForSample(frames []engine.AudioFrame, sourceSample uint64) int {
	index, found := slices.BinarySearchFunc(frames, sourceSample, func(frame engine.AudioFrame, sample uint64) int {
		return cmp.Compare(frame.EndSample(), sample)
	})
	if found {
		return index
	}
	if index >= len(frames) {
		return len(frames) - 1
	}
	return index
}

func marshalEvent(value map[string]any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func materialize(events []scheduledEvent, sessionID string, validator *openaiwire.Validator) ([]trace.Record, error) {
	sort.SliceStable(events, func(left, right int) bool {
		if events[left].atNS != events[right].atNS {
			return events[left].atNS < events[right].atNS
		}
		return events[left].order < events[right].order
	})
	state := trace.NewState()
	records := make([]trace.Record, 0, len(events))
	for sequence, event := range events {
		message, err := openaiwire.Decode(event.message)
		if err != nil {
			return nil, fmt.Errorf("event %s: %w", event.key, err)
		}
		if err := validator.Validate(event.profile, event.direction, message); err != nil {
			return nil, fmt.Errorf("event %s: %w", event.key, err)
		}
		record := trace.Record{
			SchemaVersion: trace.SchemaVersion, TraceID: event.key, SessionID: sessionID,
			Sequence: uint64(sequence), MonotonicNS: event.atNS, Direction: event.direction,
			Profile: event.profile, CausalParentIDs: slices.Clone(event.parents), Message: slices.Clone(event.message),
		}
		if err := state.Accept(record); err != nil {
			return nil, fmt.Errorf("event %s: %w", event.key, err)
		}
		records = append(records, record)
	}
	return records, nil
}

func absoluteDifference(left, right uint64) uint64 {
	if left >= right {
		return left - right
	}
	return right - left
}

func summarize(trials []Trial) map[string]Distribution {
	metrics := map[string][]uint64{
		"observed_response_latency_ns": {},
		"reconciled_stage_sum_ns":      {},
		"reconciliation_error_ns":      {},
		"perception_finalize_ns":       {},
		"perception_queue_ns":          {},
		"cognition_ns":                 {},
		"cognition_queue_ns":           {},
		"speech_first_chunk_ns":        {},
		"playback_queue_ns":            {},
	}
	for _, trial := range trials {
		metrics["observed_response_latency_ns"] = append(metrics["observed_response_latency_ns"], trial.ObservedResponseLatencyNS)
		metrics["reconciled_stage_sum_ns"] = append(metrics["reconciled_stage_sum_ns"], trial.ReconciledStageSumNS)
		metrics["reconciliation_error_ns"] = append(metrics["reconciliation_error_ns"], trial.ReconciliationErrorNS)
		metrics["perception_finalize_ns"] = append(metrics["perception_finalize_ns"], trial.Stages.PerceptionFinalizeNS)
		metrics["perception_queue_ns"] = append(metrics["perception_queue_ns"], trial.Stages.PerceptionQueueNS)
		metrics["cognition_ns"] = append(metrics["cognition_ns"], trial.Stages.CognitionNS)
		metrics["cognition_queue_ns"] = append(metrics["cognition_queue_ns"], trial.Stages.CognitionQueueNS)
		metrics["speech_first_chunk_ns"] = append(metrics["speech_first_chunk_ns"], trial.Stages.SpeechFirstChunkNS)
		metrics["playback_queue_ns"] = append(metrics["playback_queue_ns"], trial.Stages.PlaybackQueueNS)
	}
	result := make(map[string]Distribution, len(metrics))
	for name, values := range metrics {
		result[name] = distribution(values)
	}
	return result
}

func distribution(values []uint64) Distribution {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return Distribution{
		Count: uint64(len(sorted)), MinNS: sorted[0], P50NS: percentile(sorted, 50),
		P90NS: percentile(sorted, 90), P95NS: percentile(sorted, 95),
		P99NS: percentile(sorted, 99), MaxNS: sorted[len(sorted)-1],
	}
}

func percentile(sorted []uint64, percent uint64) uint64 {
	index := (percent*uint64(len(sorted)) + 99) / 100
	if index == 0 {
		return sorted[0]
	}
	return sorted[index-1]
}
