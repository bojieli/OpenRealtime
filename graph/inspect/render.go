package inspect

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"sort"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/ir"
)

// Mermaid generates a documentation view. It is deliberately derived output,
// never an accepted executable graph source.
func Mermaid(graph ir.Graph) (string, error) {
	model, err := Build(graph)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	output.WriteString("%% Generated from OpenRealtime Graph IR; fingerprint: ")
	output.WriteString(model.Fingerprint)
	output.WriteString("\nflowchart LR\n")
	for _, boundary := range model.Boundaries {
		output.WriteString("    ")
		output.WriteString(mermaidBoundaryID(boundary.Name))
		output.WriteString("{{\"")
		output.WriteString(mermaidText(string(boundary.Direction) + " " + boundary.Name + "\n" + boundary.Type))
		output.WriteString("\"}}\n")
	}
	writeMermaidHierarchy(&output, model)

	linkIndex := 0
	var styles []string
	writeLink := func(from, to, label, role string, delivery ir.Delivery) {
		output.WriteString("    ")
		output.WriteString(from)
		output.WriteString(" -->|\"")
		output.WriteString(mermaidText(label))
		output.WriteString("\"| ")
		output.WriteString(to)
		output.WriteByte('\n')
		style := mermaidLinkStyle(role, delivery)
		if style != "" {
			styles = append(styles, "    linkStyle "+strconv.Itoa(linkIndex)+" "+style+"\n")
		}
		linkIndex++
	}
	for _, boundary := range model.Boundaries {
		boundaryID := mermaidBoundaryID(boundary.Name)
		nodeID := mermaidNodeID(boundary.Endpoint.Node)
		label := boundary.Endpoint.Port + " · " + boundary.Type
		if boundary.Direction == ir.InputBoundary {
			writeLink(boundaryID, nodeID, label, boundary.Role, ir.Lossless)
		} else {
			writeLink(nodeID, boundaryID, label, boundary.Role, ir.Lossless)
		}
	}
	for _, edge := range model.Edges {
		arrow := "→"
		if edge.Delivery == ir.Lossy {
			arrow = "⇒"
		}
		label := edge.From.Port + " " + arrow + " " + edge.To.Port + "\n" +
			edge.Type + " · depth " + strconv.Itoa(edge.Depth)
		writeLink(mermaidNodeID(edge.From.Node), mermaidNodeID(edge.To.Node), label, edge.Role, edge.Delivery)
	}
	for _, style := range styles {
		output.WriteString(style)
	}
	output.WriteString("    classDef ortBoundary fill:#f6f8fa,stroke:#57606a,stroke-width:1px;\n")
	if len(model.Boundaries) > 0 {
		output.WriteString("    class ")
		for index, boundary := range model.Boundaries {
			if index > 0 {
				output.WriteByte(',')
			}
			output.WriteString(mermaidBoundaryID(boundary.Name))
		}
		output.WriteString(" ortBoundary;\n")
	}
	return output.String(), nil
}

// DOT generates an exact static Graphviz view.
func DOT(graph ir.Graph) (string, error) {
	model, err := Build(graph)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	output.WriteString("// Generated from OpenRealtime Graph IR; fingerprint: ")
	output.WriteString(model.Fingerprint)
	output.WriteString("\ndigraph OpenRealtime {\n")
	output.WriteString("  rankdir=LR;\n  graph [label=")
	output.WriteString(strconv.Quote(model.GraphID + "\n" + model.Fingerprint))
	output.WriteString(", labelloc=t];\n  node [shape=box];\n")
	for _, boundary := range model.Boundaries {
		output.WriteString("  ")
		output.WriteString(strconv.Quote(dotBoundaryID(boundary.Name)))
		output.WriteString(" [shape=diamond,label=")
		output.WriteString(strconv.Quote(string(boundary.Direction) + " " + boundary.Name + "\n" + boundary.Type))
		output.WriteString("];\n")
	}
	writeDOTHierarchy(&output, model)
	writeEdge := func(from, to, label, role string, delivery ir.Delivery) {
		output.WriteString("  ")
		output.WriteString(strconv.Quote(from))
		output.WriteString(" -> ")
		output.WriteString(strconv.Quote(to))
		output.WriteString(" [label=")
		output.WriteString(strconv.Quote(label))
		if delivery == ir.Lossy {
			output.WriteString(",style=dashed")
		}
		if color := dotRoleColor(role); color != "" {
			output.WriteString(",color=")
			output.WriteString(strconv.Quote(color))
		}
		output.WriteString("];\n")
	}
	for _, boundary := range model.Boundaries {
		label := boundary.Endpoint.Port + "\n" + boundary.Type
		if boundary.Direction == ir.InputBoundary {
			writeEdge(dotBoundaryID(boundary.Name), boundary.Endpoint.Node, label, boundary.Role, ir.Lossless)
		} else {
			writeEdge(boundary.Endpoint.Node, dotBoundaryID(boundary.Name), label, boundary.Role, ir.Lossless)
		}
	}
	for _, edge := range model.Edges {
		label := edge.From.Port + " → " + edge.To.Port + "\n" + edge.Type + " · depth " + strconv.Itoa(edge.Depth)
		writeEdge(edge.From.Node, edge.To.Node, label, edge.Role, edge.Delivery)
	}
	output.WriteString("}\n")
	return output.String(), nil
}

func mermaidNodeID(identity string) string     { return "n_" + shortHash("node:"+identity) }
func mermaidBoundaryID(identity string) string { return "b_" + shortHash("boundary:"+identity) }
func dotBoundaryID(identity string) string     { return "boundary:" + identity }

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func mermaidText(value string) string {
	value = html.EscapeString(value)
	value = strings.ReplaceAll(value, "\n", "<br/>")
	return strings.ReplaceAll(value, "`", "&#96;")
}

func mermaidLinkStyle(role string, delivery ir.Delivery) string {
	var parts []string
	if delivery == ir.Lossy {
		parts = append(parts, "stroke-dasharray:5 5")
	}
	switch role {
	case "trigger":
		parts = append(parts, "stroke:#1f6feb", "stroke-width:2px")
	case "interrupt":
		parts = append(parts, "stroke:#cf222e", "stroke-width:2px")
	case "state":
		parts = append(parts, "stroke:#8250df")
	case "control":
		parts = append(parts, "stroke:#bf8700")
	}
	return strings.Join(parts, ",")
}

func dotRoleColor(role string) string {
	switch role {
	case "trigger":
		return "#1f6feb"
	case "interrupt":
		return "#cf222e"
	case "state":
		return "#8250df"
	case "control":
		return "#bf8700"
	default:
		return ""
	}
}

func writeMermaidHierarchy(output *strings.Builder, model Model) {
	nodes := make(map[string]Node, len(model.Nodes))
	for _, node := range model.Nodes {
		nodes[node.ID] = node
	}
	scopes := make(map[string]Scope, len(model.Scopes))
	children := make(map[string][]string)
	for _, scope := range model.Scopes {
		scopes[scope.ID] = scope
		children[scope.Parent] = append(children[scope.Parent], scope.ID)
	}
	for parent := range children {
		sort.Strings(children[parent])
	}
	owner := deepestScopeOwners(model.Scopes)
	writeNode := func(node Node, indentation string) {
		output.WriteString(indentation)
		output.WriteString(mermaidNodeID(node.ID))
		output.WriteString("[\"")
		output.WriteString(mermaidText(nodeInspectionLabel(node)))
		output.WriteString("\"]\n")
	}
	var writeScope func(string, string)
	writeScope = func(scopeID, indentation string) {
		scope := scopes[scopeID]
		output.WriteString(indentation)
		output.WriteString("subgraph ")
		output.WriteString(mermaidScopeID(scope.ID))
		output.WriteString("[\"")
		output.WriteString(mermaidText(scope.ID + "\n" + scope.Composite.Name + "@" +
			strconv.FormatUint(scope.Composite.Revision, 10)))
		output.WriteString("\"]\n")
		for _, nodeID := range scope.Nodes {
			if owner[nodeID] == scope.ID {
				writeNode(nodes[nodeID], indentation+"    ")
			}
		}
		for _, child := range children[scope.ID] {
			writeScope(child, indentation+"    ")
		}
		output.WriteString(indentation)
		output.WriteString("end\n")
	}
	for _, node := range model.Nodes {
		if owner[node.ID] == "" {
			writeNode(node, "    ")
		}
	}
	for _, scope := range children[""] {
		writeScope(scope, "    ")
	}
}

func nodeInspectionLabel(node Node) string {
	lines := []string{node.ID, node.Element.Name + "@" + strconv.FormatUint(node.Element.Revision, 10)}
	for _, reaction := range []struct {
		label string
		ports []string
	}{
		{label: "trigger", ports: node.Reaction.Triggers},
		{label: "sample", ports: node.Reaction.SampledState},
		{label: "interrupt", ports: node.Reaction.Interrupts},
		{label: "outcome", ports: node.Reaction.Outcomes},
	} {
		if len(reaction.ports) != 0 {
			lines = append(lines, reaction.label+": "+strings.Join(reaction.ports, ", "))
		}
	}
	if node.Reaction.MaxConcurrency != 0 {
		lines = append(lines, "max concurrency: "+strconv.Itoa(node.Reaction.MaxConcurrency))
	}
	if node.Reaction.BreaksCycles {
		lines = append(lines, "causal break: true")
	}
	if node.StateSchema != "" {
		lines = append(lines, "state schema: "+node.StateSchema)
	}
	if node.StateTransfer != nil {
		capabilities := make([]string, 0, 3)
		if node.StateTransfer.Snapshot {
			capabilities = append(capabilities, "snapshot")
		}
		if node.StateTransfer.Restore {
			capabilities = append(capabilities, "restore")
		}
		if node.StateTransfer.Quiesce {
			capabilities = append(capabilities, "quiesce")
		}
		lines = append(lines, "state transfer: "+strings.Join(capabilities, ", "))
	}
	for _, effect := range node.Effects {
		attributes := make([]string, 0, 2)
		if effect.External {
			attributes = append(attributes, "external", "irreversible")
		} else if effect.Reversible {
			attributes = append(attributes, "reversible")
		} else {
			attributes = append(attributes, "irreversible")
		}
		if effect.Authority != "" {
			attributes = append(attributes, "authority="+effect.Authority)
		}
		lines = append(lines, "effect: "+effect.Name+" ["+strings.Join(attributes, ", ")+"]")
	}
	return strings.Join(lines, "\n")
}

func writeDOTHierarchy(output *strings.Builder, model Model) {
	nodes := make(map[string]Node, len(model.Nodes))
	for _, node := range model.Nodes {
		nodes[node.ID] = node
	}
	scopes := make(map[string]Scope, len(model.Scopes))
	children := make(map[string][]string)
	for _, scope := range model.Scopes {
		scopes[scope.ID] = scope
		children[scope.Parent] = append(children[scope.Parent], scope.ID)
	}
	for parent := range children {
		sort.Strings(children[parent])
	}
	owner := deepestScopeOwners(model.Scopes)
	writeNode := func(node Node, indentation string) {
		output.WriteString(indentation)
		output.WriteString(strconv.Quote(node.ID))
		output.WriteString(" [label=")
		output.WriteString(strconv.Quote(nodeInspectionLabel(node)))
		output.WriteString("];\n")
	}
	var writeScope func(string, string)
	writeScope = func(scopeID, indentation string) {
		scope := scopes[scopeID]
		output.WriteString(indentation)
		output.WriteString("subgraph ")
		output.WriteString(strconv.Quote("cluster:" + scope.ID))
		output.WriteString(" {\n")
		output.WriteString(indentation + "  label=")
		output.WriteString(strconv.Quote(scope.ID + "\n" + scope.Composite.Name + "@" + strconv.FormatUint(scope.Composite.Revision, 10)))
		output.WriteString(";\n")
		for _, nodeID := range scope.Nodes {
			if owner[nodeID] == scope.ID {
				writeNode(nodes[nodeID], indentation+"  ")
			}
		}
		for _, child := range children[scope.ID] {
			writeScope(child, indentation+"  ")
		}
		output.WriteString(indentation + "}\n")
	}
	for _, node := range model.Nodes {
		if owner[node.ID] == "" {
			writeNode(node, "  ")
		}
	}
	for _, scope := range children[""] {
		writeScope(scope, "  ")
	}
}

func deepestScopeOwners(scopes []Scope) map[string]string {
	byID := make(map[string]Scope, len(scopes))
	for _, scope := range scopes {
		byID[scope.ID] = scope
	}
	depth := func(scope Scope) int {
		result := 1
		for scope.Parent != "" {
			result++
			scope = byID[scope.Parent]
		}
		return result
	}
	owners := map[string]string{}
	ownerDepth := map[string]int{}
	for _, scope := range scopes {
		candidateDepth := depth(scope)
		for _, node := range scope.Nodes {
			if candidateDepth > ownerDepth[node] {
				owners[node] = scope.ID
				ownerDepth[node] = candidateDepth
			}
		}
	}
	return owners
}

func mermaidScopeID(identity string) string { return "s_" + shortHash("scope:"+identity) }
