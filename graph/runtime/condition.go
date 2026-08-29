package runtime

import "sync"

// condition is an edge-triggered generation channel. Callers obtain current
// while holding the state lock that protects the condition they checked;
// signal after changing that state. This avoids lost wake-ups without one
// goroutine per blocked channel operation.
type condition struct {
	mu sync.Mutex
	ch chan struct{}
}

func newCondition() *condition { return &condition{ch: make(chan struct{})} }

func (condition *condition) current() <-chan struct{} {
	condition.mu.Lock()
	defer condition.mu.Unlock()
	return condition.ch
}

func (condition *condition) signal() {
	condition.mu.Lock()
	close(condition.ch)
	condition.ch = make(chan struct{})
	condition.mu.Unlock()
}
