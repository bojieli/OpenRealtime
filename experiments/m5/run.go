// Package m5 runs deterministic translation and rapid audio game demonstrations.
package m5

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/analysis"
	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/internal/fixture"
	"github.com/bojieli/OpenRealtime/internal/wiretrace"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/rapidgame"
	"github.com/bojieli/OpenRealtime/trace"
	"github.com/bojieli/OpenRealtime/translation"
)

type Config struct {
	FixturePath       string
	DemonstrationPath string
	Demonstrations    reference.Demonstrations
	Trials            uint64
	Seed              uint64
}

type TranslationTrial struct {
	Index      uint64                 `json:"index"`
	Seed       uint64                 `json:"seed"`
	Evaluation translation.Evaluation `json:"evaluation"`
	Trace      []trace.Record         `json:"-"`
}

type TranslationCondition struct {
	Policy              translation.PolicyKind      `json:"policy"`
	Trials              []TranslationTrial          `json:"trials"`
	MeanLag             analysis.Distribution       `json:"mean_lag_ns"`
	CompletionLag       analysis.Distribution       `json:"completion_lag_ns"`
	Quality             analysis.ScalarDistribution `json:"quality_score"`
	Compute             analysis.ScalarDistribution `json:"compute_units"`
	Failures            analysis.ScalarDistribution `json:"failures_per_trial"`
	FailureCount        uint64                      `json:"failure_count"`
	ProtocolRecordCount analysis.ScalarDistribution `json:"protocol_record_count"`
}

type TranslationStudy struct {
	EngineFrameMS       uint64                 `json:"engine_frame_ms"`
	ProtocolProfile     openaiwire.Profile     `json:"protocol_profile"`
	Conditions          []TranslationCondition `json:"conditions"`
	ProtocolValidTraces uint64                 `json:"protocol_valid_traces"`
}

type GameTrial struct {
	Index      uint64               `json:"index"`
	Seed       uint64               `json:"seed"`
	Evaluation rapidgame.Evaluation `json:"evaluation"`
	Trace      []trace.Record       `json:"-"`
}

type GameCondition struct {
	Condition           rapidgame.ConditionKind     `json:"condition"`
	Trials              []GameTrial                 `json:"trials"`
	ReactionLatency     analysis.Distribution       `json:"reaction_latency_ns"`
	Quality             analysis.ScalarDistribution `json:"quality_score"`
	Compute             analysis.ScalarDistribution `json:"compute_units"`
	Failures            analysis.ScalarDistribution `json:"failures_per_trial"`
	FailureCount        uint64                      `json:"failure_count"`
	CorrectActionCount  uint64                      `json:"correct_action_count"`
	ProtocolRecordCount analysis.ScalarDistribution `json:"protocol_record_count"`
}

type GameStudy struct {
	Name                string             `json:"name"`
	ProtocolProfile     openaiwire.Profile `json:"protocol_profile"`
	Conditions          []GameCondition    `json:"conditions"`
	ProtocolValidTraces uint64             `json:"protocol_valid_traces"`
}

type Report struct {
	SchemaVersion       string           `json:"schema_version"`
	Experiment          string           `json:"experiment"`
	TimingMode          string           `json:"timing_mode"`
	EvidenceScope       string           `json:"evidence_scope"`
	FixtureSHA256       string           `json:"fixture_sha256"`
	DemonstrationSHA256 string           `json:"demonstration_sha256"`
	Seed                uint64           `json:"seed"`
	TrialsPerCondition  uint64           `json:"trials_per_condition"`
	Translation         TranslationStudy `json:"translation"`
	Game                GameStudy        `json:"game"`
}

func Run(ctx context.Context, config Config) (Report, error) {
	if config.FixturePath == "" || config.DemonstrationPath == "" || config.Trials == 0 {
		return Report{}, errors.New("M5 requires fixture and demonstration paths plus positive trials")
	}
	fixtureDigest, err := audio.HashFile(config.FixturePath)
	if err != nil {
		return Report{}, err
	}
	fixtureHash := hex.EncodeToString(fixtureDigest[:])
	if fixtureHash != config.Demonstrations.FixtureSHA256 {
		return Report{}, fmt.Errorf("fixture SHA-256 %s does not match demonstrations %s", fixtureHash, config.Demonstrations.FixtureSHA256)
	}
	demonstrationHash, err := hashFile(config.DemonstrationPath)
	if err != nil {
		return Report{}, err
	}
	translationInput, err := fixture.Load(config.FixturePath, 200, "m5-translation-input")
	if err != nil {
		return Report{}, err
	}
	gameInput, err := fixture.Load(config.FixturePath, 20, "m5-game-input")
	if err != nil {
		return Report{}, err
	}
	segments := convertTranslationSegments(config.Demonstrations.Translation.Segments)
	lastSegmentMS := config.Demonstrations.Translation.Segments[len(segments)-1].EndMS
	if lastSegmentMS > math.MaxUint64/1_000_000 || lastSegmentMS*1_000_000 != translationInput.EndpointNS {
		return Report{}, errors.New("translation segments must span the complete audio fixture")
	}
	if len(translationInput.Frames) != len(segments) {
		return Report{}, errors.New("translation demonstration requires one authored segment per 200 ms engine frame")
	}
	rounds := convertGameRounds(config.Demonstrations.Game.Rounds)
	validator := openaiwire.NewValidator()
	report := Report{
		SchemaVersion: "0.1.0", Experiment: "M5_translation_and_rapid_interaction",
		TimingMode: "deterministic_simulation", EvidenceScope: "symbolic_exact_match_and_deadline_instrumentation_no_model_quality_claim",
		FixtureSHA256: fixtureHash, DemonstrationSHA256: demonstrationHash,
		Seed: config.Seed, TrialsPerCondition: config.Trials,
		Translation: TranslationStudy{EngineFrameMS: 200, ProtocolProfile: openaiwire.ProfileTranslation},
		Game:        GameStudy{Name: config.Demonstrations.Game.Name, ProtocolProfile: openaiwire.ProfileRealtime},
	}
	for _, policy := range translation.DefaultPolicies() {
		condition := TranslationCondition{Policy: policy.Kind, Trials: make([]TranslationTrial, 0, config.Trials)}
		for trialIndex := range config.Trials {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			seed := config.Seed + trialIndex
			evaluation, err := policy.Evaluate(segments, seed)
			if err != nil {
				return Report{}, fmt.Errorf("translation %s trial %d: %w", policy.Kind, trialIndex, err)
			}
			records, err := translationTrace(trialIndex, policy.Kind, evaluation, config.Demonstrations.Translation, translationInput, validator)
			if err != nil {
				return Report{}, fmt.Errorf("translation %s trial %d trace: %w", policy.Kind, trialIndex, err)
			}
			condition.Trials = append(condition.Trials, TranslationTrial{Index: trialIndex, Seed: seed, Evaluation: evaluation, Trace: records})
			report.Translation.ProtocolValidTraces++
		}
		if err := summarizeTranslation(&condition); err != nil {
			return Report{}, err
		}
		report.Translation.Conditions = append(report.Translation.Conditions, condition)
	}
	for _, gameCondition := range rapidgame.DefaultConditions() {
		condition := GameCondition{Condition: gameCondition.Kind, Trials: make([]GameTrial, 0, config.Trials)}
		for trialIndex := range config.Trials {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			seed := config.Seed + trialIndex
			evaluation, err := gameCondition.Evaluate(rounds, seed)
			if err != nil {
				return Report{}, fmt.Errorf("game %s trial %d: %w", gameCondition.Kind, trialIndex, err)
			}
			records, err := gameTrace(trialIndex, gameCondition.Kind, evaluation, rounds, gameInput, validator)
			if err != nil {
				return Report{}, fmt.Errorf("game %s trial %d trace: %w", gameCondition.Kind, trialIndex, err)
			}
			condition.Trials = append(condition.Trials, GameTrial{Index: trialIndex, Seed: seed, Evaluation: evaluation, Trace: records})
			report.Game.ProtocolValidTraces++
		}
		if err := summarizeGame(&condition); err != nil {
			return Report{}, err
		}
		report.Game.Conditions = append(report.Game.Conditions, condition)
	}
	return report, nil
}

func translationTrace(
	trialIndex uint64,
	policy translation.PolicyKind,
	evaluation translation.Evaluation,
	demonstration reference.TranslationDemonstration,
	input fixture.Input,
	validator *openaiwire.Validator,
) ([]trace.Record, error) {
	prefix := fmt.Sprintf("m5_translation_%s_%04d", policy, trialIndex)
	sessionID := "sess_" + prefix
	session := map[string]any{
		"id": sessionID, "type": "translation", "expires_at": 1_800_000_000,
		"model": "gpt-realtime-translate",
		"audio": map[string]any{
			"input":  map[string]any{"transcription": map[string]any{"model": "gpt-realtime-whisper"}, "noise_reduction": nil},
			"output": map[string]any{"language": demonstration.TargetLanguage},
		},
	}
	var events []wiretrace.Event
	var order uint64
	add := func(key string, atNS uint64, direction openaiwire.Direction, value map[string]any) error {
		message, err := wiretrace.Marshal(value)
		if err != nil {
			return err
		}
		events = append(events, wiretrace.Event{Key: prefix + "_" + key, AtNS: atNS, Order: order, Direction: direction, Profile: openaiwire.ProfileTranslation, Message: message})
		order++
		return nil
	}
	server := openaiwire.DirectionServer
	client := openaiwire.DirectionClient
	if err := add("created", 0, server, map[string]any{"event_id": "event_" + prefix + "_created", "type": "session.created", "session": session}); err != nil {
		return nil, err
	}
	update := map[string]any{"audio": map[string]any{
		"input":  map[string]any{"transcription": map[string]any{"model": "gpt-realtime-whisper"}, "noise_reduction": nil},
		"output": map[string]any{"language": demonstration.TargetLanguage},
	}}
	if err := add("update", 1, client, map[string]any{"event_id": "event_" + prefix + "_update", "type": "session.update", "session": update}); err != nil {
		return nil, err
	}
	if err := add("updated", 2, server, map[string]any{"event_id": "event_" + prefix + "_updated", "type": "session.updated", "session": session}); err != nil {
		return nil, err
	}
	for index, frame := range input.Frames {
		atNS := uint64(index) * 200_000_000
		if index == 0 {
			atNS = 3
		}
		if err := add(fmt.Sprintf("input_%02d", index), atNS, client, map[string]any{
			"event_id": fmt.Sprintf("event_%s_input_%02d", prefix, index), "type": "session.input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(frame.PCM16LE),
		}); err != nil {
			return nil, err
		}
		segment := demonstration.Segments[index]
		if err := add(fmt.Sprintf("source_%02d", index), segment.EndMS*1_000_000+10_000_000, server, map[string]any{
			"event_id": fmt.Sprintf("event_%s_source_%02d", prefix, index), "type": "session.input_transcript.delta",
			"delta": segment.SourceDelta, "elapsed_ms": segment.EndMS,
		}); err != nil {
			return nil, err
		}
	}
	finalEndNS := input.EndpointNS
	if err := add("close", finalEndNS+1, client, map[string]any{"event_id": "event_" + prefix + "_close", "type": "session.close"}); err != nil {
		return nil, err
	}
	var lastOutputNS uint64
	for index, emission := range evaluation.Emissions {
		elapsedMS := demonstration.Segments[index].EndMS
		if err := add(fmt.Sprintf("target_%02d", index), emission.EmittedAtNS, server, map[string]any{
			"event_id": fmt.Sprintf("event_%s_target_%02d", prefix, index), "type": "session.output_transcript.delta",
			"delta": emission.TargetDelta, "elapsed_ms": elapsedMS,
		}); err != nil {
			return nil, err
		}
		if err := add(fmt.Sprintf("audio_%02d", index), emission.EmittedAtNS+1, server, map[string]any{
			"event_id": fmt.Sprintf("event_%s_audio_%02d", prefix, index), "type": "session.output_audio.delta",
			"delta": base64.StdEncoding.EncodeToString(input.Frames[index].PCM16LE), "sample_rate": 24_000,
			"channels": 1, "format": "pcm16", "elapsed_ms": elapsedMS,
		}); err != nil {
			return nil, err
		}
		lastOutputNS = max(lastOutputNS, emission.EmittedAtNS+1)
	}
	closedAtNS := max(finalEndNS+1, lastOutputNS) + 10_000_000
	if err := add("closed", closedAtNS, server, map[string]any{"event_id": "event_" + prefix + "_closed", "type": "session.closed"}); err != nil {
		return nil, err
	}
	chainScheduled(events)
	return wiretrace.Materialize(events, prefix, validator)
}

func gameTrace(
	trialIndex uint64,
	condition rapidgame.ConditionKind,
	evaluation rapidgame.Evaluation,
	rounds []rapidgame.Round,
	input fixture.Input,
	validator *openaiwire.Validator,
) ([]trace.Record, error) {
	prefix := fmt.Sprintf("m5_game_%s_%04d", condition, trialIndex)
	var events []wiretrace.Event
	var order uint64
	add := func(key string, atNS uint64, direction openaiwire.Direction, value map[string]any) error {
		message, err := wiretrace.Marshal(value)
		if err != nil {
			return err
		}
		events = append(events, wiretrace.Event{Key: prefix + "_" + key, AtNS: atNS, Order: order, Direction: direction, Profile: openaiwire.ProfileRealtime, Message: message})
		order++
		return nil
	}
	server := openaiwire.DirectionServer
	client := openaiwire.DirectionClient
	for index, round := range rounds {
		outcome := evaluation.Outcomes[index]
		responseID := fmt.Sprintf("resp_%s_%02d", prefix, index)
		itemID := fmt.Sprintf("item_%s_%02d", prefix, index)
		base := round.CueAtMS * 1_000_000
		common := []struct {
			key   string
			atNS  uint64
			value map[string]any
		}{
			{"created", base, map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_created", prefix, index), "type": openaiwire.EventResponseCreated, "response": map[string]any{"id": responseID, "object": "realtime.response", "status": "in_progress", "output": []any{}}}},
			{"item", base, map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_item", prefix, index), "type": openaiwire.EventResponseOutputItemAdded, "response_id": responseID, "output_index": 0, "item": map[string]any{"id": itemID, "object": "realtime.item", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}},
			{"part", base, map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_part", prefix, index), "type": openaiwire.EventResponseContentPartAdded, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "audio", "transcript": round.Prompt}}},
			{"transcript", base + 1, map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_transcript", prefix, index), "type": openaiwire.EventResponseOutputAudioTranscriptDone, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "transcript": round.Prompt}},
			{"audio", base + 2, map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_audio", prefix, index), "type": openaiwire.EventResponseOutputAudioDelta, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "delta": base64.StdEncoding.EncodeToString(input.Frames[index].PCM16LE)}},
			{"started", base + 3, map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_started", prefix, index), "type": openaiwire.EventOutputAudioBufferStarted, "response_id": responseID}},
		}
		for _, event := range common {
			if err := add(fmt.Sprintf("round_%02d_%s", index, event.key), event.atNS, server, event.value); err != nil {
				return nil, err
			}
		}
		doneAt := base + 20_000_000
		done := []struct {
			key   string
			value map[string]any
		}{
			{"audio_done", map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_audio_done", prefix, index), "type": openaiwire.EventResponseOutputAudioDone, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0}},
			{"part_done", map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_part_done", prefix, index), "type": openaiwire.EventResponseContentPartDone, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "audio", "transcript": round.Prompt}}},
			{"item_done", map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_item_done", prefix, index), "type": openaiwire.EventResponseOutputItemDone, "response_id": responseID, "output_index": 0, "item": map[string]any{"id": itemID, "object": "realtime.item", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_audio", "transcript": round.Prompt}}}}},
			{"response_done", map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_response_done", prefix, index), "type": openaiwire.EventResponseDone, "response": map[string]any{"id": responseID, "object": "realtime.response", "status": "completed", "output": []any{}}}},
			{"stopped", map[string]any{"event_id": fmt.Sprintf("event_%s_%02d_stopped", prefix, index), "type": openaiwire.EventOutputAudioBufferStopped, "response_id": responseID}},
		}
		for _, event := range done {
			if err := add(fmt.Sprintf("round_%02d_%s", index, event.key), doneAt, server, event.value); err != nil {
				return nil, err
			}
		}
		if err := add(fmt.Sprintf("round_%02d_input", index), outcome.RespondedAtNS, client, map[string]any{
			"event_id": fmt.Sprintf("event_%s_%02d_input", prefix, index), "type": openaiwire.EventInputAudioBufferAppend,
			"audio": base64.StdEncoding.EncodeToString(input.Frames[index].PCM16LE),
		}); err != nil {
			return nil, err
		}
	}
	chainScheduled(events)
	return wiretrace.Materialize(events, prefix, validator)
}

func chainScheduled(events []wiretrace.Event) {
	sort.SliceStable(events, func(left, right int) bool {
		if events[left].AtNS != events[right].AtNS {
			return events[left].AtNS < events[right].AtNS
		}
		return events[left].Order < events[right].Order
	})
	for index := range events {
		if index == 0 {
			events[index].Parents = nil
			continue
		}
		events[index].Parents = []string{events[index-1].Key}
	}
}

func summarizeTranslation(condition *TranslationCondition) error {
	meanLag := make([]uint64, 0, len(condition.Trials))
	completionLag := make([]uint64, 0, len(condition.Trials))
	quality := make([]uint64, 0, len(condition.Trials))
	compute := make([]uint64, 0, len(condition.Trials))
	failures := make([]uint64, 0, len(condition.Trials))
	records := make([]uint64, 0, len(condition.Trials))
	for _, trial := range condition.Trials {
		meanLag = append(meanLag, trial.Evaluation.MeanLagNS)
		completionLag = append(completionLag, trial.Evaluation.CompletionLagNS)
		quality = append(quality, trial.Evaluation.QualityScore)
		compute = append(compute, trial.Evaluation.ComputeUnits)
		failures = append(failures, trial.Evaluation.FailureCount)
		condition.FailureCount += trial.Evaluation.FailureCount
		records = append(records, uint64(len(trial.Trace)))
	}
	var err error
	if condition.MeanLag, err = analysis.Summarize(meanLag); err != nil {
		return err
	}
	if condition.CompletionLag, err = analysis.Summarize(completionLag); err != nil {
		return err
	}
	if condition.Quality, err = analysis.SummarizeScalar(quality); err != nil {
		return err
	}
	if condition.Compute, err = analysis.SummarizeScalar(compute); err != nil {
		return err
	}
	if condition.Failures, err = analysis.SummarizeScalar(failures); err != nil {
		return err
	}
	condition.ProtocolRecordCount, err = analysis.SummarizeScalar(records)
	return err
}

func summarizeGame(condition *GameCondition) error {
	reactions := []uint64{}
	quality := make([]uint64, 0, len(condition.Trials))
	compute := make([]uint64, 0, len(condition.Trials))
	failures := make([]uint64, 0, len(condition.Trials))
	records := make([]uint64, 0, len(condition.Trials))
	for _, trial := range condition.Trials {
		quality = append(quality, trial.Evaluation.QualityScore)
		compute = append(compute, trial.Evaluation.ComputeUnits)
		failures = append(failures, trial.Evaluation.FailureCount)
		condition.FailureCount += trial.Evaluation.FailureCount
		for _, outcome := range trial.Evaluation.Outcomes {
			reactions = append(reactions, outcome.ReactionNS)
			if outcome.Correct {
				condition.CorrectActionCount++
			}
		}
		records = append(records, uint64(len(trial.Trace)))
	}
	var err error
	if condition.ReactionLatency, err = analysis.Summarize(reactions); err != nil {
		return err
	}
	if condition.Quality, err = analysis.SummarizeScalar(quality); err != nil {
		return err
	}
	if condition.Compute, err = analysis.SummarizeScalar(compute); err != nil {
		return err
	}
	if condition.Failures, err = analysis.SummarizeScalar(failures); err != nil {
		return err
	}
	condition.ProtocolRecordCount, err = analysis.SummarizeScalar(records)
	return err
}

func convertTranslationSegments(input []reference.TranslationSegment) []translation.Segment {
	result := make([]translation.Segment, len(input))
	for index, segment := range input {
		result[index] = translation.Segment{
			EndMS: segment.EndMS, SourceDelta: segment.SourceDelta,
			TargetDelta: segment.TargetDelta, EarlyTargetDelta: segment.EarlyTargetDelta,
		}
	}
	return result
}

func convertGameRounds(input []reference.GameRound) []rapidgame.Round {
	result := make([]rapidgame.Round, len(input))
	for index, round := range input {
		result[index] = rapidgame.Round{
			ID: round.ID, CueAtMS: round.CueAtMS, DeadlineMS: round.DeadlineMS,
			Prompt: round.Prompt, ExpectedAction: round.ExpectedAction,
		}
	}
	return result
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
