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

func TestImportedSubgraphElaboratesToTypedHierarchyAndLock(t *testing.T) {
	catalog := moduleCatalog(t)
	child := parseModule(t, "lib/pair.ortg", `graph pair {
    test.Pass :: pass;
    input in = pass.in;
    output out = pass.out;
}`)
	root := parseModule(t, "root.ortg", `import "lib/pair.ortg" as lib;
graph root {
    test.Source :: source;
    lib.pair :: middle;
    test.Sink :: sink;
    source.out -> middle.in;
    middle.out -> sink.in;
    input trigger = source.trigger;
}`)
	loader := moduleLoader(map[string]syntax.File{"lib/pair.ortg": child})
	updated, err := graph.Compile(root, graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Update, Loader: loader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found := updated.Lock.Lookup("lib.pair"); !found {
		t.Fatalf("subgraph identity is absent from lock: %+v", updated.Lock)
	}
	if got, want := len(updated.Graph.Scopes), 1; got != want {
		t.Fatalf("scopes = %d, want %d: %+v", got, want, updated.Graph.Scopes)
	}
	scope := updated.Graph.Scopes[0]
	if scope.ID != "middle" || len(scope.Nodes) != 1 || scope.Nodes[0] != "middle__pass" ||
		scope.Composite.Name != "subgraph.pair" {
		t.Fatalf("unexpected scope: %+v", scope)
	}
	if got := nodeIDs(updated.Graph.Nodes); strings.Join(got, ",") != "middle__pass,sink,source" {
		t.Fatalf("flattened nodes = %v", got)
	}
	if len(updated.Graph.Lineage) != 1 || !strings.Contains(updated.Graph.Lineage[0], "lib.pair=subgraph.pair@1#sha256:") {
		t.Fatalf("lineage = %+v", updated.Graph.Lineage)
	}
	locked, err := graph.Compile(root, graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Locked, Lock: updated.Lock, Loader: loader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if locked.Graph.Fingerprint != updated.Graph.Fingerprint {
		t.Fatalf("locked fingerprint = %s, updated = %s", locked.Graph.Fingerprint, updated.Graph.Fingerprint)
	}
}

func TestSubgraphBodyChangeInvalidatesLockEvenWhenContractMatches(t *testing.T) {
	catalog := moduleCatalog(t)
	root := parseModule(t, "root.ortg", `import "pair.ortg" as lib;
graph root {
    lib.pair :: middle;
    input in = middle.in;
    output out = middle.out;
}`)
	first := parseModule(t, "pair.ortg", `graph pair {
    test.Pass :: original;
    input in = original.in;
    output out = original.out;
}`)
	updated, err := graph.Compile(root, graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
		Loader: moduleLoader(map[string]syntax.File{"pair.ortg": first}),
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := parseModule(t, "pair.ortg", `graph pair {
    test.Pass :: renamed;
    input in = renamed.in;
    output out = renamed.out;
}`)
	_, err = graph.Compile(root, graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Locked, Lock: updated.Lock,
		Loader: moduleLoader(map[string]syntax.File{"pair.ortg": changed}),
	})
	if err == nil || !strings.Contains(err.Error(), "stale subgraph lock") {
		t.Fatalf("changed body error = %v", err)
	}
}

func TestSubgraphBoundaryMustBeUsedExactlyOnce(t *testing.T) {
	catalog := moduleCatalog(t)
	child := parseModule(t, "pair.ortg", `graph pair {
    test.Pass :: pass;
    input in = pass.in;
    output out = pass.out;
}`)
	root := parseModule(t, "root.ortg", `import "pair.ortg" as lib;
graph root {
    lib.pair :: middle;
    input in = middle.in;
}`)
	_, err := graph.Compile(root, graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
		Loader: moduleLoader(map[string]syntax.File{"pair.ortg": child}),
	})
	if err == nil || !strings.Contains(err.Error(), "boundary out requires exactly one connection") {
		t.Fatalf("unconnected boundary error = %v", err)
	}
}

func TestUnusedImportDoesNotEnterIdentityOrRequireLock(t *testing.T) {
	catalog := moduleCatalog(t)
	root := parseModule(t, "root.ortg", `import "unused.ortg" as unused;
graph root {
    test.Pass :: pass;
    input in = pass.in;
    output out = pass.out;
}`)
	child := parseModule(t, "unused.ortg", `graph child {
    test.Pass :: child;
    input in = child.in;
    output out = child.out;
}`)
	compiled, err := graph.Compile(root, graph.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
		Loader: moduleLoader(map[string]syntax.File{"unused.ortg": child}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Graph.Lineage) != 0 || len(compiled.Graph.Scopes) != 0 {
		t.Fatalf("unused import changed graph identity: %+v %+v", compiled.Graph.Lineage, compiled.Graph.Scopes)
	}
	if _, found := compiled.Lock.Lookup("unused.child"); found {
		t.Fatal("unused import entered lock")
	}
}

func moduleCatalog(t *testing.T) *resolve.Catalog {
	t.Helper()
	catalog := resolve.NewCatalog()
	valueType := element.Event(element.Named("test.Value"))
	descriptors := []element.Descriptor{
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.Pass", Revision: 1,
			Ports: []element.Port{
				{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true},
			},
			Reaction: element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}},
		},
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.Source", Revision: 1,
			Ports: []element.Port{
				{Name: "trigger", Direction: element.Input, Type: element.Trigger(element.Named("test.Start")), Cardinality: element.One, Required: true},
				{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true},
			},
			Reaction: element.Reaction{Triggers: []string{"trigger"}, Outcomes: []string{"out"}},
		},
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.Sink", Revision: 1,
			Ports: []element.Port{{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true}},
		},
	}
	for _, descriptor := range descriptors {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	return catalog
}

func parseModule(t *testing.T, path, source string) syntax.File {
	t.Helper()
	file, err := syntax.Parse(path, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func moduleLoader(files map[string]syntax.File) graph.SourceLoader {
	return graph.SourceLoaderFunc(func(_, path string) (syntax.File, error) {
		file, found := files[path]
		if !found {
			return syntax.File{}, errors.New("missing module " + path)
		}
		return file, nil
	})
}

func nodeIDs(nodes []ir.Node) []string {
	result := make([]string, len(nodes))
	for index, node := range nodes {
		result[index] = node.ID
	}
	return result
}
