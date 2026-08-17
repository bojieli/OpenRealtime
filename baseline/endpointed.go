// Package baseline implements the conventional endpointed B0 reference condition.
package baseline

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/analysis"
	"github.com/bojieli/OpenRealtime/engine"
	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/internal/fixture"
	"github.com/bojieli/OpenRealtime/internal/simtime"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/trace"
)

type Delay = simtime.Delay
type TimingModel = simtime.Model
type StageDurations = simtime.Stages
type Distribution = analysis.Distribution

func DefaultTimingModel() TimingModel {
	return simtime.DefaultModel()
}

type Config struct {
	FixturePath string
	Manifest    reference.Manifest
	Trials      uint64
	Seed        uint64
	FrameMS     uint32
	Timing      TimingModel
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

type scheduledEvent struct {
	key       string
	parents   []string
	atNS      uint64
	order     uint64
	direction openaiwire.Direction
	profile   openaiwire.Profile
	message   json.RawMessage
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
	input, err := fixture.Load(config.FixturePath, config.FrameMS, "m1-input")
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
		timing, err := simtime.Sample(config.Timing, trialSeed)
		if err != nil {
			return Report{}, err
		}
		trial, err := runTrial(ctx, trialIndex, trialSeed, config.Manifest, input, timing, validator)
		if err != nil {
			return Report{}, fmt.Errorf("trial %d: %w", trialIndex, err)
		}
		report.Trials = append(report.Trials, trial)
	}
	report.Distributions, err = summarize(report.Trials)
	if err != nil {
		return Report{}, err
	}
	return report, nil
}

func runTrial(
	ctx context.Context,
	index uint64,
	seed uint64,
	manifest reference.Manifest,
	input fixture.Input,
	timing simtime.Sampled,
	validator *openaiwire.Validator,
) (Trial, error) {
	perception := reference.NewPerception(manifest)
	cognition := reference.NewCognition(manifest.ResponseText)
	speech := reference.NewSpeech(100)
	if !perception.Capabilities()[engine.CapabilityStreamingInput] {
		return Trial{}, errors.New("M1 perception adapter must support streaming input")
	}

	schedule := make([]scheduledEvent, 0, len(input.Records)+len(manifest.Cues)+12)
	var order uint64
	for _, record := range input.Records {
		schedule = append(schedule, scheduledEvent{
			key: record.TraceID, parents: slices.Clone(record.CausalParentIDs), atNS: record.MonotonicNS,
			order: order, direction: record.Direction, profile: record.Profile, message: slices.Clone(record.Message),
		})
		order++
	}
	var revisions []engine.PerceptionRevision
	for _, frame := range input.Frames {
		produced, err := perception.PushFrame(ctx, frame)
		if err != nil {
			return Trial{}, err
		}
		for _, revision := range produced {
			revisions = append(revisions, revision)
			frameIndex := frameForSample(input.Frames, revision.SourceSample)
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
				parents: []string{input.Records[frameIndex].TraceID},
				atNS:    revision.SourceSample*1_000_000_000/24_000 + timing.StreamingRevisionNS,
				order:   order, direction: openaiwire.DirectionServer, profile: openaiwire.ProfileRealtime,
				message: message,
			})
			order++
		}
	}
	finalRevision, err := perception.Finalize(ctx, input.SampleCount)
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

	endpointKey := input.Records[len(input.Records)-1].TraceID
	lastRevisionKey := endpointKey
	if len(revisions) > 0 {
		lastRevisionKey = fmt.Sprintf("trial_%02d_revision_%04d", index, revisions[len(revisions)-1].RevisionID)
	}
	stage := timing.Stages
	perceptionDoneNS := input.EndpointNS + stage.PerceptionFinalizeNS
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
		"usage": map[string]any{"type": "duration", "seconds": float64(input.EndpointNS) / 1_000_000_000},
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
	observed := firstOutputPlaybackNS - input.EndpointNS
	sum := stage.Sum()
	return Trial{
		Index: index, Seed: seed, EndpointNS: input.EndpointNS,
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

func summarize(trials []Trial) (map[string]Distribution, error) {
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
		distribution, err := analysis.Summarize(values)
		if err != nil {
			return nil, err
		}
		result[name] = distribution
	}
	return result, nil
}
