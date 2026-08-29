// Package inspect derives static and live inspection models and generated
// diagrams from the exact immutable Graph IR mounted by the runtime.
package inspect

import (
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// Model is the frontend-neutral static graph view consumed by a canvas,
// console, or documentation renderer.
type Model struct {
	GraphID     string     `json:"graph_id"`
	Revision    uint64     `json:"revision"`
	Fingerprint string     `json:"fingerprint"`
	Nodes       []Node     `json:"nodes"`
	Edges       []Edge     `json:"edges,omitempty"`
	Boundaries  []Boundary `json:"boundaries,omitempty"`
	Scopes      []Scope    `json:"scopes,omitempty"`
}

type Node struct {
	ID           string               `json:"id"`
	Element      element.Identity     `json:"element"`
	Ports        []Port               `json:"ports"`
	Dependencies []element.Dependency `json:"dependencies,omitempty"`
	Effects      []element.Effect     `json:"effects,omitempty"`
}

type Port struct {
	Name        string              `json:"name"`
	Direction   element.Direction   `json:"direction"`
	Type        string              `json:"type"`
	Cardinality element.Cardinality `json:"cardinality"`
	Lanes       []string            `json:"lanes,omitempty"`
	Role        string              `json:"role"`
}

type Edge struct {
	ID       string      `json:"id"`
	From     ir.Endpoint `json:"from"`
	To       ir.Endpoint `json:"to"`
	Type     string      `json:"type"`
	Role     string      `json:"role"`
	Delivery ir.Delivery `json:"delivery"`
	Depth    int         `json:"depth"`
}

type Boundary struct {
	Name      string               `json:"name"`
	Direction ir.BoundaryDirection `json:"direction"`
	Endpoint  ir.Endpoint          `json:"endpoint"`
	Type      string               `json:"type"`
	Role      string               `json:"role"`
}

type Scope struct {
	ID         string           `json:"id"`
	Parent     string           `json:"parent,omitempty"`
	Composite  element.Identity `json:"composite"`
	Nodes      []string         `json:"nodes"`
	Boundaries []ScopeBoundary  `json:"boundaries,omitempty"`
}

type ScopeBoundary struct {
	Name      string               `json:"name"`
	Direction ir.BoundaryDirection `json:"direction"`
	Endpoint  ir.Endpoint          `json:"endpoint"`
	Type      string               `json:"type"`
}

// Live overlays runtime evidence without mutating the static graph model.
// Sequence makes snapshots monotonically comparable within one mount.
type Live struct {
	Fingerprint string              `json:"fingerprint"`
	Sequence    uint64              `json:"sequence"`
	ObservedAt  time.Time           `json:"observed_at"`
	Nodes       map[string]NodeLive `json:"nodes,omitempty"`
	Edges       map[string]EdgeLive `json:"edges,omitempty"`
}

type NodeLive struct {
	State          string `json:"state"`
	ActiveRuns     int    `json:"active_runs"`
	LastTriggerID  string `json:"last_trigger_id,omitempty"`
	LastOutcome    string `json:"last_outcome,omitempty"`
	FirstOutputNS  uint64 `json:"first_output_ns,omitempty"`
	CompletionNS   uint64 `json:"completion_ns,omitempty"`
	CancellationNS uint64 `json:"cancellation_ns,omitempty"`
	Error          string `json:"error,omitempty"`
}

type EdgeLive struct {
	Occupancy    int    `json:"occupancy"`
	HighWater    int    `json:"high_water"`
	Enqueued     uint64 `json:"enqueued"`
	Dequeued     uint64 `json:"dequeued"`
	Dropped      uint64 `json:"dropped"`
	Backpressure uint64 `json:"backpressure"`
	LastItemID   string `json:"last_item_id,omitempty"`
	QueueWaitNS  uint64 `json:"queue_wait_ns,omitempty"`
}

// Build validates and projects one exact graph.
func Build(graph ir.Graph) (Model, error) {
	if err := graph.Validate(); err != nil {
		return Model{}, err
	}
	model := Model{
		GraphID: graph.ID, Revision: graph.Revision, Fingerprint: graph.Fingerprint,
		Nodes: make([]Node, 0, len(graph.Nodes)), Edges: make([]Edge, 0, len(graph.Edges)),
		Boundaries: make([]Boundary, 0, len(graph.Boundaries)),
		Scopes:     make([]Scope, 0, len(graph.Scopes)),
	}
	for _, source := range graph.Nodes {
		node := Node{
			ID: source.ID, Element: source.Element,
			Dependencies: append([]element.Dependency(nil), source.Dependencies...),
			Effects:      append([]element.Effect(nil), source.Effects...),
		}
		for _, sourcePort := range source.Ports {
			node.Ports = append(node.Ports, Port{
				Name: sourcePort.Name, Direction: sourcePort.Direction,
				Type: sourcePort.Type.String(), Cardinality: sourcePort.Cardinality,
				Lanes: append([]string(nil), sourcePort.Lanes...), Role: protocolRole(sourcePort.Type),
			})
		}
		model.Nodes = append(model.Nodes, node)
	}
	for _, source := range graph.Edges {
		model.Edges = append(model.Edges, Edge{
			ID: source.ID, From: source.From, To: source.To, Type: source.Type.String(),
			Role: protocolRole(source.Type), Delivery: source.Delivery, Depth: source.Depth,
		})
	}
	for _, source := range graph.Boundaries {
		model.Boundaries = append(model.Boundaries, Boundary{
			Name: source.Name, Direction: source.Direction, Endpoint: source.Endpoint,
			Type: source.Type.String(), Role: protocolRole(source.Type),
		})
	}
	for _, source := range graph.Scopes {
		scope := Scope{
			ID: source.ID, Parent: source.Parent, Composite: source.Composite,
			Nodes: append([]string(nil), source.Nodes...),
		}
		for _, boundary := range source.Boundaries {
			scope.Boundaries = append(scope.Boundaries, ScopeBoundary{
				Name: boundary.Name, Direction: boundary.Direction,
				Endpoint: boundary.Endpoint, Type: boundary.Type.String(),
			})
		}
		model.Scopes = append(model.Scopes, scope)
	}
	return model, nil
}

func protocolRole(value element.Type) string {
	switch value.Name {
	case "Trigger":
		return "trigger"
	case "Interrupt":
		return "interrupt"
	case "State":
		return "state"
	case "Request", "Reply":
		return "control"
	default:
		return "data"
	}
}
