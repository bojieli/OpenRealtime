package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

type nodeResult struct {
	id  string
	err error
}

// Run supervises all element reaction loops. The first unexpected node error
// cancels sibling work; shutdown is bounded and reports any element that does
// not honor cancellation.
func (mounted *Mounted) Run(parent context.Context) error {
	if parent == nil {
		return errors.New("run graph: nil context")
	}
	mounted.mu.Lock()
	if mounted.started || mounted.closed {
		mounted.mu.Unlock()
		return ErrAlreadyRunning
	}
	mounted.started = true
	ctx, cancel := context.WithCancelCause(parent)
	mounted.cancel = cancel
	mounted.mu.Unlock()
	mounted.recorder.signal()

	results := make(chan nodeResult, len(mounted.nodes))
	active := make(map[string]struct{}, len(mounted.nodes))
	lifecycleFailed := make(map[string]struct{})
	for _, node := range mounted.nodes {
		active[node.id] = struct{}{}
		mounted.setNodeState(node.id, inspectState("running", ""))
		go func(node mountedNode) {
			err := runMountedNode(node.runnable, ctx)
			results <- nodeResult{id: node.id, err: err}
		}(node)
	}

	remaining := len(mounted.nodes)
	var primary error
	runDone := ctx.Done()
	var shutdownTimer *time.Timer
	var shutdownDeadline <-chan time.Time
	beginCancellation := func(cause error) {
		if shutdownDeadline != nil {
			return
		}
		cancel(cause)
		shutdownTimer = time.NewTimer(mounted.timeout)
		shutdownDeadline = shutdownTimer.C
	}
	for remaining > 0 {
		select {
		case result := <-mounted.lifecycleFailures:
			failure := fmt.Errorf("graph node %s lifecycle failed: %w", result.id, result.err)
			primary = errors.Join(primary, failure)
			lifecycleFailed[result.id] = struct{}{}
			mounted.setNodeState(result.id, inspectState("failed", result.err.Error()))
			beginCancellation(failure)
		case result := <-results:
			if _, found := active[result.id]; !found {
				continue
			}
			delete(active, result.id)
			remaining--
			_, ownedWorkFailed := lifecycleFailed[result.id]
			if result.err != nil && shutdownDeadline == nil && ctx.Err() == nil {
				primary = fmt.Errorf("graph node %s stopped: %w", result.id, result.err)
				mounted.setNodeState(result.id, inspectState("failed", result.err.Error()))
				beginCancellation(primary)
			} else if ownedWorkFailed {
				// The lifecycle failure is the node's terminal state. Its main
				// Runnable returning after sibling cancellation must not erase it.
			} else if result.err != nil {
				mounted.setNodeState(result.id, inspectState("stopped", result.err.Error()))
			} else {
				mounted.setNodeState(result.id, inspectState("stopped", ""))
			}
		case <-runDone:
			if shutdownDeadline == nil {
				primary = context.Cause(ctx)
				beginCancellation(primary)
			}
			runDone = nil
		case <-shutdownDeadline:
			names := make([]string, 0, len(active))
			for name := range active {
				names = append(names, name)
			}
			sortStrings(names)
			leak := fmt.Errorf("graph shutdown timed out after %s; unresponsive elements: %s",
				mounted.timeout, strings.Join(names, ", "))
			primary = errors.Join(primary, leak)
			remaining = 0
		}
	}
	if shutdownTimer != nil && !shutdownTimer.Stop() {
		select {
		case <-shutdownTimer.C:
		default:
		}
	}
	cancel(primary)
	shutdownErr := mounted.shutdownResources()
	if primary == nil {
		primary = shutdownErr
	} else if shutdownErr != nil {
		primary = errors.Join(primary, shutdownErr)
	}
	mounted.mu.Lock()
	mounted.runErr = primary
	mounted.closed = true
	mounted.mu.Unlock()
	_ = mounted.recorder.finish(mounted)
	close(mounted.done)
	return primary
}

func runMountedNode(runnable interface{ Run(context.Context) error }, ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panicked: %v", recovered)
		}
	}()
	return runnable.Run(ctx)
}

// Close cancels a running graph or disposes a graph that was mounted but never
// run. It waits only within the caller's context.
func (mounted *Mounted) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("close graph: nil context")
	}
	mounted.mu.Lock()
	if mounted.closed {
		done := mounted.done
		started := mounted.started
		shutdownErr := mounted.shutdownErr
		mounted.mu.Unlock()
		if started {
			select {
			case <-done:
				return shutdownErr
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		return shutdownErr
	}
	if !mounted.started {
		mounted.closed = true
		mounted.mu.Unlock()
		err := mounted.shutdownResources()
		mounted.mu.Lock()
		mounted.runErr = ErrGraphClosed
		mounted.mu.Unlock()
		_ = mounted.recorder.finish(mounted)
		close(mounted.done)
		return err
	}
	cancel := mounted.cancel
	done := mounted.done
	mounted.mu.Unlock()
	if cancel != nil {
		cancel(ErrGraphClosed)
	}
	select {
	case <-done:
		mounted.mu.Lock()
		err := mounted.shutdownErr
		mounted.mu.Unlock()
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (mounted *Mounted) Done() <-chan struct{} { return mounted.done }

func (mounted *Mounted) Err() error {
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	return mounted.runErr
}

func (mounted *Mounted) shutdownResources() error {
	mounted.shutdown.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), mounted.timeout)
		defer cancel()
		var failures []error
		// Close ingress routing admission first so cached senders cannot remain
		// blocked while lifecycle-owned producers stop. Egress stays drainable
		// until terminal output queues close below.
		if err := mounted.boundaries.beginQueueShutdown(ctx); err != nil {
			failures = append(failures, fmt.Errorf("close graph boundaries: %w", err))
		}
		for index := len(mounted.nodes) - 1; index >= 0; index-- {
			if err := mounted.nodes[index].scope.close(ctx); err != nil {
				failures = append(failures, err)
			}
		}
		if err := mounted.boundaries.waitIngressShutdown(ctx); err != nil {
			failures = append(failures, fmt.Errorf("wait for graph boundary ingress shutdown: %w", err))
		}
		// Elements own producers. Dispose them while output queues remain
		// drainable, then close channels so consumers can observe EOF after the
		// final lifecycle event rather than losing it.
		queueIDs := make([]string, 0, len(mounted.queues))
		for id := range mounted.queues {
			queueIDs = append(queueIDs, id)
		}
		sortStrings(queueIDs)
		for _, id := range queueIDs {
			mounted.queues[id].close()
		}
		if err := mounted.boundaries.finishQueueShutdown(ctx, true); err != nil {
			failures = append(failures, fmt.Errorf("finish graph boundary shutdown: %w", err))
		}
		mounted.mu.Lock()
		mounted.shutdownErr = errors.Join(failures...)
		mounted.mu.Unlock()
	})
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	return mounted.shutdownErr
}

func (mounted *Mounted) setNodeState(node string, state inspect.NodeLive) {
	mounted.liveMu.Lock()
	if current, found := mounted.nodeLive[node]; found {
		next := current.Clone()
		next.State = state.State
		next.Error = state.Error
		state = next
	}
	mounted.nodeLive[node] = state
	mounted.liveMu.Unlock()
	mounted.recorder.signal()
}

func inspectState(state, failure string) inspect.NodeLive {
	return inspect.NodeLive{State: state, Error: failure}
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor] < values[cursor-1]; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}
