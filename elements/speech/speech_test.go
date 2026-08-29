package speech_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const speechGraph = `graph speech_pipeline {
    speech.TTS      :: tts;
    speech.Playback :: playback;

    input text            = tts.text;
    input tts_cancel      = tts.cancel;
    input playback_cancel = playback.cancel;

    tts.audio -> playback.audio;

    output tts_status        = tts.status;
    output tts_outcome       = tts.outcome;
    output tts_resolved      = tts.resolved;
    output playback_status   = playback.status;
    output playback_outcome  = playback.outcome;
    output playback_resolved = playback.resolved;
}
`

var (
	providerDescriptor = v1.Descriptor{
		Name: "test-tts", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingOutput: true,
			v1.CapabilityCancellation:    true,
			v1.CapabilityPCM16Output:     true,
		},
	}
	sinkDescriptor = v1.Descriptor{
		Name: "test-speaker", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true,
			v1.CapabilityCancellation:   true,
		},
	}
)

func TestSpeechDescriptorsExposeSeparateTypedInterruptibleStages(t *testing.T) {
	t.Parallel()
	for _, descriptor := range speechelements.Descriptors() {
		if err := descriptor.Validate(); err != nil {
			t.Fatalf("%s descriptor: %v", descriptor.Name, err)
		}
		if descriptor.Reaction.MaxConcurrency != 1 ||
			!reflect.DeepEqual(descriptor.Reaction.Interrupts, []string{"cancel"}) {
			t.Fatalf("%s reaction = %+v", descriptor.Name, descriptor.Reaction)
		}
	}
	tts := speechelements.TTSDescriptor()
	playback := speechelements.PlaybackDescriptor()
	if port(t, tts, "text").Type.String() != "Trigger<speech.TextSegment>" ||
		port(t, tts, "audio").Type.String() != "Segmented<Prepared<speech.AudioFrame>, speech.UtteranceID>" ||
		!port(t, tts, "text").Required || port(t, tts, "text").Cardinality != element.One {
		t.Fatalf("unexpected TTS contract: %+v", tts.Ports)
	}
	if port(t, playback, "audio").Cardinality != element.One {
		t.Fatal("playback must have one writer; fan-in requires an explicit arbiter")
	}
	foundExternal := false
	for _, effect := range playback.Effects {
		foundExternal = foundExternal || effect.External && effect.Authority == "Prepared"
	}
	if !foundExternal {
		t.Fatalf("playback effects = %+v", playback.Effects)
	}
}

func TestSlowProducerSynthesizesAndPlaysThroughExplicitLedgerBoundary(t *testing.T) {
	provider := &scriptedProvider{chunks: testChunks("slow-1", 3, 32)}
	sink := newRecordingSink()
	fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
	defer fixture.stop(t)
	fixture.receiveResolutions(t)

	fixture.sendText(t, speechelements.TextSegment{
		ID: "slow-1", Text: "the deliberative result may speak directly",
		Producer: "deliberative", Phase: trajectory.PhaseSlow,
		// "silent" is legacy provider-resolution provenance. The explicit graph
		// edge into Prepared speech, not this label, authorizes output.
		SpeechAuthority: "silent",
	})
	ttsOutcome := receive(t, fixture.egress(t, "tts_outcome")).Payload.(speechelements.SynthesisOutcome)
	playbackOutcome := receive(t, fixture.egress(t, "playback_outcome")).Payload.(speechelements.PlaybackOutcome)
	if ttsOutcome.Kind != speechelements.OutcomeSucceeded || ttsOutcome.Chunks != 3 {
		t.Fatalf("synthesis outcome = %+v", ttsOutcome)
	}
	if playbackOutcome.Kind != speechelements.OutcomeSucceeded ||
		!playbackOutcome.CrossedBoundary || playbackOutcome.PlayedNS == 0 {
		t.Fatalf("playback outcome = %+v", playbackOutcome)
	}
	assertStates(t, fixture.egress(t, "tts_status"), 2,
		speechelements.StateGenerating, speechelements.StateGenerated)
	assertStates(t, fixture.egress(t, "playback_status"), 3,
		speechelements.StateQueued, speechelements.StateEmitting, speechelements.StatePlayed)
	commitment, found := fixture.ledger.Lookup("slow-1")
	if !found || commitment.State != action.StatePlayed ||
		commitment.SpeechAuthority != "silent" || commitment.Phase != trajectory.PhaseSlow {
		t.Fatalf("commitment = %+v, found %v", commitment, found)
	}
	begin, frames, end := sink.snapshot()
	if len(begin) != 1 || len(frames) < 3 || len(end) != 1 || !end[0].Completed {
		t.Fatalf("sink begin=%d frames=%d end=%+v", len(begin), len(frames), end)
	}
}

func TestNonStreamingTTSUsesTheSameFramedPlaybackContract(t *testing.T) {
	descriptor := cloneDescriptor(providerDescriptor)
	delete(descriptor.Capabilities, v1.CapabilityStreamingOutput)
	provider := &batchProvider{descriptor: descriptor, chunks: testChunks("batch", 2, 32)}
	sink := newRecordingSink()
	fixture := mountSpeech(t, descriptor, func() v1.SpeechProvider { return provider },
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
	defer fixture.stop(t)
	fixture.receiveResolutions(t)
	fixture.sendText(t, speechelements.TextSegment{ID: "batch", Text: "batch provider"})
	if outcome := receive(t, fixture.egress(t, "tts_outcome")).Payload.(speechelements.SynthesisOutcome); outcome.Kind != speechelements.OutcomeSucceeded || outcome.Chunks != 2 {
		t.Fatalf("batch synthesis outcome = %+v", outcome)
	}
	if outcome := receive(t, fixture.egress(t, "playback_outcome")).Payload.(speechelements.PlaybackOutcome); outcome.Kind != speechelements.OutcomeSucceeded {
		t.Fatalf("batch playback outcome = %+v", outcome)
	}
}

func TestCancellationBeforeFirstAudioIsReversible(t *testing.T) {
	provider := newBlockingProvider(false)
	sink := newRecordingSink()
	fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
	defer fixture.stop(t)
	fixture.receiveResolutions(t)
	fixture.sendText(t, speechelements.TextSegment{ID: "before", Text: "never heard", Producer: "fast"})
	waitSignal(t, provider.entered, "provider did not begin")
	waitUntil(t, time.Second, func() bool {
		commitment, found := fixture.ledger.Lookup("before")
		return found && commitment.State == action.StateQueued
	})
	fixture.sendCancel(t, "playback_cancel", "before", "barge-in")
	fixture.sendCancel(t, "tts_cancel", "before", "barge-in")

	playbackOutcome := receiveMatchingPlayback(t, fixture.egress(t, "playback_outcome"), "before")
	if playbackOutcome.Kind != speechelements.OutcomeCancelled || playbackOutcome.CrossedBoundary ||
		playbackOutcome.Code != "cancelled_before_commitment" {
		t.Fatalf("playback outcome = %+v", playbackOutcome)
	}
	ttsOutcome := receiveMatchingSynthesis(t, fixture.egress(t, "tts_outcome"), "before")
	if ttsOutcome.Kind != speechelements.OutcomeCancelled {
		t.Fatalf("synthesis outcome = %+v", ttsOutcome)
	}
	assertStates(t, fixture.egress(t, "tts_status"), 2,
		speechelements.StateGenerating, speechelements.StateCancelled)
	playbackTransitions := receiveTransitions(t, fixture.egress(t, "playback_status"), 2)
	if playbackTransitions[0].State != speechelements.StateQueued ||
		playbackTransitions[1].State != speechelements.StateCancelled ||
		playbackTransitions[1].CrossedBoundary {
		t.Fatalf("playback transitions = %+v", playbackTransitions)
	}
	commitment, _ := fixture.ledger.Lookup("before")
	if commitment.State != action.StateCancelled || commitment.PlayedMS != 0 {
		t.Fatalf("commitment after reversible cancellation = %+v", commitment)
	}
	_, frames, end := sink.snapshot()
	if len(frames) != 0 || len(end) != 1 || end[0].Completed || end[0].PlayedMS != 0 {
		t.Fatalf("sink frames=%d end=%+v", len(frames), end)
	}
}

func TestCancellationAfterFirstAudioRecordsIrreversiblePartialPlayback(t *testing.T) {
	provider := newBlockingProvider(true)
	sink := newRecordingSink()
	fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
	defer fixture.stop(t)
	fixture.receiveResolutions(t)
	fixture.sendText(t, speechelements.TextSegment{ID: "after", Text: "partly heard", Producer: "deliberative"})
	waitSignal(t, sink.audioDelivered, "first audio was not handed to sink")
	fixture.sendCancel(t, "playback_cancel", "after", "user correction")
	fixture.sendCancel(t, "tts_cancel", "after", "user correction")

	playbackOutcome := receiveMatchingPlayback(t, fixture.egress(t, "playback_outcome"), "after")
	if playbackOutcome.Kind != speechelements.OutcomeCancelled || !playbackOutcome.CrossedBoundary ||
		playbackOutcome.PlayedNS == 0 || playbackOutcome.Code != "cancelled_after_commitment" {
		t.Fatalf("playback outcome = %+v", playbackOutcome)
	}
	playbackTransitions := receiveTransitions(t, fixture.egress(t, "playback_status"), 3)
	if playbackTransitions[0].State != speechelements.StateQueued ||
		playbackTransitions[1].State != speechelements.StateEmitting ||
		playbackTransitions[2].State != speechelements.StateCancelled ||
		!playbackTransitions[2].CrossedBoundary {
		t.Fatalf("playback transitions = %+v", playbackTransitions)
	}
	commitment, _ := fixture.ledger.Lookup("after")
	// The test frame is shorter than one millisecond. State, not rounded
	// PlayedMS, proves the irreversible transition; nanoseconds stay exact in
	// the graph outcome.
	if commitment.State != action.StatePlayed {
		t.Fatalf("commitment after irreversible cancellation = %+v", commitment)
	}
	_, frames, end := sink.snapshot()
	if len(frames) == 0 || len(end) != 1 || end[0].Completed {
		t.Fatalf("sink frames=%d end=%+v", len(frames), end)
	}
}

func TestStrictValuesLiveResolutionAndLifecycleCleanup(t *testing.T) {
	provider := &scriptedProvider{chunks: testChunks("unused", 1, 32)}
	sink := newRecordingSink()
	fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
	fixture.receiveResolutions(t)
	fixture.stop(t)
	if provider.closed.Load() != 1 || sink.closed.Load() != 1 {
		t.Fatalf("close counts provider=%d sink=%d", provider.closed.Load(), sink.closed.Load())
	}

	for name, values := range map[string]map[string]json.RawMessage{
		"unknown TTS field": {
			"tts":      json.RawMessage(`{"provider":"primary","mystery":true}`),
			"playback": json.RawMessage(`{"sink":"speaker"}`),
		},
		"unbounded playback chunk": {
			"tts":      json.RawMessage(`{"provider":"primary"}`),
			"playback": json.RawMessage(`{"sink":"speaker","max_chunk_bytes":999999999}`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := mountWithValues(providerDescriptor, func() v1.SpeechProvider {
				return &scriptedProvider{chunks: testChunks("x", 1, 2)}
			}, sinkDescriptor, func() speechelements.PlaybackSink { return newRecordingSink() }, values, nil)
			if err == nil {
				t.Fatal("invalid values mounted")
			}
		})
	}

	drifted := providerDescriptor
	drifted.Version = "drifted"
	_, err := mountWithValues(providerDescriptor, func() v1.SpeechProvider {
		return &scriptedProvider{descriptor: drifted}
	}, sinkDescriptor, func() speechelements.PlaybackSink { return newRecordingSink() }, defaultValues(), nil)
	if err == nil || !strings.Contains(err.Error(), "descriptor drifted") {
		t.Fatalf("provider drift error = %v", err)
	}
}

func TestLifecycleReportsProviderAndSinkCloseFailures(t *testing.T) {
	provider := &scriptedProvider{
		chunks: testChunks("unused", 1, 32), closeErr: errors.New("provider close failed"),
	}
	sink := newRecordingSink()
	sink.closeErr = errors.New("sink close failed")
	fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
	fixture.receiveResolutions(t)
	fixture.stopped.Store(true)
	fixture.cancel()
	select {
	case err := <-fixture.done:
		if err == nil || !strings.Contains(err.Error(), "provider close failed") ||
			!strings.Contains(err.Error(), "sink close failed") {
			t.Fatalf("shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("speech graph did not report lifecycle failures")
	}
}

func TestOversizedProviderChunkFailsWithoutCrossingPlaybackBoundary(t *testing.T) {
	provider := &scriptedProvider{chunks: testChunks("large", 1, 64)}
	sink := newRecordingSink()
	values := defaultValues()
	values["tts"] = json.RawMessage(`{"provider":"primary","max_chunk_bytes":16}`)
	fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, values)
	defer fixture.stop(t)
	fixture.receiveResolutions(t)
	fixture.sendText(t, speechelements.TextSegment{ID: "large", Text: "bounded"})
	ttsOutcome := receive(t, fixture.egress(t, "tts_outcome")).Payload.(speechelements.SynthesisOutcome)
	playbackOutcome := receive(t, fixture.egress(t, "playback_outcome")).Payload.(speechelements.PlaybackOutcome)
	if ttsOutcome.Kind != speechelements.OutcomeFailed ||
		playbackOutcome.Kind != speechelements.OutcomeFailed || playbackOutcome.CrossedBoundary {
		t.Fatalf("tts=%+v playback=%+v", ttsOutcome, playbackOutcome)
	}
	_, frames, _ := sink.snapshot()
	if len(frames) != 0 {
		t.Fatalf("oversized provider output emitted %d frames", len(frames))
	}
}

type speechFixture struct {
	mounted *graphruntime.Mounted
	cancel  context.CancelFunc
	done    <-chan error
	ledger  *action.Ledger
	stopped atomic.Bool
}

func mountSpeech(
	t *testing.T,
	provider v1.Descriptor, providerFactory func() v1.SpeechProvider,
	sink v1.Descriptor, sinkFactory func() speechelements.PlaybackSink,
	values map[string]json.RawMessage,
) *speechFixture {
	t.Helper()
	mounted, err := mountWithValues(provider, providerFactory, sink, sinkFactory, values, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := mounted.ledger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.graph.Run(ctx) }()
	return &speechFixture{mounted: mounted.graph, cancel: cancel, done: done, ledger: ledger}
}

type mountedSpeech struct {
	graph  *graphruntime.Mounted
	ledger *action.Ledger
}

func mountWithValues(
	provider v1.Descriptor, providerFactory func() v1.SpeechProvider,
	sink v1.Descriptor, sinkFactory func() speechelements.PlaybackSink,
	values map[string]json.RawMessage, extraServices func(*graphruntime.ServiceSet),
) (*mountedSpeech, error) {
	providers := speechelements.NewTTSProviderRegistry()
	if err := providers.Register("primary", provider, func() (v1.SpeechProvider, error) {
		return providerFactory(), nil
	}); err != nil {
		return nil, err
	}
	sinks := speechelements.NewPlaybackSinkRegistry()
	if err := sinks.Register("speaker", sink, func() (speechelements.PlaybackSink, error) {
		return sinkFactory(), nil
	}); err != nil {
		return nil, err
	}
	registry := graphruntime.NewRegistry()
	if err := speechelements.RegisterFactories(registry); err != nil {
		return nil, err
	}
	services := graphruntime.NewServiceSet()
	ledger := action.NewLedger()
	for name, service := range map[string]any{
		speechelements.TTSProviderRegistryService:   providers,
		speechelements.PlaybackSinkRegistryService:  sinks,
		speechelements.IrreversibilityLedgerService: ledger,
	} {
		if _, err := services.Set(name, service); err != nil {
			return nil, err
		}
	}
	if extraServices != nil {
		extraServices(services)
	}
	if values == nil {
		values = defaultValues()
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compileSpeechGraph(), Registry: registry, Services: services, Values: values,
	})
	if err != nil {
		return nil, err
	}
	return &mountedSpeech{graph: mounted, ledger: ledger}, nil
}

func compileSpeechGraph() ir.Graph {
	parsed, err := syntax.Parse("speech.ortg", []byte(speechGraph))
	if err != nil {
		panic(err)
	}
	catalog := resolve.NewCatalog()
	if err := speechelements.RegisterDescriptors(catalog); err != nil {
		panic(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		panic(err)
	}
	return compiled.Graph
}

func defaultValues() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"tts":      json.RawMessage(`{"provider":"primary"}`),
		"playback": json.RawMessage(`{"sink":"speaker","frame_duration_ms":1}`),
	}
}

func (fixture *speechFixture) ingress(t *testing.T, name string) element.OutputPort {
	t.Helper()
	port, err := fixture.mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (fixture *speechFixture) egress(t *testing.T, name string) element.InputPort {
	t.Helper()
	port, err := fixture.mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (fixture *speechFixture) receiveResolutions(t *testing.T) {
	t.Helper()
	provider := receive(t, fixture.egress(t, "tts_resolved")).Payload.(speechelements.ProviderResolution)
	sink := receive(t, fixture.egress(t, "playback_resolved")).Payload.(speechelements.SinkResolution)
	if provider.Reference != "primary" || sink.Reference != "speaker" {
		t.Fatalf("resolutions provider=%+v sink=%+v", provider, sink)
	}
}

func (fixture *speechFixture) sendText(t *testing.T, segment speechelements.TextSegment) {
	t.Helper()
	_, err := fixture.ingress(t, "text").Broadcast(context.Background(), element.Envelope{
		Type: speechelements.TextSegmentType(), ItemID: "text-" + segment.ID,
		RunID: segment.ID, CancellationScope: segment.ID, Payload: segment,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture *speechFixture) sendCancel(t *testing.T, boundary, id, reason string) {
	t.Helper()
	_, err := fixture.ingress(t, boundary).Broadcast(context.Background(), element.Envelope{
		Type: speechelements.CancelType(), ItemID: fmt.Sprintf("%s-%s", boundary, id),
		CancellationScope: id,
		Payload:           speechelements.Cancel{UtteranceID: id, Reason: reason},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture *speechFixture) stop(t *testing.T) {
	t.Helper()
	if !fixture.stopped.CompareAndSwap(false, true) {
		return
	}
	fixture.cancel()
	select {
	case err := <-fixture.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("speech graph stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("speech graph did not stop")
	}
}

func receive(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func assertStates(t *testing.T, input element.InputPort, count int, expected ...speechelements.State) {
	t.Helper()
	transitions := receiveTransitions(t, input, count)
	states := make([]speechelements.State, len(transitions))
	for index := range transitions {
		states[index] = transitions[index].State
	}
	if !reflect.DeepEqual(states, expected) {
		t.Fatalf("states = %v, want %v", states, expected)
	}
}

func receiveTransitions(
	t *testing.T, input element.InputPort, count int,
) []speechelements.Transition {
	t.Helper()
	transitions := make([]speechelements.Transition, 0, count)
	for range count {
		transitions = append(transitions, receive(t, input).Payload.(speechelements.Transition))
	}
	return transitions
}

func receiveMatchingPlayback(
	t *testing.T, input element.InputPort, utteranceID string,
) speechelements.PlaybackOutcome {
	t.Helper()
	for range 4 {
		outcome := receive(t, input).Payload.(speechelements.PlaybackOutcome)
		if outcome.UtteranceID == utteranceID && outcome.Kind != speechelements.OutcomeIgnored {
			return outcome
		}
	}
	t.Fatalf("no terminal playback outcome for %q", utteranceID)
	return speechelements.PlaybackOutcome{}
}

func receiveMatchingSynthesis(
	t *testing.T, input element.InputPort, utteranceID string,
) speechelements.SynthesisOutcome {
	t.Helper()
	for range 4 {
		outcome := receive(t, input).Payload.(speechelements.SynthesisOutcome)
		if outcome.UtteranceID == utteranceID && outcome.Kind != speechelements.OutcomeIgnored {
			return outcome
		}
	}
	t.Fatalf("no terminal synthesis outcome for %q", utteranceID)
	return speechelements.SynthesisOutcome{}
}

func port(t *testing.T, descriptor element.Descriptor, name string) element.Port {
	t.Helper()
	for _, candidate := range descriptor.Ports {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("%s has no port %s", descriptor.Name, name)
	return element.Port{}
}

func waitSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

type scriptedProvider struct {
	descriptor v1.Descriptor
	chunks     []v1.SpeechChunk
	closed     atomic.Int32
	closeErr   error
}

func (provider *scriptedProvider) Descriptor() v1.Descriptor {
	if provider.descriptor.Name != "" {
		return cloneDescriptor(provider.descriptor)
	}
	return cloneDescriptor(providerDescriptor)
}

func (provider *scriptedProvider) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}

func (provider *scriptedProvider) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	for _, source := range provider.chunks {
		chunk := source
		chunk.CandidateID = plan.CandidateID
		chunk.ChunkID = fmt.Sprintf("%s-%s", plan.CandidateID, source.ChunkID)
		chunk.PCM16LE = append([]byte(nil), source.PCM16LE...)
		if err := emit(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (provider *scriptedProvider) Close() error {
	provider.closed.Add(1)
	return provider.closeErr
}

type batchProvider struct {
	descriptor v1.Descriptor
	chunks     []v1.SpeechChunk
}

func (provider *batchProvider) Descriptor() v1.Descriptor {
	return cloneDescriptor(provider.descriptor)
}

func (provider *batchProvider) Synthesize(
	_ context.Context, plan v1.SpeechPlan,
) ([]v1.SpeechChunk, error) {
	chunks := make([]v1.SpeechChunk, len(provider.chunks))
	for index, source := range provider.chunks {
		chunks[index] = source
		chunks[index].CandidateID = plan.CandidateID
		chunks[index].ChunkID = fmt.Sprintf("%s-%d", plan.CandidateID, index)
		chunks[index].PCM16LE = append([]byte(nil), source.PCM16LE...)
	}
	return chunks, nil
}

type blockingProvider struct {
	emitFirst bool
	entered   chan struct{}
	once      sync.Once
	closed    atomic.Int32
}

func newBlockingProvider(emitFirst bool) *blockingProvider {
	return &blockingProvider{emitFirst: emitFirst, entered: make(chan struct{})}
}

func (*blockingProvider) Descriptor() v1.Descriptor { return cloneDescriptor(providerDescriptor) }
func (provider *blockingProvider) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}
func (provider *blockingProvider) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	if provider.emitFirst {
		if err := emit(v1.SpeechChunk{
			ChunkID: plan.CandidateID + "-1", CandidateID: plan.CandidateID,
			SampleRateHz: 16_000, PCM16LE: make([]byte, 16),
		}); err != nil {
			return err
		}
	}
	provider.once.Do(func() { close(provider.entered) })
	<-ctx.Done()
	return context.Cause(ctx)
}
func (provider *blockingProvider) Close() error { provider.closed.Add(1); return nil }

func testChunks(id string, count, bytesPerChunk int) []v1.SpeechChunk {
	chunks := make([]v1.SpeechChunk, count)
	offset := uint64(0)
	for index := range chunks {
		chunks[index] = v1.SpeechChunk{
			ChunkID: fmt.Sprintf("%s-%d", id, index), CandidateID: id,
			SampleOffset: offset, SampleRateHz: 16_000,
			PCM16LE: make([]byte, bytesPerChunk), Final: index == count-1,
		}
		offset += uint64(bytesPerChunk / 2)
	}
	return chunks
}

type recordingSink struct {
	mu             sync.Mutex
	begins         []action.Utterance
	frames         []action.Frame
	ends           []action.Outcome
	audioDelivered chan struct{}
	audioOnce      sync.Once
	closed         atomic.Int32
	closeErr       error
}

func newRecordingSink() *recordingSink {
	return &recordingSink{audioDelivered: make(chan struct{})}
}

func (*recordingSink) Descriptor() v1.Descriptor { return cloneDescriptor(sinkDescriptor) }
func (sink *recordingSink) Begin(_ context.Context, utterance action.Utterance) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.begins = append(sink.begins, utterance)
	return nil
}
func (sink *recordingSink) Audio(_ context.Context, _ action.Utterance, frame action.Frame) error {
	frame.PCM16LE = append([]byte(nil), frame.PCM16LE...)
	sink.mu.Lock()
	sink.frames = append(sink.frames, frame)
	sink.mu.Unlock()
	sink.audioOnce.Do(func() { close(sink.audioDelivered) })
	return nil
}
func (sink *recordingSink) End(_ context.Context, _ action.Utterance, outcome action.Outcome) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.ends = append(sink.ends, outcome)
	return nil
}
func (sink *recordingSink) Close() error {
	sink.closed.Add(1)
	return sink.closeErr
}
func (sink *recordingSink) snapshot() ([]action.Utterance, []action.Frame, []action.Outcome) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]action.Utterance(nil), sink.begins...),
		append([]action.Frame(nil), sink.frames...), append([]action.Outcome(nil), sink.ends...)
}

func cloneDescriptor(descriptor v1.Descriptor) v1.Descriptor {
	capabilities := make(v1.Capabilities, len(descriptor.Capabilities))
	for capability, enabled := range descriptor.Capabilities {
		capabilities[capability] = enabled
	}
	descriptor.Capabilities = capabilities
	return descriptor
}
