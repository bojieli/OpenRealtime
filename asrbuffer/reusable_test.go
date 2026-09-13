package asrbuffer

import (
	"context"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

type reusableProvider struct {
	fakeProvider
	ended, closed int
}

func (provider *reusableProvider) EndUtterance() error { provider.ended++; return nil }
func (provider *reusableProvider) Close() error        { provider.closed++; return nil }

// A recogniser that keeps its connection is kept: the utterance ends inside
// it, the buffer takes the next utterance from sample zero, and the
// recogniser is closed once, at the end of the session.
func TestWrapReusesARecogniserAcrossUtterances(t *testing.T) {
	accumulator := NewAccumulator()
	provider := &reusableProvider{fakeProvider: fakeProvider{descriptor: testDescriptor()}}
	wrapped, err := accumulator.Wrap(Config{Provider: provider, MinimumChunk: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	reusable, ok := wrapped.(v1.UtteranceReusable)
	if !ok {
		t.Fatalf("a reusable recogniser was wrapped as %T, which the runtime closes every utterance", wrapped)
	}
	for utterance := range 2 {
		if _, err := wrapped.PushFrame(context.Background(), newFrame(0, 0, 160)); err != nil {
			t.Fatalf("utterance %d push: %v", utterance, err)
		}
		if _, err := wrapped.Finalize(context.Background(), 160); err != nil {
			t.Fatalf("utterance %d finalize: %v", utterance, err)
		}
		if err := reusable.EndUtterance(); err != nil {
			t.Fatal(err)
		}
	}
	if provider.ended != 2 || provider.closed != 0 {
		t.Fatalf("recogniser ended %d utterances and was closed %d times, want 2 and 0", provider.ended, provider.closed)
	}
	closer := wrapped.(interface{ Close() error })
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot := accumulator.Snapshot()
	if provider.closed != 1 || snapshot.FinalizeInvocations != 2 || snapshot.InFlight != 0 {
		t.Fatalf("after close: closed %d, snapshot %+v", provider.closed, snapshot)
	}
}

// A batch recogniser keeps owning one utterance.
func TestWrapLeavesANonReusableRecogniserPerUtterance(t *testing.T) {
	wrapped, err := NewAccumulator().Wrap(Config{
		Provider: &fakeProvider{descriptor: testDescriptor()}, MinimumChunk: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, reusable := wrapped.(v1.UtteranceReusable); reusable {
		t.Fatal("a recogniser without EndUtterance was offered for reuse")
	}
}

func testDescriptor() v1.Descriptor {
	return v1.Descriptor{Name: "reusable", Version: "1", Capabilities: v1.Capabilities{}}
}
