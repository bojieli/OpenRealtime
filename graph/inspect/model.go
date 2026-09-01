// Package inspect derives static and live inspection models and generated
// diagrams from the exact immutable Graph IR mounted by the runtime.
package inspect

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

const LiveFormatVersion uint64 = 1

// Model is the frontend-neutral static graph view consumed by a canvas,
// console, or documentation renderer.
type Model struct {
	GraphID     string     `json:"graph_id"`
	Revision    uint64     `json:"revision"`
	Fingerprint string     `json:"fingerprint"`
	Nodes       []Node     `json:"nodes"`
	Edges       []Edge     `json:"edges,omitempty"`
	Boundaries  []Boundary `json:"boundaries,omitempty"`
	Scopes      []Scope    `json:"scopes,omitempty"`
}

type Node struct {
	ID                  string               `json:"id"`
	Element             element.Identity     `json:"element"`
	Implementation      string               `json:"implementation,omitempty"`
	ConfigReference     string               `json:"config_reference,omitempty"`
	ConfigDigest        string               `json:"config_digest,omitempty"`
	DeploymentReference string               `json:"deployment_reference,omitempty"`
	DeploymentDigest    string               `json:"deployment_digest,omitempty"`
	Ports               []Port               `json:"ports"`
	Reaction            element.Reaction     `json:"reaction"`
	Dependencies        []element.Dependency `json:"dependencies,omitempty"`
	Effects             []element.Effect     `json:"effects,omitempty"`
}

type Port struct {
	Name        string              `json:"name"`
	Direction   element.Direction   `json:"direction"`
	Type        string              `json:"type"`
	Cardinality element.Cardinality `json:"cardinality"`
	Lanes       []string            `json:"lanes,omitempty"`
	Role        string              `json:"role"`
}

type Edge struct {
	ID       string      `json:"id"`
	From     ir.Endpoint `json:"from"`
	To       ir.Endpoint `json:"to"`
	Type     string      `json:"type"`
	Role     string      `json:"role"`
	Delivery ir.Delivery `json:"delivery"`
	Depth    int         `json:"depth"`
}

type Boundary struct {
	Name      string               `json:"name"`
	Direction ir.BoundaryDirection `json:"direction"`
	Endpoint  ir.Endpoint          `json:"endpoint"`
	Type      string               `json:"type"`
	Role      string               `json:"role"`
}

type Scope struct {
	ID         string           `json:"id"`
	Parent     string           `json:"parent,omitempty"`
	Composite  element.Identity `json:"composite"`
	Nodes      []string         `json:"nodes"`
	Boundaries []ScopeBoundary  `json:"boundaries,omitempty"`
}

type ScopeBoundary struct {
	Name      string               `json:"name"`
	Direction ir.BoundaryDirection `json:"direction"`
	Endpoint  ir.Endpoint          `json:"endpoint"`
	Type      string               `json:"type"`
}

// Live overlays runtime evidence without mutating the static graph model.
// Sequence makes snapshots monotonically comparable within one mount.
type Live struct {
	FormatVersion uint64              `json:"format_version"`
	GraphID       string              `json:"graph_id"`
	GraphRevision uint64              `json:"graph_revision"`
	Fingerprint   string              `json:"fingerprint"`
	Configuration *ArtifactIdentity   `json:"configuration,omitempty"`
	Deployment    *DeploymentEvidence `json:"deployment,omitempty"`
	Sequence      uint64              `json:"sequence"`
	ObservedAt    time.Time           `json:"observed_at"`
	State         string              `json:"state"`
	Error         string              `json:"error,omitempty"`
	Nodes         map[string]NodeLive `json:"nodes,omitempty"`
	Edges         map[string]EdgeLive `json:"edges,omitempty"`
	Flows         map[string]FlowLive `json:"flows,omitempty"`
	TraceDropped  uint64              `json:"trace_dropped,omitempty"`
	// Adapter identifies the exact protocol-to-boundary projection outside
	// the graph node set. It is nil for legacy/compatibility mounts; a native
	// session must not masquerade this process boundary as an element node.
	Adapter *SessionAdapterResolution `json:"adapter,omitempty"`
}

// Clone returns a recursively independent management-plane snapshot.
func (live Live) Clone() Live {
	result := live
	if live.Configuration != nil {
		copy := *live.Configuration
		result.Configuration = &copy
	}
	if live.Deployment != nil {
		copy := live.Deployment.Clone()
		result.Deployment = &copy
	}
	if live.Adapter != nil {
		copy := *live.Adapter
		result.Adapter = &copy
	}
	result.Nodes = make(map[string]NodeLive, len(live.Nodes))
	for id, node := range live.Nodes {
		result.Nodes[id] = node.Clone()
	}
	result.Edges = make(map[string]EdgeLive, len(live.Edges))
	for id, edge := range live.Edges {
		result.Edges[id] = edge
	}
	result.Flows = make(map[string]FlowLive, len(live.Flows))
	for id, flow := range live.Flows {
		result.Flows[id] = flow.Clone()
	}
	return result
}

// SessionAdapterResolution is the exact live identity of the stable Realtime
// API to Graph IR boundary adapter. ProfileFingerprint binds its complete
// operation map and public capability/ownership projection; the two explicit
// digests let inspectors compare those dimensions without loading profile
// bytes. RuntimeEvidence is registered until an implementation-specific live
// handshake can strengthen it.
type SessionAdapterResolution struct {
	ContractName       string             `json:"contract_name"`
	ContractRevision   uint64             `json:"contract_revision"`
	ContractDigest     string             `json:"contract_digest"`
	ProfileFingerprint string             `json:"profile_fingerprint"`
	Implementation     string             `json:"implementation"`
	Runtime            ArtifactIdentity   `json:"runtime"`
	RuntimeEvidence    ResolutionEvidence `json:"runtime_evidence"`
	BoundaryMapDigest  string             `json:"boundary_map_digest"`
	ProjectionDigest   string             `json:"projection_digest"`
}

// Validate verifies the identity-bearing adapter evidence without importing a
// concrete gateway, binding, or plugin implementation into graph inspection.
func (resolution SessionAdapterResolution) Validate() error {
	if resolution.ContractName == "" || resolution.ContractName != strings.TrimSpace(resolution.ContractName) ||
		resolution.ContractRevision == 0 || resolution.Implementation == "" ||
		resolution.Implementation != strings.TrimSpace(resolution.Implementation) ||
		len(resolution.ContractName) > 64<<10 || len(resolution.Implementation) > 64<<10 ||
		strings.ContainsAny(resolution.ContractName+resolution.Implementation, "\x00\r\n") {
		return errors.New("session adapter resolution has a non-canonical contract or implementation")
	}
	for name, digest := range map[string]string{
		"contract": resolution.ContractDigest, "profile": resolution.ProfileFingerprint,
		"boundary map": resolution.BoundaryMapDigest, "projection": resolution.ProjectionDigest,
	} {
		if !validIdentityDigest(digest) {
			return fmt.Errorf("session adapter resolution has an invalid %s digest", name)
		}
	}
	if err := resolution.Runtime.Validate(); err != nil {
		return fmt.Errorf("session adapter resolution runtime: %w", err)
	}
	if len(resolution.Runtime.ID) > 64<<10 || len(resolution.Runtime.Revision) > 64<<10 ||
		strings.ContainsAny(resolution.Runtime.ID+resolution.Runtime.Revision, "\x00\r\n") ||
		(resolution.Runtime.Digest != "" && !validIdentityDigest(resolution.Runtime.Digest)) {
		return errors.New("session adapter resolution runtime identity is not canonical")
	}
	switch resolution.RuntimeEvidence {
	case EvidenceRegistered, EvidenceLive:
		return nil
	default:
		return fmt.Errorf("session adapter resolution runtime evidence is %q", resolution.RuntimeEvidence)
	}
}

func validIdentityDigest(value string) bool {
	const prefix = "sha256:"
	return strings.HasPrefix(value, prefix) && len(value) == len(prefix)+sha256.Size*2 &&
		validHexadecimal(value[len(prefix):], true)
}

type NodeLive struct {
	State          string          `json:"state"`
	ActiveRuns     int             `json:"active_runs"`
	LastTriggerID  string          `json:"last_trigger_id,omitempty"`
	LastOutcome    string          `json:"last_outcome,omitempty"`
	FirstTriggerNS uint64          `json:"first_trigger_ns,omitempty"`
	FirstOutputNS  uint64          `json:"first_output_ns,omitempty"`
	CompletionNS   uint64          `json:"completion_ns,omitempty"`
	CancellationNS uint64          `json:"cancellation_ns,omitempty"`
	Error          string          `json:"error,omitempty"`
	Resolution     *NodeResolution `json:"resolution,omitempty"`
}

// ResolutionEvidence states how an immutable runtime identity was learned.
// Declared identities are useful for diagnosis but are not benchmark
// attestation. Registered identities were bound to a factory at deployment;
// live identities were reported after a provider or sidecar handshake.
type ResolutionEvidence string

const (
	EvidenceDeclared   ResolutionEvidence = "declared"
	EvidenceRegistered ResolutionEvidence = "registered"
	EvidenceLive       ResolutionEvidence = "live"
)

// ArtifactIdentity names immutable code, model, device, or adapter material.
// At least one revision or SHA-256 digest is required.
type ArtifactIdentity struct {
	ID       string `json:"id"`
	Revision string `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

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
		if !validHexadecimal(artifact.Digest[len(prefix):], false) {
			return errors.New("artifact identity has an invalid SHA-256 digest")
		}
	}
	return nil
}

func validHexadecimal(value string, lowercaseOnly bool) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= '0' && character <= '9' ||
			character >= 'a' && character <= 'f' ||
			!lowercaseOnly && character >= 'A' && character <= 'F' {
			continue
		}
		return false
	}
	return true
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

// CapabilityIdentity is one provider capability selected by a live node.
// Provider and adapter are separate because changing an adapter behind an
// unchanged model is still a changed executable treatment.
type CapabilityIdentity struct {
	Name     string            `json:"name"`
	Contract string            `json:"contract,omitempty"`
	Provider ArtifactIdentity  `json:"provider"`
	Adapter  *ArtifactIdentity `json:"adapter,omitempty"`
}

// CanonicalCapabilities validates, clones, and deterministically orders a
// complete live capability set.
func CanonicalCapabilities(source []CapabilityIdentity) ([]CapabilityIdentity, error) {
	result := cloneCapabilities(source)
	seen := make(map[string]struct{}, len(result))
	for index, capability := range result {
		if capability.Name == "" || capability.Name != strings.TrimSpace(capability.Name) ||
			capability.Contract != strings.TrimSpace(capability.Contract) {
			return nil, fmt.Errorf("capability %d has a non-canonical name or contract", index)
		}
		if err := capability.Provider.Validate(); err != nil {
			return nil, fmt.Errorf("capability %s provider: %w", capability.Name, err)
		}
		if capability.Adapter != nil {
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

// NodeResolution is the exact live implementation and provider selection for
// one mounted graph node. CapabilitiesEvidence is empty until a live reporter
// replaces the initial declaration-only set.
type NodeResolution struct {
	Element              element.Identity     `json:"element"`
	Implementation       string               `json:"implementation"`
	Runtime              ArtifactIdentity     `json:"runtime"`
	RuntimeEvidence      ResolutionEvidence   `json:"runtime_evidence"`
	Capabilities         []CapabilityIdentity `json:"capabilities,omitempty"`
	CapabilitiesEvidence ResolutionEvidence   `json:"capabilities_evidence,omitempty"`
}

func (resolution NodeResolution) Clone() NodeResolution {
	result := resolution
	result.Capabilities = cloneCapabilities(resolution.Capabilities)
	return result
}

func (live NodeLive) Clone() NodeLive {
	result := live
	if live.Resolution != nil {
		copy := live.Resolution.Clone()
		result.Resolution = &copy
	}
	return result
}

// FlowLive is a bounded, payload-free view of internal graph edges traversed
// by one envelope correlation. Repeated edges remain repeated so feedback
// loops and retries are not flattened into a misleading set.
type FlowLive struct {
	Correlation string   `json:"correlation"`
	Edges       []string `json:"edges"`
	FirstNS     uint64   `json:"first_ns,omitempty"`
	LastNS      uint64   `json:"last_ns,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
}

func (flow FlowLive) Clone() FlowLive {
	flow.Edges = slices.Clone(flow.Edges)
	return flow
}

type EdgeLive struct {
	Occupancy    int    `json:"occupancy"`
	HighWater    int    `json:"high_water"`
	Enqueued     uint64 `json:"enqueued"`
	Dequeued     uint64 `json:"dequeued"`
	Dropped      uint64 `json:"dropped"`
	Backpressure uint64 `json:"backpressure"`
	LastItemID   string `json:"last_item_id,omitempty"`
	QueueWaitNS  uint64 `json:"queue_wait_ns,omitempty"`
}

func cloneCapabilities(source []CapabilityIdentity) []CapabilityIdentity {
	result := slices.Clone(source)
	for index := range result {
		if source[index].Adapter != nil {
			copy := *source[index].Adapter
			result[index].Adapter = &copy
		}
	}
	return result
}

// Build validates and projects one exact graph.
func Build(graph ir.Graph) (Model, error) {
	if err := graph.Validate(); err != nil {
		return Model{}, err
	}
	model := Model{
		GraphID: graph.ID, Revision: graph.Revision, Fingerprint: graph.Fingerprint,
		Nodes: make([]Node, 0, len(graph.Nodes)), Edges: make([]Edge, 0, len(graph.Edges)),
		Boundaries: make([]Boundary, 0, len(graph.Boundaries)),
		Scopes:     make([]Scope, 0, len(graph.Scopes)),
	}
	for _, source := range graph.Nodes {
		reaction := source.Reaction
		reaction.Triggers = slices.Clone(source.Reaction.Triggers)
		reaction.SampledState = slices.Clone(source.Reaction.SampledState)
		reaction.Interrupts = slices.Clone(source.Reaction.Interrupts)
		reaction.Outcomes = slices.Clone(source.Reaction.Outcomes)
		node := Node{
			ID: source.ID, Element: source.Element, Implementation: source.Implementation,
			ConfigReference: source.ConfigReference, ConfigDigest: source.ConfigDigest,
			DeploymentReference: source.DeploymentReference, DeploymentDigest: source.DeploymentDigest,
			Reaction:     reaction,
			Dependencies: append([]element.Dependency(nil), source.Dependencies...),
			Effects:      append([]element.Effect(nil), source.Effects...),
		}
		for _, sourcePort := range source.Ports {
			node.Ports = append(node.Ports, Port{
				Name: sourcePort.Name, Direction: sourcePort.Direction,
				Type: sourcePort.Type.String(), Cardinality: sourcePort.Cardinality,
				Lanes: append([]string(nil), sourcePort.Lanes...), Role: protocolRole(sourcePort.Type),
			})
		}
		model.Nodes = append(model.Nodes, node)
	}
	for _, source := range graph.Edges {
		model.Edges = append(model.Edges, Edge{
			ID: source.ID, From: source.From, To: source.To, Type: source.Type.String(),
			Role: protocolRole(source.Type), Delivery: source.Delivery, Depth: source.Depth,
		})
	}
	for _, source := range graph.Boundaries {
		model.Boundaries = append(model.Boundaries, Boundary{
			Name: source.Name, Direction: source.Direction, Endpoint: source.Endpoint,
			Type: source.Type.String(), Role: protocolRole(source.Type),
		})
	}
	for _, source := range graph.Scopes {
		scope := Scope{
			ID: source.ID, Parent: source.Parent, Composite: source.Composite,
			Nodes: append([]string(nil), source.Nodes...),
		}
		for _, boundary := range source.Boundaries {
			scope.Boundaries = append(scope.Boundaries, ScopeBoundary{
				Name: boundary.Name, Direction: boundary.Direction,
				Endpoint: boundary.Endpoint, Type: boundary.Type.String(),
			})
		}
		model.Scopes = append(model.Scopes, scope)
	}
	return model, nil
}

func protocolRole(value element.Type) string {
	switch value.Name {
	case "Trigger":
		return "trigger"
	case "Interrupt":
		return "interrupt"
	case "State":
		return "state"
	case "Request", "Reply":
		return "control"
	default:
		return "data"
	}
}
