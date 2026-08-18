package asrbuffer

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

type fakeProvider struct {
	descriptor v1.Descriptor
	frames     []v1.AudioFrame
	finalized  uint64
	failAt     int
}

func (provider *fakeProvider) Descriptor() v1.Descriptor { return provider.descriptor }

func (provider *fakeProvider) PushFrame(_ context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	if provider.failAt > 0 && len(provider.frames)+1 == provider.failAt {
		return nil, errors.New("provider failed")
	}
	frame.PCM16LE = append([]byte(nil), frame.PCM16LE...)
	provider.frames = append(provider.frames, frame)
	return []v1.PerceptionRevision{{RevisionID: uint64(len(provider.frames)), SourceSample: frame.SampleOffset + uint64(len(frame.PCM16LE)/2), UnstableText: "partial"}}, nil
}

func (provider *fakeProvider) Finalize(_ context.Context, sourceSample uint64) (v1.PerceptionRevision, error) {
	provider.finalized = sourceSample
	return v1.PerceptionRevision{RevisionID: 99, SourceSample: sourceSample, StableText: "done", Final: true}, nil
}

func newFrame(index, offset, samples uint64) v1.AudioFrame {
	return v1.AudioFrame{Index: index, SampleOffset: offset, SampleRateHz: 16_000, PCM16LE: make([]byte, samples*2)}
}

func TestBufferDecouplesSchedulerTicksFromProviderChunks(t *testing.T) {
	t.Parallel()
	upstream := &fakeProvider{descriptor: v1.Descriptor{Name: "fake", Version: "1", Capabilities: v1.Capabilities{v1.CapabilityRevisions: true}}}
	buffer, err := New(Config{Provider: upstream, MinimumChunk: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for index := uint64(0); index < 3; index++ {
		revisions, err := buffer.PushFrame(context.Background(), newFrame(index, index*800, 800))
		if err != nil || len(revisions) != 0 {
			t.Fatalf("tick %d: revisions=%v err=%v", index, revisions, err)
		}
	}
	revisions, err := buffer.PushFrame(context.Background(), newFrame(3, 2400, 800))
	if err != nil || len(revisions) != 1 {
		t.Fatalf("fourth tick: revisions=%v err=%v", revisions, err)
	}
	if len(upstream.frames) != 1 || upstream.frames[0].Index != 0 || upstream.frames[0].SampleOffset != 0 || len(upstream.frames[0].PCM16LE) != 6400 {
		t.Fatalf("provider frames = %#v", upstream.frames)
	}
	stats := buffer.Stats()
	if stats.InputFrames != 4 || stats.InputSamples != 3200 || stats.ProviderChunks != 1 || stats.ProviderSamples != 3200 || stats.PendingSamples != 0 {
		t.Fatalf("stats = %#v", stats)
	}
	if buffer.ProviderInvocationCount() != 1 {
		t.Fatal("provider invocation counter did not advance")
	}
	// Descriptor capabilities must not alias the provider's map.
	descriptor := buffer.Descriptor()
	descriptor.Capabilities[v1.CapabilityRevisions] = false
	if !upstream.descriptor.Capabilities[v1.CapabilityRevisions] {
		t.Fatal("descriptor capability map was not copied")
	}
}

func TestBufferSplitsLargeFrameAndFlushesTerminalRemainder(t *testing.T) {
	t.Parallel()
	upstream := &fakeProvider{}
	buffer, err := New(Config{Provider: upstream, MinimumChunk: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := buffer.PushFrame(context.Background(), newFrame(0, 0, 7200)) // 450 ms
	if err != nil || len(revisions) != 2 {
		t.Fatalf("push: revisions=%d err=%v", len(revisions), err)
	}
	if len(upstream.frames) != 2 || upstream.frames[1].SampleOffset != 3200 {
		t.Fatalf("provider frames = %#v", upstream.frames)
	}
	if pending := buffer.Stats().PendingSamples; pending != 800 {
		t.Fatalf("pending samples = %d, want 800", pending)
	}
	final, err := buffer.Finalize(context.Background(), 7200)
	if err != nil || !final.Final || final.SourceSample != 7200 {
		t.Fatalf("final = %#v, err=%v", final, err)
	}
	if len(upstream.frames) != 3 || upstream.frames[2].Index != 2 || upstream.frames[2].SampleOffset != 6400 || len(upstream.frames[2].PCM16LE) != 1600 {
		t.Fatalf("terminal provider frame = %#v", upstream.frames)
	}
	stats := buffer.Stats()
	if stats.ProviderChunks != 3 || stats.ProviderSamples != 7200 || stats.PendingSamples != 0 || !stats.Finalized || upstream.finalized != 7200 {
		t.Fatalf("stats=%#v finalized=%d", stats, upstream.finalized)
	}
	if _, err := buffer.PushFrame(context.Background(), newFrame(1, 7200, 800)); err == nil {
		t.Fatal("expected finalized buffer to reject input")
	}
}

func TestBufferValidatesContinuityWithoutPoisoningSession(t *testing.T) {
	t.Parallel()
	buffer, err := New(Config{Provider: &fakeProvider{}, MinimumChunk: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.PushFrame(context.Background(), newFrame(0, 0, 800)); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.PushFrame(context.Background(), newFrame(2, 800, 800)); err == nil {
		t.Fatal("expected non-contiguous index failure")
	}
	if _, err := buffer.PushFrame(context.Background(), newFrame(1, 800, 800)); err != nil {
		t.Fatalf("valid retry after local validation failure: %v", err)
	}
}

func TestBufferProviderFailureIsTerminal(t *testing.T) {
	t.Parallel()
	buffer, err := New(Config{Provider: &fakeProvider{failAt: 1}, MinimumChunk: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.PushFrame(context.Background(), newFrame(0, 0, 800)); err == nil {
		t.Fatal("expected provider failure")
	}
	if _, err := buffer.PushFrame(context.Background(), newFrame(1, 800, 800)); err == nil {
		t.Fatal("expected terminal failure")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	for _, config := range []Config{
		{},
		{Provider: &fakeProvider{}},
		{Provider: &fakeProvider{}, MinimumChunk: -time.Millisecond},
		{Provider: &fakeProvider{}, MinimumChunk: time.Hour + time.Nanosecond},
		{Provider: &fakeProvider{}, MinimumChunk: time.Millisecond, MaxInputFrameBytes: -1},
	} {
		if _, err := New(config); err == nil {
			t.Fatalf("expected invalid config failure: %#v", config)
		}
	}
}
