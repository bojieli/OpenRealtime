// Package editor provides a pure, graph-aware language-service core for
// canonical .ortg topology source. It never reads or writes files, mounts a
// graph, or mixes node values into topology.
package editor

import (
	"encoding/json"
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

var (
	ErrStalePosition         = errors.New("editor position belongs to another source revision")
	ErrInvalidPosition       = errors.New("editor position is invalid")
	ErrSyntaxUnavailable     = errors.New("editor operation requires canonical parsed topology")
	ErrNoCompletionContext   = errors.New("position has no completion context")
	ErrNoSymbol              = errors.New("position does not name a graph symbol")
	ErrDefinitionMissing     = errors.New("symbol has no resolvable definition")
	ErrInvalidRename         = errors.New("invalid graph node rename")
	ErrRenameCollision       = errors.New("graph node rename collides with an existing node")
	ErrEditLimit             = errors.New("graph node rename exceeds the edit bound")
	ErrInvalidEdgeMutation   = errors.New("invalid graph edge mutation")
	ErrFormattingUnavailable = errors.New("editor formatting requires a strictly parsed topology")
	ErrPresentationLimit     = errors.New("editor presentation exceeds its byte bound")
)

// Limits bounds every input collection retained by a Document and every
// collection returned by its query methods. Zero fields select defaults.
type Limits struct {
	MaxPathBytes                int `json:"max_path_bytes"`
	MaxSourceBytes              int `json:"max_source_bytes"`
	MaxCatalogElements          int `json:"max_catalog_elements"`
	MaxCatalogPorts             int `json:"max_catalog_ports"`
	MaxCatalogBytes             int `json:"max_catalog_bytes"`
	MaxGraphStatements          int `json:"max_graph_statements"`
	MaxResultItems              int `json:"max_result_items"`
	MaxRenameEdits              int `json:"max_rename_edits"`
	MaxIdentifierBytes          int `json:"max_identifier_bytes"`
	MaxResolvedSchemaBytes      int `json:"max_resolved_schema_bytes"`
	MaxTotalResolvedSchemaBytes int `json:"max_total_resolved_schema_bytes"`
	MaxSchemaProperties         int `json:"max_schema_properties"`
}

var defaultLimits = Limits{
	MaxPathBytes: 4 << 10, MaxSourceBytes: 1 << 20,
	MaxCatalogElements: 4096, MaxCatalogPorts: 65536,
	MaxCatalogBytes:    8 << 20,
	MaxGraphStatements: 16384, MaxResultItems: 2048,
	MaxRenameEdits: 8192, MaxIdentifierBytes: 256,
	MaxResolvedSchemaBytes: 2 << 20, MaxTotalResolvedSchemaBytes: 16 << 20,
	MaxSchemaProperties: 4096,
}

// DefaultLimits returns bounds deliberately sized for an interactive editor
// rather than an unbounded generated graph. A function keeps shared defaults
// immutable and race-free.
func DefaultLimits() Limits { return defaultLimits }

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic is a deterministic, source-mapped language-service finding.
type Diagnostic struct {
	Code     string      `json:"code"`
	Severity Severity    `json:"severity"`
	Path     string      `json:"path,omitempty"`
	Span     syntax.Span `json:"span"`
	Message  string      `json:"message"`
	Notes    []string    `json:"notes,omitempty"`
}

type DiagnosticReport struct {
	Items      []Diagnostic `json:"items"`
	Total      int          `json:"total"`
	Incomplete bool         `json:"incomplete,omitempty"`
}

// Cursor binds a source position to the exact immutable source digest from
// which it was computed. Query methods reject cursors from stale snapshots and
// positions whose offset/line/column disagree.
type Cursor struct {
	SourceDigest string          `json:"source_digest"`
	Position     syntax.Position `json:"position"`
}

type CompletionKind string

const (
	CompletionElement CompletionKind = "element"
	CompletionNode    CompletionKind = "node"
	CompletionPort    CompletionKind = "port"
)

type Completion struct {
	Kind        CompletionKind `json:"kind"`
	Label       string         `json:"label"`
	InsertText  string         `json:"insert_text"`
	Detail      string         `json:"detail,omitempty"`
	Replacement syntax.Span    `json:"replacement"`
	Element     string         `json:"element,omitempty"`
	Node        string         `json:"node,omitempty"`
	Port        string         `json:"port,omitempty"`
	Type        string         `json:"type,omitempty"`
}

type CompletionList struct {
	Items      []Completion `json:"items"`
	Total      int          `json:"total"`
	Incomplete bool         `json:"incomplete,omitempty"`
}

type SymbolKind string

const (
	SymbolElement         SymbolKind = "element"
	SymbolNodeDeclaration SymbolKind = "node_declaration"
	SymbolNodeReference   SymbolKind = "node_reference"
	SymbolPortReference   SymbolKind = "port_reference"
)

// ConfigContract makes the topology/values boundary explicit. A descriptor
// supplies a schema reference, not inline .ortg configuration fields.
type ConfigContract struct {
	Artifact             string                   `json:"artifact"`
	Resolved             bool                     `json:"resolved"`
	SchemaReference      string                   `json:"schema_reference,omitempty"`
	InlineTopologyValues bool                     `json:"inline_topology_values"`
	EmptyObjectOnly      bool                     `json:"empty_object_only"`
	SchemaStatus         ConfigSchemaStatus       `json:"schema_status"`
	SchemaID             string                   `json:"schema_id,omitempty"`
	SchemaDigest         string                   `json:"schema_digest,omitempty"`
	PropertiesComplete   bool                     `json:"properties_complete"`
	Properties           []ValuesPropertyMetadata `json:"properties"`
	AdditionalProperties json.RawMessage          `json:"additional_properties,omitempty"`
}

type ConfigSchemaStatus string

const (
	ConfigSchemaEmpty      ConfigSchemaStatus = "empty-object-only"
	ConfigSchemaUnresolved ConfigSchemaStatus = "unresolved"
	ConfigSchemaResolved   ConfigSchemaStatus = "resolved"
	ConfigSchemaInvalid    ConfigSchemaStatus = "invalid"
)

type ValuesPropertyMetadata struct {
	Name        string            `json:"name"`
	Pointer     string            `json:"pointer"`
	Required    bool              `json:"required,omitempty"`
	Types       []string          `json:"types,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Format      string            `json:"format,omitempty"`
	Default     json.RawMessage   `json:"default,omitempty"`
	Enum        []json.RawMessage `json:"enum,omitempty"`
	Schema      json.RawMessage   `json:"schema"`
}

type NodeConfigContract struct {
	ConfigContract
	ValuesPath string `json:"values_path"`
}

type PortMetadata struct {
	Name           string              `json:"name"`
	Direction      element.Direction   `json:"direction"`
	Type           element.Type        `json:"type"`
	Cardinality    element.Cardinality `json:"cardinality"`
	Required       bool                `json:"required,omitempty"`
	MinConnections int                 `json:"min_connections,omitempty"`
	LossAllowed    bool                `json:"loss_allowed,omitempty"`
	DefaultDepth   int                 `json:"default_depth,omitempty"`
	ReactionRole   string              `json:"reaction_role,omitempty"`
}

type ReactionMetadata struct {
	Triggers       []string `json:"triggers,omitempty"`
	SampledState   []string `json:"sampled_state,omitempty"`
	Interrupts     []string `json:"interrupts,omitempty"`
	Outcomes       []string `json:"outcomes,omitempty"`
	MaxConcurrency int      `json:"max_concurrency,omitempty"`
	BreaksCycles   bool     `json:"breaks_cycles,omitempty"`
}

// ElementMetadata is generated only from a validated descriptor. It is
// sufficient for completion, hover, a virtual descriptor document, or a
// future visual canvas without claiming to know provider-specific config
// fields hidden behind Config.SchemaReference.
type ElementMetadata struct {
	Identity             element.Identity                   `json:"identity"`
	TopologyDeclaration  string                             `json:"topology_declaration"`
	Generics             []string                           `json:"generics,omitempty"`
	Ports                []PortMetadata                     `json:"ports"`
	Reaction             ReactionMetadata                   `json:"reaction"`
	StateSchema          string                             `json:"state_schema,omitempty"`
	StateTransfer        *element.StateTransferCapabilities `json:"state_transfer,omitempty"`
	Config               ConfigContract                     `json:"config"`
	Dependencies         []element.Dependency               `json:"dependencies,omitempty"`
	Effects              []element.Effect                   `json:"effects,omitempty"`
	CompositeFingerprint string                             `json:"composite_fingerprint,omitempty"`
}

type MetadataReport struct {
	Elements   []ElementMetadata `json:"elements"`
	Total      int               `json:"total"`
	Incomplete bool              `json:"incomplete,omitempty"`
}

type Hover struct {
	Range            syntax.Span         `json:"range"`
	Kind             SymbolKind          `json:"kind"`
	Node             string              `json:"node,omitempty"`
	ElementReference string              `json:"element_reference,omitempty"`
	Descriptor       *ElementMetadata    `json:"descriptor,omitempty"`
	Port             *PortMetadata       `json:"port,omitempty"`
	Config           *NodeConfigContract `json:"config,omitempty"`
}

// LSPPosition is a Language Server Protocol position: both fields are
// zero-based and Character counts UTF-16 code units, not UTF-8 bytes or
// Unicode scalar values.
type LSPPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type LSPRange struct {
	Start LSPPosition `json:"start"`
	End   LSPPosition `json:"end"`
}

// LSPMarkupContent deliberately uses plaintext. Descriptor and schema text is
// data supplied by plug-ins, so treating it as Markdown would grant it a
// presentation language and make markup-shaped values ambiguous.
type LSPMarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// LSPHover is the protocol-shaped, frontend-independent hover projection used
// by an LSP adapter. It contains the complete node values contract rather than
// a lossy summary of the descriptor metadata.
type LSPHover struct {
	Contents LSPMarkupContent `json:"contents"`
	Range    LSPRange         `json:"range"`
}

// LSPDiagnosticSeverity uses the numeric values fixed by the Language Server
// Protocol. The editor currently emits only mechanical errors and warnings.
type LSPDiagnosticSeverity int

const (
	LSPDiagnosticError   LSPDiagnosticSeverity = 1
	LSPDiagnosticWarning LSPDiagnosticSeverity = 2
)

// LSPDiagnosticData preserves immutable editor evidence in the protocol's
// opaque data field. It is never interpreted as markup or filesystem
// authority by this package.
type LSPDiagnosticData struct {
	SourceDigest string   `json:"source_digest"`
	Path         string   `json:"path,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

type LSPDiagnostic struct {
	Range    LSPRange              `json:"range"`
	Severity LSPDiagnosticSeverity `json:"severity"`
	Code     string                `json:"code"`
	Source   string                `json:"source"`
	Message  string                `json:"message"`
	Data     LSPDiagnosticData     `json:"data"`
}

// LSPFullDocumentDiagnosticReport is the standards-defined full document
// report. A bounded editor report that was truncated is rejected instead of
// being mislabeled as a complete LSP result.
type LSPFullDocumentDiagnosticReport struct {
	Kind  string          `json:"kind"`
	Items []LSPDiagnostic `json:"items"`
}

// LSPCompletionItemKind uses the numeric values fixed by the Language Server
// Protocol: graph ports are fields, nodes are variables, and descriptor-backed
// elements are classes.
type LSPCompletionItemKind int

const (
	LSPCompletionField    LSPCompletionItemKind = 5
	LSPCompletionVariable LSPCompletionItemKind = 6
	LSPCompletionClass    LSPCompletionItemKind = 7
)

type LSPTextEdit struct {
	Range   LSPRange `json:"range"`
	NewText string   `json:"newText"`
}

type LSPCompletionData struct {
	SourceDigest    string         `json:"source_digest"`
	Kind            CompletionKind `json:"kind"`
	Element         string         `json:"element,omitempty"`
	ElementRevision uint64         `json:"element_revision,omitempty"`
	ElementDigest   string         `json:"element_digest,omitempty"`
	Node            string         `json:"node,omitempty"`
	Port            string         `json:"port,omitempty"`
	Type            string         `json:"type,omitempty"`
}

type LSPCompletionItem struct {
	Label            string                `json:"label"`
	Kind             LSPCompletionItemKind `json:"kind"`
	Detail           string                `json:"detail,omitempty"`
	SortText         string                `json:"sortText"`
	FilterText       string                `json:"filterText"`
	InsertTextFormat int                   `json:"insertTextFormat"`
	TextEdit         LSPTextEdit           `json:"textEdit"`
	Data             LSPCompletionData     `json:"data"`
}

type LSPCompletionList struct {
	IsIncomplete bool                `json:"isIncomplete"`
	Items        []LSPCompletionItem `json:"items"`
}

// LSPDocumentIdentity binds protocol wire values to the caller's exact
// versioned document and this immutable analysis snapshot. Path is the
// language-service identity; URI is the independently validated LSP identity.
type LSPDocumentIdentity struct {
	Path         string `json:"path"`
	URI          string `json:"uri"`
	Version      int    `json:"version"`
	SourceDigest string `json:"source_digest"`
}

type LSPLocationLink struct {
	OriginSelectionRange LSPRange `json:"originSelectionRange"`
	TargetURI            string   `json:"targetUri"`
	TargetRange          LSPRange `json:"targetRange"`
	TargetSelectionRange LSPRange `json:"targetSelectionRange"`
}

// LSPVirtualDocument is immutable adapter input for descriptor-backed
// definition targets. Its plaintext body is generated from the exact catalog
// snapshot and grants no filesystem or network authority.
type LSPVirtualDocument struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Text       string `json:"text"`
}

type LSPVersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type LSPTextDocumentEdit struct {
	TextDocument LSPVersionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []LSPTextEdit                      `json:"edits"`
}

// LSPWorkspaceEdit uses documentChanges rather than the unversioned changes
// map, so a conforming client can refuse edits after its document advances.
type LSPWorkspaceEdit struct {
	DocumentChanges []LSPTextDocumentEdit `json:"documentChanges"`
}

type Definition struct {
	Kind    SymbolKind   `json:"kind"`
	URI     string       `json:"uri"`
	Span    *syntax.Span `json:"span,omitempty"`
	Node    string       `json:"node,omitempty"`
	Element string       `json:"element,omitempty"`
	Port    string       `json:"port,omitempty"`
}

type DefinitionList struct {
	Items []Definition `json:"items"`
}

type TextEdit struct {
	Span    syntax.Span `json:"span"`
	OldText string      `json:"old_text"`
	NewText string      `json:"new_text"`
}

type EditSet struct {
	Path         string     `json:"path,omitempty"`
	SourceDigest string     `json:"source_digest"`
	Edits        []TextEdit `json:"edits"`
}
