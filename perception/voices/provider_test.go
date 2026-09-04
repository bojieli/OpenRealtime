package voices_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/perception/voices"
)

type providerFixture struct {
	mu          sync.Mutex
	revisions   []v1.PerceptionRevision
	final       v1.PerceptionRevision
	endpointed  bool
	closeCalled bool
}

func (provider *providerFixture) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "fixture-asr", Version: "1", Capabilities: v1.Capabilities{}}
}

func (provider *providerFixture) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.revisions) == 0 {
		return nil, nil
	}
	revision := provider.revisions[0]
	provider.revisions = provider.revisions[1:]
	return []v1.PerceptionRevision{revision}, nil
}

func (provider *providerFixture) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return provider.final, nil
}

func (provider *providerFixture) SpeechEndpointed() bool { return provider.endpointed }
func (provider *providerFixture) Close() error {
	provider.closeCalled = true
	return nil
}

type blockingSecondEmbedder struct {
	secondStarted chan struct{}
	secondRelease chan struct{}
}

func (embedder *blockingSecondEmbedder) Embed(
	_ context.Context, pcm []byte, _ uint32,
) ([]float32, error) {
	// The enrollment frame is filled with zeroes and the comparison frame with
	// ones, which keeps the test deterministic without synchronizing a call
	// counter shared by embedding goroutines.
	if len(pcm) > 0 && pcm[0] == 0 {
		return []float32{1, 0}, nil
	}
	close(embedder.secondStarted)
	<-embedder.secondRelease
	return []float32{0, 1}, nil
}

func attributionFrame(marker byte) v1.AudioFrame {
	pcm := make([]byte, 2*16_000*2)
	for index := range pcm {
		pcm[index] = marker
	}
	return v1.AudioFrame{SampleRateHz: 16_000, PCM16LE: pcm}
}

func TestProviderDoesNotBlockPartialAndFinalJoinsLateSpeakerVerdict(t *testing.T) {
	embedder := &blockingSecondEmbedder{
		secondStarted: make(chan struct{}), secondRelease: make(chan struct{}),
	}
	recogniser := voices.New(embedder, voices.DefaultThreshold, voices.DefaultMinimum)

	firstInner := &providerFixture{final: v1.PerceptionRevision{StableText: "hello", Final: true}}
	first, err := voices.WrapProvider(t.Context(), firstInner, recogniser, "utterance-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PushFrame(t.Context(), attributionFrame(0)); err != nil {
		t.Fatal(err)
	}
	firstFinal, err := first.Finalize(t.Context(), 1)
	if err != nil || firstFinal.Source != "" {
		t.Fatalf("enrollment final = %+v, err=%v", firstFinal, err)
	}

	secondInner := &providerFixture{
		revisions:  []v1.PerceptionRevision{{StableText: "are you", Final: false}},
		final:      v1.PerceptionRevision{StableText: "are you getting milk?", Final: true},
		endpointed: true,
	}
	second, err := voices.WrapProvider(t.Context(), secondInner, recogniser, "utterance-2")
	if err != nil {
		t.Fatal(err)
	}
	partialDone := make(chan []v1.PerceptionRevision, 1)
	go func() {
		revisions, _ := second.PushFrame(t.Context(), attributionFrame(1))
		partialDone <- revisions
	}()
	select {
	case partial := <-partialDone:
		if len(partial) != 1 || partial[0].Source != "" {
			t.Fatalf("non-blocking partial = %+v", partial)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("partial transcript blocked on speaker embedding")
	}
	<-embedder.secondStarted

	finalDone := make(chan v1.PerceptionRevision, 1)
	go func() {
		revision, _ := second.Finalize(t.Context(), 2)
		finalDone <- revision
	}()
	select {
	case revision := <-finalDone:
		t.Fatalf("final overtook in-flight speaker comparison: %+v", revision)
	case <-time.After(20 * time.Millisecond):
	}
	close(embedder.secondRelease)
	select {
	case revision := <-finalDone:
		if revision.Source != voices.OtherSpeakerSource {
			t.Fatalf("late speaker verdict was not attached: %+v", revision)
		}
	case <-time.After(time.Second):
		t.Fatal("final did not resume after speaker comparison")
	}
	if !second.SpeechEndpointed() {
		t.Fatal("speaker wrapper did not forward SpeechEndpointed")
	}
	if err := second.Close(); err != nil || !secondInner.closeCalled {
		t.Fatalf("speaker wrapper Close forwarding: called=%t err=%v", secondInner.closeCalled, err)
	}
}

func TestProviderLabelsLatePartialAfterDifferentVerdict(t *testing.T) {
	embedder := &scripted{vectors: [][]float32{{1, 0}, {0, 1}}}
	recogniser := voices.New(embedder, voices.DefaultThreshold, voices.DefaultMinimum)
	first, _ := voices.WrapProvider(t.Context(), &providerFixture{}, recogniser, "first")
	_, _ = first.PushFrame(t.Context(), attributionFrame(0))
	if got := settle(t, recogniser, voices.Familiar); got != voices.Familiar {
		t.Fatalf("enrollment = %q", got)
	}

	inner := &providerFixture{revisions: []v1.PerceptionRevision{
		{StableText: "early"}, {StableText: "late"},
	}}
	provider, _ := voices.WrapProvider(t.Context(), inner, recogniser, "second")
	_, _ = provider.PushFrame(t.Context(), attributionFrame(1))
	if got := settle(t, recogniser, voices.Different); got != voices.Different {
		t.Fatalf("comparison = %q", got)
	}
	revisions, err := provider.PushFrame(t.Context(), v1.AudioFrame{
		SampleRateHz: 16_000, PCM16LE: []byte{0, 0},
	})
	if err != nil || len(revisions) != 1 || revisions[0].Source != voices.OtherSpeakerSource {
		t.Fatalf("late partial = %+v, err=%v", revisions, err)
	}
}

var _ io.Closer = (*providerFixture)(nil)
