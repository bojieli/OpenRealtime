package sidecar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

// ArtifactIdentity names immutable runtime, provider, or adapter material.
// It intentionally mirrors the management-plane shape without importing the
// graph runtime into the process-boundary protocol.
type ArtifactIdentity struct {
	ID       string `json:"id"`
	Revision string `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

// Validate rejects mutable selectors and identities that cannot be attested.
func (artifact ArtifactIdentity) Validate() error {
	if err := validateBoundedIdentifier("artifact identity ID", artifact.ID); err != nil {
		return err
	}
	if artifact.Revision != strings.TrimSpace(artifact.Revision) ||
		len(artifact.Revision) > MaxElementIdentifierBytes ||
		strings.ContainsAny(artifact.Revision, "\x00\r\n") ||
		artifact.Digest != strings.TrimSpace(artifact.Digest) {
		return errors.New("artifact identity is not canonical")
	}
	if artifact.Revision == "" && artifact.Digest == "" {
		return errors.New("artifact identity requires a revision or digest")
	}
	if placeholderIdentity(artifact.ID) || placeholderIdentity(artifact.Revision) {
		return errors.New("artifact identity contains a mutable or placeholder selector")
	}
	if artifact.Digest != "" {
		const prefix = "sha256:"
		if !strings.HasPrefix(artifact.Digest, prefix) ||
			len(artifact.Digest) != len(prefix)+sha256.Size*2 {
			return errors.New("artifact identity has an invalid SHA-256 digest")
		}
		if artifact.Digest != strings.ToLower(artifact.Digest) {
			return errors.New("artifact identity has a non-canonical SHA-256 digest")
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(artifact.Digest, prefix)); err != nil {
			return fmt.Errorf("artifact identity has an invalid SHA-256 digest: %w", err)
		}
	}
	return nil
}

func placeholderIdentity(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, marker := range []string{"${", "{{", "<revision>", "<digest>", "<version>"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	for _, placeholder := range []string{"latest", "current", "unknown", "unresolved"} {
		if value == placeholder || strings.HasSuffix(value, ":"+placeholder) ||
			strings.HasSuffix(value, "@"+placeholder) || strings.HasSuffix(value, "/"+placeholder) {
			return true
		}
	}
	return false
}

// CapabilityIdentity is one live capability selected for this graph node.
// Provider and adapter remain independent evidence: a protocol adapter change
// is a changed treatment even when the model artifact is unchanged.
type CapabilityIdentity struct {
	Name     string            `json:"name"`
	Contract string            `json:"contract,omitempty"`
	Provider ArtifactIdentity  `json:"provider"`
	Adapter  *ArtifactIdentity `json:"adapter,omitempty"`
}

// CapabilityRequirement is an exact capability name and optional contract
// that a deployment must prove during readiness.
type CapabilityRequirement struct {
	Name     string `json:"name"`
	Contract string `json:"contract,omitempty"`
}

// ElementConfigDigest binds readiness to the exact ElementConfig bytes sent
// in Hello. Whitespace is intentionally significant: this attests the applied
// deployment input, while the graph's separate bound-config fingerprint
// attests its management-plane origin.
func ElementConfigDigest(config json.RawMessage) string {
	digest := sha256.Sum256(config)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validateConfigDigest(digest string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+sha256.Size*2 ||
		digest != strings.ToLower(digest) || digest != strings.TrimSpace(digest) {
		return errors.New("applied element config digest must be canonical SHA-256")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, prefix)); err != nil {
		return fmt.Errorf("applied element config digest must be canonical SHA-256: %w", err)
	}
	return nil
}

// PortSelection tells a generic sidecar which descriptor ports the mounted
// graph actually connected. Direction is relative to the element.
type PortSelection struct {
	Name      string            `json:"name"`
	Direction element.Direction `json:"direction"`
	Type      element.Type      `json:"type"`
	Formats   []WireFormat      `json:"formats"`
}

// PortCapabilityName is the stable live-capability identity for one selected
// descriptor port.
func PortCapabilityName(direction element.Direction, name string) string {
	return "port." + string(direction) + "." + name
}

// WireEnvelope carries the language-neutral portion of element.Envelope.
// JSON and Payload are owned by Message: JSON is suitable for ordinary values
// while Payload preserves a zero-copy-friendly binary lane for media codecs.
type WireEnvelope struct {
	Type              element.Type        `json:"type"`
	ItemID            string              `json:"item_id"`
	SessionID         string              `json:"session_id,omitempty"`
	SourceID          string              `json:"source_id,omitempty"`
	OpportunityID     string              `json:"opportunity_id,omitempty"`
	RunID             string              `json:"run_id,omitempty"`
	Sequence          uint64              `json:"sequence,omitempty"`
	CaptureNS         uint64              `json:"capture_ns,omitempty"`
	ReceiveNS         uint64              `json:"receive_ns,omitempty"`
	TraceID           string              `json:"trace_id,omitempty"`
	CancellationScope string              `json:"cancellation_scope,omitempty"`
	CausalParents     []string            `json:"causal_parents,omitempty"`
	Media             *MediaFrameMetadata `json:"media,omitempty"`
	JSON              json.RawMessage     `json:"json,omitempty"`
}

// FromEnvelope converts metadata without interpreting payload semantics.
func FromEnvelope(source element.Envelope, data json.RawMessage) WireEnvelope {
	return WireEnvelope{
		Type: source.Type.Clone(), ItemID: source.ItemID, SessionID: source.SessionID,
		SourceID: source.SourceID, OpportunityID: source.OpportunityID, RunID: source.RunID,
		Sequence: source.Sequence, CaptureNS: source.CaptureNS, ReceiveNS: source.ReceiveNS,
		TraceID: source.TraceID, CancellationScope: source.CancellationScope,
		CausalParents: slices.Clone(source.CausalParents), JSON: slices.Clone(data),
	}
}

// Envelope converts metadata and attaches a codec-decoded immutable payload.
func (wire WireEnvelope) Envelope(payload any) element.Envelope {
	return element.Envelope{
		Type: wire.Type.Clone(), ItemID: wire.ItemID, SessionID: wire.SessionID,
		SourceID: wire.SourceID, OpportunityID: wire.OpportunityID, RunID: wire.RunID,
		Sequence: wire.Sequence, CaptureNS: wire.CaptureNS, ReceiveNS: wire.ReceiveNS,
		TraceID: wire.TraceID, CancellationScope: wire.CancellationScope,
		CausalParents: slices.Clone(wire.CausalParents), Payload: payload,
	}
}

func (wire WireEnvelope) validate() error {
	if err := (wire.Envelope(nil)).ValidateFor(wire.Type); err != nil {
		return err
	}
	for label, value := range map[string]string{
		"item_id": wire.ItemID, "session_id": wire.SessionID, "source_id": wire.SourceID,
		"opportunity_id": wire.OpportunityID, "run_id": wire.RunID, "trace_id": wire.TraceID,
		"cancellation_scope": wire.CancellationScope,
	} {
		if value != strings.TrimSpace(value) || len(value) > MaxElementIdentifierBytes ||
			strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("element envelope %s is non-canonical or exceeds %d bytes",
				label, MaxElementIdentifierBytes)
		}
	}
	if len(wire.CausalParents) > MaxEnvelopeParents {
		return fmt.Errorf("element envelope has more than %d causal parents", MaxEnvelopeParents)
	}
	parents := make(map[string]struct{}, len(wire.CausalParents))
	for _, parent := range wire.CausalParents {
		if err := validateBoundedIdentifier("causal parent", parent); err != nil {
			return err
		}
		if _, duplicate := parents[parent]; duplicate {
			return fmt.Errorf("element envelope repeats causal parent %q", parent)
		}
		parents[parent] = struct{}{}
	}
	if len(wire.JSON) > MaxElementJSONBytes {
		return fmt.Errorf("element envelope JSON exceeds %d bytes", MaxElementJSONBytes)
	}
	if len(wire.JSON) != 0 {
		if err := strictjson.Validate(wire.JSON); err != nil {
			return fmt.Errorf("element envelope payload is not strict JSON: %w", err)
		}
	}
	if wire.Media != nil {
		if err := wire.Media.Validate(); err != nil {
			return fmt.Errorf("element envelope media metadata: %w", err)
		}
	}
	return nil
}

func validateElementHello(message Message) error {
	if message.PayloadBytes != 0 || len(message.Payload) != 0 {
		return errors.New("v4 hello cannot carry a binary payload")
	}
	if message.ElementDescriptor == nil {
		return errors.New("v4 hello requires an element descriptor")
	}
	allowed := Message{
		Type: message.Type, PayloadBytes: message.PayloadBytes, Payload: message.Payload,
		Version: message.Version, ElementDescriptor: message.ElementDescriptor,
		ElementConfig: message.ElementConfig, SelectedPorts: message.SelectedPorts,
		RequiredCapabilities: message.RequiredCapabilities,
	}
	if !reflect.DeepEqual(message, allowed) {
		return errors.New("v4 hello contains legacy, readiness, or frame fields")
	}
	if err := message.ElementDescriptor.Validate(); err != nil {
		return fmt.Errorf("v4 hello element descriptor: %w", err)
	}
	if len(message.ElementConfig) != 0 {
		if len(message.ElementConfig) > MaxElementJSONBytes {
			return fmt.Errorf("v4 hello element config exceeds %d bytes", MaxElementJSONBytes)
		}
		if err := strictjson.Validate(message.ElementConfig); err != nil {
			return fmt.Errorf("v4 hello element config: %w", err)
		}
		// ElementConfigDigest attests the exact bytes applied by the peer.
		// encoding/json compacts RawMessage values while constructing the
		// newline-delimited header, so accepting a differently spaced value
		// here would bind readiness to bytes that never crossed the transport.
		wire, err := json.Marshal(json.RawMessage(message.ElementConfig))
		if err != nil {
			return fmt.Errorf("v4 hello element config wire encoding: %w", err)
		}
		if !bytes.Equal(wire, message.ElementConfig) {
			return errors.New("v4 hello element config must use its compact on-wire JSON encoding")
		}
	}
	if err := validateSelections(*message.ElementDescriptor, message.SelectedPorts); err != nil {
		return fmt.Errorf("v4 hello selected ports: %w", err)
	}
	if err := validateRequirements(message.RequiredCapabilities); err != nil {
		return fmt.Errorf("v4 hello required capabilities: %w", err)
	}
	return nil
}

func validateElementReady(message Message) error {
	if message.PayloadBytes != 0 || len(message.Payload) != 0 {
		return errors.New("v4 ready cannot carry a binary payload")
	}
	if message.ElementDescriptor == nil {
		return errors.New("v4 ready requires an element descriptor")
	}
	allowed := Message{
		Type: message.Type, PayloadBytes: message.PayloadBytes, Payload: message.Payload,
		Version: message.Version, ElementDescriptor: message.ElementDescriptor,
		AppliedConfigDigest:  message.AppliedConfigDigest,
		RuntimeArtifact:      message.RuntimeArtifact,
		ResolvedCapabilities: message.ResolvedCapabilities, NegotiatedPorts: message.NegotiatedPorts,
	}
	if !reflect.DeepEqual(message, allowed) {
		return errors.New("v4 ready contains legacy, hello, or frame fields")
	}
	if err := message.ElementDescriptor.Validate(); err != nil {
		return fmt.Errorf("v4 ready element descriptor: %w", err)
	}
	if err := message.RuntimeArtifact.Validate(); err != nil {
		return fmt.Errorf("v4 ready runtime artifact: %w", err)
	}
	if err := validateConfigDigest(message.AppliedConfigDigest); err != nil {
		return fmt.Errorf("v4 ready: %w", err)
	}
	if _, err := CanonicalCapabilities(message.ResolvedCapabilities); err != nil {
		return fmt.Errorf("v4 ready capabilities: %w", err)
	}
	if _, err := canonicalNegotiations(message.NegotiatedPorts); err != nil {
		return fmt.Errorf("v4 ready negotiated ports: %w", err)
	}
	return nil
}

func validateElementFrame(message Message) error {
	if err := validateBoundedIdentifier("element frame port name", message.Port); err != nil {
		return err
	}
	if message.Envelope == nil {
		return errors.New("element frame requires an envelope")
	}
	if err := message.Envelope.validate(); err != nil {
		return fmt.Errorf("element frame envelope: %w", err)
	}
	allowed := Message{
		Type: message.Type, PayloadBytes: message.PayloadBytes, Port: message.Port,
		Envelope: message.Envelope, Payload: message.Payload,
	}
	if !reflect.DeepEqual(message, allowed) {
		return errors.New("element frame contains fields outside its port envelope")
	}
	return nil
}

func validateSelections(descriptor element.Descriptor, selections []PortSelection) error {
	if len(selections) == 0 {
		return errors.New("at least one connected port is required")
	}
	if len(selections) > MaxElementPorts {
		return fmt.Errorf("more than %d connected ports are selected", MaxElementPorts)
	}
	seen := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		if err := validateBoundedIdentifier("selected port name", selection.Name); err != nil {
			return err
		}
		port, found := descriptor.Port(selection.Name)
		if !found {
			return fmt.Errorf("descriptor has no port %q", selection.Name)
		}
		if selection.Direction != port.Direction || !selection.Type.Equal(port.Type) {
			return fmt.Errorf("port %s selects %s %s, descriptor declares %s %s",
				selection.Name, selection.Direction, selection.Type.String(),
				port.Direction, port.Type.String())
		}
		if err := selection.Type.ValidateConcretePort(); err != nil {
			return fmt.Errorf("port %s type: %w", selection.Name, err)
		}
		capabilityName := PortCapabilityName(selection.Direction, selection.Name)
		capabilityContract := selection.Type.String()
		if err := validateBoundedIdentifier("selected port capability name", capabilityName); err != nil {
			return fmt.Errorf("port %s capability identity: %w", selection.Name, err)
		}
		if len(capabilityContract) > MaxElementIdentifierBytes {
			return fmt.Errorf("port %s capability contract exceeds %d bytes",
				selection.Name, MaxElementIdentifierBytes)
		}
		if err := validateExplicitPortFormats(selection); err != nil {
			return fmt.Errorf("port %s formats: %w", selection.Name, err)
		}
		key := string(selection.Direction) + "\x00" + selection.Name
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("port %q is selected more than once", selection.Name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateRequirements(requirements []CapabilityRequirement) error {
	if len(requirements) > MaxElementRequirements {
		return fmt.Errorf("more than %d capabilities are required", MaxElementRequirements)
	}
	seen := make(map[string]struct{}, len(requirements))
	for index, requirement := range requirements {
		if validateBoundedIdentifier("capability requirement name", requirement.Name) != nil ||
			requirement.Contract != strings.TrimSpace(requirement.Contract) ||
			len(requirement.Contract) > MaxElementIdentifierBytes ||
			strings.ContainsAny(requirement.Contract, "\x00\r\n") {
			return fmt.Errorf("capability requirement %d is not canonical", index)
		}
		if strings.HasPrefix(requirement.Name, "port.") {
			return fmt.Errorf("capability requirement %d uses the reserved port capability namespace", index)
		}
		key := requirement.Name + "\x00" + requirement.Contract
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("capability requirement %q is repeated", requirement.Name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// CanonicalCapabilities validates, clones, and deterministically orders a
// complete live capability set.
func CanonicalCapabilities(source []CapabilityIdentity) ([]CapabilityIdentity, error) {
	if len(source) > MaxElementCapabilities {
		return nil, fmt.Errorf("more than %d capabilities are resolved", MaxElementCapabilities)
	}
	result := slices.Clone(source)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		capability := &result[index]
		if validateBoundedIdentifier("capability name", capability.Name) != nil ||
			capability.Contract != strings.TrimSpace(capability.Contract) ||
			len(capability.Contract) > MaxElementIdentifierBytes ||
			strings.ContainsAny(capability.Contract, "\x00\r\n") {
			return nil, fmt.Errorf("capability %d has a non-canonical name or contract", index)
		}
		if err := capability.Provider.Validate(); err != nil {
			return nil, fmt.Errorf("capability %s provider: %w", capability.Name, err)
		}
		if capability.Adapter != nil {
			copy := *capability.Adapter
			capability.Adapter = &copy
			if err := capability.Adapter.Validate(); err != nil {
				return nil, fmt.Errorf("capability %s adapter: %w", capability.Name, err)
			}
		}
		capability.Provider = ArtifactIdentity{
			ID: capability.Provider.ID, Revision: capability.Provider.Revision,
			Digest: capability.Provider.Digest,
		}
		key := capability.Name + "\x00" + capability.Contract + "\x00" + capability.Provider.ID
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("capability %q is repeated", capability.Name)
		}
		seen[key] = struct{}{}
	}
	sort.Slice(result, func(left, right int) bool {
		a, b := result[left], result[right]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Contract != b.Contract {
			return a.Contract < b.Contract
		}
		return a.Provider.ID < b.Provider.ID
	})
	return result, nil
}

// ValidateElementReady binds a live v4 Ready frame to the exact Hello
// descriptor, selected graph ports, and required capability contracts.
func ValidateElementReady(hello, ready Message) error {
	_, err := NegotiateElementSession(hello, ready)
	return err
}
