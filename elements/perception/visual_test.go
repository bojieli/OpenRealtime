package perception_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const visualGraph = `graph graph_native_visual {
    perception.VisualObserver :: vision;
    input observe = vision.observe;
    input refresh = vision.refresh;
    input close = vision.close;
    input cancel = vision.cancel;
    output observations = vision.observations;
    output outcome = vision.outcome;
    output resolved = vision.resolved;
    output metrics = vision.metrics;
}
`

func TestVisualElementMakesAdmissionRefreshAndTimingExplicit(t *testing.T) {
	narrator := &testNarrator{name: "vision/test@1", text: "The editor shows a saved document."}
	mounted, done, cancelRun := mountVisual(t, narrator, `{
        "provider":"primary",
        "source":"screen",
        "change_threshold":0.01
    }`)
	defer stopVisual(t, done, cancelRun)

	resolved, _ := mounted.Egress("resolved")
	resolution := receiveVisual(t, resolved).Payload.(perceptionelements.VisualProviderResolution)
	if resolution.Reference != "primary" || resolution.Descriptor.Name != narrator.name ||
		resolution.RegistryRevision == 0 {
		t.Fatalf("visual resolution = %+v", resolution)
	}
	metricsOutput, _ := mounted.Egress("metrics")
	startup := receiveVisual(t, metricsOutput).Payload.(perceptionelements.VisualMetrics)
	if startup.Source != "screen" || startup.Frames != 0 {
		t.Fatalf("startup metrics = %+v", startup)
	}

	observe, _ := mounted.Ingress("observe")
	refresh, _ := mounted.Ingress("refresh")
	observations, _ := mounted.Egress("observations")
	outcomes, _ := mounted.Egress("outcome")
	frame := visualFrame(t, "screen", color.RGBA{R: 20, G: 80, B: 140, A: 255})
	sendVisual(t, observe, element.Envelope{
		Type: element.Trigger(element.Named("image.FrameBatch")), ItemID: "frame-1",
		SourceID: "screen", CancellationScope: "screen-stream",
		Payload: perceptionelements.ImageBatch{StreamID: "screen-stream", Frames: []coreperception.Frame{frame}},
	})
	observationEnvelope := receiveVisual(t, observations)
	observation := observationEnvelope.Payload.(coreperception.Observation)
	if observation.Text != narrator.text || observation.Source != "screen" ||
		observation.Authority != trajectory.AuthorityObserver || observationEnvelope.CaptureNS != frame.CapturedNS {
		t.Fatalf("visual observation = %+v, envelope = %+v", observation, observationEnvelope)
	}
	firstMetrics := receiveVisual(t, metricsOutput).Payload.(perceptionelements.VisualMetrics)
	firstOutcome := receiveVisual(t, outcomes).Payload.(perceptionelements.VisualOutcome)
	if firstMetrics.Admitted != 1 || firstMetrics.Narrations != 1 ||
		firstOutcome.Kind != perceptionelements.VisualSucceeded || firstOutcome.ObservationCount != 1 {
		t.Fatalf("metrics = %+v, outcome = %+v", firstMetrics, firstOutcome)
	}

	// There is deliberately no sleep here. A graph-native visual node has no
	// hidden cadence, and byte-identical input is still collapsed by its cheap
	// content gate.
	sendVisual(t, observe, element.Envelope{
		Type: element.Trigger(element.Named("image.FrameBatch")), ItemID: "frame-2",
		SourceID: "screen", CancellationScope: "screen-stream",
		Payload: perceptionelements.ImageBatch{StreamID: "screen-stream", Frames: []coreperception.Frame{frame}},
	})
	secondMetrics := receiveVisual(t, metricsOutput).Payload.(perceptionelements.VisualMetrics)
	secondOutcome := receiveVisual(t, outcomes).Payload.(perceptionelements.VisualOutcome)
	if secondMetrics.Frames != 2 || secondMetrics.Admitted != 1 || secondOutcome.ObservationCount != 0 ||
		narrator.calls.Load() != 1 {
		t.Fatalf("identical-frame gate: metrics=%+v outcome=%+v calls=%d",
			secondMetrics, secondOutcome, narrator.calls.Load())
	}

	// A post-effect refresh is a typed control input. It forces one identical
	// frame through and then ordinary change collapse resumes.
	sendVisual(t, refresh, element.Envelope{
		Type: element.Trigger(element.Named("image.Refresh")), ItemID: "refresh-1",
		SourceID: "screen", Payload: perceptionelements.VisualRefresh{Source: "screen", Reason: "action completed"},
	})
	refreshOutcome := receiveVisual(t, outcomes).Payload.(perceptionelements.VisualOutcome)
	if refreshOutcome.Kind != perceptionelements.VisualSucceeded || refreshOutcome.Operation != "refresh" {
		t.Fatalf("refresh outcome = %+v", refreshOutcome)
	}
	sendVisual(t, observe, element.Envelope{
		Type: element.Trigger(element.Named("image.FrameBatch")), ItemID: "frame-3",
		SourceID: "screen", CancellationScope: "screen-stream",
		Payload: perceptionelements.ImageBatch{StreamID: "screen-stream", Frames: []coreperception.Frame{frame}},
	})
	_ = receiveVisual(t, observations)
	thirdMetrics := receiveVisual(t, metricsOutput).Payload.(perceptionelements.VisualMetrics)
	thirdOutcome := receiveVisual(t, outcomes).Payload.(perceptionelements.VisualOutcome)
	if thirdMetrics.Admitted != 2 || narrator.calls.Load() != 2 || thirdOutcome.ObservationCount != 1 {
		t.Fatalf("forced refresh: metrics=%+v outcome=%+v calls=%d",
			thirdMetrics, thirdOutcome, narrator.calls.Load())
	}
}

func TestVisualElementCancellationHasTerminalOutcomesAndResetsState(t *testing.T) {
	narrator := &testNarrator{
		name: "vision/blocking@1", text: "finished",
		entered: make(chan struct{}), block: true,
	}
	mounted, done, cancelRun := mountVisual(t, narrator, `{"provider":"primary","source":"camera"}`)
	defer stopVisual(t, done, cancelRun)
	resolved, _ := mounted.Egress("resolved")
	metrics, _ := mounted.Egress("metrics")
	_ = receiveVisual(t, resolved)
	_ = receiveVisual(t, metrics)
	observe, _ := mounted.Ingress("observe")
	cancelInput, _ := mounted.Ingress("cancel")
	outcomes, _ := mounted.Egress("outcome")
	frame := visualFrame(t, "camera", color.RGBA{G: 200, A: 255})
	sendVisual(t, observe, element.Envelope{
		Type: element.Trigger(element.Named("image.FrameBatch")), ItemID: "blocked-frame",
		CancellationScope: "camera-stream",
		Payload:           perceptionelements.ImageBatch{StreamID: "camera-stream", Frames: []coreperception.Frame{frame}},
	})
	select {
	case <-narrator.entered:
	case <-time.After(time.Second):
		t.Fatal("visual provider invocation did not start")
	}
	sendVisual(t, cancelInput, element.Envelope{
		Type: element.Interrupt(element.Named("image.StreamID")), ItemID: "cancel-camera",
		CancellationScope: "camera-stream",
		Payload:           perceptionelements.VisualCancel{StreamID: "camera-stream", Source: "camera", Reason: "newer intent"},
	})
	canceledMetrics := receiveVisual(t, metrics).Payload.(perceptionelements.VisualMetrics)
	operation := receiveVisual(t, outcomes).Payload.(perceptionelements.VisualOutcome)
	interrupt := receiveVisual(t, outcomes).Payload.(perceptionelements.VisualOutcome)
	if operation.Kind != perceptionelements.VisualCanceled || operation.Operation != "observe" ||
		interrupt.Kind != perceptionelements.VisualSucceeded || interrupt.Operation != "cancel" {
		t.Fatalf("operation=%+v interrupt=%+v", operation, interrupt)
	}
	if narrator.canceled.Load() != 1 {
		t.Fatalf("provider cancellation count = %d", narrator.canceled.Load())
	}
	if canceledMetrics.Frames != 1 || canceledMetrics.Admitted != 1 {
		t.Fatalf("cancellation erased cumulative metrics: %+v", canceledMetrics)
	}
}

func TestVisualElementRefusesCrossSourceAndStrictInvalidConfig(t *testing.T) {
	narrator := &testNarrator{name: "vision/test@1", text: "screen"}
	mounted, done, cancelRun := mountVisual(t, narrator, `{"provider":"primary","source":"screen"}`)
	defer stopVisual(t, done, cancelRun)
	resolved, _ := mounted.Egress("resolved")
	metrics, _ := mounted.Egress("metrics")
	_ = receiveVisual(t, resolved)
	_ = receiveVisual(t, metrics)
	observe, _ := mounted.Ingress("observe")
	outcomes, _ := mounted.Egress("outcome")
	sendVisual(t, observe, element.Envelope{
		Type: element.Trigger(element.Named("image.FrameBatch")), ItemID: "wrong-source",
		CancellationScope: "camera",
		Payload: perceptionelements.ImageBatch{StreamID: "camera", Frames: []coreperception.Frame{
			visualFrame(t, "camera", color.RGBA{B: 255, A: 255}),
		}},
	})
	refusal := receiveVisual(t, outcomes).Payload.(perceptionelements.VisualOutcome)
	if refusal.Kind != perceptionelements.VisualRefused || refusal.Code != "source_not_selected" {
		t.Fatalf("cross-source outcome = %+v", refusal)
	}

	graph := compileVisualGraph(t)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	providers := visualRegistry(t, narrator)
	if _, err := services.Set(perceptionelements.VisualProviderRegistryService, providers); err != nil {
		t.Fatal(err)
	}
	_, err = graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"vision": json.RawMessage(`{"provider":"primary","source":"screen","cadence_ms":333}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("hidden cadence configuration was accepted: %v", err)
	}
}

func TestVisualProviderDriftAndKeyframeDependencyFailBeforeRun(t *testing.T) {
	providers := perceptionelements.NewVisualProviderRegistry()
	if err := providers.Register("primary", perceptionelements.VisualProviderDescriptor{
		Name: "declared", Revision: "1",
	}, func() (coreperception.Narrator, error) {
		return &testNarrator{name: "actual", text: "x"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mountVisualWithRegistry(t, providers, `{"provider":"primary","source":"screen"}`); err == nil || !strings.Contains(err.Error(), "identity drifted") {
		t.Fatalf("live provider drift was accepted: %v", err)
	}

	valid := visualRegistry(t, &testNarrator{name: "vision/test@1", text: "x"})
	if _, err := mountVisualWithRegistry(t, valid, `{"provider":"primary","source":"screen","attach_keyframes":true}`); err == nil || !strings.Contains(err.Error(), "media retainer") {
		t.Fatalf("keyframe attachment without a retainer was accepted: %v", err)
	}
}

type testNarrator struct {
	name     string
	text     string
	block    bool
	entered  chan struct{}
	enterOne sync.Once
	calls    atomic.Int32
	canceled atomic.Int32
	closed   atomic.Int32
}

func (narrator *testNarrator) Name() string { return narrator.name }

func (narrator *testNarrator) Narrate(
	ctx context.Context, _ []coreperception.Frame, _ trajectory.Snapshot,
) (string, error) {
	narrator.calls.Add(1)
	if narrator.entered != nil {
		narrator.enterOne.Do(func() { close(narrator.entered) })
	}
	if narrator.block {
		<-ctx.Done()
		narrator.canceled.Add(1)
		return "", ctx.Err()
	}
	return narrator.text, nil
}

func (narrator *testNarrator) Close() error { narrator.closed.Add(1); return nil }

func visualFrame(t *testing.T, source string, fill color.RGBA) coreperception.Frame {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 40; x++ {
			frame.SetRGBA(x, y, fill)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	return coreperception.Frame{
		Kind: coreperception.FrameImage, Source: source, CapturedNS: 42,
		Image: encoded.Bytes(), MIMEType: "image/png", Width: 40, Height: 30,
	}
}

func visualRegistry(t *testing.T, narrator coreperception.Narrator) *perceptionelements.VisualProviderRegistry {
	t.Helper()
	registry := perceptionelements.NewVisualProviderRegistry()
	if err := registry.Register("primary", perceptionelements.VisualProviderDescriptor{
		Name: narrator.Name(), Revision: "test:1",
	}, func() (coreperception.Narrator, error) { return narrator, nil }); err != nil {
		t.Fatal(err)
	}
	return registry
}

func mountVisual(t *testing.T, narrator coreperception.Narrator, values string) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	mounted, err := mountVisualWithRegistry(t, visualRegistry(t, narrator), values)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, done, cancel
}

func mountVisualWithRegistry(t *testing.T, providers *perceptionelements.VisualProviderRegistry, values string) (*graphruntime.Mounted, error) {
	t.Helper()
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		return nil, err
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(perceptionelements.VisualProviderRegistryService, providers); err != nil {
		return nil, err
	}
	return graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compileVisualGraph(t), Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"vision": json.RawMessage(values)},
	})
}

func stopVisual(t *testing.T, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("visual graph stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("visual graph did not stop")
	}
}

func sendVisual(t *testing.T, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := output.Broadcast(ctx, envelope); err != nil {
		t.Fatal(err)
	}
}

func receiveVisual(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func compileVisualGraph(t *testing.T) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse("visual.ortg", []byte(visualGraph))
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
	return compiled.Graph
}

var (
	_ coreperception.Narrator = (*testNarrator)(nil)
	_ io.Closer               = (*testNarrator)(nil)
)
