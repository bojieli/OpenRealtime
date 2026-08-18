// Package realtimebench composes real incremental ASR, fast/slow canonical
// continuations, tool execution, and streaming TTS into one measured run.
//
// The package is intentionally independent of the OpenAI Realtime wire
// protocol. It consumes the project's existing provider interfaces and emits
// secret-free evidence; fast/slow phases and scheduler cadence remain server
// internals.
package realtimebench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/bojieli/OpenRealtime/admission"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/audiobench"
	"github.com/bojieli/OpenRealtime/benchspec"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interleave"
	"github.com/bojieli/OpenRealtime/livebench"
	"github.com/bojieli/OpenRealtime/preparation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const SchemaVersion = "realtime-benchmark-v0.8"

// Scenario describes externally checkable behavior. Tool implementations are
// supplied separately through Config.Tools, keeping domain data and execution
// authority outside the orchestration package.
type Scenario struct {
	ID                 string
	InputSHA256        string
	ReferenceText      string
	ExpectedSubstrings []string
	RequiredToolCalls  []string
	ExpectedToolCalls  []benchspec.ToolCallExpectation
	AllowToolErrors    bool
	Audio              livebench.Audio
}

// Config provides one complete internal pipeline. The ASR provider may be
// wrapped by asrbuffer to decouple scheduler and provider cadence.
// SlowPreparationMinInterval controls only speculative launch frequency; zero
// preserves immediate fast-to-slow continuation and exact commit bypasses it.
type Config struct {
	Scenario                   Scenario
	ASR                        v1.PerceptionProvider
	FastProvider               continuation.Provider
	FastPreparationProvider    continuation.Provider
	SlowProvider               continuation.Provider
	SlowPreparationProvider    continuation.Provider
	TTS                        v1.StreamingSpeechProvider
	Tools                      interleave.ToolSet
	ASRFrameDuration           time.Duration
	Paced                      bool
	FastInstruction            string
	SlowInstruction            string
	FastMaxOutputTokens        int
	SlowMaxOutputTokens        int
	MaxSlowInvocations         int
	PrepareFastBeforeEndpoint  bool
	PrepareSlowBeforeEndpoint  bool
	SlowPreparationMinInterval time.Duration
	RetainReasoning            bool
	Runtime                    map[string]string
	AdmissionGovernor          *admission.Governor
	Clock                      func() time.Time
}

// ModelInvocation is provider timing and public accounting only. Opaque model
// state and plaintext reasoning are deliberately absent.
type ModelInvocation struct {
	InvocationID       string             `json:"invocation_id"`
	Phase              trajectory.Phase   `json:"phase"`
	Ordinal            int                `json:"ordinal"`
	StartOffsetMS      float64            `json:"start_offset_ms"`
	FirstEventMS       *float64           `json:"first_event_ms,omitempty"`
	FirstEventOffsetMS *float64           `json:"first_event_offset_ms,omitempty"`
	DurationMS         float64            `json:"duration_ms"`
	Usage              continuation.Usage `json:"usage,omitempty"`
	StopReason         string             `json:"stop_reason,omitempty"`
	SourceRevision     uint64             `json:"source_revision,omitempty"`
	Error              string             `json:"error,omitempty"`
}

// SpeechSegment records when one completed assistant segment entered TTS and
// when its first PCM became available. Result.OutputPCM16 is retained only in
// memory and is omitted by audiobench's JSON tags.
type SpeechSegment struct {
	Phase              trajectory.Phase     `json:"phase"`
	Ordinal            int                  `json:"ordinal"`
	StartOffsetMS      float64              `json:"start_offset_ms"`
	FirstAudioOffsetMS float64              `json:"first_audio_offset_ms"`
	Result             audiobench.TTSResult `json:"result"`
}

// AssistantSegment is the externally speakable canonical text in append
// order. Interrupted segments are retained for audit but never synthesized.
type AssistantSegment struct {
	Phase       trajectory.Phase `json:"phase"`
	Text        string           `json:"text"`
	Interrupted bool             `json:"interrupted,omitempty"`
}

// TrajectorySummary is a public projection. The hash excludes raw reasoning,
// opaque provider state, and private instructions.
type TrajectorySummary struct {
	Items        int            `json:"items"`
	PublicSHA256 string         `json:"public_sha256"`
	KindCounts   map[string]int `json:"kind_counts"`
}

// ScoringSpec makes the task-quality contract auditable from the report
// itself. Expected tool calls are an exact semantic multiset when present.
type ScoringSpec struct {
	Version            string                          `json:"version"`
	ExpectedSubstrings []string                        `json:"expected_substrings,omitempty"`
	RequiredToolCalls  []string                        `json:"required_tool_calls,omitempty"`
	ExpectedToolCalls  []benchspec.ToolCallExpectation `json:"expected_tool_calls,omitempty"`
	AllowToolErrors    bool                            `json:"allow_tool_errors,omitempty"`
}

// Report is a secret-free end-to-end evidence artifact.
type Report struct {
	SchemaVersion              string                   `json:"schema_version"`
	CreatedAt                  time.Time                `json:"created_at"`
	ScenarioID                 string                   `json:"scenario_id"`
	InputSHA256                string                   `json:"input_sha256"`
	ReferenceText              string                   `json:"reference_text,omitempty"`
	Scoring                    ScoringSpec              `json:"scoring"`
	Runtime                    map[string]string        `json:"runtime,omitempty"`
	Admission                  *admission.Snapshot      `json:"admission,omitempty"`
	FastPreparation            *preparation.Report      `json:"fast_preparation,omitempty"`
	BackgroundPreparation      *preparation.ChainReport `json:"background_preparation,omitempty"`
	Fast                       continuation.Descriptor  `json:"fast"`
	Slow                       continuation.Descriptor  `json:"slow"`
	TTS                        v1.Descriptor            `json:"tts"`
	ASR                        audiobench.ASRResult     `json:"asr"`
	ModelInvocations           []ModelInvocation        `json:"model_invocations"`
	Speech                     []SpeechSegment          `json:"speech,omitempty"`
	Assistant                  []AssistantSegment       `json:"assistant,omitempty"`
	ToolProposals              []trajectory.ToolCall    `json:"tool_proposals,omitempty"`
	ToolCalls                  []trajectory.ToolCall    `json:"tool_calls,omitempty"`
	ToolResults                []trajectory.ToolResult  `json:"tool_results,omitempty"`
	Trajectory                 TrajectorySummary        `json:"trajectory"`
	CommittedFastInvocationID  string                   `json:"committed_fast_invocation_id,omitempty"`
	EndpointToFastFirstEventMS *float64                 `json:"endpoint_to_fast_first_event_ms,omitempty"`
	EndpointToFastSafePointMS  *float64                 `json:"endpoint_to_fast_safe_point_ms,omitempty"`
	EndpointToFirstAudioMS     *float64                 `json:"endpoint_to_first_audio_ms,omitempty"`
	TotalDurationMS            float64                  `json:"total_duration_ms"`
	Passed                     bool                     `json:"passed"`
	ScoreFailures              []string                 `json:"score_failures,omitempty"`
	Errors                     []string                 `json:"errors,omitempty"`
}

// Run performs one paced audio rollout. Changed ASR revisions may prepare a
// private fast-to-slow chain while audio is still arriving. Only an exact
// final semantic match is replayed into the canonical trajectory; tool effects
// and speech remain canonical, serial, and endpoint-gated.
func Run(ctx context.Context, config Config) (Report, error) {
	if err := validateConfig(config); err != nil {
		return Report{}, err
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	started := config.Clock()
	report := Report{
		SchemaVersion: SchemaVersion, CreatedAt: started.UTC(),
		ScenarioID: config.Scenario.ID, InputSHA256: config.Scenario.InputSHA256,
		ReferenceText: config.Scenario.ReferenceText,
		Scoring:       scoringSpec(config.Scenario),
		Runtime:       cloneStringMap(config.Runtime), Fast: config.FastProvider.Descriptor(),
		Slow: config.SlowProvider.Descriptor(), TTS: config.TTS.Descriptor(),
	}
	if report.Runtime == nil {
		report.Runtime = make(map[string]string)
	}
	report.Runtime["wire_protocol"] = "unchanged-openai-realtime"

	fastFinal := newTimedProvider(config.FastProvider, started, config.Clock)
	fastPreparationProvider := config.FastPreparationProvider
	if fastPreparationProvider == nil {
		fastPreparationProvider = config.FastProvider
	}
	fastPrepared := newTimedProvider(fastPreparationProvider, started, config.Clock)
	slowFinal := newTimedProvider(config.SlowProvider, started, config.Clock)
	slowPreparationProvider := config.SlowPreparationProvider
	if slowPreparationProvider == nil {
		slowPreparationProvider = config.SlowProvider
	}
	slowPrepared := newTimedProvider(slowPreparationProvider, started, config.Clock)
	fastInstruction := config.FastInstruction
	if fastInstruction == "" {
		fastInstruction = interleave.DefaultFastInstruction
	}
	slowInstruction := config.SlowInstruction
	if slowInstruction == "" {
		slowInstruction = interleave.DefaultSlowInstruction
	}

	var preparationManager *preparation.Manager
	var chainManager *preparation.ChainManager
	if config.PrepareSlowBeforeEndpoint {
		var managerErr error
		chainManager, managerErr = preparation.NewChainManager(preparation.ChainConfig{
			Stages: []preparation.ChainStage{
				{
					Provider: fastPrepared,
					Invocation: makeInvocation(
						fastInstruction, config.Tools, config.FastMaxOutputTokens,
					),
				},
				{
					Provider: slowPrepared,
					Invocation: makeInvocation(
						slowInstruction, config.Tools, config.SlowMaxOutputTokens,
					),
					MinimumStartInterval: config.SlowPreparationMinInterval,
				},
			},
			RetainReasoning: true, Clock: config.Clock, Origin: started,
		})
		if managerErr != nil {
			return report, managerErr
		}
	} else if config.PrepareFastBeforeEndpoint {
		var managerErr error
		preparationManager, managerErr = preparation.NewManager(preparation.Config{
			Provider: fastPrepared, Clock: config.Clock, Origin: started,
		})
		if managerErr != nil {
			return report, managerErr
		}
	}
	onRevision := func(revision v1.PerceptionRevision) error { return nil }
	if chainManager != nil {
		onRevision = func(revision v1.PerceptionRevision) error {
			text := revision.StableText + revision.UnstableText
			if !preparation.NonEmptyObservation(text) {
				return nil
			}
			return chainManager.Observe(ctx, makeChainInput(revision.RevisionID, text))
		}
	} else if preparationManager != nil {
		onRevision = func(revision v1.PerceptionRevision) error {
			text := revision.StableText + revision.UnstableText
			if !preparation.NonEmptyObservation(text) {
				return nil
			}
			return preparationManager.Observe(ctx, makePreparationInput(
				fastPrepared.Descriptor(), fastInstruction, config.Tools,
				config.FastMaxOutputTokens, revision.RevisionID, text,
			))
		}
	}
	asr, err := audiobench.RunASR(ctx, config.ASR, audiobench.ASRConfig{
		CaseID: config.Scenario.ID, InputSHA256: config.Scenario.InputSHA256,
		ReferenceText: config.Scenario.ReferenceText, Audio: config.Scenario.Audio,
		FrameDuration: config.ASRFrameDuration, Paced: config.Paced,
		OnRevision: func(_ context.Context, revision v1.PerceptionRevision) error { return onRevision(revision) },
	})
	report.ASR = asr
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
		if chainManager != nil {
			preparationReport, closeErr := chainManager.Close(ctx, err)
			report.BackgroundPreparation = &preparationReport
			if closeErr != nil && !errors.Is(closeErr, ctx.Err()) {
				report.Errors = append(report.Errors, fmt.Sprintf("close background preparation: %v", closeErr))
			}
		}
		if preparationManager != nil {
			preparationReport, closeErr := preparationManager.Close(ctx, err)
			report.FastPreparation = &preparationReport
			if closeErr != nil && !errors.Is(closeErr, ctx.Err()) {
				report.Errors = append(report.Errors, fmt.Sprintf("close fast preparation: %v", closeErr))
			}
		}
		if config.AdmissionGovernor != nil {
			snapshot := config.AdmissionGovernor.Snapshot()
			report.Admission = &snapshot
		}
		report.TotalDurationMS = milliseconds(config.Clock().Sub(started))
		return report, err
	}

	store := trajectory.NewStore()
	finalRevision := uint64(1)
	if len(asr.Revisions) > 0 {
		finalRevision = asr.Revisions[len(asr.Revisions)-1].RevisionID
	}
	if err := store.Append(trajectory.Item{
		ID: "observation-1", Kind: trajectory.KindObservation,
		MonotonicNS: durationNS(config.Clock().Sub(started)), SourceRevision: finalRevision,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: asr.FinalTranscript,
	}); err != nil {
		return report, err
	}

	var engineFast continuation.Provider = fastFinal
	var engineSlow continuation.Provider = slowFinal
	var preparedFastStage *preparation.PreparedStageProvider
	if chainManager != nil {
		committed, preparationReport, preparationErr := chainManager.Commit(makeChainInput(finalRevision, asr.FinalTranscript))
		report.BackgroundPreparation = &preparationReport
		if preparationErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("background preparation: %v", preparationErr))
		}
		if committed != nil {
			preparedFastStage, preparationErr = committed.StageProvider(0, fastFinal)
			if preparationErr == nil {
				engineFast = preparedFastStage
				var preparedSlowStage *preparation.PreparedStageProvider
				preparedSlowStage, preparationErr = committed.StageProvider(1, slowFinal)
				if preparationErr == nil {
					engineSlow = preparedSlowStage
				}
			}
			if preparationErr != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("commit background preparation: %v", preparationErr))
			}
		}
	} else if preparationManager != nil {
		candidate, preparationReport, preparationErr := preparationManager.Finalize(ctx, makePreparationInput(
			fastPrepared.Descriptor(), fastInstruction, config.Tools,
			config.FastMaxOutputTokens, finalRevision, asr.FinalTranscript,
		))
		report.FastPreparation = &preparationReport
		if preparationErr != nil {
			if ctx.Err() != nil {
				return report, preparationErr
			}
			report.Errors = append(report.Errors, fmt.Sprintf("fast preparation: %v", preparationErr))
		}
		if candidate != nil {
			engineFast = candidate.ReplayProvider()
			report.CommittedFastInvocationID = candidate.InvocationID()
		}
	}
	var sequence atomic.Uint64
	nextID := func(prefix string) string {
		return fmt.Sprintf("%s-%d", prefix, sequence.Add(1))
	}
	now := func() uint64 { return durationNS(config.Clock().Sub(started)) }
	engine, err := interleave.New(interleave.Config{
		Store: store, FastProvider: engineFast, SlowProvider: engineSlow, Tools: config.Tools,
		FastInstruction: fastInstruction, SlowInstruction: slowInstruction,
		FastMaxOutputTokens: config.FastMaxOutputTokens, SlowMaxOutputTokens: config.SlowMaxOutputTokens,
		MaxSlowInvocations: config.MaxSlowInvocations,
		RetainReasoning:    config.RetainReasoning || config.PrepareSlowBeforeEndpoint,
		Now:                now, NextID: nextID,
	})
	if err != nil {
		return report, err
	}

	var runErrors []error
	request := interleave.Request{SourceRevision: finalRevision}
	fastResult, fastErr := engine.RunFast(ctx, request, nil)
	if preparedFastStage != nil && preparedFastStage.ReplayedInvocationID() != "" {
		report.CommittedFastInvocationID = preparedFastStage.ReplayedInvocationID()
	} else if report.CommittedFastInvocationID == "" {
		report.CommittedFastInvocationID = fastResult.InvocationID
	}
	if fastErr != nil {
		runErrors = append(runErrors, fmt.Errorf("fast continuation: %w", fastErr))
	}

	var fastSpeech <-chan speechOutcome
	if !fastResult.Interrupted && strings.TrimSpace(fastResult.AssistantText) != "" {
		if err := queueAssistantItems(store, fastResult, now, nextID); err != nil {
			runErrors = append(runErrors, err)
		} else {
			fastSpeech = startSpeech(ctx, config.TTS, trajectory.PhaseFast, 1, fastResult.AssistantText, started, config.Clock)
		}
	}

	slowResult, slowErr := engine.RunSlow(ctx, request, nil)
	if slowErr != nil {
		runErrors = append(runErrors, fmt.Errorf("slow continuation: %w", slowErr))
	}
	report.ToolResults = cloneToolResults(slowResult.ToolResults)

	if fastSpeech != nil {
		outcome := <-fastSpeech
		if outcome.err != nil {
			runErrors = append(runErrors, fmt.Errorf("fast TTS: %w", outcome.err))
		} else {
			report.Speech = append(report.Speech, outcome.segment)
		}
	}

	slowSpeechOrdinal := 0
	for _, result := range slowResult.Runs {
		if result.Interrupted || strings.TrimSpace(result.AssistantText) == "" {
			continue
		}
		if err := queueAssistantItems(store, result, now, nextID); err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		slowSpeechOrdinal++
		outcome := <-startSpeech(ctx, config.TTS, trajectory.PhaseSlow, slowSpeechOrdinal, result.AssistantText, started, config.Clock)
		if outcome.err != nil {
			runErrors = append(runErrors, fmt.Errorf("slow TTS %d: %w", slowSpeechOrdinal, outcome.err))
			continue
		}
		report.Speech = append(report.Speech, outcome.segment)
	}

	report.ModelInvocations = mergeTimings(
		fastPrepared.Timings(), slowPrepared.Timings(),
		fastFinal.Timings(), slowFinal.Timings(),
	)
	if chainManager != nil {
		preparationReport := chainManager.Report()
		report.BackgroundPreparation = &preparationReport
	}
	snapshot := store.Snapshot()
	report.Trajectory = summarizeTrajectory(snapshot)
	for _, item := range snapshot.Items {
		switch item.Kind {
		case trajectory.KindAssistant:
			report.Assistant = append(report.Assistant, AssistantSegment{
				Phase: item.Producer.Phase, Text: item.Content, Interrupted: item.Interrupted,
			})
		case trajectory.KindToolProposal:
			proposal := *item.ToolCall
			proposal.Arguments = slices.Clone(proposal.Arguments)
			report.ToolProposals = append(report.ToolProposals, proposal)
		case trajectory.KindToolCall:
			call := *item.ToolCall
			call.Arguments = slices.Clone(call.Arguments)
			report.ToolCalls = append(report.ToolCalls, call)
		}
	}
	report.ScoreFailures = score(config.Scenario, report)
	for _, runErr := range runErrors {
		if runErr != nil {
			report.Errors = append(report.Errors, runErr.Error())
		}
	}
	report.Passed = len(report.ScoreFailures) == 0 && len(report.Errors) == 0
	report.TotalDurationMS = milliseconds(config.Clock().Sub(started))
	report.EndpointToFastFirstEventMS = endpointToFast(report)
	report.EndpointToFastSafePointMS = endpointToFastSafePoint(report)
	report.EndpointToFirstAudioMS = endpointToFirstAudio(report)
	if config.AdmissionGovernor != nil {
		snapshot := config.AdmissionGovernor.Snapshot()
		report.Admission = &snapshot
	}
	return report, errors.Join(runErrors...)
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.Scenario.ID) == "" {
		return errors.New("realtime benchmark scenario ID is required")
	}
	if config.ASR == nil || config.FastProvider == nil || config.SlowProvider == nil || config.TTS == nil {
		return errors.New("realtime benchmark requires ASR, fast, slow, and TTS providers")
	}
	if config.FastPreparationProvider != nil && config.FastPreparationProvider.Descriptor() != config.FastProvider.Descriptor() {
		return errors.New("fast preparation and final providers must expose the same continuation descriptor")
	}
	if config.SlowPreparationProvider != nil && config.SlowPreparationProvider.Descriptor() != config.SlowProvider.Descriptor() {
		return errors.New("slow preparation and final providers must expose the same continuation descriptor")
	}
	if config.PrepareSlowBeforeEndpoint && !config.PrepareFastBeforeEndpoint {
		return errors.New("slow pre-endpoint preparation requires fast pre-endpoint preparation")
	}
	if config.SlowPreparationMinInterval < 0 {
		return errors.New("slow preparation minimum interval cannot be negative")
	}
	if config.SlowPreparationMinInterval > 0 && !config.PrepareSlowBeforeEndpoint {
		return errors.New("slow preparation minimum interval requires slow pre-endpoint preparation")
	}
	if config.Scenario.Audio.SampleRateHz <= 0 || len(config.Scenario.Audio.PCM16) == 0 {
		return errors.New("realtime benchmark requires non-empty PCM16 input audio")
	}
	if config.ASRFrameDuration <= 0 {
		return errors.New("realtime benchmark ASR frame duration must be positive")
	}
	for _, expected := range config.Scenario.ExpectedSubstrings {
		if normalize(expected) == "" {
			return errors.New("realtime benchmark expected substring cannot be empty")
		}
	}
	for _, required := range config.Scenario.RequiredToolCalls {
		if strings.TrimSpace(required) == "" {
			return errors.New("realtime benchmark required tool name cannot be empty")
		}
	}
	if err := benchspec.ValidateToolCallExpectations(config.Scenario.ExpectedToolCalls); err != nil {
		return fmt.Errorf("realtime benchmark scoring: %w", err)
	}
	return nil
}

type timedProvider struct {
	provider continuation.Provider
	origin   time.Time
	clock    func() time.Time
	mu       sync.Mutex
	timings  []ModelInvocation
}

func newTimedProvider(provider continuation.Provider, origin time.Time, clock func() time.Time) *timedProvider {
	return &timedProvider{provider: provider, origin: origin, clock: clock}
}

func (provider *timedProvider) Descriptor() continuation.Descriptor {
	return provider.provider.Descriptor()
}

func (provider *timedProvider) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	started := provider.clock()
	var first *time.Time
	completion, err := provider.provider.Continue(ctx, request, func(event continuation.Event) error {
		if first == nil {
			value := provider.clock()
			first = &value
		}
		return emit(event)
	})
	ended := provider.clock()
	timing := ModelInvocation{
		InvocationID: request.InvocationID, Phase: request.Descriptor.Phase,
		SourceRevision: request.Invocation.SourceRevision,
		StartOffsetMS:  milliseconds(started.Sub(provider.origin)),
		DurationMS:     milliseconds(ended.Sub(started)), Usage: completion.Usage,
		StopReason: completion.StopReason,
	}
	if first != nil {
		relative := milliseconds(first.Sub(started))
		absolute := milliseconds(first.Sub(provider.origin))
		timing.FirstEventMS = &relative
		timing.FirstEventOffsetMS = &absolute
	}
	if err != nil {
		timing.Error = err.Error()
	}
	provider.mu.Lock()
	timing.Ordinal = len(provider.timings) + 1
	provider.timings = append(provider.timings, timing)
	provider.mu.Unlock()
	return completion, err
}

func (provider *timedProvider) Timings() []ModelInvocation {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]ModelInvocation(nil), provider.timings...)
}

func mergeTimings(groups ...[]ModelInvocation) []ModelInvocation {
	var merged []ModelInvocation
	for _, group := range groups {
		merged = append(merged, group...)
	}
	slices.SortStableFunc(merged, func(left, right ModelInvocation) int {
		if left.StartOffsetMS < right.StartOffsetMS {
			return -1
		}
		if left.StartOffsetMS > right.StartOffsetMS {
			return 1
		}
		return strings.Compare(left.InvocationID, right.InvocationID)
	})
	ordinals := make(map[trajectory.Phase]int)
	for index := range merged {
		ordinals[merged[index].Phase]++
		merged[index].Ordinal = ordinals[merged[index].Phase]
	}
	return merged
}

func makePreparationInput(
	descriptor continuation.Descriptor,
	instruction string,
	toolSet interleave.ToolSet,
	maxOutputTokens int,
	sourceRevision uint64,
	observation string,
) preparation.Input {
	invocation := makeInvocation(instruction, toolSet, maxOutputTokens)
	invocation.SourceRevision = sourceRevision
	return preparation.Input{Request: continuation.Request{
		Descriptor: descriptor, Invocation: invocation,
		Trajectory: trajectory.Snapshot{Version: 2, Items: []trajectory.Item{
			{
				ID: "observation", Kind: trajectory.KindObservation, SourceRevision: sourceRevision,
				Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: observation,
			},
			{
				ID: "instruction", Kind: trajectory.KindInstruction, SourceRevision: sourceRevision,
				CausalParentIDs: []string{"observation"},
				Producer:        trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: instruction,
			},
		}},
	}}
}

func makeInvocation(instruction string, toolSet interleave.ToolSet, maxOutputTokens int) continuation.Invocation {
	invocation := continuation.Invocation{Instruction: instruction, MaxOutputTokens: maxOutputTokens}
	if toolSet == nil {
		return invocation
	}
	invocation.Capabilities = append([]continuation.Capability(nil), toolSet.Capabilities()...)
	invocation.Tools = append([]continuation.ToolDefinition(nil), toolSet.Tools()...)
	for index := range invocation.Tools {
		invocation.Tools[index].Parameters = slices.Clone(invocation.Tools[index].Parameters)
	}
	return invocation
}

func makeChainInput(sourceRevision uint64, observation string) preparation.ChainInput {
	return preparation.ChainInput{
		SourceRevision: sourceRevision,
		Trajectory: trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
			ID: "observation", Kind: trajectory.KindObservation,
			SourceRevision: sourceRevision,
			Producer:       trajectory.Producer{Phase: trajectory.PhaseUser}, Content: observation,
		}}},
	}
}

type speechOutcome struct {
	segment SpeechSegment
	err     error
}

func startSpeech(ctx context.Context, provider v1.StreamingSpeechProvider, phase trajectory.Phase, ordinal int, text string, origin time.Time, clock func() time.Time) <-chan speechOutcome {
	result := make(chan speechOutcome, 1)
	started := clock()
	go func() {
		tts, err := audiobench.RunTTS(ctx, provider, audiobench.TTSConfig{
			CaseID: fmt.Sprintf("%s-%d", phase, ordinal), Text: text,
		})
		segment := SpeechSegment{
			Phase: phase, Ordinal: ordinal, StartOffsetMS: milliseconds(started.Sub(origin)),
			FirstAudioOffsetMS: milliseconds(started.Sub(origin)) + tts.FirstAudioMS,
			Result:             tts,
		}
		result <- speechOutcome{segment: segment, err: err}
		close(result)
	}()
	return result
}

func queueAssistantItems(store *trajectory.Store, result continuation.RunResult, now func() uint64, nextID func(string) string) error {
	ids := make(map[string]struct{}, len(result.AppendedIDs))
	for _, id := range result.AppendedIDs {
		ids[id] = struct{}{}
	}
	for _, item := range store.Snapshot().Items {
		if item.Kind != trajectory.KindAssistant {
			continue
		}
		if _, exists := ids[item.ID]; !exists {
			continue
		}
		snapshot := store.Snapshot()
		state := trajectory.Item{
			ID: nextID("assistant-state"), Kind: trajectory.KindAssistantState,
			MonotonicNS: now(), SourceRevision: item.SourceRevision,
			InvocationID: item.InvocationID, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			AssistantState: &trajectory.AssistantState{AssistantItemID: item.ID, Visibility: trajectory.VisibilityQueued},
		}
		if len(snapshot.Items) > 0 {
			state.CausalParentIDs = []string{snapshot.Items[len(snapshot.Items)-1].ID}
		}
		if err := store.Append(state); err != nil {
			return fmt.Errorf("queue assistant item %s: %w", item.ID, err)
		}
	}
	return nil
}

func summarizeTrajectory(snapshot trajectory.Snapshot) TrajectorySummary {
	type publicItem struct {
		Kind        trajectory.Kind        `json:"kind"`
		Phase       trajectory.Phase       `json:"phase"`
		Content     string                 `json:"content,omitempty"`
		Interrupted bool                   `json:"interrupted,omitempty"`
		ToolCall    *trajectory.ToolCall   `json:"tool_call,omitempty"`
		ToolResult  *trajectory.ToolResult `json:"tool_result,omitempty"`
	}
	projection := make([]publicItem, 0, len(snapshot.Items))
	counts := make(map[string]int)
	for _, item := range snapshot.Items {
		counts[string(item.Kind)]++
		switch item.Kind {
		case trajectory.KindObservation, trajectory.KindAssistant, trajectory.KindToolCall, trajectory.KindToolResult:
			projected := publicItem{Kind: item.Kind, Phase: item.Producer.Phase, Content: item.Content, Interrupted: item.Interrupted}
			if item.ToolCall != nil {
				copy := *item.ToolCall
				copy.Arguments = slices.Clone(copy.Arguments)
				projected.ToolCall = &copy
			}
			if item.ToolResult != nil {
				copy := *item.ToolResult
				copy.Output = slices.Clone(copy.Output)
				projected.ToolResult = &copy
			}
			projection = append(projection, projected)
		}
	}
	encoded, _ := json.Marshal(projection)
	digest := sha256.Sum256(encoded)
	return TrajectorySummary{Items: len(snapshot.Items), PublicSHA256: hex.EncodeToString(digest[:]), KindCounts: counts}
}

func score(scenario Scenario, report Report) []string {
	answer := ""
	for index := len(report.Assistant) - 1; index >= 0; index-- {
		segment := report.Assistant[index]
		if !segment.Interrupted && strings.TrimSpace(segment.Text) != "" {
			answer = normalize(segment.Text)
			break
		}
	}
	var failures []string
	for _, expected := range scenario.ExpectedSubstrings {
		if !strings.Contains(answer, normalize(expected)) {
			failures = append(failures, fmt.Sprintf("final answer does not contain %q", expected))
		}
	}
	failures = append(failures, benchspec.ScoreToolTrajectory(
		scenario.RequiredToolCalls, scenario.ExpectedToolCalls,
		report.ToolCalls, report.ToolResults, scenario.AllowToolErrors,
	)...)
	return failures
}

func scoringSpec(scenario Scenario) ScoringSpec {
	spec := ScoringSpec{
		Version:            benchspec.ToolTrajectoryScorerVersion,
		ExpectedSubstrings: slices.Clone(scenario.ExpectedSubstrings),
		RequiredToolCalls:  slices.Clone(scenario.RequiredToolCalls),
		ExpectedToolCalls:  slices.Clone(scenario.ExpectedToolCalls),
		AllowToolErrors:    scenario.AllowToolErrors,
	}
	for index := range spec.ExpectedToolCalls {
		spec.ExpectedToolCalls[index].Arguments = slices.Clone(spec.ExpectedToolCalls[index].Arguments)
	}
	return spec
}

func endpointToFast(report Report) *float64 {
	if report.FastPreparation != nil && report.FastPreparation.Accepted {
		for _, attempt := range report.FastPreparation.Attempts {
			if attempt.InvocationID == report.CommittedFastInvocationID {
				if attempt.FirstEventOffsetMS == nil {
					return nil
				}
				value := *attempt.FirstEventOffsetMS - report.ASR.InputDurationMS
				if value < 0 {
					value = 0
				}
				return &value
			}
		}
		return nil
	}
	for _, invocation := range report.ModelInvocations {
		if invocation.InvocationID == report.CommittedFastInvocationID && invocation.FirstEventOffsetMS != nil {
			value := *invocation.FirstEventOffsetMS - report.ASR.InputDurationMS
			if value < 0 {
				value = 0
			}
			return &value
		}
	}
	return nil
}

func endpointToFastSafePoint(report Report) *float64 {
	if report.FastPreparation != nil && report.FastPreparation.Accepted {
		for _, attempt := range report.FastPreparation.Attempts {
			if attempt.InvocationID == report.CommittedFastInvocationID {
				value := attempt.EndedOffsetMS - report.ASR.InputDurationMS
				if value < 0 {
					value = 0
				}
				return &value
			}
		}
		return nil
	}
	for _, invocation := range report.ModelInvocations {
		if invocation.InvocationID == report.CommittedFastInvocationID {
			value := invocation.StartOffsetMS + invocation.DurationMS - report.ASR.InputDurationMS
			if value < 0 {
				value = 0
			}
			return &value
		}
	}
	return nil
}

func endpointToFirstAudio(report Report) *float64 {
	if len(report.Speech) == 0 {
		return nil
	}
	first := report.Speech[0].FirstAudioOffsetMS
	for _, segment := range report.Speech[1:] {
		first = min(first, segment.FirstAudioOffsetMS)
	}
	value := first - report.ASR.InputDurationMS
	if value < 0 {
		value = 0
	}
	return &value
}

func normalize(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsPunct(character)
	}), " ")
}

func cloneToolResults(results []trajectory.ToolResult) []trajectory.ToolResult {
	cloned := make([]trajectory.ToolResult, len(results))
	for index, result := range results {
		result.Output = slices.Clone(result.Output)
		cloned[index] = result
	}
	return cloned
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func durationNS(duration time.Duration) uint64 {
	if duration <= 0 {
		return 0
	}
	return uint64(duration)
}
