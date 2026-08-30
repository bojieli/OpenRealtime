package sidecar

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
)

// ElementSessionContract is the immutable result of one v4 Hello/Ready
// negotiation. Both client and server use it to validate direction, type,
// payload lanes, media profile, byte limits, and readiness drift.
type ElementSessionContract struct {
	hello        Message
	evidence     elementReadyEvidence
	selections   map[string]PortSelection
	negotiations map[string]PortNegotiation
}

type elementReadyEvidence struct {
	Element      element.Identity
	ConfigDigest string
	Runtime      ArtifactIdentity
	Capabilities []CapabilityIdentity
	Ports        []PortNegotiation
}

// NegotiateElementSession validates the complete v4 readiness proof and
// returns the contract that governs all later frames.
func NegotiateElementSession(hello, ready Message) (*ElementSessionContract, error) {
	if hello.Type != TypeHello || hello.Version != VersionElementGraph {
		return nil, errors.New("element readiness requires a v4 hello")
	}
	if ready.Type != TypeReady || ready.Version != VersionElementGraph {
		return nil, errors.New("element readiness requires a v4 ready frame")
	}
	if err := validateElementHello(hello); err != nil {
		return nil, err
	}
	if err := validateElementReady(ready); err != nil {
		return nil, err
	}
	want, err := hello.ElementDescriptor.Identity()
	if err != nil {
		return nil, err
	}
	got, err := ready.ElementDescriptor.Identity()
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("sidecar element descriptor drifted: expected %+v, live %+v", want, got)
	}
	wantConfigDigest := ElementConfigDigest(hello.ElementConfig)
	if ready.AppliedConfigDigest != wantConfigDigest {
		return nil, fmt.Errorf("sidecar applied element config digest is %q, expected %q",
			ready.AppliedConfigDigest, wantConfigDigest)
	}
	capabilities, err := CanonicalCapabilities(ready.ResolvedCapabilities)
	if err != nil {
		return nil, err
	}
	available := make(map[string][]CapabilityIdentity, len(capabilities))
	for _, capability := range capabilities {
		available[capability.Name] = append(available[capability.Name], capability)
	}
	negotiated, err := canonicalNegotiations(ready.NegotiatedPorts)
	if err != nil {
		return nil, err
	}
	negotiatedByPort := make(map[string]PortNegotiation, len(negotiated))
	for _, port := range negotiated {
		negotiatedByPort[portKey(port.Direction, port.Name)] = port
	}
	selections := make(map[string]PortSelection, len(hello.SelectedPorts))
	expectedPortCapabilities := make(map[string]string, len(hello.SelectedPorts))
	for _, original := range hello.SelectedPorts {
		selection := original
		selection.Type = original.Type.Clone()
		selection.Formats = cloneWireFormats(original.Formats)
		key := portKey(selection.Direction, selection.Name)
		selections[key] = selection
		negotiation, found := negotiatedByPort[key]
		if !found {
			return nil, fmt.Errorf("sidecar did not negotiate selected port %s", selection.Name)
		}
		if err := validateWireFormat(negotiation.Format); err != nil {
			return nil, fmt.Errorf("negotiated port %s format: %w", selection.Name, err)
		}
		matched := false
		for _, offered := range selection.Formats {
			if negotiation.Format.equal(offered) {
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("sidecar selected an unoffered format for port %s", selection.Name)
		}
		delete(negotiatedByPort, key)
		requirement := CapabilityRequirement{
			Name: PortCapabilityName(selection.Direction, selection.Name), Contract: selection.Type.String(),
		}
		capability, err := uniqueCapability(available, requirement)
		if err != nil {
			return nil, fmt.Errorf("selected port %s capability: %w", selection.Name, err)
		}
		if capability.Adapter == nil {
			return nil, fmt.Errorf("sidecar selected port capability %s has no exact adapter identity", requirement.Name)
		}
		expectedPortCapabilities[requirement.Name] = requirement.Contract
	}
	if len(negotiatedByPort) != 0 {
		names := make([]string, 0, len(negotiatedByPort))
		for _, port := range negotiatedByPort {
			names = append(names, string(port.Direction)+" "+port.Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("sidecar negotiated unselected port(s): %s", strings.Join(names, ", "))
	}
	for _, capability := range capabilities {
		if !strings.HasPrefix(capability.Name, "port.") {
			continue
		}
		contract, found := expectedPortCapabilities[capability.Name]
		if !found || contract != capability.Contract {
			return nil, fmt.Errorf("sidecar proved unselected or mismatched port capability %s (%s)",
				capability.Name, capability.Contract)
		}
	}
	for _, requirement := range hello.RequiredCapabilities {
		if _, err := uniqueCapability(available, requirement); err != nil {
			return nil, fmt.Errorf("required capability %s (%s): %w", requirement.Name, requirement.Contract, err)
		}
	}
	return &ElementSessionContract{
		hello: hello.Clone(), selections: selections,
		negotiations: negotiationsByKey(negotiated),
		evidence: elementReadyEvidence{
			Element: want, ConfigDigest: ready.AppliedConfigDigest, Runtime: ready.RuntimeArtifact,
			Capabilities: capabilities, Ports: negotiated,
		},
	}, nil
}

func negotiationsByKey(source []PortNegotiation) map[string]PortNegotiation {
	result := make(map[string]PortNegotiation, len(source))
	for _, negotiation := range source {
		copy := negotiation
		copy.Format = cloneWireFormats([]WireFormat{negotiation.Format})[0]
		result[portKey(copy.Direction, copy.Name)] = copy
	}
	return result
}

func uniqueCapability(
	available map[string][]CapabilityIdentity, requirement CapabilityRequirement,
) (CapabilityIdentity, error) {
	matches := make([]CapabilityIdentity, 0, 1)
	for _, capability := range available[requirement.Name] {
		if requirement.Contract == "" || capability.Contract == requirement.Contract {
			matches = append(matches, capability)
		}
	}
	switch len(matches) {
	case 0:
		return CapabilityIdentity{}, errors.New("not proved")
	case 1:
		return matches[0], nil
	default:
		return CapabilityIdentity{}, fmt.Errorf("ambiguous: %d live identities satisfy it", len(matches))
	}
}

// ValidateReady accepts an equivalent reordered proof and rejects any runtime,
// descriptor, capability, adapter, or negotiated-format drift.
func (contract *ElementSessionContract) ValidateReady(ready Message) error {
	if contract == nil {
		return errors.New("nil element session contract")
	}
	next, err := NegotiateElementSession(contract.hello, ready)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(next.evidence, contract.evidence) {
		return errors.New("sidecar config, runtime, capability, or negotiated port format identity changed after readiness")
	}
	return nil
}

// ValidateEngineFrame validates one post-readiness engine-to-sidecar frame.
func (contract *ElementSessionContract) ValidateEngineFrame(message Message) error {
	if contract == nil {
		return errors.New("nil element session contract")
	}
	if message.Type == TypeBye {
		return validateElementControlFrame(message)
	}
	if message.Type != TypeElementFrame {
		return fmt.Errorf("v4 engine sent non-element frame %q", message.Type)
	}
	return contract.validatePortFrame(message, element.Input)
}

// ValidateSidecarFrame validates one post-readiness sidecar-to-engine frame.
func (contract *ElementSessionContract) ValidateSidecarFrame(message Message) error {
	if contract == nil {
		return errors.New("nil element session contract")
	}
	switch message.Type {
	case TypeElementFrame:
		return contract.validatePortFrame(message, element.Output)
	case TypeReady:
		return contract.ValidateReady(message)
	case TypeError, TypeLog, TypeBye:
		return validateElementControlFrame(message)
	default:
		return fmt.Errorf("v4 sidecar sent non-element frame %q", message.Type)
	}
}

// validateElementControlFrame keeps identity and port fields out of the small
// post-readiness control vocabulary. Otherwise a peer could attach ignored
// readiness evidence to a log, error, or close frame and create two apparent
// session identities on one stream.
func validateElementControlFrame(message Message) error {
	if err := message.Validate(); err != nil {
		return err
	}
	allowed := Message{
		Type: message.Type, PayloadBytes: message.PayloadBytes, Payload: message.Payload,
	}
	switch message.Type {
	case TypeLog:
		allowed.Text, allowed.Level = message.Text, message.Level
		allowed.TimestampMS, allowed.Source = message.TimestampMS, message.Source
	case TypeError:
		allowed.Text, allowed.Error = message.Text, message.Error
		allowed.Code, allowed.Fatal = message.Code, message.Fatal
	case TypeBye:
	default:
		return fmt.Errorf("%q is not a v4 control frame", message.Type)
	}
	if !reflect.DeepEqual(message, allowed) {
		return fmt.Errorf("v4 %s contains readiness, port, or unrelated fields", message.Type)
	}
	return nil
}

func (contract *ElementSessionContract) validatePortFrame(
	message Message, direction element.Direction,
) error {
	if err := message.Validate(); err != nil {
		return err
	}
	key := portKey(direction, message.Port)
	selection, found := contract.selections[key]
	if !found {
		return fmt.Errorf("element frame uses unselected %s port %q", direction, message.Port)
	}
	if message.Envelope == nil || !message.Envelope.Type.Equal(selection.Type) {
		actual := "<nil>"
		if message.Envelope != nil {
			actual = message.Envelope.Type.String()
		}
		return fmt.Errorf("element frame port %s type is %s, negotiated %s",
			message.Port, actual, selection.Type.String())
	}
	negotiation := contract.negotiations[key]
	jsonBytes, binaryBytes := len(message.Envelope.JSON), len(message.Payload)
	switch negotiation.Format.PayloadMode {
	case PayloadJSON:
		if jsonBytes == 0 || binaryBytes != 0 {
			return fmt.Errorf("element frame port %s negotiated JSON-only payloads", message.Port)
		}
	case PayloadBinary:
		if jsonBytes != 0 || binaryBytes == 0 {
			return fmt.Errorf("element frame port %s negotiated binary-only payloads", message.Port)
		}
	case PayloadJSONBinary:
		if jsonBytes == 0 {
			return fmt.Errorf("element frame port %s requires JSON metadata with its optional binary lane", message.Port)
		}
	default:
		return fmt.Errorf("element frame port %s has unknown negotiated payload mode", message.Port)
	}
	if jsonBytes > negotiation.Format.MaxJSONBytes || binaryBytes > negotiation.Format.MaxBinaryBytes {
		return fmt.Errorf("element frame port %s payload is %d JSON/%d binary bytes, negotiated maxima are %d/%d",
			message.Port, jsonBytes, binaryBytes,
			negotiation.Format.MaxJSONBytes, negotiation.Format.MaxBinaryBytes)
	}
	media := negotiation.Format.Media
	switch {
	case media == nil && message.Envelope.Media != nil:
		return fmt.Errorf("element frame port %s did not negotiate live media metadata", message.Port)
	case media != nil && binaryBytes == 0 && message.Envelope.Media != nil:
		return fmt.Errorf("element frame port %s carries media metadata without a binary frame", message.Port)
	case media != nil && binaryBytes > 0 && message.Envelope.Media == nil:
		return fmt.Errorf("element frame port %s requires explicit media frame metadata", message.Port)
	case media != nil && binaryBytes > 0:
		if err := message.Envelope.Media.validateAgainst(*media); err != nil {
			return fmt.Errorf("element frame port %s media profile: %w", message.Port, err)
		}
	}
	return nil
}

// NegotiatedPort returns a defensive copy of one selected format.
func (contract *ElementSessionContract) NegotiatedPort(
	direction element.Direction, name string,
) (PortNegotiation, bool) {
	if contract == nil {
		return PortNegotiation{}, false
	}
	negotiation, found := contract.negotiations[portKey(direction, name)]
	if !found {
		return PortNegotiation{}, false
	}
	negotiation.Format = cloneWireFormats([]WireFormat{negotiation.Format})[0]
	return negotiation, true
}

// ElementConformanceFixture builds a strict Ready proof for tests and minimal
// servers. It selects the first offered format for every port and still runs
// the same negotiation validator before returning it.
type ElementConformanceFixture struct {
	Runtime      ArtifactIdentity
	Provider     ArtifactIdentity
	Adapter      ArtifactIdentity
	Capabilities []CapabilityIdentity
}

func (fixture ElementConformanceFixture) Ready(hello Message) (Message, error) {
	if err := validateElementHello(hello); err != nil {
		return Message{}, err
	}
	for label, artifact := range map[string]ArtifactIdentity{
		"fixture runtime": fixture.Runtime, "fixture provider": fixture.Provider,
		"fixture adapter": fixture.Adapter,
	} {
		if err := artifact.Validate(); err != nil {
			return Message{}, fmt.Errorf("%s: %w", label, err)
		}
	}
	descriptor := hello.ElementDescriptor.Clone()
	ready := Message{
		Type: TypeReady, Version: VersionElementGraph, ElementDescriptor: &descriptor,
		AppliedConfigDigest: ElementConfigDigest(hello.ElementConfig),
		RuntimeArtifact:     fixture.Runtime, ResolvedCapabilities: slices.Clone(fixture.Capabilities),
	}
	for _, selection := range hello.SelectedPorts {
		ready.NegotiatedPorts = append(ready.NegotiatedPorts, PortNegotiation{
			Name: selection.Name, Direction: selection.Direction,
			Format: cloneWireFormats(selection.Formats)[0],
		})
		ready.ResolvedCapabilities = append(ready.ResolvedCapabilities, CapabilityIdentity{
			Name:     PortCapabilityName(selection.Direction, selection.Name),
			Contract: selection.Type.String(), Provider: fixture.Provider,
			Adapter: &ArtifactIdentity{
				ID: fixture.Adapter.ID, Revision: fixture.Adapter.Revision, Digest: fixture.Adapter.Digest,
			},
		})
	}
	for _, requirement := range hello.RequiredCapabilities {
		found := false
		for _, capability := range ready.ResolvedCapabilities {
			if capability.Name == requirement.Name &&
				(requirement.Contract == "" || capability.Contract == requirement.Contract) {
				found = true
				break
			}
		}
		if !found {
			ready.ResolvedCapabilities = append(ready.ResolvedCapabilities, CapabilityIdentity{
				Name: requirement.Name, Contract: requirement.Contract, Provider: fixture.Provider,
			})
		}
	}
	if _, err := NegotiateElementSession(hello, ready); err != nil {
		return Message{}, err
	}
	return ready.Clone(), nil
}
