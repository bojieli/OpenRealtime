// Package deployment owns the separate, non-secret deployment-binding
// artifact. Topology says which contracts are connected and values configure
// their behavior; deployment selects concrete implementations, placement,
// transport, resources, and references into a separately managed secret
// catalog.
package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/ir"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/internal/strictyaml"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "openrealtime.ai/deployment/v1alpha1"

	maximumArtifactBytes = 16 << 20
	maximumNodes         = 65_536
	maximumMapEntries    = 4_096
	maximumStringBytes   = 64 << 10
)

// Document is deliberately sparse at the authoring surface. Bind expands it
// to one effective entry per graph node, using the locked element name as the
// default in-process implementation. This keeps ordinary built-ins concise
// while preserving a complete, fingerprinted deployment identity at runtime.
type Document struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Graph      string          `json:"graph" yaml:"graph"`
	Nodes      map[string]Node `json:"nodes,omitempty" yaml:"nodes,omitempty"`
}

// Node contains operational bindings only. Secrets are references, never
// credential bytes. Provider/model/prompt choices that alter behavior remain
// element values rather than being smuggled into this layer.
type Node struct {
	Implementation string            `json:"implementation,omitempty" yaml:"implementation,omitempty"`
	Placement      string            `json:"placement,omitempty" yaml:"placement,omitempty"`
	Transport      string            `json:"transport,omitempty" yaml:"transport,omitempty"`
	Resources      map[string]string `json:"resources,omitempty" yaml:"resources,omitempty"`
	Secrets        map[string]string `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

// Bound is the exact executable graph plus the complete effective deployment
// snapshot. Fingerprint identifies the canonical expanded document, not merely
// the sparse author input.
type Bound struct {
	Graph       ir.Graph
	Nodes       map[string]Node
	Fingerprint string
}

// ValidateSecretCatalog proves exact reference coverage without resolving a
// value. An unused catalog entry is refused as a likely typo or excess
// privilege; deployments that intentionally share a larger catalog should
// derive a graph-scoped view before launch.
func ValidateSecretCatalog(nodes map[string]Node, catalog graphsecret.Document) error {
	if err := graphsecret.Validate(catalog); err != nil {
		return fmt.Errorf("validate deployment secret catalog: %w", err)
	}
	required := make(map[string][]string)
	for nodeID, node := range nodes {
		for slot, reference := range node.Secrets {
			if err := graphsecret.ValidateReference(reference); err != nil {
				return fmt.Errorf("deployment node %s secret %s: %w", nodeID, slot, err)
			}
			required[reference] = append(required[reference], nodeID+"."+slot)
		}
	}
	for reference, uses := range required {
		if _, found := catalog.Secrets[reference]; !found {
			sort.Strings(uses)
			return fmt.Errorf("secret catalog %q is missing %s used by %s",
				catalog.Catalog, reference, strings.Join(uses, ", "))
		}
	}
	for reference := range catalog.Secrets {
		if _, used := required[reference]; !used {
			return fmt.Errorf("secret catalog %q contains unused reference %s",
				catalog.Catalog, reference)
		}
	}
	return nil
}

func ParseJSON(path string, source []byte) (Document, error) {
	if len(source) > maximumArtifactBytes {
		return Document{}, fmt.Errorf("parse deployment %s: artifact exceeds %d bytes", path, maximumArtifactBytes)
	}
	if err := strictjson.Validate(source); err != nil {
		return Document{}, fmt.Errorf("parse deployment %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("parse deployment %s: %w", path, err)
	}
	if err := requireEOF(decoder); err != nil {
		return Document{}, fmt.Errorf("parse deployment %s: %w", path, err)
	}
	return normalize(document)
}

func ParseYAML(path string, source []byte) (Document, error) {
	if len(source) > maximumArtifactBytes {
		return Document{}, fmt.Errorf("parse deployment %s: artifact exceeds %d bytes", path, maximumArtifactBytes)
	}
	var document Document
	if _, err := strictyaml.Decode(path, source, &document); err != nil {
		return Document{}, err
	}
	return normalize(document)
}

func MarshalJSON(document Document) ([]byte, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode deployment JSON: %w", err)
	}
	return append(payload, '\n'), nil
}

func MarshalYAML(document Document) ([]byte, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(normalized); err != nil {
		return nil, fmt.Errorf("encode deployment YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close deployment YAML encoder: %w", err)
	}
	return output.Bytes(), nil
}

// Bind validates graph coverage, expands defaults, records a per-node
// deployment reference/digest in Graph IR, and re-freezes the graph. It never
// resolves or reads a secret.
func Bind(graph ir.Graph, document Document) (Bound, error) {
	if err := graph.Validate(); err != nil {
		return Bound{}, fmt.Errorf("bind deployment: %w", err)
	}
	normalized, err := normalize(document)
	if err != nil {
		return Bound{}, err
	}
	if normalized.Graph != graph.ID {
		return Bound{}, fmt.Errorf("deployment graph %q does not match Graph IR %q", normalized.Graph, graph.ID)
	}
	known := make(map[string]struct{}, len(graph.Nodes))
	for _, node := range graph.Nodes {
		known[node.ID] = struct{}{}
	}
	for id := range normalized.Nodes {
		if _, found := known[id]; !found {
			return Bound{}, fmt.Errorf("deployment contains unknown node %q", id)
		}
	}

	effective := Document{
		APIVersion: APIVersion, Graph: graph.ID,
		Nodes: make(map[string]Node, len(graph.Nodes)),
	}
	boundGraph := graph
	boundGraph.Nodes = append([]ir.Node(nil), graph.Nodes...)
	for index := range boundGraph.Nodes {
		node := &boundGraph.Nodes[index]
		binding := cloneNode(normalized.Nodes[node.ID])
		if binding.Implementation == "" {
			binding.Implementation = node.Element.Name
		}
		effective.Nodes[node.ID] = binding
		canonical, digest, digestErr := digestNode(binding)
		if digestErr != nil {
			return Bound{}, fmt.Errorf("deployment node %s: %w", node.ID, digestErr)
		}
		_ = canonical // retained by effective.Nodes in typed form
		node.Implementation = binding.Implementation
		node.DeploymentReference = "deployment://" + graph.ID + "/" + node.ID
		node.DeploymentDigest = digest
	}
	frozen, err := ir.Freeze(boundGraph)
	if err != nil {
		return Bound{}, fmt.Errorf("freeze deployment-bound graph: %w", err)
	}
	fingerprint, err := digestDocument(effective)
	if err != nil {
		return Bound{}, err
	}
	return Bound{
		Graph: frozen, Nodes: cloneNodes(effective.Nodes), Fingerprint: fingerprint,
	}, nil
}

func normalize(document Document) (Document, error) {
	if document.APIVersion != APIVersion {
		return Document{}, fmt.Errorf("deployment apiVersion must be %q, got %q", APIVersion, document.APIVersion)
	}
	if err := exactNonempty("deployment graph", document.Graph); err != nil {
		return Document{}, err
	}
	if len(document.Nodes) > maximumNodes {
		return Document{}, fmt.Errorf("deployment contains %d nodes; maximum is %d", len(document.Nodes), maximumNodes)
	}
	normalized := Document{
		APIVersion: APIVersion, Graph: document.Graph,
		Nodes: make(map[string]Node, len(document.Nodes)),
	}
	for id, node := range document.Nodes {
		if err := exactNonempty("deployment node ID", id); err != nil {
			return Document{}, err
		}
		if len(id) > maximumStringBytes {
			return Document{}, fmt.Errorf("deployment node ID exceeds %d bytes", maximumStringBytes)
		}
		normalizedNode, err := normalizeNode(id, node)
		if err != nil {
			return Document{}, err
		}
		normalized.Nodes[id] = normalizedNode
	}
	return normalized, nil
}

func normalizeNode(id string, node Node) (Node, error) {
	for label, value := range map[string]string{
		"implementation": node.Implementation,
		"placement":      node.Placement,
		"transport":      node.Transport,
	} {
		if value != "" {
			if err := exactNonempty("deployment node "+id+" "+label, value); err != nil {
				return Node{}, err
			}
			if len(value) > maximumStringBytes {
				return Node{}, fmt.Errorf("deployment node %s %s exceeds %d bytes", id, label, maximumStringBytes)
			}
		}
	}
	if len(node.Resources) > maximumMapEntries || len(node.Secrets) > maximumMapEntries {
		return Node{}, fmt.Errorf("deployment node %s contains too many resource or secret bindings", id)
	}
	resources, err := normalizeStringMap("resource", id, node.Resources, false)
	if err != nil {
		return Node{}, err
	}
	secrets, err := normalizeStringMap("secret", id, node.Secrets, true)
	if err != nil {
		return Node{}, err
	}
	node.Resources = resources
	node.Secrets = secrets
	return node, nil
}

func normalizeStringMap(kind, nodeID string, source map[string]string, secret bool) (map[string]string, error) {
	if len(source) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		if err := exactNonempty("deployment node "+nodeID+" "+kind+" name", key); err != nil {
			return nil, err
		}
		if err := exactNonempty("deployment node "+nodeID+" "+kind+" value", value); err != nil {
			return nil, err
		}
		if len(key) > maximumStringBytes || len(value) > maximumStringBytes {
			return nil, fmt.Errorf("deployment node %s %s binding exceeds %d bytes", nodeID, kind, maximumStringBytes)
		}
		if secret {
			if err := validateSecretReference(value); err != nil {
				return nil, fmt.Errorf("deployment node %s secret %s: %w", nodeID, key, err)
			}
		}
		result[key] = value
	}
	return result, nil
}

func validateSecretReference(reference string) error {
	return graphsecret.ValidateReference(reference)
}

func digestNode(node Node) ([]byte, string, error) {
	// Secret slot names affect the element's dependency surface, but the
	// reference selected for each slot belongs to the separately attested
	// private deployment artifact. Redacting reference values here prevents a
	// secret rotation from perturbing—or becoming guessable through—the public
	// Graph IR fingerprint.
	public := cloneNode(node)
	for key := range public.Secrets {
		public.Secrets[key] = "secret://redacted"
	}
	payload, err := json.Marshal(public)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(payload)
	return payload, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func digestDocument(document Document) (string, error) {
	// encoding/json orders string map keys. Sorting the node list first makes
	// that deterministic property explicit to readers and future refactors.
	ids := make([]string, 0, len(document.Nodes))
	for id := range document.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type entry struct {
		ID   string `json:"id"`
		Node Node   `json:"node"`
	}
	canonical := struct {
		APIVersion string  `json:"apiVersion"`
		Graph      string  `json:"graph"`
		Nodes      []entry `json:"nodes"`
	}{APIVersion: APIVersion, Graph: document.Graph, Nodes: make([]entry, 0, len(ids))}
	for _, id := range ids {
		canonical.Nodes = append(canonical.Nodes, entry{ID: id, Node: document.Nodes[id]})
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical deployment: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func exactNonempty(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s cannot be empty", label)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s has surrounding whitespace", label)
	}
	return nil
}

func cloneNode(node Node) Node {
	node.Resources = cloneMap(node.Resources)
	node.Secrets = cloneMap(node.Secrets)
	return node
}

func cloneNodes(nodes map[string]Node) map[string]Node {
	result := make(map[string]Node, len(nodes))
	for id, node := range nodes {
		result[id] = cloneNode(node)
	}
	return result
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return nil
}
