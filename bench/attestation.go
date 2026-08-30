package bench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// AttestationFormatVersion is the current execution-evidence schema.
//
// Benchmark result versions and Graph IR versions have independent lifetimes.
// Keeping this version at the evidence boundary lets an old result remain
// inspectable without accidentally granting it the guarantees of a newer
// attestation contract.
const AttestationFormatVersion uint64 = 1

// ExecutionKind distinguishes a graph-native execution proof from an explicit
// compatibility-path proof. An empty kind is an historical, unattested cell;
// it is retained for old benchmark artifacts but can never satisfy a
// graph-native requirement.
type ExecutionKind string

const (
	ExecutionLegacy      ExecutionKind = "legacy"
	ExecutionGraphNative ExecutionKind = "graph-native"
)

// ArtifactIdentity names immutable configuration, code, model, or adapter
// material. ID is the human/runtime spelling. At least one immutable revision
// or digest is required; configuration identities require a digest.
type ArtifactIdentity struct {
	ID       string `json:"id"`
	Revision string `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

// GraphIdentity is the exact frozen Graph IR mounted for a session.
type GraphIdentity struct {
	FormatVersion uint64 `json:"format_version"`
	ID            string `json:"id"`
	Revision      uint64 `json:"revision"`
	Fingerprint   string `json:"fingerprint"`
}

// CapabilityIdentity is one provider capability selected by a live element.
// Provider can identify a model, service, device, or code artifact. Adapter is
// separate because changing an adapter while retaining a model is still a
// changed benchmark treatment.
type CapabilityIdentity struct {
	Name     string            `json:"name"`
	Contract string            `json:"contract,omitempty"`
	Provider ArtifactIdentity  `json:"provider"`
	Adapter  *ArtifactIdentity `json:"adapter,omitempty"`
}

// ElementResolution is the live resolution reported for one graph node. The
// selected descriptor and implementation are repeated deliberately: the
// attestor verifies them against immutable Graph IR before evidence is
// accepted, while Runtime identifies the mounted implementation artifact.
type ElementResolution struct {
	Node           string               `json:"node"`
	Element        element.Identity     `json:"element"`
	Implementation string               `json:"implementation"`
	Runtime        ArtifactIdentity     `json:"runtime"`
	Capabilities   []CapabilityIdentity `json:"capabilities,omitempty"`
}

// SelectedPath is the compact output of a live inspector or trace collector.
// Edge IDs are expanded and verified against Graph IR by GraphAttestor. One
// fork is represented as two named paths rather than as an ambiguous edge bag.
type SelectedPath struct {
	Name  string   `json:"name"`
	Edges []string `json:"edges"`
}

// LiveResolution is what the deployment-specific inspector learned after the
// session was mounted. Every element must be present. Paths are optional
// because not every graph makes a runtime route choice and not every current
// runtime exports route traces yet.
type LiveResolution struct {
	Deployment *inspect.DeploymentEvidence `json:"deployment,omitempty"`
	Elements   []ElementResolution         `json:"elements"`
	Paths      []SelectedPath              `json:"paths,omitempty"`
}

// EdgeEndpoint is a concrete graph port lane retained in a selected path.
type EdgeEndpoint struct {
	Node string `json:"node"`
	Port string `json:"port"`
	Lane string `json:"lane,omitempty"`
}

// PathEdge is the immutable edge identity behind a selected edge ID.
type PathEdge struct {
	ID       string       `json:"id"`
	From     EdgeEndpoint `json:"from"`
	To       EdgeEndpoint `json:"to"`
	Type     string       `json:"type"`
	Delivery string       `json:"delivery"`
	Ordering string       `json:"ordering"`
	Depth    int          `json:"depth"`
}

// GraphPath is one ordered route observed or required for a task.
type GraphPath struct {
	Name  string     `json:"name"`
	Edges []PathEdge `json:"edges"`
}

// GraphNodeEvidence combines immutable graph selection, per-node config
// identity, and the live runtime/capability resolution for one node.
type GraphNodeEvidence struct {
	Node           string               `json:"node"`
	Element        element.Identity     `json:"element"`
	Implementation string               `json:"implementation"`
	Config         ArtifactIdentity     `json:"config"`
	Runtime        ArtifactIdentity     `json:"runtime"`
	Capabilities   []CapabilityIdentity `json:"capabilities,omitempty"`
}

// GraphEvidence is a self-contained, human-inspectable execution proof. The
// graph fingerprint remains the authority; the expanded nodes, config
// digests, resolutions, and paths make that authority reviewable without
// guessing what a compact fingerprint contained.
type GraphEvidence struct {
	Graph         GraphIdentity               `json:"graph"`
	Configuration ArtifactIdentity            `json:"configuration"`
	Deployment    *inspect.DeploymentEvidence `json:"deployment,omitempty"`
	Nodes         []GraphNodeEvidence         `json:"nodes"`
	Paths         []GraphPath                 `json:"selected_paths,omitempty"`
}

// LegacyEvidence identifies a compatibility execution without claiming it is
// graph-native. RuntimeDigest protects the complete status snapshot used to
// make the record; its older, less formal fields remain intentionally distinct
// from GraphEvidence.
type LegacyEvidence struct {
	Binding       string                       `json:"binding"`
	Architecture  binding.ArchitectureIdentity `json:"architecture,omitempty"`
	RuntimeDigest string                       `json:"runtime_digest"`
}

// ExecutionEvidence is one immutable, fingerprinted live-session proof.
type ExecutionEvidence struct {
	FormatVersion uint64        `json:"format_version"`
	Kind          ExecutionKind `json:"kind"`
	// Scope is the benchmark task/session identity supplied to the attestor.
	// It is optional for independently captured cell-wide evidence (such as an
	// external tau-Voice preflight), and otherwise binds selected paths to the
	// row whose execution produced them.
	Scope       string          `json:"scope,omitempty"`
	Fingerprint string          `json:"fingerprint"`
	Graph       *GraphEvidence  `json:"graph,omitempty"`
	Legacy      *LegacyEvidence `json:"legacy,omitempty"`
}

// LegacyRequirement describes an explicitly legacy benchmark cell. A legacy
// requirement can constrain an architecture identity when that catalog was in
// use, but it never accepts graph evidence as a substitute.
type LegacyRequirement struct {
	Binding      string                       `json:"binding"`
	Architecture binding.ArchitectureIdentity `json:"architecture,omitempty"`
}

// ExecutionRequirement is the immutable execution contract carried by a
// benchmark cell. For graph-native cells, Paths are required selections while
// extra observed paths remain valid task evidence.
type ExecutionRequirement struct {
	FormatVersion uint64             `json:"format_version,omitempty"`
	Kind          ExecutionKind      `json:"kind,omitempty"`
	Graph         *GraphEvidence     `json:"graph,omitempty"`
	Legacy        *LegacyRequirement `json:"legacy,omitempty"`
}

// AttestationRequest binds the live status and server-issued inspection
// capability to the benchmark task. Scope labels resulting evidence and route
// attribution; it is never authority for selecting a mounted session.
type AttestationRequest struct {
	Scope      string
	Status     binding.Status
	Inspection *openrealtime.InspectionAccess
}

func (requirement ExecutionRequirement) canonicalized() ExecutionRequirement {
	result := requirement
	if requirement.Graph != nil {
		copy := requirement.Graph.clone()
		copy.canonicalize()
		result.Graph = &copy
	}
	if requirement.Legacy != nil {
		copy := *requirement.Legacy
		result.Legacy = &copy
	}
	return result
}

// RuntimeAttestor turns a post-handshake live status into versioned execution
// evidence. Implementations may query an inspector, trace store, or local
// management plane; intended launch flags are not a valid substitute.
type RuntimeAttestor interface {
	Attest(context.Context, AttestationRequest) (ExecutionEvidence, error)
}

// RuntimeAttestorFunc adapts a function to RuntimeAttestor.
type RuntimeAttestorFunc func(context.Context, AttestationRequest) (ExecutionEvidence, error)

func (function RuntimeAttestorFunc) Attest(
	ctx context.Context, request AttestationRequest,
) (ExecutionEvidence, error) {
	if function == nil {
		return ExecutionEvidence{}, errors.New("runtime attestor function is nil")
	}
	return function(ctx, request)
}

// GraphAttestor verifies the live graph identity and combines the immutable IR
// with a deployment-backed resolution callback. The shared driver invokes it
// at the task completion boundary, so Resolve can observe routes selected
// during execution. Resolve is intentionally required: binding.Status alone
// proves the mounted graph fingerprint but does not expose exact dynamic
// provider revisions or selected routes.
type GraphAttestor struct {
	Graph         ir.Graph
	Configuration ArtifactIdentity
	Resolve       func(context.Context, AttestationRequest) (LiveResolution, error)
}

// Attest captures one graph-native execution proof.
func (attestor GraphAttestor) Attest(
	ctx context.Context, request AttestationRequest,
) (ExecutionEvidence, error) {
	if ctx == nil {
		return ExecutionEvidence{}, errors.New("attest graph execution: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return ExecutionEvidence{}, err
	}
	if request.Scope != "" && !canonical(request.Scope) {
		return ExecutionEvidence{}, errors.New("attest graph execution: scope is not canonical")
	}
	if err := attestor.Graph.Validate(); err != nil {
		return ExecutionEvidence{}, fmt.Errorf("attest graph execution: %w", err)
	}
	if err := attestor.Configuration.validateExact("configuration artifact"); err != nil {
		return ExecutionEvidence{}, fmt.Errorf("attest graph execution: %w", err)
	}
	if attestor.Configuration.Digest == "" {
		return ExecutionEvidence{}, errors.New("attest graph execution: configuration artifact requires a digest")
	}
	identity := graphIdentity(attestor.Graph)
	if err := matchLiveGraph(identity, request.Status.Graph); err != nil {
		return ExecutionEvidence{}, fmt.Errorf("attest graph execution: %w", err)
	}
	if attestor.Resolve == nil {
		return ExecutionEvidence{}, errors.New(
			"attest graph execution: a live element/capability resolver is required",
		)
	}
	resolution, err := attestor.Resolve(ctx, request)
	if err != nil {
		return ExecutionEvidence{}, fmt.Errorf("attest graph execution: resolve live graph: %w", err)
	}
	graph, err := buildGraphEvidence(attestor.Graph, attestor.Configuration, resolution)
	if err != nil {
		return ExecutionEvidence{}, fmt.Errorf("attest graph execution: %w", err)
	}
	if len(graph.Paths) > 0 && request.Scope == "" {
		return ExecutionEvidence{}, errors.New(
			"attest graph execution: selected routes require a task/session scope",
		)
	}
	return FreezeExecutionEvidence(ExecutionEvidence{
		FormatVersion: AttestationFormatVersion,
		Kind:          ExecutionGraphNative,
		Scope:         request.Scope,
		Graph:         &graph,
	})
}

// LegacyStatusAttestor records the complete legacy status digest and refuses a
// runtime that reports a mounted graph. This prevents a compatibility helper
// from silently relabeling graph evidence as legacy (or vice versa).
type LegacyStatusAttestor struct{}

func (LegacyStatusAttestor) Attest(
	ctx context.Context, request AttestationRequest,
) (ExecutionEvidence, error) {
	if ctx == nil {
		return ExecutionEvidence{}, errors.New("attest legacy execution: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return ExecutionEvidence{}, err
	}
	status := request.Status
	if !status.Graph.Empty() {
		return ExecutionEvidence{}, fmt.Errorf(
			"attest legacy execution: runtime mounted graph %s@%d; graph evidence requires GraphAttestor",
			status.Graph.ID, status.Graph.Revision,
		)
	}
	if strings.TrimSpace(status.Binding) == "" {
		return ExecutionEvidence{}, errors.New("attest legacy execution: live status has no binding")
	}
	digest, err := digestJSON(status)
	if err != nil {
		return ExecutionEvidence{}, fmt.Errorf("attest legacy execution: %w", err)
	}
	legacy := LegacyEvidence{
		Binding: status.Binding, Architecture: status.Architecture, RuntimeDigest: digest,
	}
	return FreezeExecutionEvidence(ExecutionEvidence{
		FormatVersion: AttestationFormatVersion,
		Kind:          ExecutionLegacy,
		Scope:         request.Scope,
		Legacy:        &legacy,
	})
}

// RequireGraph constructs a canonical graph-native cell requirement. Expected
// contains the exact live element/capability identities the deployment is
// meant to resolve and any route selections every task must report.
func RequireGraph(
	graph ir.Graph, configuration ArtifactIdentity, expected LiveResolution,
) (ExecutionRequirement, error) {
	evidence, err := buildGraphEvidence(graph, configuration, expected)
	if err != nil {
		return ExecutionRequirement{}, fmt.Errorf("build graph execution requirement: %w", err)
	}
	evidence.canonicalize()
	return ExecutionRequirement{
		FormatVersion: AttestationFormatVersion,
		Kind:          ExecutionGraphNative,
		Graph:         &evidence,
	}, nil
}

// RequireLegacy constructs an explicit compatibility-cell requirement.
func RequireLegacy(
	bindingName string, architecture binding.ArchitectureIdentity,
) (ExecutionRequirement, error) {
	requirement := ExecutionRequirement{
		FormatVersion: AttestationFormatVersion,
		Kind:          ExecutionLegacy,
		Legacy: &LegacyRequirement{
			Binding: bindingName, Architecture: architecture,
		},
	}
	if err := requirement.Validate(); err != nil {
		return ExecutionRequirement{}, err
	}
	return requirement, nil
}

// Required reports whether this cell demands per-task execution evidence.
func (requirement ExecutionRequirement) Required() bool { return requirement.Kind != "" }

// IsZero lets result and manifest JSON omit the unattested historical default.
// This preserves existing artifact and manifest fingerprints while still
// serializing an explicit legacy or graph-native contract.
func (requirement ExecutionRequirement) IsZero() bool {
	return requirement.FormatVersion == 0 && requirement.Kind == "" &&
		requirement.Graph == nil && requirement.Legacy == nil
}

// Validate checks an execution requirement independently of any result.
func (requirement ExecutionRequirement) Validate() error {
	switch requirement.Kind {
	case "":
		if requirement.FormatVersion != 0 || requirement.Graph != nil || requirement.Legacy != nil {
			return errors.New("an unattested execution requirement cannot carry evidence fields")
		}
		return nil
	case ExecutionGraphNative:
		if requirement.FormatVersion != AttestationFormatVersion {
			return fmt.Errorf("execution requirement format must be %d", AttestationFormatVersion)
		}
		if requirement.Graph == nil || requirement.Legacy != nil {
			return errors.New("a graph-native execution requirement needs graph evidence only")
		}
		copy := requirement.Graph.clone()
		copy.canonicalize()
		return copy.validate()
	case ExecutionLegacy:
		if requirement.FormatVersion != AttestationFormatVersion {
			return fmt.Errorf("execution requirement format must be %d", AttestationFormatVersion)
		}
		if requirement.Legacy == nil || requirement.Graph != nil {
			return errors.New("a legacy execution requirement needs legacy identity only")
		}
		if !canonical(requirement.Legacy.Binding) {
			return errors.New("legacy execution requirement needs a canonical binding name")
		}
		return validateArchitectureIdentity("legacy architecture", requirement.Legacy.Architecture, true)
	default:
		return fmt.Errorf("unknown execution requirement kind %q", requirement.Kind)
	}
}

// MatchStatus verifies the status fields available at handshake time. A graph
// result still needs full ExecutionEvidence; this check only rejects a wrong
// endpoint early.
func (requirement ExecutionRequirement) MatchStatus(status binding.Status) error {
	if err := requirement.Validate(); err != nil {
		return err
	}
	switch requirement.Kind {
	case "":
		return nil
	case ExecutionGraphNative:
		return matchLiveGraph(requirement.Graph.Graph, status.Graph)
	case ExecutionLegacy:
		if !status.Graph.Empty() {
			return fmt.Errorf("expected legacy execution, runtime mounted graph %s@%d",
				status.Graph.ID, status.Graph.Revision)
		}
		if status.Binding != requirement.Legacy.Binding {
			return fmt.Errorf("legacy binding is %q, want %q", status.Binding, requirement.Legacy.Binding)
		}
		if !requirement.Legacy.Architecture.Empty() &&
			status.Architecture != requirement.Legacy.Architecture {
			return errors.New("legacy architecture identity differs")
		}
		return nil
	default:
		panic("validated execution requirement has an unknown kind")
	}
}

// Match verifies one task's evidence against its cell requirement.
func (requirement ExecutionRequirement) Match(evidence *ExecutionEvidence) error {
	if err := requirement.Validate(); err != nil {
		return fmt.Errorf("invalid cell execution requirement: %w", err)
	}
	if evidence == nil {
		if requirement.Required() {
			return fmt.Errorf("%s execution evidence is missing", requirement.Kind)
		}
		return nil
	}
	if err := evidence.Validate(); err != nil {
		return fmt.Errorf("invalid execution evidence: %w", err)
	}
	if !requirement.Required() {
		// Valid evidence attached to an historical cell remains useful, but it
		// does not retroactively turn that cell into a graph-native experiment.
		return nil
	}
	if evidence.Kind != requirement.Kind {
		return fmt.Errorf("execution evidence kind is %q, want %q", evidence.Kind, requirement.Kind)
	}
	switch requirement.Kind {
	case ExecutionLegacy:
		if evidence.Legacy.Binding != requirement.Legacy.Binding {
			return fmt.Errorf("legacy binding is %q, want %q",
				evidence.Legacy.Binding, requirement.Legacy.Binding)
		}
		if !requirement.Legacy.Architecture.Empty() &&
			evidence.Legacy.Architecture != requirement.Legacy.Architecture {
			return errors.New("legacy architecture identity differs")
		}
		return nil
	case ExecutionGraphNative:
		expected := requirement.Graph.clone()
		expected.canonicalize()
		observed := evidence.Graph.clone()
		observed.canonicalize()
		if expected.Graph != observed.Graph {
			return fmt.Errorf("graph identity is %+v, want %+v", observed.Graph, expected.Graph)
		}
		if expected.Configuration != observed.Configuration {
			return errors.New("configuration artifact identity or digest differs")
		}
		if !reflect.DeepEqual(expected.Deployment, observed.Deployment) {
			return errors.New("deployment identity, secret catalog, or provider evidence differs")
		}
		if !reflect.DeepEqual(expected.Nodes, observed.Nodes) {
			return errors.New("live element, config, or capability resolutions differ")
		}
		observedPaths := make(map[string]GraphPath, len(observed.Paths))
		for _, path := range observed.Paths {
			observedPaths[path.Name] = path
		}
		for _, path := range expected.Paths {
			actual, found := observedPaths[path.Name]
			if !found {
				return fmt.Errorf("required selected path %q is missing", path.Name)
			}
			if !reflect.DeepEqual(actual, path) {
				return fmt.Errorf("selected path %q uses different graph edges", path.Name)
			}
		}
		return nil
	default:
		panic("validated execution requirement has an unknown kind")
	}
}

// FreezeExecutionEvidence clones, canonicalizes, validates, and fingerprints
// evidence. The input and all of its slices remain owned by the caller.
func FreezeExecutionEvidence(evidence ExecutionEvidence) (ExecutionEvidence, error) {
	canonicalEvidence := evidence.Clone()
	canonicalEvidence.Fingerprint = ""
	canonicalEvidence.canonicalize()
	if err := canonicalEvidence.validateStructure(); err != nil {
		return ExecutionEvidence{}, err
	}
	fingerprint, err := canonicalEvidence.computeFingerprint()
	if err != nil {
		return ExecutionEvidence{}, err
	}
	canonicalEvidence.Fingerprint = fingerprint
	return canonicalEvidence, nil
}

// Validate verifies structure and the stored evidence fingerprint.
func (evidence ExecutionEvidence) Validate() error {
	canonicalEvidence := evidence.Clone()
	canonicalEvidence.canonicalize()
	if err := canonicalEvidence.validateStructure(); err != nil {
		return err
	}
	want, err := canonicalEvidence.computeFingerprint()
	if err != nil {
		return err
	}
	if evidence.Fingerprint != want {
		return fmt.Errorf("execution evidence fingerprint is %q, want %q", evidence.Fingerprint, want)
	}
	return nil
}

// Clone returns evidence with recursively independent slices and pointers.
func (evidence ExecutionEvidence) Clone() ExecutionEvidence {
	result := evidence
	if evidence.Graph != nil {
		copy := evidence.Graph.clone()
		result.Graph = &copy
	}
	if evidence.Legacy != nil {
		copy := *evidence.Legacy
		result.Legacy = &copy
	}
	return result
}

// MarshalExecutionEvidence returns deterministic, newline-terminated JSON.
func MarshalExecutionEvidence(evidence ExecutionEvidence) ([]byte, error) {
	if err := evidence.Validate(); err != nil {
		return nil, err
	}
	canonicalEvidence := evidence.Clone()
	canonicalEvidence.canonicalize()
	payload, err := json.MarshalIndent(canonicalEvidence, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode execution evidence: %w", err)
	}
	return append(payload, '\n'), nil
}

// ParseExecutionEvidence strictly decodes and verifies one evidence artifact.
func ParseExecutionEvidence(source []byte) (ExecutionEvidence, error) {
	if err := strictjson.Validate(source); err != nil {
		return ExecutionEvidence{}, fmt.Errorf("decode execution evidence: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var evidence ExecutionEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return ExecutionEvidence{}, fmt.Errorf("decode execution evidence: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return ExecutionEvidence{}, errors.New("decode execution evidence: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return ExecutionEvidence{}, fmt.Errorf("decode execution evidence trailing data: %w", err)
	}
	if err := evidence.Validate(); err != nil {
		return ExecutionEvidence{}, err
	}
	evidence.canonicalize()
	return evidence, nil
}

func buildGraphEvidence(
	graph ir.Graph, configuration ArtifactIdentity, resolution LiveResolution,
) (GraphEvidence, error) {
	if err := graph.Validate(); err != nil {
		return GraphEvidence{}, err
	}
	if err := configuration.validateExact("configuration artifact"); err != nil {
		return GraphEvidence{}, err
	}
	if configuration.Digest == "" {
		return GraphEvidence{}, errors.New("configuration artifact requires a digest")
	}

	resolved := make(map[string]ElementResolution, len(resolution.Elements))
	for _, item := range resolution.Elements {
		if !canonical(item.Node) {
			return GraphEvidence{}, fmt.Errorf("live resolution has invalid node %q", item.Node)
		}
		if _, duplicate := resolved[item.Node]; duplicate {
			return GraphEvidence{}, fmt.Errorf("live resolution repeats node %q", item.Node)
		}
		resolved[item.Node] = cloneElementResolution(item)
	}

	result := GraphEvidence{
		Graph: graphIdentity(graph), Configuration: configuration,
		Nodes: make([]GraphNodeEvidence, 0, len(graph.Nodes)),
	}
	if resolution.Deployment != nil {
		canonical, err := inspect.CanonicalDeploymentEvidence(*resolution.Deployment)
		if err != nil {
			return GraphEvidence{}, fmt.Errorf("live deployment evidence: %w", err)
		}
		if err := canonical.ValidateExact(); err != nil {
			return GraphEvidence{}, fmt.Errorf("live deployment evidence: %w", err)
		}
		result.Deployment = &canonical
	}
	knownNodes := make(map[string]struct{}, len(graph.Nodes))
	for _, node := range graph.Nodes {
		knownNodes[node.ID] = struct{}{}
		live, found := resolved[node.ID]
		if !found {
			return GraphEvidence{}, fmt.Errorf("live resolution is missing graph node %q", node.ID)
		}
		if live.Element != node.Element {
			return GraphEvidence{}, fmt.Errorf("node %s resolved element %+v, Graph IR selects %+v",
				node.ID, live.Element, node.Element)
		}
		if live.Implementation != node.Implementation {
			return GraphEvidence{}, fmt.Errorf("node %s resolved implementation %q, Graph IR selects %q",
				node.ID, live.Implementation, node.Implementation)
		}
		config := ArtifactIdentity{ID: node.ConfigReference, Digest: node.ConfigDigest}
		if err := config.validateExact("node " + node.ID + " config"); err != nil {
			return GraphEvidence{}, fmt.Errorf(
				"Graph IR must be bound to exact separate values before benchmarking: %w", err,
			)
		}
		result.Nodes = append(result.Nodes, GraphNodeEvidence{
			Node: node.ID, Element: node.Element, Implementation: node.Implementation,
			Config: config, Runtime: live.Runtime,
			Capabilities: cloneCapabilities(live.Capabilities),
		})
	}
	for node := range resolved {
		if _, found := knownNodes[node]; !found {
			return GraphEvidence{}, fmt.Errorf("live resolution refers to unknown graph node %q", node)
		}
	}

	edges := make(map[string]ir.Edge, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges[edge.ID] = edge
	}
	pathNames := make(map[string]struct{}, len(resolution.Paths))
	for _, selected := range resolution.Paths {
		if !canonical(selected.Name) {
			return GraphEvidence{}, fmt.Errorf("selected path has invalid name %q", selected.Name)
		}
		if _, duplicate := pathNames[selected.Name]; duplicate {
			return GraphEvidence{}, fmt.Errorf("selected path %q is repeated", selected.Name)
		}
		pathNames[selected.Name] = struct{}{}
		if len(selected.Edges) == 0 {
			return GraphEvidence{}, fmt.Errorf("selected path %q has no edges", selected.Name)
		}
		path := GraphPath{Name: selected.Name, Edges: make([]PathEdge, 0, len(selected.Edges))}
		seenEdges := make(map[string]struct{}, len(selected.Edges))
		var previous *ir.Edge
		for _, edgeID := range selected.Edges {
			edge, found := edges[edgeID]
			if !found {
				return GraphEvidence{}, fmt.Errorf("selected path %q refers to unknown edge %q",
					selected.Name, edgeID)
			}
			if _, duplicate := seenEdges[edgeID]; duplicate {
				return GraphEvidence{}, fmt.Errorf("selected path %q repeats edge %q", selected.Name, edgeID)
			}
			seenEdges[edgeID] = struct{}{}
			if previous != nil && previous.To.Node != edge.From.Node {
				return GraphEvidence{}, fmt.Errorf(
					"selected path %q is discontinuous between edges %q and %q",
					selected.Name, previous.ID, edge.ID,
				)
			}
			copy := edge
			previous = &copy
			path.Edges = append(path.Edges, pathEdge(edge))
		}
		result.Paths = append(result.Paths, path)
	}
	result.canonicalize()
	if err := result.validate(); err != nil {
		return GraphEvidence{}, err
	}
	return result, nil
}

func (evidence *ExecutionEvidence) canonicalize() {
	if evidence.Graph != nil {
		evidence.Graph.canonicalize()
	}
}

func (evidence ExecutionEvidence) validateStructure() error {
	if evidence.FormatVersion != AttestationFormatVersion {
		return fmt.Errorf("execution evidence format must be %d", AttestationFormatVersion)
	}
	if evidence.Scope != "" && !canonical(evidence.Scope) {
		return errors.New("execution evidence scope is not canonical")
	}
	switch evidence.Kind {
	case ExecutionGraphNative:
		if evidence.Graph == nil || evidence.Legacy != nil {
			return errors.New("graph-native execution evidence needs graph evidence only")
		}
		return evidence.Graph.validate()
	case ExecutionLegacy:
		if evidence.Legacy == nil || evidence.Graph != nil {
			return errors.New("legacy execution evidence needs legacy evidence only")
		}
		if !canonical(evidence.Legacy.Binding) {
			return errors.New("legacy execution evidence needs a canonical binding name")
		}
		if err := validateArchitectureIdentity("legacy architecture", evidence.Legacy.Architecture, true); err != nil {
			return err
		}
		return validateSHA256("legacy runtime digest", evidence.Legacy.RuntimeDigest)
	default:
		return fmt.Errorf("unknown execution evidence kind %q", evidence.Kind)
	}
}

func (evidence ExecutionEvidence) computeFingerprint() (string, error) {
	semantic := evidence.Clone()
	semantic.Fingerprint = ""
	semantic.canonicalize()
	return digestJSON(semantic)
}

func (graph *GraphEvidence) canonicalize() {
	if graph.Deployment != nil {
		canonical, err := inspect.CanonicalDeploymentEvidence(*graph.Deployment)
		if err == nil {
			graph.Deployment = &canonical
		}
	}
	sort.Slice(graph.Nodes, func(left, right int) bool {
		return graph.Nodes[left].Node < graph.Nodes[right].Node
	})
	for index := range graph.Nodes {
		sort.Slice(graph.Nodes[index].Capabilities, func(left, right int) bool {
			a, b := graph.Nodes[index].Capabilities[left], graph.Nodes[index].Capabilities[right]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			if a.Contract != b.Contract {
				return a.Contract < b.Contract
			}
			return a.Provider.ID < b.Provider.ID
		})
	}
	sort.Slice(graph.Paths, func(left, right int) bool {
		return graph.Paths[left].Name < graph.Paths[right].Name
	})
}

func (graph GraphEvidence) validate() error {
	if graph.Graph.FormatVersion != ir.FormatVersion {
		return fmt.Errorf("attested Graph IR format is %d, want %d",
			graph.Graph.FormatVersion, ir.FormatVersion)
	}
	if !canonical(graph.Graph.ID) || graph.Graph.Revision == 0 {
		return errors.New("attested graph requires a canonical ID and positive revision")
	}
	if err := validateSHA256("graph fingerprint", graph.Graph.Fingerprint); err != nil {
		return err
	}
	if err := graph.Configuration.validateExact("configuration artifact"); err != nil {
		return err
	}
	if graph.Configuration.Digest == "" {
		return errors.New("configuration artifact requires a digest")
	}
	if graph.Deployment != nil {
		if err := graph.Deployment.ValidateExact(); err != nil {
			return fmt.Errorf("deployment evidence: %w", err)
		}
	}
	if len(graph.Nodes) == 0 {
		return errors.New("graph execution evidence has no nodes")
	}
	nodes := make(map[string]struct{}, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if !canonical(node.Node) {
			return fmt.Errorf("graph evidence has invalid node %q", node.Node)
		}
		if _, duplicate := nodes[node.Node]; duplicate {
			return fmt.Errorf("graph evidence repeats node %q", node.Node)
		}
		nodes[node.Node] = struct{}{}
		if !canonical(node.Element.Name) || node.Element.Revision == 0 {
			return fmt.Errorf("node %s has an invalid element identity", node.Node)
		}
		if err := validateSHA256("node "+node.Node+" element digest", node.Element.Digest); err != nil {
			return err
		}
		if !canonical(node.Implementation) {
			return fmt.Errorf("node %s has no canonical implementation identity", node.Node)
		}
		if livePlaceholder(node.Implementation) {
			return fmt.Errorf("node %s implementation %q is a live placeholder, not an exact implementation identity",
				node.Node, node.Implementation)
		}
		if err := node.Config.validateExact("node " + node.Node + " config"); err != nil {
			return err
		}
		if node.Config.Digest == "" {
			return fmt.Errorf("node %s config requires a digest", node.Node)
		}
		if err := node.Runtime.validateExact("node " + node.Node + " runtime"); err != nil {
			return err
		}
		capabilities := make(map[string]struct{}, len(node.Capabilities))
		for _, capability := range node.Capabilities {
			if !canonical(capability.Name) ||
				(capability.Contract != "" && !canonical(capability.Contract)) {
				return fmt.Errorf("node %s has an invalid capability identity", node.Node)
			}
			key := capability.Name + "\x00" + capability.Contract + "\x00" + capability.Provider.ID
			if _, duplicate := capabilities[key]; duplicate {
				return fmt.Errorf("node %s repeats capability %q", node.Node, capability.Name)
			}
			capabilities[key] = struct{}{}
			if err := capability.Provider.validateExact(
				"node " + node.Node + " capability " + capability.Name + " provider",
			); err != nil {
				return err
			}
			if capability.Adapter != nil {
				if err := capability.Adapter.validateExact(
					"node " + node.Node + " capability " + capability.Name + " adapter",
				); err != nil {
					return err
				}
			}
		}
	}
	paths := make(map[string]struct{}, len(graph.Paths))
	for _, path := range graph.Paths {
		if !canonical(path.Name) || len(path.Edges) == 0 {
			return fmt.Errorf("graph evidence has an invalid selected path %q", path.Name)
		}
		if _, duplicate := paths[path.Name]; duplicate {
			return fmt.Errorf("graph evidence repeats selected path %q", path.Name)
		}
		paths[path.Name] = struct{}{}
		edges := make(map[string]struct{}, len(path.Edges))
		for index, edge := range path.Edges {
			if !canonical(edge.ID) || !canonical(edge.From.Node) || !canonical(edge.From.Port) ||
				!canonical(edge.To.Node) || !canonical(edge.To.Port) || !canonical(edge.Type) ||
				!canonical(edge.Delivery) || !canonical(edge.Ordering) || edge.Depth <= 0 {
				return fmt.Errorf("selected path %q contains an invalid edge", path.Name)
			}
			if edge.From.Lane != "" && !canonical(edge.From.Lane) ||
				edge.To.Lane != "" && !canonical(edge.To.Lane) {
				return fmt.Errorf("selected path %q contains an invalid edge lane", path.Name)
			}
			if edge.Delivery != string(ir.Lossless) && edge.Delivery != string(ir.Lossy) {
				return fmt.Errorf("selected path %q edge %q has invalid delivery %q",
					path.Name, edge.ID, edge.Delivery)
			}
			if edge.Ordering != "fifo" {
				return fmt.Errorf("selected path %q edge %q has invalid ordering %q",
					path.Name, edge.ID, edge.Ordering)
			}
			if _, found := nodes[edge.From.Node]; !found {
				return fmt.Errorf("selected path %q starts edge %q at unknown node %q",
					path.Name, edge.ID, edge.From.Node)
			}
			if _, found := nodes[edge.To.Node]; !found {
				return fmt.Errorf("selected path %q ends edge %q at unknown node %q",
					path.Name, edge.ID, edge.To.Node)
			}
			if _, duplicate := edges[edge.ID]; duplicate {
				return fmt.Errorf("selected path %q repeats edge %q", path.Name, edge.ID)
			}
			edges[edge.ID] = struct{}{}
			if index > 0 && path.Edges[index-1].To.Node != edge.From.Node {
				return fmt.Errorf("selected path %q is discontinuous between edges %q and %q",
					path.Name, path.Edges[index-1].ID, edge.ID)
			}
		}
	}
	return nil
}

func (graph GraphEvidence) clone() GraphEvidence {
	result := graph
	if graph.Deployment != nil {
		copy := graph.Deployment.Clone()
		result.Deployment = &copy
	}
	result.Nodes = slices.Clone(graph.Nodes)
	for index := range result.Nodes {
		result.Nodes[index].Capabilities = cloneCapabilities(result.Nodes[index].Capabilities)
	}
	result.Paths = slices.Clone(graph.Paths)
	for index := range result.Paths {
		result.Paths[index].Edges = slices.Clone(result.Paths[index].Edges)
	}
	return result
}

func cloneElementResolution(value ElementResolution) ElementResolution {
	result := value
	result.Capabilities = cloneCapabilities(value.Capabilities)
	return result
}

func cloneCapabilities(values []CapabilityIdentity) []CapabilityIdentity {
	result := slices.Clone(values)
	for index := range result {
		if result[index].Adapter != nil {
			copy := *result[index].Adapter
			result[index].Adapter = &copy
		}
	}
	return result
}

func graphIdentity(graph ir.Graph) GraphIdentity {
	return GraphIdentity{
		FormatVersion: graph.FormatVersion, ID: graph.ID,
		Revision: graph.Revision, Fingerprint: graph.Fingerprint,
	}
}

func matchLiveGraph(expected GraphIdentity, actual binding.ArchitectureIdentity) error {
	if actual.Empty() {
		return errors.New("runtime did not report a mounted graph")
	}
	if actual.Revision <= 0 {
		return fmt.Errorf("runtime graph %q has invalid revision %d", actual.ID, actual.Revision)
	}
	if actual.ID != expected.ID || uint64(actual.Revision) != expected.Revision ||
		actual.Fingerprint != expected.Fingerprint {
		return fmt.Errorf("live graph is %s@%d (%s), want %s@%d (%s)",
			actual.ID, actual.Revision, actual.Fingerprint,
			expected.ID, expected.Revision, expected.Fingerprint)
	}
	return nil
}

func pathEdge(edge ir.Edge) PathEdge {
	return PathEdge{
		ID:   edge.ID,
		From: EdgeEndpoint{Node: edge.From.Node, Port: edge.From.Port, Lane: edge.From.Lane},
		To:   EdgeEndpoint{Node: edge.To.Node, Port: edge.To.Port, Lane: edge.To.Lane},
		Type: edge.Type.String(), Delivery: string(edge.Delivery),
		Ordering: edge.Ordering, Depth: edge.Depth,
	}
}

func (identity ArtifactIdentity) validateExact(label string) error {
	if !canonical(identity.ID) {
		return fmt.Errorf("%s requires a canonical ID", label)
	}
	if livePlaceholder(identity.ID) {
		return fmt.Errorf("%s ID %q is a live placeholder, not an exact artifact ID", label, identity.ID)
	}
	if identity.Revision != "" && !canonical(identity.Revision) {
		return fmt.Errorf("%s has a non-canonical revision", label)
	}
	if livePlaceholder(identity.Revision) {
		return fmt.Errorf("%s revision %q is a live placeholder, not an immutable revision", label, identity.Revision)
	}
	if identity.Revision == "" && identity.Digest == "" {
		return fmt.Errorf("%s requires an immutable revision or digest", label)
	}
	if identity.Digest != "" {
		return validateSHA256(label+" digest", identity.Digest)
	}
	return nil
}

func livePlaceholder(value string) bool {
	if value == "" {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, placeholder := range []string{
		"latest", "current", "unknown", "unresolved", "dynamic", "live",
		"head", "main", "master", "dev", "development", "snapshot", "nightly",
		"pending", "placeholder", "todo", "tbd",
	} {
		if normalized == placeholder || strings.HasSuffix(normalized, ":"+placeholder) ||
			strings.HasSuffix(normalized, "@"+placeholder) ||
			strings.HasSuffix(normalized, "/"+placeholder) {
			return true
		}
	}
	return strings.ContainsAny(normalized, "${}<>*")
}

func validateArchitectureIdentity(
	label string, identity binding.ArchitectureIdentity, optional bool,
) error {
	if identity.Empty() {
		if optional {
			return nil
		}
		return fmt.Errorf("%s is missing", label)
	}
	if !canonical(identity.ID) || identity.Revision <= 0 {
		return fmt.Errorf("%s requires a canonical ID and positive revision", label)
	}
	return validateSHA256(label+" fingerprint", identity.Fingerprint)
}

func validateSHA256(label, value string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return fmt.Errorf("%s is not a canonical SHA-256 digest", label)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, prefix)); err != nil {
		return fmt.Errorf("%s is not a canonical SHA-256 digest: %w", label, err)
	}
	if strings.ToLower(value) != value {
		return fmt.Errorf("%s is not lowercase canonical SHA-256", label)
	}
	return nil
}

func digestJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func canonical(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}
