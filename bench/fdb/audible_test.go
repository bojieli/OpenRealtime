package fdb

import (
	"encoding/binary"
	"math"
	"os"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/audio"
)

func toneAt(rate int, silentMS, toneMS float64) []int16 {
	total := int((silentMS + toneMS) * float64(rate) / 1000.0)
	samples := make([]int16, total)
	begin := int(silentMS * float64(rate) / 1000.0)
	for index := begin; index < total; index++ {
		samples[index] = int16(8000 * math.Sin(2*math.Pi*220*float64(index)/float64(rate)))
	}
	return samples
}

func TestTheFirstSoundIsFoundWhereItWasPut(t *testing.T) {
	rate := 24_000
	samples := toneAt(rate, 640, 1000)
	lead, found := audibleAfterMS(samples, rate, 0)
	if !found {
		t.Fatal("a recording with a second of tone in it reported no sound")
	}
	if lead < 620 || lead > 660 {
		t.Fatalf("first sound at %.0f ms, want the 640 ms it was written at", lead)
	}
	// Measured from inside the tone, the answer is zero rather than the same
	// offset again.
	from, found := audibleAfterMS(samples, rate, 800)
	if !found || from > audibleBlockMS {
		t.Fatalf("measured from inside the speech: %.0f ms, found=%v", from, found)
	}
}

func TestSilenceThroughoutIsReportedRatherThanGuessedAt(t *testing.T) {
	rate := 24_000
	if _, found := audibleAfterMS(make([]int16, rate), rate, 0); found {
		t.Fatal("a second of digital silence was reported as sound")
	}
	if _, found := audibleAfterMS(nil, rate, 0); found {
		t.Fatal("no audio at all was reported as sound")
	}
	if _, found := audibleAfterMS(toneAt(rate, 100, 100), 0, 0); found {
		t.Fatal("a sample rate of zero produced an answer")
	}
}

// The window has to open where the person starts making a sound. The event
// clips in this dataset begin with the speaker's own pause - a median of 260 ms
// in the interruption category and up to 1,460 ms - and the category is scored
// against a one-second deadline, so timing from the annotation charges the
// agent for silence it could not have reacted to.
func TestTheYieldWindowOpensAtTheFirstSoundNotAtTheAnnotation(t *testing.T) {
	// The agent is speaking through the annotation and stops 1,150 ms after
	// it: too late by the annotation, comfortably in time by the sound that
	// arrives 600 ms later.
	var moments []bench.Moment
	for at := 4_000.0; at <= 6_150.0; at += 50 {
		moments = append(moments, bench.Moment{AtMS: at, Kind: bench.MomentAgentAudio, AudioMS: 50})
	}
	transcript := bench.Transcript{Moments: moments}
	base := attemptContext{
		Category: Interruption, ShouldYield: true, EventStartMS: 5_000,
		YieldWindowMS: 1_000, HoldWindowMS: 1_000,
	}

	byAnnotation := bench.TaskOutcome{Notes: map[string]string{}}
	scoreOutcome(&byAnnotation, transcript, base)
	if byAnnotation.Passed {
		t.Fatalf("an agent that kept going for 1,150 ms passed a 1,000 ms window: %v", byAnnotation.Metrics)
	}

	withLead := base
	withLead.EventAudibleAfterMS = 600
	byFirstSound := bench.TaskOutcome{Notes: map[string]string{}}
	scoreOutcome(&byFirstSound, transcript, withLead)
	if !byFirstSound.Passed {
		t.Fatalf("the same recording failed with its silence discounted: %v", byFirstSound.Metrics)
	}
	if got := byFirstSound.Metrics["yield_latency_ms"]; got < 500 || got > 600 {
		t.Fatalf("yield latency from the first sound = %.0f ms, want about 550", got)
	}
	if got := byFirstSound.Metrics["yield_latency_from_annotation_ms"]; got < 1_100 || got > 1_200 {
		t.Fatalf("the annotation-relative latency was not retained: %.0f ms", got)
	}
	if got := byFirstSound.Metrics["event_audible_after_ms"]; got != 600 {
		t.Fatalf("the lead was not reported: %.0f ms", got)
	}
}

// The other three categories ask the agent to keep speaking, and their clips
// have almost no lead. Moving their window later must not make holding easier.
func TestHoldingIsMeasuredFromTheFirstSoundToo(t *testing.T) {
	// The agent stops 700 ms after the annotation, which is 200 ms after a
	// sound that arrives 500 ms late. It held through the annotation window
	// and must still be judged on the window that matters.
	var moments []bench.Moment
	for at := 4_000.0; at <= 5_700.0; at += 50 {
		moments = append(moments, bench.Moment{AtMS: at, Kind: bench.MomentAgentAudio, AudioMS: 50})
	}
	transcript := bench.Transcript{Moments: moments}
	retained := attemptContext{
		Category: Backchannel, ShouldYield: false, EventStartMS: 5_000,
		YieldWindowMS: 1_000, HoldWindowMS: 1_000, EventAudibleAfterMS: 500,
	}
	outcome := bench.TaskOutcome{Notes: map[string]string{}}
	scoreOutcome(&outcome, transcript, retained)
	if !outcome.Passed {
		t.Fatalf("two hundred milliseconds of held speech was not counted: %v", outcome.Metrics)
	}
	if got := outcome.Metrics["agent_audio_hold_window_ms"]; got < 150 || got > 250 {
		t.Fatalf("hold measured %.0f ms, want the 200 ms after the first sound", got)
	}
}

// writeFixtureWAV writes a recording whose event speech begins where the
// caller says. Fixtures used to hold the bytes "not a wav" because nothing
// read them; the suite now measures where each recording starts making sound,
// so a fixture has to be one.
func writeFixtureWAV(tb testing.TB, path string, silentMS, toneMS float64) {
	tb.Helper()
	const rate = 24_000
	samples := toneAt(rate, silentMS, toneMS)
	pcm := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(sample))
	}
	encoded, err := audio.EncodeWAVMono16(pcm, rate)
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		tb.Fatal(err)
	}
}
