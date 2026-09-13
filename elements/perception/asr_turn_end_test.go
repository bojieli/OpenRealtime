package perception_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

const asrTurnEndGraph = `graph graph_native_asr_turn_end {
    perception.ASR :: asr;
    input observe = asr.observe;
    input flush = asr.flush;
    input cancel = asr.cancel;
    output observations = asr.observations;
    output outcome = asr.outcome;
    output resolved = asr.resolved;
    output turn_end = asr.turn_end;
}
`

// turnASR reports Flux-shaped turn state: eager after the second frame,
// ended after the third. Each utterance starts again from nothing.
type turnASR struct {
	frames atomic.Int32
}

func (*turnASR) Descriptor() v1.Descriptor { return cloneDescriptor(testASRDescriptor) }
func (provider *turnASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	count := provider.frames.Add(1)
	words := []string{"count", "count the", "count the animals", "count the animals please"}
	text := words[min(int(count), len(words))-1]
	return []v1.PerceptionRevision{{RevisionID: uint64(count), UnstableText: text, Delta: text}}, nil
}
func (provider *turnASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 99, StableText: "count the animals", Final: true}, nil
}
func (provider *turnASR) EndUtterance() error {
	provider.frames.Store(0)
	return nil
}
func (provider *turnASR) EagerEndOfTurn() bool   { return provider.frames.Load() == 2 }
func (provider *turnASR) SpeechEndpointed() bool { return provider.frames.Load() >= 3 }

func mountTurnEndASR(t *testing.T, endOfTurn string, provider *turnASR) *graphruntime.Mounted {
	t.Helper()
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("asr.ortg", []byte(asrTurnEndGraph))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{Catalog: catalog, ResolutionMode: resolve.Update})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(perceptionelements.ASRProviderRegistryService, providers); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"provider": "primary", "source": "microphone"}
	if endOfTurn != "" {
		config["end_of_turn"] = endOfTurn
	}
	raw, _ := json.Marshal(config)
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compiled.Graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"asr": raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	t.Cleanup(func() { stopASR(t, mounted, done, cancel) })
	return mounted
}

func observeTurnFrame(t *testing.T, mounted *graphruntime.Mounted, stream, item string) {
	t.Helper()
	observe, _ := mounted.Ingress("observe")
	observations, _ := mounted.Egress("observations")
	outcomes, _ := mounted.Egress("outcome")
	frame := coreperception.Frame{
		Kind: coreperception.FrameAudio, Source: "microphone", CapturedNS: 100,
		PCM16LE: []byte{1, 0, 2, 0}, SampleRateHz: 16_000,
	}
	if _, err := observe.Broadcast(context.Background(), element.Envelope{
		Type: element.Trigger(element.Named("audio.FrameBatch")), ItemID: item, SourceID: stream,
		Payload: perceptionelements.AudioBatch{StreamID: stream, Frames: []coreperception.Frame{frame}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = receive(t, observations)
	_ = receive(t, outcomes)
}

func awaitTurnEnd(t *testing.T, port element.InputPort, within time.Duration) (perceptionelements.TurnEnd, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		return perceptionelements.TurnEnd{}, false
	}
	turnEnd, ok := envelope.Payload.(perceptionelements.TurnEnd)
	if !ok {
		t.Fatalf("turn end payload = %T", envelope.Payload)
	}
	return turnEnd, true
}

func TestASRReportsTheConfiguredTurnEndOncePerStream(t *testing.T) {
	provider := &turnASR{}
	mounted := mountTurnEndASR(t, perceptionelements.TurnEndSignalEager, provider)
	turnEnds, _ := mounted.Egress("turn_end")

	observeTurnFrame(t, mounted, "utterance-1", "frame-1")
	if turnEnd, found := awaitTurnEnd(t, turnEnds, 100*time.Millisecond); found {
		t.Fatalf("a turn end was reported before the recogniser gave one: %+v", turnEnd)
	}
	observeTurnFrame(t, mounted, "utterance-1", "frame-2")
	turnEnd, found := awaitTurnEnd(t, turnEnds, time.Second)
	if !found || turnEnd.StreamID != "utterance-1" || turnEnd.Signal != perceptionelements.TurnEndSignalEager {
		t.Fatalf("eager turn end = %+v, found %v", turnEnd, found)
	}
	// The recogniser then ends the turn outright; the stream was already
	// reported, so nothing more is said about it.
	observeTurnFrame(t, mounted, "utterance-1", "frame-3")
	if again, found := awaitTurnEnd(t, turnEnds, 100*time.Millisecond); found {
		t.Fatalf("one stream reported two turn ends: %+v", again)
	}
}

func TestASREndOfTurnWaitsForTheRecogniserToEndTheTurn(t *testing.T) {
	provider := &turnASR{}
	mounted := mountTurnEndASR(t, perceptionelements.TurnEndSignalEndOfTurn, provider)
	turnEnds, _ := mounted.Egress("turn_end")
	observeTurnFrame(t, mounted, "utterance-1", "frame-1")
	observeTurnFrame(t, mounted, "utterance-1", "frame-2")
	if turnEnd, found := awaitTurnEnd(t, turnEnds, 100*time.Millisecond); found {
		t.Fatalf("an eager signal ended an utterance configured for end_of_turn: %+v", turnEnd)
	}
	observeTurnFrame(t, mounted, "utterance-1", "frame-3")
	turnEnd, found := awaitTurnEnd(t, turnEnds, time.Second)
	if !found || turnEnd.Signal != perceptionelements.TurnEndSignalEndOfTurn {
		t.Fatalf("end of turn = %+v, found %v", turnEnd, found)
	}
}

// Without the setting the node behaves exactly as it did: no turn end, and
// the output need not even be connected.
func TestASRWithoutEndOfTurnReportsNothing(t *testing.T) {
	provider := &turnASR{}
	mounted := mountTurnEndASR(t, "", provider)
	turnEnds, _ := mounted.Egress("turn_end")
	for index := range 3 {
		observeTurnFrame(t, mounted, "utterance-1", "frame-"+string(rune('1'+index)))
	}
	if turnEnd, found := awaitTurnEnd(t, turnEnds, 150*time.Millisecond); found {
		t.Fatalf("an unconfigured ASR reported a turn end: %+v", turnEnd)
	}
}

func TestASRRefusesAnUnknownTurnEndSignal(t *testing.T) {
	parsed, err := syntax.Parse("asr.ortg", []byte(asrTurnEndGraph))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{Catalog: catalog, ResolutionMode: resolve.Update})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(perceptionelements.ASRProviderRegistryService, perceptionelements.NewASRProviderRegistry()); err != nil {
		t.Fatal(err)
	}
	_, err = graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compiled.Graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"asr": json.RawMessage(`{"provider":"primary","end_of_turn":"soon"}`)},
	})
	if err == nil {
		t.Fatal("an unknown end_of_turn signal was accepted")
	}
}
