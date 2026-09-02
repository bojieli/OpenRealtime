package graph_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

func TestCompileInfersTeeArityAndGenericType(t *testing.T) {
	catalog := testCatalog(t)
	result := compile(t, catalog, `graph tee {
    test.Source :: source;
    flow.Tee :: fork;
    test.Sink :: left;
    test.Sink :: right;
    source.out -> fork.in;
    fork.out -> left.in;
    fork.out -> right.in;
    input source_trigger = source.trigger;
}`)
	fork := findNode(t, result, "fork")
	out := findPort(t, fork, "out")
	if got, want := len(out.Lanes), 2; got != want {
		t.Fatalf("tee output lanes = %d, want %d", got, want)
	}
	if got, want := out.Type.String(), "Event<test.Value>"; got != want {
		t.Fatalf("tee type = %s, want %s", got, want)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCompileRejectsTypeMismatch(t *testing.T) {
	catalog := testCatalog(t)
	descriptor := sinkDescriptor("test.OtherSink", element.Event(element.Named("test.Other")))
	mustRegister(t, catalog, descriptor)
	err := compileError(t, catalog, `graph mismatch {
    test.Source :: source;
    test.OtherSink :: sink;
    source.out -> sink.in;
    input source_trigger = source.trigger;
}`)
	assertDiagnostic(t, err, "E_TYPE_MISMATCH", "Event<test.Value>")
}

func TestCompileRejectsImplicitFanoutAndMultipleWriters(t *testing.T) {
	catalog := testCatalog(t)
	err := compileError(t, catalog, `graph fanout {
    test.Source :: source;
    test.Sink :: left;
    test.Sink :: right;
    source.out -> left.in;
    source.out -> right.in;
    input source_trigger = source.trigger;
}`)
	assertDiagnostic(t, err, "E_IMPLICIT_FANOUT", "explicit connector")

	err = compileError(t, catalog, `graph writers {
    test.Source :: left;
    test.Source :: right;
    test.Sink :: sink;
    left.out -> sink.in;
    right.out -> sink.in;
    input left_trigger = left.trigger;
    input right_trigger = right.trigger;
}`)
	assertDiagnostic(t, err, "E_MULTIPLE_WRITERS", "explicit connector")
}

func TestCompileRejectsLossyControlEvenWhenPortsOptIn(t *testing.T) {
	catalog := testCatalog(t)
	mustRegister(t, catalog, element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.TriggerSink",
		Revision:      1,
		Ports: []element.Port{{
			Name: "in", Direction: element.Input, Cardinality: element.One,
			Type: element.Trigger(element.Named("test.Start")), Required: true,
			LossAllowed: true,
		}},
	})
	mustRegister(t, catalog, element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.TriggerSource",
		Revision:      1,
		Ports: []element.Port{{
			Name: "out", Direction: element.Output, Cardinality: element.One,
			Type: element.Trigger(element.Named("test.Start")), Required: true,
			LossAllowed: true,
		}},
	})
	err := compileError(t, catalog, `graph lossy_control {
    test.TriggerSource :: source;
    test.TriggerSink :: sink;
    source.out => sink.in;
}`)
	assertDiagnostic(t, err, "E_LOSS_FORBIDDEN", "cannot discard")
}

func TestCompileAppliesDepthOverrideAndRejectsUnknownOverride(t *testing.T) {
	catalog := testCatalog(t)
	file := mustParse(t, `graph depths {
    test.Source :: source;
    test.Sink :: sink;
    source.out -> sink.in;
    input source_trigger = source.trigger;
}`)
	compiled, err := graph.Compile(file, graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
		ChannelDepth: map[string]int{"source.out->sink.in": 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := compiled.Graph.Edges[0].Depth; got != 9 {
		t.Fatalf("depth = %d, want 9", got)
	}
	_, err = graph.Compile(file, graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
		ChannelDepth: map[string]int{"missing": 1},
	})
	assertDiagnostic(t, err, "E_UNKNOWN_CHANNEL", "missing")
}

func TestFingerprintDoesNotDependOnDeclarationOrder(t *testing.T) {
	catalog := testCatalog(t)
	left := compile(t, catalog, `graph stable {
    test.Source :: source;
    test.Sink :: sink;
    source.out -> sink.in;
    input source_trigger = source.trigger;
}`)
	right := compile(t, catalog, `graph stable {
    test.Sink :: sink;
    test.Source :: source;
    input source_trigger = source.trigger;
    source.out -> sink.in;
}`)
	if left.Fingerprint != right.Fingerprint {
		t.Fatalf("fingerprints differ:\n%s\n%s", left.Fingerprint, right.Fingerprint)
	}
}

func TestLockedCompilationConsumesGeneratedLock(t *testing.T) {
	catalog := testCatalog(t)
	file := mustParse(t, `graph locked {
    test.Source :: source;
    test.Sink :: sink;
    source.out -> sink.in;
    input source_trigger = source.trigger;
}`)
	updated, err := graph.Compile(file, graph.Options{Catalog: catalog, ResolutionMode: resolve.Update})
	if err != nil {
		t.Fatal(err)
	}
	locked, err := graph.Compile(file, graph.Options{
		Catalog: catalog, Lock: updated.Lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	if locked.Graph.Fingerprint != updated.Graph.Fingerprint {
		t.Fatalf("locked fingerprint = %s, updated = %s",
			locked.Graph.Fingerprint, updated.Graph.Fingerprint)
	}
}

func TestCompilePropagatesExplicitStateTransferCapabilities(t *testing.T) {
	catalog := testCatalog(t)
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Stateful",
		Revision:      1,
		Ports: []element.Port{{
			Name: "out", Direction: element.Output, Cardinality: element.One,
			Type: element.Event(element.Named("test.Value")),
		}},
		StateSchema: "schema://test/state/v1",
		StateTransfer: &element.StateTransferCapabilities{
			Snapshot: true, Restore: true, Quiesce: true,
		},
	}
	mustRegister(t, catalog, descriptor)
	compiled := compile(t, catalog, `graph stateful {
    test.Stateful :: stateful;
    output value = stateful.out;
}`)
	node := findNode(t, compiled, "stateful")
	if node.StateTransfer == nil || !node.StateTransfer.Snapshot ||
		!node.StateTransfer.Restore || !node.StateTransfer.Quiesce {
		t.Fatalf("compiled state-transfer capabilities = %#v", node.StateTransfer)
	}
	node.StateTransfer.Restore = false
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	registered, found := catalog.Exact(identity)
	if !found || registered.StateTransfer == nil || !registered.StateTransfer.Restore {
		t.Fatal("compiled Graph IR aliases registered descriptor state-transfer capabilities")
	}
}

func compile(t *testing.T, catalog *resolve.Catalog, source string) ir.Graph {
	t.Helper()
	result, err := graph.Compile(mustParse(t, source), graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Graph
}

func findNode(t *testing.T, graph ir.Graph, name string) ir.Node {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.ID == name {
			return node
		}
	}
	t.Fatalf("node %q not found", name)
	return ir.Node{}
}

func findPort(t *testing.T, node ir.Node, name string) ir.Port {
	t.Helper()
	for _, port := range node.Ports {
		if port.Name == name {
			return port
		}
	}
	t.Fatalf("port %q not found on node %q", name, node.ID)
	return ir.Port{}
}

func compileError(t *testing.T, catalog *resolve.Catalog, source string) error {
	t.Helper()
	_, err := graph.Compile(mustParse(t, source), graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err == nil {
		t.Fatal("expected graph compilation to fail")
	}
	return err
}

func mustParse(t *testing.T, source string) syntax.File {
	t.Helper()
	file, err := syntax.Parse("test.ortg", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func assertDiagnostic(t *testing.T, err error, code, contains string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var failures *graph.Errors
	if !errors.As(err, &failures) {
		t.Fatalf("error type = %T, want *graph.Errors: %v", err, err)
	}
	for _, diagnostic := range failures.Diagnostics {
		if diagnostic.Code == code && strings.Contains(diagnostic.Message, contains) {
			return
		}
	}
	t.Fatalf("did not find %s containing %q in %+v", code, contains, failures.Diagnostics)
}

func testCatalog(t *testing.T) *resolve.Catalog {
	t.Helper()
	catalog := resolve.NewCatalog()
	value := element.Event(element.Named("test.Value"))
	mustRegister(t, catalog, element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Source",
		Revision:      1,
		Ports: []element.Port{
			{Name: "trigger", Direction: element.Input, Cardinality: element.One,
				Type: element.Trigger(element.Named("test.Start")), Required: true},
			{Name: "out", Direction: element.Output, Cardinality: element.One,
				Type: value, Required: true, DefaultDepth: 4},
		},
		Reaction: element.Reaction{Triggers: []string{"trigger"}, Outcomes: []string{"out"}},
	})
	mustRegister(t, catalog, sinkDescriptor("test.Sink", value))
	mustRegister(t, catalog, element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "flow.Tee",
		Revision:      1,
		Generics:      []string{"T"},
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Cardinality: element.One,
				Type: element.Var("T"), Required: true},
			{Name: "out", Direction: element.Output, Cardinality: element.Variadic,
				Type: element.Var("T"), Required: true, MinConnections: 1},
		},
	})
	return catalog
}

func sinkDescriptor(name string, value element.Type) element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          name,
		Revision:      1,
		Ports: []element.Port{{
			Name: "in", Direction: element.Input, Cardinality: element.One,
			Type: value, Required: true, DefaultDepth: 4,
		}},
	}
}

func mustRegister(t *testing.T, catalog *resolve.Catalog, descriptor element.Descriptor) {
	t.Helper()
	if err := catalog.Register(descriptor); err != nil {
		t.Fatal(err)
	}
}
