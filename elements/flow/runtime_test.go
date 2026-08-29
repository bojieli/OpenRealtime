package flow_test

import (
	"context"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	flowelements "github.com/bojieli/OpenRealtime/elements/flow"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TestTeeInfersRuntimeLanesAndCopiesToEveryBoundary(t *testing.T) {
	descriptor := flowelements.TeeDescriptor()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	valueType := element.Event(element.Named("test.Value"))
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "tee", Revision: 1,
		Nodes: []ir.Node{{
			ID: "tee", Element: identity,
			Ports: []ir.Port{
				{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One,
					Required: true, LossAllowed: true},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.Variadic,
					Required: true, MinConnections: 1, LossAllowed: true,
					Lanes: []string{"boundary:left", "boundary:right"}},
			},
			Reaction: descriptor.Reaction,
		}},
		Boundaries: []ir.Boundary{
			{Name: "input", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "tee", Port: "in"}, Type: valueType},
			{Name: "left", Direction: ir.OutputBoundary, Endpoint: ir.Endpoint{Node: "tee", Port: "out", Lane: "boundary:left"}, Type: valueType},
			{Name: "right", Direction: ir.OutputBoundary, Endpoint: ir.Endpoint{Node: "tee", Port: "out", Lane: "boundary:right"}, Type: valueType},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := flowelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{Graph: graph, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(runContext) }()
	input, _ := mounted.Ingress("input")
	left, _ := mounted.Egress("left")
	right, _ := mounted.Egress("right")
	message := element.Envelope{Type: valueType, ItemID: "copy-me", Payload: "immutable"}
	if _, err := input.Broadcast(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]element.InputPort{"left": left, "right": right} {
		got, err := output.Receive(context.Background())
		if err != nil || got.ItemID != message.ItemID || got.Payload != message.Payload {
			t.Fatalf("%s output = %+v, %v", name, got, err)
		}
	}
	cancelRun()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("tee did not stop")
	}
}

func TestEveryBuiltinDescriptorAndFactoryRegisters(t *testing.T) {
	for _, descriptor := range flowelements.Descriptors() {
		if err := descriptor.Validate(); err != nil {
			t.Fatalf("descriptor %s: %v", descriptor.Name, err)
		}
	}
	registry := graphruntime.NewRegistry()
	if err := flowelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
}

func TestEveryBuiltinReportsExactPureLiveResolution(t *testing.T) {
	for _, descriptor := range flowelements.Descriptors() {
		descriptor := descriptor
		t.Run(descriptor.Name, func(t *testing.T) {
			graph := pureFlowGraph(t, descriptor)
			registry := graphruntime.NewRegistry()
			if err := flowelements.RegisterFactories(registry); err != nil {
				t.Fatal(err)
			}
			mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: graph, Registry: registry,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- mounted.Run(ctx) }()
			deadline := time.Now().Add(time.Second)
			for {
				resolution := mounted.Live().Nodes["node"].Resolution
				if resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive &&
					resolution.Runtime.ID == "builtin://openrealtime/elements/"+descriptor.Name &&
					resolution.Runtime.Revision == "implementation:1" &&
					resolution.CapabilitiesEvidence == inspect.EvidenceLive &&
					len(resolution.Capabilities) == 0 {
					break
				}
				if time.Now().After(deadline) {
					cancel()
					t.Fatalf("pure flow live resolution = %+v", resolution)
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("flow node did not stop")
			}
		})
	}
}

func pureFlowGraph(t *testing.T, descriptor element.Descriptor) ir.Graph {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	ports := make([]ir.Port, len(descriptor.Ports))
	boundaries := make([]ir.Boundary, 0, len(descriptor.Ports))
	for index, port := range descriptor.Ports {
		resolvedType := concreteFlowType(descriptor.Name, port.Name)
		ports[index] = ir.Port{
			Name: port.Name, Direction: port.Direction, Type: resolvedType,
			Cardinality: port.Cardinality, Required: port.Required,
			MinConnections: port.MinConnections, LossAllowed: port.LossAllowed,
			DefaultDepth: port.DefaultDepth,
		}
		endpoint := ir.Endpoint{Node: "node", Port: port.Name}
		if port.Cardinality == element.Variadic {
			endpoint.Lane = "boundary:" + port.Name
			ports[index].Lanes = []string{endpoint.Lane}
		}
		direction := ir.InputBoundary
		if port.Direction == element.Output {
			direction = ir.OutputBoundary
		}
		boundaries = append(boundaries, ir.Boundary{
			Name: port.Name, Direction: direction, Endpoint: endpoint, Type: resolvedType,
		})
	}
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "flow-live", Revision: 1,
		Nodes: []ir.Node{{
			ID: "node", Element: identity, Ports: ports, Reaction: descriptor.Reaction,
		}},
		Boundaries: boundaries,
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func concreteFlowType(elementName, portName string) element.Type {
	switch elementName {
	case "flow.IgnoreInterrupt":
		return element.Interrupt(element.Named("test.Key"))
	case "flow.Latest":
		if portName == "in" {
			return element.Stream(element.Named("test.Value"))
		}
		return element.State(element.Named("test.Value"))
	default:
		return element.Event(element.Named("test.Value"))
	}
}
