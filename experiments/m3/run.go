// Package m3 runs deterministic duplex, interruption, truncation, and repair scenarios.
package m3

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/analysis"
	"github.com/bojieli/OpenRealtime/duplex"
	"github.com/bojieli/OpenRealtime/engine"
	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/internal/fixture"
	"github.com/bojieli/OpenRealtime/internal/wiretrace"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/speech"
	"github.com/bojieli/OpenRealtime/trace"
)

const (
	responseCreatedNS = uint64(900_000_000)
	playbackStartedNS = uint64(1_000_000_000)
	evidenceOffsetNS  = uint64(40_000_000)
	repairDelayNS     = uint64(20_000_000)
)

type ScenarioKind string

const (
	ScenarioDirectedInterruption ScenarioKind = "directed_interruption"
	ScenarioListenerBackchannel  ScenarioKind = "listener_backchannel"
	ScenarioSideSpeech           ScenarioKind = "side_speech"
	ScenarioInvalidationRepair   ScenarioKind = "invalidation_repair"
)

type Config struct {
	FixturePath string
	Manifest    reference.Manifest
	Trials      uint64
	Seed        uint64
	FrameMS     uint32
}

type Trial struct {
	Index                       uint64           `json:"index"`
	Seed                        uint64           `json:"seed"`
	Scenario                    ScenarioKind     `json:"scenario"`
	EvidenceAtNS                uint64           `json:"evidence_at_ns"`
	StopAtNS                    uint64           `json:"stop_at_ns"`
	StopLatencyNS               uint64           `json:"stop_latency_ns"`
	ExpectedStop                bool             `json:"expected_stop"`
	Stopped                     bool             `json:"stopped"`
	FalseStop                   bool             `json:"false_stop"`
	FailureToStop               bool             `json:"failure_to_stop"`
	ContinuousInputDuringOutput bool             `json:"continuous_input_during_output"`
	Decision                    duplex.Decision  `json:"decision"`
	FinalTurnState              duplex.TurnState `json:"final_turn_state"`
	PlayedSamples               uint64           `json:"played_samples"`
	DiscardedQueuedSamples      uint64           `json:"discarded_queued_samples"`
	DiscardedPreparedSamples    uint64           `json:"discarded_prepared_samples"`
	RepairCount                 uint64           `json:"repair_count"`
	PlayedHistoryPreserved      bool             `json:"played_history_preserved"`
	Commit                      speech.Snapshot  `json:"commit"`
	Trace                       []trace.Record   `json:"-"`
}

type Condition struct {
	Scenario              ScenarioKind           `json:"scenario"`
	Trials                []Trial                `json:"trials"`
	StopLatency           *analysis.Distribution `json:"stop_latency_ns,omitempty"`
	FalseStopCount        uint64                 `json:"false_stop_count"`
	FailureToStopCount    uint64                 `json:"failure_to_stop_count"`
	RepairCount           uint64                 `json:"repair_count"`
	HistoryViolationCount uint64                 `json:"history_violation_count"`
	ProtocolRecordCount   analysis.Distribution  `json:"protocol_record_count"`
}

type Report struct {
	SchemaVersion string        `json:"schema_version"`
	Experiment    string        `json:"experiment"`
	TimingMode    string        `json:"timing_mode"`
	EvidenceScope string        `json:"evidence_scope"`
	FixtureSHA256 string        `json:"fixture_sha256"`
	Seed          uint64        `json:"seed"`
	Trials        uint64        `json:"trials_per_scenario"`
	DuplexPolicy  duplex.Policy `json:"duplex_policy"`
	Scenarios     []Condition   `json:"scenarios"`
}

func Run(ctx context.Context, config Config) (Report, error) {
	if config.FixturePath == "" || config.Trials == 0 || config.FrameMS == 0 {
		return Report{}, errors.New("M3 requires a fixture and positive trials/frame duration")
	}
	digest, err := audio.HashFile(config.FixturePath)
	if err != nil {
		return Report{}, err
	}
	fixtureHash := hex.EncodeToString(digest[:])
	if fixtureHash != config.Manifest.FixtureSHA256 {
		return Report{}, fmt.Errorf("fixture SHA-256 %s does not match manifest %s", fixtureHash, config.Manifest.FixtureSHA256)
	}
	input, err := fixture.Load(config.FixturePath, config.FrameMS, "m3-input")
	if err != nil {
		return Report{}, err
	}
	if len(input.Frames) == 0 {
		return Report{}, errors.New("M3 fixture contains no input frames")
	}
	chunks, err := streamReferenceSpeech(ctx, config.Manifest.ResponseText)
	if err != nil {
		return Report{}, err
	}
	policy := duplex.DefaultPolicy()
	validator := openaiwire.NewValidator()
	scenarios := []ScenarioKind{
		ScenarioDirectedInterruption, ScenarioListenerBackchannel,
		ScenarioSideSpeech, ScenarioInvalidationRepair,
	}
	report := Report{
		SchemaVersion: "0.1.0", Experiment: "M3_duplex_commit_horizon",
		TimingMode: "deterministic_simulation", EvidenceScope: "orchestration_only_nonsemantic_signal_audio",
		FixtureSHA256: fixtureHash, Seed: config.Seed, Trials: config.Trials, DuplexPolicy: policy,
		Scenarios: make([]Condition, 0, len(scenarios)),
	}
	for scenarioIndex, scenario := range scenarios {
		condition := Condition{Scenario: scenario, Trials: make([]Trial, 0, config.Trials)}
		for trialIndex := range config.Trials {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			seed := config.Seed + trialIndex
			trial, err := runTrial(
				ctx, trialIndex, seed, uint64(scenarioIndex), scenario, policy,
				config.Manifest.ResponseText, chunks, input.Frames[0].PCM16LE, validator,
			)
			if err != nil {
				return Report{}, fmt.Errorf("scenario %s trial %d: %w", scenario, trialIndex, err)
			}
			condition.Trials = append(condition.Trials, trial)
		}
		if err := summarize(&condition); err != nil {
			return Report{}, err
		}
		report.Scenarios = append(report.Scenarios, condition)
	}
	return report, nil
}

func streamReferenceSpeech(ctx context.Context, text string) ([]engine.SpeechChunk, error) {
	provider := reference.NewSpeech(100)
	if !provider.Capabilities()[engine.CapabilityStreamingOutput] {
		return nil, errors.New("M3 speech provider must declare streaming output")
	}
	var chunks []engine.SpeechChunk
	err := provider.Stream(ctx, engine.SpeechPlan{CandidateID: "candidate_m3", Text: text}, func(chunk engine.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(chunks) != 5 || !chunks[len(chunks)-1].Final {
		return nil, errors.New("M3 reference speech must produce five 20 ms chunks")
	}
	return chunks, nil
}

func runTrial(
	ctx context.Context,
	index uint64,
	seed uint64,
	scenarioIndex uint64,
	scenario ScenarioKind,
	policy duplex.Policy,
	text string,
	chunks []engine.SpeechChunk,
	inputPCM []byte,
	validator *openaiwire.Validator,
) (Trial, error) {
	if err := ctx.Err(); err != nil {
		return Trial{}, err
	}
	plan, err := speech.NewPlan(speech.Config{
		PlanID: fmt.Sprintf("plan_%s_%04d", scenario, index), CandidateID: "candidate_m3", Text: text,
		SampleRateHz: audio.OpenAIPCMSampleRate, MaxPreparedAheadSamples: 2_400, MaxQueuedAheadSamples: 960,
	})
	if err != nil {
		return Trial{}, err
	}
	for chunkIndex, chunk := range chunks {
		if err := plan.Prepare(chunk, responseCreatedNS+uint64(chunkIndex)*1_000_000); err != nil {
			return Trial{}, err
		}
	}
	if err := plan.QueueThrough(960, playbackStartedNS); err != nil {
		return Trial{}, err
	}
	machine, err := duplex.NewStateMachine(duplex.StateListening)
	if err != nil {
		return Trial{}, err
	}
	if err := machine.Transition(duplex.StateSystemPreparing); err != nil {
		return Trial{}, err
	}
	if err := machine.Transition(duplex.StateSystemSpeaking); err != nil {
		return Trial{}, err
	}
	evidenceAtNS := playbackStartedNS + evidenceOffsetNS
	if err := plan.MarkPlayedThrough(960, evidenceAtNS); err != nil {
		return Trial{}, err
	}
	if err := plan.QueueThrough(1_920, evidenceAtNS); err != nil {
		return Trial{}, err
	}

	trial := Trial{Index: index, Seed: seed, Scenario: scenario, EvidenceAtNS: evidenceAtNS}
	var cancellation speech.Cancellation
	switch scenario {
	case ScenarioDirectedInterruption:
		trial.ExpectedStop = true
		trial.ContinuousInputDuringOutput = true
		trial.Decision, err = policy.Decide(machine.State(), duplex.Evidence{
			Kind: duplex.EvidenceDirectedSpeech, Confidence: 0.92, AtNS: evidenceAtNS,
		})
		if err != nil {
			return Trial{}, err
		}
		if trial.Decision != duplex.DecisionYield {
			return Trial{}, errors.New("directed interruption did not yield")
		}
		if err := machine.Transition(duplex.StateOverlapUserInterruption); err != nil {
			return Trial{}, err
		}
		stopLatencyNS := sampleStopLatency(seed, scenarioIndex)
		trial.StopAtNS = evidenceAtNS + stopLatencyNS
		trial.StopLatencyNS = stopLatencyNS
		playedSamples := (trial.StopAtNS - playbackStartedNS) * uint64(audio.OpenAIPCMSampleRate) / 1_000_000_000
		if err := plan.MarkPlayedThrough(playedSamples, trial.StopAtNS); err != nil {
			return Trial{}, err
		}
		cancellation, err = plan.Cancel(speech.CancelYield, "directed user interruption", trial.StopAtNS)
		if err != nil {
			return Trial{}, err
		}
		trial.Stopped = true
		if err := machine.Transition(duplex.StateUserSpeaking); err != nil {
			return Trial{}, err
		}
	case ScenarioListenerBackchannel, ScenarioSideSpeech:
		trial.ContinuousInputDuringOutput = true
		kind := duplex.EvidenceBackchannel
		if scenario == ScenarioSideSpeech {
			kind = duplex.EvidenceSideSpeech
		}
		trial.Decision, err = policy.Decide(machine.State(), duplex.Evidence{Kind: kind, Confidence: 0.9, AtNS: evidenceAtNS})
		if err != nil {
			return Trial{}, err
		}
		if scenario == ScenarioListenerBackchannel {
			if trial.Decision != duplex.DecisionNoteBackchannel {
				return Trial{}, errors.New("listener backchannel was not preserved")
			}
			if err := machine.Transition(duplex.StateOverlapBackchannel); err != nil {
				return Trial{}, err
			}
			if err := machine.Transition(duplex.StateSystemSpeaking); err != nil {
				return Trial{}, err
			}
		} else if trial.Decision != duplex.DecisionIgnoreSide {
			return Trial{}, errors.New("side speech was not ignored")
		}
		if err := finishPlayback(plan); err != nil {
			return Trial{}, err
		}
		if err := machine.Transition(duplex.StateListening); err != nil {
			return Trial{}, err
		}
	case ScenarioInvalidationRepair:
		trial.ExpectedStop = true
		trial.Decision = duplex.DecisionYield
		trial.StopAtNS = evidenceAtNS
		if err := machine.Transition(duplex.StateRepairing); err != nil {
			return Trial{}, err
		}
		cancellation, err = plan.Cancel(speech.CancelInvalidate, "new evidence invalidated played output", trial.StopAtNS)
		if err != nil {
			return Trial{}, err
		}
		trial.Stopped = true
		if err := plan.RecordRepair("Correction: the previous response was invalidated.", trial.StopAtNS+repairDelayNS); err != nil {
			return Trial{}, err
		}
		if err := machine.Transition(duplex.StateListening); err != nil {
			return Trial{}, err
		}
	default:
		return Trial{}, errors.New("unknown M3 scenario")
	}
	if err := plan.ValidateClosed(); err != nil {
		return Trial{}, err
	}
	snapshot := plan.Snapshot()
	trial.FalseStop = trial.Stopped && !trial.ExpectedStop
	trial.FailureToStop = !trial.Stopped && trial.ExpectedStop
	trial.FinalTurnState = machine.State()
	trial.PlayedSamples = snapshot.PlayedSamples
	trial.DiscardedQueuedSamples = cancellation.DiscardedQueuedSamples
	trial.DiscardedPreparedSamples = cancellation.DiscardedPreparedSamples
	trial.RepairCount = uint64(len(snapshot.Repairs))
	trial.PlayedHistoryPreserved = snapshot.PlayedSamples == 0 ||
		(len(snapshot.PlayedHistory) == 1 && snapshot.PlayedHistory[0].PlayedSamples == snapshot.PlayedSamples)
	trial.Commit = snapshot
	trial.Trace, err = buildTrace(
		scenario, index, text, chunks, inputPCM, trial.StopAtNS, snapshot.PlayedSamples, validator,
	)
	if err != nil {
		return Trial{}, err
	}
	return trial, nil
}

func finishPlayback(plan *speech.Plan) error {
	steps := []struct {
		atNS   uint64
		played uint64
		queue  uint64
	}{
		{playbackStartedNS + 60_000_000, 1_440, 2_400},
		{playbackStartedNS + 80_000_000, 1_920, 2_400},
		{playbackStartedNS + 100_000_000, 2_400, 2_400},
	}
	for _, step := range steps {
		if err := plan.MarkPlayedThrough(step.played, step.atNS); err != nil {
			return err
		}
		if step.queue > plan.Snapshot().QueuedSamples {
			if err := plan.QueueThrough(step.queue, step.atNS); err != nil {
				return err
			}
		}
	}
	return plan.Complete(playbackStartedNS + 100_000_000)
}

func sampleStopLatency(seed, scenarioIndex uint64) uint64 {
	random := rand.New(rand.NewPCG(seed^0x243f6a8885a308d3, scenarioIndex^0x13198a2e03707344))
	return (12 + random.Uint64N(17)) * 1_000_000
}

func buildTrace(
	scenario ScenarioKind,
	trialIndex uint64,
	text string,
	chunks []engine.SpeechChunk,
	inputPCM []byte,
	stopAtNS uint64,
	playedSamples uint64,
	validator *openaiwire.Validator,
) ([]trace.Record, error) {
	responseID := fmt.Sprintf("resp_m3_%s_%04d", scenario, trialIndex)
	itemID := fmt.Sprintf("item_m3_%s_%04d", scenario, trialIndex)
	var events []wiretrace.Event
	var order uint64
	previous := ""
	add := func(key string, atNS uint64, direction openaiwire.Direction, value map[string]any) error {
		message, err := wiretrace.Marshal(value)
		if err != nil {
			return err
		}
		parents := []string{}
		if previous != "" {
			parents = append(parents, previous)
		}
		events = append(events, wiretrace.Event{
			Key: key, Parents: parents, AtNS: atNS, Order: order,
			Direction: direction, Profile: openaiwire.ProfileRealtime, Message: message,
		})
		previous = key
		order++
		return nil
	}
	prefix := fmt.Sprintf("m3_%s_%04d", scenario, trialIndex)
	server := openaiwire.DirectionServer
	client := openaiwire.DirectionClient
	baseEvents := []struct {
		suffix string
		atNS   uint64
		value  map[string]any
	}{
		{"response_created", responseCreatedNS, map[string]any{"event_id": "event_" + prefix + "_created", "type": openaiwire.EventResponseCreated, "response": map[string]any{"id": responseID, "object": "realtime.response", "status": "in_progress", "output": []any{}}}},
		{"output_item", responseCreatedNS, map[string]any{"event_id": "event_" + prefix + "_item", "type": openaiwire.EventResponseOutputItemAdded, "response_id": responseID, "output_index": 0, "item": map[string]any{"id": itemID, "object": "realtime.item", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}},
		{"content_part", responseCreatedNS, map[string]any{"event_id": "event_" + prefix + "_part", "type": openaiwire.EventResponseContentPartAdded, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "audio", "transcript": text}}},
		{"transcript", responseCreatedNS + 20_000_000, map[string]any{"event_id": "event_" + prefix + "_transcript", "type": openaiwire.EventResponseOutputAudioTranscriptDone, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "transcript": text}},
	}
	for _, event := range baseEvents {
		if err := add(prefix+"_"+event.suffix, event.atNS, server, event.value); err != nil {
			return nil, err
		}
	}
	for index, chunk := range chunks {
		if err := add(fmt.Sprintf("%s_audio_%02d", prefix, index), responseCreatedNS+30_000_000+uint64(index)*1_000_000, server, map[string]any{
			"event_id": fmt.Sprintf("event_%s_audio_%02d", prefix, index), "type": openaiwire.EventResponseOutputAudioDelta,
			"response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0,
			"delta": base64.StdEncoding.EncodeToString(chunk.PCM16LE),
		}); err != nil {
			return nil, err
		}
	}
	if err := add(prefix+"_playback_started", playbackStartedNS, server, map[string]any{
		"event_id": "event_" + prefix + "_started", "type": openaiwire.EventOutputAudioBufferStarted, "response_id": responseID,
	}); err != nil {
		return nil, err
	}
	if scenario != ScenarioInvalidationRepair {
		if err := add(prefix+"_input_during_output", playbackStartedNS+evidenceOffsetNS, client, map[string]any{
			"event_id": "event_" + prefix + "_input", "type": openaiwire.EventInputAudioBufferAppend,
			"audio": base64.StdEncoding.EncodeToString(inputPCM),
		}); err != nil {
			return nil, err
		}
	}
	if stopAtNS != 0 {
		audioEndMS := playedSamples * 1_000 / uint64(audio.OpenAIPCMSampleRate)
		cancelEvents := []struct {
			suffix    string
			direction openaiwire.Direction
			value     map[string]any
		}{
			{"cancel", client, map[string]any{"event_id": "event_" + prefix + "_cancel", "type": openaiwire.EventResponseCancel, "response_id": responseID}},
			{"clear", client, map[string]any{"event_id": "event_" + prefix + "_clear", "type": openaiwire.EventOutputAudioBufferClear}},
			{"truncate", client, map[string]any{"event_id": "event_" + prefix + "_truncate", "type": openaiwire.EventConversationItemTruncate, "item_id": itemID, "content_index": 0, "audio_end_ms": audioEndMS}},
			{"cleared", server, map[string]any{"event_id": "event_" + prefix + "_cleared", "type": openaiwire.EventOutputAudioBufferCleared, "response_id": responseID}},
			{"truncated", server, map[string]any{"event_id": "event_" + prefix + "_truncated", "type": openaiwire.EventConversationItemTruncated, "item_id": itemID, "content_index": 0, "audio_end_ms": audioEndMS}},
			{"response_done", server, map[string]any{"event_id": "event_" + prefix + "_done", "type": openaiwire.EventResponseDone, "response": map[string]any{"id": responseID, "object": "realtime.response", "status": "cancelled", "status_details": map[string]any{"type": "cancelled", "reason": "client_cancelled"}, "output": []any{}}}},
		}
		for _, event := range cancelEvents {
			if err := add(prefix+"_"+event.suffix, stopAtNS, event.direction, event.value); err != nil {
				return nil, err
			}
		}
	} else {
		doneAtNS := playbackStartedNS + 100_000_000
		completedEvents := []struct {
			suffix string
			value  map[string]any
		}{
			{"audio_done", map[string]any{"event_id": "event_" + prefix + "_audio_done", "type": openaiwire.EventResponseOutputAudioDone, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0}},
			{"part_done", map[string]any{"event_id": "event_" + prefix + "_part_done", "type": openaiwire.EventResponseContentPartDone, "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "audio", "transcript": text}}},
			{"item_done", map[string]any{"event_id": "event_" + prefix + "_item_done", "type": openaiwire.EventResponseOutputItemDone, "response_id": responseID, "output_index": 0, "item": map[string]any{"id": itemID, "object": "realtime.item", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_audio", "transcript": text}}}}},
			{"response_done", map[string]any{"event_id": "event_" + prefix + "_done", "type": openaiwire.EventResponseDone, "response": map[string]any{"id": responseID, "object": "realtime.response", "status": "completed", "output": []any{}}}},
			{"playback_stopped", map[string]any{"event_id": "event_" + prefix + "_stopped", "type": openaiwire.EventOutputAudioBufferStopped, "response_id": responseID}},
		}
		for _, event := range completedEvents {
			if err := add(prefix+"_"+event.suffix, doneAtNS, server, event.value); err != nil {
				return nil, err
			}
		}
	}
	return wiretrace.Materialize(events, fmt.Sprintf("m3-%s-%04d", scenario, trialIndex), validator)
}

func summarize(condition *Condition) error {
	stopLatencies := []uint64{}
	recordCounts := make([]uint64, 0, len(condition.Trials))
	for _, trial := range condition.Trials {
		if trial.Stopped && condition.Scenario == ScenarioDirectedInterruption {
			stopLatencies = append(stopLatencies, trial.StopLatencyNS)
		}
		if trial.FalseStop {
			condition.FalseStopCount++
		}
		if trial.FailureToStop {
			condition.FailureToStopCount++
		}
		condition.RepairCount += trial.RepairCount
		if !trial.PlayedHistoryPreserved {
			condition.HistoryViolationCount++
		}
		recordCounts = append(recordCounts, uint64(len(trial.Trace)))
	}
	if len(stopLatencies) > 0 {
		distribution, err := analysis.Summarize(stopLatencies)
		if err != nil {
			return err
		}
		condition.StopLatency = &distribution
	}
	distribution, err := analysis.Summarize(recordCounts)
	if err != nil {
		return err
	}
	condition.ProtocolRecordCount = distribution
	return nil
}
