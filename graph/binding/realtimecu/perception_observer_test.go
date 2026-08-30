package realtimecu

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type perceptionObserverFixture struct {
	name       string
	kind       perception.FrameKind
	refreshes  int
	reset      int
	closeError error
}

func (fixture *perceptionObserverFixture) Name() string               { return fixture.name }
func (fixture *perceptionObserverFixture) Cadence() time.Duration     { return 0 }
func (fixture *perceptionObserverFixture) Gate(perception.Frame) bool { return true }
func (fixture *perceptionObserverFixture) Flush(context.Context) ([]perception.Observation, error) {
	return nil, nil
}
func (fixture *perceptionObserverFixture) Reset() { fixture.reset++ }
func (fixture *perceptionObserverFixture) Accepts(frame perception.Frame) bool {
	return frame.Kind == fixture.kind
}
func (fixture *perceptionObserverFixture) Observe(
	_ context.Context, frames []perception.Frame,
) ([]perception.Observation, error) {
	authority := trajectory.AuthorityObserver
	if fixture.kind == perception.FrameAudio {
		authority = trajectory.AuthorityUser
	}
	return []perception.Observation{{
		Text: "observed", Observer: fixture.name, Source: frames[0].Source,
		Authority: authority, Revision: 1, Final: true,
	}}, nil
}
func (fixture *perceptionObserverFixture) RefreshNext() { fixture.refreshes++ }
func (fixture *perceptionObserverFixture) Close() error { return fixture.closeError }

func TestPerceptionObserverComposesSensorsAndRefreshesScreen(t *testing.T) {
	audio := &perceptionObserverFixture{name: "fixture", kind: perception.FrameAudio}
	video := &perceptionObserverFixture{name: "fixture", kind: perception.FrameImage}
	observer, err := NewPerceptionObserver(PerceptionObserverConfig{
		Name: "fixture", Audio: audio, Video: video,
	})
	if err != nil {
		t.Fatal(err)
	}
	audioResult, err := observer.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: SourceMicrophone, CapturedNS: 1,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	})
	if err != nil || len(audioResult) != 1 || audioResult[0].Authority != trajectory.AuthorityUser {
		t.Fatalf("audio observation = %+v, %v", audioResult, err)
	}
	videoResult, err := observer.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: SourceScreen, CapturedNS: 2,
		Image: []byte{1}, MIMEType: "image/jpeg", Width: 1, Height: 1,
	})
	if err != nil || len(videoResult) != 1 || videoResult[0].Authority != trajectory.AuthorityObserver {
		t.Fatalf("video observation = %+v, %v", videoResult, err)
	}
	if err := observer.Consequence(context.Background(), VisualConsequence{TargetSource: SourceScreen}); err != nil {
		t.Fatal(err)
	}
	if video.refreshes != 1 {
		t.Fatalf("video refreshes = %d, want 1", video.refreshes)
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Audio(context.Background(), perception.Frame{}); err == nil {
		t.Fatal("closed observer accepted audio")
	}
}

func TestPerceptionObserverRejectsIdentityAndJoinsCloseFailures(t *testing.T) {
	audioErr, videoErr := errors.New("audio close"), errors.New("video close")
	audio := &perceptionObserverFixture{name: "fixture", kind: perception.FrameAudio, closeError: audioErr}
	video := &perceptionObserverFixture{name: "fixture", kind: perception.FrameImage, closeError: videoErr}
	if _, err := NewPerceptionObserver(PerceptionObserverConfig{
		Name: "different", Audio: audio, Video: video,
	}); err == nil {
		t.Fatal("observer accepted drifted sensor identities")
	}
	observer, err := NewPerceptionObserver(PerceptionObserverConfig{
		Name: "fixture", Audio: audio, Video: video,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Close(); !errors.Is(err, audioErr) || !errors.Is(err, videoErr) {
		t.Fatalf("close error = %v", err)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
}

type concurrentPerceptionObserverFixture struct {
	name     string
	kind     perception.FrameKind
	started  chan struct{}
	release  chan struct{}
	start    sync.Once
	closed   atomic.Bool
	revision atomic.Uint64
}

func (fixture *concurrentPerceptionObserverFixture) Name() string       { return fixture.name }
func (*concurrentPerceptionObserverFixture) Cadence() time.Duration     { return 0 }
func (*concurrentPerceptionObserverFixture) Gate(perception.Frame) bool { return true }
func (*concurrentPerceptionObserverFixture) Flush(context.Context) ([]perception.Observation, error) {
	return nil, nil
}
func (*concurrentPerceptionObserverFixture) Reset() {}
func (fixture *concurrentPerceptionObserverFixture) Accepts(frame perception.Frame) bool {
	return frame.Kind == fixture.kind
}
func (fixture *concurrentPerceptionObserverFixture) Observe(
	ctx context.Context, frames []perception.Frame,
) ([]perception.Observation, error) {
	if fixture.kind == perception.FrameImage {
		fixture.start.Do(func() { close(fixture.started) })
		select {
		case <-fixture.release:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	authority := trajectory.AuthorityObserver
	if fixture.kind == perception.FrameAudio {
		authority = trajectory.AuthorityUser
	}
	return []perception.Observation{{
		Text: "observed", Observer: fixture.name, Source: frames[0].Source,
		Authority: authority, Revision: fixture.revision.Add(1), Final: true,
	}}, nil
}
func (*concurrentPerceptionObserverFixture) RefreshNext() {}
func (fixture *concurrentPerceptionObserverFixture) Close() error {
	fixture.closed.Store(true)
	return nil
}

func TestPerceptionObserverDoesNotLetBlockedVisionStarveAudioOrRaceClose(t *testing.T) {
	audio := &concurrentPerceptionObserverFixture{name: "fixture", kind: perception.FrameAudio}
	video := &concurrentPerceptionObserverFixture{
		name: "fixture", kind: perception.FrameImage,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	observer, err := NewPerceptionObserver(PerceptionObserverConfig{
		Name: "fixture", Audio: audio, Video: video,
	})
	if err != nil {
		t.Fatal(err)
	}
	videoDone := make(chan error, 1)
	go func() {
		_, observeErr := observer.Video(t.Context(), perception.Frame{
			Kind: perception.FrameImage, Source: SourceScreen, CapturedNS: 1,
			Image: []byte{1}, MIMEType: "image/jpeg", Width: 1, Height: 1,
		})
		videoDone <- observeErr
	}()
	select {
	case <-video.started:
	case <-time.After(time.Second):
		t.Fatal("vision fixture did not block")
	}
	audioDone := make(chan error, 1)
	go func() {
		_, observeErr := observer.Audio(t.Context(), perception.Frame{
			Kind: perception.FrameAudio, Source: SourceMicrophone, CapturedNS: 2,
			PCM16LE: []byte{1, 0}, SampleRateHz: 8_000,
		})
		audioDone <- observeErr
	}()
	select {
	case err := <-audioDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked vision starved concurrent audio")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- observer.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before active vision drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(video.release)
	if err := <-videoDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if !audio.closed.Load() || !video.closed.Load() {
		t.Fatal("sensor providers were not closed after active calls drained")
	}
}
