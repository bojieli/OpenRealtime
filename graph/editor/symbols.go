package editor

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

var nodeNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

type symbol struct {
	kind     SymbolKind
	span     syntax.Span
	node     string
	element  string
	port     string
	expected element.Direction
}

type nodeRecord struct {
	name        string
	element     string
	declaration syntax.Span
	references  []syntax.Span
	ambiguous   bool
}

func buildSymbolIndex(
	source []byte, file syntax.File, entries map[string]catalogEntry,
) ([]symbol, map[string]nodeRecord, error) {
	_ = entries // Resolution is intentionally deferred to individual queries.
	positions := newSourcePositions(source)
	nodes := make(map[string]nodeRecord)
	var symbols []symbol
	for _, node := range file.Graph.Nodes() {
		start, end := node.Span.Start.Offset, node.Span.End.Offset
		want := node.Element + " :: " + node.Name + ";"
		if start < 0 || end > len(source) || start > end || string(source[start:end]) != want {
			return nil, nil, fmt.Errorf("canonical node span for %q does not match source", node.Name)
		}
		elementSpan, err := sourceSpan(positions, start, start+len(node.Element))
		if err != nil {
			return nil, nil, err
		}
		nameStart := end - 1 - len(node.Name)
		nameSpan, err := sourceSpan(positions, nameStart, end-1)
		if err != nil {
			return nil, nil, err
		}
		symbols = append(symbols,
			symbol{kind: SymbolElement, span: elementSpan, node: node.Name, element: node.Element},
			symbol{kind: SymbolNodeDeclaration, span: nameSpan, node: node.Name, element: node.Element},
		)
		if previous, duplicate := nodes[node.Name]; duplicate {
			previous.ambiguous = true
			nodes[node.Name] = previous
			continue
		}
		nodes[node.Name] = nodeRecord{
			name: node.Name, element: node.Element, declaration: nameSpan,
			references: []syntax.Span{nameSpan},
		}
	}
	addEndpoint := func(endpoint syntax.Endpoint, expected element.Direction) error {
		start, end := endpoint.Span.Start.Offset, endpoint.Span.End.Offset
		want := endpoint.Node + "." + endpoint.Port
		if start < 0 || end > len(source) || start > end || string(source[start:end]) != want {
			return fmt.Errorf("canonical endpoint span for %q does not match source", want)
		}
		nodeSpan, err := sourceSpan(positions, start, start+len(endpoint.Node))
		if err != nil {
			return err
		}
		portSpan, err := sourceSpan(positions, start+len(endpoint.Node)+1, end)
		if err != nil {
			return err
		}
		record, found := nodes[endpoint.Node]
		elementReference := ""
		if found {
			// A duplicate declaration does not have one trustworthy element
			// contract. Keep the reference unresolved instead of borrowing the
			// descriptor from whichever declaration happened to be indexed first.
			if !record.ambiguous {
				elementReference = record.element
			}
			record.references = append(record.references, nodeSpan)
			nodes[endpoint.Node] = record
		}
		symbols = append(symbols,
			symbol{kind: SymbolNodeReference, span: nodeSpan, node: endpoint.Node,
				element: elementReference, port: endpoint.Port, expected: expected},
			symbol{kind: SymbolPortReference, span: portSpan, node: endpoint.Node,
				element: elementReference, port: endpoint.Port, expected: expected},
		)
		return nil
	}
	for _, edge := range file.Graph.Edges() {
		if err := addEndpoint(edge.From, element.Output); err != nil {
			return nil, nil, err
		}
		if err := addEndpoint(edge.To, element.Input); err != nil {
			return nil, nil, err
		}
	}
	for _, boundary := range file.Graph.Boundaries() {
		expected := element.Input
		if boundary.Direction == syntax.BoundaryOutput {
			expected = element.Output
		}
		if err := addEndpoint(boundary.Endpoint, expected); err != nil {
			return nil, nil, err
		}
	}
	for name, record := range nodes {
		sort.Slice(record.references, func(left, right int) bool {
			return record.references[left].Start.Offset < record.references[right].Start.Offset
		})
		nodes[name] = record
	}
	sort.SliceStable(symbols, func(left, right int) bool {
		if symbols[left].span.Start.Offset != symbols[right].span.Start.Offset {
			return symbols[left].span.Start.Offset < symbols[right].span.Start.Offset
		}
		if symbols[left].span.End.Offset != symbols[right].span.End.Offset {
			return symbols[left].span.End.Offset < symbols[right].span.End.Offset
		}
		return symbols[left].kind < symbols[right].kind
	})
	return symbols, nodes, nil
}

func sourceSpan(positions sourcePositions, start, end int) (syntax.Span, error) {
	if start < 0 || end < start || end > len(positions.source) {
		return syntax.Span{}, fmt.Errorf("%w: source span [%d,%d) is invalid", ErrInvalidPosition, start, end)
	}
	left, err := positions.at(start)
	if err != nil {
		return syntax.Span{}, err
	}
	right, err := positions.at(end)
	if err != nil {
		return syntax.Span{}, err
	}
	return syntax.Span{Start: left, End: right}, nil
}

func (document *Document) symbolAt(cursor Cursor) (symbol, error) {
	if document == nil || !document.parsed || !document.canonical {
		return symbol{}, ErrSyntaxUnavailable
	}
	if err := document.validateCursor(cursor); err != nil {
		return symbol{}, err
	}
	offset := cursor.Position.Offset
	var (
		match symbol
		found bool
	)
	for _, candidate := range document.symbols {
		if offset < candidate.span.Start.Offset || offset > candidate.span.End.Offset {
			continue
		}
		if !found || spanLength(candidate.span) < spanLength(match.span) {
			match, found = candidate, true
		}
	}
	if !found {
		return symbol{}, ErrNoSymbol
	}
	return match, nil
}

func spanLength(span syntax.Span) int { return span.End.Offset - span.Start.Offset }

// Complete dispatches to element, node, or port completion according to the
// exact symbol under the cursor.
func (document *Document) Complete(cursor Cursor) (CompletionList, error) {
	value, err := document.symbolAt(cursor)
	if err != nil {
		if err == ErrNoSymbol {
			return CompletionList{}, ErrNoCompletionContext
		}
		return CompletionList{}, err
	}
	switch value.kind {
	case SymbolElement:
		return document.completeElements(cursor, value), nil
	case SymbolNodeReference:
		return document.completeNodes(cursor, value), nil
	case SymbolPortReference:
		return document.completePorts(cursor, value), nil
	default:
		return CompletionList{}, ErrNoCompletionContext
	}
}

func (document *Document) CompleteElements(cursor Cursor) (CompletionList, error) {
	value, err := document.symbolAt(cursor)
	if err != nil {
		return CompletionList{}, err
	}
	if value.kind != SymbolElement {
		return CompletionList{}, ErrNoCompletionContext
	}
	return document.completeElements(cursor, value), nil
}

// ElementCompletions returns catalog-derived declaration completions at an
// arbitrary valid insertion cursor. Unlike CompleteElements it does not
// require an already-valid element token, so an LSP may use it while the user
// is creating a new node declaration.
func (document *Document) ElementCompletions(cursor Cursor, prefix string) (CompletionList, error) {
	if !document.languageSnapshotAvailable() {
		return CompletionList{}, ErrSyntaxUnavailable
	}
	if err := document.validateCursor(cursor); err != nil {
		return CompletionList{}, err
	}
	if len(prefix) > document.limits.MaxIdentifierBytes || strings.ContainsAny(prefix, "\x00\r\n") {
		return CompletionList{}, fmt.Errorf("element completion prefix exceeds its bound or is not canonical")
	}
	replacement := syntax.Span{Start: cursor.Position, End: cursor.Position}
	items := make([]Completion, 0, len(document.catalog))
	for _, name := range sortedEntryNames(document.catalog) {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		identity := document.catalog[name].metadata.Identity
		items = append(items, Completion{
			Kind: CompletionElement, Label: name, InsertText: name,
			Detail:      "descriptor revision " + strconv.FormatUint(identity.Revision, 10),
			Replacement: replacement, Element: name,
		})
	}
	return document.boundCompletions(items), nil
}

// NodeCompletions returns only declared nodes whose exact descriptor has the
// named port in the requested direction. It is suitable for a partial
// endpoint before the parser can form an Endpoint AST.
func (document *Document) NodeCompletions(cursor Cursor, prefix, port string, direction element.Direction) (CompletionList, error) {
	if !document.languageSnapshotAvailable() {
		return CompletionList{}, ErrSyntaxUnavailable
	}
	if err := document.validateCursor(cursor); err != nil {
		return CompletionList{}, err
	}
	if direction != element.Input && direction != element.Output {
		return CompletionList{}, fmt.Errorf("node completion direction %q is invalid", direction)
	}
	if len(prefix) > document.limits.MaxIdentifierBytes || len(port) > document.limits.MaxIdentifierBytes ||
		strings.ContainsAny(prefix+port, "\x00\r\n") {
		return CompletionList{}, fmt.Errorf("node completion query exceeds its bound or is not canonical")
	}
	names := make([]string, 0, len(document.nodes))
	for name := range document.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	replacement := syntax.Span{Start: cursor.Position, End: cursor.Position}
	items := make([]Completion, 0, len(names))
	for _, name := range names {
		record := document.nodes[name]
		if record.ambiguous || !strings.HasPrefix(name, prefix) {
			continue
		}
		entry, found := document.catalog[record.element]
		if !found {
			continue
		}
		descriptorPort, found := entry.descriptor.Port(port)
		if !found || descriptorPort.Direction != direction {
			continue
		}
		items = append(items, Completion{
			Kind: CompletionNode, Label: name, InsertText: name,
			Detail:      record.element + " · " + string(direction) + " port " + port,
			Replacement: replacement, Element: record.element, Node: name,
			Port: port, Type: descriptorPort.Type.String(),
		})
	}
	return document.boundCompletions(items), nil
}

// PortCompletions returns only ports declared by the exact descriptor for a
// declared node and requested direction. Unknown/ambiguous nodes yield an
// empty list; no speculative port is manufactured.
func (document *Document) PortCompletions(cursor Cursor, node, prefix string, direction element.Direction) (CompletionList, error) {
	if !document.languageSnapshotAvailable() {
		return CompletionList{}, ErrSyntaxUnavailable
	}
	if err := document.validateCursor(cursor); err != nil {
		return CompletionList{}, err
	}
	if direction != element.Input && direction != element.Output {
		return CompletionList{}, fmt.Errorf("port completion direction %q is invalid", direction)
	}
	if len(node) > document.limits.MaxIdentifierBytes || len(prefix) > document.limits.MaxIdentifierBytes ||
		strings.ContainsAny(node+prefix, "\x00\r\n") {
		return CompletionList{}, fmt.Errorf("port completion query exceeds its bound or is not canonical")
	}
	record, found := document.nodes[node]
	if !found || record.ambiguous {
		return CompletionList{}, nil
	}
	entry, found := document.catalog[record.element]
	if !found {
		return CompletionList{}, nil
	}
	replacement := syntax.Span{Start: cursor.Position, End: cursor.Position}
	items := make([]Completion, 0, len(entry.descriptor.Ports))
	for _, port := range entry.descriptor.Ports {
		if port.Direction != direction || !strings.HasPrefix(port.Name, prefix) {
			continue
		}
		items = append(items, Completion{
			Kind: CompletionPort, Label: port.Name, InsertText: port.Name,
			Detail:      string(port.Direction) + " · " + port.Type.String(),
			Replacement: replacement, Element: record.element, Node: node,
			Port: port.Name, Type: port.Type.String(),
		})
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Label < items[right].Label })
	return document.boundCompletions(items), nil
}

func (document *Document) CompleteNodes(cursor Cursor) (CompletionList, error) {
	value, err := document.symbolAt(cursor)
	if err != nil {
		return CompletionList{}, err
	}
	if value.kind != SymbolNodeReference {
		return CompletionList{}, ErrNoCompletionContext
	}
	return document.completeNodes(cursor, value), nil
}

func (document *Document) CompletePorts(cursor Cursor) (CompletionList, error) {
	value, err := document.symbolAt(cursor)
	if err != nil {
		return CompletionList{}, err
	}
	if value.kind != SymbolPortReference {
		return CompletionList{}, ErrNoCompletionContext
	}
	return document.completePorts(cursor, value), nil
}

func (document *Document) completeElements(cursor Cursor, value symbol) CompletionList {
	prefix := document.completionPrefix(value.span, cursor.Position.Offset)
	items := make([]Completion, 0, len(document.catalog))
	for _, name := range sortedEntryNames(document.catalog) {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		identity := document.catalog[name].metadata.Identity
		items = append(items, Completion{
			Kind: CompletionElement, Label: name, InsertText: name,
			Detail:      "descriptor revision " + strconv.FormatUint(identity.Revision, 10),
			Replacement: value.span, Element: name,
		})
	}
	return document.boundCompletions(items)
}

func (document *Document) completeNodes(cursor Cursor, value symbol) CompletionList {
	prefix := document.completionPrefix(value.span, cursor.Position.Offset)
	names := make([]string, 0, len(document.nodes))
	for name := range document.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]Completion, 0, len(names))
	for _, name := range names {
		record := document.nodes[name]
		if record.ambiguous || !strings.HasPrefix(name, prefix) {
			continue
		}
		entry, found := document.catalog[record.element]
		if !found {
			continue
		}
		port, found := entry.descriptor.Port(value.port)
		if !found || port.Direction != value.expected {
			continue
		}
		items = append(items, Completion{
			Kind: CompletionNode, Label: name, InsertText: name,
			Detail:      record.element + " · " + string(value.expected) + " port " + value.port,
			Replacement: value.span, Element: record.element, Node: name,
			Port: value.port, Type: port.Type.String(),
		})
	}
	return document.boundCompletions(items)
}

func (document *Document) completePorts(cursor Cursor, value symbol) CompletionList {
	prefix := document.completionPrefix(value.span, cursor.Position.Offset)
	record, found := document.nodes[value.node]
	if !found || record.ambiguous {
		return CompletionList{}
	}
	entry, found := document.catalog[record.element]
	if !found {
		return CompletionList{}
	}
	items := make([]Completion, 0, len(entry.descriptor.Ports))
	for _, port := range entry.descriptor.Ports {
		if port.Direction != value.expected || !strings.HasPrefix(port.Name, prefix) {
			continue
		}
		items = append(items, Completion{
			Kind: CompletionPort, Label: port.Name, InsertText: port.Name,
			Detail:      string(port.Direction) + " · " + port.Type.String(),
			Replacement: value.span, Element: record.element, Node: value.node,
			Port: port.Name, Type: port.Type.String(),
		})
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Label < items[right].Label })
	return document.boundCompletions(items)
}

func (document *Document) completionPrefix(span syntax.Span, offset int) string {
	if offset < span.Start.Offset {
		return ""
	}
	if offset > span.End.Offset {
		offset = span.End.Offset
	}
	return string(document.source[span.Start.Offset:offset])
}

func (document *Document) boundCompletions(items []Completion) CompletionList {
	total := len(items)
	if len(items) > document.limits.MaxResultItems {
		items = items[:document.limits.MaxResultItems]
	}
	return CompletionList{
		Items: slices.Clone(items), Total: total,
		Incomplete: len(items) < total,
	}
}

func (document *Document) Hover(cursor Cursor) (Hover, error) {
	value, err := document.symbolAt(cursor)
	if err != nil {
		return Hover{}, err
	}
	hover := Hover{
		Range: value.span, Kind: value.kind, Node: value.node,
		ElementReference: value.element,
	}
	entry, found := document.catalog[value.element]
	if found {
		metadata := cloneElementMetadata(entry.metadata)
		hover.Descriptor = &metadata
	}
	if value.node != "" {
		contract := NodeConfigContract{
			ConfigContract: ConfigContract{
				Artifact: valuesArtifact, Resolved: found, InlineTopologyValues: false,
				SchemaStatus: ConfigSchemaUnresolved, Properties: []ValuesPropertyMetadata{},
			},
			ValuesPath: "nodes." + value.node,
		}
		if found {
			contract.ConfigContract = cloneConfigContract(entry.metadata.Config)
		}
		hover.Config = &contract
	}
	if value.kind == SymbolPortReference && found {
		port, portFound := metadataPort(entry.metadata, value.port)
		if portFound {
			copy := clonePortMetadata(port)
			hover.Port = &copy
		}
	}
	return cloneHover(hover), nil
}

func (document *Document) languageSnapshotAvailable() bool {
	return document != nil && (document.parsed && document.canonical || document.recovered)
}

func metadataPort(metadata ElementMetadata, name string) (PortMetadata, bool) {
	for _, port := range metadata.Ports {
		if port.Name == name {
			return port, true
		}
	}
	return PortMetadata{}, false
}

func cloneHover(value Hover) Hover {
	if value.Descriptor != nil {
		copy := cloneElementMetadata(*value.Descriptor)
		value.Descriptor = &copy
	}
	if value.Port != nil {
		copy := clonePortMetadata(*value.Port)
		value.Port = &copy
	}
	if value.Config != nil {
		copy := *value.Config
		copy.ConfigContract = cloneConfigContract(value.Config.ConfigContract)
		value.Config = &copy
	}
	return value
}

func (document *Document) Definitions(cursor Cursor) (DefinitionList, error) {
	value, err := document.symbolAt(cursor)
	if err != nil {
		return DefinitionList{}, err
	}
	switch value.kind {
	case SymbolElement:
		entry, found := document.catalog[value.element]
		if !found {
			return DefinitionList{}, ErrDefinitionMissing
		}
		return DefinitionList{Items: []Definition{{
			Kind: SymbolElement, URI: descriptorURI(entry.metadata.Identity),
			Element: value.element,
		}}}, nil
	case SymbolNodeDeclaration:
		span := value.span
		return DefinitionList{Items: []Definition{{
			Kind: SymbolNodeDeclaration, URI: document.sourceURI(), Span: &span,
			Node: value.node, Element: value.element,
		}}}, nil
	case SymbolNodeReference:
		record, found := document.nodes[value.node]
		if !found || record.ambiguous {
			return DefinitionList{}, ErrDefinitionMissing
		}
		span := record.declaration
		return DefinitionList{Items: []Definition{{
			Kind: SymbolNodeDeclaration, URI: document.sourceURI(), Span: &span,
			Node: value.node, Element: record.element,
		}}}, nil
	case SymbolPortReference:
		record, found := document.nodes[value.node]
		if !found || record.ambiguous {
			return DefinitionList{}, ErrDefinitionMissing
		}
		entry, found := document.catalog[record.element]
		if !found {
			return DefinitionList{}, ErrDefinitionMissing
		}
		if _, found := entry.descriptor.Port(value.port); !found {
			return DefinitionList{}, ErrDefinitionMissing
		}
		return DefinitionList{Items: []Definition{{
			Kind: SymbolPortReference,
			URI:  descriptorURI(entry.metadata.Identity) + "#ports/" + value.port,
			Node: value.node, Element: record.element, Port: value.port,
		}}}, nil
	default:
		return DefinitionList{}, ErrDefinitionMissing
	}
}

func descriptorURI(identity element.Identity) string {
	// This is a path-only URI, not an authority: embedding "name@revision" in
	// a // authority would be parsed as URI user-info and could be mistaken for
	// credential-bearing input by a conforming adapter.
	return "openrealtime-descriptor:/" + identity.Name + "@" +
		strconv.FormatUint(identity.Revision, 10) + "/" + identity.Digest
}

func (document *Document) sourceURI() string {
	if document.path != "" {
		return document.path
	}
	return "openrealtime-memory://" + document.digest
}

func (document *Document) RenameNode(cursor Cursor, newName string) (EditSet, error) {
	value, err := document.symbolAt(cursor)
	if err != nil {
		return EditSet{}, err
	}
	if value.kind != SymbolNodeDeclaration && value.kind != SymbolNodeReference {
		return EditSet{}, ErrInvalidRename
	}
	if !nodeNamePattern.MatchString(newName) {
		return EditSet{}, fmt.Errorf("%w: %q is not an .ortg node identifier", ErrInvalidRename, newName)
	}
	if len(newName) > document.limits.MaxIdentifierBytes {
		return EditSet{}, fmt.Errorf("%w: node identifier has %d bytes; maximum is %d",
			ErrInvalidRename, len(newName), document.limits.MaxIdentifierBytes)
	}
	record, found := document.nodes[value.node]
	if !found || record.ambiguous {
		return EditSet{}, fmt.Errorf("%w: node %q is ambiguous", ErrInvalidRename, value.node)
	}
	if newName == value.node {
		return EditSet{Path: document.path, SourceDigest: document.digest}, nil
	}
	if _, collision := document.nodes[newName]; collision {
		return EditSet{}, fmt.Errorf("%w: node %q already exists", ErrRenameCollision, newName)
	}
	if len(record.references) > document.limits.MaxRenameEdits {
		return EditSet{}, fmt.Errorf("%w: node %q has %d references; maximum is %d",
			ErrEditLimit, value.node, len(record.references), document.limits.MaxRenameEdits)
	}
	edits := make([]TextEdit, 0, len(record.references))
	for _, span := range record.references {
		if string(document.source[span.Start.Offset:span.End.Offset]) != value.node {
			return EditSet{}, fmt.Errorf("%w: source no longer matches node %q", ErrStalePosition, value.node)
		}
		edits = append(edits, TextEdit{Span: span, OldText: value.node, NewText: newName})
	}
	return EditSet{Path: document.path, SourceDigest: document.digest, Edits: edits}, nil
}

// ApplyEdits applies an in-memory edit set after verifying its source digest,
// exact positions, expected text, ordering, and non-overlap. It performs no
// file I/O and never mutates source or the edit set.
func ApplyEdits(source []byte, editSet EditSet) ([]byte, error) {
	const maximumEditedSourceBytes = 16 << 20
	if len(source) > maximumEditedSourceBytes {
		return nil, fmt.Errorf("edited source input exceeds %d bytes", maximumEditedSourceBytes)
	}
	if sourceDigest(source) != editSet.SourceDigest {
		return nil, ErrStalePosition
	}
	edits := slices.Clone(editSet.Edits)
	sort.Slice(edits, func(left, right int) bool {
		return edits[left].Span.Start.Offset < edits[right].Span.Start.Offset
	})
	last := 0
	resultBytes := len(source)
	positions := newSourcePositions(source)
	var output strings.Builder
	output.Grow(len(source))
	for index, edit := range edits {
		start, end := edit.Span.Start.Offset, edit.Span.End.Offset
		if start < last || start < 0 || end < start || end > len(source) {
			return nil, fmt.Errorf("%w: edit %d overlaps or has an invalid span", ErrInvalidPosition, index)
		}
		left, err := positions.at(start)
		if err != nil || left != edit.Span.Start {
			return nil, fmt.Errorf("%w: edit %d has a stale start position", ErrInvalidPosition, index)
		}
		right, err := positions.at(end)
		if err != nil || right != edit.Span.End {
			return nil, fmt.Errorf("%w: edit %d has a stale end position", ErrInvalidPosition, index)
		}
		if string(source[start:end]) != edit.OldText {
			return nil, fmt.Errorf("%w: edit %d expected %q", ErrStalePosition, index, edit.OldText)
		}
		resultBytes += len(edit.NewText) - (end - start)
		if resultBytes < 0 || resultBytes > maximumEditedSourceBytes {
			return nil, fmt.Errorf("edited source result exceeds %d bytes", maximumEditedSourceBytes)
		}
		output.Write(source[last:start])
		output.WriteString(edit.NewText)
		last = end
	}
	output.Write(source[last:])
	return []byte(output.String()), nil
}
