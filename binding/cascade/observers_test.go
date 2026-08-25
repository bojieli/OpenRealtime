package cascade_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func videoFactory() perception.Factory {
	return perception.VideoFactory(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: "A dialog is open."},
	})
}

func videoConfig(defaults []string) cascade.Config {
	return cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &scriptedASR{final: "what is on screen"}, nil
		},
		Observers:        []perception.Factory{videoFactory()},
		DefaultObservers: defaults,
		Fast:             newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "A dialog."}}),
		Slow:             newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "A dialog is open."}}),
		Speech:           toneSpeech{chunks: 1},
	}
}

func screenFrame(t *testing.T) perception.Frame {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			shade := uint8(40)
			if x > 20 && x < 200 && y > 20 && y < 120 {
				shade = 220
			}
			canvas.Set(x, y, color.RGBA{R: shade, G: shade, B: shade, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, nil); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	return perception.Frame{
		Kind: perception.FrameImage, Source: "screen", MIMEType: "image/jpeg",
		Width: 640, Height: 480, CapturedNS: uint64(time.Now().UnixNano()),
		Image: buffer.Bytes(),
	}
}

// A deployment declares what its sessions may select from; a session selects.
// Before this, the set was a property of the process and every session got the
// same one, so "observers configurable per session" was not true.
func TestObserversAreSelectablePerSession(t *testing.T) {
	bind, err := cascade.New(videoConfig(nil))
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	available := bind.Capabilities().Observers
	if len(available) != 2 || available[0] != "audio" || available[1] != "video" {
		t.Fatalf("a client cannot choose a set it cannot see: %v", available)
	}

	voiceOnly, err := bind.Start(context.Background(), binding.Options{
		Sink: &recordingSink{}, SessionID: "voice",
		Settings: binding.Settings{Observers: []string{"audio"}},
	})
	if err != nil {
		t.Fatalf("start voice session: %v", err)
	}
	t.Cleanup(func() { _ = voiceOnly.Close(context.Background(), nil) })

	watching, err := bind.Start(context.Background(), binding.Options{
		Sink: &recordingSink{}, SessionID: "watching",
		Settings: binding.Settings{Observers: []string{"audio", "video"}},
	})
	if err != nil {
		t.Fatalf("start watching session: %v", err)
	}
	t.Cleanup(func() { _ = watching.Close(context.Background(), nil) })

	// Two sessions on one server, differing in exactly one factor.
	if got := voiceOnly.Status().Observers; len(got) != 1 || got[0] != "audio" {
		t.Fatalf("the voice-only session reported %v", got)
	}
	if got := watching.Status().Observers; len(got) != 2 {
		t.Fatalf("the watching session reported %v", got)
	}
	if err := voiceOnly.Video(context.Background(), screenFrame(t)); err == nil {
		t.Fatal("a session that did not select the video observer must refuse video")
	}
	if err := watching.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("a session that selected the video observer must accept video: %v", err)
	}
}

// Seeing a page is not authorization to operate it. The video observation is
// committed immediately, but cognition stays unarmed until a user-authority
// observation establishes intent. This is what keeps a fast visual lane from
// clicking the first plausible control before the spoken request is over.
func TestObserverEvidenceCannotStartAnAutonomousTurnBeforeUserIntent(t *testing.T) {
	config := videoConfig(nil)
	fast := config.Fast.(*scriptedProvider)
	slow := config.Slow.(*scriptedProvider)
	runtime, sink := startSession(t, config, binding.Settings{})
	if err := runtime.Video(context.Background(), screenFrame(t)); err != nil {
		t.Fatalf("video: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation &&
				trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
				return true
			}
		}
		return false
	}, "the observer evidence was not committed")
	time.Sleep(150 * time.Millisecond)
	if fast.invocations() != 0 || slow.invocations() != 0 {
		t.Fatalf("passive evidence started cognition: fast=%d slow=%d", fast.invocations(), slow.invocations())
	}
	if outcomes := sink.turnOutcomes(); len(outcomes) != 0 {
		t.Fatalf("a silent observation opened a protocol turn: %+v", outcomes)
	}

	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.invocations() > 0 }, "user intent did not arm cognition")
}

// The video-only level of factor F3 has to actually remove the recogniser.
// A level that equals another level measures nothing.
func TestVideoOnlyRunsNoRecogniser(t *testing.T) {
	asr := &scriptedASR{final: "this should never be recognised"}
	config := videoConfig([]string{"video"})
	config.Perception = func() (v1.PerceptionProvider, error) { return asr, nil }
	runtime, sink := startSession(t, config, binding.Settings{})

	speak(t, runtime, 3)
	time.Sleep(200 * time.Millisecond)

	if asr.pushes != 0 {
		t.Fatalf("video-only ran the recogniser %d times", asr.pushes)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, transcript := range sink.transcripts {
		if transcript.Text != "" {
			t.Fatalf("video-only produced a transcript: %q", transcript.Text)
		}
	}
}

// An unknown observer name is refused rather than ignored: a session that
// asked for perception it did not get and was told nothing would report a
// configuration it was not running.
func TestAnUnknownObserverIsRefused(t *testing.T) {
	bind, err := cascade.New(videoConfig(nil))
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	if _, err := bind.Start(context.Background(), binding.Options{
		Sink: &recordingSink{}, Settings: binding.Settings{Observers: []string{"lidar"}},
	}); err == nil {
		t.Fatal("an unknown observer must be refused")
	}
}
