package schema

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const Draft202012 = "https://json-schema.org/draft/2020-12/schema"

var (
	// ErrSchemaNotFound is returned by a Resolver when it has no document for
	// an opaque descriptor reference. It is non-fatal unless RequireResolved
	// is enabled.
	ErrSchemaNotFound = errors.New("config schema not found")

	ErrUnknownDescriptor          = errors.New("unknown exact element descriptor")
	ErrDescriptorContractMismatch = errors.New("graph IR and descriptor contract mismatch")
	ErrUnknownConfigContract      = errors.New("config schema contract is unresolved")
	ErrInvalidResolvedSchema      = errors.New("invalid resolved config schema")
	ErrInvalidSchemaID            = errors.New("invalid schema ID")
	ErrSchemaIDCollision          = errors.New("schema ID collision")
	ErrLimitExceeded              = errors.New("schema generation limit exceeded")
	ErrInvalidOptions             = errors.New("invalid schema generation options")
)

// Resolver supplies the actual JSON Schema behind an opaque descriptor
// ConfigSchema reference. Implementations should honor ctx. The generator
// never performs fallback I/O or external reference resolution.
type Resolver interface {
	ResolveConfigSchema(ctx context.Context, reference string) (ResolvedSchema, error)
}

// ResolvedSchema is one self-contained Draft 2020-12 schema resource. ID must
// be an absolute canonical URI without a fragment. The generator copies
// Document as soon as ResolveConfigSchema returns.
type ResolvedSchema struct {
	ID       string          `json:"id"`
	Document json.RawMessage `json:"document"`
}

// Options controls one immutable generation snapshot.
type Options struct {
	// SchemaID is the absolute ID of the generated values schema. An empty ID
	// derives a stable URN from the frozen graph fingerprint.
	SchemaID string
	Resolver Resolver
	// RequireResolved changes an honest unresolved contract report into an
	// error. It is useful for release/deployment gates.
	RequireResolved bool
	Limits          Limits
}

// Limits bounds attacker-controlled graph, descriptor, resolver, and output
// work. Zero fields use DefaultLimits; negative fields are invalid.
type Limits struct {
	MaxGraphNodes               int
	MaxGraphEdges               int
	MaxGraphBoundaries          int
	MaxGraphScopes              int
	MaxGraphPorts               int
	MaxGraphBytes               int
	MaxTypeDepth                int
	MaxDistinctDescriptors      int
	MaxDescriptorBytes          int
	MaxTotalDescriptorBytes     int
	MaxConfigContracts          int
	MaxResolvedSchemas          int
	MaxResolvedSchemaBytes      int
	MaxTotalResolvedSchemaBytes int
	MaxSchemaDepth              int
	MaxSchemaValues             int
	MaxStringBytes              int
	MaxGeneratedSchemaBytes     int
}

// DefaultLimits returns a fresh copy of the production defaults. There is no
// exported mutable default value.
func DefaultLimits() Limits {
	return Limits{
		MaxGraphNodes:               16_384,
		MaxGraphEdges:               65_536,
		MaxGraphBoundaries:          16_384,
		MaxGraphScopes:              16_384,
		MaxGraphPorts:               65_536,
		MaxGraphBytes:               64 << 20,
		MaxTypeDepth:                64,
		MaxDistinctDescriptors:      16_384,
		MaxDescriptorBytes:          1 << 20,
		MaxTotalDescriptorBytes:     32 << 20,
		MaxConfigContracts:          16_384,
		MaxResolvedSchemas:          16_384,
		MaxResolvedSchemaBytes:      2 << 20,
		MaxTotalResolvedSchemaBytes: 32 << 20,
		MaxSchemaDepth:              128,
		MaxSchemaValues:             200_000,
		MaxStringBytes:              64 << 10,
		MaxGeneratedSchemaBytes:     64 << 20,
	}
}

// ContractStatus describes how a node's configuration contract is modeled.
type ContractStatus string

const (
	ContractEmptyObject ContractStatus = "empty-object-only"
	ContractResolved    ContractStatus = "resolved"
	ContractUnresolved  ContractStatus = "unresolved"
)

// NodeContract is the immutable, deterministic node index represented by the
// generated values schema.
type NodeContract struct {
	NodeID          string           `json:"node_id"`
	Element         element.Identity `json:"element"`
	ConfigReference string           `json:"config_reference,omitempty"`
	Status          ContractStatus   `json:"status"`
	SchemaID        string           `json:"schema_id,omitempty"`
	Definition      string           `json:"definition,omitempty"`
}

// ContractResolution reports one distinct, non-empty descriptor
// ConfigSchema reference and every graph node that uses it.
type ContractResolution struct {
	Reference  string         `json:"reference"`
	Status     ContractStatus `json:"status"`
	SchemaID   string         `json:"schema_id,omitempty"`
	Definition string         `json:"definition,omitempty"`
	NodeIDs    []string       `json:"node_ids"`
}

// Bundle is an immutable generation result. Schema is deterministic,
// newline-terminated JSON; Digest is its sha256 content identity. Complete is
// false exactly when at least one opaque config reference was unresolved.
type Bundle struct {
	Schema     json.RawMessage      `json:"schema"`
	Digest     string               `json:"digest"`
	Complete   bool                 `json:"complete"`
	Nodes      []NodeContract       `json:"nodes"`
	Contracts  []ContractResolution `json:"contracts"`
	Unresolved []string             `json:"unresolved"`
}

// Generator owns one exact descriptor and schema snapshot. It is read-only
// after New and safe for concurrent Bundle calls.
type Generator struct {
	bundle Bundle
}

// New snapshots and validates graph, catalog, and resolver contracts, then
// builds the values schema without performing any I/O itself.
func New(ctx context.Context, graph ir.Graph, catalog *resolve.Catalog, options Options) (*Generator, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidOptions)
	}
	if catalog == nil {
		return nil, fmt.Errorf("%w: nil descriptor catalog", ErrInvalidOptions)
	}
	limits, err := options.Limits.normalized()
	if err != nil {
		return nil, err
	}
	if err := checkContext(ctx, "start schema generation"); err != nil {
		return nil, err
	}
	if err := validateGraphBounds(graph, limits); err != nil {
		return nil, err
	}
	if err := graph.Validate(); err != nil {
		return nil, fmt.Errorf("validate graph IR for schema generation: %w", err)
	}

	schemaID := options.SchemaID
	if schemaID == "" {
		schemaID = defaultSchemaID(graph.Fingerprint)
	}
	if err := validateSchemaID(schemaID, limits.MaxStringBytes); err != nil {
		return nil, fmt.Errorf("%w: generated values schema %q: %w", ErrInvalidSchemaID, schemaID, err)
	}

	nodes, err := snapshotNodes(ctx, graph, catalog, limits)
	if err != nil {
		return nil, err
	}
	bundle, err := generateBundle(ctx, graph.ID, schemaID, nodes, options, limits)
	if err != nil {
		return nil, err
	}
	return &Generator{bundle: cloneBundle(bundle)}, nil
}

// Generate is the one-shot form of New followed by Bundle.
func Generate(ctx context.Context, graph ir.Graph, catalog *resolve.Catalog, options Options) (Bundle, error) {
	generator, err := New(ctx, graph, catalog, options)
	if err != nil {
		return Bundle{}, err
	}
	return generator.Bundle(), nil
}

// Bundle returns a recursively independent result. Mutating it cannot change
// this generator or another caller's result.
func (generator *Generator) Bundle() Bundle {
	if generator == nil {
		return Bundle{}
	}
	return cloneBundle(generator.bundle)
}

type nodeSnapshot struct {
	id        string
	element   element.Identity
	configRef string
}

type resolvedDefinition struct {
	id        string
	document  map[string]any
	canonical []byte
	key       string
}

type contractWork struct {
	result     ContractResolution
	definition *resolvedDefinition
}

func snapshotNodes(
	ctx context.Context,
	graph ir.Graph,
	catalog *resolve.Catalog,
	limits Limits,
) ([]nodeSnapshot, error) {
	sortedNodes := slices.Clone(graph.Nodes)
	sort.Slice(sortedNodes, func(left, right int) bool { return sortedNodes[left].ID < sortedNodes[right].ID })
	descriptors := make(map[element.Identity]element.Descriptor)
	totalDescriptorBytes := 0
	result := make([]nodeSnapshot, 0, len(sortedNodes))
	for _, node := range sortedNodes {
		if err := checkContext(ctx, "snapshot element descriptors"); err != nil {
			return nil, err
		}
		descriptor, found := descriptors[node.Element]
		if !found {
			resolved, exact := catalog.Exact(node.Element)
			if !exact {
				if current, sameRevision := catalog.Revision(node.Element.Name, node.Element.Revision); sameRevision {
					currentIdentity, identityErr := current.Identity()
					if identityErr == nil {
						return nil, fmt.Errorf("%w: graph node %s pins %s@%d digest %s; catalog has digest %s",
							ErrUnknownDescriptor, node.ID, node.Element.Name, node.Element.Revision,
							node.Element.Digest, currentIdentity.Digest)
					}
				}
				return nil, fmt.Errorf("%w: graph node %s pins %s@%d (%s)",
					ErrUnknownDescriptor, node.ID, node.Element.Name, node.Element.Revision, node.Element.Digest)
			}
			canonical, canonicalErr := resolved.Canonical()
			if canonicalErr != nil {
				return nil, fmt.Errorf("snapshot descriptor for graph node %s: %w", node.ID, canonicalErr)
			}
			identity, identityErr := canonical.Identity()
			if identityErr != nil {
				return nil, fmt.Errorf("snapshot descriptor identity for graph node %s: %w", node.ID, identityErr)
			}
			if identity != node.Element {
				return nil, fmt.Errorf("%w: catalog exact lookup for node %s returned %v, want %v",
					ErrUnknownDescriptor, node.ID, identity, node.Element)
			}
			if len(descriptors) >= limits.MaxDistinctDescriptors {
				return nil, limitError("distinct descriptors", len(descriptors)+1, limits.MaxDistinctDescriptors)
			}
			encoded, encodeErr := json.Marshal(canonical)
			if encodeErr != nil {
				return nil, fmt.Errorf("measure descriptor %s: %w", canonical.Name, encodeErr)
			}
			if len(encoded) > limits.MaxDescriptorBytes {
				return nil, limitError("descriptor bytes", len(encoded), limits.MaxDescriptorBytes)
			}
			if totalDescriptorBytes > limits.MaxTotalDescriptorBytes-len(encoded) {
				return nil, limitError("total descriptor bytes", totalDescriptorBytes+len(encoded), limits.MaxTotalDescriptorBytes)
			}
			totalDescriptorBytes += len(encoded)
			descriptor = canonical
			descriptors[node.Element] = canonical
		}
		if err := verifyNodeContract(node, descriptor); err != nil {
			return nil, fmt.Errorf("%w: node %s: %v", ErrDescriptorContractMismatch, node.ID, err)
		}
		result = append(result, nodeSnapshot{id: node.ID, element: node.Element, configRef: descriptor.ConfigSchema})
	}
	return result, nil
}

func generateBundle(
	ctx context.Context,
	graphID string,
	schemaID string,
	nodes []nodeSnapshot,
	options Options,
	limits Limits,
) (Bundle, error) {
	nodesByReference := make(map[string][]string)
	for _, node := range nodes {
		if node.configRef != "" {
			nodesByReference[node.configRef] = append(nodesByReference[node.configRef], node.id)
		}
	}
	if len(nodesByReference) > limits.MaxConfigContracts {
		return Bundle{}, limitError("distinct config contracts", len(nodesByReference), limits.MaxConfigContracts)
	}
	references := make([]string, 0, len(nodesByReference))
	for reference := range nodesByReference {
		references = append(references, reference)
	}
	sort.Strings(references)

	works := make([]contractWork, 0, len(references))
	byID := make(map[string]*resolvedDefinition)
	resolvedCount := 0
	totalResolvedBytes := 0
	unresolved := make([]string, 0)
	for _, reference := range references {
		if err := checkContext(ctx, "resolve config schema contracts"); err != nil {
			return Bundle{}, err
		}
		work := contractWork{result: ContractResolution{
			Reference: reference,
			Status:    ContractUnresolved,
			NodeIDs:   slices.Clone(nodesByReference[reference]),
		}}
		if options.Resolver == nil {
			if options.RequireResolved {
				return Bundle{}, fmt.Errorf("%w: %q", ErrUnknownConfigContract, reference)
			}
			unresolved = append(unresolved, reference)
			works = append(works, work)
			continue
		}

		resolved, err := options.Resolver.ResolveConfigSchema(ctx, reference)
		if err != nil {
			if errors.Is(err, ErrSchemaNotFound) {
				if options.RequireResolved {
					return Bundle{}, fmt.Errorf("%w: %q: %v", ErrUnknownConfigContract, reference, err)
				}
				unresolved = append(unresolved, reference)
				works = append(works, work)
				continue
			}
			return Bundle{}, fmt.Errorf("resolve config schema %q: %w", reference, err)
		}
		if err := checkContext(ctx, "resolve config schema contracts"); err != nil {
			return Bundle{}, err
		}
		resolvedCount++
		if resolvedCount > limits.MaxResolvedSchemas {
			return Bundle{}, limitError("resolved schemas", resolvedCount, limits.MaxResolvedSchemas)
		}
		document := slices.Clone(resolved.Document)
		if len(document) > limits.MaxResolvedSchemaBytes {
			return Bundle{}, limitError("resolved schema bytes", len(document), limits.MaxResolvedSchemaBytes)
		}
		if totalResolvedBytes > limits.MaxTotalResolvedSchemaBytes-len(document) {
			return Bundle{}, limitError("total resolved schema bytes", totalResolvedBytes+len(document), limits.MaxTotalResolvedSchemaBytes)
		}
		totalResolvedBytes += len(document)
		definition, normalizeErr := normalizeResolvedSchema(resolved.ID, document, limits)
		if normalizeErr != nil {
			return Bundle{}, fmt.Errorf("%w: contract %q: %w", ErrInvalidResolvedSchema, reference, normalizeErr)
		}
		if err := checkContext(ctx, "validate config schema contracts"); err != nil {
			return Bundle{}, err
		}
		if definition.id == schemaID {
			return Bundle{}, fmt.Errorf("%w: resolved contract %q and generated values schema both claim %q",
				ErrSchemaIDCollision, reference, schemaID)
		}
		if previous, exists := byID[definition.id]; exists {
			if !bytes.Equal(previous.canonical, definition.canonical) {
				return Bundle{}, fmt.Errorf("%w: config contracts claim %q with different documents",
					ErrSchemaIDCollision, definition.id)
			}
			definition = previous
		} else {
			byID[definition.id] = definition
		}
		work.result.Status = ContractResolved
		work.result.SchemaID = definition.id
		work.definition = definition
		works = append(works, work)
	}

	definitionIDs := make([]string, 0, len(byID))
	for id := range byID {
		definitionIDs = append(definitionIDs, id)
	}
	sort.Strings(definitionIDs)
	for index, id := range definitionIDs {
		byID[id].key = fmt.Sprintf("config_%04d", index)
	}
	for index := range works {
		if works[index].definition != nil {
			works[index].result.Definition = works[index].definition.key
		}
	}

	contracts := make([]ContractResolution, len(works))
	contractByReference := make(map[string]ContractResolution, len(works))
	for index, work := range works {
		contracts[index] = cloneContractResolution(work.result)
		contractByReference[work.result.Reference] = work.result
	}
	nodeContracts := make([]NodeContract, 0, len(nodes))
	for _, node := range nodes {
		contract := NodeContract{
			NodeID: node.id, Element: node.element,
			ConfigReference: node.configRef, Status: ContractEmptyObject,
		}
		if node.configRef != "" {
			resolution := contractByReference[node.configRef]
			contract.Status = resolution.Status
			contract.SchemaID = resolution.SchemaID
			contract.Definition = resolution.Definition
		}
		nodeContracts = append(nodeContracts, contract)
	}

	complete := len(unresolved) == 0
	if err := checkContext(ctx, "encode generated values schema"); err != nil {
		return Bundle{}, err
	}
	root := buildSchema(graphID, schemaID, nodeContracts, unresolved, byID, complete)
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return Bundle{}, fmt.Errorf("encode generated values schema: %w", err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > limits.MaxGeneratedSchemaBytes {
		return Bundle{}, limitError("generated schema bytes", len(encoded), limits.MaxGeneratedSchemaBytes)
	}
	if err := compileGeneratedSchema(schemaID, root); err != nil {
		return Bundle{}, fmt.Errorf("compile generated values schema: %w", err)
	}
	if err := checkContext(ctx, "compile generated values schema"); err != nil {
		return Bundle{}, err
	}
	digest := sha256.Sum256(encoded)
	return Bundle{
		Schema: encoded, Digest: "sha256:" + hex.EncodeToString(digest[:]), Complete: complete,
		Nodes: nodeContracts, Contracts: contracts, Unresolved: unresolved,
	}, nil
}

func buildSchema(
	graphID string,
	schemaID string,
	nodes []NodeContract,
	unresolved []string,
	definitions map[string]*resolvedDefinition,
	complete bool,
) map[string]any {
	nodeProperties := make(map[string]any, len(nodes))
	for _, node := range nodes {
		switch node.Status {
		case ContractEmptyObject:
			nodeProperties[node.NodeID] = map[string]any{
				"type": "object", "maxProperties": 0, "additionalProperties": false,
				"x-openrealtime-config-status": string(ContractEmptyObject),
			}
		case ContractResolved:
			nodeProperties[node.NodeID] = map[string]any{
				"type": "object", "$ref": "#/$defs/" + node.Definition,
				"x-openrealtime-config-reference": node.ConfigReference,
				"x-openrealtime-config-status":    string(ContractResolved),
			}
		case ContractUnresolved:
			nodeProperties[node.NodeID] = map[string]any{
				"type":                            "object",
				"x-openrealtime-config-reference": node.ConfigReference,
				"x-openrealtime-config-status":    string(ContractUnresolved),
			}
		}
	}
	properties := map[string]any{
		"apiVersion": map[string]any{"type": "string", "const": graphvalues.APIVersion},
		"graph":      map[string]any{"type": "string", "const": graphID},
		"nodes": map[string]any{
			"type": "object", "properties": nodeProperties,
			"additionalProperties": false, "default": map[string]any{},
		},
	}
	unresolvedValues := make([]any, len(unresolved))
	for index, reference := range unresolved {
		unresolvedValues[index] = reference
	}
	root := map[string]any{
		"$schema": Draft202012,
		"$id":     schemaID,
		"title":   "OpenRealtime values for graph " + graphID,
		"type":    "object",
		"required": []any{
			"apiVersion", "graph",
		},
		"properties":                          properties,
		"additionalProperties":                false,
		"x-openrealtime-complete":             complete,
		"x-openrealtime-unresolved-contracts": unresolvedValues,
	}
	if len(definitions) != 0 {
		defs := make(map[string]any, len(definitions))
		for _, definition := range definitions {
			defs[definition.key] = definition.document
		}
		root["$defs"] = defs
	}
	return root
}

func normalizeResolvedSchema(id string, source []byte, limits Limits) (*resolvedDefinition, error) {
	if err := validateSchemaID(id, limits.MaxStringBytes); err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrInvalidSchemaID, id, err)
	}
	if len(source) == 0 {
		return nil, errors.New("schema document is empty")
	}
	if err := preflightSchemaJSON(source, limits); err != nil {
		return nil, err
	}
	if err := strictjson.Validate(source); err != nil {
		return nil, fmt.Errorf("strict JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode schema: %w", err)
	}
	root, object := decoded.(map[string]any)
	if !object {
		return nil, errors.New("root schema must be a JSON object")
	}
	if declared, exists := root["$schema"]; exists {
		value, ok := declared.(string)
		if !ok || value != Draft202012 {
			return nil, fmt.Errorf("$schema must be %q", Draft202012)
		}
	} else {
		root["$schema"] = Draft202012
	}
	if declared, exists := root["$id"]; exists {
		value, ok := declared.(string)
		if !ok {
			return nil, errors.New("root $id must be a string")
		}
		if value != id {
			return nil, fmt.Errorf("root $id %q does not match resolver ID %q", value, id)
		}
	} else {
		root["$id"] = id
	}
	if err := inspectSchemaResource(root, true, "$", limits.MaxSchemaDepth); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("canonicalize schema: %w", err)
	}
	canonical, _, err := graphvalues.Digest(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize schema numbers: %w", err)
	}
	decoder = json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode canonical schema: %w", err)
	}
	compiled, err := compileSchema(id, root)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	for _, primitive := range []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "boolean", value: true},
		{name: "string", value: "openrealtime"},
		{name: "number", value: json.Number("1")},
		{name: "array", value: []any{}},
	} {
		if err := compiled.Validate(primitive.value); err == nil {
			return nil, fmt.Errorf("schema accepts representative non-object %s configuration", primitive.name)
		}
	}
	return &resolvedDefinition{id: id, document: root, canonical: canonical}, nil
}

func inspectSchemaResource(value any, root bool, path string, remainingDepth int) error {
	if remainingDepth <= 0 {
		return fmt.Errorf("schema subschema depth exceeds limit at %s", path)
	}
	if _, boolean := value.(bool); boolean {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("schema at %s must be an object or boolean", path)
	}
	if !root {
		if _, nestedID := object["$id"]; nestedID {
			return fmt.Errorf("nested $id resource is not supported at %s", path)
		}
		if _, nestedDialect := object["$schema"]; nestedDialect {
			return fmt.Errorf("nested $schema dialect is not supported at %s", path)
		}
	}
	for _, keyword := range []string{"$ref", "$dynamicRef"} {
		reference, exists := object[keyword]
		if !exists {
			continue
		}
		text, stringValue := reference.(string)
		if !stringValue {
			return fmt.Errorf("%s at %s must be a string", keyword, path)
		}
		if text != "" && !strings.HasPrefix(text, "#") {
			return fmt.Errorf("non-local %s %q at %s is not self-contained", keyword, text, path)
		}
	}

	walk := func(keyword string, child any, childPath string) error {
		return inspectSchemaResource(child, false, childPath+"/"+keyword, remainingDepth-1)
	}
	for _, keyword := range []string{
		"not", "if", "then", "else", "items", "contains", "propertyNames",
		"additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contentSchema",
	} {
		if child, exists := object[keyword]; exists {
			if err := walk(keyword, child, path); err != nil {
				return err
			}
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		child, exists := object[keyword]
		if !exists {
			continue
		}
		items, array := child.([]any)
		if !array {
			return fmt.Errorf("%s at %s must be an array", keyword, path)
		}
		for index, item := range items {
			if err := inspectSchemaResource(item, false, fmt.Sprintf("%s/%s/%d", path, keyword, index), remainingDepth-1); err != nil {
				return err
			}
		}
	}
	for _, keyword := range []string{"$defs", "properties", "patternProperties", "dependentSchemas"} {
		child, exists := object[keyword]
		if !exists {
			continue
		}
		entries, objectValue := child.(map[string]any)
		if !objectValue {
			return fmt.Errorf("%s at %s must be an object", keyword, path)
		}
		keys := make([]string, 0, len(entries))
		for key := range entries {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := inspectSchemaResource(entries[key], false, path+"/"+keyword+"/"+key, remainingDepth-1); err != nil {
				return err
			}
		}
	}
	return nil
}

func preflightSchemaJSON(source []byte, limits Limits) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	depth := 0
	values := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode schema JSON: %w", err)
		}
		values++
		if values > limits.MaxSchemaValues {
			return limitError("schema JSON values", values, limits.MaxSchemaValues)
		}
		switch typed := token.(type) {
		case json.Delim:
			switch typed {
			case '{', '[':
				depth++
				if depth > limits.MaxSchemaDepth {
					return limitError("schema JSON depth", depth, limits.MaxSchemaDepth)
				}
			case '}', ']':
				depth--
			}
		case string:
			if len(typed) > limits.MaxStringBytes {
				return limitError("schema string bytes", len(typed), limits.MaxStringBytes)
			}
		}
	}
}

func compileSchema(id string, document any) (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(rejectLoader{})
	if err := compiler.AddResource(id, document); err != nil {
		return nil, err
	}
	return compiler.Compile(id)
}

func compileGeneratedSchema(id string, document map[string]any) error {
	_, err := compileSchema(id, document)
	return err
}

type rejectLoader struct{}

func (rejectLoader) Load(resource string) (any, error) {
	return nil, fmt.Errorf("external schema resolution is disabled for %s", resource)
}

func verifyNodeContract(node ir.Node, descriptor element.Descriptor) error {
	if node.StateSchema != descriptor.StateSchema || node.ConfigSchema != descriptor.ConfigSchema {
		return errors.New("state/config schema differs from the exact descriptor")
	}
	if !reflect.DeepEqual(node.StateTransfer, descriptor.StateTransfer) {
		return errors.New("state-transfer capabilities differ from the exact descriptor")
	}
	actualReaction := cloneSortedReaction(node.Reaction)
	expectedReaction := cloneSortedReaction(descriptor.Reaction)
	if !reflect.DeepEqual(actualReaction, expectedReaction) {
		return errors.New("reaction contract differs from the exact descriptor")
	}
	actualDependencies := slices.Clone(node.Dependencies)
	sort.Slice(actualDependencies, func(left, right int) bool { return actualDependencies[left].Name < actualDependencies[right].Name })
	if !reflect.DeepEqual(actualDependencies, descriptor.Dependencies) {
		return errors.New("dependency contract differs from the exact descriptor")
	}
	actualEffects := slices.Clone(node.Effects)
	sort.Slice(actualEffects, func(left, right int) bool { return actualEffects[left].Name < actualEffects[right].Name })
	if !reflect.DeepEqual(actualEffects, descriptor.Effects) {
		return errors.New("effect contract differs from the exact descriptor")
	}
	if len(node.Ports) != len(descriptor.Ports) {
		return fmt.Errorf("graph has %d ports; descriptor defines %d", len(node.Ports), len(descriptor.Ports))
	}
	actualPorts := slices.Clone(node.Ports)
	sort.Slice(actualPorts, func(left, right int) bool { return actualPorts[left].Name < actualPorts[right].Name })
	bindings := make(map[string]element.Type)
	for index, expected := range descriptor.Ports {
		actual := actualPorts[index]
		if actual.Name != expected.Name || actual.Direction != expected.Direction ||
			actual.Cardinality != expected.Cardinality || actual.Required != expected.Required ||
			actual.MinConnections != expected.MinConnections || actual.LossAllowed != expected.LossAllowed ||
			actual.DefaultDepth != expected.DefaultDepth {
			return fmt.Errorf("port %s metadata differs from the exact descriptor", expected.Name)
		}
		if err := matchResolvedType(expected.Type, actual.Type, bindings); err != nil {
			return fmt.Errorf("port %s: %w", expected.Name, err)
		}
	}
	return nil
}

func cloneSortedReaction(reaction element.Reaction) element.Reaction {
	result := reaction
	result.Triggers = slices.Clone(reaction.Triggers)
	result.SampledState = slices.Clone(reaction.SampledState)
	result.Interrupts = slices.Clone(reaction.Interrupts)
	result.Outcomes = slices.Clone(reaction.Outcomes)
	sort.Strings(result.Triggers)
	sort.Strings(result.SampledState)
	sort.Strings(result.Interrupts)
	sort.Strings(result.Outcomes)
	return result
}

func matchResolvedType(pattern, concrete element.Type, bindings map[string]element.Type) error {
	if concrete.ContainsVariable() {
		return fmt.Errorf("resolved type %s still contains a generic variable", concrete.String())
	}
	if pattern.Variable != "" {
		if previous, found := bindings[pattern.Variable]; found {
			if !previous.Equal(concrete) {
				return fmt.Errorf("generic $%s resolves to both %s and %s",
					pattern.Variable, previous.String(), concrete.String())
			}
			return nil
		}
		bindings[pattern.Variable] = concrete.Clone()
		return nil
	}
	if pattern.Name != concrete.Name || len(pattern.Arguments) != len(concrete.Arguments) {
		return fmt.Errorf("resolved type %s does not instantiate descriptor type %s", concrete.String(), pattern.String())
	}
	for index := range pattern.Arguments {
		if err := matchResolvedType(pattern.Arguments[index], concrete.Arguments[index], bindings); err != nil {
			return err
		}
	}
	return nil
}

func validateGraphBounds(graph ir.Graph, limits Limits) error {
	for _, value := range []struct {
		name  string
		value int
		limit int
	}{
		{name: "graph nodes", value: len(graph.Nodes), limit: limits.MaxGraphNodes},
		{name: "graph edges", value: len(graph.Edges), limit: limits.MaxGraphEdges},
		{name: "graph boundaries", value: len(graph.Boundaries), limit: limits.MaxGraphBoundaries},
		{name: "graph scopes", value: len(graph.Scopes), limit: limits.MaxGraphScopes},
	} {
		if value.value > value.limit {
			return limitError(value.name, value.value, value.limit)
		}
	}
	if err := checkString("graph ID", graph.ID, limits.MaxStringBytes); err != nil {
		return err
	}
	ports := 0
	for _, node := range graph.Nodes {
		ports += len(node.Ports)
		if ports > limits.MaxGraphPorts {
			return limitError("graph ports", ports, limits.MaxGraphPorts)
		}
		for _, value := range []struct{ name, text string }{
			{name: "node ID", text: node.ID},
			{name: "element name", text: node.Element.Name},
			{name: "config schema reference", text: node.ConfigSchema},
		} {
			if err := checkString(value.name, value.text, limits.MaxStringBytes); err != nil {
				return err
			}
		}
		for _, port := range node.Ports {
			if err := checkTypeDepth(port.Type, limits.MaxTypeDepth); err != nil {
				return fmt.Errorf("graph node %s port %s: %w", node.ID, port.Name, err)
			}
		}
	}
	for _, edge := range graph.Edges {
		if err := checkTypeDepth(edge.Type, limits.MaxTypeDepth); err != nil {
			return fmt.Errorf("graph edge %s: %w", edge.ID, err)
		}
	}
	for _, boundary := range graph.Boundaries {
		if err := checkTypeDepth(boundary.Type, limits.MaxTypeDepth); err != nil {
			return fmt.Errorf("graph boundary %s: %w", boundary.Name, err)
		}
	}
	for _, scope := range graph.Scopes {
		for _, boundary := range scope.Boundaries {
			if err := checkTypeDepth(boundary.Type, limits.MaxTypeDepth); err != nil {
				return fmt.Errorf("graph scope %s boundary %s: %w", scope.ID, boundary.Name, err)
			}
		}
	}
	writer := &boundedWriter{limit: limits.MaxGraphBytes}
	if err := json.NewEncoder(writer).Encode(graph); err != nil {
		if errors.Is(err, ErrLimitExceeded) {
			return err
		}
		return fmt.Errorf("measure graph IR: %w", err)
	}
	return nil
}

func checkTypeDepth(root element.Type, limit int) error {
	type entry struct {
		value element.Type
		depth int
	}
	stack := []entry{{value: root, depth: 1}}
	for len(stack) != 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if current.depth > limit {
			return limitError("graph type depth", current.depth, limit)
		}
		for _, argument := range current.value.Arguments {
			stack = append(stack, entry{value: argument, depth: current.depth + 1})
		}
	}
	return nil
}

type boundedWriter struct {
	written int
	limit   int
}

func (writer *boundedWriter) Write(value []byte) (int, error) {
	if len(value) > writer.limit-writer.written {
		return 0, limitError("graph bytes", writer.written+len(value), writer.limit)
	}
	writer.written += len(value)
	return len(value), nil
}

func validateSchemaID(id string, maxBytes int) error {
	if err := checkString("schema ID", id, maxBytes); err != nil {
		return err
	}
	if id == "" || strings.TrimSpace(id) != id {
		return errors.New("must be non-empty and have no surrounding whitespace")
	}
	if strings.Contains(id, "#") {
		return errors.New("must not contain a fragment")
	}
	parsed, err := url.Parse(id)
	if err != nil {
		return err
	}
	if !parsed.IsAbs() || parsed.Scheme == "" {
		return errors.New("must be an absolute URI")
	}
	if parsed.Scheme != strings.ToLower(parsed.Scheme) {
		return errors.New("URI scheme must be lowercase")
	}
	if parsed.Hostname() != strings.ToLower(parsed.Hostname()) {
		return errors.New("URI hostname must be lowercase")
	}
	if parsed.Opaque == "" && parsed.Host == "" && parsed.Path == "" {
		return errors.New("absolute URI has no resource component")
	}
	if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == "" {
		return errors.New("HTTP schema ID must contain a host")
	}
	if parsed.String() != id {
		return fmt.Errorf("URI is not in canonical parsed form; use %q", parsed.String())
	}
	for index := 0; index < len(id); index++ {
		if id[index] != '%' {
			continue
		}
		if index+2 >= len(id) || !isUpperHex(id[index+1]) || !isUpperHex(id[index+2]) {
			return errors.New("percent escapes must use two uppercase hexadecimal digits")
		}
		index += 2
	}
	return nil
}

func isUpperHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'A' && value <= 'F'
}

func defaultSchemaID(fingerprint string) string {
	return "urn:openrealtime:values-schema:" + strings.TrimPrefix(fingerprint, "sha256:")
}

func checkString(name, value string, limit int) error {
	if len(value) > limit {
		return limitError(name+" bytes", len(value), limit)
	}
	return nil
}

func checkContext(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

func limitError(name string, value, limit int) error {
	return fmt.Errorf("%w: %s is %d, maximum is %d", ErrLimitExceeded, name, value, limit)
}

func (limits Limits) normalized() (Limits, error) {
	defaults := DefaultLimits()
	fields := []struct {
		name            string
		value, fallback *int
	}{
		{name: "MaxGraphNodes", value: &limits.MaxGraphNodes, fallback: &defaults.MaxGraphNodes},
		{name: "MaxGraphEdges", value: &limits.MaxGraphEdges, fallback: &defaults.MaxGraphEdges},
		{name: "MaxGraphBoundaries", value: &limits.MaxGraphBoundaries, fallback: &defaults.MaxGraphBoundaries},
		{name: "MaxGraphScopes", value: &limits.MaxGraphScopes, fallback: &defaults.MaxGraphScopes},
		{name: "MaxGraphPorts", value: &limits.MaxGraphPorts, fallback: &defaults.MaxGraphPorts},
		{name: "MaxGraphBytes", value: &limits.MaxGraphBytes, fallback: &defaults.MaxGraphBytes},
		{name: "MaxTypeDepth", value: &limits.MaxTypeDepth, fallback: &defaults.MaxTypeDepth},
		{name: "MaxDistinctDescriptors", value: &limits.MaxDistinctDescriptors, fallback: &defaults.MaxDistinctDescriptors},
		{name: "MaxDescriptorBytes", value: &limits.MaxDescriptorBytes, fallback: &defaults.MaxDescriptorBytes},
		{name: "MaxTotalDescriptorBytes", value: &limits.MaxTotalDescriptorBytes, fallback: &defaults.MaxTotalDescriptorBytes},
		{name: "MaxConfigContracts", value: &limits.MaxConfigContracts, fallback: &defaults.MaxConfigContracts},
		{name: "MaxResolvedSchemas", value: &limits.MaxResolvedSchemas, fallback: &defaults.MaxResolvedSchemas},
		{name: "MaxResolvedSchemaBytes", value: &limits.MaxResolvedSchemaBytes, fallback: &defaults.MaxResolvedSchemaBytes},
		{name: "MaxTotalResolvedSchemaBytes", value: &limits.MaxTotalResolvedSchemaBytes, fallback: &defaults.MaxTotalResolvedSchemaBytes},
		{name: "MaxSchemaDepth", value: &limits.MaxSchemaDepth, fallback: &defaults.MaxSchemaDepth},
		{name: "MaxSchemaValues", value: &limits.MaxSchemaValues, fallback: &defaults.MaxSchemaValues},
		{name: "MaxStringBytes", value: &limits.MaxStringBytes, fallback: &defaults.MaxStringBytes},
		{name: "MaxGeneratedSchemaBytes", value: &limits.MaxGeneratedSchemaBytes, fallback: &defaults.MaxGeneratedSchemaBytes},
	}
	for _, field := range fields {
		if *field.value < 0 {
			return Limits{}, fmt.Errorf("%w: %s cannot be negative", ErrInvalidOptions, field.name)
		}
		if *field.value == 0 {
			*field.value = *field.fallback
		}
	}
	return limits, nil
}

func cloneBundle(source Bundle) Bundle {
	result := source
	result.Schema = slices.Clone(source.Schema)
	result.Nodes = slices.Clone(source.Nodes)
	result.Contracts = make([]ContractResolution, len(source.Contracts))
	for index, contract := range source.Contracts {
		result.Contracts[index] = cloneContractResolution(contract)
	}
	result.Unresolved = slices.Clone(source.Unresolved)
	return result
}

func cloneContractResolution(source ContractResolution) ContractResolution {
	result := source
	result.NodeIDs = slices.Clone(source.NodeIDs)
	return result
}
