package clock

import (
	"errors"
	"math"
	"sync"
	"testing"
)

func TestVirtualRejectsBackwardTime(t *testing.T) {
	t.Parallel()
	clock := NewVirtual(10)
	if err := clock.AdvanceToNS(9); !errors.Is(err, ErrBackwards) {
		t.Fatalf("AdvanceToNS() error = %v, want ErrBackwards", err)
	}
	if got := clock.NowNS(); got != 10 {
		t.Fatalf("NowNS() = %d, want 10", got)
	}
}

func TestVirtualConcurrentAdvanceIsMonotonic(t *testing.T) {
	t.Parallel()
	clock := NewVirtual(0)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 1_000 {
				if _, err := clock.AdvanceNS(1); err != nil {
					t.Errorf("AdvanceNS() error = %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
	if got := clock.NowNS(); got != 8_000 {
		t.Fatalf("NowNS() = %d, want 8000", got)
	}
}

func TestVirtualRejectsOverflow(t *testing.T) {
	t.Parallel()
	clock := NewVirtual(math.MaxUint64)
	if _, err := clock.AdvanceNS(1); err == nil {
		t.Fatal("AdvanceNS() error = nil, want overflow")
	}
}
