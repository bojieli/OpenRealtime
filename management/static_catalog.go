package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/plugin"
)

type staticGraph struct {
	graph  []byte
	schema schema.Bundle
}

// Catalog adapts the immutable graph, element, plugin, and values-schema
// catalogs into one read-only management service. Registered JSON bytes are
// reparsed on each read so a caller can never mutate catalog state.
type Catalog struct {
	elements *resolve.Catalog
	plugins  *plugin.Catalog

	mu     sync.RWMutex
	graphs map[string]staticGraph
}

func NewCatalog(elements *resolve.Catalog, plugins *plugin.Catalog) (*Catalog, error) {
	if elements == nil || plugins == nil {
		return nil, fmt.Errorf("create management catalog: %w", ErrInvalid)
	}
	return &Catalog{elements: elements, plugins: plugins, graphs: make(map[string]staticGraph)}, nil
}

// RegisterGraph publishes an exact Graph IR and its separately generated
// values-schema bundle. A fingerprint can be registered only once, even if a
// second caller supplies equal bytes, making ownership mistakes visible.
func (catalog *Catalog) RegisterGraph(graph ir.Graph, values schema.Bundle) error {
	if catalog == nil {
		return fmt.Errorf("register management graph: %w", ErrInvalid)
	}
	graphBytes, err := graph.Marshal()
	if err != nil {
		return fmt.Errorf("register management graph: %w", err)
	}
	if err := validateValuesBundle(graph, values); err != nil {
		return fmt.Errorf("register management values schema: %w", err)
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, duplicate := catalog.graphs[graph.Fingerprint]; duplicate {
		return fmt.Errorf("register management graph %s: %w", graph.Fingerprint, ErrConflict)
	}
	catalog.graphs[graph.Fingerprint] = staticGraph{graph: graphBytes, schema: cloneSchemaBundle(values)}
	return nil
}

func validateValuesBundle(graph ir.Graph, bundle schema.Bundle) error {
	if len(bundle.Schema) == 0 || len(bundle.Schema) > 64<<20 {
		return fmt.Errorf("%w: schema bytes are absent or exceed the bound", ErrInvalid)
	}
	if err := strictjson.Validate(bundle.Schema); err != nil {
		return fmt.Errorf("%w: schema JSON: %v", ErrInvalid, err)
	}
	digest := sha256.Sum256(bundle.Schema)
	want := "sha256:" + hex.EncodeToString(digest[:])
	if bundle.Digest != want {
		return fmt.Errorf("%w: schema digest is %q, want %q", ErrInvalid, bundle.Digest, want)
	}
	graphNodes := make(map[string]element.Identity, len(graph.Nodes))
	for _, node := range graph.Nodes {
		graphNodes[node.ID] = node.Element
	}
	seen := make(map[string]struct{}, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		if _, duplicate := seen[node.NodeID]; duplicate {
			return fmt.Errorf("%w: schema repeats node %q", ErrInvalid, node.NodeID)
		}
		seen[node.NodeID] = struct{}{}
		if graphNodes[node.NodeID] != node.Element {
			return fmt.Errorf("%w: schema node %q differs from graph IR", ErrInvalid, node.NodeID)
		}
	}
	if len(seen) != len(graphNodes) {
		return fmt.Errorf("%w: schema node coverage differs from graph IR", ErrInvalid)
	}
	for _, unresolved := range bundle.Unresolved {
		if unresolved == "" || unresolved != strings.TrimSpace(unresolved) {
			return fmt.Errorf("%w: schema has a non-canonical unresolved reference", ErrInvalid)
		}
	}
	if bundle.Complete != (len(bundle.Unresolved) == 0) {
		return fmt.Errorf("%w: schema completeness disagrees with unresolved references", ErrInvalid)
	}
	return nil
}

func (catalog *Catalog) Graph(_ context.Context, fingerprint string) (ir.Graph, error) {
	if catalog == nil {
		return ir.Graph{}, ErrNotFound
	}
	catalog.mu.RLock()
	entry, found := catalog.graphs[fingerprint]
	payload := append([]byte(nil), entry.graph...)
	catalog.mu.RUnlock()
	if !found {
		return ir.Graph{}, ErrNotFound
	}
	graph, err := ir.Parse(payload)
	if err != nil {
		return ir.Graph{}, fmt.Errorf("%w: stored graph: %v", ErrConflict, err)
	}
	return graph, nil
}

func (catalog *Catalog) ElementDescriptor(
	_ context.Context, identity element.Identity,
) (element.Descriptor, error) {
	if catalog == nil || catalog.elements == nil {
		return element.Descriptor{}, ErrNotFound
	}
	descriptor, found := catalog.elements.Exact(identity)
	if !found {
		return element.Descriptor{}, ErrNotFound
	}
	return descriptor, nil
}

func (catalog *Catalog) PluginDescriptor(
	_ context.Context, identity plugin.Identity,
) (plugin.Descriptor, error) {
	if catalog == nil || catalog.plugins == nil {
		return plugin.Descriptor{}, ErrNotFound
	}
	descriptor, err := catalog.plugins.Resolve(identity)
	if err != nil {
		return plugin.Descriptor{}, ErrNotFound
	}
	return descriptor, nil
}

func (catalog *Catalog) ValuesSchema(_ context.Context, fingerprint string) (schema.Bundle, error) {
	if catalog == nil {
		return schema.Bundle{}, ErrNotFound
	}
	catalog.mu.RLock()
	entry, found := catalog.graphs[fingerprint]
	bundle := cloneSchemaBundle(entry.schema)
	catalog.mu.RUnlock()
	if !found {
		return schema.Bundle{}, ErrNotFound
	}
	return bundle, nil
}

func cloneSchemaBundle(source schema.Bundle) schema.Bundle {
	result := source
	result.Schema = slices.Clone(source.Schema)
	result.Nodes = slices.Clone(source.Nodes)
	result.Contracts = make([]schema.ContractResolution, len(source.Contracts))
	for index, contract := range source.Contracts {
		result.Contracts[index] = contract
		result.Contracts[index].NodeIDs = slices.Clone(contract.NodeIDs)
	}
	result.Unresolved = slices.Clone(source.Unresolved)
	return result
}
