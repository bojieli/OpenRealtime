package editor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

const valuesArtifact = "openrealtime.ai/config/v1alpha1"

type catalogEntry struct {
	descriptor element.Descriptor
	metadata   ElementMetadata
}

// Document is an immutable analysis snapshot. Its methods allocate fresh
// result slices, so concurrent readers cannot mutate one another or the
// catalog/source snapshot used for analysis.
type Document struct {
	path        string
	source      []byte
	digest      string
	limits      Limits
	file        syntax.File
	parsed      bool
	recovered   bool
	canonical   bool
	diagnostics []Diagnostic
	catalog     map[string]catalogEntry
	metadata    []ElementMetadata
	symbols     []symbol
	nodes       map[string]nodeRecord
	positions   sourcePositions
}

// Options controls one immutable language-service analysis snapshot.
type Options struct {
	Limits         Limits
	SchemaResolver schema.Resolver
	SchemaLimits   schema.Limits
}

// Analyze captures a stable latest-revision catalog snapshot and analyzes one
// in-memory .ortg document. Syntax/semantic failures are diagnostics, while
// invalid API inputs and configured bound violations are returned as errors.
func Analyze(path string, source []byte, catalog *resolve.Catalog, limits Limits) (*Document, error) {
	return AnalyzeWithOptions(context.Background(), path, source, catalog, Options{Limits: limits})
}

// AnalyzeWithOptions is the context-aware form used by management services.
// Schema resolution is optional and its results are captured into the same
// immutable descriptor snapshot as syntax and diagnostics.
func AnalyzeWithOptions(
	ctx context.Context, path string, source []byte, catalog *resolve.Catalog, options Options,
) (*Document, error) {
	if ctx == nil {
		return nil, errors.New("editor analysis requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("editor analysis canceled: %w", err)
	}
	resolved, err := normalizeLimits(options.Limits)
	if err != nil {
		return nil, err
	}
	if len(path) > resolved.MaxPathBytes || strings.ContainsAny(path, "\x00\r\n") {
		return nil, fmt.Errorf("editor path exceeds its bound or contains control characters")
	}
	if len(source) > resolved.MaxSourceBytes {
		return nil, fmt.Errorf("editor source has %d bytes; maximum is %d", len(source), resolved.MaxSourceBytes)
	}
	entries, metadata, err := snapshotCatalog(catalog, resolved)
	if err != nil {
		return nil, err
	}
	owned := slices.Clone(source)
	document := &Document{
		path: path, source: owned, digest: sourceDigest(owned), limits: resolved,
		catalog: entries, metadata: metadata, positions: newSourcePositions(owned),
	}
	if options.SchemaResolver != nil {
		diagnostics, err := resolveSchemaMetadata(ctx, path, document.catalog, document.metadata,
			options.SchemaResolver, options.SchemaLimits, resolved)
		if err != nil {
			return nil, err
		}
		document.diagnostics = append(document.diagnostics, diagnostics...)
	}
	file, parseErr := syntax.Parse(path, owned)
	if parseErr != nil {
		recovery, recoveryErr := syntax.Recover(path, owned, syntax.RecoveryLimits{
			MaxSourceBytes: resolved.MaxSourceBytes,
			MaxTokens:      min(max(131_072, resolved.MaxGraphStatements*16), 1<<20),
			MaxStatements:  resolved.MaxGraphStatements, MaxDiagnostics: min(resolved.MaxResultItems, 65_536),
		})
		if recoveryErr != nil {
			return nil, fmt.Errorf("editor recover topology: %w", recoveryErr)
		}
		document.file = recovery.File
		document.recovered = true
		document.nodes = buildRecoveredNodeIndex(recovery.File)
		for _, failure := range recovery.Diagnostics {
			document.diagnostics = append(document.diagnostics, Diagnostic{
				Code: "E_SYNTAX", Severity: SeverityError, Path: failure.Path,
				Span: failure.Span, Message: failure.Message,
			})
		}
		if len(recovery.Diagnostics) == 0 {
			document.diagnostics = append(document.diagnostics, syntaxDiagnostic(path, parseErr))
		}
		sortDiagnostics(document.diagnostics)
		return document, nil
	}
	document.file = file
	document.parsed = true
	if len(file.Graph.Statements) > resolved.MaxGraphStatements {
		return nil, fmt.Errorf("editor graph has %d statements; maximum is %d",
			len(file.Graph.Statements), resolved.MaxGraphStatements)
	}
	formatted := syntax.Format(file)
	document.canonical = bytes.Equal(owned, []byte(formatted))
	if !document.canonical {
		document.diagnostics = append(document.diagnostics, Diagnostic{
			Code: "E_NON_CANONICAL_SOURCE", Severity: SeverityError, Path: path,
			Span:    file.Graph.Span,
			Message: "topology source is not canonical .ortg; format it before position-sensitive editor operations",
		})
	}
	document.diagnostics = append(document.diagnostics, compileDiagnostics(file, entries)...)
	if document.canonical {
		symbols, nodes, indexErr := buildSymbolIndex(owned, file, entries)
		if indexErr != nil {
			document.canonical = false
			document.diagnostics = append(document.diagnostics, Diagnostic{
				Code: "E_EDITOR_INDEX", Severity: SeverityError, Path: path,
				Span: file.Graph.Span, Message: indexErr.Error(),
			})
		} else {
			document.symbols, document.nodes = symbols, nodes
		}
	}
	sortDiagnostics(document.diagnostics)
	return document, nil
}

func (document *Document) Path() string         { return document.path }
func (document *Document) SourceDigest() string { return document.digest }
func (document *Document) Parsed() bool         { return document.parsed }
func (document *Document) Recovered() bool      { return document.recovered }
func (document *Document) Canonical() bool      { return document.canonical }

func (document *Document) Source() []byte { return slices.Clone(document.source) }

func (document *Document) Diagnostics() DiagnosticReport {
	items, incomplete := boundedClone(document.diagnostics, document.limits.MaxResultItems, cloneDiagnostic)
	return DiagnosticReport{Items: items, Total: len(document.diagnostics), Incomplete: incomplete}
}

// CatalogMetadata returns descriptor-derived metadata in element-name order.
// Configuration remains an opaque schema reference in the separate values
// artifact; this API never manufactures inline topology fields.
func (document *Document) CatalogMetadata() MetadataReport {
	items, incomplete := boundedClone(document.metadata, document.limits.MaxResultItems, cloneElementMetadata)
	return MetadataReport{Elements: items, Total: len(document.metadata), Incomplete: incomplete}
}

func (document *Document) Cursor(offset int) (Cursor, error) {
	position, err := document.positions.at(offset)
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{SourceDigest: document.digest, Position: position}, nil
}

// ValidateCursor verifies both the source revision and the exact
// offset/line/column tuple without performing a symbol query.
func (document *Document) ValidateCursor(cursor Cursor) error {
	return document.validateCursor(cursor)
}

func (document *Document) validateCursor(cursor Cursor) error {
	if cursor.SourceDigest != document.digest {
		return ErrStalePosition
	}
	want, err := document.positions.at(cursor.Position.Offset)
	if err != nil {
		return err
	}
	if want != cursor.Position {
		return fmt.Errorf("%w: position is %+v, source position is %+v", ErrInvalidPosition, cursor.Position, want)
	}
	return nil
}

func normalizeLimits(value Limits) (Limits, error) {
	defaults := DefaultLimits()
	fields := []struct {
		name string
		set  *int
		def  int
		hard int
	}{
		{"max_path_bytes", &value.MaxPathBytes, defaults.MaxPathBytes, 64 << 10},
		{"max_source_bytes", &value.MaxSourceBytes, defaults.MaxSourceBytes, 16 << 20},
		{"max_catalog_elements", &value.MaxCatalogElements, defaults.MaxCatalogElements, 65536},
		{"max_catalog_ports", &value.MaxCatalogPorts, defaults.MaxCatalogPorts, 1 << 20},
		{"max_catalog_bytes", &value.MaxCatalogBytes, defaults.MaxCatalogBytes, 64 << 20},
		{"max_graph_statements", &value.MaxGraphStatements, defaults.MaxGraphStatements, 1 << 20},
		{"max_result_items", &value.MaxResultItems, defaults.MaxResultItems, 65536},
		{"max_rename_edits", &value.MaxRenameEdits, defaults.MaxRenameEdits, 1 << 20},
		{"max_identifier_bytes", &value.MaxIdentifierBytes, defaults.MaxIdentifierBytes, 4096},
		{"max_resolved_schema_bytes", &value.MaxResolvedSchemaBytes, defaults.MaxResolvedSchemaBytes, 16 << 20},
		{"max_total_resolved_schema_bytes", &value.MaxTotalResolvedSchemaBytes, defaults.MaxTotalResolvedSchemaBytes, 64 << 20},
		{"max_schema_properties", &value.MaxSchemaProperties, defaults.MaxSchemaProperties, 65_536},
	}
	for _, field := range fields {
		if *field.set == 0 {
			*field.set = field.def
		}
		if *field.set < 1 || *field.set > field.hard {
			return Limits{}, fmt.Errorf("editor %s must be between 1 and %d", field.name, field.hard)
		}
	}
	return value, nil
}

func buildRecoveredNodeIndex(file syntax.File) map[string]nodeRecord {
	nodes := make(map[string]nodeRecord)
	for _, node := range file.Graph.Nodes() {
		record, duplicate := nodes[node.Name]
		if duplicate {
			record.ambiguous = true
			nodes[node.Name] = record
			continue
		}
		nodes[node.Name] = nodeRecord{name: node.Name, element: node.Element}
	}
	return nodes
}

func snapshotCatalog(catalog *resolve.Catalog, limits Limits) (map[string]catalogEntry, []ElementMetadata, error) {
	if catalog == nil {
		return nil, nil, errors.New("editor analysis requires a descriptor catalog")
	}
	var previous []catalogSnapshot
	for attempt := 0; attempt < 4; attempt++ {
		current, err := captureCatalog(catalog, limits)
		if err != nil {
			return nil, nil, err
		}
		if previous != nil && equalCatalogSnapshots(previous, current) {
			entries := make(map[string]catalogEntry, len(current))
			metadata := make([]ElementMetadata, 0, len(current))
			for _, captured := range current {
				value := metadataFromDescriptor(captured.descriptor, captured.identity)
				entries[captured.identity.Name] = catalogEntry{
					descriptor: captured.descriptor.Clone(), metadata: cloneElementMetadata(value),
				}
				metadata = append(metadata, value)
			}
			return entries, metadata, nil
		}
		previous = current
	}
	return nil, nil, errors.New("descriptor catalog changed continuously while the editor captured a snapshot")
}

type catalogSnapshot struct {
	identity   element.Identity
	descriptor element.Descriptor
}

func captureCatalog(catalog *resolve.Catalog, limits Limits) ([]catalogSnapshot, error) {
	names := catalog.Names()
	if len(names) > limits.MaxCatalogElements {
		return nil, fmt.Errorf("editor catalog has %d elements; maximum is %d", len(names), limits.MaxCatalogElements)
	}
	result := make([]catalogSnapshot, 0, len(names))
	ports := 0
	bytesRetained := 0
	for _, name := range names {
		descriptor, found := catalog.Latest(name)
		if !found {
			continue
		}
		identity, err := descriptor.Identity()
		if err != nil {
			return nil, fmt.Errorf("editor catalog element %s: %w", name, err)
		}
		ports += len(descriptor.Ports)
		if ports > limits.MaxCatalogPorts {
			return nil, fmt.Errorf("editor catalog has more than %d ports", limits.MaxCatalogPorts)
		}
		canonical, err := descriptor.Canonical()
		if err != nil {
			return nil, fmt.Errorf("editor catalog element %s: %w", name, err)
		}
		encoded, err := json.Marshal(canonical)
		if err != nil {
			return nil, fmt.Errorf("editor catalog element %s: encode descriptor: %w", name, err)
		}
		bytesRetained += len(encoded)
		if bytesRetained > limits.MaxCatalogBytes {
			return nil, fmt.Errorf("editor catalog descriptors exceed %d bytes", limits.MaxCatalogBytes)
		}
		result = append(result, catalogSnapshot{identity: identity, descriptor: canonical})
	}
	return result, nil
}

func equalCatalogSnapshots(left, right []catalogSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].identity != right[index].identity {
			return false
		}
	}
	return true
}

func compileDiagnostics(file syntax.File, entries map[string]catalogEntry) []Diagnostic {
	catalog := resolve.NewCatalog()
	for _, name := range sortedEntryNames(entries) {
		if err := catalog.Register(entries[name].descriptor); err != nil {
			return []Diagnostic{{
				Code: "E_EDITOR_CATALOG", Severity: SeverityError, Path: file.Path,
				Span: file.Graph.Span, Message: err.Error(),
			}}
		}
	}
	_, err := graphcompiler.Compile(file, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err == nil {
		return nil
	}
	var failures *graphcompiler.Errors
	if errors.As(err, &failures) {
		result := make([]Diagnostic, 0, len(failures.Diagnostics))
		for _, diagnostic := range failures.Diagnostics {
			result = append(result, Diagnostic{
				Code: diagnostic.Code, Severity: SeverityError, Path: diagnostic.Path,
				Span: diagnostic.Span, Message: diagnostic.Message,
				Notes: slices.Clone(diagnostic.Notes),
			})
		}
		return result
	}
	return []Diagnostic{{
		Code: "E_EDITOR_ANALYSIS", Severity: SeverityError, Path: file.Path,
		Span: file.Graph.Span, Message: err.Error(),
	}}
}

func syntaxDiagnostic(path string, err error) Diagnostic {
	var failure *syntax.Error
	if errors.As(err, &failure) {
		return Diagnostic{
			Code: "E_SYNTAX", Severity: SeverityError, Path: failure.Path,
			Span: failure.Span, Message: failure.Message,
		}
	}
	return Diagnostic{
		Code: "E_SYNTAX", Severity: SeverityError, Path: path,
		Message: err.Error(), Span: syntax.Span{
			Start: syntax.Position{Line: 1, Column: 1},
			End:   syntax.Position{Line: 1, Column: 1},
		},
	}
}

func metadataFromDescriptor(descriptor element.Descriptor, identity element.Identity) ElementMetadata {
	roles := make(map[string]string)
	for _, name := range descriptor.Reaction.Triggers {
		roles[name] = "trigger"
	}
	for _, name := range descriptor.Reaction.SampledState {
		roles[name] = "sampled_state"
	}
	for _, name := range descriptor.Reaction.Interrupts {
		roles[name] = "interrupt"
	}
	for _, name := range descriptor.Reaction.Outcomes {
		roles[name] = "outcome"
	}
	ports := make([]PortMetadata, 0, len(descriptor.Ports))
	for _, port := range descriptor.Ports {
		ports = append(ports, PortMetadata{
			Name: port.Name, Direction: port.Direction, Type: port.Type.Clone(),
			Cardinality: port.Cardinality, Required: port.Required,
			MinConnections: port.MinConnections, LossAllowed: port.LossAllowed,
			DefaultDepth: port.DefaultDepth, ReactionRole: roles[port.Name],
		})
	}
	return ElementMetadata{
		Identity: identity, TopologyDeclaration: descriptor.Name + " :: <node>;",
		Generics: slices.Clone(descriptor.Generics), Ports: ports,
		Reaction: ReactionMetadata{
			Triggers:       slices.Clone(descriptor.Reaction.Triggers),
			SampledState:   slices.Clone(descriptor.Reaction.SampledState),
			Interrupts:     slices.Clone(descriptor.Reaction.Interrupts),
			Outcomes:       slices.Clone(descriptor.Reaction.Outcomes),
			MaxConcurrency: descriptor.Reaction.MaxConcurrency,
			BreaksCycles:   descriptor.Reaction.BreaksCycles,
		},
		StateSchema: descriptor.StateSchema,
		Config: ConfigContract{
			Artifact: valuesArtifact, Resolved: true, SchemaReference: descriptor.ConfigSchema,
			InlineTopologyValues: false, EmptyObjectOnly: descriptor.ConfigSchema == "",
			SchemaStatus: func() ConfigSchemaStatus {
				if descriptor.ConfigSchema == "" {
					return ConfigSchemaEmpty
				}
				return ConfigSchemaUnresolved
			}(),
			PropertiesComplete: descriptor.ConfigSchema == "",
			Properties:         []ValuesPropertyMetadata{},
		},
		Dependencies:         slices.Clone(descriptor.Dependencies),
		Effects:              slices.Clone(descriptor.Effects),
		CompositeFingerprint: descriptor.CompositeFingerprint,
	}
}

func sortedEntryNames(entries map[string]catalogEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sourceDigest(source []byte) string {
	digest := sha256.Sum256(source)
	return "sha256:" + hex.EncodeToString(digest[:])
}

type sourcePositions struct {
	source     []byte
	lineStarts []int
}

func newSourcePositions(source []byte) sourcePositions {
	starts := []int{0}
	for index, value := range source {
		if value == '\n' {
			starts = append(starts, index+1)
		}
	}
	return sourcePositions{source: source, lineStarts: starts}
}

func (positions sourcePositions) at(offset int) (syntax.Position, error) {
	if offset < 0 || offset > len(positions.source) {
		return syntax.Position{}, fmt.Errorf("%w: offset %d is outside [0,%d]", ErrInvalidPosition, offset, len(positions.source))
	}
	if offset < len(positions.source) && offset > 0 && positions.source[offset]&0xc0 == 0x80 {
		return syntax.Position{}, fmt.Errorf("%w: offset %d splits a UTF-8 sequence", ErrInvalidPosition, offset)
	}
	lineIndex := sort.Search(len(positions.lineStarts), func(index int) bool {
		return positions.lineStarts[index] > offset
	}) - 1
	if lineIndex < 0 {
		lineIndex = 0
	}
	column := 1
	for cursor := positions.lineStarts[lineIndex]; cursor < offset; {
		character, size := utf8.DecodeRune(positions.source[cursor:])
		if character == utf8.RuneError && size == 1 {
			return syntax.Position{}, fmt.Errorf("%w: source contains invalid UTF-8", ErrInvalidPosition)
		}
		cursor += size
		column++
	}
	return syntax.Position{Offset: offset, Line: lineIndex + 1, Column: column}, nil
}

func sortDiagnostics(values []Diagnostic) {
	sort.SliceStable(values, func(left, right int) bool {
		a, b := values[left], values[right]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Span.Start.Offset != b.Span.Start.Offset {
			return a.Span.Start.Offset < b.Span.Start.Offset
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
}

func cloneDiagnostic(value Diagnostic) Diagnostic {
	value.Notes = slices.Clone(value.Notes)
	return value
}

func clonePortMetadata(value PortMetadata) PortMetadata {
	value.Type = value.Type.Clone()
	return value
}

func cloneElementMetadata(value ElementMetadata) ElementMetadata {
	value.Generics = slices.Clone(value.Generics)
	ports := value.Ports
	value.Ports = make([]PortMetadata, len(ports))
	for index := range ports {
		value.Ports[index] = clonePortMetadata(ports[index])
	}
	value.Reaction.Triggers = slices.Clone(value.Reaction.Triggers)
	value.Reaction.SampledState = slices.Clone(value.Reaction.SampledState)
	value.Reaction.Interrupts = slices.Clone(value.Reaction.Interrupts)
	value.Reaction.Outcomes = slices.Clone(value.Reaction.Outcomes)
	value.Dependencies = slices.Clone(value.Dependencies)
	value.Effects = slices.Clone(value.Effects)
	value.Config = cloneConfigContract(value.Config)
	return value
}

func cloneConfigContract(value ConfigContract) ConfigContract {
	value.AdditionalProperties = slices.Clone(value.AdditionalProperties)
	properties := value.Properties
	value.Properties = make([]ValuesPropertyMetadata, len(properties))
	for index, property := range properties {
		property.Types = slices.Clone(property.Types)
		property.Default = slices.Clone(property.Default)
		property.Schema = slices.Clone(property.Schema)
		values := property.Enum
		property.Enum = make([]json.RawMessage, len(values))
		for enumIndex := range values {
			property.Enum[enumIndex] = slices.Clone(values[enumIndex])
		}
		value.Properties[index] = property
	}
	return value
}

func boundedClone[T any](source []T, limit int, clone func(T) T) ([]T, bool) {
	count := len(source)
	if count > limit {
		count = limit
	}
	result := make([]T, count)
	for index := range count {
		result[index] = clone(source[index])
	}
	return result, count < len(source)
}
