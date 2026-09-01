package management

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// RuntimeSession is satisfied by graph/runtime.Mounted without coupling this
// package to its concrete implementation.
type RuntimeSession interface {
	Live() inspect.Live
	RecordedTrace() (inspect.LiveTrace, error)
}

type registeredSession struct {
	id     uint64
	source RuntimeSession
}

// SessionRegistry is a dynamically scoped directory of live graph runtimes.
// Register returns an owner-specific disposer so an old session cleanup cannot
// remove a newer source that reused the public ID.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]registeredSession
	next     uint64
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{sessions: make(map[string]registeredSession)}
}

func (registry *SessionRegistry) Register(session string, source RuntimeSession) (func(), error) {
	if registry == nil || source == nil || !canonicalSessionID(session) {
		return nil, fmt.Errorf("register management session: %w", ErrInvalid)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, duplicate := registry.sessions[session]; duplicate {
		return nil, fmt.Errorf("register management session %s: %w", session, ErrConflict)
	}
	registry.next++
	entry := registeredSession{id: registry.next, source: source}
	registry.sessions[session] = entry
	var once sync.Once
	return func() {
		once.Do(func() {
			registry.mu.Lock()
			if current, found := registry.sessions[session]; found && current.id == entry.id {
				delete(registry.sessions, session)
			}
			registry.mu.Unlock()
		})
	}, nil
}

func (registry *SessionRegistry) source(session string) (RuntimeSession, error) {
	if registry == nil || !canonicalSessionID(session) {
		return nil, ErrNotFound
	}
	registry.mu.RLock()
	entry, found := registry.sessions[session]
	registry.mu.RUnlock()
	if !found {
		return nil, ErrNotFound
	}
	return entry.source, nil
}

func (registry *SessionRegistry) Snapshot(_ context.Context, session string) (inspect.Live, error) {
	source, err := registry.source(session)
	if err != nil {
		return inspect.Live{}, err
	}
	return RedactLive(source.Live()), nil
}

type runtimeGraphSource interface {
	Graph() ir.Graph
}

type runtimeInspectionModelSource interface {
	InspectionModel() (inspect.Model, error)
}

func (registry *SessionRegistry) Model(_ context.Context, session string) (inspect.Model, error) {
	source, err := registry.source(session)
	if err != nil {
		return inspect.Model{}, err
	}
	var model inspect.Model
	if provider, found := source.(runtimeInspectionModelSource); found {
		model, err = provider.InspectionModel()
	} else if provider, found := source.(runtimeGraphSource); found {
		model, err = inspect.Build(provider.Graph())
	} else {
		return inspect.Model{}, ErrUnavailable
	}
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return inspect.Model{}, err
		}
		return inspect.Model{}, fmt.Errorf("%w: build session inspection model: %v", ErrConflict, err)
	}
	if err := ValidateSessionModel(source.Live(), model); err != nil {
		return inspect.Model{}, err
	}
	return model, nil
}

func (registry *SessionRegistry) Trace(_ context.Context, session string) (inspect.LiveTrace, error) {
	source, err := registry.source(session)
	if err != nil {
		return inspect.LiveTrace{}, err
	}
	trace, err := source.RecordedTrace()
	if err != nil {
		return inspect.LiveTrace{}, fmt.Errorf("%w: recorded trace: %v", ErrUnavailable, err)
	}
	if err := trace.Validate(); err != nil {
		return inspect.LiveTrace{}, fmt.Errorf("%w: invalid recorded trace: %v", ErrConflict, err)
	}
	return trace.Clone(), nil
}

func (registry *SessionRegistry) Deltas(
	ctx context.Context, session string, after uint64, limit uint32,
) (DeltaPage, error) {
	if limit == 0 || limit > 4096 {
		return DeltaPage{}, fmt.Errorf("%w: delta limit", ErrInvalid)
	}
	trace, err := registry.Trace(ctx, session)
	if err != nil {
		return DeltaPage{}, err
	}
	if len(trace.Snapshots) == 0 {
		return DeltaPage{}, fmt.Errorf("%w: trace has no baseline", ErrUnavailable)
	}
	snapshots := append([]inspect.TraceSnapshot(nil), trace.Snapshots...)
	sort.Slice(snapshots, func(left, right int) bool { return snapshots[left].Sequence < snapshots[right].Sequence })
	first := snapshots[0]
	latest := first.Sequence
	if len(trace.Events) > 0 && trace.Events[len(trace.Events)-1].Sequence > latest {
		latest = trace.Events[len(trace.Events)-1].Sequence
	}
	if after > latest {
		return DeltaPage{}, fmt.Errorf("%w: delta cursor is ahead of the recorder", ErrInvalid)
	}
	page := DeltaPage{
		FormatVersion: 1, SessionID: session, Graph: trace.Graph,
		After: after, Next: after, Dropped: first.Graph.TraceDropped,
	}
	start := after
	if after == 0 || after < first.Sequence {
		baseline := first.Clone()
		page.Baseline = &baseline
		page.Compacted = after != 0 && after < first.Sequence
		start = first.Sequence
		page.Next = start
	}
	for _, event := range trace.Events {
		if event.Sequence <= start {
			continue
		}
		if len(page.Events) == int(limit) {
			break
		}
		page.Events = append(page.Events, event.Clone())
		page.Next = event.Sequence
	}
	return page, nil
}

func canonicalSessionID(value string) bool {
	return CanonicalSessionID(value)
}
