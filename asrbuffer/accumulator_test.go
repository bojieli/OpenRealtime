package asrbuffer

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// slowProvider takes a declared amount of wall-clock time per advance, which
// is what makes the elapsed counters assertable rather than merely non-zero.
type slowProvider struct {
	advance  time.Duration
	finalize time.Duration
}

func (provider *slowProvider) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "slow", Version: "1"}
}

func (provider *slowProvider) PushFrame(_ context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	time.Sleep(provider.advance)
	return []v1.PerceptionRevision{{RevisionID: 1, SourceSample: frame.SampleOffset, UnstableText: "partial"}}, nil
}

func (provider *slowProvider) Finalize(_ context.Context, sourceSample uint64) (v1.PerceptionRevision, error) {
	time.Sleep(provider.finalize)
	return v1.PerceptionRevision{RevisionID: 2, SourceSample: sourceSample, StableText: "done", Final: true}, nil
}

// runUtterance drives one buffer through advances and a finalisation, which is
// the shape of every real utterance.
func runUtterance(t *testing.T, accumulator *Accumulator, provider v1.PerceptionProvider, advances uint64) *Buffer {
	t.Helper()
	buffer, err := accumulator.New(Config{Provider: provider, MinimumChunk: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for index := uint64(0); index < advances*4; index++ {
		if _, err := buffer.PushFrame(context.Background(), newFrame(index, index*800, 800)); err != nil {
			t.Fatal(err)
		}
	}
	return buffer
}

func TestAccumulatorFoldsAnUtteranceWhenItCloses(t *testing.T) {
	t.Parallel()
	accumulator := NewAccumulator()
	if snapshot := accumulator.Snapshot(); snapshot.Utterances != 0 || snapshot.AdvanceInvocations != 0 {
		t.Fatalf("a fresh accumulator is not empty: %+v", snapshot)
	}

	buffer := runUtterance(t, accumulator, &slowProvider{advance: 2 * time.Millisecond}, 3)
	if snapshot := accumulator.Snapshot(); snapshot.Utterances != 0 || snapshot.InFlight != 1 {
		t.Fatalf("an open utterance should be in flight, not folded: %+v", snapshot)
	}
	if _, err := buffer.Finalize(context.Background(), 9600); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot := accumulator.Snapshot()
	if snapshot.Utterances != 1 || snapshot.InFlight != 0 {
		t.Fatalf("close should fold exactly one utterance: %+v", snapshot)
	}
	if snapshot.AdvanceInvocations != 3 {
		t.Fatalf("advance invocations = %d, want 3", snapshot.AdvanceInvocations)
	}
	if snapshot.FinalizeInvocations != 1 {
		t.Fatalf("finalize invocations = %d, want 1", snapshot.FinalizeInvocations)
	}
	if snapshot.AdvanceElapsedNS == 0 || snapshot.AdvanceMaxElapsedNS == 0 {
		t.Fatalf("timing should survive the fold: %+v", snapshot)
	}
	if snapshot.MeanAdvanceNS() < uint64(time.Millisecond) {
		t.Fatalf("mean advance %d ns is below the provider's own delay", snapshot.MeanAdvanceNS())
	}
}

// The counters exist to be read while the recogniser is degrading, and a
// degrading recogniser is one whose current call has not returned yet. A total
// that only moved at the endpoint would go quiet during the stall it reports.
func TestAccumulatorCountsUtterancesStillOpen(t *testing.T) {
	t.Parallel()
	accumulator := NewAccumulator()
	runUtterance(t, accumulator, &slowProvider{advance: time.Millisecond}, 2)
	runUtterance(t, accumulator, &slowProvider{advance: time.Millisecond}, 3)

	snapshot := accumulator.Snapshot()
	if snapshot.Utterances != 0 {
		t.Fatalf("nothing has closed, so nothing is folded: %+v", snapshot)
	}
	if snapshot.InFlight != 2 {
		t.Fatalf("in flight = %d, want 2", snapshot.InFlight)
	}
	if snapshot.AdvanceInvocations != 5 {
		t.Fatalf("open utterances should still be totalled: got %d, want 5", snapshot.AdvanceInvocations)
	}
}

func TestAccumulatorCountsAnUtteranceOnceAcrossRepeatedCloses(t *testing.T) {
	t.Parallel()
	accumulator := NewAccumulator()
	buffer := runUtterance(t, accumulator, &slowProvider{advance: time.Millisecond}, 2)
	for range 3 {
		if err := buffer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := accumulator.Snapshot()
	if snapshot.Utterances != 1 || snapshot.AdvanceInvocations != 2 {
		t.Fatalf("three closes folded more than one utterance: %+v", snapshot)
	}
}

// A maximum is not additive. Summing two peaks would report a latency no
// single call ever took, which is exactly the number an operator would page on.
func TestAccumulatorTakesTheLargestPeakRatherThanTheirSum(t *testing.T) {
	t.Parallel()
	accumulator := NewAccumulator()
	for _, advance := range []time.Duration{2 * time.Millisecond, 20 * time.Millisecond, time.Millisecond} {
		buffer := runUtterance(t, accumulator, &slowProvider{advance: advance}, 1)
		if err := buffer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := accumulator.Snapshot()
	if snapshot.Utterances != 3 {
		t.Fatalf("utterances = %d, want 3", snapshot.Utterances)
	}
	peak, total := snapshot.AdvanceMaxElapsedNS, snapshot.AdvanceElapsedNS
	if peak >= total {
		t.Fatalf("peak %d should be one call, not the sum %d", peak, total)
	}
	if peak < uint64(15*time.Millisecond) {
		t.Fatalf("peak %d ns lost the slowest utterance", peak)
	}
}

// The fold takes the buffer's lock and then the accumulator's; a snapshot must
// never hold the accumulator's while reaching into a buffer, or the two orders
// meet. Under -race this also asserts the totals are not torn.
func TestAccumulatorSnapshotsWhileUtterancesOpenAndClose(t *testing.T) {
	t.Parallel()
	accumulator := NewAccumulator()
	const utterances = 24

	var waiting, reading sync.WaitGroup
	done := make(chan struct{})
	reading.Add(1)
	go func() {
		defer reading.Done()
		for {
			select {
			case <-done:
				return
			default:
				accumulator.Snapshot()
			}
		}
	}()

	for range utterances {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			buffer := runUtterance(t, accumulator, &slowProvider{}, 2)
			if err := buffer.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	waiting.Wait()
	close(done)
	reading.Wait()

	snapshot := accumulator.Snapshot()
	if snapshot.Utterances != utterances || snapshot.InFlight != 0 {
		t.Fatalf("utterances = %d in flight = %d, want %d and 0", snapshot.Utterances, snapshot.InFlight, utterances)
	}
	if snapshot.AdvanceInvocations != utterances*2 {
		t.Fatalf("advance invocations = %d, want %d", snapshot.AdvanceInvocations, utterances*2)
	}
}

// A benchmark builds a buffer to read that one buffer's numbers, so the plain
// constructor must not enrol it in anything.
func TestAnUntrackedBufferFoldsIntoNothing(t *testing.T) {
	t.Parallel()
	buffer, err := New(Config{Provider: &slowProvider{}, MinimumChunk: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := buffer.Close(); err != nil {
		t.Fatal(err)
	}
	var absent *Accumulator
	if snapshot := absent.Snapshot(); snapshot.Utterances != 0 || snapshot.InFlight != 0 {
		t.Fatalf("a nil accumulator should report nothing: %+v", snapshot)
	}
	tracked, err := absent.New(Config{Provider: &slowProvider{}, MinimumChunk: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := tracked.Close(); err != nil {
		t.Fatalf("a nil accumulator should still hand back a usable buffer: %v", err)
	}
}

func TestMeansReportZeroRatherThanDividingByIt(t *testing.T) {
	t.Parallel()
	var empty ProviderMetrics
	if empty.MeanAdvanceNS() != 0 || empty.MeanFinalizeNS() != 0 {
		t.Fatal("an unused recogniser should report a zero mean")
	}
}
