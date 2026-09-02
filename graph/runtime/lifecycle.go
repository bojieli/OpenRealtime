package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

type disposer struct {
	name    string
	dispose func(context.Context) error
}

type lifecycleScope struct {
	instance  string
	ctx       context.Context
	cancel    context.CancelCauseFunc
	onFailure func(error)
	failOnce  sync.Once

	mu          sync.Mutex
	disposers   []disposer
	effects     map[string]struct{}
	workerNames map[string]struct{}
	workers     map[string]struct{}
	workerErr   map[string]error
	workersDone chan struct{}
	closed      bool
	closeDone   chan struct{}
	closeErr    error
}

// lifecycleScopeState is an exact, payload-free ownership snapshot. The
// sorted names make a failed retirement actionable while the counts are kept
// explicit for later reconciliation receipts.
type lifecycleScopeState struct {
	closed      bool
	workers     int
	effects     int
	workerNames []string
	effectNames []string
}

func newLifecycleScope(
	parent context.Context, instance string, onFailure func(error),
) *lifecycleScope {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancelCause(parent)
	return &lifecycleScope{
		instance: instance, ctx: ctx, cancel: cancel, onFailure: onFailure,
		effects: make(map[string]struct{}), workerNames: make(map[string]struct{}),
		workers: make(map[string]struct{}), workerErr: make(map[string]error),
	}
}

// initializeLocked preserves the historical direct lifecycleScope
// construction used by graph mounting. New reconciliation code should use
// newLifecycleScope so the element scope inherits its realm context.
func (scope *lifecycleScope) initializeLocked() {
	if scope.ctx == nil {
		scope.ctx, scope.cancel = context.WithCancelCause(context.Background())
	}
	if scope.effects == nil {
		scope.effects = make(map[string]struct{})
	}
	if scope.workerNames == nil {
		scope.workerNames = make(map[string]struct{})
	}
	if scope.workers == nil {
		scope.workers = make(map[string]struct{})
	}
	if scope.workerErr == nil {
		scope.workerErr = make(map[string]error)
	}
}

func (scope *lifecycleScope) Defer(name string, dispose func(context.Context) error) error {
	if name == "" || dispose == nil {
		return fmt.Errorf("element %s lifecycle disposer requires a name and function", scope.instance)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.initializeLocked()
	if scope.closed {
		return fmt.Errorf("element %s lifecycle is closed", scope.instance)
	}
	if _, duplicate := scope.effects[name]; duplicate {
		return fmt.Errorf("element %s repeats lifecycle effect %q", scope.instance, name)
	}
	scope.effects[name] = struct{}{}
	scope.disposers = append(scope.disposers, disposer{name: name, dispose: dispose})
	return nil
}

// Do executes caller-blocking work under this lifecycle's cancellation scope.
// Close will wait, within its caller-provided bound, for the work to return.
func (scope *lifecycleScope) Do(name string, work func(context.Context) error) error {
	ctx, err := scope.beginWorker(name, work)
	if err != nil {
		return err
	}
	err = runLifecycleWorker(work, ctx)
	scope.finishWorker(name, err)
	return err
}

// Go starts one lifecycle-owned background worker. The worker is registered
// before the goroutine can run, so close can never miss admitted work.
func (scope *lifecycleScope) Go(name string, worker func(context.Context) error) error {
	ctx, err := scope.beginWorker(name, worker)
	if err != nil {
		return err
	}
	go func() {
		scope.finishWorker(name, runLifecycleWorker(worker, ctx))
	}()
	return nil
}

func runLifecycleWorker(worker func(context.Context) error, ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panicked: %v", recovered)
		}
	}()
	return worker(ctx)
}

func (scope *lifecycleScope) beginWorker(
	name string, worker func(context.Context) error,
) (context.Context, error) {
	if name == "" || worker == nil {
		return nil, fmt.Errorf("element %s lifecycle worker requires a name and function", scope.instance)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.initializeLocked()
	if scope.closed {
		return nil, fmt.Errorf("element %s lifecycle is closed", scope.instance)
	}
	if _, duplicate := scope.workerNames[name]; duplicate {
		return nil, fmt.Errorf("element %s repeats lifecycle worker %q", scope.instance, name)
	}
	if len(scope.workers) == 0 {
		scope.workersDone = make(chan struct{})
	}
	scope.workerNames[name] = struct{}{}
	scope.workers[name] = struct{}{}
	return scope.ctx, nil
}

func (scope *lifecycleScope) finishWorker(name string, err error) {
	var failure error
	scope.mu.Lock()
	if !scope.isCancellationLocked(err) {
		scope.workerErr[name] = err
		failure = fmt.Errorf("element %s worker %s: %w", scope.instance, name, err)
	}
	delete(scope.workers, name)
	if len(scope.workers) == 0 && scope.workersDone != nil {
		close(scope.workersDone)
		scope.workersDone = nil
	}
	scope.mu.Unlock()

	if failure != nil && scope.onFailure != nil {
		scope.failOnce.Do(func() { scope.onFailure(failure) })
	}
}

func (scope *lifecycleScope) isCancellationLocked(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return true
	}
	cause := context.Cause(scope.ctx)
	return cause != nil && errors.Is(err, cause)
}

func (scope *lifecycleScope) close(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("element %s lifecycle close requires a context", scope.instance)
	}
	scope.mu.Lock()
	scope.initializeLocked()
	if scope.closed {
		done := scope.closeDone
		scope.mu.Unlock()
		return scope.waitForClose(ctx, done)
	}
	scope.closed = true
	scope.closeDone = make(chan struct{})
	done := scope.closeDone
	disposers := append([]disposer(nil), scope.disposers...)
	scope.disposers = nil
	workersDone := scope.workersDone
	scope.mu.Unlock()
	scope.cancel(ErrGraphClosed)

	var failures []error
	if workersDone != nil {
		select {
		case <-workersDone:
		case <-ctx.Done():
			// Prefer a completed join if cancellation and the last worker race.
			select {
			case <-workersDone:
			default:
				names := scope.liveWorkerNames()
				failures = append(failures, fmt.Errorf(
					"element %s shutdown timed out; live workers: %v: %w",
					scope.instance, names, context.Cause(ctx),
				))
			}
		}
	}
	for index := len(disposers) - 1; index >= 0; index-- {
		if err := runLifecycleDisposer(disposers[index].dispose, ctx); err != nil {
			failures = append(failures, fmt.Errorf("element %s dispose %s: %w",
				scope.instance, disposers[index].name, err))
		}
		scope.mu.Lock()
		delete(scope.effects, disposers[index].name)
		scope.mu.Unlock()
	}

	scope.mu.Lock()
	workerNames := make([]string, 0, len(scope.workerErr))
	for name := range scope.workerErr {
		workerNames = append(workerNames, name)
	}
	sort.Strings(workerNames)
	for _, name := range workerNames {
		failures = append(failures, fmt.Errorf("element %s worker %s: %w",
			scope.instance, name, scope.workerErr[name]))
	}
	scope.closeErr = errors.Join(failures...)
	result := scope.closeErr
	close(done)
	scope.mu.Unlock()
	return result
}

func runLifecycleDisposer(
	dispose func(context.Context) error, ctx context.Context,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panicked: %v", recovered)
		}
	}()
	return dispose(ctx)
}

func (scope *lifecycleScope) waitForClose(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		scope.mu.Lock()
		result := scope.closeErr
		scope.mu.Unlock()
		return result
	}
	select {
	case <-done:
		scope.mu.Lock()
		result := scope.closeErr
		scope.mu.Unlock()
		return result
	default:
	}
	select {
	case <-done:
		scope.mu.Lock()
		result := scope.closeErr
		scope.mu.Unlock()
		return result
	case <-ctx.Done():
		return fmt.Errorf("element %s lifecycle close still in progress: %w",
			scope.instance, context.Cause(ctx))
	}
}

func (scope *lifecycleScope) liveWorkerNames() []string {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	names := make([]string, 0, len(scope.workers))
	for name := range scope.workers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (scope *lifecycleScope) counts() (workers, effects int) {
	state := scope.state()
	return state.workers, state.effects
}

func (scope *lifecycleScope) state() lifecycleScopeState {
	if scope == nil {
		return lifecycleScopeState{}
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.initializeLocked()
	workerNames := make([]string, 0, len(scope.workers))
	for name := range scope.workers {
		workerNames = append(workerNames, name)
	}
	effectNames := make([]string, 0, len(scope.effects))
	for name := range scope.effects {
		effectNames = append(effectNames, name)
	}
	sort.Strings(workerNames)
	sort.Strings(effectNames)
	return lifecycleScopeState{
		closed: scope.closed, workers: len(workerNames), effects: len(effectNames),
		workerNames: workerNames, effectNames: effectNames,
	}
}
