package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLifecycleDoCancelsAndJoinsCallerOwnedWork(t *testing.T) {
	scope := newLifecycleScope(context.Background(), "request-owner", nil)
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	workResult := make(chan error, 1)
	go func() {
		workResult <- scope.Do("request-1", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		})
	}()
	waitLifecycleTestSignal(t, started, "caller-owned work did not start")
	if workers, effects := scope.counts(); workers != 1 || effects != 0 {
		t.Fatalf("active caller-owned lifecycle = workers %d effects %d", workers, effects)
	}

	closeResult := make(chan error, 1)
	go func() {
		closeContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		closeResult <- scope.close(closeContext, errors.New("test retirement"))
	}()
	waitLifecycleTestSignal(t, canceled, "lifecycle close did not cancel caller-owned work")
	select {
	case err := <-closeResult:
		t.Fatalf("lifecycle close returned before caller-owned work joined: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := waitLifecycleTestResult(t, workResult, "caller-owned work did not return"); err != nil {
		t.Fatal(err)
	}
	if err := waitLifecycleTestResult(t, closeResult, "lifecycle close did not finish"); err != nil {
		t.Fatal(err)
	}
	state := scope.state()
	if !state.closed || state.workers != 0 || state.effects != 0 || state.children != 0 {
		t.Fatalf("closed caller-owned lifecycle retained ownership = %+v", state)
	}
}

func waitLifecycleTestSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}

func waitLifecycleTestResult(t *testing.T, result <-chan error, failure string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal(failure)
		return nil
	}
}
