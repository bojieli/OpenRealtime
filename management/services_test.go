package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/plugin"
)

const managedSource = `graph managed {
    test.ManagedSource :: source;
    test.ManagedSink :: sink;
    source.out -> sink.in;
}
`

type managedSchemaResolver func(context.Context, string) (schema.ResolvedSchema, error)

func (resolver managedSchemaResolver) ResolveConfigSchema(ctx context.Context, reference string) (schema.ResolvedSchema, error) {
	return resolver(ctx, reference)
}

func TestAuthoringEngineCompilesLocksAnalyzesAndRendersOneSemanticGraph(t *testing.T) {
	elements := managedElementCatalog(t)
	engine, err := NewAuthoringEngine(AuthoringOptions{Catalog: elements})
	if err != nil {
		t.Fatal(err)
	}
	document := AuthoringDocument{Path: "agent.ortg", Source: managedSource, Revision: 7}
	analysis, err := engine.Analyze(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.Parsed || !analysis.Canonical || analysis.SourceDigest == "" ||
		analysis.Diagnostics.Total != 0 || analysis.Catalog.Total != 2 {
		t.Fatalf("analysis = %+v", analysis)
	}
	generated, err := engine.Compile(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	if generated.Graph.Revision != 7 || len(generated.Lock.Entries) != 2 {
		t.Fatalf("generated compile = graph %+v lock %+v", generated.Graph, generated.Lock)
	}
	lockedDocument := document
	lockedDocument.Lock = &generated.Lock
	locked, err := engine.Compile(context.Background(), lockedDocument)
	if err != nil {
		t.Fatal(err)
	}
	if locked.Graph.Fingerprint != generated.Graph.Fingerprint || !locked.Lock.Equal(generated.Lock) {
		t.Fatalf("locked compile drifted: %+v %+v", locked.Graph, locked.Lock)
	}
	for _, format := range []RenderFormat{RenderModel, RenderMermaid, RenderDOT} {
		rendered, err := engine.Render(context.Background(), RenderRequest{Graph: locked.Graph, Format: format})
		if err != nil {
			t.Fatalf("render %s: %v", format, err)
		}
		if rendered.Fingerprint != locked.Graph.Fingerprint || rendered.Format != format {
			t.Fatalf("render %s lost identity: %+v", format, rendered)
		}
		if format == RenderModel && rendered.Model == nil {
			t.Fatal("model rendering has no model")
		}
		if format != RenderModel && !strings.Contains(rendered.Text, locked.Graph.Fingerprint) {
			t.Fatalf("render %s omitted fingerprint: %s", format, rendered.Text)
		}
	}

	stale := generated.Lock
	stale.Entries[0].Identity.Digest = "sha256:" + strings.Repeat("0", 64)
	lockedDocument.Lock = &stale
	if _, err := engine.Compile(context.Background(), lockedDocument); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale lock error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.Analyze(canceled, document); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("canceled analysis error = %v", err)
	}
}

func TestAuthoringRecoveryFormattingAndResolvedPropertiesNeverCrossCompileBoundary(t *testing.T) {
	base := managedElementCatalog(t)
	configured := resolve.NewCatalog()
	for _, name := range base.Names() {
		descriptor, found := base.Latest(name)
		if !found {
			t.Fatalf("missing descriptor %s", name)
		}
		if name == "test.ManagedSource" {
			descriptor.ConfigSchema = "schema://test/managed-source/v1"
		}
		if err := configured.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	resolver := managedSchemaResolver(func(_ context.Context, reference string) (schema.ResolvedSchema, error) {
		if reference != "schema://test/managed-source/v1" {
			return schema.ResolvedSchema{}, schema.ErrSchemaNotFound
		}
		return schema.ResolvedSchema{
			ID: "https://schemas.example.test/managed-source-v1.json",
			Document: json.RawMessage(`{
                "$id":"https://schemas.example.test/managed-source-v1.json",
                "type":"object","required":["model"],
                "properties":{"model":{"type":"string"}},"additionalProperties":false
            }`),
		}, nil
	})
	engine, err := NewAuthoringEngine(AuthoringOptions{Catalog: configured, SchemaResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	partial := AuthoringDocument{Path: "partial.ortg", Source: `graph managed {
    test.ManagedSource :: source;
    test.ManagedSink :: sink;
    source.out ->
`}
	analysis, err := engine.Analyze(context.Background(), partial)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Parsed || !analysis.Recovered || analysis.Canonical || analysis.Formatting != nil ||
		analysis.Diagnostics.Total == 0 {
		t.Fatalf("recovery analysis = %+v", analysis)
	}
	var sourceMetadata *editor.ElementMetadata
	for index := range analysis.Catalog.Elements {
		if analysis.Catalog.Elements[index].Identity.Name == "test.ManagedSource" {
			sourceMetadata = &analysis.Catalog.Elements[index]
		}
	}
	if sourceMetadata == nil || sourceMetadata.Config.SchemaStatus != editor.ConfigSchemaResolved ||
		len(sourceMetadata.Config.Properties) != 1 || sourceMetadata.Config.Properties[0].Name != "model" {
		t.Fatalf("resolved authoring metadata = %+v", sourceMetadata)
	}
	if err := ValidateAnalysisResult(partial, analysis); err != nil {
		t.Fatalf("validate recovery analysis: %v", err)
	}
	if compiled, err := engine.Compile(context.Background(), partial); !errors.Is(err, ErrInvalid) ||
		compiled.Graph.Fingerprint != "" {
		t.Fatalf("recovery source crossed strict compile boundary: %+v, %v", compiled, err)
	}
	trustedCompile, err := engine.Compile(context.Background(), AuthoringDocument{
		Path: "agent.ortg", Source: managedSource,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompileResult(partial, trustedCompile); !errors.Is(err, ErrConflict) {
		t.Fatalf("provider compile result bypassed strict recovery boundary: %v", err)
	}

	unformatted := AuthoringDocument{
		Path: "format.ortg", Source: strings.Replace(managedSource, "test.ManagedSource ::", "test.ManagedSource  ::", 1),
	}
	formattedAnalysis, err := engine.Analyze(context.Background(), unformatted)
	if err != nil {
		t.Fatal(err)
	}
	if !formattedAnalysis.Parsed || formattedAnalysis.Canonical || formattedAnalysis.Recovered ||
		formattedAnalysis.Formatting == nil || len(formattedAnalysis.Formatting.Edits) != 1 {
		t.Fatalf("formatting analysis = %+v", formattedAnalysis)
	}
	formatted, err := editor.ApplyEdits([]byte(unformatted.Source), *formattedAnalysis.Formatting)
	if err != nil || string(formatted) != managedSource {
		t.Fatalf("management formatting edits = %q, %v", formatted, err)
	}
}

func TestAuthoringEverySourcePrefixIsDeterministicAndRecoveryNeverCompiles(t *testing.T) {
	engine, err := NewAuthoringEngine(AuthoringOptions{Catalog: managedElementCatalog(t)})
	if err != nil {
		t.Fatal(err)
	}
	for length := 1; length <= len(managedSource); length++ {
		document := AuthoringDocument{Path: "prefix.ortg", Source: managedSource[:length]}
		first, err := engine.Analyze(context.Background(), document)
		if err != nil {
			t.Fatalf("analyze prefix %d: %v", length, err)
		}
		if err := ValidateAnalysisResult(document, first); err != nil {
			t.Fatalf("validate prefix %d: %v (result %+v)", length, err, first)
		}
		second, err := engine.Analyze(context.Background(), document)
		if err != nil {
			t.Fatalf("reanalyze prefix %d: %v", length, err)
		}
		left, _ := json.Marshal(first)
		right, _ := json.Marshal(second)
		if !bytes.Equal(left, right) {
			t.Fatalf("prefix %d changed between snapshots:\n%s\n%s", length, left, right)
		}
		compiled, compileErr := engine.Compile(context.Background(), document)
		if first.Recovered {
			if !errors.Is(compileErr, ErrInvalid) || compiled.Graph.Fingerprint != "" {
				t.Fatalf("recovered prefix %d crossed compile boundary: %+v, %v", length, compiled, compileErr)
			}
			continue
		}
		if !first.Parsed || compileErr != nil {
			t.Fatalf("strict prefix %d analysis/compile disagree: %+v, %v", length, first, compileErr)
		}
	}
}

func TestAuthoringValidationRejectsForgedSnapshotIdentityAndMetadata(t *testing.T) {
	engine, err := NewAuthoringEngine(AuthoringOptions{Catalog: managedElementCatalog(t)})
	if err != nil {
		t.Fatal(err)
	}
	document := AuthoringDocument{Path: "agent.ortg", Source: managedSource}
	analysis, err := engine.Analyze(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	clone := func(value AnalysisResult) AnalysisResult {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var result AnalysisResult
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	wrongDigest := clone(analysis)
	wrongDigest.SourceDigest = "sha256:" + strings.Repeat("0", 64)
	if err := ValidateAnalysisResult(document, wrongDigest); !errors.Is(err, ErrConflict) {
		t.Fatalf("forged source identity error = %v", err)
	}
	forgedRecovery := clone(analysis)
	forgedRecovery.Parsed, forgedRecovery.Recovered, forgedRecovery.Canonical = false, true, false
	forgedRecovery.Formatting = nil
	if err := ValidateAnalysisResult(document, forgedRecovery); !errors.Is(err, ErrConflict) {
		t.Fatalf("recovery without syntax evidence error = %v", err)
	}
	forgedFormatting := clone(analysis)
	forgedFormatting.Canonical = false
	forgedFormatting.Formatting.Edits = []editor.TextEdit{{
		Span: syntax.Span{
			Start: syntax.Position{Line: 1, Column: 1},
			End:   syntax.Position{Offset: len(managedSource), Line: 6, Column: 1},
		},
		OldText: managedSource, NewText: "graph forged {}\n",
	}}
	if err := ValidateAnalysisResult(document, forgedFormatting); !errors.Is(err, ErrConflict) {
		t.Fatalf("forged formatter output error = %v", err)
	}

	contract := editor.ConfigContract{
		Artifact: "openrealtime.ai/config/v1alpha1", Resolved: true,
		SchemaReference: "schema://test/unresolved", SchemaStatus: editor.ConfigSchemaUnresolved,
		Properties: []editor.ValuesPropertyMetadata{}, AdditionalProperties: json.RawMessage("false"),
	}
	if err := validateConfigMetadata(contract, new(int)); err == nil {
		t.Fatal("unresolved schema invented additionalProperties metadata")
	}
}

func TestStaticCatalogReturnsIndependentExactResources(t *testing.T) {
	elements := managedElementCatalog(t)
	graph := compileManagedGraph(t, elements)
	plugins := plugin.NewCatalog()
	pluginDescriptor := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "test.management.Plugin", Revision: 1,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{{
			Name: "test.management.service", Revision: 1,
			Digest: "sha256:" + strings.Repeat("1", 64),
		}},
	}
	pluginIdentity, err := plugins.Register(pluginDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	values, err := schema.Generate(context.Background(), graph, elements, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewCatalog(elements, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.RegisterGraph(graph, values); err != nil {
		t.Fatal(err)
	}
	if err := catalog.RegisterGraph(graph, values); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate registration = %v", err)
	}
	first, err := catalog.Graph(context.Background(), graph.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	first.Nodes[0].ID = "forged"
	second, err := catalog.Graph(context.Background(), graph.Fingerprint)
	if err != nil || second.Nodes[0].ID == "forged" {
		t.Fatalf("graph catalog aliased a read: err=%v graph=%+v", err, second)
	}
	elementDescriptor, err := catalog.ElementDescriptor(context.Background(), graph.Nodes[0].Element)
	if err != nil {
		t.Fatal(err)
	}
	elementDescriptor.Ports[0].Name = "forged"
	again, err := catalog.ElementDescriptor(context.Background(), graph.Nodes[0].Element)
	if err != nil || again.Ports[0].Name == "forged" {
		t.Fatalf("element catalog aliased a read: err=%v descriptor=%+v", err, again)
	}
	resolvedPlugin, err := catalog.PluginDescriptor(context.Background(), pluginIdentity)
	if err != nil || resolvedPlugin.Name != pluginDescriptor.Name {
		t.Fatalf("plugin resource = %+v, %v", resolvedPlugin, err)
	}
	firstSchema, err := catalog.ValuesSchema(context.Background(), graph.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	firstSchema.Schema[0] = 'x'
	secondSchema, err := catalog.ValuesSchema(context.Background(), graph.Fingerprint)
	if err != nil || len(secondSchema.Schema) == 0 || secondSchema.Schema[0] == 'x' {
		t.Fatalf("schema catalog aliased a read: err=%v", err)
	}
	if _, err := catalog.Graph(context.Background(), "sha256:"+strings.Repeat("f", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown graph error = %v", err)
	}
}

func TestValuesSchemaResourcePreservesExactArtifactBytesAcrossJSON(t *testing.T) {
	elements := managedElementCatalog(t)
	graph := compileManagedGraph(t, elements)
	values, err := schema.Generate(context.Background(), graph, elements, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values.Schema) == 0 || values.Schema[len(values.Schema)-1] != '\n' {
		t.Fatal("fixture schema is not the canonical newline-terminated artifact")
	}
	resource, err := NewValuesSchemaResource(values)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ValuesSchemaResource
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := decoded.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	if string(roundTrip.Schema) != string(values.Schema) || roundTrip.Digest != values.Digest {
		t.Fatalf("schema resource changed exact bytes or identity: got %q/%s, want %q/%s",
			roundTrip.Schema, roundTrip.Digest, values.Schema, values.Digest)
	}
	decoded.Schema = strings.TrimSuffix(decoded.Schema, "\n")
	if _, err := decoded.Bundle(); !errors.Is(err, ErrConflict) {
		t.Fatalf("tampered schema resource error = %v", err)
	}
}

type recordedSession struct {
	graph ir.Graph
	live  inspect.Live
	trace inspect.LiveTrace
}

func (session *recordedSession) Live() inspect.Live { return session.live.Clone() }

func (session *recordedSession) Graph() ir.Graph { return session.graph }

func (session *recordedSession) RecordedTrace() (inspect.LiveTrace, error) {
	return session.trace.Clone(), nil
}

func TestSessionRegistryProvidesBoundedResumablePagesAndOwnerSafeDisposal(t *testing.T) {
	elements := managedElementCatalog(t)
	graph := compileManagedGraph(t, elements)
	live, trace := managedLiveTrace(t, graph)
	live.Error = "private prompt fragment"
	node := live.Nodes["source"]
	node.LastTriggerID = "item-private"
	node.LastOutcome = "model text"
	node.Error = "private failure"
	node.FirstTriggerNS = 10
	node.FirstOutputNS = 20
	node.AuthorityDecision = &inspect.AuthorityDecisionLive{
		Kind: "succeeded", Operation: "authorize", Crossed: true, AtNS: 25,
	}
	live.Nodes["source"] = node
	edge := live.Edges[graph.Edges[0].ID]
	edge.LastItemID = "item-private"
	live.Edges[graph.Edges[0].ID] = edge
	live.Flows["raw-correlation"] = inspect.FlowLive{
		Correlation: "raw-correlation", Edges: []string{graph.Edges[0].ID},
		CausalStages: []inspect.CausalStageLive{{
			Item: "model-output-private", Parents: []string{"observation-private", "state-private"},
			Kind: element.CauseModelRun,
		}},
		EdgeNS: []uint64{30}, FirstNS: 30, LastNS: 30,
	}
	registry := NewSessionRegistry()
	source := &recordedSession{graph: graph, live: live, trace: trace}
	dispose, err := registry.Register("sess-one", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register("sess-one", source); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate session registration = %v", err)
	}
	snapshot, err := registry.Snapshot(context.Background(), "sess-one")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Error != "redacted" || snapshot.Nodes["source"].LastTriggerID != "" ||
		snapshot.Nodes["source"].LastOutcome != "" || snapshot.Nodes["source"].Error != "redacted" ||
		snapshot.Nodes["source"].FirstTriggerNS != 10 || snapshot.Nodes["source"].FirstOutputNS != 20 ||
		snapshot.Edges[graph.Edges[0].ID].LastItemID != "" || snapshot.Flows["flow_000001"].Correlation != "flow_000001" {
		t.Fatalf("snapshot was not payload-redacted: %+v", snapshot)
	}
	if timing := snapshot.Flows["flow_000001"].EdgeNS; len(timing) != 1 || timing[0] != 30 {
		t.Fatalf("snapshot omitted redacted flow-stage timing: %+v", snapshot.Flows)
	}
	causal := snapshot.Flows["flow_000001"].CausalStages
	if len(causal) != 1 || causal[0].Item != "cause_000001" ||
		causal[0].Kind != element.CauseModelRun ||
		!slices.Equal(causal[0].Parents, []string{"cause_000002", "cause_000003"}) {
		t.Fatalf("snapshot omitted redacted causal lineage: %+v", causal)
	}
	encodedSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"model-output-private", "observation-private", "state-private"} {
		if bytes.Contains(encodedSnapshot, []byte(private)) {
			t.Fatalf("redacted snapshot leaked causal identity %q: %s", private, encodedSnapshot)
		}
	}
	twice, err := json.Marshal(RedactLive(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodedSnapshot, twice) {
		t.Fatalf("causal redaction is not idempotent:\n%s\n%s", encodedSnapshot, twice)
	}
	if decision := snapshot.Nodes["source"].AuthorityDecision; decision == nil ||
		decision.Kind != "succeeded" || decision.Operation != "authorize" ||
		!decision.Crossed || decision.AtNS != 25 {
		t.Fatalf("snapshot omitted payload-free authority decision: %+v", decision)
	}
	snapshotNode := snapshot.Nodes["source"]
	snapshotNode.AuthorityDecision.Operation = "cancel"
	snapshot.Flows["flow_000001"].CausalStages[0].Parents[0] = "mutated"
	snapshot.Nodes["source"] = inspect.NodeLive{}
	second, err := registry.Snapshot(context.Background(), "sess-one")
	if err != nil || second.Nodes["source"].Resolution == nil ||
		second.Nodes["source"].AuthorityDecision.Operation == "cancel" ||
		second.Flows["flow_000001"].CausalStages[0].Parents[0] == "mutated" {
		t.Fatalf("snapshot aliased source: err=%v snapshot=%+v", err, second)
	}
	model, err := registry.Model(context.Background(), "sess-one")
	if err != nil || model.Fingerprint != graph.Fingerprint || len(model.Nodes) != len(live.Nodes) ||
		model.Nodes[0].Element != live.Nodes[model.Nodes[0].ID].Resolution.Element {
		t.Fatalf("session model = %+v, %v", model, err)
	}
	initial, err := registry.Deltas(context.Background(), "sess-one", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Baseline == nil || initial.Next != 6 || initial.Compacted || len(initial.Events) != 1 ||
		initial.Events[0].Sequence != 6 {
		t.Fatalf("initial delta page = %+v", initial)
	}
	compacted, err := registry.Deltas(context.Background(), "sess-one", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if compacted.Baseline == nil || !compacted.Compacted || compacted.Next != 6 {
		t.Fatalf("compacted delta page = %+v", compacted)
	}
	if _, err := registry.Deltas(context.Background(), "sess-one", 7, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future cursor error = %v", err)
	}
	dispose()
	if _, err := registry.Snapshot(context.Background(), "sess-one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disposed session error = %v", err)
	}
	replacement := &recordedSession{graph: graph, live: live, trace: trace}
	disposeReplacement, err := registry.Register("sess-one", replacement)
	if err != nil {
		t.Fatal(err)
	}
	// The old owner may finish cleanup after the public ID has been reused.
	// Its idempotent disposer must not unregister the replacement runtime.
	dispose()
	if _, err := registry.Snapshot(context.Background(), "sess-one"); err != nil {
		t.Fatalf("stale owner disposer removed replacement session: %v", err)
	}
	disposeReplacement()
}

func TestSessionModelRefusesStaticLiveIdentityDriftAndOmitsSource(t *testing.T) {
	elements := managedElementCatalog(t)
	graph := compileManagedGraph(t, elements)
	live, _ := managedLiveTrace(t, graph)
	model, err := inspect.Build(graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSessionModel(live, model); err != nil {
		t.Fatalf("exact session model = %v", err)
	}
	payload, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(`"source":`)) {
		t.Fatalf("session model retained authoring source: %s", payload)
	}
	mutations := map[string]func(*inspect.Live, *inspect.Model){
		"graph fingerprint": func(_ *inspect.Live, model *inspect.Model) {
			model.Fingerprint = "sha256:" + strings.Repeat("f", 64)
		},
		"missing static node": func(_ *inspect.Live, model *inspect.Model) {
			model.Nodes = model.Nodes[:len(model.Nodes)-1]
		},
		"static element": func(_ *inspect.Live, model *inspect.Model) {
			model.Nodes[0].Element.Digest = "sha256:" + strings.Repeat("e", 64)
		},
		"missing live node": func(live *inspect.Live, _ *inspect.Model) {
			delete(live.Nodes, model.Nodes[0].ID)
		},
		"live element": func(live *inspect.Live, _ *inspect.Model) {
			node := live.Nodes[model.Nodes[0].ID]
			node.Resolution.Element.Digest = "sha256:" + strings.Repeat("d", 64)
			live.Nodes[model.Nodes[0].ID] = node
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidateLive := live.Clone()
			candidateModel, buildErr := inspect.Build(graph)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			mutate(&candidateLive, &candidateModel)
			if err := ValidateSessionModel(candidateLive, candidateModel); !errors.Is(err, ErrConflict) {
				t.Fatalf("identity drift error = %v", err)
			}
		})
	}
}

func managedElementCatalog(t testing.TB) *resolve.Catalog {
	t.Helper()
	catalog := resolve.NewCatalog()
	value := element.Event(element.Named("test.ManagedValue"))
	for _, descriptor := range []element.Descriptor{
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.ManagedSource", Revision: 1,
			Ports: []element.Port{{
				Name: "out", Direction: element.Output, Type: value, Cardinality: element.One,
			}},
		},
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.ManagedSink", Revision: 1,
			Ports: []element.Port{{
				Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, Required: true,
			}},
		},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	return catalog
}

func compileManagedGraph(t *testing.T, catalog *resolve.Catalog) ir.Graph {
	t.Helper()
	file, err := syntax.Parse("agent.ortg", []byte(managedSource))
	if err != nil {
		t.Fatal(err)
	}
	result, err := graphcompiler.Compile(file, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Graph
}

func TestValidateSessionSnapshotRejectsMalformedDeploymentEvidence(t *testing.T) {
	graph := compileManagedGraph(t, managedElementCatalog(t))
	live, _ := managedLiveTrace(t, graph)
	live.Deployment = &inspect.DeploymentEvidence{}
	if err := ValidateSessionSnapshot(live); err == nil ||
		!strings.Contains(err.Error(), "invalid deployment evidence") {
		t.Fatalf("malformed deployment error = %v", err)
	}
	live.Deployment = &inspect.DeploymentEvidence{Public: inspect.ArtifactIdentity{
		ID: "deployment://managed", Revision: "1",
		Digest: "sha256:" + strings.Repeat("e", 64),
	}}
	if err := ValidateSessionSnapshot(live); err != nil {
		t.Fatalf("valid compatibility deployment evidence: %v", err)
	}
}

func TestValidateSessionSnapshotRejectsImpossibleTriggerRelativeTiming(t *testing.T) {
	graph := compileManagedGraph(t, managedElementCatalog(t))
	live, _ := managedLiveTrace(t, graph)
	node := live.Nodes["source"]
	node.FirstTriggerNS = 20
	node.FirstOutputNS = 19
	live.Nodes["source"] = node
	if err := ValidateSessionSnapshot(live); err == nil ||
		!strings.Contains(err.Error(), "impossible reaction timing") {
		t.Fatalf("impossible trigger-relative timing error = %v", err)
	}
}

func TestValidateSessionSnapshotRejectsInvalidAuthorityDecision(t *testing.T) {
	graph := compileManagedGraph(t, managedElementCatalog(t))
	for _, test := range []struct {
		name     string
		decision inspect.AuthorityDecisionLive
	}{
		{name: "unbounded kind", decision: inspect.AuthorityDecisionLive{
			Kind: "private-payload", Operation: "select", AtNS: 20,
		}},
		{name: "unbounded operation", decision: inspect.AuthorityDecisionLive{
			Kind: "succeeded", Operation: "private-payload", AtNS: 20,
		}},
		{name: "before output", decision: inspect.AuthorityDecisionLive{
			Kind: "succeeded", Operation: "select", AtNS: 19,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			live, _ := managedLiveTrace(t, graph)
			node := live.Nodes["source"]
			node.FirstTriggerNS = 10
			node.FirstOutputNS = 20
			node.AuthorityDecision = &test.decision
			live.Nodes["source"] = node
			if err := ValidateSessionSnapshot(live); err == nil ||
				!strings.Contains(err.Error(), "invalid authority-decision evidence") {
				t.Fatalf("invalid authority-decision error = %v", err)
			}
		})
	}
}

func TestValidateSessionSnapshotRejectsImpossibleQueueTelemetry(t *testing.T) {
	graph := compileManagedGraph(t, managedElementCatalog(t))
	live, _ := managedLiveTrace(t, graph)
	edge := live.Edges[graph.Edges[0].ID]
	edge.Occupancy = 1
	live.Edges[graph.Edges[0].ID] = edge
	if err := ValidateSessionSnapshot(live); err == nil ||
		!strings.Contains(err.Error(), "impossible queue telemetry") {
		t.Fatalf("impossible queue telemetry error = %v", err)
	}
}

func TestValidateSessionSnapshotRejectsInvalidCausalFlowTelemetry(t *testing.T) {
	graph := compileManagedGraph(t, managedElementCatalog(t))
	base, _ := managedLiveTrace(t, graph)
	edgeID := graph.Edges[0].ID
	base.Flows["raw-flow"] = inspect.FlowLive{
		Correlation: "raw-flow", Edges: []string{edgeID}, EdgeNS: []uint64{10},
		CausalStages: []inspect.CausalStageLive{{Item: "child", Parents: []string{"parent"}, Kind: element.CauseObservation}},
		FirstNS:      10, LastNS: 10,
	}
	if err := ValidateSessionSnapshot(base); err != nil {
		t.Fatalf("valid causal flow telemetry: %v", err)
	}
	tests := []struct {
		name   string
		want   string
		mutate func(*inspect.Live)
	}{
		{name: "correlation mismatch", want: "invalid flow telemetry", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"]
			flow.Correlation = "other"
			live.Flows["raw-flow"] = flow
		}},
		{name: "incomplete stages", want: "incomplete causal flow telemetry", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"]
			flow.Edges = append(flow.Edges, edgeID)
			flow.EdgeNS = append(flow.EdgeNS, 10)
			live.Flows["raw-flow"] = flow
		}},
		{name: "self parent", want: "invalid causal flow telemetry", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"]
			flow.CausalStages[0].Parents[0] = flow.CausalStages[0].Item
			live.Flows["raw-flow"] = flow
		}},
		{name: "duplicate parent", want: "repeated causal flow parent", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"]
			flow.CausalStages[0].Parents = []string{"parent", "parent"}
			live.Flows["raw-flow"] = flow
		}},
		{name: "too many parents", want: "invalid causal flow telemetry", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"]
			flow.CausalStages[0].Parents = make([]string, inspect.MaximumCausalParentsPerStage+1)
			live.Flows["raw-flow"] = flow
		}},
		{name: "invalid classification", want: "invalid causal flow classification", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"]
			flow.CausalStages[0].Kind = "private text"
			live.Flows["raw-flow"] = flow
		}},
		{name: "conflicting parents", want: "conflicting causal flow parents or classification", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"].Clone()
			flow.Correlation = "second-flow"
			flow.CausalStages[0].Parents[0] = "different-parent"
			live.Flows["second-flow"] = flow
		}},
		{name: "conflicting classification", want: "conflicting causal flow parents or classification", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"].Clone()
			flow.Correlation = "second-flow"
			flow.CausalStages[0].Kind = element.CausePolicy
			live.Flows["second-flow"] = flow
		}},
		{name: "regressing stage time", want: "invalid flow timing", mutate: func(live *inspect.Live) {
			flow := live.Flows["raw-flow"]
			flow.Edges = append(flow.Edges, edgeID)
			flow.EdgeNS = append(flow.EdgeNS, 9)
			flow.CausalStages = append(flow.CausalStages,
				inspect.CausalStageLive{Item: "next", Parents: []string{"child"}})
			live.Flows["raw-flow"] = flow
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base.Clone()
			test.mutate(&candidate)
			if err := ValidateSessionSnapshot(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("causal flow validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateInspectionModelRejectsInvalidChannelContract(t *testing.T) {
	graph := compileManagedGraph(t, managedElementCatalog(t))
	model, err := inspect.Build(graph)
	if err != nil {
		t.Fatal(err)
	}
	model.Edges[0].Depth = 0
	if err := ValidateInspectionModel(model); err == nil ||
		!strings.Contains(err.Error(), "invalid static edge") {
		t.Fatalf("invalid channel depth error = %v", err)
	}
	model, err = inspect.Build(graph)
	if err != nil {
		t.Fatal(err)
	}
	model.Edges[0].Delivery = "invented"
	if err := ValidateInspectionModel(model); err == nil ||
		!strings.Contains(err.Error(), "invalid static edge") {
		t.Fatalf("invalid delivery policy error = %v", err)
	}
}

func TestValidateSessionModelRequiresExactChannelPopulationAndDepth(t *testing.T) {
	graph := compileManagedGraph(t, managedElementCatalog(t))
	model, err := inspect.Build(graph)
	if err != nil {
		t.Fatal(err)
	}
	live, _ := managedLiveTrace(t, graph)
	if err := ValidateSessionModel(live, model); err != nil {
		t.Fatalf("valid exact channel population: %v", err)
	}
	missing := live.Clone()
	delete(missing.Edges, graph.Edges[0].ID)
	if err := ValidateSessionModel(missing, model); err == nil ||
		!strings.Contains(err.Error(), "edge populations differ") {
		t.Fatalf("missing live edge error = %v", err)
	}
	extra := live.Clone()
	extra.Edges[ir.BoundaryQueuePrefix+"invented"] = inspect.EdgeLive{}
	if err := ValidateSessionModel(extra, model); err == nil ||
		!strings.Contains(err.Error(), "edge populations differ") {
		t.Fatalf("extra live edge error = %v", err)
	}
	beyond := live.Clone()
	edge := beyond.Edges[graph.Edges[0].ID]
	edge.Occupancy = model.Edges[0].Depth + 1
	edge.HighWater = edge.Occupancy
	edge.Enqueued = uint64(edge.Occupancy)
	beyond.Edges[graph.Edges[0].ID] = edge
	if err := ValidateSessionModel(beyond, model); err == nil ||
		!strings.Contains(err.Error(), "edge evidence differs") {
		t.Fatalf("live depth overflow error = %v", err)
	}
	unknownFlow := live.Clone()
	unknownFlow.Flows["raw"] = inspect.FlowLive{
		Correlation: "raw", Edges: []string{"invented"},
		CausalStages: []inspect.CausalStageLive{{Item: "item"}},
	}
	if err := ValidateSessionModel(unknownFlow, model); err == nil ||
		!strings.Contains(err.Error(), "unknown internal edge") {
		t.Fatalf("unknown flow edge error = %v", err)
	}
}

func managedLiveTrace(t *testing.T, graph ir.Graph) (inspect.Live, inspect.LiveTrace) {
	t.Helper()
	configuration := inspect.ArtifactIdentity{
		ID: "values://managed", Revision: "1", Digest: "sha256:" + strings.Repeat("a", 64),
	}
	nodes := make(map[string]inspect.NodeLive, len(graph.Nodes))
	for _, graphNode := range graph.Nodes {
		nodes[graphNode.ID] = inspect.NodeLive{
			State: "mounted", Resolution: &inspect.NodeResolution{
				Element: graphNode.Element,
				Runtime: inspect.ArtifactIdentity{
					ID: "runtime://" + graphNode.ID, Revision: "1",
					Digest: "sha256:" + strings.Repeat("b", 64),
				}, RuntimeEvidence: inspect.EvidenceRegistered,
				CapabilitiesEvidence: inspect.EvidenceLive,
			},
		}
	}
	edges := make(map[string]inspect.EdgeLive, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges[edge.ID] = inspect.EdgeLive{}
	}
	live := inspect.Live{
		FormatVersion: inspect.LiveFormatVersion, GraphID: graph.ID, GraphRevision: graph.Revision,
		Fingerprint: graph.Fingerprint, Configuration: &configuration, Sequence: 5,
		ObservedAt: time.Now().UTC(), State: "mounted", Nodes: nodes, Edges: edges,
		Flows: map[string]inspect.FlowLive{},
	}
	baseline, err := inspect.TraceSnapshotFromLive(graph, configuration, live, 100)
	if err != nil {
		t.Fatal(err)
	}
	running := inspect.TraceGraphLive{State: inspect.TraceGraphRunning}
	trace, err := inspect.FreezeLiveTrace(inspect.LiveTrace{
		FormatVersion: inspect.LiveTraceFormatVersion,
		Graph: inspect.GraphReference{
			FormatVersion: graph.FormatVersion, ID: graph.ID,
			Revision: graph.Revision, Fingerprint: graph.Fingerprint,
		},
		Configuration: configuration, Limits: inspect.DefaultTraceLimits(),
		Snapshots: []inspect.TraceSnapshot{baseline},
		Events: []inspect.TraceEvent{{
			Sequence: 6, AtNS: 110, Kind: inspect.TraceEventGraph, Graph: &running,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return live, trace
}
