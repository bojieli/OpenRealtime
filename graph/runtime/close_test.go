package runtime_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TestConcurrentGraphCloseWaitsForTheSameCleanup(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "mounted-before-run"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			descriptor := passDescriptor(nil)
			entered, release, started := make(chan struct{}), make(chan struct{}), make(chan struct{})
			failure := errors.New("subscription cleanup failed")
			var disposals atomic.Int32
			registry := graphruntime.NewRegistry()
			if err := registry.Register("", closingPassFactory{
				passFactory: passFactory{descriptor: descriptor}, entered: entered, release: release,
				started: started, failure: failure, disposals: &disposals,
			}); err != nil {
				t.Fatal(err)
			}
			mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: passGraph(t, descriptor), Registry: registry, ShutdownTimeout: 3 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = mounted.Close(ctx)
			})
			egress, err := mounted.Egress("output")
			if err != nil {
				t.Fatal(err)
			}
			if running {
				go func() { _ = mounted.Run(context.Background()) }()
				awaitCloseSignal(t, started, "reaction loop start")
			}
			firstCtx, cancelFirst := context.WithCancel(context.Background())
			defer cancelFirst()
			first := make(chan error, 1)
			go func() { first <- mounted.Close(firstCtx) }()
			awaitCloseSignal(t, entered, "cleanup start")
			// A second caller must join shutdown, even when Run never started.
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := mounted.Close(canceled); !errors.Is(err, context.Canceled) {
				t.Errorf("Close reported completion while cleanup was blocked: %v", err)
			}
			cancelFirst()
			select {
			case err := <-first:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("first caller did not retain its own cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Error("first Close ignored caller cancellation while cleanup remained active")
			}
			select {
			case <-mounted.Done():
				t.Fatal("graph Done closed before resource disposal completed")
			default:
			}
			if err := mounted.Run(context.Background()); !errors.Is(err, graphruntime.ErrAlreadyRunning) {
				t.Fatalf("Run reopened a closing graph: %v", err)
			}
			releaseOnce.Do(func() { close(release) })
			ctx, cancelClose := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelClose()
			for i := 0; i < 2; i++ {
				if err := mounted.Close(ctx); !errors.Is(err, failure) {
					t.Fatalf("Close lost the shared cleanup failure: %v", err)
				}
			}
			awaitCloseSignal(t, mounted.Done(), "graph retirement")
			if disposals.Load() != 1 {
				t.Fatalf("resource disposed %d times", disposals.Load())
			}
			if terminal, err := egress.Receive(ctx); err != nil || terminal.ItemID != "cleanup-terminal" {
				t.Fatalf("cleanup terminal lost across close: %+v, %v", terminal, err)
			}
			if _, err := egress.Receive(ctx); !errors.Is(err, graphruntime.ErrChannelClosed) {
				t.Fatalf("retired egress did not close after its terminal: %v", err)
			}
		})
	}
}

type closingPassFactory struct {
	passFactory
	entered, release, started chan struct{}
	failure                   error
	disposals                 *atomic.Int32
}

func (factory closingPassFactory) Mount(ctx context.Context, mount element.MountContext) (element.Runnable, error) {
	output, err := mount.Ports.Output("out")
	if err != nil {
		return nil, err
	}
	if err := mount.Lifecycle.Defer("subscription", func(ctx context.Context) error {
		factory.disposals.Add(1)
		close(factory.entered)
		select {
		case <-factory.release:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		_, err := output.Broadcast(ctx, element.Envelope{
			Type: element.Event(element.Named("test.Value")), ItemID: "cleanup-terminal", Payload: "closed",
		})
		return errors.Join(factory.failure, err)
	}); err != nil {
		return nil, err
	}
	runnable, err := factory.passFactory.Mount(ctx, mount)
	if err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		close(factory.started)
		return runnable.Run(ctx)
	}), nil
}

func awaitCloseSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}
