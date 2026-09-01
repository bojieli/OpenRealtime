package management

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// CanonicalSessionID reports whether value is safe as one opaque management
// resource segment. It deliberately rejects separators instead of trying to
// assign filesystem or URL semantics to a session identifier.
func CanonicalSessionID(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 256 &&
		!strings.ContainsAny(value, "/\\\x00\r\n")
}

// CanonicalDigest reports whether value is a lowercase, canonical SHA-256
// content identity.
func CanonicalDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

// ValidateSessionSnapshot checks the identity-bearing evidence required at a
// public management boundary. Free-form fields are permitted here because a
// provider may validate before redaction; callers must still apply RedactLive
// before returning a snapshot to an inspector.
func ValidateSessionSnapshot(snapshot inspect.Live) error {
	if snapshot.FormatVersion != inspect.LiveFormatVersion || snapshot.GraphID == "" ||
		snapshot.GraphRevision == 0 || !CanonicalDigest(snapshot.Fingerprint) || snapshot.Configuration == nil {
		return fmt.Errorf("%w: session source returned incomplete live evidence", ErrConflict)
	}
	if err := snapshot.Configuration.Validate(); err != nil || snapshot.Configuration.Digest == "" {
		return fmt.Errorf("%w: session source returned invalid configuration evidence", ErrConflict)
	}
	if snapshot.Adapter != nil {
		if err := snapshot.Adapter.Validate(); err != nil {
			return fmt.Errorf("%w: session source returned invalid adapter resolution", ErrConflict)
		}
	}
	if snapshot.Deployment != nil {
		if err := snapshot.Deployment.Validate(); err != nil {
			return fmt.Errorf("%w: session source returned invalid deployment evidence", ErrConflict)
		}
	}
	if len(snapshot.Nodes) == 0 {
		return fmt.Errorf("%w: session source returned no resolved nodes", ErrConflict)
	}
	for id, node := range snapshot.Nodes {
		if !canonicalEvidenceName(id) || node.Resolution == nil {
			return fmt.Errorf("%w: session source returned an unresolved node", ErrConflict)
		}
		if node.ActiveRuns < 0 || node.ActiveRuns > 65_536 {
			return fmt.Errorf("%w: session source returned invalid active-run telemetry", ErrConflict)
		}
		if node.FirstTriggerNS != 0 &&
			((node.FirstOutputNS != 0 && node.FirstOutputNS < node.FirstTriggerNS) ||
				(node.CompletionNS != 0 && node.CompletionNS < node.FirstTriggerNS)) {
			return fmt.Errorf("%w: session source returned impossible reaction timing", ErrConflict)
		}
		if decision := node.AuthorityDecision; decision != nil {
			if err := decision.Validate(); err != nil ||
				node.FirstTriggerNS != 0 && decision.AtNS < node.FirstTriggerNS ||
				node.FirstOutputNS != 0 && decision.AtNS < node.FirstOutputNS ||
				node.CompletionNS != 0 && decision.AtNS < node.CompletionNS {
				return fmt.Errorf("%w: session source returned invalid authority-decision evidence", ErrConflict)
			}
		}
		if err := element.ValidateIdentity(node.Resolution.Element); err != nil ||
			node.Resolution.Runtime.Validate() != nil {
			return fmt.Errorf("%w: session source returned invalid node resolution", ErrConflict)
		}
		if _, err := inspect.CanonicalCapabilities(node.Resolution.Capabilities); err != nil {
			return fmt.Errorf("%w: session source returned invalid capabilities", ErrConflict)
		}
	}
	if len(snapshot.Edges) > 65_536 {
		return fmt.Errorf("%w: session source returned too many live edges", ErrConflict)
	}
	for id, edge := range snapshot.Edges {
		if !canonicalEvidenceName(id) || edge.Occupancy < 0 || edge.HighWater < edge.Occupancy ||
			edge.Dequeued > edge.Enqueued || edge.Enqueued-edge.Dequeued != uint64(edge.Occupancy) {
			return fmt.Errorf("%w: session source returned impossible queue telemetry", ErrConflict)
		}
	}
	return nil
}

// ValidateInspectionModel bounds and verifies the source-free static model
// returned under a session capability. It intentionally accepts only the
// frontend-neutral projection, never authoring source locations or values.
func ValidateInspectionModel(model inspect.Model) error {
	if !canonicalEvidenceName(model.GraphID) || model.Revision == 0 || !CanonicalDigest(model.Fingerprint) ||
		len(model.Nodes) == 0 || len(model.Nodes) > 65_536 || len(model.Edges) > 65_536 ||
		len(model.Boundaries) > 65_536 || len(model.Scopes) > 65_536 {
		return fmt.Errorf("%w: session source returned an invalid static model", ErrConflict)
	}
	nodes := make(map[string]element.Identity, len(model.Nodes))
	ports := make(map[string]map[string]element.Direction, len(model.Nodes))
	for _, node := range model.Nodes {
		if !canonicalEvidenceName(node.ID) || len(node.Ports) == 0 || len(node.Ports) > 65_536 ||
			len(node.Reaction.Triggers) > 65_536 || len(node.Reaction.SampledState) > 65_536 ||
			len(node.Reaction.Interrupts) > 65_536 || len(node.Reaction.Outcomes) > 65_536 ||
			len(node.Dependencies) > 65_536 || len(node.Effects) > 65_536 ||
			node.Reaction.MaxConcurrency < 0 || element.ValidateIdentity(node.Element) != nil {
			return fmt.Errorf("%w: session source returned an invalid static node", ErrConflict)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return fmt.Errorf("%w: session source repeated a static node", ErrConflict)
		}
		nodes[node.ID] = node.Element
		nodePorts := make(map[string]element.Direction, len(node.Ports))
		for _, port := range node.Ports {
			if !canonicalEvidenceName(port.Name) || !canonicalEvidenceName(port.Type) ||
				!canonicalEvidenceName(port.Role) || len(port.Lanes) > 65_536 {
				return fmt.Errorf("%w: session source returned an invalid static port", ErrConflict)
			}
			switch port.Direction {
			case element.Input, element.Output:
			default:
				return fmt.Errorf("%w: session source returned a static port with invalid direction", ErrConflict)
			}
			switch port.Cardinality {
			case element.One, element.Variadic:
			default:
				return fmt.Errorf("%w: session source returned a static port with invalid cardinality", ErrConflict)
			}
			if _, duplicate := nodePorts[port.Name]; duplicate {
				return fmt.Errorf("%w: session source repeated a static port", ErrConflict)
			}
			nodePorts[port.Name] = port.Direction
			laneNames := make(map[string]struct{}, len(port.Lanes))
			for _, lane := range port.Lanes {
				if !canonicalEvidenceName(lane) {
					return fmt.Errorf("%w: session source returned an invalid static lane", ErrConflict)
				}
				if _, duplicate := laneNames[lane]; duplicate {
					return fmt.Errorf("%w: session source repeated a static lane", ErrConflict)
				}
				laneNames[lane] = struct{}{}
			}
		}
		ports[node.ID] = nodePorts
		for _, reactionPorts := range []struct {
			names     []string
			direction element.Direction
		}{
			{node.Reaction.Triggers, element.Input},
			{node.Reaction.SampledState, element.Input},
			{node.Reaction.Interrupts, element.Input},
			{node.Reaction.Outcomes, element.Output},
		} {
			seen := make(map[string]struct{}, len(reactionPorts.names))
			for _, name := range reactionPorts.names {
				if nodePorts[name] != reactionPorts.direction {
					return fmt.Errorf("%w: session source returned an invalid static reaction", ErrConflict)
				}
				if _, duplicate := seen[name]; duplicate {
					return fmt.Errorf("%w: session source repeated a static reaction port", ErrConflict)
				}
				seen[name] = struct{}{}
			}
		}
		dependencies := make(map[string]struct{}, len(node.Dependencies))
		for _, dependency := range node.Dependencies {
			if !canonicalEvidenceName(dependency.Name) {
				return fmt.Errorf("%w: session source returned an invalid static dependency", ErrConflict)
			}
			if _, duplicate := dependencies[dependency.Name]; duplicate {
				return fmt.Errorf("%w: session source repeated a static dependency", ErrConflict)
			}
			dependencies[dependency.Name] = struct{}{}
		}
		effects := make(map[string]struct{}, len(node.Effects))
		for _, effect := range node.Effects {
			if !canonicalEvidenceName(effect.Name) ||
				(effect.Authority != "" && !canonicalEvidenceName(effect.Authority)) {
				return fmt.Errorf("%w: session source returned an invalid static effect", ErrConflict)
			}
			if _, duplicate := effects[effect.Name]; duplicate {
				return fmt.Errorf("%w: session source repeated a static effect", ErrConflict)
			}
			effects[effect.Name] = struct{}{}
		}
	}
	edges := make(map[string]struct{}, len(model.Edges))
	for _, edge := range model.Edges {
		if !canonicalEvidenceName(edge.ID) || !canonicalEvidenceName(edge.Type) ||
			!canonicalEvidenceName(edge.Role) || edge.Depth <= 0 ||
			(edge.Delivery != ir.Lossless && edge.Delivery != ir.Lossy) {
			return fmt.Errorf("%w: session source returned an invalid static edge", ErrConflict)
		}
		if _, duplicate := edges[edge.ID]; duplicate {
			return fmt.Errorf("%w: session source repeated a static edge", ErrConflict)
		}
		edges[edge.ID] = struct{}{}
		if ports[edge.From.Node][edge.From.Port] != element.Output {
			return fmt.Errorf("%w: session static edge has an unknown source", ErrConflict)
		}
		if ports[edge.To.Node][edge.To.Port] != element.Input {
			return fmt.Errorf("%w: session static edge has an unknown target", ErrConflict)
		}
	}
	boundaries := make(map[string]struct{}, len(model.Boundaries))
	for _, boundary := range model.Boundaries {
		if !canonicalEvidenceName(boundary.Name) || !canonicalEvidenceName(boundary.Type) ||
			!canonicalEvidenceName(boundary.Role) {
			return fmt.Errorf("%w: session source returned an invalid static boundary", ErrConflict)
		}
		if _, duplicate := boundaries[boundary.Name]; duplicate {
			return fmt.Errorf("%w: session source repeated a static boundary", ErrConflict)
		}
		boundaries[boundary.Name] = struct{}{}
		direction := ports[boundary.Endpoint.Node][boundary.Endpoint.Port]
		if (boundary.Direction == ir.InputBoundary && direction != element.Input) ||
			(boundary.Direction == ir.OutputBoundary && direction != element.Output) ||
			(boundary.Direction != ir.InputBoundary && boundary.Direction != ir.OutputBoundary) {
			return fmt.Errorf("%w: session static boundary has an unknown endpoint", ErrConflict)
		}
	}
	scopes := make(map[string]struct{}, len(model.Scopes))
	for _, scope := range model.Scopes {
		if !canonicalEvidenceName(scope.ID) || element.ValidateIdentity(scope.Composite) != nil ||
			(scope.Parent != "" && !canonicalEvidenceName(scope.Parent)) ||
			len(scope.Nodes) > 65_536 || len(scope.Boundaries) > 65_536 {
			return fmt.Errorf("%w: session source returned an invalid static scope", ErrConflict)
		}
		if _, duplicate := scopes[scope.ID]; duplicate {
			return fmt.Errorf("%w: session source repeated a static scope", ErrConflict)
		}
		scopes[scope.ID] = struct{}{}
		members := make(map[string]struct{}, len(scope.Nodes))
		for _, node := range scope.Nodes {
			if _, found := nodes[node]; !found {
				return fmt.Errorf("%w: session static scope has an unknown node", ErrConflict)
			}
			if _, duplicate := members[node]; duplicate {
				return fmt.Errorf("%w: session static scope repeats a node", ErrConflict)
			}
			members[node] = struct{}{}
		}
	}
	return nil
}

// ValidateSessionModel proves that one static model describes exactly the
// graph and node contracts named by the accompanying live snapshot.
func ValidateSessionModel(snapshot inspect.Live, model inspect.Model) error {
	if err := ValidateSessionSnapshot(snapshot); err != nil {
		return err
	}
	if err := ValidateInspectionModel(model); err != nil {
		return err
	}
	if model.GraphID != snapshot.GraphID || model.Revision != snapshot.GraphRevision ||
		model.Fingerprint != snapshot.Fingerprint || len(model.Nodes) != len(snapshot.Nodes) {
		return fmt.Errorf("%w: session static and live graph identities differ", ErrConflict)
	}
	for _, node := range model.Nodes {
		live, found := snapshot.Nodes[node.ID]
		if !found || live.Resolution == nil || live.Resolution.Element != node.Element {
			return fmt.Errorf("%w: session static and live node identities differ", ErrConflict)
		}
	}
	expectedEdges := make(map[string]int, len(model.Edges)+len(model.Boundaries))
	for _, edge := range model.Edges {
		expectedEdges[edge.ID] = edge.Depth
	}
	for _, boundary := range model.Boundaries {
		expectedEdges[ir.BoundaryQueuePrefix+boundary.Name] = 0
	}
	if len(snapshot.Edges) != len(expectedEdges) {
		return fmt.Errorf("%w: session static and live edge populations differ", ErrConflict)
	}
	for id, depth := range expectedEdges {
		live, found := snapshot.Edges[id]
		if !found || (depth > 0 && (live.Occupancy > depth || live.HighWater > depth)) {
			return fmt.Errorf("%w: session static and live edge evidence differs", ErrConflict)
		}
	}
	return nil
}

func canonicalEvidenceName(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 1024 &&
		!strings.ContainsAny(value, "\x00\r\n")
}

// ValidateDeltaPage verifies a bounded response against the request that
// selected it. Event payload structure is subsequently covered by the trace
// package when a complete recording is exported; this boundary additionally
// prevents cursor and resource substitution.
func ValidateDeltaPage(session string, after uint64, limit uint32, page DeltaPage) error {
	if !CanonicalSessionID(session) || limit == 0 || limit > 4096 ||
		page.FormatVersion != 1 || page.SessionID != session || page.After != after ||
		page.Next < page.After || len(page.Events) > int(limit) ||
		page.Graph.FormatVersion != ir.FormatVersion || page.Graph.ID == "" ||
		page.Graph.Revision == 0 || !CanonicalDigest(page.Graph.Fingerprint) {
		return fmt.Errorf("%w: session source returned an invalid delta page", ErrConflict)
	}
	if page.Baseline != nil {
		if page.Baseline.Sequence == 0 || page.Baseline.Sequence > page.Next {
			return fmt.Errorf("%w: delta baseline has an invalid sequence", ErrConflict)
		}
		if page.After != 0 && !page.Compacted && page.Baseline.Sequence > page.After {
			return fmt.Errorf("%w: an unsolicited delta baseline changes cursor semantics", ErrConflict)
		}
	}
	previous := page.After
	if page.Baseline != nil && page.Baseline.Sequence > previous {
		previous = page.Baseline.Sequence
	}
	for index, event := range page.Events {
		if event.Sequence <= previous || event.Sequence > page.Next {
			return fmt.Errorf("%w: delta event %d has an invalid sequence", ErrConflict, index)
		}
		previous = event.Sequence
	}
	if len(page.Events) > 0 && page.Next != page.Events[len(page.Events)-1].Sequence {
		return fmt.Errorf("%w: delta cursor does not name its last event", ErrConflict)
	}
	return nil
}

// ValidateReconciliationReceipt binds a mutation receipt to the exact request
// reviewed by the caller.
func ValidateReconciliationReceipt(
	request ReconciliationRequest, receipt ReconciliationReceipt,
) error {
	if receipt.FormatVersion != 1 || receipt.SessionID != request.SessionID ||
		receipt.PreviousFingerprint != request.ExpectedFingerprint ||
		receipt.CandidateFingerprint != request.Candidate.Fingerprint ||
		!CanonicalDigest(receipt.PreviousFingerprint) ||
		!CanonicalDigest(receipt.CandidateFingerprint) {
		return fmt.Errorf("%w: reconciler returned an invalid receipt", ErrConflict)
	}
	return nil
}
