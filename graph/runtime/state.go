package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

// MaximumStateSnapshotBytes bounds predecessor, restored, and migrated graph
// element state before and after canonicalization.
const MaximumStateSnapshotBytes = 1 << 20

// stateLifecycle is a mount-scoped registration boundary. It deliberately
// stores callbacks rather than invoking them during ordinary execution; a
// future graph-plan reconciler invokes the sealed callbacks only at its safe
// point.
type stateLifecycle struct {
	mu sync.Mutex

	instance string
	schema   string
	transfer element.StateTransferCapabilities
	restored json.RawMessage
	consumed bool
	quiesce  element.StateQuiescer
	snapshot element.StateSnapshotter
	sealed   bool
}

func newStateLifecycle(
	instance, schema string,
	transfer *element.StateTransferCapabilities,
	restored json.RawMessage,
) *stateLifecycle {
	capabilities := element.StateTransferCapabilities{}
	if transfer != nil {
		capabilities = *transfer
	}
	return &stateLifecycle{
		instance: instance, schema: schema, transfer: capabilities,
		restored: slices.Clone(restored),
	}
}

func (state *stateLifecycle) Restored() (json.RawMessage, bool, error) {
	if state == nil {
		return nil, false, errors.New("element state lifecycle is unavailable")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sealed {
		return nil, false, fmt.Errorf("element %s state lifecycle is sealed", state.instance)
	}
	if !state.transfer.Restore {
		return nil, false, fmt.Errorf(
			"element %s does not declare state restore capability", state.instance,
		)
	}
	if state.consumed {
		return nil, false, fmt.Errorf("element %s restored state was already consumed", state.instance)
	}
	state.consumed = true
	if len(state.restored) == 0 {
		return nil, false, nil
	}
	return slices.Clone(state.restored), true, nil
}

func (state *stateLifecycle) Snapshot(snapshot element.StateSnapshotter) error {
	if state == nil {
		return errors.New("element state lifecycle is unavailable")
	}
	if snapshot == nil {
		return fmt.Errorf("element %s state snapshot requires a callback", state.instance)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sealed {
		return fmt.Errorf("element %s state lifecycle is sealed", state.instance)
	}
	if !state.transfer.Snapshot {
		return fmt.Errorf(
			"element %s does not declare state snapshot capability", state.instance,
		)
	}
	if state.snapshot != nil {
		return fmt.Errorf("element %s registered more than one state snapshot", state.instance)
	}
	state.snapshot = snapshot
	return nil
}

func (state *stateLifecycle) Quiesce(quiesce element.StateQuiescer) error {
	if state == nil {
		return errors.New("element state lifecycle is unavailable")
	}
	if quiesce == nil {
		return fmt.Errorf("element %s state quiescence requires a callback", state.instance)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sealed {
		return fmt.Errorf("element %s state lifecycle is sealed", state.instance)
	}
	if !state.transfer.Quiesce {
		return fmt.Errorf(
			"element %s does not declare state quiesce capability", state.instance,
		)
	}
	if state.quiesce != nil {
		return fmt.Errorf("element %s registered more than one state quiescer", state.instance)
	}
	state.quiesce = quiesce
	return nil
}

func (state *stateLifecycle) seal() (
	element.StateSnapshotter, element.StateQuiescer, error,
) {
	if state == nil {
		return nil, nil, nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sealed {
		return nil, nil, fmt.Errorf("element %s state lifecycle was already sealed", state.instance)
	}
	state.sealed = true
	if len(state.restored) != 0 {
		if !state.consumed {
			return nil, nil, fmt.Errorf("element %s did not consume restored state", state.instance)
		}
	}
	if state.transfer.Snapshot && state.snapshot == nil {
		return nil, nil, fmt.Errorf(
			"element %s declared state snapshot capability without registering a callback",
			state.instance,
		)
	}
	if state.transfer.Quiesce && state.quiesce == nil {
		return nil, nil, fmt.Errorf(
			"element %s declared state quiesce capability without registering a callback",
			state.instance,
		)
	}
	if state.quiesce != nil && state.snapshot == nil {
		return nil, nil, fmt.Errorf(
			"element %s registered state quiescence without a state snapshot", state.instance,
		)
	}
	return state.snapshot, state.quiesce, nil
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

type stateCapture struct {
	node           string
	identity       element.Identity
	implementation string
	schema         string
	snapshot       json.RawMessage
	digest         string
}

type stateQuiescence struct {
	node   string
	resume element.StateResumer
}

// captureStateAtBarrier is the pre-teardown half of state transfer. It keeps
// quiescence and capture inseparable so a capture refusal cannot accidentally
// strand mutation admission. On success, the caller owns the returned
// quiescence handles until it either retires the predecessor or explicitly
// resumes after a later pre-teardown refusal.
func (mounted *Mounted) captureStateAtBarrier(
	operation context.Context, selected map[string]struct{},
) ([]stateCapture, []stateQuiescence, error) {
	if err := mounted.requireTransferableState(selected); err != nil {
		return nil, nil, err
	}
	quiesced, err := mounted.quiesceState(operation, selected)
	if err != nil {
		return nil, nil, err
	}
	captures, err := mounted.captureState(operation, selected)
	if err == nil {
		return captures, quiesced, nil
	}
	resumeErr := mounted.resumeState(quiesced)
	return nil, nil, errors.Join(
		err, wrapStateError("resume graph state mutation admission", resumeErr),
	)
}

// requireTransferableState proves the currently mounted side of a future
// state-preserving transition before any quiescence or teardown. An element
// may declare StateSchema without opting in to hot transfer; such a node makes
// the candidate update ineligible instead of breaking ordinary initial mounts.
func (mounted *Mounted) requireTransferableState(selected map[string]struct{}) error {
	if mounted == nil {
		return errors.New("graph state transfer requires a mounted graph")
	}
	known := make(map[string]struct{}, len(mounted.nodes))
	for _, node := range mounted.nodes {
		known[node.id] = struct{}{}
	}
	unknown := make([]string, 0)
	for node := range selected {
		if _, found := known[node]; !found {
			unknown = append(unknown, node)
		}
	}
	if len(unknown) != 0 {
		slices.Sort(unknown)
		return fmt.Errorf("graph state selection names unknown nodes: %v", unknown)
	}
	for _, node := range mounted.nodes {
		if _, affected := selected[node.id]; !affected || node.stateSchema == "" {
			continue
		}
		if node.stateTransfer == nil || !node.stateTransfer.Snapshot {
			return fmt.Errorf(
				"graph node %s schema %s does not declare state snapshot capability",
				node.id, node.stateSchema,
			)
		}
		if node.snapshot == nil {
			return fmt.Errorf(
				"graph node %s schema %s has no live state snapshot callback",
				node.id, node.stateSchema,
			)
		}
	}
	return nil
}

// quiesceState closes mutation admission in reverse mount order. Any refusal
// resumes every callback that was successfully admitted, including a callback
// returned alongside an error by the refusing node.
func (mounted *Mounted) quiesceState(
	operation context.Context, selected map[string]struct{},
) ([]stateQuiescence, error) {
	if operation == nil {
		return nil, errors.New("quiesce graph state: nil context")
	}
	var quiesced []stateQuiescence
	for index := len(mounted.nodes) - 1; index >= 0; index-- {
		node := mounted.nodes[index]
		if _, affected := selected[node.id]; !affected || node.quiesce == nil {
			continue
		}
		resume, err := mounted.callStateQuiescer(operation, node)
		if resume != nil {
			quiesced = append(quiesced, stateQuiescence{node: node.id, resume: resume})
		}
		if err == nil && resume == nil {
			err = errors.New("state quiescer returned no resume callback")
		}
		if err != nil {
			resumeErr := mounted.resumeState(quiesced)
			return nil, errors.Join(
				fmt.Errorf("quiesce graph state for node %s schema %s: %w",
					node.id, node.stateSchema, err),
				wrapStateError("resume graph state mutation admission", resumeErr),
			)
		}
	}
	return quiesced, nil
}

func (mounted *Mounted) callStateQuiescer(
	operation context.Context, node mountedNode,
) (resume element.StateResumer, err error) {
	callbackContext, cancel := context.WithTimeout(operation, mounted.timeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.Join(err, fmt.Errorf("state quiescer panicked: %v", recovered))
		}
		if cause := context.Cause(callbackContext); cause != nil {
			err = errors.Join(err, cause)
		}
	}()
	return node.quiesce(callbackContext)
}

// resumeState reopens predecessors in mount/dependency order under detached,
// bounded recovery contexts. Caller cancellation must not strand the live
// predecessor after a refused pre-teardown transition.
func (mounted *Mounted) resumeState(quiesced []stateQuiescence) (err error) {
	for index := len(quiesced) - 1; index >= 0; index-- {
		row := quiesced[index]
		if resumeErr := mounted.callStateResumer(row); resumeErr != nil {
			err = errors.Join(err, fmt.Errorf("resume graph state for node %s: %w", row.node, resumeErr))
		}
	}
	return err
}

func (mounted *Mounted) callStateResumer(row stateQuiescence) (err error) {
	callbackContext, cancel := context.WithTimeout(context.Background(), mounted.timeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.Join(err, fmt.Errorf("state resumer panicked: %v", recovered))
		}
		if cause := context.Cause(callbackContext); cause != nil {
			err = errors.Join(err, cause)
		}
	}()
	return row.resume(callbackContext)
}

// captureState snapshots selected stateful nodes after the caller establishes
// the graph safe point. Captures remain tied to the exact mounted generation;
// only schema and digest may later cross into a payload-free receipt.
func (mounted *Mounted) captureState(
	operation context.Context, selected map[string]struct{},
) ([]stateCapture, error) {
	if operation == nil {
		return nil, errors.New("capture graph state: nil context")
	}
	if err := mounted.requireTransferableState(selected); err != nil {
		return nil, err
	}
	captures := make([]stateCapture, 0, len(selected))
	for _, node := range mounted.nodes {
		if _, affected := selected[node.id]; !affected || node.stateSchema == "" {
			continue
		}
		raw, err := mounted.callStateSnapshotter(operation, node)
		if err != nil {
			return nil, fmt.Errorf(
				"snapshot graph state for node %s schema %s: %w",
				node.id, node.stateSchema, err,
			)
		}
		canonical, digest, err := canonicalStateSnapshot(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"snapshot graph state for node %s schema %s: %w",
				node.id, node.stateSchema, err,
			)
		}
		captures = append(captures, stateCapture{
			node: node.id, identity: node.identity,
			implementation: node.implementation, schema: node.stateSchema,
			snapshot: slices.Clone(canonical), digest: digest,
		})
	}
	return captures, nil
}

func (mounted *Mounted) callStateSnapshotter(
	operation context.Context, node mountedNode,
) (raw json.RawMessage, err error) {
	callbackContext, cancel := context.WithTimeout(operation, mounted.timeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.Join(err, fmt.Errorf("state snapshotter panicked: %v", recovered))
		}
		if cause := context.Cause(callbackContext); cause != nil {
			err = errors.Join(err, cause)
		}
	}()
	return node.snapshot(callbackContext)
}

func wrapStateError(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", label, err)
}
