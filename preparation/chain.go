package preparation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// ChainStage is one ordinary continuation in a speculative preparation chain.
// Stages execute sequentially over a private append-only trajectory. A stage
// with executable tool authority may only be last: preparation can capture its
// call, but deliberately has no tool runtime and cannot create a side effect.
// MinimumStartInterval is an operational launch budget; it never participates
// in semantic fingerprints, and exact commit bypasses its remaining wait.
type ChainStage struct {
	Provider             continuation.Provider
	Invocation           continuation.Invocation
	Projection           continuation.TrajectoryProjection
	MinimumStartInterval time.Duration
}

// ChainInput is the immutable trajectory prefix available at one perception
// revision. SourceRevision is operational provenance and is excluded from the
// semantic acceptance fingerprint.
type ChainInput struct {
	Trajectory     trajectory.Snapshot
	SourceRevision uint64
}

// ChainConfig defines a fixed continuation pipeline. The initial OpenRealtime
// use is fast proposal-only Qwen followed by slow execute-authority Gemini, but
// the manager has no provider- or model-specific branching.
type ChainConfig struct {
	Stages          []ChainStage
	RetainReasoning bool
	Clock           func() time.Time
	Origin          time.Time
}

// ChainStageAttempt contains timing and disposition only. Model text,
// reasoning, tool arguments, and native provider state are never copied into
// preparation telemetry.
type ChainStageAttempt struct {
	Index              int              `json:"index"`
	Phase              trajectory.Phase `json:"phase"`
	InvocationID       string           `json:"invocation_id,omitempty"`
	ReadyOffsetMS      float64          `json:"ready_offset_ms"`
	EligibleOffsetMS   float64          `json:"eligible_offset_ms"`
	StartedOffsetMS    *float64         `json:"started_offset_ms,omitempty"`
	FirstEventOffsetMS *float64         `json:"first_event_offset_ms,omitempty"`
	EndedOffsetMS      float64          `json:"ended_offset_ms"`
	PacingWaitMS       float64          `json:"pacing_wait_ms,omitempty"`
	CommitBypassedWait bool             `json:"commit_bypassed_wait,omitempty"`
	Outcome            string           `json:"outcome"`
}

// ChainAttempt is one latest-revision pipeline attempt.
type ChainAttempt struct {
	AttemptID         string              `json:"attempt_id"`
	Fingerprint       string              `json:"fingerprint"`
	SourceRevision    uint64              `json:"source_revision,omitempty"`
	TriggeredOffsetMS float64             `json:"triggered_offset_ms"`
	EndedOffsetMS     float64             `json:"ended_offset_ms"`
	Outcome           string              `json:"outcome"`
	Stages            []ChainStageAttempt `json:"stages,omitempty"`
}

// ChainReport summarizes a latest-wins preparation pipeline. Committed means
// its root semantic input exactly matched the final input. ReplayedStages are
// the stage outputs that also matched their exact canonical stage input and
// were consumed after commit; FallbackStages ran through their live provider.
type ChainReport struct {
	Enabled           bool           `json:"enabled"`
	Observed          uint64         `json:"observed"`
	DistinctInputs    uint64         `json:"distinct_inputs"`
	Started           uint64         `json:"started"`
	Completed         uint64         `json:"completed"`
	Superseded        uint64         `json:"superseded"`
	Failed            uint64         `json:"failed"`
	Coalesced         uint64         `json:"coalesced"`
	Committed         bool           `json:"committed"`
	CommittedAttempt  string         `json:"committed_attempt,omitempty"`
	Fingerprint       string         `json:"committed_fingerprint,omitempty"`
	ReplayedStages    uint64         `json:"replayed_stages"`
	FallbackStages    uint64         `json:"fallback_stages"`
	PacedStageWaits   uint64         `json:"paced_stage_waits"`
	PacedStageCancels uint64         `json:"paced_stage_cancellations"`
	CommitBypasses    uint64         `json:"commit_pacing_bypasses"`
	Attempts          []ChainAttempt `json:"attempts,omitempty"`
}

type preparedStage struct {
	done      chan struct{}
	mu        sync.Mutex
	candidate *Candidate
	err       error
	closed    bool
}

func newPreparedStage() *preparedStage { return &preparedStage{done: make(chan struct{})} }

func (stage *preparedStage) finish(candidate *Candidate, err error) {
	stage.mu.Lock()
	defer stage.mu.Unlock()
	if stage.closed {
		return
	}
	stage.candidate, stage.err, stage.closed = candidate, err, true
	close(stage.done)
}

func (stage *preparedStage) wait(ctx context.Context) (*Candidate, error) {
	select {
	case <-stage.done:
		stage.mu.Lock()
		defer stage.mu.Unlock()
		return cloneCandidate(stage.candidate), stage.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type chainState struct {
	input        ChainInput
	fingerprint  string
	attemptID    string
	triggered    time.Time
	cancel       context.CancelCauseFunc
	stages       []*preparedStage
	done         chan struct{}
	expedite     chan struct{}
	expediteOnce sync.Once
}

func (state *chainState) expeditePacing() {
	state.expediteOnce.Do(func() { close(state.expedite) })
}

// ChainManager owns at most one speculative fast-to-slow pipeline. New
// observations cancel the active pipeline and coalesce while cancellation is
// reaching a provider safe point. Commit never waits for the pipeline: the
// returned stage providers wait only when the canonical engine reaches them.
type ChainManager struct {
	mu sync.Mutex

	stages          []ChainStage
	retainReasoning bool
	clock           func() time.Time
	origin          time.Time
	nextAttempt     uint64
	latest          *ChainInput
	latestParent    context.Context
	latestTriggered time.Time
	latestHash      string
	active          *chainState
	ready           *chainState
	lastStageStart  []time.Time
	closed          bool
	report          ChainReport
}

// NewChainManager validates and creates a latest-revision pipeline manager.
func NewChainManager(config ChainConfig) (*ChainManager, error) {
	if len(config.Stages) == 0 {
		return nil, errors.New("preparation chain requires at least one stage")
	}
	stages := make([]ChainStage, len(config.Stages))
	for index, stage := range config.Stages {
		if stage.Provider == nil {
			return nil, fmt.Errorf("preparation chain stage %d requires a provider", index)
		}
		descriptor := stage.Provider.Descriptor()
		if err := continuation.ValidateDescriptor(descriptor); err != nil {
			return nil, fmt.Errorf("preparation chain stage %d descriptor: %w", index, err)
		}
		if err := continuation.ValidateInvocation(stage.Invocation, descriptor); err != nil {
			return nil, fmt.Errorf("preparation chain stage %d invocation: %w", index, err)
		}
		if stage.MinimumStartInterval < 0 {
			return nil, fmt.Errorf("preparation chain stage %d minimum start interval cannot be negative", index)
		}
		if index != len(config.Stages)-1 && descriptor.EffectiveToolAuthority() == continuation.ToolAuthorityExecute {
			return nil, fmt.Errorf("preparation chain stage %d has executable tool authority before the final stage", index)
		}
		stages[index] = ChainStage{
			Provider: stage.Provider, Invocation: cloneInvocation(stage.Invocation),
			Projection:           stage.Projection,
			MinimumStartInterval: stage.MinimumStartInterval,
		}
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Origin.IsZero() {
		config.Origin = config.Clock()
	}
	return &ChainManager{
		stages: stages, retainReasoning: config.RetainReasoning,
		clock: config.Clock, origin: config.Origin,
		lastStageStart: make([]time.Time, len(stages)), report: ChainReport{Enabled: true},
	}, nil
}

// Observe schedules one complete private continuation chain for the latest
// semantic input. It returns after scheduling and never waits for inference.
func (manager *ChainManager) Observe(ctx context.Context, input ChainInput) error {
	if ctx == nil {
		return errors.New("preparation chain context must not be nil")
	}
	if len(input.Trajectory.Items) == 0 {
		return errors.New("preparation chain requires a non-empty trajectory")
	}
	fingerprint, err := manager.fingerprint(input)
	if err != nil {
		return err
	}
	input = cloneChainInput(input)
	now := manager.clock()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return ErrClosed
	}
	manager.report.Observed++
	if fingerprint == manager.latestHash {
		if manager.active == nil && manager.ready == nil {
			manager.latest, manager.latestParent, manager.latestTriggered = &input, ctx, now
			manager.startLocked(ctx, input, fingerprint, now)
		} else {
			manager.report.Coalesced++
		}
		return nil
	}
	manager.report.DistinctInputs++
	manager.latest, manager.latestHash = &input, fingerprint
	manager.latestParent, manager.latestTriggered = ctx, now
	manager.ready = nil
	if manager.active != nil {
		manager.active.cancel(ErrSuperseded)
		return nil
	}
	manager.startLocked(ctx, input, fingerprint, now)
	return nil
}

// Commit closes observation intake and returns a handle only when the complete
// final semantic input exactly matches the latest prepared root. An in-flight
// exact pipeline continues; stale work is cancelled. No model output is made
// canonical and no tool can execute during this operation.
func (manager *ChainManager) Commit(input ChainInput) (*CommittedChain, ChainReport, error) {
	fingerprint, err := manager.fingerprint(input)
	if err != nil {
		return nil, manager.Report(), err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.closed = true
	state := manager.ready
	if state == nil {
		state = manager.active
	}
	if state == nil || state.fingerprint != fingerprint {
		if manager.active != nil {
			manager.active.cancel(ErrSuperseded)
		}
		return nil, manager.cloneReportLocked(), nil
	}
	manager.report.Committed = true
	manager.report.CommittedAttempt = state.attemptID
	manager.report.Fingerprint = fingerprint
	state.expeditePacing()
	return &CommittedChain{manager: manager, state: state}, manager.cloneReportLocked(), nil
}

// Close cancels an unfinished chain and waits for its provider safe point.
func (manager *ChainManager) Close(ctx context.Context, cause error) (ChainReport, error) {
	if ctx == nil {
		return manager.Report(), errors.New("preparation chain close context must not be nil")
	}
	if cause == nil {
		cause = context.Canceled
	}
	manager.mu.Lock()
	manager.closed = true
	active := manager.active
	if active != nil {
		active.cancel(cause)
	}
	manager.mu.Unlock()
	if active == nil {
		return manager.Report(), nil
	}
	select {
	case <-active.done:
		return manager.Report(), nil
	case <-ctx.Done():
		return manager.Report(), ctx.Err()
	}
}

// Report returns a deep copy of current preparation telemetry.
func (manager *ChainManager) Report() ChainReport {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.cloneReportLocked()
}

func (manager *ChainManager) cloneReportLocked() ChainReport {
	report := manager.report
	report.Attempts = slices.Clone(manager.report.Attempts)
	for index := range report.Attempts {
		report.Attempts[index].Stages = slices.Clone(report.Attempts[index].Stages)
	}
	return report
}

func (manager *ChainManager) fingerprint(input ChainInput) (string, error) {
	parts := make([]string, len(manager.stages))
	for index, stage := range manager.stages {
		invocation := cloneInvocation(stage.Invocation)
		invocation.SourceRevision = input.SourceRevision
		projected := cloneSnapshot(input.Trajectory)
		if stage.Projection != nil {
			var projectionErr error
			projected, projectionErr = stage.Projection(projected)
			if projectionErr != nil {
				return "", fmt.Errorf("project preparation chain stage %d: %w", index, projectionErr)
			}
		}
		value, err := Fingerprint(Input{Request: continuation.Request{
			Descriptor: stage.Provider.Descriptor(), Invocation: invocation,
			Trajectory: projected,
		}})
		if err != nil {
			return "", fmt.Errorf("fingerprint preparation chain stage %d: %w", index, err)
		}
		parts[index] = value
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return "", fmt.Errorf("encode preparation chain fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (manager *ChainManager) startLocked(parent context.Context, input ChainInput, fingerprint string, triggered time.Time) {
	manager.nextAttempt++
	attemptID := fmt.Sprintf("prepared-chain-%d", manager.nextAttempt)
	ctx, cancel := context.WithCancelCause(parent)
	state := &chainState{
		input: input, fingerprint: fingerprint, attemptID: attemptID,
		triggered: triggered, cancel: cancel, done: make(chan struct{}),
		expedite: make(chan struct{}),
		stages:   make([]*preparedStage, len(manager.stages)),
	}
	for index := range state.stages {
		state.stages[index] = newPreparedStage()
	}
	manager.active = state
	manager.report.Started++
	go manager.run(ctx, state)
}

func (manager *ChainManager) run(ctx context.Context, state *chainState) {
	store := trajectory.NewStore()
	base := cloneSnapshot(state.input.Trajectory)
	if err := store.AppendBatch(base.Items); err != nil {
		manager.finish(state, nil, err)
		return
	}
	var idMu sync.Mutex
	nextID := uint64(0)
	lastNS := uint64(0)
	usedIDs := make(map[string]struct{}, len(base.Items))
	for _, item := range base.Items {
		usedIDs[item.ID] = struct{}{}
		lastNS = max(lastNS, item.MonotonicNS)
	}
	runner, err := continuation.NewRunner(continuation.RunnerConfig{
		Store: store, RetainReasoning: manager.retainReasoning,
		Now: func() uint64 {
			idMu.Lock()
			defer idMu.Unlock()
			elapsed := manager.clock().Sub(manager.origin)
			value := uint64(0)
			if elapsed > 0 {
				value = uint64(elapsed)
			}
			lastNS = max(lastNS, value)
			return lastNS
		},
		NextID: func(prefix string) string {
			idMu.Lock()
			defer idMu.Unlock()
			for {
				nextID++
				candidate := fmt.Sprintf("%s-%s-%d", state.attemptID, prefix, nextID)
				if _, exists := usedIDs[candidate]; exists {
					continue
				}
				usedIDs[candidate] = struct{}{}
				return candidate
			}
		},
	})
	if err != nil {
		manager.finish(state, nil, err)
		return
	}

	var stageRecords []ChainStageAttempt
	for index, configured := range manager.stages {
		admitted, waitErr := manager.awaitStage(ctx, state, index)
		if waitErr != nil {
			ended := manager.clock()
			outcome := "failed_before_start"
			if errors.Is(waitErr, ErrSuperseded) {
				outcome = "superseded_before_start"
			}
			stageRecords = append(stageRecords, ChainStageAttempt{
				Index: index, Phase: configured.Provider.Descriptor().Phase,
				ReadyOffsetMS:    milliseconds(admitted.ready.Sub(manager.origin)),
				EligibleOffsetMS: milliseconds(admitted.eligible.Sub(manager.origin)),
				EndedOffsetMS:    milliseconds(ended.Sub(manager.origin)),
				PacingWaitMS:     milliseconds(ended.Sub(admitted.ready)), Outcome: outcome,
			})
			state.stages[index].finish(nil, waitErr)
			manager.finish(state, stageRecords, waitErr)
			return
		}
		started := admitted.started
		var first *time.Time
		capture := &captureProvider{
			provider: configured.Provider,
			onFirst: func() {
				if first == nil {
					value := manager.clock()
					first = &value
				}
			},
		}
		invocation := cloneInvocation(configured.Invocation)
		invocation.SourceRevision = state.input.SourceRevision
		var result continuation.RunResult
		var runErr error
		if configured.Projection == nil {
			result, runErr = runner.Run(ctx, capture, invocation, nil)
		} else {
			result, runErr = runner.RunProjected(ctx, capture, invocation, configured.Projection, nil)
		}
		ended := manager.clock()
		outcome := "completed"
		if runErr != nil || result.Interrupted {
			outcome = "failed"
			if errors.Is(context.Cause(ctx), ErrSuperseded) {
				outcome = "superseded"
			}
		}
		record := ChainStageAttempt{
			Index: index, Phase: configured.Provider.Descriptor().Phase,
			InvocationID:     result.InvocationID,
			ReadyOffsetMS:    milliseconds(admitted.ready.Sub(manager.origin)),
			EligibleOffsetMS: milliseconds(admitted.eligible.Sub(manager.origin)),
			EndedOffsetMS:    milliseconds(ended.Sub(manager.origin)), Outcome: outcome,
			PacingWaitMS:       milliseconds(started.Sub(admitted.ready)),
			CommitBypassedWait: admitted.commitBypass,
		}
		startedOffset := milliseconds(started.Sub(manager.origin))
		record.StartedOffsetMS = &startedOffset
		if first != nil {
			value := milliseconds(first.Sub(manager.origin))
			record.FirstEventOffsetMS = &value
		}
		stageRecords = append(stageRecords, record)
		if runErr != nil || result.Interrupted {
			if runErr == nil {
				runErr = context.Cause(ctx)
			}
			runErr = errors.Join(runErr, context.Cause(ctx))
			state.stages[index].finish(nil, runErr)
			manager.finish(state, stageRecords, runErr)
			return
		}
		candidate, candidateErr := capture.candidate(result.InvocationID)
		if candidateErr != nil {
			state.stages[index].finish(nil, candidateErr)
			manager.finish(state, stageRecords, candidateErr)
			return
		}
		state.stages[index].finish(candidate, nil)
	}
	manager.finish(state, stageRecords, context.Cause(ctx))
}

type stageAdmission struct {
	ready        time.Time
	eligible     time.Time
	started      time.Time
	commitBypass bool
}

// awaitStage enforces only a temporal launch budget. It never inspects model
// content. Exact final commit may bypass the remaining wait so this operational
// cost policy cannot delay the canonical response.
func (manager *ChainManager) awaitStage(ctx context.Context, state *chainState, index int) (stageAdmission, error) {
	admission := stageAdmission{ready: manager.clock()}
	admission.eligible = admission.ready

	manager.mu.Lock()
	interval := manager.stages[index].MinimumStartInterval
	lastStart := manager.lastStageStart[index]
	if interval > 0 && !lastStart.IsZero() {
		admission.eligible = maxTime(admission.ready, lastStart.Add(interval))
	}
	delay := admission.eligible.Sub(admission.ready)
	if delay > 0 {
		manager.report.PacedStageWaits++
	}
	manager.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			manager.mu.Lock()
			manager.report.PacedStageCancels++
			manager.mu.Unlock()
			return admission, contextCause(ctx)
		case <-state.expedite:
			if err := ctx.Err(); err != nil {
				manager.mu.Lock()
				manager.report.PacedStageCancels++
				manager.mu.Unlock()
				return admission, contextCause(ctx)
			}
			admission.commitBypass = true
			manager.mu.Lock()
			manager.report.CommitBypasses++
			manager.mu.Unlock()
		case <-timer.C:
		}
	}

	if err := ctx.Err(); err != nil {
		return admission, contextCause(ctx)
	}
	admission.started = manager.clock()
	manager.mu.Lock()
	manager.lastStageStart[index] = admission.started
	manager.mu.Unlock()
	return admission, nil
}

func contextCause(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

func maxTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func (manager *ChainManager) finish(state *chainState, stages []ChainStageAttempt, runErr error) {
	for _, stage := range state.stages {
		stage.finish(nil, runErr)
	}
	ended := manager.clock()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	outcome := "completed"
	if errors.Is(runErr, ErrSuperseded) {
		outcome = "superseded"
		manager.report.Superseded++
	} else if runErr != nil {
		outcome = "failed"
		manager.report.Failed++
	} else {
		manager.report.Completed++
		if manager.latestHash == state.fingerprint {
			manager.ready = state
		}
	}
	manager.report.Attempts = append(manager.report.Attempts, ChainAttempt{
		AttemptID: state.attemptID, Fingerprint: state.fingerprint,
		SourceRevision:    state.input.SourceRevision,
		TriggeredOffsetMS: milliseconds(state.triggered.Sub(manager.origin)),
		EndedOffsetMS:     milliseconds(ended.Sub(manager.origin)), Outcome: outcome,
		Stages: slices.Clone(stages),
	})
	if manager.active == state {
		manager.active = nil
	}
	close(state.done)
	if !manager.closed && manager.latest != nil && manager.latestHash != state.fingerprint && manager.latestParent.Err() == nil {
		manager.startLocked(manager.latestParent, *manager.latest, manager.latestHash, manager.latestTriggered)
	}
}

// CommittedChain is an exact-root-match handle. It exposes one provider per
// stage; no stage output is appended until the canonical continuation runner
// invokes that provider after endpoint commit.
type CommittedChain struct {
	manager *ChainManager
	state   *chainState
}

// StageProvider returns a provider that waits for the selected prepared stage,
// replays it only for an exact stage-input match, and otherwise calls fallback.
func (chain *CommittedChain) StageProvider(index int, fallback continuation.Provider) (*PreparedStageProvider, error) {
	if chain == nil || chain.state == nil || chain.manager == nil {
		return nil, errors.New("committed preparation chain is nil")
	}
	if index < 0 || index >= len(chain.state.stages) {
		return nil, fmt.Errorf("preparation chain stage %d is out of range", index)
	}
	if fallback == nil {
		return nil, errors.New("prepared stage fallback provider is required")
	}
	expected := chain.manager.stages[index].Provider.Descriptor()
	if fallback.Descriptor() != expected {
		return nil, errors.New("prepared stage fallback descriptor mismatch")
	}
	return &PreparedStageProvider{
		descriptor: expected, stage: chain.state.stages[index],
		fallback: fallback, manager: chain.manager,
	}, nil
}

// PreparedStageProvider is a one-prepared-result prefix in front of a live
// provider. Subsequent calls always use the live provider, which lets a
// prepared slow tool call continue normally after its authoritative result.
type PreparedStageProvider struct {
	mu sync.Mutex

	descriptor continuation.Descriptor
	stage      *preparedStage
	fallback   continuation.Provider
	manager    *ChainManager
	firstUsed  bool
	replayedID string
}

func (provider *PreparedStageProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (provider *PreparedStageProvider) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	provider.mu.Lock()
	first := !provider.firstUsed
	provider.firstUsed = true
	provider.mu.Unlock()
	if !first {
		return provider.fallback.Continue(ctx, request, emit)
	}
	candidate, err := provider.stage.wait(ctx)
	if err != nil || candidate == nil {
		if ctx.Err() != nil {
			return continuation.Completion{}, ctx.Err()
		}
		provider.recordFallback()
		return provider.fallback.Continue(ctx, request, emit)
	}
	fingerprint, err := Fingerprint(Input{Request: request})
	if err != nil || request.Descriptor != candidate.descriptor || fingerprint != candidate.fingerprint {
		provider.recordFallback()
		return provider.fallback.Continue(ctx, request, emit)
	}
	completion, err := candidate.ReplayProvider().Continue(ctx, request, emit)
	if err != nil {
		return completion, err
	}
	provider.mu.Lock()
	provider.replayedID = candidate.invocation
	provider.mu.Unlock()
	provider.manager.mu.Lock()
	provider.manager.report.ReplayedStages++
	provider.manager.mu.Unlock()
	return completion, nil
}

// ReplayedInvocationID is non-empty only after exact prepared replay succeeds.
func (provider *PreparedStageProvider) ReplayedInvocationID() string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.replayedID
}

func (provider *PreparedStageProvider) recordFallback() {
	provider.manager.mu.Lock()
	provider.manager.report.FallbackStages++
	provider.manager.mu.Unlock()
}

type captureProvider struct {
	provider continuation.Provider
	onFirst  func()
	request  continuation.Request
	events   []continuation.Event
	result   continuation.Completion
}

func (provider *captureProvider) Descriptor() continuation.Descriptor {
	return provider.provider.Descriptor()
}

func (provider *captureProvider) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	provider.request = cloneInput(Input{Request: request}).Request
	completion, err := provider.provider.Continue(ctx, request, func(event continuation.Event) error {
		if provider.onFirst != nil && len(provider.events) == 0 {
			provider.onFirst()
		}
		provider.events = append(provider.events, cloneEvent(event))
		return emit(event)
	})
	provider.result = cloneCompletion(completion)
	return completion, err
}

func (provider *captureProvider) candidate(invocation string) (*Candidate, error) {
	fingerprint, err := Fingerprint(Input{Request: provider.request})
	if err != nil {
		return nil, err
	}
	return &Candidate{
		descriptor: provider.Descriptor(), fingerprint: fingerprint,
		invocation: invocation, events: cloneEvents(provider.events),
		completion: cloneCompletion(provider.result),
	}, nil
}

func cloneCandidate(candidate *Candidate) *Candidate {
	if candidate == nil {
		return nil
	}
	return &Candidate{
		descriptor: candidate.descriptor, fingerprint: candidate.fingerprint,
		invocation: candidate.invocation, events: cloneEvents(candidate.events),
		completion: cloneCompletion(candidate.completion),
	}
}

func cloneChainInput(input ChainInput) ChainInput {
	input.Trajectory = cloneSnapshot(input.Trajectory)
	return input
}
