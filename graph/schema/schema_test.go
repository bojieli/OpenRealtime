package schema

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	configuredReference = "schema://vendor/model-config/v1"
	configuredSchemaID  = "https://schemas.example.test/model-config-v1.json"
)

type fixtureSpec struct {
	nodeID    string
	name      string
	configRef string
	stateRef  string
}

type fixture struct {
	graph       ir.Graph
	catalog     *resolve.Catalog
	descriptors []element.Descriptor
}

type resolverFunc func(context.Context, string) (ResolvedSchema, error)

func (function resolverFunc) ResolveConfigSchema(ctx context.Context, reference string) (ResolvedSchema, error) {
	return function(ctx, reference)
}

func TestEnvelopeOnlyGenerationIsHonestAndStrict(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{
		{nodeID: "configured", name: "test.Configured", configRef: configuredReference},
		{nodeID: "empty", name: "test.Empty"},
	})
	bundle, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if bundle.Complete {
		t.Fatal("envelope-only bundle unexpectedly complete")
	}
	if !reflect.DeepEqual(bundle.Unresolved, []string{configuredReference}) {
		t.Fatalf("Unresolved = %v", bundle.Unresolved)
	}
	if len(bundle.Contracts) != 1 || bundle.Contracts[0].Status != ContractUnresolved ||
		!reflect.DeepEqual(bundle.Contracts[0].NodeIDs, []string{"configured"}) {
		t.Fatalf("Contracts = %#v", bundle.Contracts)
	}
	if got := bundle.Nodes[0].NodeID; got != "configured" {
		t.Fatalf("first node = %q, want deterministic lexical order", got)
	}
	if bundle.Nodes[0].Status != ContractUnresolved || bundle.Nodes[1].Status != ContractEmptyObject {
		t.Fatalf("Nodes = %#v", bundle.Nodes)
	}

	root := decodeObject(t, bundle.Schema)
	if root["x-openrealtime-complete"] != false {
		t.Fatalf("schema completeness annotation = %#v", root["x-openrealtime-complete"])
	}
	properties := objectAt(t, root, "properties")
	nodesSchema := objectAt(t, properties, "nodes")
	nodeProperties := objectAt(t, nodesSchema, "properties")
	configured := objectAt(t, nodeProperties, "configured")
	if _, invented := configured["properties"]; invented {
		t.Fatalf("unresolved schema invented config fields: %#v", configured)
	}
	if _, closed := configured["additionalProperties"]; closed {
		t.Fatalf("unresolved schema falsely closed an unknown contract: %#v", configured)
	}
	if configured["type"] != "object" || configured["x-openrealtime-config-reference"] != configuredReference {
		t.Fatalf("unresolved schema = %#v", configured)
	}
	empty := objectAt(t, nodeProperties, "empty")
	if empty["type"] != "object" || empty["additionalProperties"] != false || numberText(empty["maxProperties"]) != "0" {
		t.Fatalf("configless node schema = %#v", empty)
	}

	compiled := compileBundle(t, bundle.Schema)
	assertValid(t, compiled, map[string]any{
		"apiVersion": graphvalues.APIVersion,
		"graph":      fixture.graph.ID,
		"nodes": map[string]any{
			"configured": map[string]any{"providerSpecific": true},
			"empty":      map[string]any{},
		},
	})
	assertValid(t, compiled, map[string]any{
		"apiVersion": graphvalues.APIVersion,
		"graph":      fixture.graph.ID,
	})
	assertInvalid(t, compiled, map[string]any{
		"apiVersion": graphvalues.APIVersion,
		"graph":      fixture.graph.ID,
		"nodes":      map[string]any{"unknown": map[string]any{}},
	})
	assertInvalid(t, compiled, map[string]any{
		"apiVersion": graphvalues.APIVersion,
		"graph":      fixture.graph.ID,
		"nodes":      map[string]any{"empty": map[string]any{"invented": true}},
	})
	assertInvalid(t, compiled, map[string]any{
		"apiVersion": graphvalues.APIVersion,
		"graph":      fixture.graph.ID,
		"nodes":      map[string]any{"configured": "not-an-object"},
	})
	assertInvalid(t, compiled, map[string]any{"graph": fixture.graph.ID})
}

func TestResolverBackedGenerationEmbedsAndValidatesContracts(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{
		{nodeID: "configured", name: "test.Configured", configRef: configuredReference},
		{nodeID: "configured_copy", name: "test.ConfiguredCopy", configRef: configuredReference},
		{nodeID: "empty", name: "test.Empty"},
	})
	var calls atomic.Int32
	resolver := resolverFunc(func(_ context.Context, reference string) (ResolvedSchema, error) {
		calls.Add(1)
		if reference != configuredReference {
			return ResolvedSchema{}, fmt.Errorf("unexpected reference %q", reference)
		}
		return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://schemas.example.test/model-config-v1.json",
  "type": "object",
  "additionalProperties": false,
  "properties": {"model": {"$ref": "#/$defs/nonempty"}},
  "required": ["model"],
  "$defs": {"nonempty": {"type": "string", "minLength": 1}}
}`)}, nil
	})
	bundle, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{
		Resolver: resolver, RequireResolved: true,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want one per distinct reference", calls.Load())
	}
	if !bundle.Complete || len(bundle.Unresolved) != 0 {
		t.Fatalf("complete = %v, unresolved = %v", bundle.Complete, bundle.Unresolved)
	}
	if len(bundle.Contracts) != 1 || bundle.Contracts[0].Definition != "config_0000" ||
		bundle.Contracts[0].SchemaID != configuredSchemaID ||
		!reflect.DeepEqual(bundle.Contracts[0].NodeIDs, []string{"configured", "configured_copy"}) {
		t.Fatalf("Contracts = %#v", bundle.Contracts)
	}
	root := decodeObject(t, bundle.Schema)
	definitions := objectAt(t, root, "$defs")
	if len(definitions) != 1 {
		t.Fatalf("$defs = %#v", definitions)
	}
	definition := objectAt(t, definitions, "config_0000")
	if definition["$id"] != configuredSchemaID {
		t.Fatalf("definition $id = %#v", definition["$id"])
	}

	compiled := compileBundle(t, bundle.Schema)
	valid := map[string]any{
		"apiVersion": graphvalues.APIVersion,
		"graph":      fixture.graph.ID,
		"nodes": map[string]any{
			"configured":      map[string]any{"model": "fast"},
			"configured_copy": map[string]any{"model": "slow"},
			"empty":           map[string]any{},
		},
	}
	assertValid(t, compiled, valid)
	invalidMissing := cloneJSONValue(t, valid)
	delete(invalidMissing["nodes"].(map[string]any)["configured"].(map[string]any), "model")
	assertInvalid(t, compiled, invalidMissing)
	invalidExtra := cloneJSONValue(t, valid)
	invalidExtra["nodes"].(map[string]any)["configured"].(map[string]any)["unknown"] = true
	assertInvalid(t, compiled, invalidExtra)
	invalidPrimitive := cloneJSONValue(t, valid)
	invalidPrimitive["nodes"].(map[string]any)["configured"] = true
	assertInvalid(t, compiled, invalidPrimitive)
}

func TestExactDescriptorAndIRContractsAreRequired(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{{
		nodeID: "configured", name: "test.Configured", configRef: configuredReference,
		stateRef: "schema://test/state/v1",
	}})
	t.Run("unknown", func(t *testing.T) {
		_, err := Generate(context.Background(), fixture.graph, resolve.NewCatalog(), Options{})
		if !errors.Is(err, ErrUnknownDescriptor) {
			t.Fatalf("error = %v, want ErrUnknownDescriptor", err)
		}
	})
	t.Run("stale exact identity", func(t *testing.T) {
		catalog := resolve.NewCatalog()
		changed := fixture.descriptors[0].Clone()
		changed.ConfigSchema = "schema://vendor/model-config/v2"
		if err := catalog.Register(changed); err != nil {
			t.Fatal(err)
		}
		_, err := Generate(context.Background(), fixture.graph, catalog, Options{})
		if !errors.Is(err, ErrUnknownDescriptor) {
			t.Fatalf("error = %v, want ErrUnknownDescriptor", err)
		}
	})
	t.Run("config schema mismatch", func(t *testing.T) {
		changed := fixture.graph
		changed.Nodes = slices.Clone(changed.Nodes)
		changed.Nodes[0].ConfigSchema = "schema://forged/config"
		changed = freezeGraph(t, changed)
		_, err := Generate(context.Background(), changed, fixture.catalog, Options{})
		if !errors.Is(err, ErrDescriptorContractMismatch) {
			t.Fatalf("error = %v, want ErrDescriptorContractMismatch", err)
		}
	})
	t.Run("state-transfer capability mismatch", func(t *testing.T) {
		changed := fixture.graph
		changed.Nodes = slices.Clone(changed.Nodes)
		changed.Nodes[0].StateTransfer = &element.StateTransferCapabilities{Restore: true}
		changed = freezeGraph(t, changed)
		_, err := Generate(context.Background(), changed, fixture.catalog, Options{})
		if !errors.Is(err, ErrDescriptorContractMismatch) ||
			!strings.Contains(err.Error(), "state-transfer capabilities") {
			t.Fatalf("error = %v, want state-transfer ErrDescriptorContractMismatch", err)
		}
	})
	t.Run("port metadata mismatch", func(t *testing.T) {
		changed := fixture.graph
		changed.Nodes = slices.Clone(changed.Nodes)
		changed.Nodes[0].Ports = slices.Clone(changed.Nodes[0].Ports)
		changed.Nodes[0].Ports[0].DefaultDepth = 7
		changed = freezeGraph(t, changed)
		_, err := Generate(context.Background(), changed, fixture.catalog, Options{})
		if !errors.Is(err, ErrDescriptorContractMismatch) {
			t.Fatalf("error = %v, want ErrDescriptorContractMismatch", err)
		}
	})
}

func TestMissingResolutionModes(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{{
		nodeID: "configured", name: "test.Configured", configRef: configuredReference,
	}})
	notFound := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
		return ResolvedSchema{}, fmt.Errorf("registry lookup: %w", ErrSchemaNotFound)
	})
	bundle, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: notFound})
	if err != nil {
		t.Fatalf("default generation error = %v", err)
	}
	if bundle.Complete || len(bundle.Contracts) != 1 || bundle.Contracts[0].Status != ContractUnresolved {
		t.Fatalf("bundle = %#v", bundle)
	}
	_, err = Generate(context.Background(), fixture.graph, fixture.catalog, Options{
		Resolver: notFound, RequireResolved: true,
	})
	if !errors.Is(err, ErrUnknownConfigContract) {
		t.Fatalf("required resolution error = %v", err)
	}
	_, err = Generate(context.Background(), fixture.graph, fixture.catalog, Options{RequireResolved: true})
	if !errors.Is(err, ErrUnknownConfigContract) {
		t.Fatalf("nil resolver required error = %v", err)
	}
}

func TestConfiglessGraphNeedsNoResolver(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{{nodeID: "empty", name: "test.Empty"}})
	resolver := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
		t.Fatal("resolver called for a configless graph")
		return ResolvedSchema{}, nil
	})
	bundle, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{
		Resolver: resolver, RequireResolved: true,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !bundle.Complete || len(bundle.Contracts) != 0 || bundle.Nodes[0].Status != ContractEmptyObject {
		t.Fatalf("bundle = %#v", bundle)
	}
}

func TestInvalidConstructionInputs(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{{nodeID: "empty", name: "test.Empty"}})
	if _, err := New(nil, fixture.graph, fixture.catalog, Options{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("nil context error = %v", err)
	}
	if _, err := New(context.Background(), fixture.graph, nil, Options{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("nil catalog error = %v", err)
	}
	if _, err := New(context.Background(), fixture.graph, fixture.catalog, Options{
		SchemaID: "relative/schema.json",
	}); !errors.Is(err, ErrInvalidSchemaID) {
		t.Fatalf("relative output schema ID error = %v", err)
	}
}

func TestInvalidResolvedSchemasAreRejected(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{{
		nodeID: "configured", name: "test.Configured", configRef: configuredReference,
	}})
	tests := []struct {
		name string
		id   string
		doc  string
	}{
		{name: "duplicate key", id: configuredSchemaID, doc: `{"type":"object","type":"object"}`},
		{name: "trailing value", id: configuredSchemaID, doc: `{"type":"object"} {"type":"object"}`},
		{name: "wrong draft", id: configuredSchemaID, doc: `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`},
		{name: "wrong draft type", id: configuredSchemaID, doc: `{"$schema":7,"type":"object"}`},
		{name: "boolean root", id: configuredSchemaID, doc: `true`},
		{name: "array root", id: configuredSchemaID, doc: `[]`},
		{name: "accepts primitive", id: configuredSchemaID, doc: `{"type":["object","string"]}`},
		{name: "external ref", id: configuredSchemaID, doc: `{"type":"object","$ref":"https://remote.example/schema"}`},
		{name: "external dynamic ref", id: configuredSchemaID, doc: `{"type":"object","$dynamicRef":"other.json#anchor"}`},
		{name: "nested ID", id: configuredSchemaID, doc: `{"type":"object","properties":{"x":{"$id":"https://nested.example/schema","type":"string"}}}`},
		{name: "nested dialect", id: configuredSchemaID, doc: `{"type":"object","properties":{"x":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string"}}}`},
		{name: "invalid keyword", id: configuredSchemaID, doc: `{"type":"not-a-json-type"}`},
		{name: "invalid subschema", id: configuredSchemaID, doc: `{"type":"object","properties":{"x":1}}`},
		{name: "mismatched root ID", id: configuredSchemaID, doc: `{"$id":"https://other.example/schema","type":"object"}`},
		{name: "relative resolver ID", id: "relative/schema.json", doc: `{"type":"object"}`},
		{name: "fragment resolver ID", id: configuredSchemaID + "#fragment", doc: `{"type":"object"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
				return ResolvedSchema{ID: test.id, Document: json.RawMessage(test.doc)}, nil
			})
			_, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: resolver})
			if !errors.Is(err, ErrInvalidResolvedSchema) {
				t.Fatalf("error = %v, want ErrInvalidResolvedSchema", err)
			}
		})
	}
}

func TestSchemaKeywordsInsideConstRemainInstanceData(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{{
		nodeID: "configured", name: "test.Configured", configRef: configuredReference,
	}})
	resolver := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
		return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(`{
  "type":"object",
  "properties":{"literal":{"const":{"$id":"not-a-resource","$ref":"https://not-a-schema.example"}}}
}`)}, nil
	})
	bundle, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: resolver})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	compiled := compileBundle(t, bundle.Schema)
	assertValid(t, compiled, map[string]any{
		"apiVersion": graphvalues.APIVersion,
		"graph":      fixture.graph.ID,
		"nodes": map[string]any{
			"configured": map[string]any{"literal": map[string]any{
				"$id": "not-a-resource", "$ref": "https://not-a-schema.example",
			}},
		},
	})
}

func TestSchemaIDCollisionsAndAliases(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{
		{nodeID: "first", name: "test.First", configRef: "schema://contract/first"},
		{nodeID: "second", name: "test.Second", configRef: "schema://contract/second"},
	})
	t.Run("generated root collision", func(t *testing.T) {
		resolver := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
			return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(`{"type":"object"}`)}, nil
		})
		_, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{
			SchemaID: configuredSchemaID, Resolver: resolver,
		})
		if !errors.Is(err, ErrSchemaIDCollision) {
			t.Fatalf("error = %v, want ErrSchemaIDCollision", err)
		}
	})
	t.Run("different documents", func(t *testing.T) {
		resolver := resolverFunc(func(_ context.Context, reference string) (ResolvedSchema, error) {
			doc := `{"type":"object","maxProperties":1}`
			if reference == "schema://contract/second" {
				doc = `{"type":"object","maxProperties":2}`
			}
			return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(doc)}, nil
		})
		_, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: resolver})
		if !errors.Is(err, ErrSchemaIDCollision) {
			t.Fatalf("error = %v, want ErrSchemaIDCollision", err)
		}
	})
	t.Run("identical normalized aliases", func(t *testing.T) {
		resolver := resolverFunc(func(_ context.Context, reference string) (ResolvedSchema, error) {
			doc := `{"properties":{},"type":"object","maxProperties":1.0}`
			if reference == "schema://contract/second" {
				doc = `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"https://schemas.example.test/model-config-v1.json","type":"object","properties":{},"maxProperties":1}`
			}
			return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(doc)}, nil
		})
		bundle, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: resolver})
		if err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		root := decodeObject(t, bundle.Schema)
		if definitions := objectAt(t, root, "$defs"); len(definitions) != 1 {
			t.Fatalf("$defs = %#v", definitions)
		}
		if bundle.Contracts[0].Definition != "config_0000" || bundle.Contracts[1].Definition != "config_0000" {
			t.Fatalf("contracts = %#v", bundle.Contracts)
		}
	})
}

func TestGenerationIsDeterministicAcrossInputOrders(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{
		{nodeID: "zeta", name: "test.Zeta", configRef: "schema://contract/zeta"},
		{nodeID: "alpha", name: "test.Alpha", configRef: "schema://contract/alpha"},
		{nodeID: "empty", name: "test.Empty"},
	})
	var calls atomic.Int32
	resolver := resolverFunc(func(_ context.Context, reference string) (ResolvedSchema, error) {
		id := "https://schemas.example.test/zeta.json"
		if reference == "schema://contract/alpha" {
			id = "https://schemas.example.test/alpha.json"
		}
		if calls.Add(1)%2 == 0 {
			return ResolvedSchema{ID: id, Document: json.RawMessage(`{"properties":{"value":{"type":"string"}},"type":"object","maxProperties":1.0}`)}, nil
		}
		return ResolvedSchema{ID: id, Document: json.RawMessage(`{"type":"object","maxProperties":1,"properties":{"value":{"type":"string"}}}`)}, nil
	})

	reversedCatalog := resolve.NewCatalog()
	for index := len(fixture.descriptors) - 1; index >= 0; index-- {
		if err := reversedCatalog.Register(fixture.descriptors[index]); err != nil {
			t.Fatal(err)
		}
	}
	reorderedGraph := fixture.graph
	reorderedGraph.Nodes = slices.Clone(reorderedGraph.Nodes)
	slices.Reverse(reorderedGraph.Nodes)
	if err := reorderedGraph.Validate(); err != nil {
		t.Fatalf("reordered graph should preserve fingerprint: %v", err)
	}

	first, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(context.Background(), reorderedGraph, reversedCatalog, Options{Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	third, err := Generate(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, third) {
		t.Fatalf("generation is not deterministic\nfirst:  %#v\nsecond: %#v\nthird:  %#v", first, second, third)
	}
	if first.Nodes[0].NodeID != "alpha" || first.Contracts[0].Reference != "schema://contract/alpha" {
		t.Fatalf("indexes are not sorted: nodes=%#v contracts=%#v", first.Nodes, first.Contracts)
	}
}

func TestGeneratorSnapshotsAndClonesMutableInputsAndOutputs(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{{
		nodeID: "configured", name: "test.Configured", configRef: configuredReference,
	}})
	raw := json.RawMessage(`{"type":"object","properties":{"model":{"type":"string"}}}`)
	resolver := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
		return ResolvedSchema{ID: configuredSchemaID, Document: raw}, nil
	})
	generator, err := New(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	baseline := generator.Bundle()

	fixture.graph.Nodes[0].ID = "mutated-input"
	fixture.descriptors[0].ConfigSchema = "mutated-descriptor"
	for index := range raw {
		raw[index] = ' '
	}
	first := generator.Bundle()
	first.Schema[0] = 'x'
	first.Nodes[0].NodeID = "mutated-output"
	first.Contracts[0].Reference = "mutated-contract"
	first.Contracts[0].NodeIDs[0] = "mutated-node-index"
	if len(first.Unresolved) != 0 {
		first.Unresolved[0] = "mutated-unresolved"
	}
	second := generator.Bundle()
	if !reflect.DeepEqual(second, baseline) {
		t.Fatalf("generator snapshot changed\nwant: %#v\n got: %#v", baseline, second)
	}
}

func TestGeneratorSupportsConcurrentReadersAndMutators(t *testing.T) {
	fixture := newFixture(t, []fixtureSpec{{
		nodeID: "configured", name: "test.Configured", configRef: configuredReference,
	}})
	resolver := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
		return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(`{"type":"object"}`)}, nil
	})
	generator, err := New(context.Background(), fixture.graph, fixture.catalog, Options{Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := generator.Bundle().Digest
	var wait sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				bundle := generator.Bundle()
				if bundle.Digest != wantDigest {
					t.Errorf("digest = %q, want %q", bundle.Digest, wantDigest)
					return
				}
				bundle.Schema[0] ^= 1
				bundle.Nodes[0].NodeID = "local"
				bundle.Contracts[0].NodeIDs[0] = "local"
			}
		}()
	}
	wait.Wait()
}

func TestGenerationBoundsAndCancellation(t *testing.T) {
	twoNodes := newFixture(t, []fixtureSpec{
		{nodeID: "first", name: "test.First", configRef: "schema://contract/first"},
		{nodeID: "second", name: "test.Second", configRef: "schema://contract/second"},
	})
	oneNode := newFixture(t, []fixtureSpec{{
		nodeID: "configured", name: "test.Configured", configRef: configuredReference,
	}})
	validResolver := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
		return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(`{"type":"object"}`)}, nil
	})
	tests := []struct {
		name    string
		fixture fixture
		options Options
	}{
		{name: "graph nodes", fixture: twoNodes, options: Options{Limits: Limits{MaxGraphNodes: 1}}},
		{name: "graph bytes", fixture: oneNode, options: Options{Limits: Limits{MaxGraphBytes: 1}}},
		{name: "descriptor bytes", fixture: oneNode, options: Options{Limits: Limits{MaxDescriptorBytes: 1}}},
		{name: "contracts", fixture: twoNodes, options: Options{Limits: Limits{MaxConfigContracts: 1}}},
		{name: "resolved bytes", fixture: oneNode, options: Options{
			Resolver: validResolver, Limits: Limits{MaxResolvedSchemaBytes: 1},
		}},
		{name: "schema depth", fixture: oneNode, options: Options{
			Resolver: resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
				return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)}, nil
			}),
			Limits: Limits{MaxSchemaDepth: 2},
		}},
		{name: "schema values", fixture: oneNode, options: Options{
			Resolver: validResolver, Limits: Limits{MaxSchemaValues: 2},
		}},
		{name: "generated bytes", fixture: oneNode, options: Options{
			Limits: Limits{MaxGeneratedSchemaBytes: 1},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Generate(context.Background(), test.fixture.graph, test.fixture.catalog, test.options)
			if !errors.Is(err, ErrLimitExceeded) {
				t.Fatalf("error = %v, want ErrLimitExceeded", err)
			}
		})
	}

	_, err := Generate(context.Background(), oneNode.graph, oneNode.catalog, Options{
		Limits: Limits{MaxGraphNodes: -1},
	})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("negative limit error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Generate(canceled, oneNode.graph, oneNode.catalog, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context error = %v", err)
	}

	during, cancelDuring := context.WithCancel(context.Background())
	cancelingResolver := resolverFunc(func(context.Context, string) (ResolvedSchema, error) {
		cancelDuring()
		return ResolvedSchema{ID: configuredSchemaID, Document: json.RawMessage(`{"type":"object"}`)}, nil
	})
	_, err = Generate(during, oneNode.graph, oneNode.catalog, Options{Resolver: cancelingResolver})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("during-resolution cancellation error = %v", err)
	}
}

func newFixture(t *testing.T, specs []fixtureSpec) fixture {
	t.Helper()
	catalog := resolve.NewCatalog()
	descriptors := make([]element.Descriptor, 0, len(specs))
	nodes := make([]ir.Node, 0, len(specs))
	for _, spec := range specs {
		descriptor := element.Descriptor{
			FormatVersion: element.DescriptorFormatVersion,
			Name:          spec.name,
			Revision:      1,
			Ports: []element.Port{{
				Name: "out", Direction: element.Output,
				Type: element.Event(element.Named("test.Payload")), Cardinality: element.One,
			}},
			ConfigSchema: spec.configRef,
			StateSchema:  spec.stateRef,
		}
		identity, err := descriptor.Identity()
		if err != nil {
			t.Fatalf("descriptor %s: %v", spec.name, err)
		}
		if err := catalog.Register(descriptor); err != nil {
			t.Fatalf("register %s: %v", spec.name, err)
		}
		descriptors = append(descriptors, descriptor.Clone())
		nodes = append(nodes, ir.Node{
			ID: spec.nodeID, Element: identity,
			Ports: []ir.Port{{
				Name: "out", Direction: element.Output,
				Type: element.Event(element.Named("test.Payload")), Cardinality: element.One,
			}},
			ConfigSchema: spec.configRef,
			StateSchema:  spec.stateRef,
		})
	}
	graph := freezeGraph(t, ir.Graph{
		FormatVersion: ir.FormatVersion,
		ID:            "test-graph",
		Revision:      1,
		Nodes:         nodes,
	})
	return fixture{graph: graph, catalog: catalog, descriptors: descriptors}
}

func freezeGraph(t *testing.T, graph ir.Graph) ir.Graph {
	t.Helper()
	frozen, err := ir.Freeze(graph)
	if err != nil {
		t.Fatalf("freeze graph: %v", err)
	}
	return frozen
}

func decodeObject(t *testing.T, source []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("JSON root = %T, want object", value)
	}
	return object
}

func objectAt(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	object, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v (%T), want object", key, parent[key], parent[key])
	}
	return object
}

func numberText(value any) string {
	if number, ok := value.(json.Number); ok {
		return number.String()
	}
	return fmt.Sprint(value)
}

func compileBundle(t *testing.T, source []byte) *jsonschema.Schema {
	t.Helper()
	root := decodeObject(t, source)
	id, ok := root["$id"].(string)
	if !ok {
		t.Fatalf("bundle $id = %#v", root["$id"])
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource(id, root); err != nil {
		t.Fatalf("AddResource() error = %v", err)
	}
	compiled, err := compiler.Compile(id)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	return compiled
}

func assertValid(t *testing.T, schema *jsonschema.Schema, value any) {
	t.Helper()
	if err := schema.Validate(value); err != nil {
		t.Fatalf("Validate(%#v) unexpected error = %v", value, err)
	}
}

func assertInvalid(t *testing.T, schema *jsonschema.Schema, value any) {
	t.Helper()
	if err := schema.Validate(value); err == nil {
		t.Fatalf("Validate(%#v) unexpectedly succeeded", value)
	}
}

func cloneJSONValue(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return decodeObject(t, encoded)
}
