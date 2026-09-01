package inspect_test

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestLiveTraceStrictRoundTripAndDeterministicReplay(t *testing.T) {
	graph, configuration, trace, secret := liveTraceFixture(t)
	payload, err := inspect.MarshalLiveTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "private_payload", "last_item_id", "last_trigger_id", `"error"`} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("payload-free trace contains %q:\n%s", forbidden, payload)
		}
	}
	parsed, err := inspect.ParseLiveTrace(payload)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := inspect.MarshalLiveTrace(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, reencoded) {
		t.Fatalf("live trace encoding is nondeterministic\n%s\n%s", payload, reencoded)
	}
	if parsed.Graph.Fingerprint != graph.Fingerprint || parsed.Configuration != configuration {
		t.Fatalf("trace lost exact graph/config identity: %+v", parsed)
	}
	if parsed.Adapter == nil || parsed.Adapter.ProfileFingerprint != traceAdapterResolution().ProfileFingerprint {
		t.Fatalf("trace lost exact session adapter identity: %+v", parsed.Adapter)
	}

	replayer, err := inspect.NewTraceReplayer(graph, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if timeline := replayer.Timeline(); len(timeline) != 6 ||
		timeline[0].Kind != inspect.TraceEventKind("snapshot") || timeline[1].Kind != inspect.TraceEventNode {
		t.Fatalf("timeline = %+v", timeline)
	}
	atEdge, err := replayer.At(3)
	if err != nil {
		t.Fatal(err)
	}
	if atEdge.Sequence != 3 || atEdge.Edges["stream"].Occupancy != 1 ||
		atEdge.Edges["stream"].Enqueued != 1 || len(atEdge.Flows) != 0 {
		t.Fatalf("overlay at edge event = %+v", atEdge)
	}
	final, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	correlation := inspect.OpaqueTraceCorrelation("trace:" + secret)
	if final.Sequence != 6 || final.Live.TraceDropped != 1 ||
		final.Edges["stream"].Occupancy != 0 || final.Edges["stream"].Dequeued != 1 ||
		len(final.Flows) != 1 || final.Flows[correlation].Edges[0] != "stream" ||
		final.Adapter == nil || final.Adapter.Implementation != "go://test/session-adapter/v1" {
		t.Fatalf("final overlay = %+v", final)
	}
	if _, err := replayer.At(0); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("replay pretended to cover unrecorded time: %v", err)
	}
}

func TestLiveTraceReadsStrictV1WithoutGrantingV2EvidenceFields(t *testing.T) {
	graph, _, current, _ := liveTraceFixture(t)
	legacy := current.Clone()
	legacy.FormatVersion = 1
	legacy.Adapter = nil
	legacy.Deployment = nil
	legacy.Fingerprint = ""
	frozen, err := inspect.FreezeLiveTrace(legacy)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := inspect.MarshalLiveTrace(frozen)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := inspect.ParseLiveTrace(payload)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.FormatVersion != 1 || parsed.Adapter != nil || parsed.Deployment != nil {
		t.Fatalf("strict v1 trace acquired v2 evidence: %+v", parsed)
	}
	if _, err := inspect.NewTraceReplayer(graph, parsed); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"adapter", "deployment"} {
		t.Run(field, func(t *testing.T) {
			candidate := frozen.Clone()
			candidate.Fingerprint = ""
			if field == "adapter" {
				candidate.Adapter = traceAdapterResolution()
			} else {
				candidate.Deployment = &inspect.DeploymentEvidence{
					Public: inspect.ArtifactIdentity{
						ID: "deployment://trace-test", Revision: "deployment:1",
						Digest: "sha256:" + strings.Repeat("f", 64),
					},
					PrivateDeploymentFingerprint: "sha256:" + strings.Repeat("1", 64),
				}
			}
			_, err := inspect.FreezeLiveTrace(candidate)
			if err == nil || !strings.Contains(err.Error(), "format 1 cannot carry") {
				t.Fatalf("v1 %s evidence refusal = %v", field, err)
			}
		})
	}
}

func TestLiveTraceParserRejectsTamperDuplicateUnknownAndTrailingData(t *testing.T) {
	_, _, trace, _ := liveTraceFixture(t)
	payload, err := inspect.MarshalLiveTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		source []byte
		want   string
	}{
		{
			name: "duplicate", want: "duplicate JSON key",
			source: bytes.Replace(payload, []byte(`"format_version": 2,`),
				[]byte(`"format_version": 2, "format_version": 2,`), 1),
		},
		{
			name: "unknown payload field", want: "unknown field",
			source: bytes.Replace(payload, []byte(`"format_version": 2,`),
				[]byte(`"format_version": 2, "private_payload": "do not accept",`), 1),
		},
		{name: "trailing", source: append(append([]byte(nil), payload...), []byte("{}")...), want: "trailing"},
		{
			name: "fingerprint tamper", want: "fingerprint",
			source: bytes.Replace(payload, []byte(`"trace_dropped": 1`),
				[]byte(`"trace_dropped": 2`), 1),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if bytes.Equal(test.source, payload) {
				t.Fatal("test mutation did not alter artifact")
			}
			_, err := inspect.ParseLiveTrace(test.source)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLiveTraceEnforcesEveryDeclaredRetentionBound(t *testing.T) {
	_, _, frozen, _ := liveTraceFixture(t)
	tests := []struct {
		name   string
		mutate func(*inspect.LiveTrace)
		want   string
	}{
		{name: "events", want: "events, limit", mutate: func(trace *inspect.LiveTrace) {
			trace.Limits.MaxEvents = 1
		}},
		{name: "snapshots", want: "snapshots, limit", mutate: func(trace *inspect.LiveTrace) {
			trace.Limits.MaxSnapshots = 1
		}},
		{name: "nodes", want: "nodes, limit", mutate: func(trace *inspect.LiveTrace) {
			trace.Limits.MaxNodes = 1
		}},
		{name: "edges", want: "edges, limit", mutate: func(trace *inspect.LiveTrace) {
			trace.Limits.MaxEdges = 1
		}},
		{name: "flows", want: "flows, limit", mutate: func(trace *inspect.LiveTrace) {
			trace.Limits.MaxFlows = 1
			copy := trace.Snapshots[1].Flows[0]
			copy.Correlation = inspect.OpaqueTraceCorrelation("second-flow")
			trace.Snapshots[1].Flows = append(trace.Snapshots[1].Flows, copy)
		}},
		{name: "edges per flow", want: "edges, limit", mutate: func(trace *inspect.LiveTrace) {
			trace.Limits.MaxEdgesPerFlow = 1
			trace.Snapshots[1].Flows[0].Edges = append(trace.Snapshots[1].Flows[0].Edges, "stream")
		}},
		{name: "capabilities", want: "capabilities, limit", mutate: func(trace *inspect.LiveTrace) {
			trace.Limits.MaxCapabilitiesPerNode = 1
			capability := inspect.CapabilityIdentity{
				Name: "first", Provider: inspect.ArtifactIdentity{ID: "provider://one", Revision: "weights:1"},
			}
			trace.Snapshots[0].Nodes[0].Resolution.Capabilities = []inspect.CapabilityIdentity{
				capability,
				{Name: "second", Provider: inspect.ArtifactIdentity{ID: "provider://two", Revision: "weights:2"}},
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := frozen.Clone()
			candidate.Fingerprint = ""
			test.mutate(&candidate)
			_, err := inspect.FreezeLiveTrace(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("bound refusal = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLiveTraceRejectsUnboundedDeclarationsAndOversizedSource(t *testing.T) {
	limits := inspect.DefaultTraceLimits()
	tests := []struct {
		name   string
		mutate func(*inspect.TraceLimits)
	}{
		{name: "events", mutate: func(value *inspect.TraceLimits) { value.MaxEvents = math.MaxUint32 }},
		{name: "snapshots", mutate: func(value *inspect.TraceLimits) { value.MaxSnapshots = math.MaxUint32 }},
		{name: "nodes", mutate: func(value *inspect.TraceLimits) { value.MaxNodes = math.MaxUint32 }},
		{name: "edges", mutate: func(value *inspect.TraceLimits) { value.MaxEdges = math.MaxUint32 }},
		{name: "flows", mutate: func(value *inspect.TraceLimits) { value.MaxFlows = math.MaxUint32 }},
		{name: "edges per flow", mutate: func(value *inspect.TraceLimits) { value.MaxEdgesPerFlow = math.MaxUint32 }},
		{name: "capabilities", mutate: func(value *inspect.TraceLimits) {
			value.MaxCapabilitiesPerNode = math.MaxUint32
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := limits
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), "want 1..") {
				t.Fatalf("absolute ceiling refusal = %v", err)
			}
		})
	}

	oversized := make([]byte, inspect.MaxLiveTraceBytes+1)
	if _, err := inspect.ParseLiveTrace(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized source refusal = %v", err)
	}
}

func TestLiveTraceRequiresCanonicalArtifactDigests(t *testing.T) {
	_, _, frozen, _ := liveTraceFixture(t)
	tests := []struct {
		name   string
		mutate func(*inspect.LiveTrace)
	}{
		{name: "configuration", mutate: func(trace *inspect.LiveTrace) {
			trace.Configuration.Digest = "sha256:" + strings.Repeat("A", 64)
		}},
		{name: "runtime", mutate: func(trace *inspect.LiveTrace) {
			trace.Snapshots[0].Nodes[0].Resolution.Runtime.Digest = "sha256:" + strings.Repeat("A", 64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := frozen.Clone()
			candidate.Fingerprint = ""
			test.mutate(&candidate)
			if _, err := inspect.FreezeLiveTrace(candidate); err == nil ||
				!strings.Contains(err.Error(), "canonical SHA-256") {
				t.Fatalf("non-canonical digest refusal = %v", err)
			}
		})
	}
}

func TestLiveTraceRejectsInvalidSessionAdapterEvidence(t *testing.T) {
	_, _, frozen, _ := liveTraceFixture(t)
	tests := []struct {
		name   string
		mutate func(*inspect.SessionAdapterResolution)
		want   string
	}{
		{name: "boundary map digest", want: "boundary map digest", mutate: func(adapter *inspect.SessionAdapterResolution) {
			adapter.BoundaryMapDigest = "sha256:" + strings.Repeat("A", 64)
		}},
		{name: "runtime evidence", want: "runtime evidence", mutate: func(adapter *inspect.SessionAdapterResolution) {
			adapter.RuntimeEvidence = inspect.ResolutionEvidence("assumed")
		}},
		{name: "runtime artifact", want: "runtime", mutate: func(adapter *inspect.SessionAdapterResolution) {
			adapter.Runtime.ID = ""
		}},
		{name: "runtime digest canonical case", want: "runtime identity", mutate: func(adapter *inspect.SessionAdapterResolution) {
			adapter.Runtime.Digest = "sha256:" + strings.Repeat("A", 64)
		}},
		{name: "runtime control character", want: "runtime identity", mutate: func(adapter *inspect.SessionAdapterResolution) {
			adapter.Runtime.Revision = "build:1\nsubstituted"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := frozen.Clone()
			candidate.Fingerprint = ""
			test.mutate(candidate.Adapter)
			_, err := inspect.FreezeLiveTrace(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid adapter evidence refusal = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLiveTraceReplayRejectsSemanticForgeryDespiteValidArtifactFingerprint(t *testing.T) {
	graph, _, frozen, _ := liveTraceFixture(t)
	tests := []struct {
		name   string
		mutate func(*inspect.LiveTrace)
		want   string
	}{
		{name: "counter rollback", want: "counter decreased", mutate: func(trace *inspect.LiveTrace) {
			edge := traceEdge(trace.Snapshots[1].Edges, "stream")
			edge.Enqueued, edge.Dequeued = 0, 0
		}},
		{name: "queue wait rollback", want: "queue-wait counter decreased", mutate: func(trace *inspect.LiveTrace) {
			initial := traceEdge(trace.Snapshots[0].Edges, "stream")
			initial.QueueWaitNS = 2
			trace.Events[1].Edge.QueueWaitNS = 1
		}},
		{name: "resolution drift", want: "immutable live resolution changed", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[0].Node.Resolution.Runtime.Revision = "build:forged"
		}},
		{name: "occurrence timestamp rewrite", want: "immutable first-output time changed", mutate: func(trace *inspect.LiveTrace) {
			for index := range trace.Snapshots[1].Nodes {
				if trace.Snapshots[1].Nodes[index].Node == "source" {
					trace.Snapshots[1].Nodes[index].FirstOutputNS = 106
				}
			}
		}},
		{name: "trigger timestamp rewrite", want: "immutable first-trigger time changed", mutate: func(trace *inspect.LiveTrace) {
			for index := range trace.Snapshots[1].Nodes {
				if trace.Snapshots[1].Nodes[index].Node == "source" {
					trace.Snapshots[1].Nodes[index].FirstTriggerNS++
				}
			}
		}},
		{name: "lifecycle regression", want: "graph state regressed", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[3].Graph.State = inspect.TraceGraphMounted
		}},
		{name: "flow history rewrite", want: "edge history was rewritten", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[2].Flow.Edges = []string{"stream", "stream"}
		}},
		{name: "unknown flow edge", want: "unknown internal graph edge", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[2].Flow.Edges = []string{"unknown-edge"}
			trace.Snapshots[1].Flows[0].Edges = []string{"unknown-edge"}
		}},
		{name: "incomplete snapshot", want: "exact graph has", mutate: func(trace *inspect.LiveTrace) {
			trace.Snapshots[0].Nodes = trace.Snapshots[0].Nodes[:1]
		}},
		{name: "different graph identity", want: "graph identity", mutate: func(trace *inspect.LiveTrace) {
			trace.Graph.ID = "other-graph"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := frozen.Clone()
			candidate.Fingerprint = ""
			test.mutate(&candidate)
			refrozen, err := inspect.FreezeLiveTrace(candidate)
			if err != nil {
				t.Fatalf("test forgery is not structurally valid: %v", err)
			}
			_, err = inspect.NewTraceReplayer(graph, refrozen)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("semantic forgery refusal = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLiveTraceAcceptsGraphDefaultImplementationWithExactRuntimeIdentity(t *testing.T) {
	graph := fixture(t)
	configuration := inspect.ArtifactIdentity{
		ID: "values://default-implementation", Revision: "config:1",
		Digest: "sha256:" + strings.Repeat("6", 64),
	}
	live := traceLiveView(graph, configuration, 1, "mounted", "mounted", "secret")
	for id, node := range live.Nodes {
		if node.Resolution == nil || node.Resolution.Implementation != "" ||
			node.Resolution.Runtime.ID == "" {
			t.Fatalf("fixture node %s does not exercise default selection: %+v", id, node)
		}
	}
	snapshot, err := inspect.TraceSnapshotFromLive(graph, configuration, live, 10)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := inspect.FreezeLiveTrace(inspect.LiveTrace{
		FormatVersion: inspect.LiveTraceFormatVersion,
		Graph: inspect.GraphReference{
			FormatVersion: graph.FormatVersion, ID: graph.ID,
			Revision: graph.Revision, Fingerprint: graph.Fingerprint,
		},
		Configuration: configuration, Limits: inspect.DefaultTraceLimits(),
		Snapshots: []inspect.TraceSnapshot{snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspect.NewTraceReplayer(graph, trace); err != nil {
		t.Fatal(err)
	}
}

func TestLiveTraceRejectsNonMonotonicTimeSequenceAndImpossibleCounters(t *testing.T) {
	_, _, frozen, _ := liveTraceFixture(t)
	tests := []struct {
		name   string
		mutate func(*inspect.LiveTrace)
		want   string
	}{
		{name: "time", want: "time", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[1].AtNS = 90
		}},
		{name: "duplicate sequence", want: "repeats sequence", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[0].Sequence = trace.Snapshots[0].Sequence
		}},
		{name: "impossible counter", want: "counters disagree", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[1].Edge.Occupancy = 0
		}},
		{name: "future node timestamp", want: "exceeds record time", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[0].Node.FirstOutputNS = trace.Events[0].AtNS + 1
		}},
		{name: "future trigger timestamp", want: "exceeds record time", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[0].Node.FirstTriggerNS = trace.Events[0].AtNS + 1
			trace.Events[0].Node.FirstOutputNS = 0
		}},
		{name: "output before trigger", want: "first output precedes its first trigger", mutate: func(trace *inspect.LiveTrace) {
			trace.Events[0].Node.FirstTriggerNS = trace.Events[0].Node.FirstOutputNS + 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := frozen.Clone()
			candidate.Fingerprint = ""
			test.mutate(&candidate)
			_, err := inspect.FreezeLiveTrace(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("temporal refusal = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLiveTraceCloneAndConcurrentReplayAreRecursivelyIndependent(t *testing.T) {
	graph, _, frozen, _ := liveTraceFixture(t)
	input := frozen.Clone()
	input.Fingerprint = ""
	refrozen, err := inspect.FreezeLiveTrace(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Snapshots[0].Nodes[0].Resolution.Runtime.ID = "mutated"
	input.Snapshots[1].Flows[0].Edges[0] = "mutated"
	input.Adapter.Runtime.ID = "mutated"
	if refrozen.Snapshots[0].Nodes[0].Resolution.Runtime.ID == "mutated" ||
		refrozen.Snapshots[1].Flows[0].Edges[0] == "mutated" ||
		refrozen.Adapter.Runtime.ID == "mutated" {
		t.Fatal("FreezeLiveTrace retained caller aliases")
	}
	replayer, err := inspect.NewTraceReplayer(graph, frozen)
	if err != nil {
		t.Fatal(err)
	}
	first, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	first.Nodes["source"] = inspect.TraceNodeLive{}
	first.Edges["stream"] = inspect.TraceEdgeLive{}
	for id, flow := range first.Flows {
		flow.Edges[0] = "mutated"
		first.Flows[id] = flow
	}
	first.Adapter.Runtime.ID = "mutated"
	again, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if again.Nodes["source"].Node != "source" || again.Edges["stream"].Depth == 0 ||
		again.Adapter == nil || again.Adapter.Runtime.ID == "mutated" {
		t.Fatal("replayer retained returned overlay aliases")
	}
	for _, flow := range again.Flows {
		if flow.Edges[0] == "mutated" {
			t.Fatal("replayer retained returned flow aliases")
		}
	}

	var wait sync.WaitGroup
	failures := make(chan error, 128)
	for worker := 0; worker < 32; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 32; iteration++ {
				overlay, replayErr := replayer.At(uint64(1 + iteration%6))
				if replayErr != nil {
					failures <- replayErr
					return
				}
				overlay.Nodes["source"] = inspect.TraceNodeLive{}
				_ = replayer.Timeline()
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
}

func TestLiveTraceSupportsAValidGraphWithNoQueueEdges(t *testing.T) {
	base := fixture(t)
	node := base.Nodes[0]
	node.Implementation = "go://test/isolated/v1"
	node.ConfigReference = "values://isolated"
	node.ConfigDigest = "sha256:" + strings.Repeat("4", 64)
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "isolated", Revision: 1,
		Nodes: []ir.Node{node},
	})
	if err != nil {
		t.Fatal(err)
	}
	configuration := inspect.ArtifactIdentity{
		ID: "values://isolated", Revision: "config:1",
		Digest: "sha256:" + strings.Repeat("5", 64),
	}
	configurationCopy := configuration
	live := inspect.Live{
		FormatVersion: inspect.LiveFormatVersion,
		GraphID:       graph.ID, GraphRevision: graph.Revision, Fingerprint: graph.Fingerprint,
		Configuration: &configurationCopy, Sequence: 1, State: "mounted",
		Nodes: map[string]inspect.NodeLive{node.ID: {
			State: "mounted", Resolution: &inspect.NodeResolution{
				Element: node.Element, Implementation: node.Implementation,
				Runtime:              inspect.ArtifactIdentity{ID: "runtime://isolated", Revision: "build:1"},
				RuntimeEvidence:      inspect.EvidenceRegistered,
				CapabilitiesEvidence: inspect.EvidenceLive,
			},
		}},
	}
	snapshot, err := inspect.TraceSnapshotFromLive(graph, configuration, live, 1)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := inspect.FreezeLiveTrace(inspect.LiveTrace{
		FormatVersion: inspect.LiveTraceFormatVersion,
		Graph: inspect.GraphReference{
			FormatVersion: graph.FormatVersion, ID: graph.ID,
			Revision: graph.Revision, Fingerprint: graph.Fingerprint,
		},
		Configuration: configuration, Limits: inspect.DefaultTraceLimits(),
		Snapshots: []inspect.TraceSnapshot{snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayer, err := inspect.NewTraceReplayer(graph, trace)
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay.Edges) != 0 || len(overlay.Nodes) != 1 {
		t.Fatalf("isolated overlay = %+v", overlay)
	}
}

func liveTraceFixture(t *testing.T) (ir.Graph, inspect.ArtifactIdentity, inspect.LiveTrace, string) {
	t.Helper()
	graph := cloneGraph(t, fixture(t))
	for index := range graph.Nodes {
		node := &graph.Nodes[index]
		node.Implementation = "go://test/" + node.ID + "/v1"
		node.ConfigReference = "values://" + node.ID
		digit := "1"
		if node.ID == "source" {
			digit = "2"
		}
		node.ConfigDigest = "sha256:" + strings.Repeat(digit, 64)
	}
	var err error
	graph, err = ir.Freeze(graph)
	if err != nil {
		t.Fatal(err)
	}
	configuration := inspect.ArtifactIdentity{
		ID: "values://inspect", Revision: "openrealtime.ai/config/v1alpha1",
		Digest: "sha256:" + strings.Repeat("7", 64),
	}
	const secret = "private caller words and tool arguments"
	initialLive := traceLiveView(graph, configuration, 1, "mounted", "mounted", secret)
	initial, err := inspect.TraceSnapshotFromLive(graph, configuration, initialLive, 100)
	if err != nil {
		t.Fatal(err)
	}
	finalLive := traceLiveView(graph, configuration, 5, "running", "running", secret)
	finalLive.Error = secret
	finalLive.Nodes["source"] = withNodeTiming(finalLive.Nodes["source"], 105)
	stream := finalLive.Edges["stream"]
	stream.Enqueued, stream.Dequeued, stream.HighWater = 1, 1, 1
	finalLive.Edges["stream"] = stream
	finalLive.Flows["trace:"+secret] = inspect.FlowLive{
		Correlation: "trace:" + secret, Edges: []string{"stream"}, FirstNS: 120, LastNS: 120,
	}
	finalSnapshot, err := inspect.TraceSnapshotFromLive(graph, configuration, finalLive, 130)
	if err != nil {
		t.Fatal(err)
	}
	source := traceNode(initial.Nodes, "source")
	source.State = inspect.TraceNodeRunning
	source.FirstTriggerNS = 102
	source.FirstOutputNS = 105
	edge := *traceEdge(initial.Edges, "stream")
	edge.Occupancy, edge.HighWater, edge.Enqueued = 1, 1, 1
	flow := inspect.TraceFlowLive{
		Correlation: inspect.OpaqueTraceCorrelation("trace:" + secret),
		Edges:       []string{"stream"}, FirstNS: 120, LastNS: 120,
	}
	candidate := inspect.LiveTrace{
		FormatVersion: inspect.LiveTraceFormatVersion,
		Graph: inspect.GraphReference{
			FormatVersion: graph.FormatVersion, ID: graph.ID,
			Revision: graph.Revision, Fingerprint: graph.Fingerprint,
		},
		Configuration: configuration, Adapter: traceAdapterResolution(), Limits: inspect.DefaultTraceLimits(),
		Snapshots: []inspect.TraceSnapshot{initial, finalSnapshot},
		Events: []inspect.TraceEvent{
			{Sequence: 2, AtNS: 110, Kind: inspect.TraceEventNode, Node: &source},
			{Sequence: 3, AtNS: 120, Kind: inspect.TraceEventEdge, Edge: &edge},
			{Sequence: 4, AtNS: 120, Kind: inspect.TraceEventFlow, Flow: &flow},
			{Sequence: 6, AtNS: 140, Kind: inspect.TraceEventGraph, Graph: &inspect.TraceGraphLive{
				State: inspect.TraceGraphRunning, Failed: true, TraceDropped: 1,
			}},
		},
	}
	frozen, err := inspect.FreezeLiveTrace(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return graph, configuration, frozen, secret
}

func traceAdapterResolution() *inspect.SessionAdapterResolution {
	return &inspect.SessionAdapterResolution{
		ContractName:       "openrealtime.realtime.session",
		ContractRevision:   1,
		ContractDigest:     "sha256:" + strings.Repeat("a", 64),
		ProfileFingerprint: "sha256:" + strings.Repeat("b", 64),
		Implementation:     "go://test/session-adapter/v1",
		Runtime: inspect.ArtifactIdentity{
			ID: "runtime://session-adapter", Revision: "build:1",
			Digest: "sha256:" + strings.Repeat("c", 64),
		},
		RuntimeEvidence:   inspect.EvidenceRegistered,
		BoundaryMapDigest: "sha256:" + strings.Repeat("d", 64),
		ProjectionDigest:  "sha256:" + strings.Repeat("e", 64),
	}
}

func traceLiveView(
	graph ir.Graph, configuration inspect.ArtifactIdentity, sequence uint64,
	graphState, nodeState, secret string,
) inspect.Live {
	configurationCopy := configuration
	live := inspect.Live{
		FormatVersion: inspect.LiveFormatVersion,
		GraphID:       graph.ID, GraphRevision: graph.Revision, Fingerprint: graph.Fingerprint,
		Configuration: &configurationCopy, Sequence: sequence, ObservedAt: time.Now().UTC(),
		State: graphState, Nodes: make(map[string]inspect.NodeLive, len(graph.Nodes)),
		Edges: make(map[string]inspect.EdgeLive, len(graph.Edges)+len(graph.Boundaries)),
		Flows: make(map[string]inspect.FlowLive),
	}
	for _, node := range graph.Nodes {
		live.Nodes[node.ID] = inspect.NodeLive{
			State: nodeState, LastTriggerID: secret, LastOutcome: secret, Error: secret,
			Resolution: &inspect.NodeResolution{
				Element: node.Element, Implementation: node.Implementation,
				Runtime: inspect.ArtifactIdentity{
					ID: "runtime://" + node.ID, Revision: "build:1",
				},
				RuntimeEvidence:      inspect.EvidenceRegistered,
				CapabilitiesEvidence: inspect.EvidenceLive,
			},
		}
	}
	for _, edge := range graph.Edges {
		live.Edges[edge.ID] = inspect.EdgeLive{LastItemID: secret}
	}
	for _, boundary := range graph.Boundaries {
		live.Edges["boundary:"+boundary.Name] = inspect.EdgeLive{LastItemID: secret}
	}
	return live
}

func withNodeTiming(node inspect.NodeLive, firstOutput uint64) inspect.NodeLive {
	if firstOutput > 3 {
		node.FirstTriggerNS = firstOutput - 3
	}
	node.FirstOutputNS = firstOutput
	return node
}

func traceNode(nodes []inspect.TraceNodeLive, id string) inspect.TraceNodeLive {
	for _, node := range nodes {
		if node.Node == id {
			return node
		}
	}
	panic(fmt.Sprintf("missing trace node %s", id))
}

func traceEdge(edges []inspect.TraceEdgeLive, id string) *inspect.TraceEdgeLive {
	for index := range edges {
		if edges[index].Edge == id {
			return &edges[index]
		}
	}
	panic(fmt.Sprintf("missing trace edge %s", id))
}
