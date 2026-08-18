// Package preparation runs latest-revision continuation work before an input
// endpoint and exposes only exact-input candidates for canonical commit.
//
// The package is deliberately content-agnostic. It compares SHA-256
// fingerprints of the complete provider-visible semantic input; it performs no
// keyword routing, difficulty classification, or transcript normalization.
package preparation

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
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

var (
	ErrSuperseded = errors.New("prepared continuation superseded by a newer input revision")
	ErrClosed     = errors.New("continuation preparation is closed")
)

// Input is one immutable continuation request candidate. Operational IDs,
// timestamps, causal IDs, and source revision numbers are excluded from the
// semantic fingerprint because provider adapters do not expose them to the
// model. All model-visible content, tools, policy, and opaque state are kept.
type Input struct {
	Request continuation.Request
}

// Fingerprint returns the exact acceptance key for provider-visible semantic
// input. A change to any model-visible item, tool schema, capability, policy,
// model profile, or retained provider state changes the key.
func Fingerprint(input Input) (string, error) {
	if err := continuation.ValidateDescriptor(input.Request.Descriptor); err != nil {
		return "", err
	}
	if err := continuation.ValidateInvocation(input.Request.Invocation, input.Request.Descriptor); err != nil {
		return "", err
	}
	type semanticItem struct {
		Kind              trajectory.Kind        `json:"kind"`
		Content           string                 `json:"content,omitempty"`
		ToolCall          *trajectory.ToolCall   `json:"tool_call,omitempty"`
		ToolResult        *trajectory.ToolResult `json:"tool_result,omitempty"`
		ProviderStateType string                 `json:"provider_state_type,omitempty"`
		ProviderState     json.RawMessage        `json:"provider_state,omitempty"`
	}
	type semanticInput struct {
		Descriptor continuation.Descriptor `json:"descriptor"`
		Invocation continuation.Invocation `json:"invocation"`
		Items      []semanticItem          `json:"items"`
	}
	projection := semanticInput{
		Descriptor: input.Request.Descriptor,
		Invocation: cloneInvocation(input.Request.Invocation),
	}
	projection.Invocation.SourceRevision = 0
	for _, item := range input.Request.Trajectory.Items {
		if item.Kind == trajectory.KindAssistantState {
			// Provider adapters skip media visibility transitions.
			continue
		}
		projected := semanticItem{
			Kind: item.Kind, Content: item.Content, ProviderStateType: item.ProviderStateType,
			ProviderState: slices.Clone(item.ProviderState),
		}
		if item.ToolCall != nil {
			call := *item.ToolCall
			call.Arguments = slices.Clone(call.Arguments)
			projected.ToolCall = &call
		}
		if item.ToolResult != nil {
			result := *item.ToolResult
			result.Output = slices.Clone(result.Output)
			projected.ToolResult = &result
		}
		projection.Items = append(projection.Items, projected)
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", fmt.Errorf("encode preparation fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// Attempt is safe timing and disposition telemetry. It never contains model
// input, output, or reasoning text.
type Attempt struct {
	InvocationID       string   `json:"invocation_id"`
	Fingerprint        string   `json:"fingerprint"`
	SourceRevision     uint64   `json:"source_revision,omitempty"`
	TriggeredOffsetMS  float64  `json:"triggered_offset_ms"`
	StartedOffsetMS    float64  `json:"started_offset_ms"`
	FirstEventOffsetMS *float64 `json:"first_event_offset_ms,omitempty"`
	EndedOffsetMS      float64  `json:"ended_offset_ms"`
	Outcome            string   `json:"outcome"`
}

// Report summarizes latest-wins preparation without exposing candidate text.
type Report struct {
	Enabled              bool      `json:"enabled"`
	Observed             uint64    `json:"observed"`
	DistinctInputs       uint64    `json:"distinct_inputs"`
	Started              uint64    `json:"started"`
	Completed            uint64    `json:"completed"`
	Superseded           uint64    `json:"superseded"`
	Failed               uint64    `json:"failed"`
	Coalesced            uint64    `json:"coalesced"`
	Accepted             bool      `json:"accepted"`
	AcceptedInvocationID string    `json:"accepted_invocation_id,omitempty"`
	AcceptedFingerprint  string    `json:"accepted_fingerprint,omitempty"`
	Attempts             []Attempt `json:"attempts,omitempty"`
}

// Config supplies one fast proposal-capable provider and a stable session
// clock. Manager never appends to the canonical trajectory.
type Config struct {
	Provider continuation.Provider
	Clock    func() time.Time
	Origin   time.Time
}

type attemptState struct {
	input       Input
	fingerprint string
	invocation  string
	triggered   time.Time
	started     time.Time
	first       *time.Time
	cancel      context.CancelCauseFunc
	done        chan struct{}
}

// Candidate is completed provider output captured off-trajectory.
type Candidate struct {
	descriptor  continuation.Descriptor
	fingerprint string
	invocation  string
	events      []continuation.Event
	completion  continuation.Completion
}

func (candidate *Candidate) Fingerprint() string  { return candidate.fingerprint }
func (candidate *Candidate) InvocationID() string { return candidate.invocation }

// ReplayProvider returns a one-shot provider that replays the accepted output
// through the ordinary continuation Runner, so canonical append invariants are
// identical to a post-endpoint invocation.
func (candidate *Candidate) ReplayProvider() continuation.Provider {
	if candidate == nil {
		return nil
	}
	return &replayProvider{
		descriptor: candidate.descriptor, expectedFingerprint: candidate.fingerprint,
		events:     cloneEvents(candidate.events),
		completion: cloneCompletion(candidate.completion),
	}
}

// Manager owns at most one provider request. New revisions cancel the active
// request and replace the pending input; if cancellation is slow, intermediate
// revisions coalesce instead of creating concurrent model work.
type Manager struct {
	mu sync.Mutex

	provider          continuation.Provider
	clock             func() time.Time
	origin            time.Time
	nextID            uint64
	latest            *Input
	latestParent      context.Context
	latestTriggered   time.Time
	latestFingerprint string
	active            *attemptState
	ready             *Candidate
	closed            bool
	report            Report
}

func NewManager(config Config) (*Manager, error) {
	if config.Provider == nil {
		return nil, errors.New("preparation provider is required")
	}
	descriptor := config.Provider.Descriptor()
	if err := continuation.ValidateDescriptor(descriptor); err != nil {
		return nil, err
	}
	if descriptor.Phase != trajectory.PhaseFast {
		return nil, errors.New("preparation provider must be a fast continuation")
	}
	if descriptor.EffectiveToolAuthority() == continuation.ToolAuthorityExecute {
		return nil, errors.New("preparation provider cannot have executable tool authority")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Origin.IsZero() {
		config.Origin = config.Clock()
	}
	return &Manager{provider: config.Provider, clock: config.Clock, origin: config.Origin, report: Report{Enabled: true}}, nil
}

// Observe records one revision opportunity and returns immediately after
// scheduling. Empty semantic trajectories should be filtered by the caller.
func (manager *Manager) Observe(ctx context.Context, input Input) error {
	if ctx == nil {
		return errors.New("preparation context must not be nil")
	}
	fingerprint, err := Fingerprint(input)
	if err != nil {
		return err
	}
	input = cloneInput(input)
	now := manager.clock()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return ErrClosed
	}
	manager.report.Observed++
	if fingerprint == manager.latestFingerprint {
		if manager.active == nil && manager.ready == nil {
			manager.latest, manager.latestParent, manager.latestTriggered = &input, ctx, now
			manager.startLocked(ctx, input, fingerprint, now)
		} else {
			manager.report.Coalesced++
		}
		return nil
	}
	manager.report.DistinctInputs++
	manager.latest, manager.latestFingerprint = &input, fingerprint
	manager.latestParent, manager.latestTriggered = ctx, now
	manager.ready = nil
	if manager.active != nil {
		manager.active.cancel(ErrSuperseded)
		return nil
	}
	manager.startLocked(ctx, input, fingerprint, now)
	return nil
}

// Finalize accepts only a candidate whose complete semantic fingerprint equals
// finalInput. A matching in-flight attempt is awaited; a stale attempt is
// cancelled and reported as a miss so the caller can invoke the final model.
func (manager *Manager) Finalize(ctx context.Context, finalInput Input) (*Candidate, Report, error) {
	if ctx == nil {
		return nil, Report{}, errors.New("preparation context must not be nil")
	}
	fingerprint, err := Fingerprint(finalInput)
	if err != nil {
		return nil, manager.Report(), err
	}
	manager.mu.Lock()
	manager.closed = true
	if manager.ready != nil && manager.ready.fingerprint == fingerprint {
		candidate := manager.acceptLocked(manager.ready)
		report := manager.cloneReportLocked()
		manager.mu.Unlock()
		return candidate, report, nil
	}
	active := manager.active
	if active == nil || active.fingerprint != fingerprint {
		if active != nil {
			active.cancel(ErrSuperseded)
		}
		if active == nil {
			report := manager.cloneReportLocked()
			manager.mu.Unlock()
			return nil, report, nil
		}
		done := active.done
		manager.mu.Unlock()
		select {
		case <-done:
			return nil, manager.Report(), nil
		case <-ctx.Done():
			return nil, manager.Report(), ctx.Err()
		}
	}
	done := active.done
	manager.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		active.cancel(ctx.Err())
		return nil, manager.Report(), ctx.Err()
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.ready == nil || manager.ready.fingerprint != fingerprint {
		return nil, manager.cloneReportLocked(), nil
	}
	candidate := manager.acceptLocked(manager.ready)
	return candidate, manager.cloneReportLocked(), nil
}

// Close cancels unfinished preparation and waits for the provider safe point.
// It is used when perception or the surrounding session fails before a final
// semantic input is available.
func (manager *Manager) Close(ctx context.Context, cause error) (Report, error) {
	if ctx == nil {
		return manager.Report(), errors.New("preparation close context must not be nil")
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

func (manager *Manager) startLocked(parent context.Context, input Input, fingerprint string, triggered time.Time) {
	manager.nextID++
	invocation := fmt.Sprintf("prepared-%d", manager.nextID)
	ctx, cancel := context.WithCancelCause(parent)
	state := &attemptState{
		input: input, fingerprint: fingerprint, invocation: invocation,
		triggered: triggered, started: manager.clock(), cancel: cancel, done: make(chan struct{}),
	}
	state.input.Request.InvocationID = invocation
	manager.active = state
	manager.report.Started++
	go manager.run(ctx, state)
}

func (manager *Manager) run(ctx context.Context, state *attemptState) {
	var events []continuation.Event
	completion, providerErr := manager.provider.Continue(ctx, state.input.Request, func(event continuation.Event) error {
		if err := continuation.ValidateEvent(event); err != nil {
			return err
		}
		manager.mu.Lock()
		if state.first == nil {
			value := manager.clock()
			state.first = &value
		}
		manager.mu.Unlock()
		events = append(events, cloneEvent(event))
		return nil
	})
	ended := manager.clock()
	cause := context.Cause(ctx)

	manager.mu.Lock()
	defer manager.mu.Unlock()
	outcome := "failed"
	if errors.Is(cause, ErrSuperseded) {
		outcome = "superseded"
		manager.report.Superseded++
	} else if providerErr == nil {
		outcome = "completed"
		manager.report.Completed++
		if manager.latestFingerprint == state.fingerprint {
			manager.ready = &Candidate{
				descriptor: manager.provider.Descriptor(), fingerprint: state.fingerprint,
				invocation: state.invocation, events: cloneEvents(events), completion: cloneCompletion(completion),
			}
		}
	} else {
		manager.report.Failed++
	}
	record := Attempt{
		InvocationID: state.invocation, Fingerprint: state.fingerprint,
		SourceRevision:    state.input.Request.Invocation.SourceRevision,
		TriggeredOffsetMS: milliseconds(state.triggered.Sub(manager.origin)),
		StartedOffsetMS:   milliseconds(state.started.Sub(manager.origin)),
		EndedOffsetMS:     milliseconds(ended.Sub(manager.origin)), Outcome: outcome,
	}
	if state.first != nil {
		value := milliseconds(state.first.Sub(manager.origin))
		record.FirstEventOffsetMS = &value
	}
	manager.report.Attempts = append(manager.report.Attempts, record)
	if manager.active == state {
		manager.active = nil
	}
	close(state.done)
	if !manager.closed && manager.latest != nil && manager.latestFingerprint != state.fingerprint && manager.latestParent.Err() == nil {
		manager.startLocked(manager.latestParent, *manager.latest, manager.latestFingerprint, manager.latestTriggered)
	}
}

func (manager *Manager) acceptLocked(candidate *Candidate) *Candidate {
	manager.report.Accepted = true
	manager.report.AcceptedInvocationID = candidate.invocation
	manager.report.AcceptedFingerprint = candidate.fingerprint
	for index := len(manager.report.Attempts) - 1; index >= 0; index-- {
		if manager.report.Attempts[index].InvocationID == candidate.invocation {
			manager.report.Attempts[index].Outcome = "accepted"
			break
		}
	}
	return &Candidate{
		descriptor: candidate.descriptor, fingerprint: candidate.fingerprint,
		invocation: candidate.invocation, events: cloneEvents(candidate.events),
		completion: cloneCompletion(candidate.completion),
	}
}

func (manager *Manager) Report() Report {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.cloneReportLocked()
}

func (manager *Manager) cloneReportLocked() Report {
	report := manager.report
	report.Attempts = slices.Clone(manager.report.Attempts)
	return report
}

type replayProvider struct {
	mu                  sync.Mutex
	descriptor          continuation.Descriptor
	expectedFingerprint string
	events              []continuation.Event
	completion          continuation.Completion
	used                bool
}

func (provider *replayProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *replayProvider) Continue(_ context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	if request.Descriptor != provider.descriptor {
		return continuation.Completion{}, errors.New("prepared replay descriptor mismatch")
	}
	fingerprint, err := Fingerprint(Input{Request: request})
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("fingerprint prepared replay input: %w", err)
	}
	if fingerprint != provider.expectedFingerprint {
		return continuation.Completion{}, errors.New("prepared replay semantic input mismatch")
	}
	provider.mu.Lock()
	if provider.used {
		provider.mu.Unlock()
		return continuation.Completion{}, errors.New("prepared replay is one-shot")
	}
	provider.used = true
	events := cloneEvents(provider.events)
	completion := cloneCompletion(provider.completion)
	provider.mu.Unlock()
	for _, event := range events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return completion, nil
}

func cloneInput(input Input) Input {
	input.Request.Invocation = cloneInvocation(input.Request.Invocation)
	input.Request.Trajectory = cloneSnapshot(input.Request.Trajectory)
	return input
}

func cloneSnapshot(snapshot trajectory.Snapshot) trajectory.Snapshot {
	items := make([]trajectory.Item, len(snapshot.Items))
	for index, item := range snapshot.Items {
		item.CausalParentIDs = slices.Clone(item.CausalParentIDs)
		item.ProviderState = slices.Clone(item.ProviderState)
		if item.ToolCall != nil {
			call := *item.ToolCall
			call.Arguments = slices.Clone(call.Arguments)
			item.ToolCall = &call
		}
		if item.ToolResult != nil {
			result := *item.ToolResult
			result.Output = slices.Clone(result.Output)
			item.ToolResult = &result
		}
		if item.AssistantState != nil {
			state := *item.AssistantState
			item.AssistantState = &state
		}
		items[index] = item
	}
	snapshot.Items = items
	return snapshot
}

func cloneInvocation(invocation continuation.Invocation) continuation.Invocation {
	invocation.Capabilities = slices.Clone(invocation.Capabilities)
	invocation.Tools = slices.Clone(invocation.Tools)
	for index := range invocation.Tools {
		invocation.Tools[index].Parameters = slices.Clone(invocation.Tools[index].Parameters)
	}
	return invocation
}

func cloneEvent(event continuation.Event) continuation.Event {
	if event.ToolCall != nil {
		call := *event.ToolCall
		call.Arguments = slices.Clone(call.Arguments)
		event.ToolCall = &call
	}
	return event
}

func cloneEvents(events []continuation.Event) []continuation.Event {
	cloned := make([]continuation.Event, len(events))
	for index, event := range events {
		cloned[index] = cloneEvent(event)
	}
	return cloned
}

func cloneCompletion(completion continuation.Completion) continuation.Completion {
	completion.ProviderState = slices.Clone(completion.ProviderState)
	return completion
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

// NonEmptyObservation reports whether a transcript contains any semantic input
// without classifying its content.
func NonEmptyObservation(text string) bool { return strings.TrimSpace(text) != "" }
