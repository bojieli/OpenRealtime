// Package flow implements the standard, typed connector elements used to make
// branching, arbitration, dropping, and protocol transformations explicit in
// an OpenRealtime graph.
package flow

import (
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TeeDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "flow.Tee",
		Revision:      1,
		Generics:      []string{"P"},
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: element.Var("P"), Cardinality: element.One,
				Required: true, LossAllowed: true},
			{Name: "out", Direction: element.Output, Type: element.Var("P"), Cardinality: element.Variadic,
				Required: true, MinConnections: 1, LossAllowed: true},
		},
		Reaction: element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1},
	}
}

func EventMuxDescriptor() element.Descriptor {
	valueType := element.Event(element.Var("T"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "flow.EventMux",
		Revision:      1,
		Generics:      []string{"T"},
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.Variadic,
				Required: true, MinConnections: 1, LossAllowed: true},
			{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One,
				Required: true, LossAllowed: true},
		},
		Reaction: element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1},
	}
}

func DropDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "flow.Drop",
		Revision:      1,
		Generics:      []string{"P"},
		Ports: []element.Port{{
			Name: "in", Direction: element.Input, Type: element.Var("P"), Cardinality: element.One,
			Required: true, LossAllowed: true,
		}},
		Reaction: element.Reaction{Triggers: []string{"in"}, MaxConcurrency: 1},
	}
}

func IgnoreInterruptDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "flow.IgnoreInterrupt",
		Revision:      1,
		Generics:      []string{"K"},
		Ports: []element.Port{{
			Name: "in", Direction: element.Input,
			Type: element.Interrupt(element.Var("K")), Cardinality: element.One, Required: true,
		}},
		Reaction: element.Reaction{Triggers: []string{"in"}, MaxConcurrency: 1},
	}
}

func LatestDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "flow.Latest",
		Revision:      1,
		Generics:      []string{"T"},
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: element.Stream(element.Var("T")),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "state", Direction: element.Output, Type: element.State(element.Var("T")),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"state"}, MaxConcurrency: 1},
	}
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{
		TeeDescriptor(), EventMuxDescriptor(), DropDescriptor(), IgnoreInterruptDescriptor(), LatestDescriptor(),
	}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register flow descriptors: nil catalog")
	}
	for _, descriptor := range Descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register flow factories: nil registry")
	}
	for _, factory := range []element.Factory{
		teeFactory{}, eventMuxFactory{}, dropFactory{descriptor: DropDescriptor()},
		dropFactory{descriptor: IgnoreInterruptDescriptor()}, latestFactory{},
	} {
		if err := registry.Register("", factory); err != nil {
			return err
		}
	}
	return nil
}
