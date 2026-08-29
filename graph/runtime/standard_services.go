package runtime

import (
	"errors"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
)

const (
	ClockServiceName    = "runtime.clock"
	SequenceServiceName = "runtime.sequence"
)

// Clock is the monotonic time coeffect available to every mounted graph.
// Elements declare it when timestamps or deadlines affect semantics.
type Clock interface {
	NowNS() uint64
}

type ClockFunc func() uint64

func (function ClockFunc) NowNS() uint64 { return function() }

// SequenceAllocator supplies monotonic, mount-scoped namespaces. It avoids a
// hidden package-global counter while allowing several independently authored
// elements to allocate one canonical revision domain.
type SequenceAllocator struct {
	mu     sync.Mutex
	values map[string]uint64
}

func NewSequenceAllocator() *SequenceAllocator {
	return &SequenceAllocator{values: make(map[string]uint64)}
}

func (allocator *SequenceAllocator) Next(namespace string) (uint64, error) {
	if allocator == nil {
		return 0, errors.New("sequence allocator is nil")
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return 0, errors.New("sequence namespace is empty")
	}
	allocator.mu.Lock()
	defer allocator.mu.Unlock()
	if allocator.values == nil {
		allocator.values = make(map[string]uint64)
	}
	if allocator.values[namespace] == ^uint64(0) {
		return 0, errors.New("sequence exhausted")
	}
	allocator.values[namespace]++
	return allocator.values[namespace], nil
}

// mountServices overlays runtime-owned coeffects on deployment services. The
// overlay is deliberately created for each Mount call: callers commonly reuse
// one deployment ServiceSet across sessions, but clocks and revision
// namespaces must never leak across those sessions. Runtime-owned names shadow
// identically named deployment entries rather than mutating caller state.
type mountServices struct {
	deployment element.Services
	clock      Clock
	sequences  *SequenceAllocator
}

func newMountServices(deployment element.Services, now func() uint64) element.Services {
	return &mountServices{
		deployment: deployment,
		clock:      ClockFunc(now),
		sequences:  NewSequenceAllocator(),
	}
}

func (services *mountServices) Lookup(name string) (any, uint64, bool) {
	switch name {
	case ClockServiceName:
		return services.clock, 1, true
	case SequenceServiceName:
		return services.sequences, 1, true
	default:
		if services.deployment == nil {
			return nil, 0, false
		}
		return services.deployment.Lookup(name)
	}
}
