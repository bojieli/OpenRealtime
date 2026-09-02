package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/plugin"
)

// MaximumStateSnapshotBytes bounds both predecessor snapshots and migrated
// state. The bound is checked before and after canonicalization.
const MaximumStateSnapshotBytes = 1 << 20

type stateLifecycle struct {
	mu sync.Mutex

	descriptor plugin.Descriptor
	restored   json.RawMessage
	consumed   bool
	snapshot   StateSnapshotter
	sealed     bool
}

func newStateLifecycle(
	descriptor plugin.Descriptor, restored json.RawMessage,
) *stateLifecycle {
	return &stateLifecycle{
		descriptor: descriptor.Clone(), restored: slices.Clone(restored),
	}
}

func (state *stateLifecycle) Restored() (json.RawMessage, bool, error) {
	if state == nil {
		return nil, false, errors.New("plugin state lifecycle is unavailable")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sealed {
		return nil, false, errors.New("plugin state lifecycle is sealed")
	}
	if state.consumed {
		return nil, false, errors.New("plugin restored state was already consumed")
	}
	state.consumed = true
	if len(state.restored) == 0 {
		return nil, false, nil
	}
	return slices.Clone(state.restored), true, nil
}

func (state *stateLifecycle) Snapshot(snapshot StateSnapshotter) error {
	if state == nil {
		return errors.New("plugin state lifecycle is unavailable")
	}
	if snapshot == nil {
		return errors.New("plugin state snapshot requires a callback")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sealed {
		return errors.New("plugin state lifecycle is sealed")
	}
	if !state.descriptor.Lifecycle.Snapshot {
		return fmt.Errorf("plugin %s does not declare snapshot support", state.descriptor.Name)
	}
	if state.snapshot != nil {
		return fmt.Errorf("plugin %s registered more than one state snapshot", state.descriptor.Name)
	}
	state.snapshot = snapshot
	return nil
}

func (state *stateLifecycle) seal() (StateSnapshotter, error) {
	if state == nil {
		return nil, nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sealed {
		return nil, errors.New("plugin state lifecycle was already sealed")
	}
	state.sealed = true
	if len(state.restored) > 0 {
		if !state.descriptor.Lifecycle.Restore {
			return nil, fmt.Errorf("plugin %s does not declare restore support", state.descriptor.Name)
		}
		if !state.consumed {
			return nil, fmt.Errorf("plugin %s did not consume restored state", state.descriptor.Name)
		}
	}
	if state.descriptor.Lifecycle.Snapshot && state.snapshot == nil {
		return nil, fmt.Errorf("plugin %s did not register a state snapshot", state.descriptor.Name)
	}
	return state.snapshot, nil
}

func canonicalStateSnapshot(raw json.RawMessage) (json.RawMessage, string, error) {
	if len(raw) == 0 {
		return nil, "", errors.New("state snapshot is empty")
	}
	if len(raw) > MaximumStateSnapshotBytes {
		return nil, "", fmt.Errorf(
			"state snapshot is %d bytes, limit is %d", len(raw), MaximumStateSnapshotBytes,
		)
	}
	canonical, digest, err := graphvalues.Digest(raw)
	if err != nil {
		return nil, "", fmt.Errorf("state snapshot must be a strict JSON object: %w", err)
	}
	if len(canonical) > MaximumStateSnapshotBytes {
		return nil, "", fmt.Errorf(
			"canonical state snapshot is %d bytes, limit is %d",
			len(canonical), MaximumStateSnapshotBytes,
		)
	}
	return canonical, digest, nil
}
