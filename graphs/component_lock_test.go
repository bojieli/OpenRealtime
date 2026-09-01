package graphs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/elements"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

func TestStandardComponentGraphsCompileFromCommittedLocks(t *testing.T) {
	for _, name := range []string{"acoustic-endpoint", "multimodal-content", "text-file-cognition"} {
		name := name
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join("components", name)
			topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := syntax.Parse("agent.ortg", topology)
			if err != nil {
				t.Fatal(err)
			}
			lockPayload, err := os.ReadFile(filepath.Join(directory, "openrealtime.lock"))
			if err != nil {
				t.Fatal(err)
			}
			lock, err := resolve.ParseLock(lockPayload)
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := elements.Catalog()
			if err != nil {
				t.Fatal(err)
			}
			compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
				Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := compiled.Graph.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
