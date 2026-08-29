package sidecar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	if artifact.ID == "" || artifact.ID != strings.TrimSpace(artifact.ID) {
		return errors.New("artifact identity requires a canonical ID")
	}
	if artifact.Revision != strings.TrimSpace(artifact.Revision) ||
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

// PortSelection tells a generic sidecar which descriptor ports the mounted
// graph actually connected. Direction is relative to the element.
type PortSelection struct {
	Name      string            `json:"name"`
	Direction element.Direction `json:"direction"`
	Type      element.Type      `json:"type"`
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
	Type              element.Type    `json:"type"`
	ItemID            string          `json:"item_id"`
	SessionID         string          `json:"session_id,omitempty"`
	SourceID          string          `json:"source_id,omitempty"`
	OpportunityID     string          `json:"opportunity_id,omitempty"`
	RunID             string          `json:"run_id,omitempty"`
	Sequence          uint64          `json:"sequence,omitempty"`
	CaptureNS         uint64          `json:"capture_ns,omitempty"`
	ReceiveNS         uint64          `json:"receive_ns,omitempty"`
	TraceID           string          `json:"trace_id,omitempty"`
	CancellationScope string          `json:"cancellation_scope,omitempty"`
	CausalParents     []string        `json:"causal_parents,omitempty"`
	JSON              json.RawMessage `json:"json,omitempty"`
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
	if len(wire.JSON) != 0 && !json.Valid(wire.JSON) {
		return errors.New("element envelope payload is not valid JSON")
	}
	return nil
}

func validateElementHello(message Message) error {
	if message.ElementDescriptor == nil {
		return errors.New("v4 hello requires an element descriptor")
	}
	if err := message.ElementDescriptor.Validate(); err != nil {
		return fmt.Errorf("v4 hello element descriptor: %w", err)
	}
	if len(message.ElementConfig) != 0 {
		if err := strictjson.Validate(message.ElementConfig); err != nil {
			return fmt.Errorf("v4 hello element config: %w", err)
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
	if message.ElementDescriptor == nil {
		return errors.New("v4 ready requires an element descriptor")
	}
	if err := message.ElementDescriptor.Validate(); err != nil {
		return fmt.Errorf("v4 ready element descriptor: %w", err)
	}
	if err := message.RuntimeArtifact.Validate(); err != nil {
		return fmt.Errorf("v4 ready runtime artifact: %w", err)
	}
	if _, err := CanonicalCapabilities(message.ResolvedCapabilities); err != nil {
		return fmt.Errorf("v4 ready capabilities: %w", err)
	}
	return nil
}

func validateElementFrame(message Message) error {
	if message.Port == "" || message.Port != strings.TrimSpace(message.Port) {
		return errors.New("element frame requires a canonical port name")
	}
	if message.Envelope == nil {
		return errors.New("element frame requires an envelope")
	}
	if err := message.Envelope.validate(); err != nil {
		return fmt.Errorf("element frame envelope: %w", err)
	}
	return nil
}

func validateSelections(descriptor element.Descriptor, selections []PortSelection) error {
	if len(selections) == 0 {
		return errors.New("at least one connected port is required")
	}
	seen := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		port, found := descriptor.Port(selection.Name)
		if !found {
			return fmt.Errorf("descriptor has no port %q", selection.Name)
		}
		if selection.Direction != port.Direction || !selection.Type.Equal(port.Type) {
			return fmt.Errorf("port %s selects %s %s, descriptor declares %s %s",
				selection.Name, selection.Direction, selection.Type.String(),
				port.Direction, port.Type.String())
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
	seen := make(map[string]struct{}, len(requirements))
	for index, requirement := range requirements {
		if requirement.Name == "" || requirement.Name != strings.TrimSpace(requirement.Name) ||
			requirement.Contract != strings.TrimSpace(requirement.Contract) {
			return fmt.Errorf("capability requirement %d is not canonical", index)
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
	result := slices.Clone(source)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		capability := &result[index]
		if capability.Name == "" || capability.Name != strings.TrimSpace(capability.Name) ||
			capability.Contract != strings.TrimSpace(capability.Contract) {
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
	if hello.Type != TypeHello || hello.Version != VersionElementGraph {
		return errors.New("element readiness requires a v4 hello")
	}
	if ready.Type != TypeReady || ready.Version != VersionElementGraph {
		return errors.New("element readiness requires a v4 ready frame")
	}
	if err := validateElementHello(hello); err != nil {
		return err
	}
	if err := validateElementReady(ready); err != nil {
		return err
	}
	want, err := hello.ElementDescriptor.Identity()
	if err != nil {
		return err
	}
	got, err := ready.ElementDescriptor.Identity()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("sidecar element descriptor drifted: expected %+v, live %+v", want, got)
	}
	capabilities, err := CanonicalCapabilities(ready.ResolvedCapabilities)
	if err != nil {
		return err
	}
	available := make(map[string][]CapabilityIdentity, len(capabilities))
	for _, capability := range capabilities {
		available[capability.Name] = append(available[capability.Name], capability)
	}
	for _, selection := range hello.SelectedPorts {
		requirement := CapabilityRequirement{
			Name:     PortCapabilityName(selection.Direction, selection.Name),
			Contract: selection.Type.String(),
		}
		capability, found := matchingCapability(available, requirement)
		if !found {
			return fmt.Errorf("sidecar did not prove selected port capability %s (%s)",
				requirement.Name, requirement.Contract)
		}
		if capability.Adapter == nil {
			return fmt.Errorf("sidecar selected port capability %s has no exact adapter identity",
				requirement.Name)
		}
	}
	for _, requirement := range hello.RequiredCapabilities {
		if _, found := matchingCapability(available, requirement); !found {
			return fmt.Errorf("sidecar did not prove required capability %s (%s)",
				requirement.Name, requirement.Contract)
		}
	}
	return nil
}

func matchingCapability(
	available map[string][]CapabilityIdentity, requirement CapabilityRequirement,
) (CapabilityIdentity, bool) {
	for _, capability := range available[requirement.Name] {
		if requirement.Contract == "" || capability.Contract == requirement.Contract {
			return capability, true
		}
	}
	return CapabilityIdentity{}, false
}
