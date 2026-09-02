package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLifecycleDoCancellationAndJoin(t *testing.T) {
	scope := newLifecycleScope(context.Background(), "caller-owner", nil)
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
			return context.Cause(ctx)
		})
	}()
	waitLifecycleSignal(t, started, "caller-owned work did not start")
	if got := scope.state(); got.workers != 1 || !reflect.DeepEqual(got.workerNames, []string{"request-1"}) {
		t.Fatalf("active lifecycle state = %+v", got)
	}

	closeResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		closeResult <- scope.close(ctx)
	}()
	waitLifecycleSignal(t, canceled, "lifecycle close did not cancel caller-owned work")
	select {
	case err := <-closeResult:
		t.Fatalf("lifecycle close returned before caller-owned work joined: %v", err)
	default:
	}
	close(release)
	if err := waitLifecycleResult(t, workResult, "caller-owned work did not return"); !errors.Is(err, ErrGraphClosed) {
		t.Fatalf("Do() cancellation = %v, want %v", err, ErrGraphClosed)
	}
	if err := waitLifecycleResult(t, closeResult, "lifecycle close did not finish"); err != nil {
		t.Fatal(err)
	}
	if got := scope.state(); !got.closed || got.workers != 0 || got.effects != 0 {
		t.Fatalf("closed lifecycle retained ownership = %+v", got)
	}
}

func TestLifecycleGoInheritsRealmCancellation(t *testing.T) {
	realm, cancelRealm := context.WithCancelCause(context.Background())
	scope := newLifecycleScope(realm, "background-owner", nil)
	started := make(chan struct{})
	observed := make(chan error, 1)
	if err := scope.Go("reader", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		observed <- context.Cause(ctx)
		return context.Cause(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	waitLifecycleSignal(t, started, "background worker did not start")
	realmFailure := errors.New("realm retired")
	cancelRealm(realmFailure)
	if got := waitLifecycleResult(t, observed, "worker did not inherit realm cancellation"); !errors.Is(got, realmFailure) {
		t.Fatalf("worker cancellation cause = %v, want %v", got, realmFailure)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scope.close(ctx); err != nil {
		t.Fatalf("realm cancellation was reported as worker failure: %v", err)
	}
}

func TestLifecycleRejectsDuplicateNames(t *testing.T) {
	scope := newLifecycleScope(context.Background(), "duplicates", nil)
	if err := scope.Defer("subscription", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := scope.Defer("subscription", func(context.Context) error { return nil }); err == nil ||
		!strings.Contains(err.Error(), `repeats lifecycle effect "subscription"`) {
		t.Fatalf("duplicate disposer error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	if err := scope.Go("reader", func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitLifecycleSignal(t, started, "worker did not start")
	var duplicateRan atomic.Bool
	if err := scope.Do("reader", func(context.Context) error {
		duplicateRan.Store(true)
		return nil
	}); err == nil || !strings.Contains(err.Error(), `repeats lifecycle worker "reader"`) {
		t.Fatalf("duplicate worker error = %v", err)
	}
	if duplicateRan.Load() {
		t.Fatal("duplicate worker callback ran")
	}
	close(release)
	waitLifecycleOwnership(t, scope, 0, 1)
	if err := scope.Go("reader", func(context.Context) error {
		duplicateRan.Store(true)
		return nil
	}); err == nil || !strings.Contains(err.Error(), `repeats lifecycle worker "reader"`) {
		t.Fatalf("reused worker name error = %v", err)
	}
	if duplicateRan.Load() {
		t.Fatal("reused worker callback ran")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scope.close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleReportsWorkerFailuresDeterministically(t *testing.T) {
	callback := make(chan error, 1)
	scope := newLifecycleScope(context.Background(), "failures", func(err error) {
		callback <- err
	})
	zetaRelease := make(chan struct{})
	alphaRelease := make(chan struct{})
	zetaFailure := errors.New("zeta failed")
	alphaFailure := errors.New("alpha failed")
	if err := scope.Go("zeta", func(context.Context) error {
		<-zetaRelease
		return zetaFailure
	}); err != nil {
		t.Fatal(err)
	}
	if err := scope.Go("alpha", func(context.Context) error {
		<-alphaRelease
		return alphaFailure
	}); err != nil {
		t.Fatal(err)
	}
	close(zetaRelease)
	callbackErr := waitLifecycleResult(t, callback, "worker failure callback did not run")
	if !errors.Is(callbackErr, zetaFailure) || !strings.Contains(callbackErr.Error(), "element failures worker zeta") {
		t.Fatalf("worker failure callback = %v", callbackErr)
	}
	close(alphaRelease)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := scope.close(ctx)
	if !errors.Is(err, alphaFailure) || !errors.Is(err, zetaFailure) {
		t.Fatalf("close worker errors = %v", err)
	}
	alphaIndex := strings.Index(err.Error(), "worker alpha")
	zetaIndex := strings.Index(err.Error(), "worker zeta")
	if alphaIndex < 0 || zetaIndex < 0 || alphaIndex >= zetaIndex {
		t.Fatalf("worker failures are not name-sorted: %v", err)
	}
	select {
	case extra := <-callback:
		t.Fatalf("failure callback ran more than once: %v", extra)
	default:
	}
}

func TestLifecycleContainsWorkerPanics(t *testing.T) {
	callback := make(chan error, 2)
	scope := newLifecycleScope(context.Background(), "panic-owner", func(err error) {
		callback <- err
	})
	doErr := scope.Do("caller", func(context.Context) error {
		panic("caller panic")
	})
	if doErr == nil || !strings.Contains(doErr.Error(), "panicked: caller panic") {
		t.Fatalf("Do panic error = %v", doErr)
	}
	callbackErr := waitLifecycleResult(t, callback, "Do panic did not reach supervisor")
	if !strings.Contains(callbackErr.Error(), "worker caller: panicked: caller panic") {
		t.Fatalf("Do panic callback = %v", callbackErr)
	}

	// A panic in Go must become an ordinary supervised failure rather than
	// escaping the goroutine and terminating the process.
	if err := scope.Go("background", func(context.Context) error {
		panic("background panic")
	}); err != nil {
		t.Fatal(err)
	}
	waitLifecycleOwnership(t, scope, 0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := scope.close(ctx)
	if err == nil || !strings.Contains(err.Error(), "worker background: panicked: background panic") ||
		!strings.Contains(err.Error(), "worker caller: panicked: caller panic") {
		t.Fatalf("contained panic audit = %v", err)
	}
}

func TestLifecycleCloseIsIdempotentAndReverseOrdered(t *testing.T) {
	scope := newLifecycleScope(context.Background(), "idempotent", nil)
	var order []string
	if err := scope.Defer("first", func(context.Context) error {
		order = append(order, "first")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	disposeStarted := make(chan struct{})
	disposeRelease := make(chan struct{})
	disposeFailure := errors.New("dispose failed")
	if err := scope.Defer("second", func(context.Context) error {
		close(disposeStarted)
		<-disposeRelease
		order = append(order, "second")
		return disposeFailure
	}); err != nil {
		t.Fatal(err)
	}
	if got := scope.state(); got.effects != 2 ||
		!reflect.DeepEqual(got.effectNames, []string{"first", "second"}) {
		t.Fatalf("registered effects = %+v", got)
	}

	firstClose := make(chan error, 1)
	secondClose := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		firstClose <- scope.close(ctx)
	}()
	waitLifecycleSignal(t, disposeStarted, "reverse-order disposer did not start")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		secondClose <- scope.close(ctx)
	}()
	select {
	case err := <-secondClose:
		t.Fatalf("idempotent close did not join the in-progress close: %v", err)
	default:
	}
	close(disposeRelease)
	firstErr := waitLifecycleResult(t, firstClose, "first close did not finish")
	secondErr := waitLifecycleResult(t, secondClose, "second close did not finish")
	if !errors.Is(firstErr, disposeFailure) || !errors.Is(secondErr, disposeFailure) ||
		firstErr.Error() != secondErr.Error() {
		t.Fatalf("idempotent close results = first %v, second %v", firstErr, secondErr)
	}
	if !reflect.DeepEqual(order, []string{"second", "first"}) {
		t.Fatalf("dispose order = %v", order)
	}
	if got := scope.state(); !got.closed || got.effects != 0 || got.workers != 0 {
		t.Fatalf("closed lifecycle state = %+v", got)
	}
}

func TestLifecycleContainsDisposerPanicAndContinuesUnwind(t *testing.T) {
	scope := newLifecycleScope(context.Background(), "panic-disposer", nil)
	var firstRan atomic.Bool
	if err := scope.Defer("first", func(context.Context) error {
		firstRan.Store(true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := scope.Defer("panic", func(context.Context) error {
		panic("dispose panic")
	}); err != nil {
		t.Fatal(err)
	}

	err := scope.close(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dispose panic: panicked: dispose panic") {
		t.Fatalf("contained disposer error = %v", err)
	}
	if !firstRan.Load() {
		t.Fatal("disposer panic prevented the remaining reverse unwind")
	}
	if got := scope.state(); !got.closed || got.effects != 0 || got.workers != 0 {
		t.Fatalf("panicking disposer retained lifecycle ownership = %+v", got)
	}
	if repeated := scope.close(context.Background()); repeated == nil || repeated.Error() != err.Error() {
		t.Fatalf("repeated close after disposer panic = %v, want %v", repeated, err)
	}
}

func TestLifecycleRejectsRegistrationAfterClose(t *testing.T) {
	scope := newLifecycleScope(context.Background(), "closed", nil)
	if err := scope.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var invoked atomic.Int32
	registrations := []struct {
		name string
		run  func() error
	}{
		{name: "Defer", run: func() error {
			return scope.Defer("late-effect", func(context.Context) error {
				invoked.Add(1)
				return nil
			})
		}},
		{name: "Do", run: func() error {
			return scope.Do("late-do", func(context.Context) error {
				invoked.Add(1)
				return nil
			})
		}},
		{name: "Go", run: func() error {
			return scope.Go("late-go", func(context.Context) error {
				invoked.Add(1)
				return nil
			})
		}},
	}
	for _, registration := range registrations {
		t.Run(registration.name, func(t *testing.T) {
			if err := registration.run(); err == nil || !strings.Contains(err.Error(), "lifecycle is closed") {
				t.Fatalf("registration error = %v", err)
			}
		})
	}
	if got := invoked.Load(); got != 0 {
		t.Fatalf("late lifecycle callbacks invoked = %d", got)
	}
}

func TestLifecycleAuditsUnresponsiveWorkers(t *testing.T) {
	scope := newLifecycleScope(context.Background(), "stubborn", nil)
	started := make(chan struct{}, 2)
	canceled := make(chan struct{}, 2)
	release := make(chan struct{})
	for _, name := range []string{"zeta", "alpha"} {
		if err := scope.Go(name, func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			canceled <- struct{}{}
			<-release
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitLifecycleSignal(t, started, "first stubborn worker did not start")
	waitLifecycleSignal(t, started, "second stubborn worker did not start")
	disposed := make(chan struct{}, 1)
	if err := scope.Defer("subscription", func(context.Context) error {
		disposed <- struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	closeLimit := errors.New("shutdown bound reached")
	closeContext, cancelClose := context.WithCancelCause(context.Background())
	closeResult := make(chan error, 1)
	go func() { closeResult <- scope.close(closeContext) }()
	waitLifecycleSignal(t, canceled, "first stubborn worker was not canceled")
	waitLifecycleSignal(t, canceled, "second stubborn worker was not canceled")
	cancelClose(closeLimit)
	err := waitLifecycleResult(t, closeResult, "bounded close did not return")
	if !errors.Is(err, closeLimit) || !strings.Contains(err.Error(), "live workers: [alpha zeta]") {
		t.Fatalf("unresponsive worker audit = %v", err)
	}
	waitLifecycleSignal(t, disposed, "registered effects were not disposed after worker timeout")
	state := scope.state()
	if !state.closed || state.workers != 2 || state.effects != 0 ||
		!reflect.DeepEqual(state.workerNames, []string{"alpha", "zeta"}) {
		t.Fatalf("unresponsive lifecycle state = %+v", state)
	}
	workers, effects := scope.counts()
	if workers != 2 || effects != 0 {
		t.Fatalf("unresponsive lifecycle counts = workers %d effects %d", workers, effects)
	}

	close(release)
	waitLifecycleOwnership(t, scope, 0, 0)
	if repeated := scope.close(context.Background()); repeated == nil || repeated.Error() != err.Error() {
		t.Fatalf("repeated close audit = %v, want %v", repeated, err)
	}
}

func waitLifecycleSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}

func waitLifecycleResult(t *testing.T, result <-chan error, failure string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal(failure)
		return nil
	}
}

func waitLifecycleOwnership(t *testing.T, scope *lifecycleScope, workers, effects int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		gotWorkers, gotEffects := scope.counts()
		if gotWorkers == workers && gotEffects == effects {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lifecycle ownership = workers %d effects %d, want workers %d effects %d",
				gotWorkers, gotEffects, workers, effects)
		}
		time.Sleep(time.Millisecond)
	}
}
