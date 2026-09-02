package ir_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestFingerprintIgnoresDeclarationOrderAndSourceLocation(t *testing.T) {
	left := fixture()
	right := fixture()
	right.Nodes[0], right.Nodes[1] = right.Nodes[1], right.Nodes[0]
	right.Nodes[0].Source = &ir.Source{Path: "elsewhere.ortg", Line: 100, Column: 4}

	frozenLeft, err := ir.Freeze(left)
	if err != nil {
		t.Fatal(err)
	}
	frozenRight, err := ir.Freeze(right)
	if err != nil {
		t.Fatal(err)
	}
	if frozenLeft.Fingerprint != frozenRight.Fingerprint {
		t.Fatalf("fingerprints differ:\n%s\n%s", frozenLeft.Fingerprint, frozenRight.Fingerprint)
	}
}

func TestCanonicalIRRoundTrip(t *testing.T) {
	frozen, err := ir.Freeze(fixture())
	if err != nil {
		t.Fatal(err)
	}
	first, err := frozen.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ir.Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parsed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("IR encoding is not canonical:\n%s\n%s", first, second)
	}
}

func TestIRRejectsUnboundedAndLossWithoutPermission(t *testing.T) {
	graph := fixture()
	graph.Edges[0].Depth = 0
	if _, err := ir.Freeze(graph); err == nil {
		t.Fatal("expected zero depth to fail")
	}
	graph = fixture()
	graph.Edges[0].Delivery = ir.Lossy
	if _, err := ir.Freeze(graph); err == nil {
		t.Fatal("expected forbidden loss to fail")
	}
}

func TestIRRejectsInternalEdgeInReservedBoundaryQueueNamespace(t *testing.T) {
	graph := fixture()
	graph.Edges[0].ID = ir.BoundaryQueuePrefix + "input"
	if _, err := ir.Freeze(graph); err == nil {
		t.Fatal("expected reserved boundary queue edge ID to fail")
	}
}

func TestStateTransferContractIsValidatedClonedAndFingerprinted(t *testing.T) {
	for name, mutate := range map[string]func(*ir.Node){
		"empty capabilities": func(node *ir.Node) {
			node.StateSchema = "schema://test/state/v1"
			node.StateTransfer = &element.StateTransferCapabilities{}
		},
		"missing schema": func(node *ir.Node) {
			node.StateTransfer = &element.StateTransferCapabilities{Snapshot: true}
		},
		"quiesce without snapshot": func(node *ir.Node) {
			node.StateSchema = "schema://test/state/v1"
			node.StateTransfer = &element.StateTransferCapabilities{Quiesce: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			graph := fixture()
			mutate(&graph.Nodes[1])
			if _, err := ir.Freeze(graph); err == nil || !strings.Contains(err.Error(), "state transfer") {
				t.Fatalf("invalid state-transfer contract error = %v", err)
			}
		})
	}

	schemaOnly := fixture()
	schemaOnly.Nodes[1].StateSchema = "schema://test/state/v1"
	schemaOnlyFrozen, err := ir.Freeze(schemaOnly)
	if err != nil {
		t.Fatal(err)
	}
	transferable := schemaOnly
	capabilities := &element.StateTransferCapabilities{Restore: true}
	transferable.Nodes[1].StateTransfer = capabilities
	transferableFrozen, err := ir.Freeze(transferable)
	if err != nil {
		t.Fatal(err)
	}
	if transferableFrozen.Fingerprint == schemaOnlyFrozen.Fingerprint {
		t.Fatal("state-transfer capability did not affect Graph IR fingerprint")
	}
	capabilities.Restore = false
	if !transferableFrozen.Nodes[1].StateTransfer.Restore {
		t.Fatal("frozen Graph IR aliases state-transfer capability input")
	}
	payload, err := schemaOnlyFrozen.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(`"state_transfer"`)) {
		t.Fatalf("schema-only Graph IR encoded absent state-transfer capabilities: %s", payload)
	}
}

func fixture() ir.Graph {
	typeOfEdge := element.Event(element.Named("test.Value"))
	sourceIdentity, _ := descriptor("test.Source", element.Output, "out", typeOfEdge).Identity()
	sinkIdentity, _ := descriptor("test.Sink", element.Input, "in", typeOfEdge).Identity()
	return ir.Graph{
		FormatVersion: ir.FormatVersion,
		ID:            "example",
		Revision:      1,
		Nodes: []ir.Node{
			{ID: "sink", Element: sinkIdentity, Ports: []ir.Port{{
				Name: "in", Direction: element.Input, Type: typeOfEdge,
				Cardinality: element.One, DefaultDepth: 4,
			}}},
			{ID: "source", Element: sourceIdentity, Ports: []ir.Port{{
				Name: "out", Direction: element.Output, Type: typeOfEdge,
				Cardinality: element.One, DefaultDepth: 4,
			}}},
		},
		Edges: []ir.Edge{{
			ID: "source.out->sink.in", From: ir.Endpoint{Node: "source", Port: "out"},
			To: ir.Endpoint{Node: "sink", Port: "in"}, Type: typeOfEdge,
			Delivery: ir.Lossless, Ordering: "fifo", Depth: 4,
		}},
	}
}

func descriptor(name string, direction element.Direction, port string, value element.Type) element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          name,
		Revision:      1,
		Ports: []element.Port{{
			Name: port, Direction: direction, Type: value,
			Cardinality: element.One, DefaultDepth: 4,
		}},
	}
}
