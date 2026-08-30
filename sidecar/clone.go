package sidecar

import (
	"slices"
)

// Clone returns an independent message suitable for retaining as a readiness
// baseline or handing to an untrusted session implementation.
func (message Message) Clone() Message {
	result := message
	result.Capabilities = slices.Clone(message.Capabilities)
	if message.Tools != nil {
		result.Tools = make([]Tool, len(message.Tools))
		for index, tool := range message.Tools {
			result.Tools[index] = tool
			result.Tools[index].Parameters = slices.Clone(tool.Parameters)
		}
	}
	if message.ElementDescriptor != nil {
		descriptor := message.ElementDescriptor.Clone()
		result.ElementDescriptor = &descriptor
	}
	result.ElementConfig = slices.Clone(message.ElementConfig)
	if message.SelectedPorts != nil {
		result.SelectedPorts = make([]PortSelection, len(message.SelectedPorts))
		for index, selection := range message.SelectedPorts {
			result.SelectedPorts[index] = PortSelection{
				Name: selection.Name, Direction: selection.Direction, Type: selection.Type.Clone(),
				Formats: cloneWireFormats(selection.Formats),
			}
		}
	}
	result.RequiredCapabilities = slices.Clone(message.RequiredCapabilities)
	if message.ResolvedCapabilities != nil {
		result.ResolvedCapabilities = make([]CapabilityIdentity, len(message.ResolvedCapabilities))
		for index, capability := range message.ResolvedCapabilities {
			result.ResolvedCapabilities[index] = capability
			if capability.Adapter != nil {
				adapter := *capability.Adapter
				result.ResolvedCapabilities[index].Adapter = &adapter
			}
		}
	}
	if message.NegotiatedPorts != nil {
		result.NegotiatedPorts = make([]PortNegotiation, len(message.NegotiatedPorts))
		for index, negotiation := range message.NegotiatedPorts {
			result.NegotiatedPorts[index] = PortNegotiation{
				Name: negotiation.Name, Direction: negotiation.Direction,
				Format: cloneWireFormats([]WireFormat{negotiation.Format})[0],
			}
		}
	}
	if message.Envelope != nil {
		envelope := message.Envelope.Clone()
		result.Envelope = &envelope
	}
	result.Arguments = slices.Clone(message.Arguments)
	result.Output = slices.Clone(message.Output)
	result.Payload = slices.Clone(message.Payload)
	return result
}

// Clone returns an independent wire envelope.
func (wire WireEnvelope) Clone() WireEnvelope {
	result := wire
	result.Type = wire.Type.Clone()
	result.CausalParents = slices.Clone(wire.CausalParents)
	if wire.Media != nil {
		media := *wire.Media
		result.Media = &media
	}
	result.JSON = slices.Clone(wire.JSON)
	return result
}
