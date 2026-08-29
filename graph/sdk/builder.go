// Package sdk provides an optional pure Go frontend for generated graphs and
// repository tests. It emits the same syntax AST consumed by the normative
// language-neutral compiler and never mounts resources itself.
package sdk

import (
	"fmt"
	"sort"
	"strings"

	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

type Builder struct {
	name       string
	nodes      map[string]Node
	statements []syntax.Statement
	err        error
}

type Node struct {
	name    string
	element string
}

func New(name string) *Builder {
	return &Builder{name: name, nodes: make(map[string]Node)}
}

// Add instantiates one symbolic descriptor and returns its typed-port anchor.
func (builder *Builder) Add(name, elementReference string) Node {
	if builder.err != nil {
		return Node{}
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(elementReference) == "" {
		builder.err = fmt.Errorf("graph node requires a name and element reference")
		return Node{}
	}
	if _, duplicate := builder.nodes[name]; duplicate {
		builder.err = fmt.Errorf("graph repeats node %q", name)
		return Node{}
	}
	node := Node{name: name, element: elementReference}
	builder.nodes[name] = node
	builder.statements = append(builder.statements, syntax.Statement{Node: &syntax.Node{
		Name: name, Element: elementReference,
	}})
	return node
}

// In and Out are phantom-typed port handles. The host compiler catches an
// accidental mismatch between handles; the graph compiler still verifies
// their claims against resolved descriptors.
type In[T any] struct{ endpoint syntax.Endpoint }
type Out[T any] struct{ endpoint syntax.Endpoint }

func Input[T any](node Node, port string) In[T] {
	return In[T]{endpoint: syntax.Endpoint{Node: node.name, Port: port}}
}

func Output[T any](node Node, port string) Out[T] {
	return Out[T]{endpoint: syntax.Endpoint{Node: node.name, Port: port}}
}

type ConnectOption func(*syntax.Edge)

func Lossy(edge *syntax.Edge) { edge.Delivery = syntax.Lossy }

func Named(name string) ConnectOption {
	return func(edge *syntax.Edge) { edge.Name = name }
}

// Connect adds one explicit, lossless edge unless an option selects loss.
func Connect[T any](builder *Builder, from Out[T], to In[T], options ...ConnectOption) {
	if builder.err != nil {
		return
	}
	edge := &syntax.Edge{From: from.endpoint, To: to.endpoint, Delivery: syntax.Lossless}
	for _, option := range options {
		if option != nil {
			option(edge)
		}
	}
	builder.statements = append(builder.statements, syntax.Statement{Edge: edge})
}

func ExportInput[T any](builder *Builder, name string, input In[T]) {
	builder.boundary(name, syntax.BoundaryInput, input.endpoint)
}

func ExportOutput[T any](builder *Builder, name string, output Out[T]) {
	builder.boundary(name, syntax.BoundaryOutput, output.endpoint)
}

func (builder *Builder) boundary(name string, direction syntax.BoundaryDirection, endpoint syntax.Endpoint) {
	if builder.err != nil {
		return
	}
	builder.statements = append(builder.statements, syntax.Statement{Boundary: &syntax.Boundary{
		Name: name, Direction: direction, Endpoint: endpoint,
	}})
}

// File returns the frontend-neutral AST in declaration order.
func (builder *Builder) File() (syntax.File, error) {
	if builder.err != nil {
		return syntax.File{}, builder.err
	}
	if strings.TrimSpace(builder.name) == "" {
		return syntax.File{}, fmt.Errorf("graph requires a name")
	}
	statements := append([]syntax.Statement(nil), builder.statements...)
	return syntax.File{Graph: syntax.Graph{Name: builder.name, Statements: statements}}, nil
}

// Source emits canonical `.ortg` for review or deployment.
func (builder *Builder) Source() (string, error) {
	file, err := builder.File()
	if err != nil {
		return "", err
	}
	return syntax.Format(file), nil
}

// Compile uses the same normative elaborator as declarative frontends.
func (builder *Builder) Compile(options graphcompiler.Options) (graphcompiler.Result, error) {
	file, err := builder.File()
	if err != nil {
		return graphcompiler.Result{}, err
	}
	return graphcompiler.Compile(file, options)
}

// Nodes returns stable instance names for generator diagnostics.
func (builder *Builder) Nodes() []string {
	names := make([]string, 0, len(builder.nodes))
	for name := range builder.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
