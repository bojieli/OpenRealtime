// Package manifest implements the strict normalized YAML/JSON graph frontend.
// It carries topology only; element values, deployment, locks, channel
// overrides, evidence, and secrets are separate artifacts.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/syntax"
	"gopkg.in/yaml.v3"
)

const APIVersion = "openrealtime.ai/graph/v1alpha1"

// Document is the normalized interchange representation.
type Document struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Graph      Graph  `json:"graph" yaml:"graph"`
}

type Graph struct {
	Name       string     `json:"name" yaml:"name"`
	Imports    []Import   `json:"imports,omitempty" yaml:"imports,omitempty"`
	Nodes      []Node     `json:"nodes" yaml:"nodes"`
	Edges      []Edge     `json:"edges,omitempty" yaml:"edges,omitempty"`
	Boundaries []Boundary `json:"boundaries,omitempty" yaml:"boundaries,omitempty"`
}

type Import struct {
	Path  string `json:"path" yaml:"path"`
	Alias string `json:"alias,omitempty" yaml:"alias,omitempty"`
}

type Node struct {
	ID      string `json:"id" yaml:"id"`
	Element string `json:"element" yaml:"element"`
}

type Edge struct {
	ID       string `json:"id,omitempty" yaml:"id,omitempty"`
	From     string `json:"from" yaml:"from"`
	To       string `json:"to" yaml:"to"`
	Delivery string `json:"delivery" yaml:"delivery"`
}

type Boundary struct {
	Name      string `json:"name" yaml:"name"`
	Direction string `json:"direction" yaml:"direction"`
	Endpoint  string `json:"endpoint" yaml:"endpoint"`
}

// Error is a source-mapped normalized-manifest diagnostic.
type Error struct {
	Path    string
	Line    int
	Column  int
	Message string
}

func (failure *Error) Error() string {
	location := fmt.Sprintf("%d:%d", failure.Line, failure.Column)
	if failure.Path != "" {
		location = failure.Path + ":" + location
	}
	return location + ": " + failure.Message
}

// ParseYAML parses a deliberately strict YAML 1.2-like subset. Aliases,
// anchors, merge keys, custom tags, duplicate keys, multiple documents, and
// unknown fields are rejected.
func ParseYAML(path string, source []byte) (syntax.File, error) {
	var tree yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	if err := decoder.Decode(&tree); err != nil {
		return syntax.File{}, &Error{Path: path, Line: 1, Column: 1, Message: "decode YAML: " + err.Error()}
	}
	if len(tree.Content) == 0 {
		return syntax.File{}, &Error{Path: path, Line: 1, Column: 1, Message: "empty graph manifest"}
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err == nil {
		return syntax.File{}, &Error{Path: path, Line: trailing.Line, Column: trailing.Column, Message: "multiple YAML documents are not allowed"}
	} else if !errors.Is(err, io.EOF) {
		return syntax.File{}, &Error{Path: path, Line: 1, Column: 1, Message: "decode trailing YAML: " + err.Error()}
	}
	if err := validateYAMLNode(path, tree.Content[0]); err != nil {
		return syntax.File{}, err
	}

	strict := yaml.NewDecoder(bytes.NewReader(source))
	strict.KnownFields(true)
	var document Document
	if err := strict.Decode(&document); err != nil {
		return syntax.File{}, yamlDecodeError(path, err)
	}
	return lower(path, document, tree.Content[0])
}

// ParseJSON parses strict normalized JSON with unknown-field and trailing-data
// rejection. JSON is a first-class equivalent of the YAML interchange form.
func ParseJSON(path string, source []byte) (syntax.File, error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return syntax.File{}, &Error{Path: path, Line: 1, Column: 1, Message: "decode JSON: " + err.Error()}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return syntax.File{}, &Error{Path: path, Line: 1, Column: 1, Message: "trailing JSON value"}
	} else if !errors.Is(err, io.EOF) {
		return syntax.File{}, &Error{Path: path, Line: 1, Column: 1, Message: "decode trailing JSON: " + err.Error()}
	}
	return lower(path, document, nil)
}

// FromSyntax converts a parsed netlist into normalized interchange data.
func FromSyntax(file syntax.File) Document {
	document := Document{APIVersion: APIVersion, Graph: Graph{Name: file.Graph.Name}}
	for _, imported := range file.Imports {
		document.Graph.Imports = append(document.Graph.Imports, Import{Path: imported.Path, Alias: imported.Alias})
	}
	for _, node := range file.Graph.Nodes() {
		document.Graph.Nodes = append(document.Graph.Nodes, Node{ID: node.Name, Element: node.Element})
	}
	for _, edge := range file.Graph.Edges() {
		document.Graph.Edges = append(document.Graph.Edges, Edge{
			ID: edge.Name, From: edge.From.String(), To: edge.To.String(), Delivery: string(edge.Delivery),
		})
	}
	for _, boundary := range file.Graph.Boundaries() {
		document.Graph.Boundaries = append(document.Graph.Boundaries, Boundary{
			Name: boundary.Name, Direction: string(boundary.Direction), Endpoint: boundary.Endpoint.String(),
		})
	}
	return document
}

// MarshalJSON returns deterministic, pretty, newline-terminated JSON.
func MarshalJSON(document Document) ([]byte, error) {
	if _, err := lower("", document, nil); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode graph manifest JSON: %w", err)
	}
	return append(payload, '\n'), nil
}

// MarshalYAML returns canonical, newline-terminated YAML using only the strict
// subset accepted by ParseYAML.
func MarshalYAML(document Document) ([]byte, error) {
	if _, err := lower("", document, nil); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode graph manifest YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close graph manifest YAML encoder: %w", err)
	}
	return output.Bytes(), nil
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
var qualifiedPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*(?:\.[A-Za-z_][A-Za-z0-9_-]*)+$`)

func lower(path string, document Document, root *yaml.Node) (syntax.File, error) {
	rootSpan := spanFor(root)
	if document.APIVersion != APIVersion {
		return syntax.File{}, manifestError(path, root, fmt.Sprintf("apiVersion must be %q", APIVersion))
	}
	if !identifierPattern.MatchString(document.Graph.Name) {
		return syntax.File{}, manifestError(path, mappingValue(root, "graph"),
			fmt.Sprintf("invalid graph name %q", document.Graph.Name))
	}
	if len(document.Graph.Nodes) == 0 {
		return syntax.File{}, manifestError(path, mappingValue(root, "graph"), "graph must contain at least one node")
	}
	file := syntax.File{Path: path, Graph: syntax.Graph{Name: document.Graph.Name, Span: rootSpan}}
	importsNode := nestedSequence(root, "graph", "imports")
	for index, imported := range document.Graph.Imports {
		position := sequenceItem(importsNode, index)
		if strings.TrimSpace(imported.Path) == "" {
			return syntax.File{}, manifestError(path, position, "import path cannot be empty")
		}
		if imported.Alias != "" && !identifierPattern.MatchString(imported.Alias) {
			return syntax.File{}, manifestError(path, position, fmt.Sprintf("invalid import alias %q", imported.Alias))
		}
		file.Imports = append(file.Imports, syntax.Import{
			Path: imported.Path, Alias: imported.Alias, Span: spanFor(position),
		})
	}
	nodesNode := nestedSequence(root, "graph", "nodes")
	for index, node := range document.Graph.Nodes {
		position := sequenceItem(nodesNode, index)
		if !identifierPattern.MatchString(node.ID) {
			return syntax.File{}, manifestError(path, position, fmt.Sprintf("invalid node ID %q", node.ID))
		}
		if !qualifiedPattern.MatchString(node.Element) {
			return syntax.File{}, manifestError(path, position, fmt.Sprintf("invalid element reference %q", node.Element))
		}
		file.Graph.Statements = append(file.Graph.Statements, syntax.Statement{Node: &syntax.Node{
			Name: node.ID, Element: node.Element, Span: spanFor(position),
		}})
	}
	edgesNode := nestedSequence(root, "graph", "edges")
	for index, edge := range document.Graph.Edges {
		position := sequenceItem(edgesNode, index)
		from, err := parseEndpoint(path, position, edge.From)
		if err != nil {
			return syntax.File{}, err
		}
		to, err := parseEndpoint(path, position, edge.To)
		if err != nil {
			return syntax.File{}, err
		}
		if edge.ID != "" && !identifierPattern.MatchString(edge.ID) {
			return syntax.File{}, manifestError(path, position, fmt.Sprintf("invalid edge ID %q", edge.ID))
		}
		var delivery syntax.Delivery
		switch edge.Delivery {
		case string(syntax.Lossless):
			delivery = syntax.Lossless
		case string(syntax.Lossy):
			delivery = syntax.Lossy
		default:
			return syntax.File{}, manifestError(path, position,
				fmt.Sprintf("edge delivery must be %q or %q, got %q", syntax.Lossless, syntax.Lossy, edge.Delivery))
		}
		file.Graph.Statements = append(file.Graph.Statements, syntax.Statement{Edge: &syntax.Edge{
			Name: edge.ID, From: from, To: to, Delivery: delivery, Span: spanFor(position),
		}})
	}
	boundariesNode := nestedSequence(root, "graph", "boundaries")
	for index, boundary := range document.Graph.Boundaries {
		position := sequenceItem(boundariesNode, index)
		endpoint, err := parseEndpoint(path, position, boundary.Endpoint)
		if err != nil {
			return syntax.File{}, err
		}
		if !identifierPattern.MatchString(boundary.Name) {
			return syntax.File{}, manifestError(path, position, fmt.Sprintf("invalid boundary name %q", boundary.Name))
		}
		var direction syntax.BoundaryDirection
		switch boundary.Direction {
		case string(syntax.BoundaryInput):
			direction = syntax.BoundaryInput
		case string(syntax.BoundaryOutput):
			direction = syntax.BoundaryOutput
		default:
			return syntax.File{}, manifestError(path, position,
				fmt.Sprintf("boundary direction must be %q or %q, got %q",
					syntax.BoundaryInput, syntax.BoundaryOutput, boundary.Direction))
		}
		file.Graph.Statements = append(file.Graph.Statements, syntax.Statement{Boundary: &syntax.Boundary{
			Name: boundary.Name, Direction: direction, Endpoint: endpoint, Span: spanFor(position),
		}})
	}
	return file, nil
}

func parseEndpoint(path string, node *yaml.Node, value string) (syntax.Endpoint, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || !identifierPattern.MatchString(parts[0]) || !identifierPattern.MatchString(parts[1]) {
		return syntax.Endpoint{}, manifestError(path, node,
			fmt.Sprintf("endpoint must be instance.port, got %q", value))
	}
	span := spanFor(node)
	return syntax.Endpoint{Node: parts[0], Port: parts[1], Span: span}, nil
}

func validateYAMLNode(path string, node *yaml.Node) error {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return manifestError(path, node, "YAML anchors and aliases are not allowed")
	}
	switch node.Kind {
	case yaml.MappingNode:
		if node.Tag != "!!map" {
			return manifestError(path, node, fmt.Sprintf("custom YAML tag %q is not allowed", node.Tag))
		}
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return manifestError(path, key, "YAML mapping keys must be plain strings")
			}
			if key.Value == "<<" {
				return manifestError(path, key, "YAML merge keys are not allowed")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return manifestError(path, key, fmt.Sprintf("duplicate YAML key %q", key.Value))
			}
			seen[key.Value] = struct{}{}
			if err := validateYAMLNode(path, node.Content[index+1]); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return manifestError(path, node, fmt.Sprintf("custom YAML tag %q is not allowed", node.Tag))
		}
		for _, child := range node.Content {
			if err := validateYAMLNode(path, child); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" && node.Tag != "!!null" {
			return manifestError(path, node,
				fmt.Sprintf("graph topology values must be strings, got YAML tag %q", node.Tag))
		}
	default:
		return manifestError(path, node, fmt.Sprintf("unsupported YAML node kind %d", node.Kind))
	}
	return nil
}

func yamlDecodeError(path string, err error) error {
	return &Error{Path: path, Line: 1, Column: 1, Message: "decode YAML: " + err.Error()}
}

func manifestError(path string, node *yaml.Node, message string) error {
	line, column := 1, 1
	if node != nil {
		if node.Line > 0 {
			line = node.Line
		}
		if node.Column > 0 {
			column = node.Column
		}
	}
	return &Error{Path: path, Line: line, Column: column, Message: message}
}

func spanFor(node *yaml.Node) syntax.Span {
	position := syntax.Position{Line: 1, Column: 1}
	if node != nil {
		position.Line = max(1, node.Line)
		position.Column = max(1, node.Column)
	}
	end := position
	if node != nil && node.Kind == yaml.ScalarNode {
		end.Column += len([]rune(node.Value))
	}
	return syntax.Span{Start: position, End: end}
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func nestedSequence(root *yaml.Node, parent, field string) *yaml.Node {
	return mappingValue(mappingValue(root, parent), field)
}

func sequenceItem(sequence *yaml.Node, index int) *yaml.Node {
	if sequence == nil || sequence.Kind != yaml.SequenceNode || index < 0 || index >= len(sequence.Content) {
		return nil
	}
	return sequence.Content[index]
}
