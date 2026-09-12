package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// screenshot is a small real PNG, so the real video observer decodes it.
func screenshot(shade uint8) []byte {
	picture := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			picture.Set(x, y, color.RGBA{R: shade, G: uint8(x * 8), B: uint8(y * 8), A: 255})
		}
	}
	var out bytes.Buffer
	_ = png.Encode(&out, picture)
	return out.Bytes()
}

// TestAScreenReachesTheVoiceAsContext is decision D4: a frame goes to this
// side's observer, never to the remote, and its narration is pushed silently.
func TestAScreenReachesTheVoiceAsContext(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{}
	narration := "The payment form is on screen. The card field is filled, ending 4242; expiry is empty."
	runtime, _ := startLive(t, remote, slow, func(config *upstream.Config) {
		config.Observers = []perception.Factory{perception.VideoFactory(perception.VideoConfig{
			Narrator: perception.StaticNarrator{Text: narration},
		})}
	})

	status := runtime.Status()
	if len(status.Observers) < 2 || status.Observers[1] != "video" {
		t.Fatalf("status does not list the video observer: %v", status.Observers)
	}
	if err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: "screen", Image: screenshot(40), MIMEType: "image/png",
		Width: 32, Height: 32, CapturedNS: 1,
	}); err != nil {
		t.Fatalf("video: %v", err)
	}
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.context") {
			if text, _ := message["text"].(string); strings.Contains(text, "4242") {
				return true
			}
		}
		return false
	}, "the screen's narration must reach the voice as context")
	if len(sentOfType(remote, "response.create")) != 0 || slowCalls(slow) != 0 {
		t.Fatal("a screen change must not ask the voice to speak or cost a reasoner call")
	}
	var observed bool
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindObservation && item.Observation != nil &&
			item.Observation.Observer == "video" && trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
			observed = true
		}
	}
	if !observed {
		t.Fatal("the narration must be committed as observer-authority evidence from the video observer")
	}
}

// TestVideoWithoutAnObserverIsStillUnsupported keeps the honest default.
func TestVideoWithoutAnObserverIsStillUnsupported(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, nil)
	err := runtime.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: "screen", Image: screenshot(1), MIMEType: "image/png",
		Width: 32, Height: 32,
	})
	if !errors.Is(err, binding.ErrUnsupported) {
		t.Fatalf("video with no observer must be unsupported, got %v", err)
	}
	bind, _ := upstream.New(upstream.Config{URL: remote.url(), Slow: &scriptedSlow{}, Dialect: upstream.DialectGPTLive})
	if bind.Capabilities().Video {
		t.Fatal("capabilities must not promise video without an observer")
	}
	with, _ := upstream.New(upstream.Config{
		URL: remote.url(), Slow: &scriptedSlow{}, Dialect: upstream.DialectGPTLive,
		Observers: []perception.Factory{perception.VideoFactory(perception.VideoConfig{Narrator: perception.StaticNarrator{Text: "x"}})},
	})
	if capabilities := with.Capabilities(); !capabilities.Video || len(capabilities.Observers) != 1 || capabilities.Observers[0] != "video" {
		t.Fatalf("capabilities must promise video and name the observer: %+v", capabilities)
	}
}

// TestAGreetingIsSteeredAfterTheStart is decision D5's greeting.
func TestAGreetingIsSteeredAfterTheStart(t *testing.T) {
	remote := newFakeRemote(t)
	startLive(t, remote, &scriptedSlow{}, func(config *upstream.Config) {
		config.Greeting = "Greet the caller in one sentence, then wait."
	})
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.steer") {
			if text, _ := message["text"].(string); strings.Contains(text, "Greet the caller") {
				return true
			}
		}
		return false
	}, "the greeting must be steered into the voice")
	var order []string
	for _, message := range remote.sent() {
		if kind, _ := message["type"].(string); kind == "session.update" || kind == "openrealtime.upstream.steer" {
			order = append(order, kind)
		}
	}
	if len(order) < 2 || order[0] != "session.update" || order[1] != "openrealtime.upstream.steer" {
		t.Fatalf("the greeting must follow the session declaration, got %v", order)
	}
}

// pinningExtractor pins whatever utterance mentions "short" as a rule.
type pinningExtractor struct{}

func (pinningExtractor) Name() string { return "pinning" }

func (pinningExtractor) HasArrived(context.Context, []interaction.StandingInstruction, string) bool {
	return false
}

func (pinningExtractor) Extract(_ context.Context, _ []interaction.StandingInstruction, _ []string, utterance string) (interaction.Extraction, error) {
	if strings.Contains(utterance, "short") {
		return interaction.Extraction{Pins: []interaction.StandingInstruction{{Text: "keep every answer short"}}}, nil
	}
	if strings.Contains(utterance, "never mind") {
		return interaction.Extraction{Revokes: []string{"keep every answer short"}}, nil
	}
	return interaction.Extraction{}, nil
}

// TestAStandingInstructionSaidOutLoudIsSteered is decision D5's standing
// instructions: a rule the user sets by saying it reaches the voice as an
// instruction, and lifting it reaches the voice too.
func TestAStandingInstructionSaidOutLoudIsSteered(t *testing.T) {
	remote := newFakeRemote(t)
	startLive(t, remote, &scriptedSlow{}, func(config *upstream.Config) {
		config.Extraction = pinningExtractor{}
	})
	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "from now on keep it short please",
	})
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.steer") {
			if text, _ := message["text"].(string); strings.Contains(text, "keep every answer short") {
				return true
			}
		}
		return false
	}, "the pinned rule must be steered into the voice")
	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_2", "transcript": "actually never mind about that",
	})
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.steer") {
			if text, _ := message["text"].(string); strings.Contains(text, "no longer applies") {
				return true
			}
		}
		return false
	}, "lifting the rule must reach the voice")
}

// TestSessionsAreBoundedPerEndpoint is decision D9's admission cap.
func TestSessionsAreBoundedPerEndpoint(t *testing.T) {
	remote := newFakeRemote(t)
	bind, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: &scriptedSlow{}, Dialect: upstream.DialectGPTLive, MaxSessions: 1,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	first, err := bind.Start(context.Background(), binding.Options{Sink: &collectingSink{}, SessionID: "one"})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := bind.Start(context.Background(), binding.Options{Sink: &collectingSink{}, SessionID: "two"}); !errors.Is(err, upstream.ErrAtCapacity) {
		t.Fatalf("the second session must be refused at capacity, got %v", err)
	}
	if err := first.Close(context.Background(), nil); err != nil {
		t.Fatalf("close: %v", err)
	}
	third, err := bind.Start(context.Background(), binding.Options{Sink: &collectingSink{}, SessionID: "three"})
	if err != nil {
		t.Fatalf("after a close the slot must be free, got %v", err)
	}
	_ = third.Close(context.Background(), nil)
}

// TestTheVendorsTimelineBecomesTheTrajectorys is decision D6.
func TestTheVendorsTimelineBecomesTheTrajectorys(t *testing.T) {
	remote := newFakeRemote(t)
	scheduler := clock.NewManual(5_000)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, func(config *upstream.Config) {
		config.Scheduler = scheduler
	})
	remote.emit(map[string]any{
		"type": "session.created", "session": map[string]any{"id": "live_t1", "expires_at": 1},
	})
	waitFor(t, func() bool {
		status := runtime.Status().Remote
		return status != nil && status.SessionID == "live_t1"
	}, "the session identity must land, which is when the epoch is taken")
	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "what time is it", "end_ms": 2500,
	})
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && item.Content == "what time is it" {
				// Source time lives in the item's event metadata; the wire
				// name is the contract, so that is what is asserted on.
				encoded, _ := json.Marshal(item)
				return strings.Contains(string(encoded), `"occurred_ns":2500005000`)
			}
		}
		return false
	}, "the turn must carry the vendor's end_ms translated onto this side's clock")
}
