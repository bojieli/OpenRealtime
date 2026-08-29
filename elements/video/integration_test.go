package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

const mountedVideoGraph = `graph mounted_video {
    video.FrameIngress :: ingress;
    video.AdaptiveObservation :: policy;

    edge live_frames = ingress.frames => policy.frames;
    edge live_references = ingress.references => policy.references;

    input source = policy.source;
    input frame = ingress.frame_in;
    input reference = ingress.reference_in;
    input tick = policy.tick;
    input refresh = policy.refresh;
    input end = policy.end;
    input cancel = policy.cancel;

    output observe = policy.observe;
    output observe_reference = policy.observe_reference;
    output observer_refresh = policy.observer_refresh;
    output observer_close = policy.observer_close;
    output observer_cancel = policy.observer_cancel;
    output state = policy.state;
    output decision = policy.decision;
    output outcome = policy.outcome;
}`

func TestMountedIngressMakesCaptureOverflowVisibleAndReportsLiveIdentity(t *testing.T) {
	parsed, err := syntax.Parse("mounted-video.ortg", []byte(mountedVideoGraph))
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := json.Marshal(AdaptiveObservationConfig{
		Source: "screen", Mode: CadenceAdaptive, FixedIntervalMS: 100,
		MinIntervalMS: 100, MaxIntervalMS: 500, ChangeThreshold: 0.1,
		MaxFrameBytes: 1024, MaxChangeSamples: 16, MaxMetadataBytes: 1024,
		TerminalMemory: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: "mounted_video",
		Nodes: map[string]json.RawMessage{"ingress": json.RawMessage(`{}`), "policy": configuration},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: bound.Graph, Registry: registry, Values: bound.Values,
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(runContext) }()
	t.Cleanup(func() {
		cancel()
		select {
		case runErr := <-done:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("mounted video shutdown: %v", runErr)
			}
		case <-time.After(2 * time.Second):
			t.Error("mounted video graph did not stop")
		}
		closeContext, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		if err := mounted.Close(closeContext); err != nil {
			t.Errorf("close mounted video graph: %v", err)
		}
	})

	source := mountedIngress(t, mounted, "source")
	state := mountedEgress(t, mounted, "state")
	outcome := mountedEgress(t, mounted, "outcome")
	sendContext, sendCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer sendCancel()
	start := sourceEnvelope("mounted-source", "session-a", 1, testOpenedNS)
	if result, err := source.Broadcast(sendContext, start); err != nil || result.Delivered != 1 || result.Dropped != 0 {
		t.Fatalf("source ingress = %+v, %v", result, err)
	}
	_ = receiveMounted(t, state)
	_ = receiveMounted(t, outcome)

	assertVideoLiveResolution(t, mounted, "ingress", frameIngressRuntimeID)
	assertVideoLiveResolution(t, mounted, "policy", policyRuntimeID)

	// Do not drain policy state/outcome after source startup. The first frame
	// fills the state boundary; a later frame blocks policy progress, allowing
	// the one-slot live_frames edge to demonstrate explicit best-effort loss.
	frames := mountedIngress(t, mounted, "frame")
	for index := uint64(1); index <= 256; index++ {
		envelope := frameEnvelope(fmt.Sprintf("mounted-frame-%d", index), "session-a", 1,
			index, testOpenedNS+index, []byte{byte(index)})
		result, sendErr := frames.Broadcast(sendContext, envelope)
		if sendErr != nil || result.Delivered != 1 || result.Dropped != 0 {
			t.Fatalf("frame boundary send %d = %+v, %v", index, result, sendErr)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for mounted.Live().Edges["live_frames"].Dropped == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	live := mounted.Live()
	if live.Edges["live_frames"].Dropped == 0 {
		t.Fatalf("lossy capture edge did not report overflow: %+v", live.Edges["live_frames"])
	}

	// Control boundaries remain lossless even while policy output is
	// backpressured. Admission may queue, but it may not report a legal drop.
	ticks := mountedIngress(t, mounted, "tick")
	tick := tickEnvelope("mounted-control", "session-a", 1, testOpenedNS+1_000_000_000)
	result, err := ticks.Broadcast(sendContext, tick)
	if err != nil || result.Delivered != 1 || result.Dropped != 0 {
		t.Fatalf("lossless control send = %+v, %v", result, err)
	}
	if edge := mounted.Live().Edges["boundary:tick"]; edge.Dropped != 0 {
		t.Fatalf("control boundary dropped an item: %+v", edge)
	}
}

func assertVideoLiveResolution(t *testing.T, mounted *graphruntime.Mounted, node, runtimeID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resolution := mounted.Live().Nodes[node].Resolution
		if resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive &&
			resolution.CapabilitiesEvidence == inspect.EvidenceLive {
			if resolution.Runtime != (inspect.ArtifactIdentity{ID: runtimeID, Revision: implementationRev}) ||
				len(resolution.Capabilities) != 0 {
				t.Fatalf("node %s live resolution = %+v", node, resolution)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %s did not report live resolution: %+v", node, resolution)
		}
		time.Sleep(time.Millisecond)
	}
}

func mountedIngress(t *testing.T, mounted *graphruntime.Mounted, name string) element.OutputPort {
	t.Helper()
	port, err := mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func mountedEgress(t *testing.T, mounted *graphruntime.Mounted, name string) element.InputPort {
	t.Helper()
	port, err := mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func receiveMounted(t *testing.T, port element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
