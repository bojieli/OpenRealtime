package realtimegateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interleave"
	"github.com/bojieli/OpenRealtime/preparation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type sessionSemantics struct {
	Instruction string
	Tools       []continuation.ToolDefinition
}

func (semantics sessionSemantics) clone() sessionSemantics {
	semantics.Tools = slices.Clone(semantics.Tools)
	for index := range semantics.Tools {
		semantics.Tools[index].Parameters = slices.Clone(semantics.Tools[index].Parameters)
	}
	return semantics
}

type staticToolCatalog struct {
	tools []continuation.ToolDefinition
}

func (catalog staticToolCatalog) Tools() []continuation.ToolDefinition {
	result := slices.Clone(catalog.tools)
	for index := range result {
		result[index].Parameters = slices.Clone(result[index].Parameters)
	}
	return result
}

func (catalog staticToolCatalog) Capabilities() []continuation.Capability {
	result := make([]continuation.Capability, 0, len(catalog.tools))
	for _, tool := range catalog.tools {
		result = append(result, continuation.Capability{
			Name: tool.Name, Description: tool.Description, Available: true,
			ExecutionPhase: string(trajectory.PhaseSlow),
		})
	}
	return result
}

type preparedTurn struct {
	manager *preparation.ChainManager
	runtime *cognitionRuntime
}

func (turn *preparedTurn) input(revision uint64, text string) preparation.ChainInput {
	snapshot := turn.runtime.store.Snapshot()
	observation := trajectory.Item{
		ID: fmt.Sprintf("prepared-observation-%d", revision), Kind: trajectory.KindObservation,
		SourceRevision: revision, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content: text,
	}
	if len(snapshot.Items) > 0 {
		observation.CausalParentIDs = []string{snapshot.Items[len(snapshot.Items)-1].ID}
	}
	snapshot.Items = append(snapshot.Items, observation)
	snapshot.Version++
	return preparation.ChainInput{Trajectory: snapshot, SourceRevision: revision}
}

func (turn *preparedTurn) Observe(ctx context.Context, revision uint64, text string) error {
	if !preparation.NonEmptyObservation(text) {
		return nil
	}
	return turn.manager.Observe(ctx, turn.input(revision, text))
}

func (turn *preparedTurn) Commit(revision uint64, text string) (*preparation.CommittedChain, preparation.ChainReport, error) {
	return turn.manager.Commit(turn.input(revision, text))
}

type cognitionCallbacks interface {
	Semantics() sessionSemantics
	PublishAssistant(trajectory.Phase, continuation.RunResult) error
	PublishToolCalls(string, []trajectory.ToolCall, *continuation.Usage) error
}

type cognitionRuntime struct {
	store       *trajectory.Store
	fast        continuation.Provider
	slow        continuation.Provider
	prepareFast continuation.Provider
	prepareSlow continuation.Provider
	callbacks   cognitionCallbacks
	fastTokens  int
	slowTokens  int
	maxSlow     int
	slowPace    time.Duration
	slowPolicy  interleave.SlowContextPolicy
	slowProject continuation.TrajectoryProjection
	now         func() uint64
	nextID      func(string) string
	preparedMu  sync.Mutex
	prepared    map[uint64]*preparation.CommittedChain
}

type cognitionConfig struct {
	Store           *trajectory.Store
	Fast            continuation.Provider
	Slow            continuation.Provider
	PreparationFast continuation.Provider
	PreparationSlow continuation.Provider
	Callbacks       cognitionCallbacks
	FastTokens      int
	SlowTokens      int
	MaxSlow         int
	SlowPace        time.Duration
	SlowPolicy      interleave.SlowContextPolicy
	Now             func() uint64
	NextID          func(string) string
}

func newCognitionRuntime(config cognitionConfig) (*cognitionRuntime, error) {
	if config.Store == nil || config.Fast == nil || config.Slow == nil || config.Callbacks == nil {
		return nil, errors.New("gateway cognition requires store, fast/slow providers, and callbacks")
	}
	if config.PreparationFast == nil {
		config.PreparationFast = config.Fast
	}
	if config.PreparationSlow == nil {
		config.PreparationSlow = config.Slow
	}
	if config.FastTokens <= 0 {
		config.FastTokens = 32
	}
	if config.SlowTokens <= 0 {
		config.SlowTokens = 2_048
	}
	if config.MaxSlow <= 0 {
		config.MaxSlow = 8
	}
	if config.Now == nil {
		origin := time.Now()
		config.Now = func() uint64 { return uint64(time.Since(origin)) }
	}
	if config.NextID == nil {
		var sequence atomic.Uint64
		config.NextID = func(prefix string) string { return fmt.Sprintf("%s-%d", prefix, sequence.Add(1)) }
	}
	if config.SlowPolicy == "" {
		config.SlowPolicy = interleave.SlowContextCanonical
	}
	slowProject, err := interleave.SlowContextProjection(config.SlowPolicy)
	if err != nil {
		return nil, err
	}
	return &cognitionRuntime{
		store: config.Store, fast: config.Fast, slow: config.Slow,
		prepareFast: config.PreparationFast, prepareSlow: config.PreparationSlow,
		callbacks:  config.Callbacks,
		fastTokens: config.FastTokens, slowTokens: config.SlowTokens, maxSlow: config.MaxSlow,
		slowPace: config.SlowPace, now: config.Now, nextID: config.NextID,
		slowPolicy: config.SlowPolicy, slowProject: slowProject,
		prepared: make(map[uint64]*preparation.CommittedChain),
	}, nil
}

func (runtime *cognitionRuntime) NewPreparation() (*preparedTurn, error) {
	semantics := runtime.callbacks.Semantics().clone()
	catalog := staticToolCatalog{tools: semantics.Tools}
	capabilities := catalog.Capabilities()
	tools := catalog.Tools()
	fastInstruction := composePhaseInstruction(semantics.Instruction, interleave.DefaultFastInstruction)
	slowInstruction := composePhaseInstruction(semantics.Instruction, interleave.DefaultSlowInstruction)
	manager, err := preparation.NewChainManager(preparation.ChainConfig{
		Stages: []preparation.ChainStage{
			{Provider: runtime.prepareFast, Invocation: continuation.Invocation{
				Instruction: fastInstruction, Capabilities: capabilities, Tools: tools,
				MaxOutputTokens: runtime.fastTokens,
			}},
			{Provider: runtime.prepareSlow, Invocation: continuation.Invocation{
				Instruction: slowInstruction, Capabilities: capabilities, Tools: tools,
				MaxOutputTokens: runtime.slowTokens,
			}, Projection: runtime.slowProject, MinimumStartInterval: runtime.slowPace},
		},
		RetainReasoning: true,
	})
	if err != nil {
		return nil, err
	}
	return &preparedTurn{manager: manager, runtime: runtime}, nil
}

func composePhaseInstruction(agent, phase string) string {
	if strings.TrimSpace(agent) == "" {
		return phase
	}
	return agent + "\n\n" + phase
}

func (runtime *cognitionRuntime) AttachPreparation(revision uint64, chain *preparation.CommittedChain) {
	if chain == nil {
		return
	}
	runtime.preparedMu.Lock()
	displaced := runtime.prepared[revision]
	runtime.prepared[revision] = chain
	runtime.preparedMu.Unlock()
	if displaced != nil && displaced != chain {
		displaced.Cancel(preparation.ErrSuperseded)
	}
}

func (runtime *cognitionRuntime) Process(ctx context.Context, batch eventloop.Batch) error {
	var observation bool
	var toolResult bool
	var sourceRevision uint64
	for _, item := range batch.Items {
		sourceRevision = max(sourceRevision, item.SourceRevision)
		switch item.Kind {
		case trajectory.KindObservation:
			observation = true
		case trajectory.KindToolResult:
			toolResult = true
		}
	}
	if !observation && !toolResult {
		return nil
	}
	processor, err := runtime.newProcessor(sourceRevision)
	if err != nil {
		return err
	}
	return processor.Process(ctx, batch)
}

func (runtime *cognitionRuntime) newProcessor(sourceRevision uint64) (*interleave.Processor, error) {
	semantics := runtime.callbacks.Semantics().clone()
	fast, slow := runtime.fast, runtime.slow
	chain := runtime.takePreparation(sourceRevision)
	if chain != nil {
		var err error
		fast, err = chain.StageProvider(0, fast)
		if err != nil {
			return nil, err
		}
		slow, err = chain.StageProvider(1, slow)
		if err != nil {
			return nil, err
		}
	}
	catalog := staticToolCatalog{tools: semantics.Tools}
	engine, err := interleave.New(interleave.Config{
		Store: runtime.store, FastProvider: fast, SlowProvider: slow,
		ToolCatalog: catalog, AgentInstruction: semantics.Instruction,
		FastMaxOutputTokens: runtime.fastTokens, SlowMaxOutputTokens: runtime.slowTokens,
		MaxSlowInvocations: runtime.maxSlow, RetainReasoning: true,
		SlowContextPolicy: runtime.slowPolicy,
		Now:               runtime.now, NextID: runtime.nextID,
	})
	if err != nil {
		return nil, err
	}
	var lastSlow continuation.RunResult
	processor, err := interleave.NewProcessor(interleave.ProcessorConfig{
		Engine: engine,
		RunObserver: func(phase trajectory.Phase, result continuation.RunResult) error {
			if !result.Committed || result.Interrupted {
				return nil
			}
			if phase == trajectory.PhaseSlow {
				lastSlow = result
			}
			return runtime.callbacks.PublishAssistant(phase, result)
		},
		ToolCallSink: func(_ context.Context, invocation string, calls []trajectory.ToolCall) error {
			if lastSlow.InvocationID != invocation {
				return fmt.Errorf("tool-call safe point %q does not match the committed slow invocation %q", invocation, lastSlow.InvocationID)
			}
			var usage *continuation.Usage
			if strings.TrimSpace(lastSlow.AssistantText) == "" {
				usage = &lastSlow.Completion.Usage
			}
			return runtime.callbacks.PublishToolCalls(invocation, calls, usage)
		},
	})
	if err != nil {
		return nil, err
	}
	return processor, nil
}

// takePreparation consumes the exact final revision and cancels only older
// prepared roots. A newer root may already have arrived concurrently and must
// remain available for its own committed observation batch.
func (runtime *cognitionRuntime) takePreparation(sourceRevision uint64) *preparation.CommittedChain {
	runtime.preparedMu.Lock()
	chain := runtime.prepared[sourceRevision]
	var stale []*preparation.CommittedChain
	for revision, candidate := range runtime.prepared {
		if revision > sourceRevision {
			continue
		}
		delete(runtime.prepared, revision)
		if revision != sourceRevision && candidate != nil {
			stale = append(stale, candidate)
		}
	}
	runtime.preparedMu.Unlock()
	for _, candidate := range stale {
		candidate.Cancel(preparation.ErrSuperseded)
	}
	return chain
}

func resultAssistantIDs(store *trajectory.Store, result continuation.RunResult) []string {
	ids := make(map[string]struct{}, len(result.AppendedIDs))
	for _, id := range result.AppendedIDs {
		ids[id] = struct{}{}
	}
	var output []string
	for _, item := range store.Snapshot().Items {
		if item.Kind != trajectory.KindAssistant {
			continue
		}
		if _, exists := ids[item.ID]; exists {
			output = append(output, item.ID)
		}
	}
	return output
}

func encodeToolResult(callID, name, output string) trajectory.ToolResult {
	raw := json.RawMessage(output)
	if !json.Valid(raw) {
		raw, _ = json.Marshal(output)
	}
	return trajectory.ToolResult{CallID: callID, Name: name, Output: raw}
}

var _ eventloop.Processor = (*cognitionRuntime)(nil)
