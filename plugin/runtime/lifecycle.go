package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

type disposer struct {
	name string
	run  func(context.Context) error
}

type lifecycleScope struct {
	entry  string
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu          sync.Mutex
	disposers   []disposer
	children    []*lifecycleScope
	disposed    map[string]struct{}
	workers     map[string]struct{}
	workerErr   map[string]error
	workersDone chan struct{}
	closed      bool
	onFailure   func(error)
	failOnce    sync.Once
}

func newLifecycleScope(parent context.Context, entry string, onFailure func(error)) *lifecycleScope {
	ctx, cancel := context.WithCancelCause(parent)
	return &lifecycleScope{
		entry: entry, ctx: ctx, cancel: cancel,
		disposed: make(map[string]struct{}), workers: make(map[string]struct{}),
		workerErr: make(map[string]error), onFailure: onFailure,
	}
}

func (scope *lifecycleScope) Defer(name string, dispose func(context.Context) error) error {
	if name == "" || dispose == nil {
		return fmt.Errorf("plugin %s lifecycle disposer requires a name and function", scope.entry)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed {
		return fmt.Errorf("plugin %s lifecycle is closed", scope.entry)
	}
	if _, duplicate := scope.disposed[name]; duplicate {
		return fmt.Errorf("plugin %s repeats lifecycle effect %q", scope.entry, name)
	}
	scope.disposed[name] = struct{}{}
	scope.disposers = append(scope.disposers, disposer{name: name, run: dispose})
	return nil
}

// adopt transfers a prepared child lifecycle into a live entry without
// consuming the plugin-visible disposer namespace.
func (scope *lifecycleScope) adopt(child *lifecycleScope) error {
	if child == nil || child == scope {
		return fmt.Errorf("plugin %s cannot adopt an invalid child lifecycle", scope.entry)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed {
		return fmt.Errorf("plugin %s lifecycle is closed", scope.entry)
	}
	for _, existing := range scope.children {
		if existing == child {
			return fmt.Errorf("plugin %s already adopted candidate lifecycle", scope.entry)
		}
	}
	scope.children = append(scope.children, child)
	return nil
}

func (scope *lifecycleScope) Go(name string, worker func(context.Context) error) error {
	if name == "" || worker == nil {
		return fmt.Errorf("plugin %s lifecycle worker requires a name and function", scope.entry)
	}
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return fmt.Errorf("plugin %s lifecycle is closed", scope.entry)
	}
	if _, duplicate := scope.workers[name]; duplicate {
		scope.mu.Unlock()
		return fmt.Errorf("plugin %s repeats lifecycle worker %q", scope.entry, name)
	}
	if len(scope.workers) == 0 {
		scope.workersDone = make(chan struct{})
	}
	scope.workers[name] = struct{}{}
	scope.mu.Unlock()
	go func() {
		var failure error
		if err := worker(scope.ctx); err != nil && !errors.Is(err, context.Canceled) {
			scope.mu.Lock()
			scope.workerErr[name] = err
			scope.mu.Unlock()
			failure = fmt.Errorf("worker %s: %w", name, err)
		}
		scope.mu.Lock()
		delete(scope.workers, name)
		if len(scope.workers) == 0 && scope.workersDone != nil {
			close(scope.workersDone)
			scope.workersDone = nil
		}
		scope.mu.Unlock()
		if failure != nil && scope.onFailure != nil {
			scope.failOnce.Do(func() { scope.onFailure(failure) })
		}
	}()
	return nil
}

func (scope *lifecycleScope) close(ctx context.Context, cause error) error {
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return nil
	}
	scope.closed = true
	disposers := append([]disposer(nil), scope.disposers...)
	scope.disposers = nil
	children := append([]*lifecycleScope(nil), scope.children...)
	scope.children = nil
	workersDone := scope.workersDone
	scope.mu.Unlock()
	scope.cancel(cause)

	var failures []error
	if workersDone != nil {
		select {
		case <-workersDone:
		case <-ctx.Done():
			scope.mu.Lock()
			names := make([]string, 0, len(scope.workers))
			for name := range scope.workers {
				names = append(names, name)
			}
			scope.mu.Unlock()
			sort.Strings(names)
			failures = append(failures, fmt.Errorf("plugin %s shutdown timed out; live workers: %v",
				scope.entry, names))
		}
	}
	for index := len(disposers) - 1; index >= 0; index-- {
		if err := disposers[index].run(ctx); err != nil {
			failures = append(failures, fmt.Errorf("plugin %s dispose %s: %w",
				scope.entry, disposers[index].name, err))
		}
	}
	for index := len(children) - 1; index >= 0; index-- {
		if err := children[index].close(ctx, cause); err != nil {
			failures = append(failures, fmt.Errorf("plugin %s dispose candidate lifecycle: %w",
				scope.entry, err))
		}
	}
	scope.mu.Lock()
	workerNames := make([]string, 0, len(scope.workerErr))
	for name := range scope.workerErr {
		workerNames = append(workerNames, name)
	}
	sort.Strings(workerNames)
	for _, name := range workerNames {
		failures = append(failures, fmt.Errorf("plugin %s worker %s: %w",
			scope.entry, name, scope.workerErr[name]))
	}
	scope.mu.Unlock()
	return errors.Join(failures...)
}

func (scope *lifecycleScope) counts() (workers, effects int) {
	if scope == nil {
		return 0, 0
	}
	scope.mu.Lock()
	workers = len(scope.workers)
	effects = len(scope.disposers)
	children := append([]*lifecycleScope(nil), scope.children...)
	scope.mu.Unlock()
	for _, child := range children {
		childWorkers, childEffects := child.counts()
		workers += childWorkers
		effects += childEffects
	}
	return workers, effects
}
