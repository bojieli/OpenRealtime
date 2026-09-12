package upstream_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"testing"
	"time"

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

// TestBargeInStopsASteerableRemote is the defect Full-Duplex-Bench found
// against the real endpoint: the user takes the floor and the voice keeps
// talking. The remote owns the floor and, being full duplex, often chooses to
// continue - measured at sixteen seconds on one recording. This binding has a
// measured policy for that decision and, until now, no way to act on it.
func TestBargeInStopsASteerableRemote(t *testing.T) {
	remote := newFakeRemote(t)
	enabled := true
	runtime, _ := startLive(t, remote, &scriptedSlow{}, func(config *upstream.Config) {
		config.BargeIn = &enabled
	})

	// The remote is speaking - two seconds of it, so the barge-in below lands
	// while audio is still playing out rather than after it.
	remote.emit(map[string]any{
		"type":  "response.output_audio.delta",
		"delta": base64.StdEncoding.EncodeToString(make([]byte, 24000*2*2)),
	})
	waitFor(t, func() bool { return runtime.Status().Interaction.Transport != "" }, "the session to settle")
	time.Sleep(50 * time.Millisecond)
	// The user takes the floor over it.
	remote.emit(map[string]any{
		"type": "input_audio_buffer.speech_started", "item_id": "item_1", "audio_start_ms": 500,
	})
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.steer") {
			if text, _ := message["text"].(string); strings.Contains(strings.ToLower(text), "stop speaking") {
				return true
			}
		}
		return false
	}, "the policy must stop a steerable remote when the user talks over it")
}

// TestBargeInIsOffByDefaultAndRefusedWhereItCannotAct checks both halves of
// the default. A full-duplex model handles interruption itself and keeps that
// job unless a deployment takes it away; an endpoint with no way to be stopped
// mid-sentence cannot be given the job at all.
func TestBargeInIsOffByDefaultAndRefusedWhereItCannotAct(t *testing.T) {
	remote := newFakeRemote(t)
	enabled := true
	if _, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: &scriptedSlow{}, BargeIn: &enabled,
	}); err == nil {
		t.Error("a Realtime endpoint cannot be stopped mid-sentence and must refuse the job")
	}
	bind, err := upstream.New(upstream.Config{URL: remote.url(), Slow: &scriptedSlow{}, Model: "remote-model"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{Sink: sink, SessionID: "rt"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	<-remote.ready
	remote.emit(map[string]any{
		"type":  "response.output_audio.delta",
		"delta": base64.StdEncoding.EncodeToString(make([]byte, 24000*2*2)),
	})
	time.Sleep(50 * time.Millisecond)
	remote.emit(map[string]any{
		"type": "input_audio_buffer.speech_started", "item_id": "item_1", "audio_start_ms": 500,
	})
	time.Sleep(300 * time.Millisecond)
	for _, message := range remote.sent() {
		if kind, _ := message["type"].(string); kind == "openrealtime.upstream.steer" {
			t.Fatalf("a Realtime endpoint was sent a stop it never needed: %v", message)
		}
	}
}

// TestBargeInHoldsTheAudioTheUserWouldHear is the other half of barge-in, and
// the half the person who interrupted actually notices.
//
// Telling a full-duplex remote to stop is slow: measured against the real
// endpoint the instruction took 4.6 s to take effect, and the vendor states
// that its acknowledgement proves neither that the model stopped nor that
// queued audio stopped playing. This binding sits between the remote and the
// client, so it holds the audio back itself - which is where the vendor says
// to block output - and releases it when the interrupted utterance ends.
func TestBargeInHoldsTheAudioTheUserWouldHear(t *testing.T) {
	remote := newFakeRemote(t)
	enabled := true
	_, sink := startLive(t, remote, &scriptedSlow{}, func(config *upstream.Config) {
		config.BargeIn = &enabled
	})

	twoSeconds := base64.StdEncoding.EncodeToString(make([]byte, 24000*2*2))
	remote.emit(map[string]any{"type": "response.output_audio.delta", "delta": twoSeconds})
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.audioFrames >= 1
	}, "the remote's speech must reach the client before the interruption")

	remote.emit(map[string]any{
		"type": "input_audio_buffer.speech_started", "item_id": "item_1", "audio_start_ms": 500,
	})
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.steer") {
			if text, _ := message["text"].(string); strings.Contains(strings.ToLower(text), "stop speaking") {
				return true
			}
		}
		return false
	}, "the remote must be told to stop")

	// The remote has not obeyed yet and keeps sending audio. None of it may
	// reach the person who just took the floor.
	sink.mu.Lock()
	heardBefore := sink.audioFrames
	sink.mu.Unlock()
	for tick := 0; tick < 5; tick++ {
		remote.emit(map[string]any{"type": "response.output_audio.delta", "delta": twoSeconds})
	}
	time.Sleep(300 * time.Millisecond)
	sink.mu.Lock()
	heardDuring := sink.audioFrames
	sink.mu.Unlock()
	if heardDuring != heardBefore {
		t.Fatalf("%d further audio frames reached the user after they took the floor",
			heardDuring-heardBefore)
	}

	// When the interrupted utterance ends, the remote is heard again.
	remote.emit(map[string]any{"type": "response.done"})
	time.Sleep(100 * time.Millisecond)
	remote.emit(map[string]any{"type": "response.output_audio.delta", "delta": twoSeconds})
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.audioFrames > heardDuring
	}, "after the interrupted utterance ends the remote must be audible again")
}

// TestALiveVoiceIsToldToDelegate is the bug tau2-bench found, and the reason
// the agent was mute.
//
// A Realtime endpoint answers for itself and this binding's reasoner runs on
// every turn, so the voice only needs to know that answers will arrive.
// GPT-Live reasons about nothing: it asks for help by delegating, and the
// reasoner runs when it does. A voice never told to delegate never asks, so
// nothing runs and nothing is said - measured on tau2-bench as sixty-eight
// seconds of silence and no tool call, after a caller gave a complete request.
func TestALiveVoiceIsToldToDelegate(t *testing.T) {
	remote := newFakeRemote(t)
	startLive(t, remote, &scriptedSlow{}, nil)
	waitFor(t, func() bool { return len(sentOfType(remote, "session.update")) > 0 },
		"the session declaration must reach the remote")
	declared, _ := json.Marshal(sentOfType(remote, "session.update")[0])
	instruction := strings.ToLower(string(declared))
	if !strings.Contains(instruction, "delegate") {
		t.Fatalf("a GPT-Live voice must be told to delegate, or it never asks and the "+
			"agent is silent: %s", truncateInstruction(string(declared)))
	}
	if !strings.Contains(instruction, "cannot look anything up") {
		t.Errorf("the voice must be told it cannot look things up itself: %s",
			truncateInstruction(string(declared)))
	}
}

// TestARealtimeVoiceIsNotToldToDelegate keeps the other arrangement intact: a
// Realtime endpoint has nothing to delegate to and answers for itself.
func TestARealtimeVoiceIsNotToldToDelegate(t *testing.T) {
	remote := newFakeRemote(t)
	start(t, remote, &scriptedSlow{}, nil)
	waitFor(t, func() bool { return len(sentOfType(remote, "session.update")) > 0 },
		"the session declaration must reach the remote")
	declared, _ := json.Marshal(sentOfType(remote, "session.update")[0])
	if strings.Contains(strings.ToLower(string(declared)), "delegate") {
		t.Errorf("a Realtime endpoint has nothing to delegate to: %s",
			truncateInstruction(string(declared)))
	}
}

func truncateInstruction(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "…"
}

// TestALiveSessionMayChooseItsVoice checks the capability the gateway reads
// before it will pass a voice through. GPT-Live names its voice in
// session.start and this binding forwards the client's choice; declaring the
// voice unselectable made the gateway refuse the field with an error, which is
// what a client that repeats its voice in every session.update receives.
func TestALiveSessionMayChooseItsVoice(t *testing.T) {
	remote := newFakeRemote(t)
	live, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: &scriptedSlow{}, Dialect: upstream.DialectGPTLive,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if !live.Capabilities().Voice.Selectable {
		t.Error("GPT-Live names its voice at session start; a session must be able to choose it")
	}
	realtime, err := upstream.New(upstream.Config{URL: remote.url(), Slow: &scriptedSlow{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if realtime.Capabilities().Voice.Selectable {
		t.Error("a Realtime endpoint's voice is not this binding's to promise")
	}
}

// TestATelephoneCallerIsResampledToTheWireRate is the defect tau2-bench found
// after the delegation one, and the reason the agent was still mute.
//
// A telephony client sends G.711 at 8kHz. The gateway expands that to PCM16
// and says so on the frame, but this binding forwarded the samples and dropped
// the rate, so a Realtime endpoint expecting 24kHz received the caller three
// times too fast: a third of the duration, every formant tripled, and nothing
// a speech model transcribes. Nothing errors - the audio is well-formed, just
// not speech any more - so the whole failure appears as an agent that greets
// the caller and then says nothing at all.
func TestATelephoneCallerIsResampledToTheWireRate(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, nil)

	// A tenth of a second of telephone-rate speech, which is the order a real
	// leg sends: 800 samples, two bytes each.
	const callerRate, callerSamples = 8_000, 800
	frame := perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone",
		CapturedNS: uint64(time.Now().UnixNano()),
		PCM16LE:    make([]byte, callerSamples*2), SampleRateHz: callerRate,
	}
	for i := range callerSamples {
		sample := int16(8000 * math.Sin(2*math.Pi*300*float64(i)/callerRate))
		binary.LittleEndian.PutUint16(frame.PCM16LE[i*2:], uint16(sample))
	}
	if err := runtime.Audio(context.Background(), frame); err != nil {
		t.Fatalf("audio: %v", err)
	}

	waitFor(t, func() bool { return len(sentOfType(remote, "input_audio_buffer.append")) > 0 },
		"the caller's audio must reach the remote")

	forwarded := 0
	for _, message := range sentOfType(remote, "input_audio_buffer.append") {
		encoded, _ := message["audio"].(string)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decode forwarded audio: %v", err)
		}
		forwarded += len(decoded) / 2
	}
	// A tenth of a second of speech must still be a tenth of a second. At
	// 24kHz that is 2400 samples; forwarding the caller's 800 verbatim is the
	// bug, and lands as a third of the duration at three times the pitch.
	const wireRate, want = 24_000, 2_400
	if forwarded < want*9/10 || forwarded > want*11/10 {
		t.Errorf("%.2fs of %dHz speech reached the remote as %d samples, "+
			"which at the wire's %dHz is %.3fs: the caller's rate was dropped "+
			"rather than converted",
			float64(callerSamples)/callerRate, callerRate, forwarded, wireRate,
			float64(forwarded)/wireRate)
	}
}

// TestAWireRateCallerIsForwardedUntouched keeps the ordinary path exact: a
// client already at the wire's rate must not be run through a converter that
// can only lose to it.
func TestAWireRateCallerIsForwardedUntouched(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, nil)

	const samples = 2_400 // a tenth of a second at the wire's rate
	payload := make([]byte, samples*2)
	for i := range samples {
		binary.LittleEndian.PutUint16(payload[i*2:], uint16(int16(i%2000-1000)))
	}
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone",
		CapturedNS: uint64(time.Now().UnixNano()),
		PCM16LE:    payload, SampleRateHz: 24_000,
	}); err != nil {
		t.Fatalf("audio: %v", err)
	}
	waitFor(t, func() bool { return len(sentOfType(remote, "input_audio_buffer.append")) > 0 },
		"the caller's audio must reach the remote")

	var forwarded []byte
	for _, message := range sentOfType(remote, "input_audio_buffer.append") {
		encoded, _ := message["audio"].(string)
		decoded, _ := base64.StdEncoding.DecodeString(encoded)
		forwarded = append(forwarded, decoded...)
	}
	if !bytes.Equal(forwarded, payload) {
		t.Errorf("audio already at the wire's rate was altered on the way through: "+
			"sent %d bytes, remote saw %d", len(payload), len(forwarded))
	}
}

// TestATelephoneStreamKeepsItsDurationAcrossFrames covers what one frame
// cannot show. A converter holds the tail of each frame back until the next
// one gives it the neighbours it needs; rebuilt per frame, it starts cold
// every time and drops that tail, so a stream loses a slice of every frame and
// the caller drifts steadily ahead of what the remote hears.
func TestATelephoneStreamKeepsItsDurationAcrossFrames(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, nil)

	const callerRate, perFrame, frames = 8_000, 160, 50 // 20ms frames, one second
	for f := range frames {
		payload := make([]byte, perFrame*2)
		for i := range perFrame {
			n := f*perFrame + i
			sample := int16(8000 * math.Sin(2*math.Pi*300*float64(n)/callerRate))
			binary.LittleEndian.PutUint16(payload[i*2:], uint16(sample))
		}
		if err := runtime.Audio(context.Background(), perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone",
			CapturedNS: uint64(time.Now().UnixNano()),
			PCM16LE:    payload, SampleRateHz: callerRate,
		}); err != nil {
			t.Fatalf("audio frame %d: %v", f, err)
		}
	}

	samples := func() []int16 {
		var all []int16
		for _, message := range sentOfType(remote, "input_audio_buffer.append") {
			encoded, _ := message["audio"].(string)
			decoded, _ := base64.StdEncoding.DecodeString(encoded)
			for i := 0; i+1 < len(decoded); i += 2 {
				all = append(all, int16(binary.LittleEndian.Uint16(decoded[i:])))
			}
		}
		return all
	}
	forwarded := func() int { return len(samples()) }
	// One second in, one second out, within a percent. Waiting on the quantity
	// under test rather than on a frame count is deliberate: a converter holds
	// part of a frame back for the next one, so the number of messages is not
	// the number of frames, and counting messages first and samples afterwards
	// reads a total that is still growing.
	const want = 24_000
	waitFor(t, func() bool { return forwarded() >= want*99/100 },
		fmt.Sprintf("one second of telephone speech must reach the remote as about "+
			"%d samples; it arrived as %d, so the caller's rate is being dropped",
			want, forwarded()))

	// Duration alone cannot see a seam: a converter restarted every frame
	// lands within half a percent of the right length and still clicks at
	// every boundary, because it begins each frame with no history and guesses
	// the samples before it. On this tone neighbouring samples move by at most
	// 626; measured, a per-frame restart triples that to 1867. Anything that
	// far above the signal's own slope is a discontinuity, which is heard as a
	// click and read by a speech model as a consonant nobody said.
	const smoothest = 626
	got, worst, at := samples(), 0, 0
	for i := 1; i < len(got); i++ {
		delta := int(got[i]) - int(got[i-1])
		if delta < 0 {
			delta = -delta
		}
		if delta > worst {
			worst, at = delta, i
		}
	}
	if worst > smoothest*2 {
		t.Errorf("the converted stream jumps by %d at sample %d, against %d for the "+
			"tone itself: the converter is starting cold on each frame instead of "+
			"carrying its state across them", worst, at, smoothest)
	}
}

// TestACallerMayChangeRateMidSession covers a client that re-declares its
// audio format while the session is open, which the protocol allows: every
// session.update carries a format, and a client may send a second one. The
// converter is keyed on the rate it was built for, so it has to be rebuilt
// when that changes - held onto, it would go on dividing by the old number and
// stretch the caller instead of shortening them.
func TestACallerMayChangeRateMidSession(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, nil)

	speak := func(rate, samples int) {
		t.Helper()
		payload := make([]byte, samples*2)
		for i := range samples {
			value := int16(8000 * math.Sin(2*math.Pi*300*float64(i)/float64(rate)))
			binary.LittleEndian.PutUint16(payload[i*2:], uint16(value))
		}
		if err := runtime.Audio(context.Background(), perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone",
			CapturedNS: uint64(time.Now().UnixNano()),
			PCM16LE:    payload, SampleRateHz: uint32(rate),
		}); err != nil {
			t.Fatalf("audio at %dHz: %v", rate, err)
		}
	}
	forwarded := func() int {
		total := 0
		for _, message := range sentOfType(remote, "input_audio_buffer.append") {
			encoded, _ := message["audio"].(string)
			decoded, _ := base64.StdEncoding.DecodeString(encoded)
			total += len(decoded) / 2
		}
		return total
	}

	// Half a second at 8kHz, then half a second at 16kHz: one second in total,
	// so 24000 samples out. A converter still set to 8kHz would read the 16kHz
	// half as twice the duration and send half again too much.
	speak(8_000, 4_000)
	speak(16_000, 8_000)
	const want = 24_000
	waitFor(t, func() bool { return forwarded() >= want*98/100 },
		fmt.Sprintf("a second of speech across two rates must reach the remote as "+
			"about %d samples; it arrived as %d", want, forwarded()))
	if got := forwarded(); got > want*102/100 {
		t.Errorf("a second of speech across two rates reached the remote as %d "+
			"samples instead of about %d: the converter kept the first rate after "+
			"the caller changed it", got, want)
	}
}
