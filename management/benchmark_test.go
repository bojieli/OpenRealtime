package management

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

func BenchmarkCapabilityAuthorize(b *testing.B) {
	registry := NewCapabilityRegistry()
	access, err := registry.Issue(time.Hour, []Grant{{Operation: ReadSession, Resource: "sess-bench"}})
	if err != nil {
		b.Fatal(err)
	}
	request := AuthorizationRequest{
		Capability: access.Token, Operation: ReadSession, Resource: "sess-bench",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := registry.Authorize(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRedactLive64(b *testing.B) {
	benchmarkRedaction(b, RedactLive)
}

func benchmarkRedaction(b *testing.B, redact func(inspect.Live) inspect.Live) {
	live := inspect.Live{
		FormatVersion: inspect.LiveFormatVersion, GraphID: "bench", GraphRevision: 1,
		Fingerprint: "sha256:" + strings.Repeat("a", 64), State: "running",
		Nodes: make(map[string]inspect.NodeLive, 64), Edges: make(map[string]inspect.EdgeLive, 64),
		Flows: make(map[string]inspect.FlowLive, 64),
	}
	for index := range 64 {
		id := string(rune('a'+index%26)) + strings.Repeat("x", index/26+1)
		live.Nodes[id] = inspect.NodeLive{
			LastTriggerID: "private", LastOutcome: "private", Error: "private",
			AuthorityDecision: &inspect.AuthorityDecisionLive{
				Kind: "succeeded", Operation: "authorize", Crossed: true, AtNS: 2,
			},
		}
		live.Edges[id] = inspect.EdgeLive{LastItemID: "private"}
		live.Flows[id] = inspect.FlowLive{
			Correlation: id, Edges: []string{id, id}, EdgeNS: []uint64{1, 2},
			CausalStages: []inspect.CausalStageLive{
				{Item: id + "-output", Parents: []string{id + "-observation", id + "-state"}},
				{Item: id + "-result", Parents: []string{id + "-output"}},
			},
			FirstNS: 1, LastNS: 2,
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result := redact(live)
		if len(result.Nodes) != 64 || len(result.Flows["flow_000001"].EdgeNS) != 2 ||
			len(result.Flows["flow_000001"].CausalStages) != 2 ||
			result.Nodes["ax"].AuthorityDecision == nil {
			b.Fatal("redaction lost nodes")
		}
	}
}

func BenchmarkAuthoringAnalyzeRecovery(b *testing.B) {
	engine, err := NewAuthoringEngine(AuthoringOptions{Catalog: managedElementCatalog(b)})
	if err != nil {
		b.Fatal(err)
	}
	document := AuthoringDocument{Path: "partial.ortg", Source: `graph managed {
    test.ManagedSource :: source;
    test.ManagedSink :: sink;
    source.out ->
`}
	b.ReportAllocs()
	for range b.N {
		result, err := engine.Analyze(context.Background(), document)
		if err != nil || !result.Recovered || result.Parsed {
			b.Fatalf("recovery analysis = %+v, %v", result, err)
		}
	}
}
