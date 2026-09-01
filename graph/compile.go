package graph

import (
	"fmt"
	"sort"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalidate "github.com/bojieli/OpenRealtime/graph/validate"
)

// Options controls pure elaboration. ChannelDepth contains exceptional,
// positive overrides keyed by explicit or endpoint-derived edge identity.
type Options struct {
	Catalog        *resolve.Catalog
	Lock           resolve.Lock
	ResolutionMode resolve.Mode
	Revision       uint64
	ChannelDepth   map[string]int
	Loader         SourceLoader

	lineage []string
	scopes  []ir.Scope
}

// Result is the canonical IR and the exact minimal lock consumed or generated
// for it.
type Result struct {
	Graph ir.Graph
	Lock  resolve.Lock
}

type nodeState struct {
	source     syntax.Node
	descriptor element.Descriptor
	identity   element.Identity
	ports      map[string]*portState
}

type portState struct {
	descriptor element.Port
	typeTerm   typeTerm
	uses       []portUse
}

type portUse struct {
	id   string
	span syntax.Span
}

type edgeState struct {
	source syntax.Edge
	id     string
	from   *nodePort
	to     *nodePort
}

type boundaryState struct {
	source syntax.Boundary
	port   *nodePort
}

type nodePort struct {
	node *nodeState
	port *portState
}

// Compile resolves, checks, and freezes one parsed topology without acquiring
// resources. The same function is used by every authoring frontend.
func compileFlat(file syntax.File, options Options) (Result, error) {
	failures := &Errors{}
	path := file.Path
	if options.Catalog == nil {
		failures.add(path, "E_CATALOG", file.Graph.Span, "graph compilation requires an element catalog")
		return Result{}, failures
	}
	if len(file.Imports) != 0 {
		// Descriptor namespaces need no import. Imports are reserved for graph
		// libraries/subgraphs and must not be silently ignored.
		for _, imported := range file.Imports {
			failures.add(path, "E_IMPORT_UNSUPPORTED", imported.Span,
				fmt.Sprintf("graph-library import %q is not yet resolved by this compiler", imported.Path))
		}
		return Result{}, failures
	}

	nodeSources := file.Graph.Nodes()
	if len(nodeSources) == 0 {
		failures.add(path, "E_EMPTY_GRAPH", file.Graph.Span, "a graph must instantiate at least one element")
		return Result{}, failures
	}
	nodes := make(map[string]*nodeState, len(nodeSources))
	references := make([]string, 0, len(nodeSources))
	for _, source := range nodeSources {
		if previous, duplicate := nodes[source.Name]; duplicate {
			failures.add(path, "E_DUPLICATE_NODE", source.Span,
				fmt.Sprintf("node instance %q is declared more than once", source.Name),
				fmt.Sprintf("first declaration starts at %d:%d", previous.source.Span.Start.Line, previous.source.Span.Start.Column))
			continue
		}
		nodes[source.Name] = &nodeState{source: source}
		references = append(references, source.Element)
	}
	if failures.any() {
		return Result{}, failures
	}

	resolution, err := resolve.Resolve(options.Catalog, references, options.Lock, options.ResolutionMode)
	if err != nil {
		failures.add(path, "E_RESOLUTION", file.Graph.Span, err.Error())
		return Result{}, failures
	}
	for _, node := range nodes {
		descriptor := resolution.Descriptors[node.source.Element].Clone()
		identity, identityErr := descriptor.Identity()
		if identityErr != nil {
			failures.add(path, "E_DESCRIPTOR", node.source.Span, identityErr.Error())
			continue
		}
		node.descriptor = descriptor
		node.identity = identity
		node.ports = make(map[string]*portState, len(descriptor.Ports))
		for _, port := range descriptor.Ports {
			copy := port
			node.ports[port.Name] = &portState{
				descriptor: copy,
				typeTerm:   scopedType(node.source.Name, port.Type),
			}
		}
	}
	if failures.any() {
		return Result{}, failures
	}

	solver := newUnifier()
	edges := compileEdges(path, file.Graph.Edges(), nodes, solver, failures)
	boundaries := compileBoundaries(path, file.Graph.Boundaries(), nodes, failures)
	validateConnections(path, nodes, failures)
	if failures.any() {
		return Result{}, failures
	}

	resultGraph := ir.Graph{
		FormatVersion: ir.FormatVersion,
		ID:            file.Graph.Name,
		Revision:      options.Revision,
		Lineage:       append([]string(nil), options.lineage...),
		Scopes:        append([]ir.Scope(nil), options.scopes...),
	}
	if resultGraph.Revision == 0 {
		resultGraph.Revision = 1
	}
	resultGraph.Nodes = emitNodes(path, nodes, solver)
	resultGraph.Edges = emitEdges(path, edges, solver, options.ChannelDepth, failures)
	resultGraph.Boundaries = emitBoundaries(path, boundaries, solver, failures)
	validateOverrides(path, file.Graph.Span, options.ChannelDepth, resultGraph.Edges, failures)
	if failures.any() {
		return Result{}, failures
	}
	frozen, err := ir.Freeze(resultGraph)
	if err != nil {
		failures.add(path, "E_INVALID_IR", file.Graph.Span, err.Error())
		return Result{}, failures
	}
	for _, finding := range graphvalidate.Errors(graphvalidate.Check(frozen, graphvalidate.Core)) {
		failures.add(path, finding.Code, findingSpan(file, finding.Node, finding.Edge, finding.Boundary), finding.Message)
	}
	if failures.any() {
		return Result{}, failures
	}
	return Result{Graph: frozen, Lock: resolution.Lock}, nil
}

func compileEdges(
	path string,
	sources []syntax.Edge,
	nodes map[string]*nodeState,
	solver *unifier,
	failures *Errors,
) []edgeState {
	result := make([]edgeState, 0, len(sources))
	identities := make(map[string]syntax.Span, len(sources))
	for _, source := range sources {
		identity := source.Identity()
		if previous, duplicate := identities[identity]; duplicate {
			failures.add(path, "E_DUPLICATE_EDGE", source.Span,
				fmt.Sprintf("edge identity %q is not unique", identity),
				fmt.Sprintf("first connection starts at %d:%d", previous.Start.Line, previous.Start.Column))
			continue
		}
		identities[identity] = source.Span
		from := lookupPort(path, source.From, element.Output, nodes, failures)
		to := lookupPort(path, source.To, element.Input, nodes, failures)
		if from == nil || to == nil {
			continue
		}
		from.port.uses = append(from.port.uses, portUse{id: identity, span: source.Span})
		to.port.uses = append(to.port.uses, portUse{id: identity, span: source.Span})
		if err := solver.tryUnify(from.port.typeTerm, to.port.typeTerm); err != nil {
			failures.add(path, "E_TYPE_MISMATCH", source.Span,
				fmt.Sprintf("%s produces %s but %s accepts %s: %v",
					source.From.String(), from.port.descriptor.Type.String(),
					source.To.String(), to.port.descriptor.Type.String(), err))
		}
		if source.Delivery == syntax.Lossy {
			if !from.port.descriptor.LossAllowed || !to.port.descriptor.LossAllowed {
				failures.add(path, "E_LOSS_FORBIDDEN", source.Span,
					fmt.Sprintf("lossy edge %s => %s is forbidden by an endpoint contract",
						source.From.String(), source.To.String()))
			}
			if forbiddenLossProtocol(solver.resolve(from.port.typeTerm)) {
				failures.add(path, "E_LOSS_FORBIDDEN", source.Span,
					fmt.Sprintf("protocol %s cannot discard individual channel items",
						solver.resolve(from.port.typeTerm).String()))
			}
		}
		result = append(result, edgeState{source: source, id: identity, from: from, to: to})
	}
	return result
}

func compileBoundaries(
	path string,
	sources []syntax.Boundary,
	nodes map[string]*nodeState,
	failures *Errors,
) []boundaryState {
	result := make([]boundaryState, 0, len(sources))
	names := make(map[string]syntax.Span, len(sources))
	for _, source := range sources {
		if previous, duplicate := names[source.Name]; duplicate {
			failures.add(path, "E_DUPLICATE_BOUNDARY", source.Span,
				fmt.Sprintf("boundary %q is declared more than once", source.Name),
				fmt.Sprintf("first declaration starts at %d:%d", previous.Start.Line, previous.Start.Column))
			continue
		}
		names[source.Name] = source.Span
		want := element.Input
		if source.Direction == syntax.BoundaryOutput {
			want = element.Output
		}
		port := lookupPort(path, source.Endpoint, want, nodes, failures)
		if port == nil {
			continue
		}
		port.port.uses = append(port.port.uses, portUse{id: "boundary:" + source.Name, span: source.Span})
		result = append(result, boundaryState{source: source, port: port})
	}
	return result
}

func lookupPort(
	path string,
	endpoint syntax.Endpoint,
	want element.Direction,
	nodes map[string]*nodeState,
	failures *Errors,
) *nodePort {
	node, found := nodes[endpoint.Node]
	if !found {
		failures.add(path, "E_UNKNOWN_NODE", endpoint.Span,
			fmt.Sprintf("endpoint %s refers to unknown node %q", endpoint.String(), endpoint.Node))
		return nil
	}
	port, found := node.ports[endpoint.Port]
	if !found {
		failures.add(path, "E_UNKNOWN_PORT", endpoint.Span,
			fmt.Sprintf("element %s has no port %q", node.descriptor.Name, endpoint.Port))
		return nil
	}
	if port.descriptor.Direction != want {
		failures.add(path, "E_PORT_DIRECTION", endpoint.Span,
			fmt.Sprintf("%s is an %s port; this endpoint requires %s",
				endpoint.String(), port.descriptor.Direction, want))
		return nil
	}
	return &nodePort{node: node, port: port}
}

func validateConnections(path string, nodes map[string]*nodeState, failures *Errors) {
	for _, node := range sortedNodes(nodes) {
		portNames := make([]string, 0, len(node.ports))
		for name := range node.ports {
			portNames = append(portNames, name)
		}
		sort.Strings(portNames)
		for _, name := range portNames {
			port := node.ports[name]
			if port.descriptor.Cardinality == element.One && len(port.uses) > 1 {
				kind := "consumers"
				code := "E_IMPLICIT_FANOUT"
				if port.descriptor.Direction == element.Input {
					kind = "writers"
					code = "E_MULTIPLE_WRITERS"
				}
				last := port.uses[len(port.uses)-1]
				failures.add(path, code, last.span,
					fmt.Sprintf("singular %s port %s.%s has %d direct %s; insert an explicit connector",
						port.descriptor.Direction, node.source.Name, name, len(port.uses), kind))
			}
			minimum := port.descriptor.MinConnections
			if port.descriptor.Required && minimum < 1 {
				minimum = 1
			}
			if len(port.uses) < minimum {
				failures.add(path, "E_REQUIRED_PORT", node.source.Span,
					fmt.Sprintf("port %s.%s requires at least %d connection(s), got %d",
						node.source.Name, name, minimum, len(port.uses)))
			}
		}
	}
}

func emitNodes(path string, nodes map[string]*nodeState, solver *unifier) []ir.Node {
	result := make([]ir.Node, 0, len(nodes))
	for _, node := range sortedNodes(nodes) {
		ports := make([]ir.Port, 0, len(node.ports))
		portNames := make([]string, 0, len(node.ports))
		for name := range node.ports {
			portNames = append(portNames, name)
		}
		sort.Strings(portNames)
		for _, name := range portNames {
			state := node.ports[name]
			port := state.descriptor
			lanes := make([]string, 0, len(state.uses))
			if port.Cardinality == element.Variadic {
				for _, use := range state.uses {
					lanes = append(lanes, use.id)
				}
			}
			ports = append(ports, ir.Port{
				Name: port.Name, Direction: port.Direction, Type: solver.resolve(state.typeTerm),
				Cardinality: port.Cardinality, Required: port.Required,
				MinConnections: port.MinConnections, LossAllowed: port.LossAllowed,
				DefaultDepth: port.DefaultDepth, Lanes: lanes,
			})
		}
		result = append(result, ir.Node{
			ID: node.source.Name, Element: node.identity, Ports: ports,
			Reaction: node.descriptor.Reaction, StateSchema: node.descriptor.StateSchema,
			ConfigSchema: node.descriptor.ConfigSchema,
			Dependencies: node.descriptor.Dependencies, Effects: node.descriptor.Effects,
			Source: source(path, node.source.Span),
		})
	}
	return result
}

func emitEdges(
	path string,
	edges []edgeState,
	solver *unifier,
	overrides map[string]int,
	failures *Errors,
) []ir.Edge {
	result := make([]ir.Edge, 0, len(edges))
	for _, edge := range edges {
		fromType := solver.resolve(edge.from.port.typeTerm)
		toType := solver.resolve(edge.to.port.typeTerm)
		if fromType.ContainsVariable() || toType.ContainsVariable() {
			failures.add(path, "E_UNRESOLVED_TYPE", edge.source.Span,
				fmt.Sprintf("edge %s cannot infer a concrete type from %s and %s",
					edge.id, fromType.String(), toType.String()))
			continue
		}
		if !fromType.Equal(toType) {
			failures.add(path, "E_TYPE_MISMATCH", edge.source.Span,
				fmt.Sprintf("edge %s resolved incompatible types %s and %s",
					edge.id, fromType.String(), toType.String()))
			continue
		}
		depth := effectiveDepth(fromType, edge.from.port.descriptor, edge.to.port.descriptor)
		if override, found := overrides[edge.id]; found {
			if override <= 0 {
				failures.add(path, "E_CHANNEL_DEPTH", edge.source.Span,
					fmt.Sprintf("edge %s depth override must be positive, got %d", edge.id, override))
				continue
			}
			depth = override
		}
		delivery := ir.Lossless
		if edge.source.Delivery == syntax.Lossy {
			delivery = ir.Lossy
		}
		result = append(result, ir.Edge{
			ID:   edge.id,
			From: endpointIR(edge.from, edge.id), To: endpointIR(edge.to, edge.id),
			Type: fromType, Delivery: delivery, Ordering: "fifo", Depth: depth,
			Source: source(path, edge.source.Span),
		})
	}
	return result
}

func emitBoundaries(path string, boundaries []boundaryState, solver *unifier, failures *Errors) []ir.Boundary {
	result := make([]ir.Boundary, 0, len(boundaries))
	for _, boundary := range boundaries {
		valueType := solver.resolve(boundary.port.port.typeTerm)
		if valueType.ContainsVariable() {
			failures.add(path, "E_UNRESOLVED_TYPE", boundary.source.Span,
				fmt.Sprintf("boundary %s cannot infer a concrete type for %s",
					boundary.source.Name, boundary.source.Endpoint.String()))
			continue
		}
		direction := ir.InputBoundary
		if boundary.source.Direction == syntax.BoundaryOutput {
			direction = ir.OutputBoundary
		}
		result = append(result, ir.Boundary{
			Name: boundary.source.Name, Direction: direction,
			Endpoint: endpointIR(boundary.port, "boundary:"+boundary.source.Name),
			Type:     valueType, Source: source(path, boundary.source.Span),
		})
	}
	return result
}

func endpointIR(port *nodePort, useID string) ir.Endpoint {
	endpoint := ir.Endpoint{Node: port.node.source.Name, Port: port.port.descriptor.Name}
	if port.port.descriptor.Cardinality == element.Variadic {
		endpoint.Lane = useID
	}
	return endpoint
}

func effectiveDepth(valueType element.Type, from, to element.Port) int {
	if to.DefaultDepth > 0 {
		return to.DefaultDepth
	}
	if from.DefaultDepth > 0 {
		return from.DefaultDepth
	}
	switch valueType.Name {
	case "State":
		return 1
	case "Stream", "Segmented", "Revisions":
		return 32
	default:
		return 16
	}
}

func forbiddenLossProtocol(valueType element.Type) bool {
	switch valueType.Name {
	case "Trigger", "Interrupt", "Request", "Reply", "Segmented":
		return true
	default:
		return false
	}
}

func validateOverrides(path string, span syntax.Span, overrides map[string]int, edges []ir.Edge, failures *Errors) {
	known := make(map[string]struct{}, len(edges))
	for _, edge := range edges {
		known[edge.ID] = struct{}{}
	}
	keys := make([]string, 0, len(overrides))
	for identity := range overrides {
		keys = append(keys, identity)
	}
	sort.Strings(keys)
	for _, identity := range keys {
		if _, found := known[identity]; !found {
			failures.add(path, "E_UNKNOWN_CHANNEL", span,
				fmt.Sprintf("channel depth override refers to unknown edge %q", identity))
		}
	}
}

func sortedNodes(nodes map[string]*nodeState) []*nodeState {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]*nodeState, 0, len(names))
	for _, name := range names {
		result = append(result, nodes[name])
	}
	return result
}

func source(path string, span syntax.Span) *ir.Source {
	return &ir.Source{
		Path: path, StartOffset: span.Start.Offset, EndOffset: span.End.Offset,
		Line: span.Start.Line, Column: span.Start.Column,
	}
}

func findingSpan(file syntax.File, node, edge, boundary string) syntax.Span {
	if edge != "" {
		for _, candidate := range file.Graph.Edges() {
			identity := candidate.Name
			if identity == "" {
				identity = candidate.From.String() + "->" + candidate.To.String()
			}
			if identity == edge {
				return candidate.Span
			}
		}
	}
	if boundary != "" {
		for _, candidate := range file.Graph.Boundaries() {
			if candidate.Name == boundary {
				return candidate.Span
			}
		}
	}
	if node != "" {
		for _, candidate := range file.Graph.Nodes() {
			if candidate.Name == node {
				return candidate.Span
			}
		}
	}
	return file.Graph.Span
}
