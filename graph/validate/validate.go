// Package validate provides graph-wide mechanical checks and named lint
// profiles over immutable Graph IR.
package validate

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Finding is stable and addressable by node, edge, or boundary identity.
type Finding struct {
	Severity Severity `json:"severity" yaml:"severity"`
	Code     string   `json:"code" yaml:"code"`
	Message  string   `json:"message" yaml:"message"`
	Node     string   `json:"node,omitempty" yaml:"node,omitempty"`
	Edge     string   `json:"edge,omitempty" yaml:"edge,omitempty"`
	Boundary string   `json:"boundary,omitempty" yaml:"boundary,omitempty"`
}

// Profile enables opinionated completeness checks independently of core type
// and safety errors.
type Profile struct {
	Name                        string
	RequireExportedBoundary     bool
	RequireTerminalOutcome      bool
	RequireInterruptDecision    bool
	RequireConsumedOutcomes     bool
	WarnLargeLosslessQueueDepth int
}

var (
	// Core contains only soundness and safety checks.
	Core = Profile{Name: "core"}
	// RealtimeAgent exposes design omissions that are usually important in an
	// interactive, deadline-sensitive agent without assuming an audio topology.
	RealtimeAgent = Profile{
		Name: "realtime-agent", RequireExportedBoundary: true,
		RequireTerminalOutcome: true, RequireInterruptDecision: true,
		RequireConsumedOutcomes: true, WarnLargeLosslessQueueDepth: 128,
	}
	// ConversationalVoice currently shares the general obligations; voice-
	// specific policy libraries can add named rules without changing Core.
	ConversationalVoice = Profile{
		Name: "conversational-voice", RequireExportedBoundary: true,
		RequireTerminalOutcome: true, RequireInterruptDecision: true,
		RequireConsumedOutcomes: true, WarnLargeLosslessQueueDepth: 64,
	}
	// ComputerUse keeps external-effect authority mandatory (a Core rule) and
	// applies the general realtime completeness checks.
	ComputerUse = Profile{
		Name: "computer-use", RequireExportedBoundary: true,
		RequireTerminalOutcome: true, RequireInterruptDecision: true,
		RequireConsumedOutcomes: true, WarnLargeLosslessQueueDepth: 64,
	}
)

// Check returns deterministic findings. An invalid IR value produces an E_IR
// error and no weaker follow-on guesses.
func Check(graph ir.Graph, profile Profile) []Finding {
	if err := graph.Validate(); err != nil {
		return []Finding{{Severity: Error, Code: "E_IR", Message: err.Error()}}
	}
	var findings []Finding
	findings = append(findings, cycleFindings(graph)...)
	findings = append(findings, authorityFindings(graph)...)
	findings = append(findings, profileFindings(graph, profile)...)
	sort.SliceStable(findings, func(left, right int) bool {
		leftKey := findingKey(findings[left])
		rightKey := findingKey(findings[right])
		return leftKey < rightKey
	})
	return findings
}

// Errors returns only findings that prevent mounting.
func Errors(findings []Finding) []Finding {
	var result []Finding
	for _, finding := range findings {
		if finding.Severity == Error {
			result = append(result, finding)
		}
	}
	return result
}

func cycleFindings(graph ir.Graph) []Finding {
	breaks := make(map[string]bool, len(graph.Nodes))
	adjacency := make(map[string][]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		breaks[node.ID] = node.Reaction.BreaksCycles
		if !node.Reaction.BreaksCycles {
			adjacency[node.ID] = nil
		}
	}
	for _, edge := range graph.Edges {
		if !breaks[edge.From.Node] && !breaks[edge.To.Node] {
			adjacency[edge.From.Node] = append(adjacency[edge.From.Node], edge.To.Node)
		}
	}
	for node := range adjacency {
		sort.Strings(adjacency[node])
	}

	var (
		index      int
		indices    = make(map[string]int, len(adjacency))
		lowlink    = make(map[string]int, len(adjacency))
		onStack    = make(map[string]bool, len(adjacency))
		stack      []string
		components [][]string
	)
	var visit func(string)
	visit = func(node string) {
		index++
		indices[node] = index
		lowlink[node] = index
		stack = append(stack, node)
		onStack[node] = true
		for _, next := range adjacency[node] {
			if indices[next] == 0 {
				visit(next)
				lowlink[node] = min(lowlink[node], lowlink[next])
			} else if onStack[next] {
				lowlink[node] = min(lowlink[node], indices[next])
			}
		}
		if lowlink[node] != indices[node] {
			return
		}
		var component []string
		for {
			last := len(stack) - 1
			member := stack[last]
			stack = stack[:last]
			onStack[member] = false
			component = append(component, member)
			if member == node {
				break
			}
		}
		sort.Strings(component)
		components = append(components, component)
	}
	names := make([]string, 0, len(adjacency))
	for node := range adjacency {
		names = append(names, node)
	}
	sort.Strings(names)
	for _, node := range names {
		if indices[node] == 0 {
			visit(node)
		}
	}

	var findings []Finding
	for _, component := range components {
		cyclic := len(component) > 1
		if len(component) == 1 {
			cyclic = slices.Contains(adjacency[component[0]], component[0])
		}
		if cyclic {
			findings = append(findings, Finding{
				Severity: Error, Code: "E_CAUSAL_CYCLE", Node: component[0],
				Message: fmt.Sprintf("cycle among [%s] has no explicit State, Delay, or other causal-break element",
					strings.Join(component, ", ")),
			})
		}
	}
	return findings
}

func authorityFindings(graph ir.Graph) []Finding {
	connectedInputs := make(map[string]struct{})
	for _, edge := range graph.Edges {
		connectedInputs[endpointGroup(edge.To)] = struct{}{}
	}
	for _, boundary := range graph.Boundaries {
		if boundary.Direction == ir.InputBoundary {
			connectedInputs[endpointGroup(boundary.Endpoint)] = struct{}{}
		}
	}
	var findings []Finding
	for _, node := range graph.Nodes {
		for _, effect := range node.Effects {
			if !effect.External {
				continue
			}
			var authorityPorts []string
			for _, port := range node.Ports {
				if port.Direction == element.Input && containsNamedType(port.Type, effect.Authority) {
					authorityPorts = append(authorityPorts, port.Name)
				}
			}
			if len(authorityPorts) == 0 {
				findings = append(findings, Finding{
					Severity: Error, Code: "E_AUTHORITY_PORT", Node: node.ID,
					Message: fmt.Sprintf("external effect %q requires authority type %s, but no input port carries it",
						effect.Name, effect.Authority),
				})
				continue
			}
			connected := false
			for _, port := range authorityPorts {
				if _, found := connectedInputs[node.ID+"."+port]; found {
					connected = true
					break
				}
			}
			if !connected {
				findings = append(findings, Finding{
					Severity: Error, Code: "E_AUTHORITY_UNCONNECTED", Node: node.ID,
					Message: fmt.Sprintf("external effect %q has no connected %s authority input",
						effect.Name, effect.Authority),
				})
			}
		}
	}
	return findings
}

func profileFindings(graph ir.Graph, profile Profile) []Finding {
	if profile.Name == "" || profile.Name == Core.Name {
		return nil
	}
	var findings []Finding
	if profile.RequireExportedBoundary && len(graph.Boundaries) == 0 {
		findings = append(findings, Finding{
			Severity: Warning, Code: "W_NO_BOUNDARY",
			Message: fmt.Sprintf("validation profile %s expects an explicit observation or action boundary", profile.Name),
		})
	}
	usedOutputs := make(map[string]struct{})
	usedInputs := make(map[string]struct{})
	for _, edge := range graph.Edges {
		usedOutputs[endpointGroup(edge.From)] = struct{}{}
		usedInputs[endpointGroup(edge.To)] = struct{}{}
		if profile.WarnLargeLosslessQueueDepth > 0 && edge.Delivery == ir.Lossless &&
			edge.Depth > profile.WarnLargeLosslessQueueDepth {
			findings = append(findings, Finding{
				Severity: Warning, Code: "W_DEEP_QUEUE", Edge: edge.ID,
				Message: fmt.Sprintf("lossless queue depth %d exceeds profile %s review threshold %d and may hide stale work",
					edge.Depth, profile.Name, profile.WarnLargeLosslessQueueDepth),
			})
		}
	}
	for _, boundary := range graph.Boundaries {
		if boundary.Direction == ir.InputBoundary {
			usedInputs[endpointGroup(boundary.Endpoint)] = struct{}{}
		} else {
			usedOutputs[endpointGroup(boundary.Endpoint)] = struct{}{}
		}
	}
	for _, node := range graph.Nodes {
		if profile.RequireTerminalOutcome && len(node.Reaction.Triggers) > 0 && len(node.Reaction.Outcomes) == 0 {
			findings = append(findings, Finding{
				Severity: Warning, Code: "W_NO_TERMINAL_OUTCOME", Node: node.ID,
				Message: "triggered reaction declares no terminal outcome port for success, cancellation, refusal, timeout, or failure",
			})
		}
		if profile.RequireInterruptDecision && len(node.Reaction.Triggers) > 0 {
			for _, interrupt := range node.Reaction.Interrupts {
				if _, connected := usedInputs[node.ID+"."+interrupt]; !connected {
					findings = append(findings, Finding{
						Severity: Warning, Code: "W_INTERRUPT_UNCONNECTED", Node: node.ID,
						Message: fmt.Sprintf("interrupt port %s.%s is unconnected; connect cancellation or an explicit IgnoreInterrupt boundary",
							node.ID, interrupt),
					})
				}
			}
		}
		if profile.RequireConsumedOutcomes {
			for _, outcome := range node.Reaction.Outcomes {
				if _, connected := usedOutputs[node.ID+"."+outcome]; !connected {
					findings = append(findings, Finding{
						Severity: Warning, Code: "W_OUTCOME_UNCONSUMED", Node: node.ID,
						Message: fmt.Sprintf("terminal outcome %s.%s is unconsumed; route it to supervision or an explicit terminal sink",
							node.ID, outcome),
					})
				}
			}
		}
	}
	return findings
}

func endpointGroup(endpoint ir.Endpoint) string { return endpoint.Node + "." + endpoint.Port }

func containsNamedType(value element.Type, name string) bool {
	if value.Name == name {
		return true
	}
	for _, argument := range value.Arguments {
		if containsNamedType(argument, name) {
			return true
		}
	}
	return false
}

func findingKey(finding Finding) string {
	return string(finding.Severity) + "\x00" + finding.Code + "\x00" + finding.Node + "\x00" +
		finding.Edge + "\x00" + finding.Boundary + "\x00" + finding.Message
}
