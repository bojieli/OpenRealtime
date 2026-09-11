package acoustic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

type admissionFactory struct{}

func (admissionFactory) Descriptor() element.Descriptor { return EnergyAdmissionDescriptor() }

func (admissionFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeAdmissionConfig(source)
	return err
}

func (admissionFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeAdmissionConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("acoustic.EnergyAdmission %s config: %w", mount.InstanceID, err)
	}
	ports, err := admissionPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return newAdmissionRunner(mount.InstanceID, config, ports, mount.Resolution), nil
}

type endpointFactory struct{}

func (endpointFactory) Descriptor() element.Descriptor { return EndpointPolicyDescriptor() }

func (endpointFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeEndpointConfig(source)
	return err
}

func (endpointFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeEndpointConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("acoustic.EndpointPolicy %s config: %w", mount.InstanceID, err)
	}
	ports, err := endpointPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return newEndpointRunner(mount.InstanceID, config, ports, mount.Resolution), nil
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{EnergyAdmissionDescriptor(), EndpointPolicyDescriptor(), NoiseFilterDescriptor()}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register acoustic descriptors: nil catalog")
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
		return errors.New("register acoustic factories: nil registry")
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
	return factoryprofile.Registrations(
		factoryprofile.Entry{Factory: noiseFilterFactory{}, Artifact: inspect.ArtifactIdentity{ID: noiseFilterRuntimeID, Revision: "implementation:1"}},
		factoryprofile.Entry{Factory: admissionFactory{}, Artifact: inspect.ArtifactIdentity{
			ID: admissionRuntimeID, Revision: admissionImplementationRevision,
		}},
		factoryprofile.Entry{Factory: endpointFactory{}, Artifact: inspect.ArtifactIdentity{
			ID: endpointRuntimeID, Revision: endpointImplementationRevision,
		}},
	)
}

type admissionPorts struct {
	audio, policy, command, cancel          element.InputPort
	admitted, candidate, activity, endpoint element.OutputPort
	state, outcome                          element.OutputPort
}

func admissionPortsFrom(ports element.Ports) (admissionPorts, error) {
	if ports == nil {
		return admissionPorts{}, errors.New("acoustic.EnergyAdmission has nil ports")
	}
	var result admissionPorts
	inputs := []struct {
		name string
		set  *element.InputPort
	}{
		{"audio", &result.audio}, {"policy", &result.policy},
		{"command", &result.command}, {"cancel", &result.cancel},
	}
	for _, input := range inputs {
		port, err := ports.Input(input.name)
		if err != nil {
			return admissionPorts{}, err
		}
		*input.set = port
	}
	outputs := []struct {
		name string
		set  *element.OutputPort
	}{
		{"admitted", &result.admitted}, {"candidate", &result.candidate},
		{"activity", &result.activity}, {"endpoint", &result.endpoint},
		{"state", &result.state}, {"outcome", &result.outcome},
	}
	for _, output := range outputs {
		port, err := ports.Output(output.name)
		if err != nil {
			return admissionPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type endpointPorts struct {
	candidate, tick, commit, verdict, cancel element.InputPort
	command, state, outcome                  element.OutputPort
}

func endpointPortsFrom(ports element.Ports) (endpointPorts, error) {
	if ports == nil {
		return endpointPorts{}, errors.New("acoustic.EndpointPolicy has nil ports")
	}
	var result endpointPorts
	inputs := []struct {
		name string
		set  *element.InputPort
	}{
		{"candidate", &result.candidate}, {"tick", &result.tick},
		{"commit", &result.commit}, {"verdict", &result.verdict}, {"cancel", &result.cancel},
	}
	for _, input := range inputs {
		port, err := ports.Input(input.name)
		if err != nil {
			return endpointPorts{}, err
		}
		*input.set = port
	}
	outputs := []struct {
		name string
		set  *element.OutputPort
	}{
		{"command", &result.command}, {"state", &result.state}, {"outcome", &result.outcome},
	}
	for _, output := range outputs {
		port, err := ports.Output(output.name)
		if err != nil {
			return endpointPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

var (
	_ element.Factory         = admissionFactory{}
	_ element.ConfigValidator = admissionFactory{}
	_ element.Factory         = endpointFactory{}
	_ element.ConfigValidator = endpointFactory{}
)
