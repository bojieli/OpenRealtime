package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type disposer struct {
	name    string
	dispose func(context.Context) error
}

type lifecycleScope struct {
	instance string

	mu        sync.Mutex
	disposers []disposer
	closed    bool
}

func (scope *lifecycleScope) Defer(name string, dispose func(context.Context) error) error {
	if name == "" || dispose == nil {
		return fmt.Errorf("element %s lifecycle disposer requires a name and function", scope.instance)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed {
		return fmt.Errorf("element %s lifecycle is already closed", scope.instance)
	}
	scope.disposers = append(scope.disposers, disposer{name: name, dispose: dispose})
	return nil
}

func (scope *lifecycleScope) close(ctx context.Context) error {
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return nil
	}
	scope.closed = true
	disposers := append([]disposer(nil), scope.disposers...)
	scope.disposers = nil
	scope.mu.Unlock()

	var failures []error
	for index := len(disposers) - 1; index >= 0; index-- {
		if err := disposers[index].dispose(ctx); err != nil {
			failures = append(failures, fmt.Errorf("element %s dispose %s: %w",
				scope.instance, disposers[index].name, err))
		}
	}
	return errors.Join(failures...)
}
