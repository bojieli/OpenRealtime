package video

import (
	"os"
	"path/filepath"
	"testing"

	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

func TestAdaptiveVideoReferenceCompilesFromExactLock(t *testing.T) {
	directory := filepath.Join("..", "..", "graphs", "components", "adaptive-video")
	topology := readVideoComponent(t, directory, "agent.ortg")
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	if err := perceptionelements.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}

	updated, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(readVideoComponent(t, directory, "openrealtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !lock.Equal(updated.Lock) {
		want, marshalErr := updated.Lock.Marshal()
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		t.Fatalf("adaptive-video lock is stale; generated lock:\n%s", want)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	values, err := graphvalues.ParseYAML("agent.values.yaml", readVideoComponent(t, directory, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, values)
	if err != nil {
		t.Fatal(err)
	}
	if err := bound.Graph.Validate(); err != nil {
		t.Fatal(err)
	}
	config, err := decodeAdaptiveObservationConfig(bound.Values["policy"])
	if err != nil {
		t.Fatal(err)
	}
	if config.Source != "youtube" || config.Mode != CadenceAdaptive {
		t.Fatalf("reference policy config = %+v", config)
	}

	wantLossy := map[string]bool{"live_frames": true, "live_references": true}
	for _, edge := range bound.Graph.Edges {
		if wantLossy[edge.ID] {
			if edge.Delivery != ir.Lossy {
				t.Fatalf("capture edge %s delivery = %s", edge.ID, edge.Delivery)
			}
			delete(wantLossy, edge.ID)
		} else if edge.Delivery != ir.Lossless {
			t.Fatalf("non-capture edge %s is unexpectedly lossy", edge.ID)
		}
	}
	if len(wantLossy) != 0 {
		t.Fatalf("missing named capture edges: %v", wantLossy)
	}
	for _, boundary := range bound.Graph.Boundaries {
		if boundary.Type.String() == "Stream<audio.InputFrame>" {
			t.Fatalf("adaptive-video reference unexpectedly exposes audio: %+v", boundary)
		}
	}
}

func readVideoComponent(t *testing.T, directory, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
