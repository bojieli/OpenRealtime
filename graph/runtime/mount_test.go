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
		"unknown": {"missing": json.RawMessage(`{}`)},
		"scalar":  {"pass": json.RawMessage(`true`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: graph, Registry: registry, Values: values,
			}); err == nil {
				t.Fatal("expected invalid values to fail")
			}
		})
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

func (factory passFactory) Descriptor() element.Descriptor { return factory.descriptor.Clone() }

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
