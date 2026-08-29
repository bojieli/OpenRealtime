package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

// SourceLoader resolves a graph-library import without running code or
// acquiring resources. The returned file must retain its actual source path.
type SourceLoader interface {
	Load(importerPath, importPath string) (syntax.File, error)
}

type SourceLoaderFunc func(importerPath, importPath string) (syntax.File, error)

func (function SourceLoaderFunc) Load(importerPath, importPath string) (syntax.File, error) {
	return function(importerPath, importPath)
}

// FileLoader resolves local imports relative to the importing topology.
type FileLoader struct{}

func (FileLoader) Load(importerPath, importPath string) (syntax.File, error) {
	resolved := importPath
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(importerPath), importPath)
	}
	resolved = filepath.Clean(resolved)
	body, err := os.ReadFile(resolved)
	if err != nil {
		return syntax.File{}, fmt.Errorf("read imported graph %s: %w", resolved, err)
	}
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".ortg":
		return syntax.Parse(resolved, body)
	case ".yaml", ".yml":
		return manifest.ParseYAML(resolved, body)
	case ".json":
		return manifest.ParseJSON(resolved, body)
	default:
		return syntax.File{}, fmt.Errorf("imported graph %s must use .ortg, .yaml, .yml, or .json", resolved)
	}
}

// Compile is the sole normative elaborator. Imports are expanded as typed
// subgraph instances, but their identities and hierarchy remain in Graph IR.
func Compile(file syntax.File, options Options) (Result, error) {
	if len(file.Imports) == 0 {
		return compileFlat(file, options)
	}
	if options.Loader == nil {
		failures := &Errors{}
		for _, imported := range file.Imports {
			failures.add(file.Path, "E_IMPORT_LOADER", imported.Span,
				fmt.Sprintf("graph import %q requires a source loader", imported.Path))
		}
		return Result{}, failures
	}
	expanded, err := expandModule(file, options, "", map[string]bool{})
	if err != nil {
		return Result{}, err
	}
	flatOptions := options
	flatOptions.Loader = nil
	flatOptions.lineage = append(flatOptions.lineage, expanded.lineage...)
	flatOptions.scopes = append(flatOptions.scopes, expanded.scopes...)
	result, err := compileFlat(expanded.file, flatOptions)
	if err != nil {
		return Result{}, err
	}
	result.Lock, err = mergeLock(result.Lock, expanded.lockEntries)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

type expandedGraph struct {
	file        syntax.File
	lineage     []string
	scopes      []ir.Scope
	lockEntries []resolve.Entry
}

type importedGraph struct {
	alias       string
	reference   string
	file        syntax.File
	graph       ir.Graph
	identity    element.Identity
	lineage     []string
	scopes      []ir.Scope
	lockEntries []resolve.Entry
}

type subgraphInstance struct {
	node       syntax.Node
	imported   *importedGraph
	boundaries map[string]ir.Boundary
	uses       map[string]int
}

func expandModule(file syntax.File, options Options, namespace string, stack map[string]bool) (expandedGraph, error) {
	stackKey := file.Path
	if stackKey == "" {
		stackKey = "<graph:" + file.Graph.Name + ">"
	}
	if stack[stackKey] {
		failures := &Errors{}
		failures.add(file.Path, "E_IMPORT_CYCLE", file.Graph.Span,
			fmt.Sprintf("graph import cycle reaches %s", stackKey))
		return expandedGraph{}, failures
	}
	stack[stackKey] = true
	defer delete(stack, stackKey)

	imports := make(map[string]*importedGraph, len(file.Imports))
	declaredAliases := make(map[string]struct{}, len(file.Imports))
	var accumulated expandedGraph
	for _, declaration := range file.Imports {
		childFile, err := options.Loader.Load(file.Path, declaration.Path)
		if err != nil {
			failures := &Errors{}
			failures.add(file.Path, "E_IMPORT_LOAD", declaration.Span, err.Error())
			return expandedGraph{}, failures
		}
		alias := declaration.Alias
		if alias == "" {
			alias = childFile.Graph.Name
		}
		if _, duplicate := declaredAliases[alias]; duplicate {
			failures := &Errors{}
			failures.add(file.Path, "E_IMPORT_ALIAS", declaration.Span,
				fmt.Sprintf("import alias %q is declared more than once", alias))
			return expandedGraph{}, failures
		}
		declaredAliases[alias] = struct{}{}
		used := false
		for _, node := range file.Graph.Nodes() {
			if strings.HasPrefix(node.Element, alias+".") {
				used = true
				break
			}
		}
		if !used {
			continue
		}
		childNamespace := joinReference(namespace, alias)
		childExpanded, err := expandModule(childFile, options, childNamespace, stack)
		if err != nil {
			return expandedGraph{}, err
		}
		childOptions := options
		childOptions.Loader = nil
		childOptions.lineage = append(childOptions.lineage, childExpanded.lineage...)
		childOptions.scopes = append(childOptions.scopes, childExpanded.scopes...)
		compiled, err := compileFlat(childExpanded.file, childOptions)
		if err != nil {
			return expandedGraph{}, err
		}
		descriptor, err := compositeDescriptor(compiled.Graph)
		if err != nil {
			failures := &Errors{}
			failures.add(file.Path, "E_SUBGRAPH_CONTRACT", declaration.Span, err.Error())
			return expandedGraph{}, failures
		}
		identity, err := descriptor.Identity()
		if err != nil {
			return expandedGraph{}, err
		}
		reference := joinReference(childNamespace, childFile.Graph.Name)
		imports[alias] = &importedGraph{
			alias: alias, reference: reference, file: childExpanded.file,
			graph: compiled.Graph, identity: identity, lineage: childExpanded.lineage,
			scopes: childExpanded.scopes, lockEntries: childExpanded.lockEntries,
		}
	}

	instances := make(map[string]*subgraphInstance)
	usedImports := make(map[*importedGraph]bool)
	for _, node := range file.Graph.Nodes() {
		parts := strings.Split(node.Element, ".")
		imported := imports[parts[0]]
		if imported == nil {
			continue
		}
		want := imported.alias + "." + imported.graph.ID
		if node.Element != want {
			failures := &Errors{}
			failures.add(file.Path, "E_SUBGRAPH_REFERENCE", node.Span,
				fmt.Sprintf("import alias %s exports graph %s; instantiate it as %s", imported.alias, imported.graph.ID, want))
			return expandedGraph{}, failures
		}
		boundaries := make(map[string]ir.Boundary, len(imported.graph.Boundaries))
		for _, boundary := range imported.graph.Boundaries {
			boundaries[boundary.Name] = boundary
		}
		instances[node.Name] = &subgraphInstance{
			node: node, imported: imported, boundaries: boundaries, uses: make(map[string]int),
		}
		if !usedImports[imported] {
			if err := verifyCompositeLock(options, imported.reference, imported.identity); err != nil {
				failures := &Errors{}
				failures.add(file.Path, "E_SUBGRAPH_LOCK", node.Span, err.Error())
				return expandedGraph{}, failures
			}
			usedImports[imported] = true
			accumulated.lineage = append(accumulated.lineage, imported.lineage...)
			accumulated.lineage = append(accumulated.lineage, lineageIdentity(imported.reference, imported.identity))
			accumulated.lockEntries = append(accumulated.lockEntries, imported.lockEntries...)
			accumulated.lockEntries = append(accumulated.lockEntries,
				resolve.Entry{Reference: imported.reference, Identity: imported.identity})
		}
	}

	output := syntax.File{Path: file.Path, Graph: syntax.Graph{
		Name: file.Graph.Name, Comments: append([]string(nil), file.Graph.Comments...),
		TrailingComments: append([]string(nil), file.Graph.TrailingComments...), Span: file.Graph.Span,
	}}
	for _, statement := range file.Graph.Statements {
		switch {
		case statement.Node != nil:
			instance := instances[statement.Node.Name]
			if instance == nil {
				copy := *statement.Node
				output.Graph.Statements = append(output.Graph.Statements, syntax.Statement{Node: &copy})
				continue
			}
			prefix := instance.node.Name
			for _, childStatement := range instance.imported.file.Graph.Statements {
				switch {
				case childStatement.Node != nil:
					copy := *childStatement.Node
					copy.Name = prefixID(prefix, copy.Name)
					output.Graph.Statements = append(output.Graph.Statements, syntax.Statement{Node: &copy})
				case childStatement.Edge != nil:
					copy := *childStatement.Edge
					copy.From.Node = prefixID(prefix, copy.From.Node)
					copy.To.Node = prefixID(prefix, copy.To.Node)
					if copy.Name != "" {
						copy.Name = prefixID(prefix, copy.Name)
					}
					output.Graph.Statements = append(output.Graph.Statements, syntax.Statement{Edge: &copy})
				}
			}
			accumulated.scopes = append(accumulated.scopes, instantiateScopes(prefix, instance.imported)...)
		case statement.Edge != nil:
			copy := *statement.Edge
			from, err := rewriteSubgraphEndpoint(file.Path, copy.From, ir.OutputBoundary, instances)
			if err != nil {
				return expandedGraph{}, err
			}
			to, err := rewriteSubgraphEndpoint(file.Path, copy.To, ir.InputBoundary, instances)
			if err != nil {
				return expandedGraph{}, err
			}
			copy.From, copy.To = from, to
			output.Graph.Statements = append(output.Graph.Statements, syntax.Statement{Edge: &copy})
		case statement.Boundary != nil:
			copy := *statement.Boundary
			want := ir.InputBoundary
			if copy.Direction == syntax.BoundaryOutput {
				want = ir.OutputBoundary
			}
			endpoint, err := rewriteSubgraphEndpoint(file.Path, copy.Endpoint, want, instances)
			if err != nil {
				return expandedGraph{}, err
			}
			copy.Endpoint = endpoint
			output.Graph.Statements = append(output.Graph.Statements, syntax.Statement{Boundary: &copy})
		}
	}
	for _, instance := range instances {
		for name := range instance.boundaries {
			if instance.uses[name] != 1 {
				failures := &Errors{}
				failures.add(file.Path, "E_SUBGRAPH_BOUNDARY", instance.node.Span,
					fmt.Sprintf("subgraph instance %s boundary %s requires exactly one connection, got %d",
						instance.node.Name, name, instance.uses[name]))
				return expandedGraph{}, failures
			}
		}
	}
	accumulated.file = output
	accumulated.lineage = uniqueSorted(accumulated.lineage)
	return accumulated, nil
}

func rewriteSubgraphEndpoint(
	path string,
	endpoint syntax.Endpoint,
	want ir.BoundaryDirection,
	instances map[string]*subgraphInstance,
) (syntax.Endpoint, error) {
	instance := instances[endpoint.Node]
	if instance == nil {
		return endpoint, nil
	}
	boundary, found := instance.boundaries[endpoint.Port]
	if !found {
		failures := &Errors{}
		failures.add(path, "E_SUBGRAPH_PORT", endpoint.Span,
			fmt.Sprintf("subgraph %s has no boundary port %q", endpoint.Node, endpoint.Port))
		return syntax.Endpoint{}, failures
	}
	if boundary.Direction != want {
		failures := &Errors{}
		failures.add(path, "E_SUBGRAPH_DIRECTION", endpoint.Span,
			fmt.Sprintf("subgraph boundary %s.%s is %s, want %s", endpoint.Node, endpoint.Port, boundary.Direction, want))
		return syntax.Endpoint{}, failures
	}
	instance.uses[endpoint.Port]++
	if instance.uses[endpoint.Port] > 1 {
		failures := &Errors{}
		failures.add(path, "E_SUBGRAPH_CARDINALITY", endpoint.Span,
			fmt.Sprintf("subgraph boundary %s.%s has more than one direct connection", endpoint.Node, endpoint.Port))
		return syntax.Endpoint{}, failures
	}
	return syntax.Endpoint{
		Node: prefixID(instance.node.Name, boundary.Endpoint.Node),
		Port: boundary.Endpoint.Port,
		Span: endpoint.Span,
	}, nil
}

func instantiateScopes(prefix string, imported *importedGraph) []ir.Scope {
	allNodes := make([]string, 0, len(imported.graph.Nodes))
	for _, node := range imported.graph.Nodes {
		allNodes = append(allNodes, prefixID(prefix, node.ID))
	}
	boundaries := make([]ir.ScopeBoundary, 0, len(imported.graph.Boundaries))
	for _, boundary := range imported.graph.Boundaries {
		boundaries = append(boundaries, ir.ScopeBoundary{
			Name: boundary.Name, Direction: boundary.Direction,
			Endpoint: ir.Endpoint{
				Node: prefixID(prefix, boundary.Endpoint.Node), Port: boundary.Endpoint.Port,
			},
			Type: boundary.Type,
		})
	}
	result := []ir.Scope{{
		ID: prefix, Composite: imported.identity, Nodes: allNodes, Boundaries: boundaries,
	}}
	for _, child := range imported.scopes {
		copy := child
		copy.ID = prefixID(prefix, child.ID)
		if child.Parent == "" {
			copy.Parent = prefix
		} else {
			copy.Parent = prefixID(prefix, child.Parent)
		}
		copy.Nodes = make([]string, len(child.Nodes))
		for index, node := range child.Nodes {
			copy.Nodes[index] = prefixID(prefix, node)
		}
		copy.Boundaries = append([]ir.ScopeBoundary(nil), child.Boundaries...)
		for index := range copy.Boundaries {
			copy.Boundaries[index].Endpoint.Node = prefixID(prefix, copy.Boundaries[index].Endpoint.Node)
			copy.Boundaries[index].Endpoint.Lane = ""
		}
		result = append(result, copy)
	}
	return result
}

func compositeDescriptor(graph ir.Graph) (element.Descriptor, error) {
	nodes := make(map[string]ir.Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "subgraph." + descriptorSegment(graph.ID), Revision: graph.Revision,
		CompositeFingerprint: graph.Fingerprint,
	}
	for _, boundary := range graph.Boundaries {
		direction := element.Input
		if boundary.Direction == ir.OutputBoundary {
			direction = element.Output
		}
		internal := findIRPort(nodes[boundary.Endpoint.Node], boundary.Endpoint.Port)
		port := element.Port{
			Name: boundary.Name, Direction: direction, Type: boundary.Type,
			Cardinality: element.One, Required: true,
			LossAllowed: internal.LossAllowed, DefaultDepth: internal.DefaultDepth,
		}
		descriptor.Ports = append(descriptor.Ports, port)
		if direction == element.Input {
			switch boundary.Type.Name {
			case "Trigger":
				descriptor.Reaction.Triggers = append(descriptor.Reaction.Triggers, boundary.Name)
			case "Interrupt":
				descriptor.Reaction.Interrupts = append(descriptor.Reaction.Interrupts, boundary.Name)
			case "State":
				descriptor.Reaction.SampledState = append(descriptor.Reaction.SampledState, boundary.Name)
			}
		} else if containsString(nodes[boundary.Endpoint.Node].Reaction.Outcomes, boundary.Endpoint.Port) {
			descriptor.Reaction.Outcomes = append(descriptor.Reaction.Outcomes, boundary.Name)
		}
	}
	dependencies := map[string]element.Dependency{}
	effects := map[string]element.Effect{}
	for _, node := range graph.Nodes {
		for _, dependency := range node.Dependencies {
			current, found := dependencies[dependency.Name]
			if !found || current.Optional && !dependency.Optional {
				dependencies[dependency.Name] = dependency
			}
		}
		for _, effect := range node.Effects {
			if current, found := effects[effect.Name]; found && current != effect {
				return element.Descriptor{}, fmt.Errorf("subgraph %s contains incompatible declarations of effect %s",
					graph.ID, effect.Name)
			}
			effects[effect.Name] = effect
		}
	}
	for _, dependency := range dependencies {
		descriptor.Dependencies = append(descriptor.Dependencies, dependency)
	}
	for _, effect := range effects {
		descriptor.Effects = append(descriptor.Effects, effect)
	}
	canonical, err := descriptor.Canonical()
	if err != nil {
		return element.Descriptor{}, err
	}
	return canonical, nil
}

func verifyCompositeLock(options Options, reference string, identity element.Identity) error {
	if options.ResolutionMode == resolve.Update {
		return nil
	}
	locked, found := options.Lock.Lookup(reference)
	if !found {
		return fmt.Errorf("subgraph %q is absent from the resolution lock; run an explicit graph update", reference)
	}
	if locked != identity {
		return fmt.Errorf("stale subgraph lock for %s: locked %+v, source resolves to %+v",
			reference, locked, identity)
	}
	return nil
}

func mergeLock(lock resolve.Lock, entries []resolve.Entry) (resolve.Lock, error) {
	result := lock
	byReference := make(map[string]element.Identity, len(lock.Entries)+len(entries))
	for _, entry := range lock.Entries {
		byReference[entry.Reference] = entry.Identity
	}
	for _, entry := range entries {
		if previous, found := byReference[entry.Reference]; found {
			if previous != entry.Identity {
				return resolve.Lock{}, fmt.Errorf("resolution lock reference %s resolves to two identities", entry.Reference)
			}
			continue
		}
		byReference[entry.Reference] = entry.Identity
		result.Entries = append(result.Entries, entry)
	}
	return result.Canonical()
}

func findIRPort(node ir.Node, name string) ir.Port {
	for _, port := range node.Ports {
		if port.Name == name {
			return port
		}
	}
	return ir.Port{}
}

func descriptorSegment(value string) string {
	var output strings.Builder
	for index, character := range value {
		valid := character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-'
		if !valid {
			character = '_'
		}
		if index == 0 && !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z') {
			output.WriteString("g_")
		}
		output.WriteRune(character)
	}
	if output.Len() == 0 {
		return "graph"
	}
	return output.String()
}

func prefixID(prefix, identity string) string {
	if prefix == "" {
		return identity
	}
	if identity == "" {
		return prefix
	}
	return prefix + "__" + identity
}

func joinReference(parts ...string) string {
	var nonempty []string
	for _, part := range parts {
		if part != "" {
			nonempty = append(nonempty, part)
		}
	}
	return strings.Join(nonempty, ".")
}

func lineageIdentity(reference string, identity element.Identity) string {
	return fmt.Sprintf("%s=%s@%d#%s", reference, identity.Name, identity.Revision, identity.Digest)
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
