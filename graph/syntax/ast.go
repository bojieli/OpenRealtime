// Package syntax parses and formats the compact OpenRealtime graph language.
// It deliberately contains no type checking or runtime behavior; every
// frontend lowers into the same elaborator after producing this AST.
package syntax

// Position identifies one byte location in a source file. Line and Column are
// one-based; Offset is zero-based.
type Position struct {
	Offset int `json:"offset"`
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Span is a half-open source range.
type Span struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Import makes another graph library available to symbolic resolution.
type Import struct {
	Path     string   `json:"path"`
	Alias    string   `json:"alias,omitempty"`
	Comments []string `json:"comments,omitempty"`
	Span     Span     `json:"span"`
}

// File is one parsed .ortg source file.
type File struct {
	Path    string   `json:"path,omitempty"`
	Imports []Import `json:"imports,omitempty"`
	Graph   Graph    `json:"graph"`
}

// Graph is a named element graph. Statement order is preserved for readable
// formatting; semantic identity is established later by canonical Graph IR.
type Graph struct {
	Name             string      `json:"name"`
	Comments         []string    `json:"comments,omitempty"`
	Statements       []Statement `json:"statements,omitempty"`
	TrailingComments []string    `json:"trailing_comments,omitempty"`
	Span             Span        `json:"span"`
}

// Statement is exactly one node, edge, or exported boundary declaration.
type Statement struct {
	Node     *Node     `json:"node,omitempty"`
	Edge     *Edge     `json:"edge,omitempty"`
	Boundary *Boundary `json:"boundary,omitempty"`
}

// Node instantiates one symbolic element or subgraph contract.
type Node struct {
	Element  string   `json:"element"`
	Name     string   `json:"name"`
	Comments []string `json:"comments,omitempty"`
	Span     Span     `json:"span"`
}

// Delivery is the one ordinary edge-level choice exposed by .ortg.
type Delivery string

const (
	Lossless Delivery = "lossless"
	Lossy    Delivery = "lossy"
)

// Endpoint refers to a named port group. Variadic lanes are inferred from
// edges and therefore never appear in source.
type Endpoint struct {
	Node string `json:"node"`
	Port string `json:"port"`
	Span Span   `json:"span"`
}

func (endpoint Endpoint) String() string { return endpoint.Node + "." + endpoint.Port }

// Edge connects one output port group to one input port group. Name is empty
// unless the author needs a stable identity independent of its endpoints.
type Edge struct {
	Name     string   `json:"name,omitempty"`
	From     Endpoint `json:"from"`
	To       Endpoint `json:"to"`
	Delivery Delivery `json:"delivery"`
	Comments []string `json:"comments,omitempty"`
	Span     Span     `json:"span"`
}

// BoundaryDirection is relative to the composite graph.
type BoundaryDirection string

const (
	BoundaryInput  BoundaryDirection = "input"
	BoundaryOutput BoundaryDirection = "output"
)

// Boundary exports an internal port as part of the composite graph contract.
// Input boundaries bind an internal input; output boundaries bind an internal
// output. Their types are inferred from that port.
type Boundary struct {
	Direction BoundaryDirection `json:"direction"`
	Name      string            `json:"name"`
	Endpoint  Endpoint          `json:"endpoint"`
	Comments  []string          `json:"comments,omitempty"`
	Span      Span              `json:"span"`
}

// Nodes returns declarations in source order.
func (graph Graph) Nodes() []Node {
	var result []Node
	for _, statement := range graph.Statements {
		if statement.Node != nil {
			result = append(result, *statement.Node)
		}
	}
	return result
}

// Edges returns declarations in source order.
func (graph Graph) Edges() []Edge {
	var result []Edge
	for _, statement := range graph.Statements {
		if statement.Edge != nil {
			result = append(result, *statement.Edge)
		}
	}
	return result
}

// Boundaries returns declarations in source order.
func (graph Graph) Boundaries() []Boundary {
	var result []Boundary
	for _, statement := range graph.Statements {
		if statement.Boundary != nil {
			result = append(result, *statement.Boundary)
		}
	}
	return result
}
