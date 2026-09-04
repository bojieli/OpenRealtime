package graphs_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	graphrealtimecu "github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/graphs"
)

func TestRealtimeComputerUseToolAndRepetitionAdmissionTopologyCompilesUnlocked(t *testing.T) {
	directory := filepath.Join("components", "realtime-computer-use")
	topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("realtime-computer-use/agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := graphrealtimecu.RegisterElementDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := realtimeCUNodeElement(compiled.Graph, "repetition_admission"); got != "action.RepetitionAdmission" {
		t.Fatalf("repetition_admission element = %q", got)
	}
	if got := realtimeCUNodeElement(compiled.Graph, "tool_admission"); got != "action.ToolAdmission" {
		t.Fatalf("tool_admission element = %q", got)
	}
	for _, edge := range []struct{ fromNode, fromPort, toNode, toPort string }{
		{"normalize_arguments", "normalized", "tool_admission", "action"},
		{"tool_admission", "admitted", "repetition_admission", "action"},
		{"repetition_admission", "admitted", "confirmation", "action"},
		{"tool_result_commit", "canonical", "repetition_admission", "result"},
		{"tool_admission", "terminal", "effect_terminal_mux", "in"},
		{"repetition_admission", "terminal", "effect_terminal_mux", "in"},
		{"effect_terminal_mux", "out", "activation", "effect_terminal"},
	} {
		delivery, found := realtimeCUEdgeDelivery(
			compiled.Graph, edge.fromNode, edge.fromPort, edge.toNode, edge.toPort,
		)
		if !found || delivery != ir.Lossless {
			t.Errorf("required edge %s.%s -> %s.%s = %q, found=%t; want lossless",
				edge.fromNode, edge.fromPort, edge.toNode, edge.toPort, delivery, found)
		}
	}
	for _, forbidden := range []struct{ fromNode, fromPort, toNode, toPort string }{
		{"normalize_arguments", "normalized", "confirmation", "action"},
		{"normalize_arguments", "normalized", "repetition_admission", "action"},
		{"tool_admission", "admitted", "confirmation", "action"},
	} {
		if _, found := realtimeCUEdgeDelivery(
			compiled.Graph, forbidden.fromNode, forbidden.fromPort, forbidden.toNode, forbidden.toPort,
		); found {
			t.Errorf("repetition policy bypass remains at %s.%s -> %s.%s",
				forbidden.fromNode, forbidden.fromPort, forbidden.toNode, forbidden.toPort)
		}
	}
	foundCanonicalBoundary := false
	for _, boundary := range compiled.Graph.Boundaries {
		if boundary.Name != "canonical_result" || boundary.Direction != ir.OutputBoundary {
			continue
		}
		foundCanonicalBoundary = true
		if boundary.Endpoint.Node != "repetition_admission" ||
			boundary.Endpoint.Port != "canonical_result" ||
			!boundary.Type.Equal(actionelements.CanonicalResultType()) {
			t.Errorf("canonical_result boundary = %+v", boundary)
		}
	}
	if !foundCanonicalBoundary {
		t.Fatal("canonical_result boundary is absent")
	}

	valuesBody, err := os.ReadFile(filepath.Join(directory, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := graphvalues.ParseYAML("realtime-computer-use/agent.values.yaml", valuesBody)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, values)
	if err != nil {
		t.Fatal(err)
	}
	var toolConfig actionelements.ToolAdmissionConfig
	if err := json.Unmarshal(bound.Values["tool_admission"], &toolConfig); err != nil {
		t.Fatal(err)
	}
	if toolConfig.AllowedTools != nil ||
		!slices.Equal(toolConfig.DeniedTools, []string{"computer.screenshot", "computer.wait"}) {
		t.Fatalf("Realtime-CU tool admission policy = %+v", toolConfig)
	}
	var config actionelements.RepetitionAdmissionConfig
	if err := json.Unmarshal(bound.Values["repetition_admission"], &config); err != nil {
		t.Fatal(err)
	}
	wantRepeatable := []string{
		"computer.move", "computer.drag", "computer.key", "computer.scroll",
		"computer.screenshot", "computer.wait",
	}
	if config.Mode != actionelements.RepetitionAdmissionAtMostOnceAfterSuccessPerUserIntent ||
		config.MaxTrackedEffects != 512 || !slices.Equal(config.RepeatableTools, wantRepeatable) {
		t.Fatalf("Realtime-CU repetition policy = %+v", config)
	}
}

func TestRealtimeComputerUseSettlementTopologyHasExplicitRetryNoBypassAndExactCancellationRoutes(t *testing.T) {
	graph, values := compileRealtimeCUProductionGraphAndValues(t)

	wantEdges := [][4]string{
		{"temporal_evidence_admission", "admitted", "settlement", "evidence"},
		{"settlement", "probe", "settlement_retry", "probe"},
		{"settlement_retry", "attempt", "settlement_producer", "probe"},
		{"settlement_producer", "disposition", "settlement_disposition_copy", "in"},
		{"settlement_disposition_copy", "out", "settlement", "disposition"},
		{"settlement_disposition_copy", "out", "settlement_retry", "disposition"},
		{"settlement_retry", "exhausted", "settlement_retry_exhausted_sink", "in"},
		{"settlement_retry", "forwarded_reset", "settlement", "reset"},
		{"settlement", "admitted", "activation", "admitted"},
		{"settlement", "terminal", "activation", "settlement"},
		{"activation", "settlement_ack", "settlement", "ack"},
		{"cancellation_coordinator", "settlement_cancel", "settlement_retry", "cancel"},
		{"settlement_retry", "forwarded_cancel", "settlement_cancel_copy", "in"},
		{"settlement_cancel_copy", "out", "settlement", "cancel"},
		{"settlement_cancel_copy", "out", "settlement_producer", "cancel"},
		{"cancellation_coordinator", "activation_cancel", "activation", "cancel"},
		{"cancellation_coordinator", "model_cancel", "model", "cancel"},
		{"cancellation_coordinator", "action_cancel", "action_cancel_copy", "in"},
		{"settlement_outcome_copy", "out", "cancellation_coordinator", "settlement_outcome"},
		{"settlement_producer", "outcome", "settlement_producer_outcome_copy", "in"},
		{"settlement_producer_outcome_copy", "out", "cancellation_coordinator", "settlement_producer_outcome"},
		{"activation_outcome_copy", "out", "cancellation_coordinator", "activation_outcome"},
		{"model_outcome_copy", "out", "cancellation_coordinator", "model_outcome"},
		{"model_commit_outcome_copy", "out", "cancellation_coordinator", "model_commit_outcome"},
		{"action_outcome_mux", "out", "cancellation_coordinator", "action_outcome"},
	}
	for _, edge := range wantEdges {
		if delivery, found := realtimeCUEdgeDelivery(
			graph, edge[0], edge[1], edge[2], edge[3],
		); !found || delivery != ir.Lossless {
			t.Errorf("required settlement edge %s.%s -> %s.%s = %q, found=%t; want lossless",
				edge[0], edge[1], edge[2], edge[3], delivery, found)
		}
	}

	incoming := 0
	for _, edge := range graph.Edges {
		if edge.To.Node != "activation" || edge.To.Port != "admitted" {
			continue
		}
		incoming++
		if edge.From.Node != "settlement" || edge.From.Port != "admitted" {
			t.Errorf("activation admission bypass = %s -> %s", edge.From.String(), edge.To.String())
		}
	}
	if incoming != 1 {
		t.Fatalf("activation.admitted incoming edges = %d, want exactly 1", incoming)
	}
	assertRealtimeCUConsumers(t, graph, "temporal_evidence_admission", "admitted", []string{
		"settlement.evidence",
	})
	assertRealtimeCUConsumers(t, graph, "settlement", "probe", []string{
		"settlement_retry.probe",
	})
	assertRealtimeCUConsumers(t, graph, "settlement_disposition_copy", "out", []string{
		"settlement.disposition", "settlement_retry.disposition",
	})
	assertRealtimeCUConsumers(t, graph, "settlement_cancel_copy", "out", []string{
		"settlement.cancel", "settlement_producer.cancel",
	})
	assertRealtimeCUConsumers(t, graph, "settlement_producer", "outcome", []string{
		"settlement_producer_outcome_copy.in",
	})
	assertRealtimeCUConsumers(t, graph, "settlement_producer_outcome_copy", "out", []string{
		"cancellation_coordinator.settlement_producer_outcome",
	})
	actionStages := []string{
		"authorized_call_commit", "confirmation", "dispatch", "ledger_commit",
		"proposal_admission", "provenance_join", "tool_result_commit",
	}
	actionNodes := []string{
		"canonical_call_commit.cancel", "confirmation.cancel", "dispatch.cancel",
		"ledger_commit.cancel", "proposal_admission.cancel", "provenance_join.cancel",
		"tool_result_commit.cancel",
	}
	assertRealtimeCUConsumers(t, graph, "action_cancel_copy", "out", actionNodes)
	assertRealtimeCUProducers(t, graph, "action_outcome_mux", "in", []string{
		"admission_outcome_copy.out", "canonical_call_outcome_copy.out",
		"confirmation_outcome_copy.out", "dispatch_outcome_copy.out",
		"ledger_outcome_copy.out", "provenance_outcome_copy.out",
		"result_commit_outcome_copy.out",
	})
	for _, boundary := range graph.Boundaries {
		if boundary.Direction == ir.InputBoundary && boundary.Endpoint.Node == "activation" {
			t.Errorf("activation has direct input boundary bypass: %+v", boundary)
		}
		if boundary.Name == "activation_cancel" || boundary.Name == "model_cancel" ||
			boundary.Name == "action_cancel" {
			t.Errorf("legacy cancellation bypass boundary remains: %+v", boundary)
		}
	}
	assertRealtimeCUBoundary(t, graph, "session_cancel", ir.InputBoundary,
		"cancellation_coordinator", "request", graphrealtimecu.SessionCancellationType())
	assertRealtimeCUBoundary(t, graph, "cancellation_outcome", ir.OutputBoundary,
		"cancellation_coordinator", "outcome", graphrealtimecu.SessionCancellationOutcomeType())
	assertRealtimeCUBoundary(t, graph, "settlement_producer_outcome", ir.OutputBoundary,
		"settlement_producer_outcome_copy", "out", policyelements.IntentDispositionProducerOutcomeType())
	assertRealtimeCUBoundary(t, graph, "settlement_retry_outcome", ir.OutputBoundary,
		"settlement_retry", "outcome", policyelements.IntentDispositionRetryOutcomeType())
	assertRealtimeCUBoundary(t, graph, "settlement_reset", ir.InputBoundary,
		"settlement_retry", "reset", policyelements.IntentSettlementResetType())

	var settlement policyelements.IntentSettlementConfig
	if err := json.Unmarshal(values.Values["settlement"], &settlement); err != nil {
		t.Fatal(err)
	}
	var producer policyelements.IntentDispositionProducerConfig
	if err := json.Unmarshal(values.Values["settlement_producer"], &producer); err != nil {
		t.Fatal(err)
	}
	var activation graphrealtimecu.ActivationConfig
	if err := json.Unmarshal(values.Values["activation"], &activation); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(producer.ExpectedSettlement, settlement) ||
		activation.ExpectedSettlement == nil ||
		!reflect.DeepEqual(*activation.ExpectedSettlement, settlement) ||
		!reflect.DeepEqual(activation.ExpectedAdmission, settlement.ExpectedAdmission) {
		t.Fatalf("settlement contracts drifted: gate=%+v producer=%+v activation=%+v",
			settlement, producer.ExpectedSettlement, activation)
	}
	if settlement.Detector.Reference != graphrealtimecu.SettlementPolicyReference {
		t.Fatalf("settlement detector reference = %q", settlement.Detector.Reference)
	}
	var retry policyelements.IntentDispositionRetryConfig
	if err := json.Unmarshal(values.Values["settlement_retry"], &retry); err != nil {
		t.Fatal(err)
	}
	if retry != (policyelements.IntentDispositionRetryConfig{
		InitialDelayMS: 100, BackoffFactor: 2, MaxDelayMS: 1_000, MaxRetries: 3,
		MaxElapsedMS: 5_000, MaxPending: 64, TerminalMemory: 512, CancelMemory: 256,
	}) {
		t.Fatalf("settlement retry policy = %+v", retry)
	}
	var coordinator graphrealtimecu.CancellationCoordinatorConfig
	if err := json.Unmarshal(values.Values["cancellation_coordinator"], &coordinator); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(coordinator.ActionAckStages, actionStages) {
		t.Fatalf("coordinator action acknowledgement stages = %v, want %v",
			coordinator.ActionAckStages, actionStages)
	}
}

func TestRealtimeComputerUseArtifactsBindExactSelectedSettlementContract(t *testing.T) {
	target := computeruse.Target{
		Name: "settlement-contract-browser", Sources: []string{graphrealtimecu.SourceScreen},
		Width: 640, Height: 480,
	}
	descriptor := testRealtimeCUPolicyDescriptor()
	artifacts, err := graphs.RealtimeComputerUseArtifacts(
		target, "selected-settlement-observer", descriptor,
	)
	if err != nil {
		t.Fatal(err)
	}
	document, err := graphvalues.ParseJSON(artifacts.Values.Path, artifacts.Values.Data)
	if err != nil {
		t.Fatal(err)
	}
	var settlement policyelements.IntentSettlementConfig
	if err := json.Unmarshal(document.Nodes["settlement"], &settlement); err != nil {
		t.Fatal(err)
	}
	wantDetector := policyelements.IntentDetectorIdentity{
		Reference: graphrealtimecu.SettlementPolicyReference,
		Revision:  descriptor.Revision, ConfigurationDigest: descriptor.ConfigurationDigest,
	}
	wantSources := []policyelements.TemporalEvidenceRequirement{{
		Observer: "selected-settlement-observer", Source: graphrealtimecu.SourceScreen,
	}}
	if settlement.Detector != wantDetector || !reflect.DeepEqual(settlement.CandidateSources, wantSources) {
		t.Fatalf("selected settlement contract = %+v, want detector=%+v sources=%+v",
			settlement, wantDetector, wantSources)
	}
	var producer policyelements.IntentDispositionProducerConfig
	if err := json.Unmarshal(document.Nodes["settlement_producer"], &producer); err != nil {
		t.Fatal(err)
	}
	var activation graphrealtimecu.ActivationConfig
	if err := json.Unmarshal(document.Nodes["activation"], &activation); err != nil {
		t.Fatal(err)
	}
	if !producer.DirectVisualInput || !reflect.DeepEqual(producer.ExpectedSettlement, settlement) ||
		activation.ExpectedSettlement == nil ||
		!reflect.DeepEqual(*activation.ExpectedSettlement, settlement) ||
		!reflect.DeepEqual(activation.ExpectedAdmission, settlement.ExpectedAdmission) {
		t.Fatalf("derived settlement contracts drifted: gate=%+v producer=%+v activation=%+v",
			settlement, producer, activation)
	}
}

func compileRealtimeCUProductionGraphAndValues(t *testing.T) (ir.Graph, graphvalues.Bound) {
	t.Helper()
	directory := filepath.Join("components", "realtime-computer-use")
	topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("realtime-computer-use/agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := graphrealtimecu.RegisterElementDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesBody, err := os.ReadFile(filepath.Join(directory, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := graphvalues.ParseYAML("realtime-computer-use/agent.values.yaml", valuesBody)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, document)
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Graph, bound
}

func assertRealtimeCUBoundary(
	t *testing.T, graph ir.Graph, name string, direction ir.BoundaryDirection,
	node, port string, valueType element.Type,
) {
	t.Helper()
	for _, boundary := range graph.Boundaries {
		if boundary.Name != name {
			continue
		}
		if boundary.Direction != direction || boundary.Endpoint.Node != node ||
			boundary.Endpoint.Port != port || !boundary.Type.Equal(valueType) {
			t.Fatalf("boundary %q = %+v", name, boundary)
		}
		return
	}
	t.Fatalf("boundary %q is absent", name)
}

func assertRealtimeCUConsumers(
	t *testing.T, graph ir.Graph, node, port string, want []string,
) {
	t.Helper()
	got := make([]string, 0)
	for _, edge := range graph.Edges {
		if edge.From.Node == node && edge.From.Port == port {
			got = append(got, edge.To.Node+"."+edge.To.Port)
		}
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s.%s consumers = %v, want exactly %v", node, port, got, want)
	}
}

func assertRealtimeCUProducers(
	t *testing.T, graph ir.Graph, node, port string, want []string,
) {
	t.Helper()
	got := make([]string, 0)
	for _, edge := range graph.Edges {
		if edge.To.Node == node && edge.To.Port == port {
			got = append(got, edge.From.Node+"."+edge.From.Port)
		}
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s.%s producers = %v, want exactly %v", node, port, got, want)
	}
}

func realtimeCUNodeElement(graph ir.Graph, id string) string {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node.Element.Name
		}
	}
	return ""
}

func realtimeCUEdgeDelivery(
	graph ir.Graph, fromNode, fromPort, toNode, toPort string,
) (ir.Delivery, bool) {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return edge.Delivery, true
		}
	}
	return "", false
}
