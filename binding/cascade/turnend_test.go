package cascade_test

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
)

// fixedTurnEnd answers every pause with the same probability and remembers
// the windows it was shown.
type fixedTurnEnd struct {
	mu          sync.Mutex
	probability float64
	windows     []int
}

func (classifier *fixedTurnEnd) Name() string { return "fixed-turn-end" }

func (classifier *fixedTurnEnd) Evaluate(_ context.Context, pcm16le []byte) (interaction.AcousticEndpoint, error) {
	classifier.mu.Lock()
	defer classifier.mu.Unlock()
	classifier.windows = append(classifier.windows, len(pcm16le))
	return interaction.AcousticEndpoint{Probability: classifier.probability, Model: "fixed"}, nil
}

func (classifier *fixedTurnEnd) consulted() []int {
	classifier.mu.Lock()
	defer classifier.mu.Unlock()
	return append([]int(nil), classifier.windows...)
}

// debugSink is a recording sink that also keeps debug events.
type debugSink struct {
	*recordingSink
	mu     sync.Mutex
	events []binding.DebugEvent
}

func (sink *debugSink) Debug(_ context.Context, event binding.DebugEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, event)
	return nil
}

func (sink *debugSink) named(name string) []binding.DebugEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var matching []binding.DebugEvent
	for _, event := range sink.events {
		if event.Name == name {
			matching = append(matching, event)
		}
	}
	return matching
}

// speakThenPause sends one second of speech and then silence in 100 ms frames,
// and returns how many milliseconds of silence passed before the turn ended
// (-1 when it had not ended after the whole pause).
func speakThenPause(t *testing.T, config cascade.Config, pauseMS int) (int, *debugSink) {
	t.Helper()
	config.Perception = func() (v1.PerceptionProvider, error) {
		return &scriptedASR{partials: []string{"I would", "I would like", "I would like to"}, final: "I would like to"}, nil
	}
	config.Fast, config.Slow = newFast(), newSlow()
	config.Speech = toneSpeech{chunks: 1}
	// The floor's bounds are on the session clock; a manual one advanced by
	// each frame's duration makes the pause measured here the pause it sees.
	manual := clock.NewManual(uint64(time.Second))
	config.Scheduler = manual
	bind, err := cascade.New(config)
	if err != nil {
		t.Fatal(err)
	}
	sink := &debugSink{recordingSink: &recordingSink{}}
	runtime, err := bind.Start(context.Background(), binding.Options{Sink: sink, SessionID: "turn-end"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	push := func(pcm []byte) {
		manual.AdvanceNS(uint64(100 * time.Millisecond))
		if err := runtime.Audio(context.Background(), perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000, PCM16LE: pcm,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range 10 {
		push(tone(2_400, 8_000))
	}
	stopped := func() bool {
		for _, event := range sink.activityEvents() {
			if event.Stopped {
				return true
			}
		}
		return false
	}
	for elapsed := 100; elapsed <= pauseMS; elapsed += 100 {
		push(silence(2_400))
		if stopped() {
			return elapsed, sink
		}
	}
	return -1, sink
}

func controllingPolicies(t *testing.T) interaction.Policies {
	t.Helper()
	projection, err := interaction.NewAcousticProjection(0.5)
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.TurnProjection = projection
	policies.Floor = interaction.NewEngineFloor(interaction.EngineFloorOptions{Projection: projection})
	return policies
}

func TestAFinishedSoundingPauseEndsTheTurnBeforeTheSilenceThreshold(t *testing.T) {
	classifier := &fixedTurnEnd{probability: 0.9}
	endedAfter, sink := speakThenPause(t, cascade.Config{
		TurnEnd: classifier, Policies: controllingPolicies(t), EndpointSilenceMS: 200,
	}, 1_000)
	if endedAfter < 0 || endedAfter >= 500 {
		t.Fatalf("turn ended after %d ms of silence; the classifier said it was complete at the first pause", endedAfter)
	}
	windows := classifier.consulted()
	if len(windows) == 0 || windows[0] < 16_000*2 {
		t.Fatalf("classifier windows %v; it must read the speech before the pause at 16 kHz", windows)
	}
	evidence := sink.named("turn_end.acoustic")
	if len(evidence) == 0 || evidence[0].Attributes["probability"] != 0.9 {
		t.Fatalf("the evidence was not recorded: %+v", evidence)
	}
}

func TestAnUnfinishedSoundingPauseIsHeldButOnlyAsLongAsTheBound(t *testing.T) {
	classifier := &fixedTurnEnd{probability: 0.1}
	endedAfter, _ := speakThenPause(t, cascade.Config{
		TurnEnd: classifier, Policies: controllingPolicies(t), EndpointSilenceMS: 200,
	}, 2_500)
	// The floor's silence threshold is 500 ms and the projection may hold one
	// second past it.
	if endedAfter < 1_000 || endedAfter > 1_700 {
		t.Fatalf("turn ended after %d ms of silence; want it held past 500 ms and ended by the 1.5 s bound", endedAfter)
	}
}

// Observation mode: the classifier is consulted and recorded, and the turn
// ends exactly where it would without it.
func TestObservedEvidenceChangesNoEndpoint(t *testing.T) {
	baseline, _ := speakThenPause(t, cascade.Config{Policies: interaction.Defaults(), EndpointSilenceMS: 200}, 1_500)
	classifier := &fixedTurnEnd{probability: 0.99}
	observed, sink := speakThenPause(t, cascade.Config{
		TurnEnd: classifier, Policies: interaction.Defaults(), EndpointSilenceMS: 200,
	}, 1_500)
	if baseline < 0 || observed != baseline {
		t.Fatalf("observation moved the endpoint from %d ms to %d ms", baseline, observed)
	}
	if len(classifier.consulted()) == 0 || len(sink.named("turn_end.acoustic")) == 0 {
		t.Fatal("observation mode must still consult and record the classifier")
	}
}
