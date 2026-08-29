package flow_test

import (
	"context"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	flowelements "github.com/bojieli/OpenRealtime/elements/flow"
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
