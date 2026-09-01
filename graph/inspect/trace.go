package inspect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	// LiveTraceFormatVersion 2 adds exact deployment and session-adapter
	// evidence to the envelope. Version 1 remains readable only when both
	// fields are absent; a strict v1 artifact must never acquire v2 semantics.
	LiveTraceFormatVersion       uint64 = 2
	legacyLiveTraceFormatVersion uint64 = 1
	MaxLiveTraceBytes                   = 32 << 20
)

// Absolute ceilings bound hostile artifacts before their self-declared limits
// are trusted. A recording chooses smaller explicit limits and stores them in
// the artifact so truncation is reviewable rather than implicit.
const (
	hardMaxTraceEvents       uint32 = 1 << 18
	hardMaxTraceSnapshots    uint32 = 1 << 14
	hardMaxTraceNodes        uint32 = 1 << 14
	hardMaxTraceEdges        uint32 = 1 << 16
	hardMaxTraceFlows        uint32 = 1 << 14
	hardMaxTraceEdgesPerFlow uint32 = 1 << 14
	hardMaxTraceCapabilities uint32 = 1 << 12
)

// TraceLimits are part of a trace's signed semantics. MaxEdges bounds queue
// overlays per snapshot (including exported boundary queues), while
// MaxEdgesPerFlow independently bounds a causal route.
type TraceLimits struct {
	MaxEvents              uint32 `json:"max_events"`
	MaxSnapshots           uint32 `json:"max_snapshots"`
	MaxNodes               uint32 `json:"max_nodes_per_snapshot"`
	MaxEdges               uint32 `json:"max_edges_per_snapshot"`
	MaxFlows               uint32 `json:"max_flows_per_snapshot"`
	MaxEdgesPerFlow        uint32 `json:"max_edges_per_flow"`
	MaxCapabilitiesPerNode uint32 `json:"max_capabilities_per_node"`
}

func DefaultTraceLimits() TraceLimits {
	return TraceLimits{
		MaxEvents: 65536, MaxSnapshots: 1024, MaxNodes: 4096,
		MaxEdges: 16384, MaxFlows: 4096, MaxEdgesPerFlow: 1024,
		MaxCapabilitiesPerNode: 256,
	}
}

func hardTraceLimits() TraceLimits {
	return TraceLimits{
		MaxEvents: hardMaxTraceEvents, MaxSnapshots: hardMaxTraceSnapshots,
		MaxNodes: hardMaxTraceNodes, MaxEdges: hardMaxTraceEdges,
		MaxFlows: hardMaxTraceFlows, MaxEdgesPerFlow: hardMaxTraceEdgesPerFlow,
		MaxCapabilitiesPerNode: hardMaxTraceCapabilities,
	}
}

func (limits TraceLimits) Validate() error {
	values := []struct {
		name string
		got  uint32
		max  uint32
	}{
		{"max_events", limits.MaxEvents, hardMaxTraceEvents},
		{"max_snapshots", limits.MaxSnapshots, hardMaxTraceSnapshots},
		{"max_nodes_per_snapshot", limits.MaxNodes, hardMaxTraceNodes},
		{"max_edges_per_snapshot", limits.MaxEdges, hardMaxTraceEdges},
		{"max_flows_per_snapshot", limits.MaxFlows, hardMaxTraceFlows},
		{"max_edges_per_flow", limits.MaxEdgesPerFlow, hardMaxTraceEdgesPerFlow},
		{"max_capabilities_per_node", limits.MaxCapabilitiesPerNode, hardMaxTraceCapabilities},
	}
	for _, value := range values {
		if value.got == 0 || value.got > value.max {
			return fmt.Errorf("trace limit %s is %d, want 1..%d", value.name, value.got, value.max)
		}
	}
	return nil
}

type TraceGraphState string

const (
	TraceGraphMounted TraceGraphState = "mounted"
	TraceGraphRunning TraceGraphState = "running"
	TraceGraphClosed  TraceGraphState = "closed"
)

type TraceNodeState string

const (
	TraceNodeMounted TraceNodeState = "mounted"
	TraceNodeRunning TraceNodeState = "running"
	TraceNodeStopped TraceNodeState = "stopped"
	TraceNodeFailed  TraceNodeState = "failed"
)

// TraceGraphLive contains only enum state and monotonic telemetry. Failure is
// a boolean on purpose: free-form errors can inherit private request content.
type TraceGraphLive struct {
	State        TraceGraphState `json:"state"`
	Failed       bool            `json:"failed,omitempty"`
	TraceDropped uint64          `json:"trace_dropped,omitempty"`
}

type TraceNodeLive struct {
	Node              string                 `json:"node"`
	State             TraceNodeState         `json:"state"`
	ActiveRuns        uint32                 `json:"active_runs"`
	FirstTriggerNS    uint64                 `json:"first_trigger_ns,omitempty"`
	FirstOutputNS     uint64                 `json:"first_output_ns,omitempty"`
	CompletionNS      uint64                 `json:"completion_ns,omitempty"`
	CancellationNS    uint64                 `json:"cancellation_ns,omitempty"`
	AuthorityDecision *AuthorityDecisionLive `json:"authority_decision,omitempty"`
	Resolution        NodeResolution         `json:"resolution"`
}

// TraceEdgeLive is a complete queue overlay after one observation. IDs name
// immutable Graph IR edges or "boundary:<name>" queues; no item ID is stored.
type TraceEdgeLive struct {
	Edge         string `json:"edge"`
	Depth        int    `json:"depth"`
	Occupancy    int    `json:"occupancy"`
	HighWater    int    `json:"high_water"`
	Enqueued     uint64 `json:"enqueued"`
	Dequeued     uint64 `json:"dequeued"`
	Dropped      uint64 `json:"dropped"`
	Backpressure uint64 `json:"backpressure"`
	QueueWaitNS  uint64 `json:"queue_wait_ns,omitempty"`
}

// TraceFlowLive uses SHA-256 opaque correlation and causal identities rather
// than envelope metadata. Edge repetitions and their parallel monotonic
// timestamps, direct-parent assertions, and closed semantic classifications
// are retained because loops, retries, and derived items are semantically
// different from sets of visited channels.
// EdgeNS and CausalStages remain optional when reading an older trace artifact.
type TraceFlowLive struct {
	Correlation  string            `json:"correlation"`
	Edges        []string          `json:"edges"`
	EdgeNS       []uint64          `json:"edge_ns,omitempty"`
	CausalStages []CausalStageLive `json:"causal_stages,omitempty"`
	FirstNS      uint64            `json:"first_ns,omitempty"`
	LastNS       uint64            `json:"last_ns,omitempty"`
	Truncated    bool              `json:"truncated,omitempty"`
}

type TraceSnapshot struct {
	Sequence uint64          `json:"sequence"`
	AtNS     uint64          `json:"at_ns"`
	Graph    TraceGraphLive  `json:"graph"`
	Nodes    []TraceNodeLive `json:"nodes"`
	Edges    []TraceEdgeLive `json:"edges"`
	Flows    []TraceFlowLive `json:"flows,omitempty"`
}

type TraceEventKind string

const (
	TraceEventGraph      TraceEventKind = "graph"
	TraceEventNode       TraceEventKind = "node"
	TraceEventEdge       TraceEventKind = "edge"
	TraceEventFlow       TraceEventKind = "flow"
	TraceEventFlowRemove TraceEventKind = "flow_remove"
)

// TraceEvent is a typed replacement delta. Exactly one value matching Kind is
// present. This makes replay deterministic and keeps payload-bearing event
// envelopes out of the artifact schema entirely.
type TraceEvent struct {
	Sequence uint64          `json:"sequence"`
	AtNS     uint64          `json:"at_ns"`
	Kind     TraceEventKind  `json:"kind"`
	Graph    *TraceGraphLive `json:"graph,omitempty"`
	Node     *TraceNodeLive  `json:"node,omitempty"`
	Edge     *TraceEdgeLive  `json:"edge,omitempty"`
	Flow     *TraceFlowLive  `json:"flow,omitempty"`
	FlowID   string          `json:"flow_id,omitempty"`
}

// LiveTrace is an immutable, fingerprinted, payload-free recording. Events
// and snapshots share one strictly increasing sequence/time domain.
type LiveTrace struct {
	FormatVersion uint64                    `json:"format_version"`
	Graph         GraphReference            `json:"graph"`
	Configuration ArtifactIdentity          `json:"configuration"`
	Deployment    *DeploymentEvidence       `json:"deployment,omitempty"`
	Adapter       *SessionAdapterResolution `json:"adapter,omitempty"`
	Limits        TraceLimits               `json:"limits"`
	Snapshots     []TraceSnapshot           `json:"snapshots"`
	Events        []TraceEvent              `json:"events,omitempty"`
	Fingerprint   string                    `json:"fingerprint"`
}

func (trace LiveTrace) Clone() LiveTrace {
	result := trace
	if trace.Deployment != nil {
		copy := trace.Deployment.Clone()
		result.Deployment = &copy
	}
	if trace.Adapter != nil {
		copy := *trace.Adapter
		result.Adapter = &copy
	}
	result.Snapshots = make([]TraceSnapshot, len(trace.Snapshots))
	for index, snapshot := range trace.Snapshots {
		result.Snapshots[index] = snapshot.Clone()
	}
	result.Events = make([]TraceEvent, len(trace.Events))
	for index, event := range trace.Events {
		result.Events[index] = event.Clone()
	}
	return result
}

func (snapshot TraceSnapshot) Clone() TraceSnapshot {
	result := snapshot
	result.Nodes = make([]TraceNodeLive, len(snapshot.Nodes))
	for index, node := range snapshot.Nodes {
		result.Nodes[index] = cloneTraceNode(node)
	}
	result.Edges = slices.Clone(snapshot.Edges)
	result.Flows = make([]TraceFlowLive, len(snapshot.Flows))
	for index, flow := range snapshot.Flows {
		result.Flows[index] = flow.Clone()
	}
	return result
}

func (event TraceEvent) Clone() TraceEvent {
	result := event
	if event.Graph != nil {
		copy := *event.Graph
		result.Graph = &copy
	}
	if event.Node != nil {
		copy := cloneTraceNode(*event.Node)
		result.Node = &copy
	}
	if event.Edge != nil {
		copy := *event.Edge
		result.Edge = &copy
	}
	if event.Flow != nil {
		copy := event.Flow.Clone()
		result.Flow = &copy
	}
	return result
}

func (flow TraceFlowLive) Clone() TraceFlowLive {
	flow.Edges = slices.Clone(flow.Edges)
	flow.EdgeNS = slices.Clone(flow.EdgeNS)
	causalStages := flow.CausalStages
	flow.CausalStages = make([]CausalStageLive, len(causalStages))
	for index, stage := range causalStages {
		flow.CausalStages[index] = stage.Clone()
	}
	return flow
}

func cloneTraceNode(node TraceNodeLive) TraceNodeLive {
	result := node
	if node.AuthorityDecision != nil {
		copy := *node.AuthorityDecision
		result.AuthorityDecision = &copy
	}
	result.Resolution = node.Resolution.Clone()
	return result
}

// FreezeLiveTrace clones, canonicalizes, validates, and fingerprints a trace.
func FreezeLiveTrace(trace LiveTrace) (LiveTrace, error) {
	result := trace.Clone()
	result.Fingerprint = ""
	if err := canonicalizeTrace(&result); err != nil {
		return LiveTrace{}, err
	}
	if err := result.validateStructure(); err != nil {
		return LiveTrace{}, err
	}
	fingerprint, err := result.computeFingerprint()
	if err != nil {
		return LiveTrace{}, err
	}
	result.Fingerprint = fingerprint
	return result, nil
}

func (trace LiveTrace) Validate() error {
	result := trace.Clone()
	if err := canonicalizeTrace(&result); err != nil {
		return err
	}
	if err := result.validateStructure(); err != nil {
		return err
	}
	want, err := result.computeFingerprint()
	if err != nil {
		return err
	}
	if trace.Fingerprint != want {
		return fmt.Errorf("live trace fingerprint is %q, want %q", trace.Fingerprint, want)
	}
	return nil
}

func MarshalLiveTrace(trace LiveTrace) ([]byte, error) {
	if err := trace.Validate(); err != nil {
		return nil, fmt.Errorf("encode live trace: %w", err)
	}
	canonical := trace.Clone()
	if err := canonicalizeTrace(&canonical); err != nil {
		return nil, fmt.Errorf("encode live trace: %w", err)
	}
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode live trace: %w", err)
	}
	if len(payload)+1 > MaxLiveTraceBytes {
		return nil, fmt.Errorf("encode live trace: artifact exceeds %d bytes", MaxLiveTraceBytes)
	}
	return append(payload, '\n'), nil
}

func ParseLiveTrace(source []byte) (LiveTrace, error) {
	if len(source) > MaxLiveTraceBytes {
		return LiveTrace{}, fmt.Errorf("decode live trace: artifact exceeds %d bytes", MaxLiveTraceBytes)
	}
	if err := strictjson.Validate(source); err != nil {
		return LiveTrace{}, fmt.Errorf("decode live trace: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var trace LiveTrace
	if err := decoder.Decode(&trace); err != nil {
		return LiveTrace{}, fmt.Errorf("decode live trace: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return LiveTrace{}, errors.New("decode live trace: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return LiveTrace{}, fmt.Errorf("decode live trace trailing data: %w", err)
	}
	if err := trace.Validate(); err != nil {
		return LiveTrace{}, fmt.Errorf("decode live trace: %w", err)
	}
	if err := canonicalizeTrace(&trace); err != nil {
		return LiveTrace{}, fmt.Errorf("decode live trace: %w", err)
	}
	return trace, nil
}

func (trace LiveTrace) computeFingerprint() (string, error) {
	semantic := trace.Clone()
	semantic.Fingerprint = ""
	if err := canonicalizeTrace(&semantic); err != nil {
		return "", err
	}
	payload, err := json.Marshal(semantic)
	if err != nil {
		return "", fmt.Errorf("fingerprint live trace: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func canonicalizeTrace(trace *LiveTrace) error {
	if trace.Deployment != nil {
		canonical, err := CanonicalDeploymentEvidence(*trace.Deployment)
		if err != nil {
			return fmt.Errorf("deployment evidence: %w", err)
		}
		trace.Deployment = &canonical
	}
	for snapshotIndex := range trace.Snapshots {
		snapshot := &trace.Snapshots[snapshotIndex]
		for nodeIndex := range snapshot.Nodes {
			canonical, err := canonicalTraceNode(snapshot.Nodes[nodeIndex])
			if err != nil {
				return fmt.Errorf("snapshot %d node %s: %w",
					snapshot.Sequence, snapshot.Nodes[nodeIndex].Node, err)
			}
			snapshot.Nodes[nodeIndex] = canonical
		}
		sort.Slice(snapshot.Nodes, func(left, right int) bool {
			return snapshot.Nodes[left].Node < snapshot.Nodes[right].Node
		})
		sort.Slice(snapshot.Edges, func(left, right int) bool {
			return snapshot.Edges[left].Edge < snapshot.Edges[right].Edge
		})
		for flowIndex := range snapshot.Flows {
			snapshot.Flows[flowIndex].Edges = slices.Clone(snapshot.Flows[flowIndex].Edges)
		}
		sort.Slice(snapshot.Flows, func(left, right int) bool {
			return snapshot.Flows[left].Correlation < snapshot.Flows[right].Correlation
		})
	}
	for eventIndex := range trace.Events {
		event := &trace.Events[eventIndex]
		if event.Node != nil {
			canonical, err := canonicalTraceNode(*event.Node)
			if err != nil {
				return fmt.Errorf("event %d node %s: %w", event.Sequence, event.Node.Node, err)
			}
			event.Node = &canonical
		}
		if event.Flow != nil {
			copy := event.Flow.Clone()
			event.Flow = &copy
		}
	}
	if trace.Adapter != nil {
		if err := trace.Adapter.Validate(); err != nil {
			return fmt.Errorf("live trace adapter: %w", err)
		}
	}
	return nil
}

func canonicalTraceNode(node TraceNodeLive) (TraceNodeLive, error) {
	result := cloneTraceNode(node)
	capabilities, err := CanonicalCapabilities(result.Resolution.Capabilities)
	if err != nil {
		return TraceNodeLive{}, err
	}
	if len(capabilities) == 0 {
		capabilities = nil
	}
	result.Resolution.Capabilities = capabilities
	return result, nil
}

func (trace LiveTrace) validateStructure() error {
	if trace.FormatVersion != legacyLiveTraceFormatVersion && trace.FormatVersion != LiveTraceFormatVersion {
		return fmt.Errorf("live trace format is %d, want %d or %d",
			trace.FormatVersion, legacyLiveTraceFormatVersion, LiveTraceFormatVersion)
	}
	if trace.FormatVersion == legacyLiveTraceFormatVersion &&
		(trace.Deployment != nil || trace.Adapter != nil) {
		return errors.New("live trace format 1 cannot carry deployment or session-adapter evidence")
	}
	if err := validateGraphReference(trace.Graph); err != nil {
		return err
	}
	if err := validateTraceArtifact("configuration", trace.Configuration); err != nil {
		return fmt.Errorf("live trace: %w", err)
	}
	if trace.Configuration.Digest == "" {
		return errors.New("live trace configuration requires a digest")
	}
	if trace.Deployment != nil {
		if err := trace.Deployment.Validate(); err != nil {
			return fmt.Errorf("live trace deployment: %w", err)
		}
	}
	if err := trace.Limits.Validate(); err != nil {
		return err
	}
	if len(trace.Snapshots) == 0 {
		return errors.New("live trace requires at least one snapshot")
	}
	if uint64(len(trace.Snapshots)) > uint64(trace.Limits.MaxSnapshots) {
		return fmt.Errorf("live trace has %d snapshots, limit is %d",
			len(trace.Snapshots), trace.Limits.MaxSnapshots)
	}
	if uint64(len(trace.Events)) > uint64(trace.Limits.MaxEvents) {
		return fmt.Errorf("live trace has %d events, limit is %d", len(trace.Events), trace.Limits.MaxEvents)
	}
	for index, snapshot := range trace.Snapshots {
		if err := validateTraceSnapshot(snapshot, trace.Limits); err != nil {
			return err
		}
		if index > 0 && snapshot.Sequence <= trace.Snapshots[index-1].Sequence {
			return errors.New("live trace snapshots are not strictly sequence ordered")
		}
	}
	for index, event := range trace.Events {
		if err := validateTraceEvent(event, trace.Limits); err != nil {
			return err
		}
		if index > 0 && event.Sequence <= trace.Events[index-1].Sequence {
			return errors.New("live trace events are not strictly sequence ordered")
		}
	}
	records, err := mergeTraceRecords(trace.Snapshots, trace.Events)
	if err != nil {
		return err
	}
	if len(records) == 0 || records[0].snapshot == nil {
		return errors.New("live trace first record must be a complete snapshot")
	}
	var previousSequence, previousTime uint64
	for index, record := range records {
		if record.sequence == 0 {
			return errors.New("live trace record sequence must be positive")
		}
		if index > 0 {
			if record.sequence <= previousSequence {
				return fmt.Errorf("live trace sequence %d is not strictly greater than %d",
					record.sequence, previousSequence)
			}
			if record.atNS < previousTime {
				return fmt.Errorf("live trace time %d precedes %d", record.atNS, previousTime)
			}
		}
		previousSequence, previousTime = record.sequence, record.atNS
	}
	return nil
}

func validateGraphReference(reference GraphReference) error {
	if reference.FormatVersion != ir.FormatVersion {
		return fmt.Errorf("live trace Graph IR format is %d, want %d",
			reference.FormatVersion, ir.FormatVersion)
	}
	if !canonicalTraceName(reference.ID) {
		return errors.New("live trace graph requires a canonical ID")
	}
	if reference.Revision == 0 {
		return errors.New("live trace graph requires a positive revision")
	}
	return validateTraceSHA256("live trace graph fingerprint", reference.Fingerprint)
}

func validateTraceSnapshot(snapshot TraceSnapshot, limits TraceLimits) error {
	if snapshot.Sequence == 0 {
		return errors.New("live trace snapshot sequence must be positive")
	}
	if err := validateTraceGraph(snapshot.Graph); err != nil {
		return fmt.Errorf("live trace snapshot %d: %w", snapshot.Sequence, err)
	}
	if len(snapshot.Nodes) == 0 || uint64(len(snapshot.Nodes)) > uint64(limits.MaxNodes) {
		return fmt.Errorf("live trace snapshot %d has %d nodes, limit is 1..%d",
			snapshot.Sequence, len(snapshot.Nodes), limits.MaxNodes)
	}
	if uint64(len(snapshot.Edges)) > uint64(limits.MaxEdges) {
		return fmt.Errorf("live trace snapshot %d has %d edges, limit is 0..%d",
			snapshot.Sequence, len(snapshot.Edges), limits.MaxEdges)
	}
	if uint64(len(snapshot.Flows)) > uint64(limits.MaxFlows) {
		return fmt.Errorf("live trace snapshot %d has %d flows, limit is %d",
			snapshot.Sequence, len(snapshot.Flows), limits.MaxFlows)
	}
	seenNodes := make(map[string]struct{}, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if _, duplicate := seenNodes[node.Node]; duplicate {
			return fmt.Errorf("live trace snapshot %d repeats node %q", snapshot.Sequence, node.Node)
		}
		seenNodes[node.Node] = struct{}{}
		if err := validateTraceNode(node, limits); err != nil {
			return fmt.Errorf("live trace snapshot %d: %w", snapshot.Sequence, err)
		}
		if err := validateTraceTimes(snapshot.AtNS, &node, nil); err != nil {
			return fmt.Errorf("live trace snapshot %d: %w", snapshot.Sequence, err)
		}
	}
	seenEdges := make(map[string]struct{}, len(snapshot.Edges))
	for _, edge := range snapshot.Edges {
		if _, duplicate := seenEdges[edge.Edge]; duplicate {
			return fmt.Errorf("live trace snapshot %d repeats edge %q", snapshot.Sequence, edge.Edge)
		}
		seenEdges[edge.Edge] = struct{}{}
		if err := validateTraceEdge(edge); err != nil {
			return fmt.Errorf("live trace snapshot %d: %w", snapshot.Sequence, err)
		}
	}
	seenFlows := make(map[string]struct{}, len(snapshot.Flows))
	for _, flow := range snapshot.Flows {
		if _, duplicate := seenFlows[flow.Correlation]; duplicate {
			return fmt.Errorf("live trace snapshot %d repeats flow %q", snapshot.Sequence, flow.Correlation)
		}
		seenFlows[flow.Correlation] = struct{}{}
		if err := validateTraceFlow(flow, limits); err != nil {
			return fmt.Errorf("live trace snapshot %d: %w", snapshot.Sequence, err)
		}
		if err := validateTraceTimes(snapshot.AtNS, nil, &flow); err != nil {
			return fmt.Errorf("live trace snapshot %d: %w", snapshot.Sequence, err)
		}
	}
	if err := validateCausalAssertions(snapshot.Flows); err != nil {
		return fmt.Errorf("live trace snapshot %d: %w", snapshot.Sequence, err)
	}
	return nil
}

func validateCausalAssertions(flows []TraceFlowLive) error {
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

func validateTraceEvent(event TraceEvent, limits TraceLimits) error {
	if event.Sequence == 0 {
		return errors.New("live trace event sequence must be positive")
	}
	pointers := 0
	for _, present := range []bool{event.Graph != nil, event.Node != nil, event.Edge != nil, event.Flow != nil} {
		if present {
			pointers++
		}
	}
	switch event.Kind {
	case TraceEventGraph:
		if pointers != 1 || event.Graph == nil || event.FlowID != "" {
			return fmt.Errorf("live trace event %d graph kind has inconsistent fields", event.Sequence)
		}
		return validateTraceGraph(*event.Graph)
	case TraceEventNode:
		if pointers != 1 || event.Node == nil || event.FlowID != "" {
			return fmt.Errorf("live trace event %d node kind has inconsistent fields", event.Sequence)
		}
		if err := validateTraceNode(*event.Node, limits); err != nil {
			return err
		}
		return validateTraceTimes(event.AtNS, event.Node, nil)
	case TraceEventEdge:
		if pointers != 1 || event.Edge == nil || event.FlowID != "" {
			return fmt.Errorf("live trace event %d edge kind has inconsistent fields", event.Sequence)
		}
		return validateTraceEdge(*event.Edge)
	case TraceEventFlow:
		if pointers != 1 || event.Flow == nil || event.FlowID != "" {
			return fmt.Errorf("live trace event %d flow kind has inconsistent fields", event.Sequence)
		}
		if err := validateTraceFlow(*event.Flow, limits); err != nil {
			return err
		}
		return validateTraceTimes(event.AtNS, nil, event.Flow)
	case TraceEventFlowRemove:
		if pointers != 0 || !canonicalTraceDigest(event.FlowID) {
			return fmt.Errorf("live trace event %d flow_remove requires one opaque flow ID", event.Sequence)
		}
		return nil
	default:
		return fmt.Errorf("live trace event %d has unknown kind %q", event.Sequence, event.Kind)
	}
}

func validateTraceGraph(graph TraceGraphLive) error {
	switch graph.State {
	case TraceGraphMounted, TraceGraphRunning, TraceGraphClosed:
		return nil
	default:
		return fmt.Errorf("live trace has invalid graph state %q", graph.State)
	}
}

func validateTraceNode(node TraceNodeLive, limits TraceLimits) error {
	if !canonicalTraceName(node.Node) {
		return fmt.Errorf("live trace has invalid node %q", node.Node)
	}
	switch node.State {
	case TraceNodeMounted, TraceNodeRunning, TraceNodeStopped, TraceNodeFailed:
	default:
		return fmt.Errorf("live trace node %s has invalid state %q", node.Node, node.State)
	}
	if err := validateTraceResolution(node.Resolution, limits); err != nil {
		return fmt.Errorf("live trace node %s resolution: %w", node.Node, err)
	}
	for _, observed := range []struct {
		label string
		value uint64
	}{
		{"first output", node.FirstOutputNS}, {"completion", node.CompletionNS},
	} {
		if node.FirstTriggerNS != 0 && observed.value != 0 && observed.value < node.FirstTriggerNS {
			return fmt.Errorf("live trace node %s %s precedes its first trigger", node.Node, observed.label)
		}
	}
	if decision := node.AuthorityDecision; decision != nil {
		if err := decision.Validate(); err != nil {
			return fmt.Errorf("live trace node %s authority decision: %w", node.Node, err)
		}
		if node.FirstTriggerNS != 0 && decision.AtNS < node.FirstTriggerNS ||
			node.FirstOutputNS != 0 && decision.AtNS < node.FirstOutputNS ||
			node.CompletionNS != 0 && decision.AtNS < node.CompletionNS {
			return fmt.Errorf("live trace node %s authority decision precedes its observed reaction", node.Node)
		}
	}
	return nil
}

func validateTraceResolution(resolution NodeResolution, limits TraceLimits) error {
	if err := element.ValidateIdentity(resolution.Element); err != nil {
		return err
	}
	// An empty selector is the Graph IR's explicit default implementation
	// selection. The exact mounted code identity is still mandatory in Runtime.
	// Non-empty selectors must themselves be immutable and canonical.
	if resolution.Implementation != "" &&
		(!canonicalTraceName(resolution.Implementation) || placeholderIdentity(resolution.Implementation)) {
		return errors.New("implementation selector requires an exact canonical identity")
	}
	if err := validateTraceArtifact("runtime", resolution.Runtime); err != nil {
		return err
	}
	switch resolution.RuntimeEvidence {
	case EvidenceRegistered, EvidenceLive:
	default:
		return fmt.Errorf("runtime evidence is %q, want registered or live", resolution.RuntimeEvidence)
	}
	if resolution.CapabilitiesEvidence != EvidenceLive {
		return fmt.Errorf("capability evidence is %q, want live", resolution.CapabilitiesEvidence)
	}
	if uint64(len(resolution.Capabilities)) > uint64(limits.MaxCapabilitiesPerNode) {
		return fmt.Errorf("has %d capabilities, limit is %d",
			len(resolution.Capabilities), limits.MaxCapabilitiesPerNode)
	}
	canonical, err := CanonicalCapabilities(resolution.Capabilities)
	if err != nil {
		return err
	}
	if !sameJSON(canonical, resolution.Capabilities) {
		return errors.New("capabilities are not canonical")
	}
	for _, capability := range canonical {
		if !canonicalTraceName(capability.Name) ||
			(capability.Contract != "" && !canonicalTraceName(capability.Contract)) {
			return fmt.Errorf("capability %q has a non-canonical name or contract", capability.Name)
		}
		if err := validateTraceArtifact("capability provider", capability.Provider); err != nil {
			return err
		}
		if capability.Adapter != nil {
			if err := validateTraceArtifact("capability adapter", *capability.Adapter); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateTraceEdge(edge TraceEdgeLive) error {
	if !canonicalTraceChannel(edge.Edge) {
		return fmt.Errorf("live trace has invalid edge %q", edge.Edge)
	}
	if edge.Depth <= 0 || edge.Occupancy < 0 || edge.Occupancy > edge.Depth ||
		edge.HighWater < edge.Occupancy || edge.HighWater > edge.Depth {
		return fmt.Errorf("live trace edge %s has impossible queue depth/occupancy/high-water", edge.Edge)
	}
	if edge.Dequeued > edge.Enqueued || edge.Enqueued-edge.Dequeued != uint64(edge.Occupancy) {
		return fmt.Errorf("live trace edge %s counters disagree with occupancy", edge.Edge)
	}
	return nil
}

func validateTraceFlow(flow TraceFlowLive, limits TraceLimits) error {
	if !canonicalTraceDigest(flow.Correlation) {
		return fmt.Errorf("live trace flow correlation %q is not an opaque SHA-256 identity", flow.Correlation)
	}
	if len(flow.Edges) == 0 || uint64(len(flow.Edges)) > uint64(limits.MaxEdgesPerFlow) {
		return fmt.Errorf("live trace flow %s has %d edges, limit is 1..%d",
			flow.Correlation, len(flow.Edges), limits.MaxEdgesPerFlow)
	}
	for _, edge := range flow.Edges {
		if !canonicalTraceName(edge) || strings.HasPrefix(edge, ir.BoundaryQueuePrefix) {
			return fmt.Errorf("live trace flow %s contains invalid internal edge %q", flow.Correlation, edge)
		}
	}
	if len(flow.EdgeNS) != 0 && len(flow.EdgeNS) != len(flow.Edges) {
		return fmt.Errorf("live trace flow %s has %d edge timestamps for %d edges",
			flow.Correlation, len(flow.EdgeNS), len(flow.Edges))
	}
	if len(flow.CausalStages) != 0 && len(flow.CausalStages) != len(flow.Edges) {
		return fmt.Errorf("live trace flow %s has %d causal stages for %d edges",
			flow.Correlation, len(flow.CausalStages), len(flow.Edges))
	}
	assertions := make(map[string]CausalStageLive, len(flow.CausalStages))
	for index, stage := range flow.CausalStages {
		if !canonicalTraceDigest(stage.Item) {
			return fmt.Errorf("live trace flow %s causal stage %d has invalid item identity %q",
				flow.Correlation, index, stage.Item)
		}
		if len(stage.Parents) > MaximumCausalParentsPerStage {
			return fmt.Errorf("live trace flow %s causal stage %d has %d parents, limit is %d",
				flow.Correlation, index, len(stage.Parents), MaximumCausalParentsPerStage)
		}
		if stage.Kind != "" {
			if err := stage.Kind.Validate(); err != nil {
				return fmt.Errorf("live trace flow %s causal stage %d: %w",
					flow.Correlation, index, err)
			}
		}
		seenParents := make(map[string]struct{}, len(stage.Parents))
		for _, parent := range stage.Parents {
			if !canonicalTraceDigest(parent) || parent == stage.Item {
				return fmt.Errorf("live trace flow %s causal stage %d has an invalid parent identity",
					flow.Correlation, index)
			}
			if _, duplicate := seenParents[parent]; duplicate {
				return fmt.Errorf("live trace flow %s causal stage %d repeats a parent identity",
					flow.Correlation, index)
			}
			seenParents[parent] = struct{}{}
		}
		if previous, found := assertions[stage.Item]; found &&
			(previous.Kind != stage.Kind || !slices.Equal(previous.Parents, stage.Parents)) {
			return fmt.Errorf("live trace flow %s rewrites causal parents or semantic kind for one item", flow.Correlation)
		}
		assertions[stage.Item] = stage.Clone()
	}
	for index, atNS := range flow.EdgeNS {
		if index > 0 && atNS < flow.EdgeNS[index-1] {
			return fmt.Errorf("live trace flow %s edge timestamp %d regresses", flow.Correlation, index)
		}
		if atNS < flow.FirstNS || atNS > flow.LastNS {
			return fmt.Errorf("live trace flow %s edge timestamp %d is outside its first/last times",
				flow.Correlation, index)
		}
	}
	if flow.FirstNS > flow.LastNS {
		return fmt.Errorf("live trace flow %s last time precedes first time", flow.Correlation)
	}
	return nil
}

func validateTraceSHA256(label, value string) error {
	if !canonicalTraceDigest(value) {
		return fmt.Errorf("%s is not a canonical SHA-256 digest", label)
	}
	return nil
}

func validateTraceArtifact(label string, artifact ArtifactIdentity) error {
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !canonicalTraceName(artifact.ID) ||
		(artifact.Revision != "" && !canonicalTraceName(artifact.Revision)) {
		return fmt.Errorf("%s contains an invalid identifier", label)
	}
	if artifact.Digest != "" {
		if err := validateTraceSHA256(label+" digest", artifact.Digest); err != nil {
			return err
		}
	}
	return nil
}

func canonicalTraceDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func canonicalTraceName(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 1024 &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func canonicalTraceChannel(value string) bool {
	if strings.HasPrefix(value, ir.BoundaryQueuePrefix) {
		return canonicalTraceName(strings.TrimPrefix(value, ir.BoundaryQueuePrefix))
	}
	return canonicalTraceName(value)
}

type traceRecord struct {
	sequence uint64
	atNS     uint64
	snapshot *TraceSnapshot
	event    *TraceEvent
}

func mergeTraceRecords(snapshots []TraceSnapshot, events []TraceEvent) ([]traceRecord, error) {
	result := make([]traceRecord, 0, len(snapshots)+len(events))
	left, right := 0, 0
	for left < len(snapshots) || right < len(events) {
		switch {
		case right == len(events) || left < len(snapshots) && snapshots[left].Sequence < events[right].Sequence:
			copy := snapshots[left].Clone()
			result = append(result, traceRecord{
				sequence: copy.Sequence, atNS: copy.AtNS, snapshot: &copy,
			})
			left++
		case left == len(snapshots) || events[right].Sequence < snapshots[left].Sequence:
			copy := events[right].Clone()
			result = append(result, traceRecord{
				sequence: copy.Sequence, atNS: copy.AtNS, event: &copy,
			})
			right++
		default:
			return nil, fmt.Errorf("live trace repeats sequence %d across event and snapshot",
				snapshots[left].Sequence)
		}
	}
	return result, nil
}

// OpaqueTraceCorrelation replaces a payload-derived runtime correlation with
// the only fixed-width representation accepted by LiveTrace. Recorders that
// require cross-artifact unlinkability should first apply a session-secret
// keyed pseudonym; the trace intentionally stores no key or original value.
func OpaqueTraceCorrelation(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// OpaqueTraceCausalIdentity replaces an envelope item or direct-parent ID with
// a domain-separated fixed-width trace identity. A recorder first applies its
// session-secret pseudonym so two artifacts cannot be linked by this digest.
func OpaqueTraceCausalIdentity(value string) string {
	digest := sha256.Sum256([]byte("openrealtime/causal/v1\x00" + value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
