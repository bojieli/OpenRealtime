// Package flow implements the standard, typed connector elements used to make
// branching, arbitration, dropping, and protocol transformations explicit in
// an OpenRealtime graph.
package flow

import (
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

const flowImplementationRevision = "implementation:1"

func flowRuntimeID(descriptor element.Descriptor) string {
	return "builtin://openrealtime/elements/" + descriptor.Name
}

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

// MuxDescriptor is the protocol-generic explicit multiple-writer connector.
// Unlike EventMux it can arbitrate Requests, Replies, Streams, Segments,
// States, or Triggers without erasing their protocol type. ReceiveAny gives
// every materialized lane fair serialized admission; it never interleaves or
// rewrites an envelope. Stream-aware selection remains a policy element, not
// a property of this mechanical connector.
func MuxDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "flow.Mux",
		Revision:      1,
		Generics:      []string{"P"},
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: element.Var("P"), Cardinality: element.Variadic,
				Required: true, MinConnections: 1, LossAllowed: true},
			{Name: "out", Direction: element.Output, Type: element.Var("P"), Cardinality: element.One,
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
		TeeDescriptor(), MuxDescriptor(), EventMuxDescriptor(), DropDescriptor(), IgnoreInterruptDescriptor(), LatestDescriptor(),
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
	registrations, err := FactoryRegistrations()
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if err := registry.RegisterFactory(registration); err != nil {
			return err
		}
	}
	return nil
}

func FactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	entries := make([]factoryprofile.Entry, 0, len(Descriptors()))
	for _, factory := range []element.Factory{
		teeFactory{}, muxFactory{descriptor: MuxDescriptor()}, muxFactory{descriptor: EventMuxDescriptor()}, dropFactory{descriptor: DropDescriptor()},
		dropFactory{descriptor: IgnoreInterruptDescriptor()}, latestFactory{},
	} {
		entries = append(entries, factoryprofile.Entry{Factory: factory, Artifact: inspect.ArtifactIdentity{
			ID: flowRuntimeID(factory.Descriptor()), Revision: flowImplementationRevision,
		}})
	}
	return factoryprofile.Registrations(entries...)
}
