package perception_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// --- audio ------------------------------------------------------------------

func TestEnergyGateOpensOnSpeechAndClosesOnSilence(t *testing.T) {
	gate, err := perception.NewEnergyGate(perception.GateConfig{
		Threshold: 0.5, PrefixPaddingMS: 100, SilenceDurationMS: 200,
	}, 24_000)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}
	loud := make([]byte, 4800)
	for index := 0; index < len(loud); index += 2 {
		loud[index], loud[index+1] = 0x00, 0x40
	}
	quiet := make([]byte, 4800)

	if result, _ := gate.Push(quiet); result.Started {
		t.Fatal("silence does not open the gate")
	}
	result, err := gate.Push(loud)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !result.Started || !gate.Speaking() {
		t.Fatal("speech opens the gate")
	}
	if len(result.Audio) <= len(loud) {
		t.Fatal("the prefix must be prepended so the first syllable is not clipped")
	}
	stopped := false
	for index := 0; index < 3 && !stopped; index++ {
		result, _ = gate.Push(quiet)
		stopped = result.Stopped
	}
	if !stopped || gate.Speaking() {
		t.Fatal("sustained silence closes the gate")
	}
}

func TestEnergyGateRejectsMalformedConfiguration(t *testing.T) {
	if _, err := perception.NewEnergyGate(perception.GateConfig{Threshold: 2, SilenceDurationMS: 1}, 24_000); err == nil {
		t.Fatal("expected an out-of-range threshold to be rejected")
	}
	if _, err := perception.NewEnergyGate(perception.DefaultGateConfig(), 0); err == nil {
		t.Fatal("expected a zero sample rate to be rejected")
	}
}

type revisionASR struct {
	revisions []v1.PerceptionRevision
	index     int
	final     string
}

func (asr *revisionASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "revision", Version: "1", Capabilities: v1.Capabilities{}}
}

func (asr *revisionASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	if asr.index >= len(asr.revisions) {
		return nil, nil
	}
	revision := asr.revisions[asr.index]
	asr.index++
	return []v1.PerceptionRevision{revision}, nil
}

func (asr *revisionASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{StableText: asr.final, Final: true}, nil
}

func audioFrame() perception.Frame {
	return perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone",
		PCM16LE: make([]byte, 480), SampleRateHz: 24_000,
	}
}

func TestAudioObserverProducesProvisionalRevisionsAndOneFinal(t *testing.T) {
	asr := &revisionASR{
		revisions: []v1.PerceptionRevision{
			{StableText: "what is"}, {StableText: "what is"}, {StableText: "what is my"},
		},
		final: "what is my balance",
	}
	observer, err := perception.NewAudioObserver(perception.AudioConfig{
		Provider: func() (v1.PerceptionProvider, error) { return asr, nil },
	})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	ctx := context.Background()
	var provisional []perception.Observation
	for index := 0; index < 3; index++ {
		observations, err := observer.Observe(ctx, []perception.Frame{audioFrame()})
		if err != nil {
			t.Fatalf("observe %d: %v", index, err)
		}
		provisional = append(provisional, observations...)
	}
	// The repeated revision produces nothing: an unchanged transcript is not
	// new evidence.
	if len(provisional) != 2 {
		t.Fatalf("expected two distinct provisional revisions, got %d", len(provisional))
	}
	for _, observation := range provisional {
		if !observation.Provisional || observation.Final {
			t.Fatalf("partials must be provisional: %+v", observation)
		}
		if observation.Authority != trajectory.AuthorityUser {
			t.Fatal("the audio observer reports the participant's own speech")
		}
	}
	final, err := observer.Flush(ctx)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(final) != 1 || !final[0].Final || final[0].Provisional {
		t.Fatalf("expected one final observation, got %+v", final)
	}
	if final[0].Supersedes == 0 {
		t.Fatal("the final observation must supersede the partials it replaces")
	}
}

func TestAudioObserverResetDropsRecogniserState(t *testing.T) {
	created := 0
	observer, err := perception.NewAudioObserver(perception.AudioConfig{
		Provider: func() (v1.PerceptionProvider, error) {
			created++
			return &revisionASR{final: "hello"}, nil
		},
	})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	ctx := context.Background()
	if _, err := observer.Observe(ctx, []perception.Frame{audioFrame()}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	observer.Reset()
	if _, err := observer.Observe(ctx, []perception.Frame{audioFrame()}); err != nil {
		t.Fatalf("observe after reset: %v", err)
	}
	if created != 2 {
		t.Fatalf("one recogniser per utterance, got %d", created)
	}
}

// --- video ------------------------------------------------------------------

func encodeFrame(t *testing.T, fill color.Gray, patch image.Rectangle) []byte {
	t.Helper()
	canvas := image.NewGray(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			canvas.SetGray(x, y, fill)
		}
	}
	for y := patch.Min.Y; y < patch.Max.Y; y++ {
		for x := patch.Min.X; x < patch.Max.X; x++ {
			canvas.SetGray(x, y, color.Gray{Y: 255 - fill.Y})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buffer.Bytes()
}

func imageFrame(payload []byte, capturedNS uint64) perception.Frame {
	return perception.Frame{
		Kind: perception.FrameImage, Source: "screen", CapturedNS: capturedNS,
		Image: payload, MIMEType: "image/jpeg", Width: 320, Height: 240,
	}
}

type countingNarrator struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (narrator *countingNarrator) Name() string { return "counting" }

func (narrator *countingNarrator) Narrate(context.Context, []perception.Frame, trajectory.Snapshot) (string, error) {
	narrator.mu.Lock()
	defer narrator.mu.Unlock()
	narrator.calls++
	return narrator.text, nil
}

func TestVideoGateRejectsIdenticalFramesWithoutDecoding(t *testing.T) {
	narrator := &countingNarrator{text: "A settings page is open."}
	observer, err := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: narrator, Cadence: 0,
	})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	payload := encodeFrame(t, color.Gray{Y: 20}, image.Rect(0, 0, 0, 0))
	ctx := context.Background()

	if !observer.Gate(imageFrame(payload, 0)) {
		t.Fatal("the first frame must be admitted")
	}
	if _, err := observer.Observe(ctx, []perception.Frame{imageFrame(payload, 0)}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	// A screen that has not changed re-encodes to identical bytes, and that is
	// the case the cheap gate exists for.
	for index := 0; index < 10; index++ {
		if observer.Gate(imageFrame(payload, 0)) {
			t.Fatal("an identical frame must be rejected by the cheap gate")
		}
	}
	metrics := observer.Metrics()
	if metrics.Frames != 11 || metrics.Admitted != 1 {
		t.Fatalf("unexpected metrics %+v", metrics)
	}
	if narrator.calls != 1 {
		t.Fatalf("an idle stream must not reach the narrator, got %d calls", narrator.calls)
	}
}

func TestVideoObserverNarratesOnlyRealChange(t *testing.T) {
	narrator := &countingNarrator{text: "A confirmation dialog appeared: Confirm payment of $40."}
	observer, err := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: narrator, Cadence: 0, ChangeThreshold: 0.05,
	})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	ctx := context.Background()
	base := encodeFrame(t, color.Gray{Y: 20}, image.Rect(0, 0, 0, 0))
	if _, err := observer.Observe(ctx, []perception.Frame{imageFrame(base, 0)}); err != nil {
		t.Fatalf("observe base: %v", err)
	}

	// Re-encoding the same picture produces different bytes but the same
	// image, so the cheap gate lets it through and the pixel comparison stops
	// it. That is the second stage doing its job.
	similar := encodeFrame(t, color.Gray{Y: 21}, image.Rect(0, 0, 0, 0))
	observations, err := observer.Observe(ctx, []perception.Frame{imageFrame(similar, 0)})
	if err != nil {
		t.Fatalf("observe similar: %v", err)
	}
	if len(observations) != 0 {
		t.Fatalf("an unchanged screen produces no observation, got %+v", observations)
	}

	changed := encodeFrame(t, color.Gray{Y: 20}, image.Rect(60, 60, 260, 200))
	observations, err = observer.Observe(ctx, []perception.Frame{imageFrame(changed, 0)})
	if err != nil {
		t.Fatalf("observe changed: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("a real change must be narrated, got %d", len(observations))
	}
	observation := observations[0]
	if observation.Authority != trajectory.AuthorityObserver {
		t.Fatalf("screen content is observer authority, got %q", observation.Authority)
	}
	if observation.Source != "screen" || observation.Observer != "video" {
		t.Fatalf("unexpected provenance %+v", observation)
	}
	if len(observation.Media) != 0 {
		t.Fatal("narration-only observations carry no media")
	}
}

// TestVideoCadenceLimitsSampling drives the sampling interval from the
// observer's own clock rather than from the timestamps on the frames, which is
// what the observer actually reads. A client's capture time cannot be trusted
// to bound what this server spends.
func TestVideoCadenceLimitsSampling(t *testing.T) {
	simulated := clock.NewVirtual(uint64(time.Second))
	observer, err := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: "screen"}, Cadence: 333 * time.Millisecond,
		Now: simulated.NowNS,
	})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	base := encodeFrame(t, color.Gray{Y: 20}, image.Rect(0, 0, 0, 0))
	changed := encodeFrame(t, color.Gray{Y: 20}, image.Rect(60, 60, 260, 200))
	if _, err := observer.Observe(context.Background(), []perception.Frame{imageFrame(base, uint64(time.Second))}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if _, err := simulated.AdvanceNS(uint64(100 * time.Millisecond)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if observer.Gate(imageFrame(changed, uint64(time.Second+100*time.Millisecond))) {
		t.Fatal("a frame inside the sampling interval must be rejected")
	}
	if _, err := simulated.AdvanceNS(uint64(300 * time.Millisecond)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !observer.Gate(imageFrame(changed, uint64(time.Second+400*time.Millisecond))) {
		t.Fatal("a frame after the sampling interval must be admitted")
	}
}

// TestVideoCadenceIgnoresClientTimestamps is the defect this arrangement
// exists to prevent: a client that sends no timestamp at all, or the same one
// on every frame, must not be able to switch the sampling interval off or jam
// it shut.
func TestVideoCadenceIgnoresClientTimestamps(t *testing.T) {
	simulated := clock.NewVirtual(0)
	observer, err := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: "screen"}, Cadence: 333 * time.Millisecond,
		Now: simulated.NowNS,
	})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	base := encodeFrame(t, color.Gray{Y: 20}, image.Rect(0, 0, 0, 0))
	changed := encodeFrame(t, color.Gray{Y: 20}, image.Rect(60, 60, 260, 200))
	if _, err := observer.Observe(context.Background(), []perception.Frame{imageFrame(base, 0)}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	// No timestamps, no elapsed time: the interval still holds.
	for attempt := 0; attempt < 5; attempt++ {
		if observer.Gate(imageFrame(changed, 0)) {
			t.Fatal("an untimestamped burst must not bypass the sampling interval")
		}
	}
	if _, err := simulated.AdvanceNS(uint64(400 * time.Millisecond)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !observer.Gate(imageFrame(changed, 0)) {
		t.Fatal("an untimestamped frame after the interval must be admitted")
	}
}

type recordingRetainer struct {
	mu     sync.Mutex
	stored int
}

func (retainer *recordingRetainer) Retain(reference trajectory.MediaRef, payload []byte) (trajectory.MediaRef, error) {
	retainer.mu.Lock()
	defer retainer.mu.Unlock()
	retainer.stored++
	reference.Handle = "media-1"
	reference.Bytes = len(payload)
	return reference, nil
}

func TestKeyframeAttachmentIsAComposedFactor(t *testing.T) {
	retainer := &recordingRetainer{}
	observer, err := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: "A dialog appeared."},
		Retainer: retainer, AttachKeyframes: true, Cadence: 0,
	})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	payload := encodeFrame(t, color.Gray{Y: 20}, image.Rect(0, 0, 0, 0))
	observations, err := observer.Observe(context.Background(), []perception.Frame{imageFrame(payload, 0)})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if len(observations) != 1 || len(observations[0].Media) != 1 {
		t.Fatalf("expected one observation carrying one handle, got %+v", observations)
	}
	if observations[0].Media[0].Handle != "media-1" {
		t.Fatal("the observation must reference the retained frame by handle")
	}
	if retainer.stored != 1 {
		t.Fatalf("expected one retained keyframe, got %d", retainer.stored)
	}
}

func TestVideoObserverRequiresANarrator(t *testing.T) {
	_, err := perception.NewVideoObserver(perception.VideoConfig{})
	if err == nil {
		t.Fatal("narration is the value; a video observer without one must not start")
	}
	_, err = perception.NewVideoObserver(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: "x"}, AttachKeyframes: true,
	})
	if err == nil {
		t.Fatal("attaching keyframes without somewhere to put them must not start")
	}
}

type staticVision struct{ text string }

func (staticVision) Name() string { return "static-vision" }

func (vision staticVision) Describe(context.Context, []perception.Image, string) (string, error) {
	return vision.text, nil
}

func TestNarratorSuppressesNoChange(t *testing.T) {
	narrator, err := perception.NewSessionNarrator(staticVision{text: "no change."})
	if err != nil {
		t.Fatalf("new narrator: %v", err)
	}
	text, err := narrator.Narrate(context.Background(), []perception.Frame{
		{Kind: perception.FrameImage, Image: []byte("x"), MIMEType: "image/jpeg", Width: 1, Height: 1},
	}, trajectory.Snapshot{})
	if err != nil {
		t.Fatalf("narrate: %v", err)
	}
	if text != "" {
		t.Fatalf("a declining narrator must produce no observation, got %q", text)
	}
	if narrator.Metrics().Suppressed != 1 {
		t.Fatal("suppression must be counted: it is part of what makes an idle source cheap")
	}
	if narrator.Name() != "session:static-vision" {
		t.Fatalf("the name must record the composition, got %q", narrator.Name())
	}
}

func TestObserverSetRejectsDuplicateNames(t *testing.T) {
	first, _ := perception.NewVideoObserver(perception.VideoConfig{Narrator: perception.StaticNarrator{Text: "a"}})
	second, _ := perception.NewVideoObserver(perception.VideoConfig{Narrator: perception.StaticNarrator{Text: "b"}})
	if _, err := perception.NewSet(first, second); err == nil {
		t.Fatal("a report must be able to name which observer produced what")
	}
	set, err := perception.NewSet(first)
	if err != nil {
		t.Fatalf("new set: %v", err)
	}
	if names := set.Names(); len(names) != 1 || names[0] != "video" {
		t.Fatalf("unexpected set %v", names)
	}
	if len(set.For(perception.Frame{Kind: perception.FrameAudio, Source: "microphone"})) != 0 {
		t.Fatal("a video observer must not be handed audio")
	}
}

func TestMeasuredLevelsParse(t *testing.T) {
	for _, level := range []string{"audio", "audio+video", "video"} {
		if _, err := perception.ParseObserverSet(level); err != nil {
			t.Fatalf("observer set %q: %v", level, err)
		}
	}
	if _, err := perception.ParseObserverSet("everything"); err == nil {
		t.Fatal("expected an unknown observer set to be rejected")
	}
	for _, level := range []string{"keyframe+narration", "narration", "keyframe"} {
		if _, err := perception.ParseComponents(level); err != nil {
			t.Fatalf("components %q: %v", level, err)
		}
	}
	if _, err := perception.ParseComponents("magic"); err == nil {
		t.Fatal("expected unknown components to be rejected")
	}
}

func TestObservationValidationRejectsForgedProvenance(t *testing.T) {
	if err := (perception.Observation{Text: "x", Observer: "video"}).Validate(); err == nil {
		t.Fatal("an observation without authority must be rejected")
	}
	if err := (perception.Observation{
		Text: "x", Observer: "video", Authority: trajectory.AuthorityObserver,
		Revision: 2, Supersedes: 3,
	}).Validate(); err == nil {
		t.Fatal("supersession must name an older revision")
	}
	valid := perception.Observation{
		Text: "x", Observer: "video", Authority: trajectory.AuthorityObserver, Source: "screen",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid observation rejected: %v", err)
	}
	if valid.Producer().Phase != trajectory.PhaseObserver {
		t.Fatal("provenance must be derived from authority so the two cannot disagree")
	}
	if meta := valid.Meta(); meta == nil || meta.Authority != trajectory.AuthorityObserver {
		t.Fatal("observer observations must carry provenance into the trajectory")
	}
	speech := perception.Observation{Text: "hello", Observer: "audio", Authority: trajectory.AuthorityUser}
	if speech.Meta() != nil {
		t.Fatal("plain user speech needs no extra provenance")
	}
}

// A vision model is shown an image that may have been resized on the way, so
// it is asked for a fraction of the image rather than a pixel. Converting that
// back into the source's declared space is the observer's job, because the
// observer is the only party that knows the geometry.
func TestControlPositionsAreGroundedInTheSourceSpace(t *testing.T) {
	narration := "A confirmation dialog is open.\n" +
		"CONTROL: CANCEL at (445, 622)\n" +
		"CONTROL: CONFIRM at (617, 622)\n"
	observer, err := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: narration}, Cadence: 0,
	})
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	frame := imageFrame(encodeFrame(t, color.Gray{Y: 20}, image.Rect(0, 0, 0, 0)), 0)
	frame.Width, frame.Height = 1280, 720
	observations, err := observer.Observe(context.Background(), []perception.Frame{frame})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("expected one observation, got %d", len(observations))
	}
	text := observations[0].Text
	// 445/1000 of 1280 is 569; 622/1000 of 720 is 447.
	if !strings.Contains(text, "CONTROL: CANCEL at (569, 447)") {
		t.Fatalf("cancel was not grounded:\n%s", text)
	}
	if !strings.Contains(text, "CONTROL: CONFIRM at (789, 447)") {
		t.Fatalf("confirm was not grounded:\n%s", text)
	}
	if !strings.Contains(text, "A confirmation dialog is open.") {
		t.Fatal("the rest of the narration must survive untouched")
	}
}

func TestNarrationWithoutControlsIsUntouched(t *testing.T) {
	narration := "A settings page is open with a list of options."
	observer, _ := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: narration}, Cadence: 0,
	})
	frame := imageFrame(encodeFrame(t, color.Gray{Y: 30}, image.Rect(0, 0, 0, 0)), 0)
	frame.Width, frame.Height = 800, 600
	observations, err := observer.Observe(context.Background(), []perception.Frame{frame})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observations[0].Text != narration {
		t.Fatalf("unexpected rewrite: %q", observations[0].Text)
	}
}

func TestOutOfRangeControlPositionsAreClamped(t *testing.T) {
	observer, _ := perception.NewVideoObserver(perception.VideoConfig{
		Narrator: perception.StaticNarrator{Text: "CONTROL: EDGE at (1010, 1200)"}, Cadence: 0,
	})
	frame := imageFrame(encodeFrame(t, color.Gray{Y: 40}, image.Rect(0, 0, 0, 0)), 0)
	frame.Width, frame.Height = 100, 200
	observations, _ := observer.Observe(context.Background(), []perception.Frame{frame})
	// Clamped rather than dropped: a model that said 1010 meant the right
	// edge, and losing the line would lose a control the agent needs.
	if !strings.Contains(observations[0].Text, "CONTROL: EDGE at (99, 199)") {
		t.Fatalf("unexpected clamping: %q", observations[0].Text)
	}
}

// A transient is loud and short. A door, a keyboard, a lip smack all clear an
// energy threshold, and the gate had hysteresis on one side only: half a
// second of silence to believe a turn ended, one block above the threshold to
// believe one began.
func TestATransientDoesNotOpenATurn(t *testing.T) {
	gate, err := perception.NewEnergyGate(perception.DefaultGateConfig(), 24_000)
	if err != nil {
		t.Fatal(err)
	}
	block := func(amplitude byte, count int) []byte {
		audio := make([]byte, count)
		for index := 0; index < count; index += 2 {
			audio[index], audio[index+1] = 0x00, amplitude
		}
		return audio
	}
	// 40 ms of noise: loud, and over before a syllable would be.
	if result, _ := gate.Push(block(0x40, 1920)); result.Started {
		t.Fatal("a click opened a turn")
	}
	// Silence after it, which is what makes it a transient rather than speech.
	for range 3 {
		if result, _ := gate.Push(block(0x00, 1920)); result.Started {
			t.Fatal("silence after a click opened a turn")
		}
	}
	if gate.Speaking() {
		t.Fatal("the gate is open after nothing but a click")
	}
}

// The onset is only delayed, never discarded: what the threshold buys is time
// to decide, and the audio that justified the decision has to arrive with it.
func TestSustainedSpeechOpensATurnWithItsOnsetIntact(t *testing.T) {
	gate, err := perception.NewEnergyGate(perception.DefaultGateConfig(), 24_000)
	if err != nil {
		t.Fatal(err)
	}
	loud := make([]byte, 1920) // 40 ms
	for index := 0; index < len(loud); index += 2 {
		loud[index], loud[index+1] = 0x00, 0x40
	}
	var opened perception.GateResult
	for range 5 {
		result, err := gate.Push(loud)
		if err != nil {
			t.Fatal(err)
		}
		if result.Started {
			opened = result
			break
		}
	}
	if !opened.Started {
		t.Fatal("sustained speech never opened a turn")
	}
	// Everything voiced before the decision is in the audio it hands over.
	if len(opened.Audio) < 3*len(loud) {
		t.Fatalf("the onset was clipped: %d bytes for %d ms of speech", len(opened.Audio), 120)
	}
}

// A recogniser asked about audio with no words in it still answers. SenseVoice
// says ".", and an observation is a claim that the user said something.
func TestATranscriptWithNoWordsIsNotAnObservation(t *testing.T) {
	for _, text := range []string{".", " . ", "。", "…", "-", "!?", "  "} {
		if perception.CarriesSpeech(text) {
			t.Errorf("%q has no words in it", text)
		}
	}
	for _, text := range []string{"hello", "嗯", "42", "ABC123", "a."} {
		if !perception.CarriesSpeech(text) {
			t.Errorf("%q is something the user said", text)
		}
	}
}

// The whole path, not just the predicate: a recogniser that reports no words
// must not put a turn in the log.
//
// This is what room tone does to a session. The gate opens on something, the
// recogniser is asked what was in it and answers ".", and an observation says
// the user spoke - so the agent answers, and that answer cancels whatever it
// was already saying.
func TestARecogniserReportingNoWordsCommitsNoObservation(t *testing.T) {
	asr := &revisionASR{
		revisions: []v1.PerceptionRevision{{StableText: "."}, {StableText: "。"}, {StableText: " "}},
		final:     ".",
	}
	observer, err := perception.NewAudioObserver(perception.AudioConfig{
		Provider: func() (v1.PerceptionProvider, error) { return asr, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for index := range 3 {
		observations, err := observer.Observe(ctx, []perception.Frame{audioFrame()})
		if err != nil {
			t.Fatalf("observe %d: %v", index, err)
		}
		if len(observations) != 0 {
			t.Fatalf("punctuation became a turn: %+v", observations)
		}
	}
	final, err := observer.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 0 {
		t.Fatalf("a final transcript with no words is still no words: %+v", final)
	}
}
