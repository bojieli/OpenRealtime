// Package values owns the separate, schema-addressed element-values artifact.
// It binds node-local JSON objects to stable topology node IDs and folds only
// their references and canonical digests into Graph IR. Values never appear in
// .ortg source, and deployment/secrets remain separate again.
package values

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/internal/strictyaml"
	"gopkg.in/yaml.v3"
)

const APIVersion = "openrealtime.ai/config/v1alpha1"

type Document struct {
	APIVersion string                     `json:"apiVersion" yaml:"apiVersion"`
	Graph      string                     `json:"graph" yaml:"graph"`
	Nodes      map[string]json.RawMessage `json:"nodes" yaml:"-"`
}

type yamlDocument struct {
	APIVersion string                `yaml:"apiVersion"`
	Graph      string                `yaml:"graph"`
	Nodes      map[string]yamlObject `yaml:"nodes"`
}

// yamlObject preserves numeric scalars as exact JSON numbers. Decoding YAML
// through map[string]any converts decimals to float64, and encoding
// json.Number directly makes yaml.v3 quote it as a string; either behavior
// changes signed configuration identity across an otherwise harmless format
// round trip.
type yamlObject struct {
	Raw json.RawMessage
}

func (object *yamlObject) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return errors.New("element value must be a YAML mapping")
	}
	value, err := yamlNodeValue(node)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	object.Raw = encoded
	return nil
}

func (object yamlObject) MarshalYAML() (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(object.Raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return jsonValueYAMLNode(value)
}

// Bound is the exact executable graph and the canonical values that satisfy
// its node config digests. Values returns independent byte slices suitable for
// runtime mount.
type Bound struct {
	Graph       ir.Graph
	Values      map[string]json.RawMessage
	Fingerprint string
}

func ParseJSON(path string, source []byte) (Document, error) {
	if err := strictjson.Validate(source); err != nil {
		return Document{}, fmt.Errorf("parse values %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("parse values %s: %w", path, err)
	}
	if err := requireEOF(decoder); err != nil {
		return Document{}, fmt.Errorf("parse values %s: %w", path, err)
	}
	return normalize(document)
}

func ParseYAML(path string, source []byte) (Document, error) {
	var decoded yamlDocument
	if _, err := strictyaml.Decode(path, source, &decoded); err != nil {
		return Document{}, err
	}
	document := Document{
		APIVersion: decoded.APIVersion, Graph: decoded.Graph,
		Nodes: make(map[string]json.RawMessage, len(decoded.Nodes)),
	}
	for node, value := range decoded.Nodes {
		document.Nodes[node] = value.Raw
	}
	return normalize(document)
}

func normalize(document Document) (Document, error) {
	if document.APIVersion != APIVersion {
		return Document{}, fmt.Errorf("values apiVersion must be %q, got %q", APIVersion, document.APIVersion)
	}
	if strings.TrimSpace(document.Graph) == "" {
		return Document{}, errors.New("values artifact requires a graph ID")
	}
	if document.Nodes == nil {
		document.Nodes = make(map[string]json.RawMessage)
	}
	canonical := make(map[string]json.RawMessage, len(document.Nodes))
	for node, raw := range document.Nodes {
		if strings.TrimSpace(node) == "" {
			return Document{}, errors.New("values artifact contains an empty node ID")
		}
		normalized, _, err := Digest(raw)
		if err != nil {
			return Document{}, fmt.Errorf("values node %s: %w", node, err)
		}
		canonical[node] = normalized
	}
	document.Nodes = canonical
	return document, nil
}

// Digest canonicalizes one strict JSON object and returns its content digest.
// Duplicate keys and trailing values are rejected before ordinary decoding.
func Digest(raw json.RawMessage) (json.RawMessage, string, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := strictjson.Validate(raw); err != nil {
		return nil, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, "", errors.New("element value must be a JSON object")
	}
	if err := requireEOF(decoder); err != nil {
		return nil, "", err
	}
	value, err := normalizeNumbers(value)
	if err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(canonical)
	return canonical, "sha256:" + hex.EncodeToString(digest[:]), nil
}

const maxExpandedNumberZeros = 1024

// normalizeNumbers gives equivalent JSON/YAML numeric spellings one identity
// without converting through float64. Model budgets and limits are often
// integers larger than JavaScript's exact range, while temperatures and
// thresholds are decimals; retaining the exact decimal value avoids both
// precision loss and format-dependent config digests.
func normalizeNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		normalized, err := normalizeNumber(typed.String())
		if err != nil {
			return nil, err
		}
		return json.Number(normalized), nil
	case []any:
		for index := range typed {
			normalized, err := normalizeNumbers(typed[index])
			if err != nil {
				return nil, err
			}
			typed[index] = normalized
		}
		return typed, nil
	case map[string]any:
		for key, item := range typed {
			normalized, err := normalizeNumbers(item)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
		return typed, nil
	default:
		return value, nil
	}
}

func normalizeNumber(source string) (string, error) {
	original := source
	negative := strings.HasPrefix(source, "-")
	if negative {
		source = strings.TrimPrefix(source, "-")
	}
	mantissa, exponentText := source, "0"
	if separator := strings.IndexAny(source, "eE"); separator >= 0 {
		mantissa, exponentText = source[:separator], source[separator+1:]
	}
	var exponent big.Int
	if _, ok := exponent.SetString(exponentText, 10); !ok {
		return "", fmt.Errorf("invalid JSON number %q", original)
	}
	fractionDigits := 0
	if point := strings.IndexByte(mantissa, '.'); point >= 0 {
		fractionDigits = len(mantissa) - point - 1
		mantissa = mantissa[:point] + mantissa[point+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return "0", nil
	}
	withoutTrailing := strings.TrimRight(digits, "0")
	trailingZeros := len(digits) - len(withoutTrailing)
	digits = withoutTrailing
	exponent.Add(&exponent, big.NewInt(int64(trailingZeros-fractionDigits)))

	sign := ""
	if negative {
		sign = "-"
	}
	if exponent.Sign() >= 0 && exponent.IsInt64() && exponent.Int64() <= maxExpandedNumberZeros {
		return sign + digits + strings.Repeat("0", int(exponent.Int64())), nil
	}
	if exponent.Sign() < 0 {
		var position big.Int
		position.Add(&exponent, big.NewInt(int64(len(digits))))
		if position.Sign() > 0 && position.IsInt64() {
			point := int(position.Int64())
			return sign + digits[:point] + "." + digits[point:], nil
		}
		var leadingZeros big.Int
		leadingZeros.Neg(&position)
		if leadingZeros.IsInt64() && leadingZeros.Int64() <= maxExpandedNumberZeros {
			return sign + "0." + strings.Repeat("0", int(leadingZeros.Int64())) + digits, nil
		}
	}

	var scientificExponent big.Int
	scientificExponent.Add(&exponent, big.NewInt(int64(len(digits)-1)))
	mantissa = digits[:1]
	if len(digits) > 1 {
		mantissa += "." + digits[1:]
	}
	if scientificExponent.Sign() == 0 {
		return sign + mantissa, nil
	}
	return sign + mantissa + "e" + scientificExponent.String(), nil
}

// Bind validates node coverage, canonicalizes all values, sets every node's
// config reference/digest, and re-freezes Graph IR. Missing entries mean the
// empty object; unknown entries are errors rather than ignored typos.
func Bind(graph ir.Graph, document Document) (Bound, error) {
	if err := graph.Validate(); err != nil {
		return Bound{}, fmt.Errorf("bind values: %w", err)
	}
	normalized, err := normalize(document)
	if err != nil {
		return Bound{}, err
	}
	if normalized.Graph != graph.ID {
		return Bound{}, fmt.Errorf("values target graph %q, Graph IR is %q", normalized.Graph, graph.ID)
	}
	known := make(map[string]struct{}, len(graph.Nodes))
	for _, node := range graph.Nodes {
		known[node.ID] = struct{}{}
	}
	unknown := make([]string, 0)
	for node := range normalized.Nodes {
		if _, found := known[node]; !found {
			unknown = append(unknown, node)
		}
	}
	if len(unknown) != 0 {
		sort.Strings(unknown)
		return Bound{}, fmt.Errorf("values refer to unknown graph node(s): %s", strings.Join(unknown, ", "))
	}

	boundGraph := graph
	boundGraph.Nodes = append([]ir.Node(nil), graph.Nodes...)
	runtimeValues := make(map[string]json.RawMessage, len(graph.Nodes))
	digests := make(map[string]string, len(graph.Nodes))
	for index := range boundGraph.Nodes {
		node := &boundGraph.Nodes[index]
		raw, found := normalized.Nodes[node.ID]
		if !found {
			raw = json.RawMessage("{}")
		}
		canonical, digest, err := Digest(raw)
		if err != nil {
			return Bound{}, fmt.Errorf("values node %s: %w", node.ID, err)
		}
		node.ConfigReference = fmt.Sprintf("values://%s/%s", graph.ID, node.ID)
		node.ConfigDigest = digest
		runtimeValues[node.ID] = append(json.RawMessage(nil), canonical...)
		digests[node.ID] = digest
	}
	boundGraph, err = ir.Freeze(boundGraph)
	if err != nil {
		return Bound{}, fmt.Errorf("freeze graph with values: %w", err)
	}
	fingerprint, err := valuesFingerprint(graph.ID, digests)
	if err != nil {
		return Bound{}, err
	}
	return Bound{Graph: boundGraph, Values: runtimeValues, Fingerprint: fingerprint}, nil
}

func valuesFingerprint(graphID string, digests map[string]string) (string, error) {
	payload, err := json.Marshal(struct {
		Graph string            `json:"graph"`
		Nodes map[string]string `json:"nodes"`
	}{Graph: graphID, Nodes: digests})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func MarshalJSON(document Document) ([]byte, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func MarshalYAML(document Document) ([]byte, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	converted := yamlDocument{
		APIVersion: normalized.APIVersion, Graph: normalized.Graph,
		Nodes: make(map[string]yamlObject, len(normalized.Nodes)),
	}
	for node, raw := range normalized.Nodes {
		converted.Nodes[node] = yamlObject{Raw: raw}
	}
	payload, err := yaml.Marshal(converted)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func yamlNodeValue(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		value := make(map[string]any, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			item, err := yamlNodeValue(node.Content[index+1])
			if err != nil {
				return nil, err
			}
			value[key.Value] = item
		}
		return value, nil
	case yaml.SequenceNode:
		value := make([]any, len(node.Content))
		for index, child := range node.Content {
			item, err := yamlNodeValue(child)
			if err != nil {
				return nil, err
			}
			value[index] = item
		}
		return value, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str":
			return node.Value, nil
		case "!!bool":
			switch node.Value {
			case "true":
				return true, nil
			case "false":
				return false, nil
			default:
				return nil, fmt.Errorf("unsupported YAML boolean %q", node.Value)
			}
		case "!!null":
			return nil, nil
		case "!!int", "!!float":
			decoder := json.NewDecoder(strings.NewReader(node.Value))
			decoder.UseNumber()
			var number any
			if err := decoder.Decode(&number); err != nil {
				return nil, fmt.Errorf("YAML number %q must use JSON decimal syntax: %w", node.Value, err)
			}
			if err := requireEOF(decoder); err != nil {
				return nil, fmt.Errorf("YAML number %q must use JSON decimal syntax: %w", node.Value, err)
			}
			typed, ok := number.(json.Number)
			if !ok {
				return nil, fmt.Errorf("YAML number %q must use JSON decimal syntax", node.Value)
			}
			return typed, nil
		default:
			return nil, fmt.Errorf("unsupported YAML scalar tag %q", node.Tag)
		}
	default:
		return nil, fmt.Errorf("unsupported YAML node kind %d", node.Kind)
	}
}

func jsonValueYAMLNode(value any) (*yaml.Node, error) {
	switch typed := value.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: typed}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(typed)}, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(typed.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: typed.String()}, nil
	case []any:
		node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range typed {
			child, err := jsonValueYAMLNode(item)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, child)
		}
		return node, nil
	case map[string]any:
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, err := jsonValueYAMLNode(typed[key])
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
		}
		return node, nil
	default:
		return nil, fmt.Errorf("cannot encode JSON value of type %T as YAML", value)
	}
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
