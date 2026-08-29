package sdk_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/sdk"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

type valueProtocol struct{}

func TestBuilderAndNetlistCompileToSameFingerprint(t *testing.T) {
	catalog := resolve.NewCatalog()
	for _, descriptor := range descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	builder := sdk.New("generated")
	source := builder.Add("source", "test.Source")
	sink := builder.Add("sink", "test.Sink")
	sdk.Connect(builder, sdk.Output[valueProtocol](source, "out"), sdk.Input[valueProtocol](sink, "in"))
	sdk.ExportInput(builder, "trigger", sdk.Input[valueProtocol](source, "trigger"))
	built, err := builder.Compile(graphcompiler.Options{Catalog: catalog, ResolutionMode: resolve.Update})
	if err != nil {
		t.Fatal(err)
	}
	text, err := builder.Source()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("generated.ortg", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	declarative, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: built.Lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	if built.Graph.Fingerprint != declarative.Graph.Fingerprint {
		t.Fatalf("builder fingerprint = %s, netlist = %s\n%s",
			built.Graph.Fingerprint, declarative.Graph.Fingerprint, text)
	}
}

func descriptors() []element.Descriptor {
	valueType := element.Event(element.Named("test.Value"))
	return []element.Descriptor{
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.Source", Revision: 1,
			Ports: []element.Port{
				{Name: "trigger", Direction: element.Input, Type: element.Trigger(element.Named("test.Start")), Cardinality: element.One, Required: true},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true},
			},
			Reaction: element.Reaction{Triggers: []string{"trigger"}, Outcomes: []string{"out"}},
		},
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.Sink", Revision: 1,
			Ports: []element.Port{{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true}},
		},
	}
}
