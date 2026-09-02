package inspect

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// TraceOverlay is the deterministic canvas state after a trace record. Maps
// are keyed only by immutable graph identities or opaque flow digests.
type TraceOverlay struct {
	Graph         GraphReference            `json:"graph"`
	Configuration ArtifactIdentity          `json:"configuration"`
	Deployment    *DeploymentEvidence       `json:"deployment,omitempty"`
	Adapter       *SessionAdapterResolution `json:"adapter,omitempty"`
	Sequence      uint64                    `json:"sequence"`
	AtNS          uint64                    `json:"at_ns"`
	Live          TraceGraphLive            `json:"live"`
	Nodes         map[string]TraceNodeLive  `json:"nodes"`
	Edges         map[string]TraceEdgeLive  `json:"edges"`
	Flows         map[string]TraceFlowLive  `json:"flows,omitempty"`
}

func (overlay TraceOverlay) Clone() TraceOverlay {
	result := overlay
	if overlay.Deployment != nil {
		copy := overlay.Deployment.Clone()
		result.Deployment = &copy
	}
	if overlay.Adapter != nil {
		copy := *overlay.Adapter
		result.Adapter = &copy
	}
	result.Nodes = make(map[string]TraceNodeLive, len(overlay.Nodes))
	for id, node := range overlay.Nodes {
		result.Nodes[id] = cloneTraceNode(node)
	}
	result.Edges = make(map[string]TraceEdgeLive, len(overlay.Edges))
	for id, edge := range overlay.Edges {
		result.Edges[id] = edge
	}
	result.Flows = make(map[string]TraceFlowLive, len(overlay.Flows))
	for id, flow := range overlay.Flows {
		result.Flows[id] = flow.Clone()
	}
	return result
}

type TraceRecordSummary struct {
	Sequence uint64         `json:"sequence"`
	AtNS     uint64         `json:"at_ns"`
	Kind     TraceEventKind `json:"kind"`
}

// TraceReplayer validates the trace against the exact Graph IR, including
// complete node/channel coverage, immutable resolution identity, graph edge
// membership, lifecycle order, and monotonic counters. It retains private
// clones, so concurrent replay and caller mutation are race-free.
type TraceReplayer struct {
	graph         GraphReference
	configuration ArtifactIdentity
	deployment    *DeploymentEvidence
	adapter       *SessionAdapterResolution
	records       []traceRecord
	nodes         map[string]ir.Node
	edges         map[string]int
	internalEdges map[string]struct{}
	limits        TraceLimits
}

func NewTraceReplayer(graph ir.Graph, trace LiveTrace) (*TraceReplayer, error) {
	if err := graph.Validate(); err != nil {
		return nil, fmt.Errorf("configure live trace replay: graph: %w", err)
	}
	if err := trace.Validate(); err != nil {
		return nil, fmt.Errorf("configure live trace replay: trace: %w", err)
	}
	wantGraph := graphReference(graph)
	if trace.Graph != wantGraph {
		return nil, fmt.Errorf("configure live trace replay: graph identity is %+v, want %+v",
			trace.Graph, wantGraph)
	}
	private := trace.Clone()
	records, err := mergeTraceRecords(private.Snapshots, private.Events)
	if err != nil {
		return nil, fmt.Errorf("configure live trace replay: %w", err)
	}
	replayer := &TraceReplayer{
		graph: wantGraph, configuration: private.Configuration,
		records: records, nodes: make(map[string]ir.Node, len(graph.Nodes)),
		edges:         make(map[string]int, len(graph.Edges)+len(graph.Boundaries)),
		internalEdges: make(map[string]struct{}, len(graph.Edges)), limits: private.Limits,
	}
	if private.Deployment != nil {
		copy := private.Deployment.Clone()
		replayer.deployment = &copy
	}
	if private.Adapter != nil {
		copy := *private.Adapter
		replayer.adapter = &copy
	}
	for _, node := range graph.Nodes {
		replayer.nodes[node.ID] = cloneDiffNode(node)
	}
	for _, edge := range graph.Edges {
		replayer.edges[edge.ID] = edge.Depth
		replayer.internalEdges[edge.ID] = struct{}{}
	}
	for _, boundary := range graph.Boundaries {
		port := graphPort(graph, boundary.Endpoint.Node, boundary.Endpoint.Port)
		replayer.edges[ir.BoundaryQueuePrefix+boundary.Name] = traceBoundaryDepth(port, boundary.Type)
	}
	if _, _, err := replayer.replay(replayer.records[len(replayer.records)-1].sequence); err != nil {
		return nil, fmt.Errorf("configure live trace replay: %w", err)
	}
	return replayer, nil
}

func (replayer *TraceReplayer) Timeline() []TraceRecordSummary {
	if replayer == nil {
		return nil
	}
	result := make([]TraceRecordSummary, len(replayer.records))
	for index, record := range replayer.records {
		kind := TraceEventKind("snapshot")
		if record.event != nil {
			kind = record.event.Kind
		}
		result[index] = TraceRecordSummary{Sequence: record.sequence, AtNS: record.atNS, Kind: kind}
	}
	return result
}

// At returns the overlay after the last record whose sequence is at most the
// requested value. Requests outside the captured interval are refused rather
// than silently pretending the trace covers time it did not record.
func (replayer *TraceReplayer) At(sequence uint64) (TraceOverlay, error) {
	if replayer == nil || len(replayer.records) == 0 {
		return TraceOverlay{}, errors.New("replay live trace: nil or empty replayer")
	}
	first := replayer.records[0].sequence
	last := replayer.records[len(replayer.records)-1].sequence
	if sequence < first || sequence > last {
		return TraceOverlay{}, fmt.Errorf("replay live trace: sequence %d is outside %d..%d",
			sequence, first, last)
	}
	overlay, _, err := replayer.replay(sequence)
	if err != nil {
		return TraceOverlay{}, fmt.Errorf("replay live trace: %w", err)
	}
	return overlay.Clone(), nil
}

func (replayer *TraceReplayer) Final() (TraceOverlay, error) {
	if replayer == nil || len(replayer.records) == 0 {
		return TraceOverlay{}, errors.New("replay live trace: nil or empty replayer")
	}
	return replayer.At(replayer.records[len(replayer.records)-1].sequence)
}

type replayState struct {
	overlay     TraceOverlay
	retiredFlow map[string]struct{}
}

func (replayer *TraceReplayer) replay(sequence uint64) (TraceOverlay, *replayState, error) {
	state := &replayState{retiredFlow: make(map[string]struct{})}
	for _, record := range replayer.records {
		if record.sequence > sequence {
			break
		}
		var err error
		if record.snapshot != nil {
			err = replayer.applySnapshot(state, *record.snapshot)
		} else {
			err = replayer.applyEvent(state, *record.event)
		}
		if err != nil {
			return TraceOverlay{}, state, fmt.Errorf("record %d: %w", record.sequence, err)
		}
		state.overlay.Sequence = record.sequence
		state.overlay.AtNS = record.atNS
	}
	return state.overlay, state, nil
}

func (replayer *TraceReplayer) applySnapshot(state *replayState, snapshot TraceSnapshot) error {
	if err := replayer.validateSnapshotCoverage(snapshot); err != nil {
		return err
	}
	next := TraceOverlay{
		Graph: replayer.graph, Configuration: replayer.configuration,
		Deployment: cloneDeploymentEvidencePointer(replayer.deployment),
		Adapter:    cloneSessionAdapterResolution(replayer.adapter),
		Live:       snapshot.Graph, Nodes: make(map[string]TraceNodeLive, len(snapshot.Nodes)),
		Edges: make(map[string]TraceEdgeLive, len(snapshot.Edges)),
		Flows: make(map[string]TraceFlowLive, len(snapshot.Flows)),
	}
	if state.overlay.Nodes != nil {
		if err := monotonicGraph(state.overlay.Live, snapshot.Graph); err != nil {
			return err
		}
	}
	for _, node := range snapshot.Nodes {
		if previous, found := state.overlay.Nodes[node.Node]; found {
			if err := monotonicNode(previous, node); err != nil {
				return err
			}
		}
		next.Nodes[node.Node] = cloneTraceNode(node)
	}
	for _, edge := range snapshot.Edges {
		if previous, found := state.overlay.Edges[edge.Edge]; found {
			if err := monotonicEdge(previous, edge); err != nil {
				return err
			}
		}
		next.Edges[edge.Edge] = edge
	}
	for _, flow := range snapshot.Flows {
		if _, retired := state.retiredFlow[flow.Correlation]; retired {
			return fmt.Errorf("retired flow %s reappeared", flow.Correlation)
		}
		if previous, found := state.overlay.Flows[flow.Correlation]; found {
			if err := monotonicFlow(previous, flow); err != nil {
				return err
			}
		}
		next.Flows[flow.Correlation] = flow.Clone()
	}
	for correlation := range state.overlay.Flows {
		if _, retained := next.Flows[correlation]; !retained {
			state.retiredFlow[correlation] = struct{}{}
		}
	}
	if err := validateTraceOverlayCausality(next.Flows); err != nil {
		return err
	}
	state.overlay = next
	return nil
}

func cloneSessionAdapterResolution(
	source *SessionAdapterResolution,
) *SessionAdapterResolution {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

func (replayer *TraceReplayer) applyEvent(state *replayState, event TraceEvent) error {
	if state.overlay.Nodes == nil {
		return errors.New("event precedes the initial snapshot")
	}
	switch event.Kind {
	case TraceEventGraph:
		if err := monotonicGraph(state.overlay.Live, *event.Graph); err != nil {
			return err
		}
		state.overlay.Live = *event.Graph
	case TraceEventNode:
		if _, known := replayer.nodes[event.Node.Node]; !known {
			return fmt.Errorf("node event names unknown graph node %q", event.Node.Node)
		}
		previous, found := state.overlay.Nodes[event.Node.Node]
		if !found {
			return fmt.Errorf("node event names absent overlay node %q", event.Node.Node)
		}
		if err := replayer.validateNodeIdentity(*event.Node); err != nil {
			return err
		}
		if err := monotonicNode(previous, *event.Node); err != nil {
			return err
		}
		state.overlay.Nodes[event.Node.Node] = cloneTraceNode(*event.Node)
	case TraceEventEdge:
		depth, known := replayer.edges[event.Edge.Edge]
		if !known || event.Edge.Depth != depth {
			return fmt.Errorf("edge event %q has depth %d, graph requires %d",
				event.Edge.Edge, event.Edge.Depth, depth)
		}
		previous, found := state.overlay.Edges[event.Edge.Edge]
		if !found {
			return fmt.Errorf("edge event names absent overlay edge %q", event.Edge.Edge)
		}
		if err := monotonicEdge(previous, *event.Edge); err != nil {
			return err
		}
		state.overlay.Edges[event.Edge.Edge] = *event.Edge
	case TraceEventFlow:
		flow := *event.Flow
		if _, retired := state.retiredFlow[flow.Correlation]; retired {
			return fmt.Errorf("retired flow %s reappeared", flow.Correlation)
		}
		if err := replayer.validateFlowEdges(flow); err != nil {
			return err
		}
		if previous, found := state.overlay.Flows[flow.Correlation]; found {
			if err := monotonicFlow(previous, flow); err != nil {
				return err
			}
		} else if uint64(len(state.overlay.Flows)) >= uint64(replayer.limits.MaxFlows) {
			return fmt.Errorf("flow event exceeds live flow limit %d", replayer.limits.MaxFlows)
		}
		state.overlay.Flows[flow.Correlation] = flow.Clone()
		if err := validateTraceOverlayCausality(state.overlay.Flows); err != nil {
			return err
		}
	case TraceEventFlowRemove:
		if _, found := state.overlay.Flows[event.FlowID]; !found {
			return fmt.Errorf("flow_remove names absent flow %s", event.FlowID)
		}
		delete(state.overlay.Flows, event.FlowID)
		state.retiredFlow[event.FlowID] = struct{}{}
	default:
		panic("validated trace event has unknown kind")
	}
	return nil
}

func validateTraceOverlayCausality(flows map[string]TraceFlowLive) error {
	assertions := make(map[string]CausalStageLive)
	for _, flow := range flows {
		for _, stage := range flow.CausalStages {
			if previous, found := assertions[stage.Item]; found &&
				(previous.Kind != stage.Kind || !slices.Equal(previous.Parents, stage.Parents)) {
				return fmt.Errorf("causal item %s has conflicting direct parents or semantic kind", stage.Item)
			}
			assertions[stage.Item] = stage.Clone()
		}
	}
	return nil
}

func (replayer *TraceReplayer) validateSnapshotCoverage(snapshot TraceSnapshot) error {
	if len(snapshot.Nodes) != len(replayer.nodes) {
		return fmt.Errorf("snapshot has %d nodes, exact graph has %d", len(snapshot.Nodes), len(replayer.nodes))
	}
	seenNodes := make(map[string]struct{}, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if _, found := replayer.nodes[node.Node]; !found {
			return fmt.Errorf("snapshot contains unknown graph node %q", node.Node)
		}
		if err := replayer.validateNodeIdentity(node); err != nil {
			return err
		}
		seenNodes[node.Node] = struct{}{}
	}
	for node := range replayer.nodes {
		if _, found := seenNodes[node]; !found {
			return fmt.Errorf("snapshot is missing graph node %q", node)
		}
	}
	if len(snapshot.Edges) != len(replayer.edges) {
		return fmt.Errorf("snapshot has %d queue edges, exact graph has %d",
			len(snapshot.Edges), len(replayer.edges))
	}
	seenEdges := make(map[string]struct{}, len(snapshot.Edges))
	for _, edge := range snapshot.Edges {
		depth, found := replayer.edges[edge.Edge]
		if !found {
			return fmt.Errorf("snapshot contains unknown graph queue %q", edge.Edge)
		}
		if edge.Depth != depth {
			return fmt.Errorf("snapshot queue %q depth is %d, graph requires %d", edge.Edge, edge.Depth, depth)
		}
		seenEdges[edge.Edge] = struct{}{}
	}
	for edge := range replayer.edges {
		if _, found := seenEdges[edge]; !found {
			return fmt.Errorf("snapshot is missing graph queue %q", edge)
		}
	}
	for _, flow := range snapshot.Flows {
		if err := replayer.validateFlowEdges(flow); err != nil {
			return err
		}
	}
	return nil
}

func (replayer *TraceReplayer) validateNodeIdentity(node TraceNodeLive) error {
	want := replayer.nodes[node.Node]
	if node.Resolution.Element != want.Element {
		return fmt.Errorf("node %s trace element is %+v, graph selects %+v",
			node.Node, node.Resolution.Element, want.Element)
	}
	if node.Resolution.Implementation != want.Implementation {
		return fmt.Errorf("node %s trace implementation is %q, graph selects %q",
			node.Node, node.Resolution.Implementation, want.Implementation)
	}
	return nil
}

func (replayer *TraceReplayer) validateFlowEdges(flow TraceFlowLive) error {
	for _, edge := range flow.Edges {
		if _, found := replayer.internalEdges[edge]; !found {
			return fmt.Errorf("flow %s contains unknown internal graph edge %q", flow.Correlation, edge)
		}
	}
	return nil
}

func monotonicGraph(before, after TraceGraphLive) error {
	if after.TraceDropped < before.TraceDropped {
		return errors.New("graph trace-dropped counter decreased")
	}
	if before.Failed && !after.Failed {
		return errors.New("graph failure flag reverted")
	}
	if !graphStateTransition(before.State, after.State) {
		return fmt.Errorf("graph state regressed from %q to %q", before.State, after.State)
	}
	return nil
}

func graphStateTransition(before, after TraceGraphState) bool {
	if before == after {
		return true
	}
	switch before {
	case TraceGraphMounted:
		return after == TraceGraphRunning || after == TraceGraphClosed
	case TraceGraphRunning:
		return after == TraceGraphClosed
	default:
		return false
	}
}

func monotonicNode(before, after TraceNodeLive) error {
	if !sameJSON(before.Resolution, after.Resolution) {
		return fmt.Errorf("node %s immutable live resolution changed", before.Node)
	}
	if !nodeStateTransition(before.State, after.State) {
		return fmt.Errorf("node %s state regressed from %q to %q", before.Node, before.State, after.State)
	}
	for _, timestamp := range []struct {
		name          string
		before, after uint64
	}{
		{"first-trigger time", before.FirstTriggerNS, after.FirstTriggerNS},
		{"first-output time", before.FirstOutputNS, after.FirstOutputNS},
		{"completion time", before.CompletionNS, after.CompletionNS},
		{"cancellation time", before.CancellationNS, after.CancellationNS},
	} {
		if timestamp.before != 0 && timestamp.after != timestamp.before {
			return fmt.Errorf("node %s immutable %s changed from %d to %d",
				before.Node, timestamp.name, timestamp.before, timestamp.after)
		}
	}
	if before.AuthorityDecision != nil && after.AuthorityDecision == nil {
		return fmt.Errorf("node %s authority decision disappeared", before.Node)
	}
	if before.AuthorityDecision != nil && after.AuthorityDecision != nil &&
		after.AuthorityDecision.AtNS < before.AuthorityDecision.AtNS {
		return fmt.Errorf("node %s authority decision time regressed", before.Node)
	}
	return nil
}

func nodeStateTransition(before, after TraceNodeState) bool {
	if before == after {
		return true
	}
	switch before {
	case TraceNodeMounted:
		return after == TraceNodeRunning || after == TraceNodeStopped || after == TraceNodeFailed
	case TraceNodeRunning:
		return after == TraceNodeStopped || after == TraceNodeFailed
	default:
		return false
	}
}

func monotonicEdge(before, after TraceEdgeLive) error {
	if before.Depth != after.Depth {
		return fmt.Errorf("edge %s depth changed from %d to %d", before.Edge, before.Depth, after.Depth)
	}
	for _, counter := range []struct {
		name          string
		before, after uint64
	}{
		{"enqueued", before.Enqueued, after.Enqueued},
		{"dequeued", before.Dequeued, after.Dequeued},
		{"dropped", before.Dropped, after.Dropped},
		{"backpressure", before.Backpressure, after.Backpressure},
		{"queue-wait", before.QueueWaitNS, after.QueueWaitNS},
	} {
		if counter.after < counter.before {
			return fmt.Errorf("edge %s %s counter decreased", before.Edge, counter.name)
		}
	}
	if after.HighWater < before.HighWater {
		return fmt.Errorf("edge %s high-water mark decreased", before.Edge)
	}
	return nil
}

func monotonicFlow(before, after TraceFlowLive) error {
	if before.Correlation != after.Correlation {
		return errors.New("flow correlation identity changed")
	}
	if len(after.Edges) < len(before.Edges) ||
		!slices.Equal(before.Edges, after.Edges[:len(before.Edges)]) {
		return fmt.Errorf("flow %s edge history was rewritten", before.Correlation)
	}
	if (len(before.EdgeNS) == 0) != (len(after.EdgeNS) == 0) ||
		len(after.EdgeNS) < len(before.EdgeNS) ||
		!slices.Equal(before.EdgeNS, after.EdgeNS[:len(before.EdgeNS)]) {
		return fmt.Errorf("flow %s edge timing history was rewritten", before.Correlation)
	}
	if (len(before.CausalStages) == 0) != (len(after.CausalStages) == 0) ||
		len(after.CausalStages) < len(before.CausalStages) ||
		!sameCausalStagePrefix(before.CausalStages, after.CausalStages) {
		return fmt.Errorf("flow %s causal history was rewritten", before.Correlation)
	}
	if after.FirstNS != before.FirstNS || after.LastNS < before.LastNS || before.Truncated && !after.Truncated {
		return fmt.Errorf("flow %s timing/truncation regressed", before.Correlation)
	}
	return nil
}

func sameCausalStagePrefix(before, after []CausalStageLive) bool {
	if len(after) < len(before) {
		return false
	}
	for index, stage := range before {
		if stage.Item != after[index].Item || stage.Kind != after[index].Kind ||
			!slices.Equal(stage.Parents, after[index].Parents) {
			return false
		}
	}
	return true
}

// TraceSnapshotFromLive produces a payload-free checkpoint from the existing
// best-effort live view. Wall-clock ObservedAt, free-form errors, item IDs,
// trigger IDs, outcomes, and raw correlations are deliberately discarded.
// atNS must come from the mount's monotonic clock.
func TraceSnapshotFromLive(
	graph ir.Graph, configuration ArtifactIdentity, live Live, atNS uint64,
) (TraceSnapshot, error) {
	return traceSnapshotFromLive(graph, configuration, nil, live, atNS, false)
}

// TraceSnapshotFromLiveWithDeployment additionally binds the checkpoint to
// exact deployment evidence. The trace snapshot remains payload-free; the
// deployment identity is retained once at the LiveTrace envelope and replay
// overlay rather than duplicated into every telemetry checkpoint.
func TraceSnapshotFromLiveWithDeployment(
	graph ir.Graph,
	configuration ArtifactIdentity,
	deployment *DeploymentEvidence,
	live Live,
	atNS uint64,
) (TraceSnapshot, error) {
	return traceSnapshotFromLive(graph, configuration, deployment, live, atNS, true)
}

func traceSnapshotFromLive(
	graph ir.Graph,
	configuration ArtifactIdentity,
	deployment *DeploymentEvidence,
	live Live,
	atNS uint64,
	requireDeployment bool,
) (TraceSnapshot, error) {
	if err := graph.Validate(); err != nil {
		return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: graph: %w", err)
	}
	if err := validateTraceArtifact("configuration", configuration); err != nil || configuration.Digest == "" {
		if err == nil {
			err = errors.New("configuration requires a digest")
		}
		return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: %w", err)
	}
	if live.FormatVersion != LiveFormatVersion || live.GraphID != graph.ID ||
		live.GraphRevision != graph.Revision || live.Fingerprint != graph.Fingerprint {
		return TraceSnapshot{}, errors.New("capture live trace snapshot: live graph identity differs")
	}
	if live.Configuration == nil || *live.Configuration != configuration {
		return TraceSnapshot{}, errors.New("capture live trace snapshot: live configuration identity differs")
	}
	if requireDeployment {
		if deployment == nil || live.Deployment == nil {
			return TraceSnapshot{}, errors.New("capture live trace snapshot: exact deployment evidence is required")
		}
		canonical, err := CanonicalDeploymentEvidence(*deployment)
		if err != nil {
			return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: deployment: %w", err)
		}
		if !sameJSON(canonical, *deployment) || !sameJSON(canonical, *live.Deployment) {
			return TraceSnapshot{}, errors.New("capture live trace snapshot: live deployment identity differs")
		}
	}
	graphState, err := traceGraphState(live.State)
	if err != nil {
		return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: %w", err)
	}
	snapshot := TraceSnapshot{
		Sequence: live.Sequence, AtNS: atNS,
		Graph: TraceGraphLive{State: graphState, Failed: live.Error != "", TraceDropped: live.TraceDropped},
		Nodes: make([]TraceNodeLive, 0, len(graph.Nodes)),
		Edges: make([]TraceEdgeLive, 0, len(graph.Edges)+len(graph.Boundaries)),
		Flows: make([]TraceFlowLive, 0, len(live.Flows)),
	}
	for _, graphNode := range graph.Nodes {
		node, found := live.Nodes[graphNode.ID]
		if !found || node.Resolution == nil {
			return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: missing resolved node %q", graphNode.ID)
		}
		state, stateErr := traceNodeState(node.State)
		if stateErr != nil {
			return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: node %s: %w", graphNode.ID, stateErr)
		}
		if node.ActiveRuns < 0 || uint64(node.ActiveRuns) > uint64(^uint32(0)) {
			return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: node %s has invalid active runs", graphNode.ID)
		}
		traceNode := TraceNodeLive{
			Node: graphNode.ID, State: state, ActiveRuns: uint32(node.ActiveRuns),
			FirstTriggerNS: node.FirstTriggerNS, FirstOutputNS: node.FirstOutputNS,
			CompletionNS:   node.CompletionNS,
			CancellationNS: node.CancellationNS, Resolution: node.Resolution.Clone(),
		}
		if node.AuthorityDecision != nil {
			copy := *node.AuthorityDecision
			traceNode.AuthorityDecision = &copy
		}
		snapshot.Nodes = append(snapshot.Nodes, traceNode)
	}
	if len(live.Nodes) != len(graph.Nodes) {
		return TraceSnapshot{}, errors.New("capture live trace snapshot: live view contains extra nodes")
	}
	channels := expectedTraceChannels(graph)
	for channel, depth := range channels {
		edge, found := live.Edges[channel]
		if !found {
			return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: missing queue %q", channel)
		}
		snapshot.Edges = append(snapshot.Edges, TraceEdgeLive{
			Edge: channel, Depth: depth, Occupancy: edge.Occupancy, HighWater: edge.HighWater,
			Enqueued: edge.Enqueued, Dequeued: edge.Dequeued, Dropped: edge.Dropped,
			Backpressure: edge.Backpressure, QueueWaitNS: edge.QueueWaitNS,
		})
	}
	if len(live.Edges) != len(channels) {
		return TraceSnapshot{}, errors.New("capture live trace snapshot: live view contains extra queues")
	}
	seenCorrelations := make(map[string]struct{}, len(live.Flows))
	causalIdentities := newTraceCausalIdentitySet()
	for key, flow := range live.Flows {
		if key == "" || flow.Correlation == "" || key != flow.Correlation {
			return TraceSnapshot{}, errors.New("capture live trace snapshot: inconsistent flow correlation")
		}
		correlation := OpaqueTraceCorrelation(flow.Correlation)
		if _, duplicate := seenCorrelations[correlation]; duplicate {
			return TraceSnapshot{}, errors.New("capture live trace snapshot: opaque flow correlation collision")
		}
		seenCorrelations[correlation] = struct{}{}
		causalStages, causalErr := causalIdentities.encodeStages(flow.CausalStages, len(flow.Edges))
		if causalErr != nil {
			return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: flow %s: %w", key, causalErr)
		}
		snapshot.Flows = append(snapshot.Flows, TraceFlowLive{
			Correlation: correlation,
			Edges:       slices.Clone(flow.Edges), EdgeNS: slices.Clone(flow.EdgeNS),
			CausalStages: causalStages,
			FirstNS:      flow.FirstNS, LastNS: flow.LastNS,
			Truncated: flow.Truncated,
		})
	}
	sort.Slice(snapshot.Nodes, func(left, right int) bool { return snapshot.Nodes[left].Node < snapshot.Nodes[right].Node })
	sort.Slice(snapshot.Edges, func(left, right int) bool { return snapshot.Edges[left].Edge < snapshot.Edges[right].Edge })
	sort.Slice(snapshot.Flows, func(left, right int) bool {
		return snapshot.Flows[left].Correlation < snapshot.Flows[right].Correlation
	})
	if err := validateTraceSnapshot(snapshot, hardTraceLimits()); err != nil {
		return TraceSnapshot{}, fmt.Errorf("capture live trace snapshot: %w", err)
	}
	return snapshot, nil
}

const maximumRawCausalIdentityBytes = 64 << 10

type traceCausalIdentitySet struct {
	opaqueByRaw map[string]string
	rawByOpaque map[string]string
	stages      map[string]CausalStageLive
}

func newTraceCausalIdentitySet() *traceCausalIdentitySet {
	return &traceCausalIdentitySet{
		opaqueByRaw: make(map[string]string), rawByOpaque: make(map[string]string),
		stages: make(map[string]CausalStageLive),
	}
}

func (identities *traceCausalIdentitySet) encode(raw string) (string, error) {
	if raw == "" || len(raw) > maximumRawCausalIdentityBytes {
		return "", errors.New("causal identity is empty or oversized")
	}
	if opaque, found := identities.opaqueByRaw[raw]; found {
		return opaque, nil
	}
	opaque := OpaqueTraceCausalIdentity(raw)
	if previous, collision := identities.rawByOpaque[opaque]; collision && previous != raw {
		return "", errors.New("opaque causal identity collision")
	}
	identities.opaqueByRaw[raw] = opaque
	identities.rawByOpaque[opaque] = raw
	return opaque, nil
}

func (identities *traceCausalIdentitySet) encodeStages(
	stages []CausalStageLive, edges int,
) ([]CausalStageLive, error) {
	if len(stages) == 0 {
		return nil, nil
	}
	if len(stages) != edges {
		return nil, fmt.Errorf("has %d causal stages for %d edges", len(stages), edges)
	}
	result := make([]CausalStageLive, len(stages))
	for index, stage := range stages {
		if stage.Kind != "" {
			if err := stage.Kind.Validate(); err != nil {
				return nil, fmt.Errorf("causal stage %d: %w", index, err)
			}
		}
		if len(stage.Parents) > MaximumCausalParentsPerStage {
			return nil, fmt.Errorf("causal stage %d has %d parents, limit is %d",
				index, len(stage.Parents), MaximumCausalParentsPerStage)
		}
		seenParents := make(map[string]struct{}, len(stage.Parents))
		for _, parent := range stage.Parents {
			if parent == "" || len(parent) > maximumRawCausalIdentityBytes || parent == stage.Item {
				return nil, fmt.Errorf("causal stage %d has an invalid parent identity", index)
			}
			if _, duplicate := seenParents[parent]; duplicate {
				return nil, fmt.Errorf("causal stage %d repeats a parent identity", index)
			}
			seenParents[parent] = struct{}{}
		}
		if previous, found := identities.stages[stage.Item]; found &&
			(previous.Kind != stage.Kind || !slices.Equal(previous.Parents, stage.Parents)) {
			return nil, fmt.Errorf("causal stage %d rewrites one item's direct parents or semantic kind", index)
		}
		identities.stages[stage.Item] = stage.Clone()
		item, err := identities.encode(stage.Item)
		if err != nil {
			return nil, fmt.Errorf("causal stage %d item: %w", index, err)
		}
		result[index].Item = item
		result[index].Kind = stage.Kind
		result[index].Parents = make([]string, len(stage.Parents))
		for parentIndex, raw := range stage.Parents {
			parent, err := identities.encode(raw)
			if err != nil {
				return nil, fmt.Errorf("causal stage %d parent %d: %w", index, parentIndex, err)
			}
			result[index].Parents[parentIndex] = parent
		}
	}
	return result, nil
}

func cloneDeploymentEvidencePointer(source *DeploymentEvidence) *DeploymentEvidence {
	if source == nil {
		return nil
	}
	copy := source.Clone()
	return &copy
}

func expectedTraceChannels(graph ir.Graph) map[string]int {
	result := make(map[string]int, len(graph.Edges)+len(graph.Boundaries))
	for _, edge := range graph.Edges {
		result[edge.ID] = edge.Depth
	}
	for _, boundary := range graph.Boundaries {
		port := graphPort(graph, boundary.Endpoint.Node, boundary.Endpoint.Port)
		result[ir.BoundaryQueuePrefix+boundary.Name] = traceBoundaryDepth(port, boundary.Type)
	}
	return result
}

func graphPort(graph ir.Graph, nodeID, portName string) ir.Port {
	for _, node := range graph.Nodes {
		if node.ID != nodeID {
			continue
		}
		for _, port := range node.Ports {
			if port.Name == portName {
				return clonePort(port)
			}
		}
	}
	panic("validated graph boundary references an unknown port")
}

func traceBoundaryDepth(port ir.Port, valueType element.Type) int {
	if port.DefaultDepth > 0 {
		return port.DefaultDepth
	}
	switch valueType.Name {
	case "State":
		return 1
	case "Stream", "Segmented", "Revisions":
		return 32
	default:
		return 16
	}
}

func traceGraphState(value string) (TraceGraphState, error) {
	switch value {
	case string(TraceGraphMounted):
		return TraceGraphMounted, nil
	case string(TraceGraphRunning):
		return TraceGraphRunning, nil
	case string(TraceGraphClosed):
		return TraceGraphClosed, nil
	default:
		return "", fmt.Errorf("invalid graph state %q", value)
	}
}

func traceNodeState(value string) (TraceNodeState, error) {
	switch value {
	case string(TraceNodeMounted):
		return TraceNodeMounted, nil
	case string(TraceNodeRunning):
		return TraceNodeRunning, nil
	case string(TraceNodeStopped):
		return TraceNodeStopped, nil
	case string(TraceNodeFailed):
		return TraceNodeFailed, nil
	default:
		return "", fmt.Errorf("invalid node state %q", value)
	}
}

func cloneDiffNode(node ir.Node) ir.Node {
	result := node
	result.Ports = make([]ir.Port, len(node.Ports))
	for index, port := range node.Ports {
		result.Ports[index] = clonePort(port)
	}
	result.Reaction.Triggers = slices.Clone(node.Reaction.Triggers)
	result.Reaction.SampledState = slices.Clone(node.Reaction.SampledState)
	result.Reaction.Interrupts = slices.Clone(node.Reaction.Interrupts)
	result.Reaction.Outcomes = slices.Clone(node.Reaction.Outcomes)
	result.StateTransfer = node.StateTransfer.Clone()
	result.Dependencies = slices.Clone(node.Dependencies)
	result.Effects = slices.Clone(node.Effects)
	result.Source = nil
	return result
}

func validateTraceTimes(recordAt uint64, node *TraceNodeLive, flow *TraceFlowLive) error {
	if node != nil {
		for _, value := range []uint64{
			node.FirstTriggerNS, node.FirstOutputNS, node.CompletionNS, node.CancellationNS,
		} {
			if value > recordAt {
				return fmt.Errorf("node %s timestamp %d exceeds record time %d", node.Node, value, recordAt)
			}
		}
		if node.AuthorityDecision != nil && node.AuthorityDecision.AtNS > recordAt {
			return fmt.Errorf("node %s authority decision time %d exceeds record time %d",
				node.Node, node.AuthorityDecision.AtNS, recordAt)
		}
	}
	if flow != nil && (flow.FirstNS > recordAt || flow.LastNS > recordAt) {
		return fmt.Errorf("flow %s timestamp exceeds record time %d", flow.Correlation, recordAt)
	}
	return nil
}
