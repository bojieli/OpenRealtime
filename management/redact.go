package management

import (
	"fmt"
	"sort"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

// RedactLive removes free-form and item identifiers that can inherit request
// payload while retaining exact graph, configuration, runtime, queue, and
// timing evidence. It is safe to apply more than once.
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
	for key := range snapshot.Flows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	flows := make(map[string]inspect.FlowLive, len(keys))
	for index, key := range keys {
		flow := snapshot.Flows[key].Clone()
		identity := fmt.Sprintf("flow_%06d", index+1)
		flow.Correlation = identity
		flows[identity] = flow
	}
	result.Flows = flows
	return result
}
