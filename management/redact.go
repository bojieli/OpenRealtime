package management

import (
	"fmt"
	"sort"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

// RedactLive removes free-form and item identifiers that can inherit request
// payload while retaining exact graph, configuration, runtime, queue, timing,
// closed categorical authority-decision evidence, and direct causal topology.
// Flow-stage item and parent identities become snapshot-local pseudonyms. It
// is safe to apply more than once.
func RedactLive(snapshot inspect.Live) inspect.Live {
	result := snapshot
	if snapshot.Configuration != nil {
		configuration := *snapshot.Configuration
		result.Configuration = &configuration
	}
	if result.Error != "" {
		result.Error = "redacted"
	}
	result.Nodes = make(map[string]inspect.NodeLive, len(snapshot.Nodes))
	for id, node := range snapshot.Nodes {
		node = node.Clone()
		node.LastTriggerID = ""
		node.LastOutcome = ""
		if node.Error != "" {
			node.Error = "redacted"
		}
		result.Nodes[id] = node
	}
	result.Edges = make(map[string]inspect.EdgeLive, len(snapshot.Edges))
	for id, edge := range snapshot.Edges {
		edge.LastItemID = ""
		result.Edges[id] = edge
	}
	keys := make([]string, 0, len(snapshot.Flows))
	causalValues := make(map[string]struct{})
	for key := range snapshot.Flows {
		keys = append(keys, key)
		for _, stage := range snapshot.Flows[key].CausalStages {
			causalValues[stage.Item] = struct{}{}
			for _, parent := range stage.Parents {
				causalValues[parent] = struct{}{}
			}
		}
	}
	sort.Strings(keys)
	causalKeys := make([]string, 0, len(causalValues))
	for value := range causalValues {
		causalKeys = append(causalKeys, value)
	}
	sort.Strings(causalKeys)
	causalIdentities := make(map[string]string, len(causalKeys))
	for index, value := range causalKeys {
		causalIdentities[value] = fmt.Sprintf("cause_%06d", index+1)
	}
	flows := make(map[string]inspect.FlowLive, len(keys))
	for index, key := range keys {
		flow := snapshot.Flows[key].Clone()
		identity := fmt.Sprintf("flow_%06d", index+1)
		flow.Correlation = identity
		for stageIndex := range flow.CausalStages {
			stage := &flow.CausalStages[stageIndex]
			stage.Item = causalIdentities[stage.Item]
			for parentIndex, parent := range stage.Parents {
				stage.Parents[parentIndex] = causalIdentities[parent]
			}
		}
		flows[identity] = flow
	}
	result.Flows = flows
	return result
}
