package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TestMountedGraphRunsThroughTypedBoundariesAndReportsLiveQueues(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	disposed := &atomic.Bool{}
	if err := registry.Register("", passFactory{descriptor: descriptor, disposed: disposed}); err != nil {
		t.Fatal(err)
	}
	graph := passGraph(t, descriptor)
	tracer := graphruntime.NewBufferTracer(32)
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(ctx) }()
	ingress, err := mounted.Ingress("input")
	if err != nil {
		t.Fatal(err)
	}
	egress, err := mounted.Egress("output")
	if err != nil {
		t.Fatal(err)
	}
	message := element.Envelope{
		Type: element.Event(element.Named("test.Value")), ItemID: "item-1",
		TraceID: "trace-1", RunID: "run-1", Payload: "hello",
	}
	if result, err := ingress.Broadcast(context.Background(), message); err != nil || result.Delivered != 1 {
		t.Fatalf("ingress = %+v, %v", result, err)
	}
	got, err := egress.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ItemID != message.ItemID || got.Payload != "hello" {
		t.Fatalf("egress message = %+v", got)
	}
	live := mounted.Live()
	if live.Fingerprint != graph.Fingerprint || live.Edges["boundary:input"].Enqueued != 1 ||
		live.Edges["boundary:output"].Dequeued != 1 {
		t.Fatalf("unexpected live snapshot: %+v", live)
	}
	if len(tracer.Events()) < 4 {
		t.Fatalf("trace has only %d events: %+v", len(tracer.Events()), tracer.Events())
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := mounted.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if !disposed.Load() {
		t.Fatal("lifecycle disposer did not run")
	}
}

func TestMountedGraphReportsExactLiveResolutionAndCorrelatedInternalFlow(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	registered := inspect.ArtifactIdentity{ID: "image://pass", Revision: "sha256:build-7"}
	configuration := inspect.ArtifactIdentity{
		ID: "values://pass-chain", Revision: "openrealtime.ai/config/v1alpha1",
		Digest: "sha256:" + strings.Repeat("a", 64),
	}
	if err := registry.RegisterArtifact("", registered, resolvingPassFactory{
		passFactory: passFactory{descriptor: descriptor},
	}); err != nil {
		t.Fatal(err)
	}
	graph := passChainGraph(t, descriptor)
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Configuration: &configuration,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := mounted.Live()
	if before.FormatVersion != inspect.LiveFormatVersion || before.GraphID != graph.ID ||
		before.GraphRevision != graph.Revision || before.Fingerprint != graph.Fingerprint ||
		before.State != "mounted" || before.Configuration == nil || *before.Configuration != configuration {
		t.Fatalf("mounted snapshot identity = %+v", before)
	}
	if got := before.Nodes["first"].Resolution; got == nil ||
		got.Runtime != registered || got.RuntimeEvidence != inspect.EvidenceRegistered ||
		got.CapabilitiesEvidence != "" {
		t.Fatalf("registered resolution = %+v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	ingress, _ := mounted.Ingress("input")
	egress, _ := mounted.Egress("output")
	message := element.Envelope{
		Type: element.Event(element.Named("test.Value")), ItemID: "item-live",
		TraceID: "task-7", RunID: "run-7", Sequence: 1, Payload: "hello",
	}
	if _, err := ingress.Broadcast(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if _, err := egress.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	live := mounted.Live()
	if live.State != "running" {
		t.Fatalf("running snapshot state = %q", live.State)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		live = mounted.Live()
		ready := true
		for _, node := range []string{"first", "second"} {
			telemetry := live.Nodes[node]
			ready = ready && telemetry.ActiveRuns == 0 && telemetry.LastTriggerID == message.ItemID &&
				telemetry.LastOutcome == message.ItemID && telemetry.FirstTriggerNS != 0 &&
				telemetry.FirstOutputNS >= telemetry.FirstTriggerNS &&
				telemetry.CompletionNS >= telemetry.FirstOutputNS
		}
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for _, node := range []string{"first", "second"} {
		telemetry := live.Nodes[node]
		if telemetry.ActiveRuns != 0 || telemetry.LastTriggerID != message.ItemID ||
			telemetry.LastOutcome != message.ItemID || telemetry.FirstTriggerNS == 0 ||
			telemetry.FirstOutputNS < telemetry.FirstTriggerNS ||
			telemetry.CompletionNS < telemetry.FirstOutputNS || telemetry.CancellationNS != 0 {
			t.Fatalf("node %s reaction telemetry = %+v", node, telemetry)
		}
		resolution := live.Nodes[node].Resolution
		if resolution == nil || resolution.RuntimeEvidence != inspect.EvidenceLive ||
			resolution.Runtime.ID != "worker://pass" || resolution.Runtime.Revision != "build:7" ||
			resolution.CapabilitiesEvidence != inspect.EvidenceLive || len(resolution.Capabilities) != 1 ||
			resolution.Capabilities[0].Provider.ID != "provider://echo" ||
			resolution.Capabilities[0].Adapter == nil ||
			resolution.Capabilities[0].Adapter.ID != "adapter://echo" {
			t.Fatalf("node %s live resolution = %+v", node, resolution)
		}
	}
	flow, found := live.Flows["trace:task-7"]
	if !found || len(flow.Edges) != 1 || flow.Edges[0] != "first-to-second" ||
		flow.Correlation != "trace:task-7" || flow.Truncated {
		t.Fatalf("correlated flow = %+v, found=%t", flow, found)
	}
	// Live snapshots are recursively independent management-plane values.
	live.Nodes["first"].Resolution.Capabilities[0].Provider.ID = "mutated"
	live.Configuration.ID = "mutated"
	live.Flows["trace:task-7"] = inspect.FlowLive{Edges: []string{"mutated"}}
	again := mounted.Live()
	if again.Nodes["first"].Resolution.Capabilities[0].Provider.ID != "provider://echo" ||
		again.Flows["trace:task-7"].Edges[0] != "first-to-second" ||
		again.Configuration.ID != "values://pass-chain" {
		t.Fatal("live inspection snapshot retained caller aliases")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("graph did not stop")
	}
	closed := mounted.Live()
	for _, node := range []string{"first", "second"} {
		before, after := live.Nodes[node], closed.Nodes[node]
		if after.State != "stopped" || after.LastTriggerID != before.LastTriggerID ||
			after.LastOutcome != before.LastOutcome || after.FirstTriggerNS != before.FirstTriggerNS ||
			after.FirstOutputNS != before.FirstOutputNS ||
			after.CompletionNS != before.CompletionNS {
			t.Fatalf("node %s shutdown discarded reaction telemetry: before=%+v after=%+v", node, before, after)
		}
	}
}

func TestReactionTelemetryTracksCancellationAndSurvivesShutdown(t *testing.T) {
	descriptor := telemetryDescriptor()
	registry := graphruntime.NewRegistry()
	if err := registry.Register("", telemetryFactory{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	graph := telemetryGraph(t, descriptor)
	var now atomic.Uint64
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Now: func() uint64 { return now.Add(10) },
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(context.Background()) }()
	ingress, _ := mounted.Ingress("input")
	cancellation, _ := mounted.Ingress("cancel")
	egress, _ := mounted.Egress("output")
	message := element.Envelope{
		Type: element.Event(element.Named("test.Value")), ItemID: "telemetry-trigger",
		RunID: "run-telemetry", TraceID: "trace-telemetry", Payload: "value",
	}
	if result, sendErr := ingress.Broadcast(context.Background(), message); sendErr != nil || result.Delivered != 1 {
		t.Fatalf("trigger ingress = %+v, %v", result, sendErr)
	}
	waitForNodeTelemetry(t, mounted, func(node inspect.NodeLive) bool {
		return node.ActiveRuns == 1 && node.LastTriggerID == message.ItemID && node.FirstTriggerNS != 0
	})
	firstTrigger := mounted.Live().Nodes["telemetry"].FirstTriggerNS
	repeated := message
	repeated.ItemID = "telemetry-trigger-repeat"
	if result, sendErr := ingress.Broadcast(context.Background(), repeated); sendErr != nil || result.Delivered != 1 {
		t.Fatalf("repeated trigger ingress = %+v, %v", result, sendErr)
	}
	waitForNodeTelemetry(t, mounted, func(node inspect.NodeLive) bool {
		return node.ActiveRuns == 1 && node.LastTriggerID == repeated.ItemID &&
			node.FirstTriggerNS == firstTrigger
	})
	cancelEnvelope := element.Envelope{
		Type: element.Interrupt(element.Named("flow.RunID")), ItemID: "telemetry-cancel",
		RunID: message.RunID, TraceID: message.TraceID, Payload: message.RunID,
	}
	if result, sendErr := cancellation.Broadcast(context.Background(), cancelEnvelope); sendErr != nil || result.Delivered != 1 {
		t.Fatalf("cancel ingress = %+v, %v", result, sendErr)
	}
	if _, receiveErr := egress.Receive(context.Background()); receiveErr != nil {
		t.Fatal(receiveErr)
	}
	waitForNodeTelemetry(t, mounted, func(node inspect.NodeLive) bool {
		return node.ActiveRuns == 0 && node.FirstTriggerNS == firstTrigger &&
			node.CancellationNS >= firstTrigger && node.FirstOutputNS >= firstTrigger &&
			node.CompletionNS >= node.FirstOutputNS && node.LastOutcome == message.ItemID
	})
	before := mounted.Live().Nodes["telemetry"]
	closeContext, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := mounted.Close(closeContext); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("telemetry graph did not stop")
	}
	after := mounted.Live().Nodes["telemetry"]
	if after.State != "stopped" || after.ActiveRuns != 0 || after.LastTriggerID != before.LastTriggerID ||
		after.LastOutcome != before.LastOutcome || after.FirstTriggerNS != before.FirstTriggerNS ||
		after.FirstOutputNS != before.FirstOutputNS ||
		after.CompletionNS != before.CompletionNS || after.CancellationNS != before.CancellationNS {
		t.Fatalf("shutdown discarded cancellation telemetry: before=%+v after=%+v", before, after)
	}
}

func waitForNodeTelemetry(t *testing.T, mounted *graphruntime.Mounted, accept func(inspect.NodeLive) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if node := mounted.Live().Nodes["telemetry"]; accept(node) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("node telemetry did not reach expected state: %+v", mounted.Live().Nodes["telemetry"])
}

func TestMountRejectsFactoryContractMutationAndMissingDependency(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	if err := registry.Register("", passFactory{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	graph := passGraph(t, descriptor)
	graph.Nodes[0].Reaction.MaxConcurrency = 2
	graph, err := ir.Freeze(graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{Graph: graph, Registry: registry}); err == nil || !strings.Contains(err.Error(), "reaction contract") {
		t.Fatalf("contract mutation error = %v", err)
	}

	descriptor = passDescriptor([]element.Dependency{{Name: "models.fast"}})
	registry = graphruntime.NewRegistry()
	if err := registry.Register("", passFactory{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	graph = passGraph(t, descriptor)
	if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{Graph: graph, Registry: registry}); err == nil || !strings.Contains(err.Error(), "unavailable service") {
		t.Fatalf("dependency error = %v", err)
	}
}

func TestMountRejectsUnknownOrNonObjectValues(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	if err := registry.Register("", passFactory{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	graph := passGraph(t, descriptor)
	for name, values := range map[string]map[string]json.RawMessage{
		"unknown":       {"missing": json.RawMessage(`{}`)},
		"scalar":        {"pass": json.RawMessage(`true`)},
		"duplicate_key": {"pass": json.RawMessage(`{"mode":"one","mode":"two"}`)},
		"undeclared":    {"pass": json.RawMessage(`{"mode":"one"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: graph, Registry: registry, Values: values,
			}); err == nil {
				t.Fatal("expected invalid values to fail")
			}
		})
	}
	if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry,
		Values: map[string]json.RawMessage{"pass": json.RawMessage(`{ }`)},
	}); err != nil {
		t.Fatalf("semantically empty object was rejected: %v", err)
	}

	permissiveRegistry := graphruntime.NewRegistry()
	if err := permissiveRegistry.Register("", permissivePassFactory{passFactory{descriptor: descriptor}}); err != nil {
		t.Fatal(err)
	}
	if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: permissiveRegistry,
		Values: map[string]json.RawMessage{"pass": json.RawMessage(`{"mode":"one"}`)},
	}); err == nil || !strings.Contains(err.Error(), "declares no config schema") {
		t.Fatalf("validator bypassed an absent config schema: %v", err)
	}
}

func TestShutdownReportsElementThatIgnoresCancellation(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	release := make(chan struct{})
	if err := registry.Register("", stubbornFactory{descriptor: descriptor, release: release}); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: passGraph(t, descriptor), Registry: registry, ShutdownTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unresponsive elements: pass") {
			t.Fatalf("shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded shutdown did not return")
	}
	close(release)
}

type passFactory struct {
	descriptor element.Descriptor
	disposed   *atomic.Bool
}

type permissivePassFactory struct{ passFactory }

type resolvingPassFactory struct{ passFactory }

type telemetryFactory struct{ descriptor element.Descriptor }

func (permissivePassFactory) ValidateConfig(json.RawMessage) error { return nil }

func (factory resolvingPassFactory) Mount(
	ctx context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if mount.Resolution == nil {
		return nil, errors.New("mount has no live resolution reporter")
	}
	runnable, err := factory.passFactory.Mount(ctx, mount)
	if err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		if err := mount.Resolution.Runtime("worker://pass", "build:7", ""); err != nil {
			return err
		}
		if err := mount.Resolution.Capabilities([]element.CapabilityResolution{{
			Name: "generation", Contract: "test.echo/v1",
			ProviderID: "provider://echo", ProviderRevision: "weights:7",
			AdapterID: "adapter://echo", AdapterRevision: "git:7",
		}}); err != nil {
			return err
		}
		return runnable.Run(ctx)
	}), nil
}

func (factory passFactory) Descriptor() element.Descriptor { return factory.descriptor.Clone() }

func (factory telemetryFactory) Descriptor() element.Descriptor { return factory.descriptor.Clone() }

func (factory telemetryFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	input, err := mount.Ports.Input("in")
	if err != nil {
		return nil, err
	}
	cancel, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	output, err := mount.Ports.Output("out")
	if err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		message, receiveErr := input.Receive(ctx)
		if receiveErr != nil {
			return receiveErr
		}
		if _, receiveErr = input.Receive(ctx); receiveErr != nil {
			return receiveErr
		}
		if _, receiveErr = cancel.Receive(ctx); receiveErr != nil {
			return receiveErr
		}
		if _, sendErr := output.Broadcast(ctx, message); sendErr != nil {
			return sendErr
		}
		<-ctx.Done()
		return nil
	}), nil
}

func (factory passFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	input, err := mount.Ports.Input("in")
	if err != nil {
		return nil, err
	}
	output, err := mount.Ports.Output("out")
	if err != nil {
		return nil, err
	}
	if factory.disposed != nil {
		if err := mount.Lifecycle.Defer("mark-disposed", func(context.Context) error {
			factory.disposed.Store(true)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		for {
			message, err := input.Receive(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, graphruntime.ErrChannelClosed) || ctx.Err() != nil {
					return nil
				}
				return err
			}
			if _, err := output.Broadcast(ctx, message); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}), nil
}

type stubbornFactory struct {
	descriptor element.Descriptor
	release    <-chan struct{}
}

func (factory stubbornFactory) Descriptor() element.Descriptor { return factory.descriptor.Clone() }
func (factory stubbornFactory) Mount(context.Context, element.MountContext) (element.Runnable, error) {
	return element.RunnableFunc(func(context.Context) error {
		<-factory.release
		return nil
	}), nil
}

func passDescriptor(dependencies []element.Dependency) element.Descriptor {
	valueType := element.Event(element.Named("test.Value"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Pass",
		Revision:      1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction:     element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1},
		Dependencies: dependencies,
	}
}

func telemetryDescriptor() element.Descriptor {
	valueType := element.Event(element.Named("test.Value"))
	cancelType := element.Interrupt(element.Named("flow.RunID"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Telemetry", Revision: 1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "cancel", Direction: element.Input, Type: cancelType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction: element.Reaction{
			Triggers: []string{"in"}, Interrupts: []string{"cancel"},
			Outcomes: []string{"out"}, MaxConcurrency: 1,
		},
	}
}

func passGraph(t *testing.T, descriptor element.Descriptor) ir.Graph {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	valueType := element.Event(element.Named("test.Value"))
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "pass", Revision: 1,
		Nodes: []ir.Node{{
			ID: "pass", Element: identity,
			Ports: []ir.Port{
				{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			},
			Reaction: descriptor.Reaction, Dependencies: descriptor.Dependencies,
		}},
		Boundaries: []ir.Boundary{
			{Name: "input", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "pass", Port: "in"}, Type: valueType},
			{Name: "output", Direction: ir.OutputBoundary, Endpoint: ir.Endpoint{Node: "pass", Port: "out"}, Type: valueType},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func telemetryGraph(t *testing.T, descriptor element.Descriptor) ir.Graph {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	valueType := element.Event(element.Named("test.Value"))
	cancelType := element.Interrupt(element.Named("flow.RunID"))
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "telemetry", Revision: 1,
		Nodes: []ir.Node{{
			ID: "telemetry", Element: identity,
			Ports: []ir.Port{
				{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
				{Name: "cancel", Direction: element.Input, Type: cancelType, Cardinality: element.One, Required: true, DefaultDepth: 2},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			},
			Reaction: descriptor.Reaction,
		}},
		Boundaries: []ir.Boundary{
			{Name: "input", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "telemetry", Port: "in"}, Type: valueType},
			{Name: "cancel", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "telemetry", Port: "cancel"}, Type: cancelType},
			{Name: "output", Direction: ir.OutputBoundary, Endpoint: ir.Endpoint{Node: "telemetry", Port: "out"}, Type: valueType},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func passChainGraph(t *testing.T, descriptor element.Descriptor) ir.Graph {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	valueType := element.Event(element.Named("test.Value"))
	node := func(id string) ir.Node {
		return ir.Node{
			ID: id, Element: identity,
			Ports: []ir.Port{
				{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			},
			Reaction: descriptor.Reaction,
		}
	}
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "pass-chain", Revision: 1,
		Nodes: []ir.Node{node("first"), node("second")},
		Edges: []ir.Edge{{
			ID: "first-to-second", From: ir.Endpoint{Node: "first", Port: "out"},
			To: ir.Endpoint{Node: "second", Port: "in"}, Type: valueType,
			Delivery: ir.Lossless, Ordering: "fifo", Depth: 2,
		}},
		Boundaries: []ir.Boundary{
			{Name: "input", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "first", Port: "in"}, Type: valueType},
			{Name: "output", Direction: ir.OutputBoundary, Endpoint: ir.Endpoint{Node: "second", Port: "out"}, Type: valueType},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}
