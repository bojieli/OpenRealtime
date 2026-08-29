// Package editor provides a pure, graph-aware language-service core for
// canonical .ortg topology source. It never reads or writes files, mounts a
// graph, or mixes node values into topology.
package editor

import (
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

var (
	ErrStalePosition       = errors.New("editor position belongs to another source revision")
	ErrInvalidPosition     = errors.New("editor position is invalid")
	ErrSyntaxUnavailable   = errors.New("editor operation requires canonical parsed topology")
	ErrNoCompletionContext = errors.New("position has no completion context")
	ErrNoSymbol            = errors.New("position does not name a graph symbol")
	ErrDefinitionMissing   = errors.New("symbol has no resolvable definition")
	ErrInvalidRename       = errors.New("invalid graph node rename")
	ErrRenameCollision     = errors.New("graph node rename collides with an existing node")
	ErrEditLimit           = errors.New("graph node rename exceeds the edit bound")
)

// Limits bounds every input collection retained by a Document and every
// collection returned by its query methods. Zero fields select defaults.
type Limits struct {
	MaxPathBytes       int `json:"max_path_bytes"`
	MaxSourceBytes     int `json:"max_source_bytes"`
	MaxCatalogElements int `json:"max_catalog_elements"`
	MaxCatalogPorts    int `json:"max_catalog_ports"`
	MaxCatalogBytes    int `json:"max_catalog_bytes"`
	MaxGraphStatements int `json:"max_graph_statements"`
	MaxResultItems     int `json:"max_result_items"`
	MaxRenameEdits     int `json:"max_rename_edits"`
	MaxIdentifierBytes int `json:"max_identifier_bytes"`
}

var defaultLimits = Limits{
	MaxPathBytes: 4 << 10, MaxSourceBytes: 1 << 20,
	MaxCatalogElements: 4096, MaxCatalogPorts: 65536,
	MaxCatalogBytes:    8 << 20,
	MaxGraphStatements: 16384, MaxResultItems: 2048,
	MaxRenameEdits: 8192, MaxIdentifierBytes: 256,
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
	Artifact             string `json:"artifact"`
	Resolved             bool   `json:"resolved"`
	SchemaReference      string `json:"schema_reference,omitempty"`
	InlineTopologyValues bool   `json:"inline_topology_values"`
	EmptyObjectOnly      bool   `json:"empty_object_only"`
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
	Identity             element.Identity     `json:"identity"`
	TopologyDeclaration  string               `json:"topology_declaration"`
	Generics             []string             `json:"generics,omitempty"`
	Ports                []PortMetadata       `json:"ports"`
	Reaction             ReactionMetadata     `json:"reaction"`
	StateSchema          string               `json:"state_schema,omitempty"`
	Config               ConfigContract       `json:"config"`
	Dependencies         []element.Dependency `json:"dependencies,omitempty"`
	Effects              []element.Effect     `json:"effects,omitempty"`
	CompositeFingerprint string               `json:"composite_fingerprint,omitempty"`
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
