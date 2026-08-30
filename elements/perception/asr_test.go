package perception_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

const asrGraph = `graph graph_native_asr {
    perception.ASR :: asr;
    input observe = asr.observe;
    input flush = asr.flush;
    input cancel = asr.cancel;
    output observations = asr.observations;
    output outcome = asr.outcome;
    output resolved = asr.resolved;
}
`

var testASRDescriptor = v1.Descriptor{
	Name: "test-asr", Version: "1",
	Capabilities: v1.Capabilities{
		v1.CapabilityStreamingInput: true, v1.CapabilityRevisions: true,
		v1.CapabilityCancellation: true,
	},
}

func TestASRElementStreamsRevisionsFlushesAndRecreatesPerUtterance(t *testing.T) {
	var created atomic.Int32
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) {
		created.Add(1)
		return &scriptedASR{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, runDone, cancelRun := mountASR(t, providers)
	defer stopASR(t, mounted, runDone, cancelRun)

	resolvedPort, _ := mounted.Egress("resolved")
	resolvedEnvelope := receive(t, resolvedPort)
	resolution, ok := resolvedEnvelope.Payload.(perceptionelements.ProviderResolution)
	if !ok || resolution.Reference != "primary" ||
		!reflect.DeepEqual(resolution.Descriptor, testASRDescriptor) {
		t.Fatalf("provider resolution = %#v", resolvedEnvelope.Payload)
	}
	assertASRLiveResolution(t, mounted)

	observe, _ := mounted.Ingress("observe")
	flush, _ := mounted.Ingress("flush")
	observations, _ := mounted.Egress("observations")
	outcomes, _ := mounted.Egress("outcome")
	observeType := element.Trigger(element.Named("audio.FrameBatch"))
	flushType := element.Trigger(element.Named("audio.Flush"))
	frame := coreperception.Frame{
		Kind: coreperception.FrameAudio, Source: "microphone", CapturedNS: 100,
		PCM16LE: []byte{1, 0, 2, 0}, SampleRateHz: 16_000,
	}
	if _, err := observe.Broadcast(context.Background(), element.Envelope{
		Type: observeType, ItemID: "observe-1", SourceID: "utterance-1",
		CancellationScope: "utterance-1",
		Payload:           perceptionelements.AudioBatch{StreamID: "utterance-1", Frames: []coreperception.Frame{frame}},
	}); err != nil {
		t.Fatal(err)
	}
	partialEnvelope := receive(t, observations)
	partial, ok := partialEnvelope.Payload.(coreperception.Observation)
	if !ok || partial.Text != "hello" || !partial.Provisional || partial.Final ||
		partialEnvelope.CaptureNS != 100 || partialEnvelope.OpportunityID != "observe-1" ||
		!slices.Contains(partialEnvelope.CausalParents, "observe-1") {
		t.Fatalf("partial = %#v, envelope = %+v", partialEnvelope.Payload, partialEnvelope)
	}
	firstOutcomeEnvelope := receive(t, outcomes)
	firstOutcome := firstOutcomeEnvelope.Payload.(perceptionelements.Outcome)
	if firstOutcome.Kind != perceptionelements.OutcomeSucceeded ||
		firstOutcome.Operation != "observe" || firstOutcome.ObservationCount != 1 ||
		firstOutcome.CauseItemID != "observe-1" || firstOutcomeEnvelope.OpportunityID != "observe-1" ||
		!slices.Contains(firstOutcomeEnvelope.CausalParents, "observe-1") {
		t.Fatalf("observe outcome = %+v / %+v", firstOutcome, firstOutcomeEnvelope)
	}

	if _, err := flush.Broadcast(context.Background(), element.Envelope{
		Type: flushType, ItemID: "flush-1", SourceID: "utterance-1",
		Payload: perceptionelements.Flush{StreamID: "utterance-1"},
	}); err != nil {
		t.Fatal(err)
	}
	finalEnvelope := receive(t, observations)
	final := finalEnvelope.Payload.(coreperception.Observation)
	if final.Text != "hello world" || !final.Final || final.Provisional || final.Supersedes == 0 ||
		finalEnvelope.OpportunityID != "flush-1" || !slices.Contains(finalEnvelope.CausalParents, "flush-1") {
		t.Fatalf("final observation = %+v / %+v", final, finalEnvelope)
	}
	flushOutcomeEnvelope := receive(t, outcomes)
	flushOutcome := flushOutcomeEnvelope.Payload.(perceptionelements.Outcome)
	if flushOutcome.Kind != perceptionelements.OutcomeSucceeded || flushOutcome.Operation != "flush" ||
		flushOutcome.CauseItemID != "flush-1" || flushOutcomeEnvelope.OpportunityID != "flush-1" ||
		!slices.Contains(flushOutcomeEnvelope.CausalParents, "flush-1") {
		t.Fatalf("flush outcome = %+v / %+v", flushOutcome, flushOutcomeEnvelope)
	}

	if _, err := observe.Broadcast(context.Background(), element.Envelope{
		Type: observeType, ItemID: "observe-2", SourceID: "utterance-2",
		Payload: perceptionelements.AudioBatch{StreamID: "utterance-2", Frames: []coreperception.Frame{frame}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = receive(t, observations)
	_ = receive(t, outcomes)
	if got := created.Load(); got != 2 {
		t.Fatalf("provider instances = %d, want one per utterance", got)
	}
}

func TestASRInterruptCancelsActiveProviderAndReportsTerminalOutcome(t *testing.T) {
	provider := &blockingASR{entered: make(chan struct{})}
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, runDone, cancelRun := mountASR(t, providers)
	defer stopASR(t, mounted, runDone, cancelRun)
	resolved, _ := mounted.Egress("resolved")
	_ = receive(t, resolved)

	observe, _ := mounted.Ingress("observe")
	cancelInput, _ := mounted.Ingress("cancel")
	outcomes, _ := mounted.Egress("outcome")
	frame := coreperception.Frame{
		Kind: coreperception.FrameAudio, Source: "microphone",
		PCM16LE: []byte{1, 0}, SampleRateHz: 16_000,
	}
	if _, err := observe.Broadcast(context.Background(), element.Envelope{
		Type: element.Trigger(element.Named("audio.FrameBatch")), ItemID: "observe-blocked",
		SourceID: "utterance-cancel", CancellationScope: "utterance-cancel",
		Payload: perceptionelements.AudioBatch{StreamID: "utterance-cancel", Frames: []coreperception.Frame{frame}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("provider invocation did not start")
	}
	if _, err := cancelInput.Broadcast(context.Background(), element.Envelope{
		Type: element.Interrupt(element.Named("audio.StreamID")), ItemID: "cancel-1",
		CancellationScope: "utterance-cancel",
		Payload:           perceptionelements.Cancel{StreamID: "utterance-cancel", Reason: "barge-in"},
	}); err != nil {
		t.Fatal(err)
	}
	outcome := receive(t, outcomes).Payload.(perceptionelements.Outcome)
	if outcome.Kind != perceptionelements.OutcomeCanceled || outcome.StreamID != "utterance-cancel" {
		t.Fatalf("cancel outcome = %+v", outcome)
	}
	if provider.closed.Load() != 1 {
		t.Fatalf("canceled provider close count = %d, want 1", provider.closed.Load())
	}
}

func TestASRElementRefusesMalformedOrCrossStreamWorkWithoutCrashing(t *testing.T) {
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) {
		return &scriptedASR{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, runDone, cancelRun := mountASR(t, providers)
	defer stopASR(t, mounted, runDone, cancelRun)
	resolved, _ := mounted.Egress("resolved")
	_ = receive(t, resolved)
	observe, _ := mounted.Ingress("observe")
	outcomes, _ := mounted.Egress("outcome")
	typeOf := element.Trigger(element.Named("audio.FrameBatch"))
	if _, err := observe.Broadcast(context.Background(), element.Envelope{
		Type: typeOf, ItemID: "bad", Payload: "not a batch",
	}); err != nil {
		t.Fatal(err)
	}
	malformed := receive(t, outcomes).Payload.(perceptionelements.Outcome)
	if malformed.Kind != perceptionelements.OutcomeRefused || malformed.Code != "invalid_payload" {
		t.Fatalf("malformed outcome = %+v", malformed)
	}
}

func TestASRResolutionRejectsLiveDescriptorDriftBeforeReadiness(t *testing.T) {
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) {
		return &driftedASR{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, runDone, cancelRun := mountASR(t, providers)
	defer cancelRun()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "descriptor drifted") {
			t.Fatalf("graph run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ASR graph did not reject descriptor drift")
	}
}

func TestASRResolutionRejectsMutableProviderIdentityBeforeReadiness(t *testing.T) {
	descriptor := cloneDescriptor(testASRDescriptor)
	descriptor.Version = "latest"
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", descriptor, func() (v1.PerceptionProvider, error) {
		return &describedASR{descriptor: descriptor}, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, runDone, cancelRun := mountASR(t, providers)
	defer cancelRun()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "mutable or placeholder selector") {
			t.Fatalf("mutable provider identity error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ASR graph published readiness for a mutable provider identity")
	}
	resolution := mounted.Live().Nodes["asr"].Resolution
	if resolution == nil || resolution.RuntimeEvidence == inspect.EvidenceLive ||
		resolution.CapabilitiesEvidence == inspect.EvidenceLive {
		t.Fatalf("refused ASR identity left partial live evidence: %+v", resolution)
	}
}

type scriptedASR struct{}

func (*scriptedASR) Descriptor() v1.Descriptor { return cloneDescriptor(testASRDescriptor) }
func (*scriptedASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return []v1.PerceptionRevision{{RevisionID: 1, StableText: "hello"}}, nil
}
func (*scriptedASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 2, StableText: "hello world", Final: true}, nil
}

type driftedASR struct{ scriptedASR }

func (*driftedASR) Descriptor() v1.Descriptor {
	descriptor := cloneDescriptor(testASRDescriptor)
	descriptor.Version = "different"
	return descriptor
}

type describedASR struct {
	scriptedASR
	descriptor v1.Descriptor
}

func (provider *describedASR) Descriptor() v1.Descriptor {
	return cloneDescriptor(provider.descriptor)
}

type blockingASR struct {
	entered chan struct{}
	once    sync.Once
	closed  atomic.Int32
}

func (*blockingASR) Descriptor() v1.Descriptor { return cloneDescriptor(testASRDescriptor) }
func (provider *blockingASR) PushFrame(ctx context.Context, _ v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	provider.once.Do(func() { close(provider.entered) })
	<-ctx.Done()
	return nil, context.Cause(ctx)
}
func (*blockingASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{}, errors.New("unexpected finalize")
}
func (provider *blockingASR) Close() error { provider.closed.Add(1); return nil }

func mountASR(
	t *testing.T, providers *perceptionelements.ASRProviderRegistry,
) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	graph := compileASRGraph(t)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(perceptionelements.ASRProviderRegistryService, providers); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{
			"asr": json.RawMessage(`{"provider":"primary","source":"microphone"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, done, cancel
}

func stopASR(
	t *testing.T, _ *graphruntime.Mounted, done <-chan error, cancel context.CancelFunc,
) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("ASR graph stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("ASR graph did not stop")
	}
}

func receive(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func compileASRGraph(t *testing.T) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse("asr.ortg", []byte(asrGraph))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Graph
}

func cloneDescriptor(descriptor v1.Descriptor) v1.Descriptor {
	capabilities := descriptor.Capabilities
	descriptor.Capabilities = make(v1.Capabilities, len(capabilities))
	for capability, enabled := range capabilities {
		descriptor.Capabilities[capability] = enabled
	}
	return descriptor
}

func assertASRLiveResolution(t *testing.T, mounted *graphruntime.Mounted) {
	t.Helper()
	resolution := mounted.Live().Nodes["asr"].Resolution
	if resolution == nil || resolution.RuntimeEvidence != inspect.EvidenceLive ||
		resolution.Runtime.ID != "builtin://openrealtime/elements/perception.ASR" ||
		resolution.Runtime.Revision != "implementation:2" ||
		resolution.CapabilitiesEvidence != inspect.EvidenceLive {
		t.Fatalf("ASR live resolution = %+v", resolution)
	}
	for _, capability := range resolution.Capabilities {
		if capability.Name == "asr.transcription" &&
			capability.Provider.ID == "provider://openrealtime/api/v1/perception/test-asr" &&
			capability.Provider.Revision == "1" && capability.Adapter != nil &&
			capability.Adapter.ID == "builtin://openrealtime/adapters/perception.ASR-api-v1" &&
			capability.Adapter.Revision == "implementation:2" {
			return
		}
	}
	t.Fatalf("ASR provider and adapter artifacts are not independently attested: %+v",
		resolution.Capabilities)
}
