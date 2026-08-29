package model

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/speech"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

func TestOmniDuplexAndUpstreamAreSelectionsOverOneTypedElement(t *testing.T) {
	cases := []struct {
		directory string
		inputs    []string
		outputs   []string
		required  []string
	}{
		{
			directory: "omni-external-interaction",
			inputs: []string{
				"audio", "cancel", "commit", "context", "context_injection", "interaction",
				"tick", "tool_result", "tools", "trigger", "truncate", "video",
			},
			outputs: []string{
				"activity", "native_state", "outcome", "prepared_audio", "prepared_text",
				"result", "tool_proposal", "transcript",
			},
			required: []string{},
		},
		{
			directory: "duplex-native-interaction",
			inputs: []string{
				"audio", "cancel", "commit", "context", "context_injection", "tool_result",
				"tools", "truncate", "video",
			},
			outputs: []string{
				"activity", "interaction_act", "native_state", "outcome", "prepared_audio",
				"prepared_text", "result", "tool_proposal", "transcript",
			},
			required: []string{"model.concurrent-io", "model.native-interaction"},
		},
		{
			directory: "upstream-native-interaction",
			inputs: []string{
				"audio", "cancel", "commit", "context_injection", "tool_result", "tools",
				"trigger", "truncate",
			},
			outputs: []string{
				"activity", "interaction_act", "native_state", "outcome", "prepared_audio",
				"prepared_text", "result", "tool_proposal", "transcript",
			},
			required: []string{"model.native-interaction", "transport.upstream-realtime"},
		},
	}
	for _, test := range cases {
		t.Run(test.directory, func(t *testing.T) {
			graph, config := compileReference(t, test.directory)
			if len(graph.Nodes) != 1 || graph.Nodes[0].Element.Name != "model.External" {
				t.Fatalf("reference reconstructed a binding/runtime species: %+v", graph.Nodes)
			}
			inputs, outputs := boundaryNames(graph)
			if !reflect.DeepEqual(inputs, test.inputs) || !reflect.DeepEqual(outputs, test.outputs) {
				t.Fatalf("boundaries = inputs %v outputs %v", inputs, outputs)
			}
			required := make([]string, 0, len(config.RequiredCapabilities))
			for _, capability := range config.RequiredCapabilities {
				required = append(required, capability.Name)
			}
			sort.Strings(required)
			if !reflect.DeepEqual(required, test.required) {
				t.Fatalf("required live capabilities = %v, want %v", required, test.required)
			}
		})
	}
}

func TestExternalModelPreparedOutputsUseNativeCompositionContracts(t *testing.T) {
	for name, pair := range map[string][2]string{
		"prepared text":  {PreparedTextType().String(), cognitionelements.PreparedTextType().String()},
		"prepared audio": {PreparedAudioType().String(), speech.AudioType().String()},
		"result":         {ResultType().String(), cognitionelements.ResultType().String()},
		"tool proposal":  {ToolProposalType().String(), cognitionelements.ToolProposalType().String()},
		"outcome":        {OutcomeType().String(), cognitionelements.OutcomeType().String()},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s contract = %s, native element uses %s", name, pair[0], pair[1])
		}
	}
}

func compileReference(t *testing.T, directory string) (ir.Graph, Config) {
	t.Helper()
	root := filepath.Join("..", "..", "graphs", "components", directory)
	topology, err := os.ReadFile(filepath.Join(root, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptor(catalog); err != nil {
		t.Fatal(err)
	}
	lockPayload, err := os.ReadFile(filepath.Join(root, "openrealtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(lockPayload)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesSource, err := os.ReadFile(filepath.Join(root, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := graphvalues.ParseYAML("agent.values.yaml", valuesSource)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, document)
	if err != nil {
		t.Fatal(err)
	}
	config, err := decodeConfig(bound.Values["foreground"])
	if err != nil {
		t.Fatal(err)
	}
	return bound.Graph, config
}

func boundaryNames(graph ir.Graph) ([]string, []string) {
	var inputs, outputs []string
	for _, boundary := range graph.Boundaries {
		switch boundary.Direction {
		case ir.InputBoundary:
			inputs = append(inputs, boundary.Name)
		case ir.OutputBoundary:
			outputs = append(outputs, boundary.Name)
		}
	}
	sort.Strings(inputs)
	sort.Strings(outputs)
	return inputs, outputs
}
