package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TestRecordedTraceIsExactPayloadFreeReplayableAndDeterministic(t *testing.T) {
	key := bytes.Repeat([]byte{0x31}, 32)
	mounted, graph, configuration, now := mountRecordedPassChain(t, key, 8, 4)
	runDone := runRecordedGraph(t, mounted)

	const private = "private spoken words and tool arguments"
	message := element.Envelope{
		Type:   element.Event(element.Named("test.Value")),
		ItemID: "item:" + private, TraceID: "trace:" + private,
		RunID: "run:" + private, CausalParents: []string{"observation:" + private}, Payload: private,
	}
	sendRecordedMessage(t, mounted, message)
	if err := mounted.CheckpointTrace(); err != nil {
		t.Fatal(err)
	}
	closeRecordedGraph(t, mounted, runDone)

	first, err := mounted.MarshalRecordedTrace()
	if err != nil {
		t.Fatal(err)
	}
	second, err := mounted.MarshalRecordedTrace()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("sealed trace export is nondeterministic")
	}
	for _, forbidden := range []string{
		private, `"item_id"`, `"trace_id"`, `"run_id"`, `"payload"`,
		`"arguments"`, `"error"`,
	} {
		if bytes.Contains(first, []byte(forbidden)) {
			t.Fatalf("recorded trace contains forbidden data %q:\n%s", forbidden, first)
		}
	}
	trace, err := inspect.ParseLiveTrace(first)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Graph.Fingerprint != graph.Fingerprint || trace.Configuration != configuration {
		t.Fatalf("trace identity = %+v / %+v", trace.Graph, trace.Configuration)
	}
	replayer, err := inspect.NewTraceReplayer(graph, trace)
	if err != nil {
		t.Fatal(err)
	}
	final, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if final.Live.State != inspect.TraceGraphClosed || len(final.Flows) != 1 {
		t.Fatalf("final overlay = %+v", final)
	}
	if final.Edges["first-to-second"].Enqueued != 1 ||
		final.Edges["first-to-second"].Dequeued != 1 ||
		final.Edges["first-to-second"].QueueWaitNS == 0 {
		t.Fatalf("actual queue metrics were not recorded: %+v", final.Edges["first-to-second"])
	}
	for _, nodeID := range []string{"first", "second"} {
		node := final.Nodes[nodeID]
		if node.ActiveRuns != 0 || node.FirstTriggerNS == 0 ||
			node.FirstOutputNS < node.FirstTriggerNS || node.CompletionNS < node.FirstOutputNS ||
			node.CancellationNS != 0 {
			t.Fatalf("node %s reaction timing was not recorded and replayed: %+v", nodeID, node)
		}
	}
	rawCorrelation := inspect.OpaqueTraceCorrelation("trace:" + message.TraceID)
	for correlation, flow := range final.Flows {
		if correlation == rawCorrelation || !strings.HasPrefix(correlation, "sha256:") {
			t.Fatalf("flow correlation was not session-key pseudonymized: %q", correlation)
		}
		if len(flow.EdgeNS) != len(flow.Edges) || len(flow.EdgeNS) == 0 ||
			flow.EdgeNS[0] < flow.FirstNS || flow.EdgeNS[len(flow.EdgeNS)-1] > flow.LastNS {
			t.Fatalf("flow stage timing was not recorded and replayed: %+v", flow)
		}
		if len(flow.CausalStages) != len(flow.Edges) || len(flow.CausalStages) != 1 ||
			len(flow.CausalStages[0].Parents) != 1 ||
			!strings.HasPrefix(flow.CausalStages[0].Item, "sha256:") ||
			!strings.HasPrefix(flow.CausalStages[0].Parents[0], "sha256:") ||
			flow.CausalStages[0].Item == inspect.OpaqueTraceCausalIdentity(message.ItemID) ||
			flow.CausalStages[0].Parents[0] == inspect.OpaqueTraceCausalIdentity(message.CausalParents[0]) {
			t.Fatalf("causal lineage was not session-key pseudonymized and replayed: %+v", flow)
		}
	}
	if now.Load() == 0 {
		t.Fatal("recording did not use the injected monotonic clock")
	}
	if !errors.Is(mounted.CheckpointTrace(), graphruntime.ErrTraceRecordingClosed) {
		t.Fatal("sealed recorder accepted another checkpoint")
	}
}

func TestRecordedTraceCorrelationIsStableOnlyUnderTheInjectedSessionKey(t *testing.T) {
	firstFlow, firstCause := recordOneCorrelation(t, bytes.Repeat([]byte{0x41}, 32))
	againFlow, againCause := recordOneCorrelation(t, bytes.Repeat([]byte{0x41}, 32))
	otherFlow, otherCause := recordOneCorrelation(t, bytes.Repeat([]byte{0x42}, 32))
	if firstFlow != againFlow || firstCause != againCause {
		t.Fatalf("same explicit key produced unstable flow/causal identities: %q/%q != %q/%q",
			firstFlow, firstCause, againFlow, againCause)
	}
	if firstFlow == otherFlow || firstCause == otherCause {
		t.Fatalf("different per-session keys produced linkable identities %q/%q", firstFlow, firstCause)
	}
	if firstFlow == inspect.OpaqueTraceCorrelation("trace:shared-private-trace") ||
		firstCause == inspect.OpaqueTraceCausalIdentity("shared-item") {
		t.Fatal("artifact used an ambient unkeyed raw-correlation hash")
	}
}

func TestRecordedTraceClampsAndAttestsRegressingMonotonicClock(t *testing.T) {
	mounted, graph, _, now := mountRecordedPassChain(t, bytes.Repeat([]byte{0x49}, 32), 16, 4)
	runDone := runRecordedGraph(t, mounted)
	if err := mounted.CheckpointTrace(); err != nil {
		t.Fatal(err)
	}
	beforeTrace, err := mounted.RecordedTrace()
	if err != nil {
		t.Fatal(err)
	}
	beforeReplay, err := inspect.NewTraceReplayer(graph, beforeTrace)
	if err != nil {
		t.Fatal(err)
	}
	before, err := beforeReplay.Final()
	if err != nil {
		t.Fatal(err)
	}

	now.Store(1_000)
	sendRecordedMessage(t, mounted, element.Envelope{
		Type: element.Event(element.Named("test.Value")), ItemID: "clock-regression",
		RunID: "clock-regression", TraceID: "clock-regression", Payload: "value",
	})
	live := mounted.Live()
	minimumAtNS := uint64(0)
	for _, node := range live.Nodes {
		minimumAtNS = max(minimumAtNS, node.FirstTriggerNS, node.FirstOutputNS,
			node.CompletionNS, node.CancellationNS)
	}
	for _, flow := range live.Flows {
		minimumAtNS = max(minimumAtNS, flow.FirstNS, flow.LastNS)
	}
	if minimumAtNS <= before.AtNS {
		t.Fatalf("new live evidence did not advance past the prior checkpoint: live=%+v before=%+v", live, before)
	}
	now.Store(0)
	if err := mounted.CheckpointTrace(); err != nil {
		t.Fatal(err)
	}
	afterTrace, err := mounted.RecordedTrace()
	if err != nil {
		t.Fatal(err)
	}
	afterReplay, err := inspect.NewTraceReplayer(graph, afterTrace)
	if err != nil {
		t.Fatal(err)
	}
	after, err := afterReplay.Final()
	if err != nil {
		t.Fatal(err)
	}
	if after.AtNS < minimumAtNS || after.Live.TraceDropped <= before.Live.TraceDropped {
		t.Fatalf("clock regression was not clamped and attested: before=%+v after=%+v",
			before, after)
	}
	closeRecordedGraph(t, mounted, runDone)
}

func TestRecordedTraceCompactsWithinEventAndFlowBounds(t *testing.T) {
	key := bytes.Repeat([]byte{0x51}, 32)
	mounted, graph, _, _ := mountRecordedPassChain(t, key, 1, 2)
	runDone := runRecordedGraph(t, mounted)
	for index := 0; index < 8; index++ {
		message := element.Envelope{
			Type:   element.Event(element.Named("test.Value")),
			ItemID: fmt.Sprintf("item-%d", index), TraceID: fmt.Sprintf("trace-%d", index),
			Payload: strings.Repeat("private", 128),
		}
		sendRecordedMessage(t, mounted, message)
		if err := mounted.CheckpointTrace(); err != nil {
			t.Fatal(err)
		}
	}
	closeRecordedGraph(t, mounted, runDone)
	trace, err := mounted.RecordedTrace()
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Snapshots) != 1 || len(trace.Events) > 1 {
		t.Fatalf("retention escaped bounds: snapshots=%d events=%d", len(trace.Snapshots), len(trace.Events))
	}
	replayer, err := inspect.NewTraceReplayer(graph, trace)
	if err != nil {
		t.Fatal(err)
	}
	final, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if final.Live.TraceDropped == 0 || len(final.Flows) > 2 ||
		final.Edges["first-to-second"].Enqueued != 8 {
		t.Fatalf("compacted final overlay = %+v", final)
	}
}

func TestRecordedTraceConcurrentCheckpointAndTrafficIsRaceFree(t *testing.T) {
	key := bytes.Repeat([]byte{0x61}, 32)
	mounted, graph, _, _ := mountRecordedPassChain(t, key, 64, 16)
	runDone := runRecordedGraph(t, mounted)
	waitForLiveResolution(t, mounted)

	const messages = 32
	errorsFound := make(chan error, 256)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		egress, err := mounted.Egress("output")
		if err != nil {
			errorsFound <- err
			return
		}
		for index := 0; index < messages; index++ {
			if _, err := egress.Receive(context.Background()); err != nil {
				errorsFound <- err
				return
			}
		}
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		ingress, err := mounted.Ingress("input")
		if err != nil {
			errorsFound <- err
			return
		}
		for index := 0; index < messages; index++ {
			envelope := element.Envelope{
				Type:    element.Event(element.Named("test.Value")),
				ItemID:  fmt.Sprintf("concurrent-item-%d", index),
				TraceID: fmt.Sprintf("concurrent-trace-%d", index), Payload: []byte("private"),
			}
			if _, err := ingress.Broadcast(context.Background(), envelope); err != nil {
				errorsFound <- err
				return
			}
		}
	}()
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 16; iteration++ {
				trace, err := mounted.RecordedTrace()
				if err != nil {
					errorsFound <- err
					return
				}
				if _, err := inspect.NewTraceReplayer(graph, trace); err != nil {
					errorsFound <- err
					return
				}
			}
		}()
	}
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	closeRecordedGraph(t, mounted, runDone)
	if _, err := mounted.RecordedTrace(); err != nil {
		t.Fatal(err)
	}
}

func TestTraceRecordingConfigurationIsExplicitAndBounded(t *testing.T) {
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	if err := registry.Register("", resolvingPassFactory{
		passFactory: passFactory{descriptor: descriptor},
	}); err != nil {
		t.Fatal(err)
	}
	graph := passChainGraph(t, descriptor)
	validIdentity := recordedConfiguration()
	validKey := bytes.Repeat([]byte{0x71}, 32)
	tests := []struct {
		name          string
		configuration *inspect.ArtifactIdentity
		recording     graphruntime.TraceRecordingConfig
		inspection    graphruntime.InspectionConfig
		want          string
	}{
		{
			name: "missing configuration", recording: recordedConfig(validKey, 8, 4),
			inspection: recordedInspection(4), want: "configuration artifact",
		},
		{
			name: "short session key", configuration: &validIdentity,
			recording:  recordedConfig([]byte("short"), 8, 4),
			inspection: recordedInspection(4), want: "session correlation key",
		},
		{
			name: "noncanonical digest", configuration: func() *inspect.ArtifactIdentity {
				copy := validIdentity
				copy.Digest = "sha256:" + strings.Repeat("A", 64)
				return &copy
			}(),
			recording:  recordedConfig(validKey, 8, 4),
			inspection: recordedInspection(4), want: "canonical SHA-256",
		},
		{
			name: "unbounded bytes", configuration: &validIdentity,
			recording: func() graphruntime.TraceRecordingConfig {
				value := recordedConfig(validKey, 8, 4)
				value.MaxRetainedBytes = 1 << 30
				return value
			}(),
			inspection: recordedInspection(4), want: "max retained bytes",
		},
		{
			name: "trace flow bound below live retention", configuration: &validIdentity,
			recording:  recordedConfig(validKey, 8, 1),
			inspection: recordedInspection(2), want: "smaller than live inspection",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: graph, Registry: registry, Configuration: test.configuration,
				Inspection: test.inspection, TraceRecording: &test.recording,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("mount error = %v, want %q", err, test.want)
			}
		})
	}

	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mounted.RecordedTrace(); !errors.Is(err, graphruntime.ErrTraceRecordingDisabled) {
		t.Fatalf("disabled trace export = %v", err)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func recordOneCorrelation(t *testing.T, key []byte) (string, string) {
	t.Helper()
	mounted, _, _, _ := mountRecordedPassChain(t, key, 8, 4)
	runDone := runRecordedGraph(t, mounted)
	sendRecordedMessage(t, mounted, element.Envelope{
		Type: element.Event(element.Named("test.Value")), ItemID: "shared-item",
		TraceID: "shared-private-trace", CausalParents: []string{"shared-parent"}, Payload: "private",
	})
	if err := mounted.CheckpointTrace(); err != nil {
		t.Fatal(err)
	}
	closeRecordedGraph(t, mounted, runDone)
	trace, err := mounted.RecordedTrace()
	if err != nil {
		t.Fatal(err)
	}
	replayer, err := inspect.NewTraceReplayer(mounted.Graph(), trace)
	if err != nil {
		t.Fatal(err)
	}
	final, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Flows) != 1 {
		t.Fatalf("final flows = %+v", final.Flows)
	}
	for correlation, flow := range final.Flows {
		if len(flow.CausalStages) != 1 {
			t.Fatalf("final causal stages = %+v", flow.CausalStages)
		}
		return correlation, flow.CausalStages[0].Item
	}
	panic("unreachable")
}

func mountRecordedPassChain(
	t *testing.T, key []byte, maxEvents, maxFlows uint32,
) (*graphruntime.Mounted, ir.Graph, inspect.ArtifactIdentity, *atomic.Uint64) {
	t.Helper()
	descriptor := passDescriptor(nil)
	registry := graphruntime.NewRegistry()
	registered := inspect.ArtifactIdentity{ID: "image://pass", Revision: "build:recorded"}
	if err := registry.RegisterArtifact("", registered, resolvingPassFactory{
		passFactory: passFactory{descriptor: descriptor},
	}); err != nil {
		t.Fatal(err)
	}
	graph := passChainGraph(t, descriptor)
	configuration := recordedConfiguration()
	now := &atomic.Uint64{}
	recording := recordedConfig(key, maxEvents, maxFlows)
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Configuration: &configuration,
		Inspection: recordedInspection(int(maxFlows)), TraceRecording: &recording,
		Now: func() uint64 { return now.Add(10) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return mounted, graph, configuration, now
}

func recordedConfiguration() inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: "values://recorded-pass-chain", Revision: "config:recorded",
		Digest: "sha256:" + strings.Repeat("8", 64),
	}
}

func recordedConfig(key []byte, maxEvents, maxFlows uint32) graphruntime.TraceRecordingConfig {
	limits := inspect.DefaultTraceLimits()
	limits.MaxEvents = maxEvents
	limits.MaxSnapshots = 2
	limits.MaxNodes = 4
	limits.MaxEdges = 8
	limits.MaxFlows = maxFlows
	limits.MaxEdgesPerFlow = 8
	limits.MaxCapabilitiesPerNode = 8
	return graphruntime.TraceRecordingConfig{
		Limits: limits, SessionCorrelationKey: key,
		MaxRetainedBytes: 64 << 10, CaptureInterval: time.Minute,
	}
}

func recordedInspection(maxFlows int) graphruntime.InspectionConfig {
	return graphruntime.InspectionConfig{
		MaxFlows: maxFlows, MaxEdgesPerFlow: 8, MaxCorrelationBytes: 1024,
	}
}

func runRecordedGraph(t *testing.T, mounted *graphruntime.Mounted) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- mounted.Run(context.Background()) }()
	waitForLiveResolution(t, mounted)
	return done
}

func waitForLiveResolution(t *testing.T, mounted *graphruntime.Mounted) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		live := mounted.Live()
		ready := len(live.Nodes) != 0
		for _, node := range live.Nodes {
			ready = ready && node.Resolution != nil &&
				node.Resolution.CapabilitiesEvidence == inspect.EvidenceLive
		}
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("graph nodes did not report live capability resolution")
}

func sendRecordedMessage(t *testing.T, mounted *graphruntime.Mounted, message element.Envelope) {
	t.Helper()
	ingress, err := mounted.Ingress("input")
	if err != nil {
		t.Fatal(err)
	}
	egress, err := mounted.Egress("output")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := ingress.Broadcast(context.Background(), message); err != nil || result.Delivered != 1 {
		t.Fatalf("recorded ingress = %+v, %v", result, err)
	}
	if _, err := egress.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func closeRecordedGraph(t *testing.T, mounted *graphruntime.Mounted, runDone <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mounted.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("recorded graph did not shut down")
	}
}
