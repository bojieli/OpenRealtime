package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

// waitForTraceEvents returns the trace once it holds at least want records, or
// the last snapshot it saw when the deadline passes. The caller asserts on the
// result, so a trace that never fills still fails - it just fails having given
// the runtime a bounded chance to finish writing rather than none at all.
func waitForTraceEvents(
	t *testing.T, tracer *graphruntime.BufferTracer, want int,
) []graphruntime.TraceEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events := tracer.Events()
		if len(events) >= want || time.Now().After(deadline) {
			return events
		}
		time.Sleep(5 * time.Millisecond)
	}
}

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
	// Receiving the egress envelope proves the message crossed the graph; it
	// does not prove the runtime has finished recording that crossing. The
	// trace records are written by the runtime's own goroutines, so a test that
	// reads them the instant Receive returns is racing the last one or two, and
	// on a loaded machine it loses: the CI runner reported "trace has only 3
	// events" while every assertion around it held. Waiting for the count is
	// not a weaker check than reading it once - the bound below still fails if
	// the fourth record never arrives, which is the only thing this assertion
	// was ever able to catch.
	events := waitForTraceEvents(t, tracer, 4)
	live := mounted.Live()
	if live.Fingerprint != graph.Fingerprint || live.Edges["boundary:input"].Enqueued != 1 ||
		live.Edges["boundary:output"].Dequeued != 1 {
		t.Fatalf("unexpected live snapshot: %+v", live)
	}
	if len(events) < 4 {
		t.Fatalf("trace has only %d events: %+v", len(events), events)
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
		TraceID: "task-7", RunID: "run-7", Sequence: 1,
		CausalParents: []string{"observation-6", "state-revision-4"}, Payload: "hello",
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
		len(flow.EdgeNS) != 1 || flow.EdgeNS[0] < flow.FirstNS || flow.EdgeNS[0] > flow.LastNS ||
		len(flow.CausalStages) != 1 || flow.CausalStages[0].Item != message.ItemID ||
		!slices.Equal(flow.CausalStages[0].Parents, message.CausalParents) ||
		flow.Correlation != "trace:task-7" || flow.Truncated {
		t.Fatalf("correlated flow = %+v, found=%t", flow, found)
	}
	// Live snapshots are recursively independent management-plane values.
	live.Nodes["first"].Resolution.Capabilities[0].Provider.ID = "mutated"
	live.Configuration.ID = "mutated"
	originalEdgeNS := flow.EdgeNS[0]
	mutatedFlow := live.Flows["trace:task-7"]
	mutatedFlow.Edges[0] = "mutated"
	mutatedFlow.EdgeNS[0]++
	mutatedFlow.CausalStages[0].Item = "mutated"
	mutatedFlow.CausalStages[0].Parents[0] = "mutated"
	live.Flows["trace:task-7"] = mutatedFlow
	again := mounted.Live()
	if again.Nodes["first"].Resolution.Capabilities[0].Provider.ID != "provider://echo" ||
		again.Flows["trace:task-7"].Edges[0] != "first-to-second" ||
		again.Flows["trace:task-7"].EdgeNS[0] != originalEdgeNS ||
		again.Flows["trace:task-7"].CausalStages[0].Item != message.ItemID ||
		again.Flows["trace:task-7"].CausalStages[0].Parents[0] != message.CausalParents[0] ||
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
		RunID: "run-telemetry", TraceID: "trace-telemetry", Payload: invalidInspectionDecision{},
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
	if before.AuthorityDecision != nil {
		t.Fatalf("runtime retained an invalid inspection decision: %+v", before.AuthorityDecision)
	}
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

type invalidInspectionDecision struct{}

func (invalidInspectionDecision) InspectionDecision() element.InspectionDecision {
	return element.InspectionDecision{Kind: "private-payload", Operation: element.DecisionResult}
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

func TestMountExposesOnlyDeclaredServicesToElement(t *testing.T) {
	descriptor := passDescriptor([]element.Dependency{
		{Name: "models.fast"},
		{Name: "models.optional", Optional: true},
	})
	registry := graphruntime.NewRegistry()
	probe := serviceProbeFactory{
		passFactory: passFactory{descriptor: descriptor},
		inspect: func(services element.Services) error {
			value, revision, found := services.Lookup("models.fast")
			if !found || value != "fast-provider" || revision != 1 {
				return errors.New("required declared service was not exposed exactly")
			}
			if value, revision, found = services.Lookup("models.optional"); found || value != nil || revision != 0 {
				return errors.New("absent optional service was reported as available")
			}
			if value, revision, found = services.Lookup("models.ambient"); found || value != nil || revision != 0 {
				return errors.New("undeclared ambient service was exposed")
			}
			return nil
		},
	}
	if err := registry.Register("", probe); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set("models.fast", "fast-provider"); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Set("models.ambient", "ambient-provider"); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: passGraph(t, descriptor), Registry: registry, Services: services,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMountRejectsUnsafeCausalInspectionBound(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	if err := registry.Register("", passFactory{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	graph := passGraph(t, descriptor)
	for _, bound := range []int{-1, inspect.MaximumCausalParentsPerStage + 1} {
		_, err := graphruntime.Mount(context.Background(), graphruntime.Config{
			Graph: graph, Registry: registry,
			Inspection: graphruntime.InspectionConfig{MaxCausalParentsPerStage: bound},
		})
		if err == nil || !strings.Contains(err.Error(), "inspection bounds") {
			t.Fatalf("causal parent bound %d error = %v", bound, err)
		}
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

func TestLifecycleWorkerFailureCancelsMountedGraph(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	release := make(chan struct{})
	workerFailure := errors.New("auxiliary reader failed")
	if err := registry.Register("", lifecycleWorkerFactory{
		descriptor: descriptor, release: release, failure: workerFailure,
	}); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: passGraph(t, descriptor), Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mounted.Run(context.Background()) }()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, workerFailure) ||
			!strings.Contains(err.Error(), "graph node pass lifecycle failed") {
			t.Fatalf("graph lifecycle failure = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle worker failure did not stop graph")
	}
	if live := mounted.Live().Nodes["pass"]; live.State != "failed" ||
		!strings.Contains(live.Error, workerFailure.Error()) {
		t.Fatalf("failed node lifecycle evidence = %+v", live)
	}
}

func TestMountFailureBoundsAndReportsCurrentLifecycleCleanup(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	release := make(chan struct{})
	if err := registry.Register("", failingLifecycleFactory{
		descriptor: descriptor, release: release,
	}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: passGraph(t, descriptor), Registry: registry,
		ShutdownTimeout: 10 * time.Millisecond,
	})
	close(release)
	if err == nil || !strings.Contains(err.Error(), "factory failed after worker admission") ||
		!strings.Contains(err.Error(), "live workers: [stubborn]") {
		t.Fatalf("bounded mount rollback error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("mount rollback exceeded its bound: %s", elapsed)
	}
}

func TestMountAndRunContainElementPanics(t *testing.T) {
	descriptor := passDescriptor(nil)
	t.Run("factory", func(t *testing.T) {
		registry := graphruntime.NewRegistry()
		if err := registry.Register("", panickingFactory{
			descriptor: descriptor, phase: "factory",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
			Graph: passGraph(t, descriptor), Registry: registry,
		}); err == nil || !strings.Contains(err.Error(), "factory panicked: factory panic") {
			t.Fatalf("contained factory panic = %v", err)
		}
	})

	t.Run("runnable", func(t *testing.T) {
		registry := graphruntime.NewRegistry()
		if err := registry.Register("", panickingFactory{
			descriptor: descriptor, phase: "runnable",
		}); err != nil {
			t.Fatal(err)
		}
		mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
			Graph: passGraph(t, descriptor), Registry: registry,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = mounted.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "graph node pass stopped: panicked: runnable panic") {
			t.Fatalf("contained runnable panic = %v", err)
		}
		if live := mounted.Live().Nodes["pass"]; live.State != "failed" ||
			!strings.Contains(live.Error, "runnable panic") {
			t.Fatalf("runnable panic live state = %+v", live)
		}
	})
}

type passFactory struct {
	descriptor element.Descriptor
	disposed   *atomic.Bool
}

type permissivePassFactory struct{ passFactory }

type resolvingPassFactory struct{ passFactory }

type telemetryFactory struct{ descriptor element.Descriptor }

type lifecycleWorkerFactory struct {
	descriptor element.Descriptor
	release    <-chan struct{}
	failure    error
}

type failingLifecycleFactory struct {
	descriptor element.Descriptor
	release    <-chan struct{}
}

type panickingFactory struct {
	descriptor element.Descriptor
	phase      string
}

type serviceProbeFactory struct {
	passFactory
	inspect func(element.Services) error
}

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

func (factory lifecycleWorkerFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory lifecycleWorkerFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if err := mount.Lifecycle.Go("auxiliary-reader", func(context.Context) error {
		<-factory.release
		return factory.failure
	}); err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}), nil
}

func (factory failingLifecycleFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory failingLifecycleFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if err := mount.Lifecycle.Go("stubborn", func(context.Context) error {
		<-factory.release
		return nil
	}); err != nil {
		return nil, err
	}
	return nil, errors.New("factory failed after worker admission")
}

func (factory panickingFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory panickingFactory) Mount(
	_ context.Context, _ element.MountContext,
) (element.Runnable, error) {
	if factory.phase == "factory" {
		panic("factory panic")
	}
	return element.RunnableFunc(func(context.Context) error {
		panic("runnable panic")
	}), nil
}

func (factory serviceProbeFactory) Mount(
	ctx context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if factory.inspect != nil {
		if err := factory.inspect(mount.Services); err != nil {
			return nil, err
		}
	}
	return factory.passFactory.Mount(ctx, mount)
}

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
