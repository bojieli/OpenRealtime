// Package eventloop serializes asynchronous external events into one canonical
// trajectory and invokes cognition only at safe points.
//
// Event sources may run concurrently. They submit typed observations, complete
// tool-result batches, or media commitment transitions. The coordinator is the
// sole external-event commit owner. Routine events wait while cognition is in
// flight; an explicitly typed interrupt cooperatively cancels that work and is
// committed at the next provider-supported boundary. No text pattern matching
// or model-authored workflow state is involved.
package eventloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

var (
	ErrIdle        = errors.New("event coordinator has no pending events")
	ErrBusy        = errors.New("event coordinator is already processing a batch")
	ErrInterrupted = errors.New("event processing interrupted by an urgent event")
	ErrQueueFull   = errors.New("event coordinator pending queue is full")
)

// Priority is supplied by a trusted event source or a semantic/acoustic
// classifier. The coordinator never derives it from words in event content.
type Priority string

const (
	PriorityRoutine   Priority = "routine"
	PriorityInterrupt Priority = "interrupt"
)

// Event is one external occurrence. EventID is assigned on Submit when empty.
// ToolResults is deliberately a batch: one slow invocation's complete set of
// outstanding calls crosses the synchronization boundary atomically.
type Event struct {
	EventID        string
	Type           string
	Source         string
	Channel        string
	CorrelationID  string
	Priority       Priority
	Kind           trajectory.Kind
	OccurredNS     uint64
	SourceRevision uint64
	InvocationID   string
	Producer       trajectory.Producer
	Content        string
	ToolResults    []trajectory.ToolResult
	AssistantState *trajectory.AssistantState
}

// Batch is the exact event group committed before one processor invocation.
type Batch struct {
	Events       []Event
	Items        []trajectory.Item
	StartVersion uint64
	EndVersion   uint64
}

// Processor advances cognition from a committed event batch. Implementations
// may run fast then slow continuation over the same trajectory. The context is
// cancelled when an interrupt event arrives; the processor must stop at its
// next safe boundary.
type Processor interface {
	Process(context.Context, Batch) error
}

type ProcessorFunc func(context.Context, Batch) error

func (function ProcessorFunc) Process(ctx context.Context, batch Batch) error {
	return function(ctx, batch)
}

type Config struct {
	Store     *trajectory.Store
	Processor Processor
	// MaxPendingEvents is a required hard ingress bound.
	MaxPendingEvents int
	// ReservedInterruptEvents protects capacity from routine-event bursts. It
	// may be zero, but must be smaller than MaxPendingEvents.
	ReservedInterruptEvents int
	Now                     func() uint64
	NextID                  func(prefix string) string
}

type queuedEvent struct {
	sequence uint64
	event    Event
}

// Coordinator owns ingress ordering, safe-point batches, and interruption of
// the current processor invocation. Model output still commits through the
// continuation runner's version-checked transaction.
type Coordinator struct {
	mu   sync.Mutex
	idMu sync.Mutex

	store        *trajectory.Store
	processor    Processor
	now          func() uint64
	nextID       func(string) string
	pending      []queuedEvent
	eventIDs     map[string]struct{}
	nextSequence uint64
	active       bool
	activeEpoch  uint64
	activeCancel context.CancelCauseFunc
	maxPending   int
	reserved     int
}

func New(config Config) (*Coordinator, error) {
	if config.Store == nil {
		return nil, errors.New("event coordinator requires a trajectory store")
	}
	if config.MaxPendingEvents <= 0 {
		return nil, errors.New("event coordinator requires a positive pending-event bound")
	}
	if config.ReservedInterruptEvents < 0 || config.ReservedInterruptEvents >= config.MaxPendingEvents {
		return nil, errors.New("reserved interrupt capacity must be non-negative and smaller than the pending-event bound")
	}
	if config.Now == nil {
		origin := time.Now()
		config.Now = func() uint64 { return uint64(time.Since(origin)) }
	}
	if config.NextID == nil {
		var counter atomic.Uint64
		config.NextID = func(prefix string) string {
			return prefix + "-" + strconv.FormatUint(counter.Add(1), 10)
		}
	}
	return &Coordinator{
		store: config.Store, processor: config.Processor, now: config.Now,
		nextID: config.NextID, eventIDs: make(map[string]struct{}),
		maxPending: config.MaxPendingEvents, reserved: config.ReservedInterruptEvents,
	}, nil
}

// Submit validates and queues an event without waiting for cognition. An
// interrupt event cooperatively cancels the active processor immediately, but
// the event itself enters the trajectory only after that work reaches a safe
// point.
func (coordinator *Coordinator) Submit(event Event) (string, error) {
	if err := validateEvent(event); err != nil {
		return "", err
	}
	event = cloneEvent(event)
	coordinator.idMu.Lock()
	if event.EventID == "" {
		event.EventID = coordinator.nextID("event")
	}
	coordinator.idMu.Unlock()
	if strings.TrimSpace(event.EventID) == "" {
		return "", errors.New("event ID generator returned an empty ID")
	}

	coordinator.mu.Lock()
	limit := coordinator.maxPending
	if event.Priority == PriorityRoutine {
		limit -= coordinator.reserved
	}
	if len(coordinator.pending) >= limit {
		coordinator.mu.Unlock()
		return "", ErrQueueFull
	}
	if _, duplicate := coordinator.eventIDs[event.EventID]; duplicate {
		coordinator.mu.Unlock()
		return "", fmt.Errorf("duplicate event ID %q", event.EventID)
	}
	coordinator.eventIDs[event.EventID] = struct{}{}
	coordinator.nextSequence++
	coordinator.pending = append(coordinator.pending, queuedEvent{sequence: coordinator.nextSequence, event: event})
	cancel := coordinator.activeCancel
	shouldInterrupt := event.Priority == PriorityInterrupt && cancel != nil
	coordinator.mu.Unlock()
	if shouldInterrupt {
		cancel(ErrInterrupted)
	}
	return event.EventID, nil
}

// RunNext drains the currently pending events in arrival order, commits them
// atomically, and invokes Processor once. Events submitted while Processor is
// running remain queued for the next call. Only one RunNext may be active.
func (coordinator *Coordinator) RunNext(parent context.Context) (Batch, error) {
	if parent == nil {
		return Batch{}, errors.New("event processor context must not be nil")
	}
	coordinator.mu.Lock()
	if coordinator.active {
		coordinator.mu.Unlock()
		return Batch{}, ErrBusy
	}
	if len(coordinator.pending) == 0 {
		coordinator.mu.Unlock()
		return Batch{}, ErrIdle
	}
	queued := append([]queuedEvent(nil), coordinator.pending...)
	coordinator.pending = nil
	ctx, cancel := context.WithCancelCause(parent)
	coordinator.active = true
	coordinator.activeEpoch++
	epoch := coordinator.activeEpoch
	coordinator.activeCancel = cancel
	coordinator.mu.Unlock()

	batch, commitErr := coordinator.commit(queued)
	if commitErr != nil {
		coordinator.finish(epoch)
		if errors.Is(commitErr, trajectory.ErrVersionConflict) {
			coordinator.requeue(queued)
		} else {
			coordinator.forget(queued)
		}
		return Batch{}, commitErr
	}
	var processErr error
	if coordinator.processor != nil {
		processErr = coordinator.processor.Process(ctx, batch)
	}
	if cause := context.Cause(ctx); cause != nil {
		processErr = errors.Join(processErr, cause)
	}
	coordinator.finish(epoch)
	return batch, processErr
}

func (coordinator *Coordinator) Pending() int {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return len(coordinator.pending)
}

func (coordinator *Coordinator) Active() bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.active
}

// Interrupt cooperatively cancels the processor currently running at a safe
// point without fabricating a model-visible trajectory item. It is used for
// operational signals such as the acoustic start of user speech: the final
// transcript is submitted later as the authoritative observation event.
// Calling Interrupt while idle is a no-op.
func (coordinator *Coordinator) Interrupt(cause error) {
	if cause == nil {
		cause = ErrInterrupted
	}
	coordinator.mu.Lock()
	cancel := coordinator.activeCancel
	coordinator.mu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

func (coordinator *Coordinator) finish(epoch uint64) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.active && coordinator.activeEpoch == epoch {
		coordinator.active = false
		coordinator.activeCancel = nil
	}
}

func (coordinator *Coordinator) requeue(events []queuedEvent) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	coordinator.pending = append(append([]queuedEvent(nil), events...), coordinator.pending...)
}

func (coordinator *Coordinator) forget(events []queuedEvent) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	for _, event := range events {
		delete(coordinator.eventIDs, event.event.EventID)
	}
}

func (coordinator *Coordinator) commit(queued []queuedEvent) (Batch, error) {
	slices.SortStableFunc(queued, func(left, right queuedEvent) int {
		if left.sequence < right.sequence {
			return -1
		}
		if left.sequence > right.sequence {
			return 1
		}
		return 0
	})
	events := make([]Event, len(queued))
	for index := range queued {
		events[index] = cloneEvent(queued[index].event)
	}
	snapshot := coordinator.store.Snapshot()
	items, err := coordinator.compile(snapshot, events)
	if err != nil {
		return Batch{}, err
	}
	if err := coordinator.store.AppendBatchAt(snapshot.Version, items); err != nil {
		return Batch{}, fmt.Errorf("commit external event batch: %w", err)
	}
	committed := coordinator.store.Snapshot()
	return Batch{
		Events: events, Items: append([]trajectory.Item(nil), committed.Items[snapshot.Version:]...),
		StartVersion: snapshot.Version, EndVersion: committed.Version,
	}, nil
}

func (coordinator *Coordinator) compile(snapshot trajectory.Snapshot, events []Event) ([]trajectory.Item, error) {
	all := append([]trajectory.Item(nil), snapshot.Items...)
	items := make([]trajectory.Item, 0, len(events))
	lastNS := uint64(0)
	parentID := ""
	if len(all) > 0 {
		lastNS = all[len(all)-1].MonotonicNS
		parentID = all[len(all)-1].ID
	}
	nextNS := func() uint64 {
		value := coordinator.now()
		if value < lastNS {
			value = lastNS
		}
		lastNS = value
		return value
	}
	nextItemID := func() string {
		coordinator.idMu.Lock()
		defer coordinator.idMu.Unlock()
		return coordinator.nextID("event-item")
	}

	for _, event := range events {
		metadata := &trajectory.EventMetadata{
			EventID: event.EventID, Type: event.Type, Source: event.Source,
			Channel: event.Channel, OccurredNS: event.OccurredNS, CorrelationID: event.CorrelationID,
		}
		switch event.Kind {
		case trajectory.KindObservation:
			item := trajectory.Item{
				ID: nextItemID(), Kind: trajectory.KindObservation, MonotonicNS: nextNS(),
				SourceRevision: event.SourceRevision, Producer: event.Producer,
				Content: event.Content, Event: metadata,
			}
			item.CausalParentIDs = directParents(parentID)
			items = append(items, item)
			all = append(all, item)
			parentID = item.ID
		case trajectory.KindAssistantState:
			state := *event.AssistantState
			item := trajectory.Item{
				ID: nextItemID(), Kind: trajectory.KindAssistantState, MonotonicNS: nextNS(),
				SourceRevision: event.SourceRevision, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
				AssistantState: &state, Event: metadata,
			}
			item.CausalParentIDs = directParents(parentID, state.AssistantItemID)
			items = append(items, item)
			all = append(all, item)
			parentID = item.ID
		case trajectory.KindToolResult:
			compiled, err := coordinator.compileToolResults(all, parentID, nextNS, nextItemID, event, metadata)
			if err != nil {
				return nil, err
			}
			items = append(items, compiled...)
			all = append(all, compiled...)
			parentID = compiled[len(compiled)-1].ID
		}
	}
	return items, nil
}

func (coordinator *Coordinator) compileToolResults(
	all []trajectory.Item,
	parentID string,
	nextNS func() uint64,
	nextItemID func() string,
	event Event,
	metadata *trajectory.EventMetadata,
) ([]trajectory.Item, error) {
	matched, err := trajectory.MatchToolResultBatch(
		trajectory.Snapshot{Version: uint64(len(all)), Items: all},
		event.InvocationID,
		event.ToolResults,
	)
	if err != nil {
		return nil, err
	}

	items := make([]trajectory.Item, 0, len(matched))
	for _, pair := range matched {
		metadataCopy := *metadata
		result := pair.Result
		item := trajectory.Item{
			ID: nextItemID(), Kind: trajectory.KindToolResult, MonotonicNS: nextNS(),
			CausalParentIDs: directParents(parentID, pair.Pending.ItemID),
			SourceRevision:  pair.Pending.SourceRevision, InvocationID: event.InvocationID,
			Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &result,
			Event: &metadataCopy,
		}
		items = append(items, item)
		parentID = item.ID
	}
	return items, nil
}

func validateEvent(event Event) error {
	if strings.TrimSpace(event.Type) == "" || strings.TrimSpace(event.Source) == "" || strings.TrimSpace(event.Channel) == "" {
		return errors.New("event type, source, and channel are required")
	}
	switch event.Priority {
	case PriorityRoutine, PriorityInterrupt:
	default:
		return fmt.Errorf("unsupported event priority %q", event.Priority)
	}
	switch event.Kind {
	case trajectory.KindObservation:
		if strings.TrimSpace(event.Content) == "" || event.Producer.Phase == "" || len(event.ToolResults) != 0 || event.AssistantState != nil {
			return errors.New("observation event requires content and producer only")
		}
	case trajectory.KindToolResult:
		if event.InvocationID == "" || len(event.ToolResults) == 0 || event.Content != "" || event.AssistantState != nil {
			return errors.New("tool-result event requires invocation ID and a non-empty result batch")
		}
		for index, result := range event.ToolResults {
			if strings.TrimSpace(result.CallID) == "" || strings.TrimSpace(result.Name) == "" {
				return fmt.Errorf("tool result %d requires call ID and name", index)
			}
			hasOutput := len(result.Output) > 0
			hasError := strings.TrimSpace(result.Error) != ""
			if hasOutput == hasError || hasOutput && !json.Valid(result.Output) {
				return fmt.Errorf("tool result %d requires exactly one valid JSON output or error", index)
			}
		}
	case trajectory.KindAssistantState:
		if event.AssistantState == nil || event.Content != "" || len(event.ToolResults) != 0 {
			return errors.New("assistant-state event requires one state transition")
		}
	default:
		return fmt.Errorf("external event cannot directly append trajectory kind %q", event.Kind)
	}
	return nil
}

func directParents(ids ...string) []string {
	var result []string
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func cloneEvent(event Event) Event {
	event.ToolResults = append([]trajectory.ToolResult(nil), event.ToolResults...)
	for index := range event.ToolResults {
		event.ToolResults[index].Output = slices.Clone(event.ToolResults[index].Output)
	}
	if event.AssistantState != nil {
		copy := *event.AssistantState
		event.AssistantState = &copy
	}
	return event
}
