package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

func TestBindExpandsDefaultsAndAttestsEveryNode(t *testing.T) {
	graph := testGraph(t)
	document, err := ParseYAML("agent.deployment.yaml", []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: deployment_test
nodes:
  runtime:
    placement: media-pool
    transport: in-process
    resources:
      gpu: shared
    secrets:
      apiKey: secret://providers/example/api-key
`))
	if err != nil {
		t.Fatalf("parse deployment: %v", err)
	}
	bound, err := Bind(graph, document)
	if err != nil {
		t.Fatalf("bind deployment: %v", err)
	}
	if err := bound.Graph.Validate(); err != nil {
		t.Fatalf("bound Graph IR: %v", err)
	}
	if !strings.HasPrefix(bound.Fingerprint, "sha256:") {
		t.Fatalf("deployment fingerprint = %q", bound.Fingerprint)
	}
	if got := bound.Nodes["runtime"].Implementation; got != "test.Pass" {
		t.Fatalf("default implementation = %q, want test.Pass", got)
	}
	node := bound.Graph.Nodes[0]
	if node.Implementation != "test.Pass" ||
		node.DeploymentReference != "deployment://deployment_test/runtime" ||
		!strings.HasPrefix(node.DeploymentDigest, "sha256:") {
		t.Fatalf("bound node deployment = %+v", node)
	}
	if node.ConfigReference != "values://deployment_test/runtime" || node.ConfigDigest == "" {
		t.Fatalf("deployment binding discarded values identity: %+v", node)
	}
	// Results own all maps rather than aliasing the parsed document or one
	// another through an internal effective document.
	document.Nodes["runtime"].Resources["gpu"] = "mutated"
	if got := bound.Nodes["runtime"].Resources["gpu"]; got != "shared" {
		t.Fatalf("bound resources aliased caller state: %q", got)
	}
}

func TestBindIsDeterministicAcrossStrictYAMLAndJSON(t *testing.T) {
	graph := testGraph(t)
	yamlDocument, err := ParseYAML("deployment.yaml", []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: deployment_test
nodes:
  runtime:
    implementation: plugin.example/pass
    resources:
      memory: 1Gi
      cpu: "2"
    secrets:
      token: secret://example/token
`))
	if err != nil {
		t.Fatal(err)
	}
	jsonDocument, err := ParseJSON("deployment.json", []byte(`{
  "nodes": {
    "runtime": {
      "secrets": {"token": "secret://example/token"},
      "resources": {"cpu": "2", "memory": "1Gi"},
      "implementation": "plugin.example/pass"
    }
  },
  "graph": "deployment_test",
  "apiVersion": "openrealtime.ai/deployment/v1alpha1"
}`))
	if err != nil {
		t.Fatal(err)
	}
	left, err := Bind(graph, yamlDocument)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Bind(graph, jsonDocument)
	if err != nil {
		t.Fatal(err)
	}
	if left.Fingerprint != right.Fingerprint || left.Graph.Fingerprint != right.Graph.Fingerprint {
		t.Fatalf("format-dependent identity: YAML %s/%s JSON %s/%s",
			left.Fingerprint, left.Graph.Fingerprint, right.Fingerprint, right.Graph.Fingerprint)
	}

	changed := jsonDocument
	changed.Nodes = cloneNodes(jsonDocument.Nodes)
	node := changed.Nodes["runtime"]
	node.Placement = "another-pool"
	changed.Nodes["runtime"] = node
	different, err := Bind(graph, changed)
	if err != nil {
		t.Fatal(err)
	}
	if different.Fingerprint == right.Fingerprint || different.Graph.Fingerprint == right.Graph.Fingerprint {
		t.Fatal("deployment change did not change deployment and Graph IR identities")
	}

	rotated := jsonDocument
	rotated.Nodes = cloneNodes(jsonDocument.Nodes)
	node = rotated.Nodes["runtime"]
	node.Secrets["token"] = "secret://example/rotated-token"
	rotated.Nodes["runtime"] = node
	secretRotation, err := Bind(graph, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if secretRotation.Fingerprint == right.Fingerprint {
		t.Fatal("secret-reference rotation did not change private deployment identity")
	}
	if secretRotation.Graph.Fingerprint != right.Graph.Fingerprint {
		t.Fatal("secret-reference rotation leaked into public Graph IR identity")
	}
}

func TestDeploymentParsersRejectAmbiguousOrSecretBearingInput(t *testing.T) {
	tests := []struct {
		name   string
		parse  func(string, []byte) (Document, error)
		source string
		want   string
	}{
		{
			name: "duplicate JSON key", parse: ParseJSON,
			source: `{"apiVersion":"openrealtime.ai/deployment/v1alpha1","graph":"g","graph":"h"}`,
			want:   "duplicate",
		},
		{
			name: "unknown JSON field", parse: ParseJSON,
			source: `{"apiVersion":"openrealtime.ai/deployment/v1alpha1","graph":"g","credential":"secret"}`,
			want:   "unknown field",
		},
		{
			name: "YAML alias", parse: ParseYAML,
			source: "apiVersion: openrealtime.ai/deployment/v1alpha1\ngraph: g\nnodes: &nodes {}\ncopy: *nodes\n",
			want:   "anchors and aliases",
		},
		{
			name: "literal secret", parse: ParseYAML,
			source: "apiVersion: openrealtime.ai/deployment/v1alpha1\ngraph: g\nnodes:\n  n:\n    secrets:\n      token: plaintext-token\n",
			want:   "secret://",
		},
		{
			name: "secret query", parse: ParseYAML,
			source: "apiVersion: openrealtime.ai/deployment/v1alpha1\ngraph: g\nnodes:\n  n:\n    secrets:\n      token: secret://vault/item?leak=value\n",
			want:   "secret://name",
		},
		{
			name: "surrounding whitespace", parse: ParseYAML,
			source: "apiVersion: openrealtime.ai/deployment/v1alpha1\ngraph: g\nnodes:\n  n:\n    placement: ' pool '\n",
			want:   "surrounding whitespace",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.parse("deployment", []byte(test.source)); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBindRejectsWrongCoverageAndGraphIRDetectsDeploymentTampering(t *testing.T) {
	graph := testGraph(t)
	for _, test := range []struct {
		name     string
		document Document
		want     string
	}{
		{
			name:     "wrong graph",
			document: Document{APIVersion: APIVersion, Graph: "other"},
			want:     "does not match",
		},
		{
			name: "unknown node",
			document: Document{APIVersion: APIVersion, Graph: graph.ID, Nodes: map[string]Node{
				"absent": {},
			}},
			want: "unknown node",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Bind(graph, test.document); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("bind error = %v, want %q", err, test.want)
			}
		})
	}

	bound, err := Bind(graph, Document{APIVersion: APIVersion, Graph: graph.ID})
	if err != nil {
		t.Fatal(err)
	}
	tampered := bound.Graph
	tampered.Nodes = append([]ir.Node(nil), bound.Graph.Nodes...)
	tampered.Nodes[0].DeploymentDigest = ""
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "present together") {
		t.Fatalf("incomplete deployment identity error = %v", err)
	}
	tampered = bound.Graph
	tampered.Nodes = append([]ir.Node(nil), bound.Graph.Nodes...)
	tampered.Nodes[0].DeploymentDigest = "sha256:" + strings.Repeat("0", 64)
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("deployment tampering error = %v", err)
	}
}

func TestValidateSecretCatalogRequiresExactGraphScopedCoverage(t *testing.T) {
	nodes := map[string]Node{
		"a": {Secrets: map[string]string{"token": "secret://providers/a/token"}},
		"b": {Secrets: map[string]string{"token": "secret://providers/a/token"}},
	}
	valid := graphsecret.Document{
		APIVersion: graphsecret.APIVersion, Catalog: "agent",
		Secrets: map[string]graphsecret.Binding{
			"secret://providers/a/token": {Provider: "env", Locator: "A_TOKEN"},
		},
	}
	if err := ValidateSecretCatalog(nodes, valid); err != nil {
		t.Fatalf("valid secret catalog: %v", err)
	}
	missing := valid
	missing.Secrets = map[string]graphsecret.Binding{}
	if err := ValidateSecretCatalog(nodes, missing); err == nil || !strings.Contains(err.Error(), "a.token, b.token") {
		t.Fatalf("missing reference error = %v", err)
	}
	unused := valid
	unused.Secrets = map[string]graphsecret.Binding{
		"secret://providers/a/token": {Provider: "env", Locator: "A_TOKEN"},
		"secret://providers/b/token": {Provider: "env", Locator: "B_TOKEN"},
	}
	if err := ValidateSecretCatalog(nodes, unused); err == nil || !strings.Contains(err.Error(), "unused reference") {
		t.Fatalf("unused reference error = %v", err)
	}
}

func TestMarshalRoundTripOwnsCanonicalDocuments(t *testing.T) {
	document := Document{APIVersion: APIVersion, Graph: "g", Nodes: map[string]Node{
		"n": {Implementation: "example/n", Secrets: map[string]string{"key": "secret://vault/key"}},
	}}
	for _, marshal := range []struct {
		name  string
		write func(Document) ([]byte, error)
		read  func(string, []byte) (Document, error)
	}{{"json", MarshalJSON, ParseJSON}, {"yaml", MarshalYAML, ParseYAML}} {
		t.Run(marshal.name, func(t *testing.T) {
			payload, err := marshal.write(document)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := marshal.read("deployment."+marshal.name, payload)
			if err != nil {
				t.Fatal(err)
			}
			left, _ := json.Marshal(document)
			right, _ := json.Marshal(parsed)
			if string(left) != string(right) {
				t.Fatalf("round trip = %s, want %s", right, left)
			}
		})
	}
}

func testGraph(t *testing.T) ir.Graph {
	t.Helper()
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Pass",
		Revision:      1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: element.Event(element.Named("test.Payload")), Cardinality: element.One, Required: true},
			{Name: "out", Direction: element.Output, Type: element.Event(element.Named("test.Payload")), Cardinality: element.One, Required: true},
		},
	}
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "deployment_test", Revision: 1,
		Nodes: []ir.Node{{
			ID: "runtime", Element: identity,
			ConfigReference: "values://deployment_test/runtime",
			ConfigDigest:    "sha256:" + strings.Repeat("1", 64),
			Ports: []ir.Port{
				{Name: "in", Direction: element.Input, Type: element.Event(element.Named("test.Payload")), Cardinality: element.One, Required: true},
				{Name: "out", Direction: element.Output, Type: element.Event(element.Named("test.Payload")), Cardinality: element.One, Required: true},
			},
		}},
		Boundaries: []ir.Boundary{
			{Name: "in", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "runtime", Port: "in"}, Type: element.Event(element.Named("test.Payload"))},
			{Name: "out", Direction: ir.OutputBoundary, Endpoint: ir.Endpoint{Node: "runtime", Port: "out"}, Type: element.Event(element.Named("test.Payload"))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}
