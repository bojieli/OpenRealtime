package plugin

import (
	"fmt"
	"sort"
	"sync"
)

type catalogEntry struct {
	descriptor Descriptor
	identity   Identity
}

// Catalog stores immutable plugin descriptor revisions. It never resolves an
// implementation binary or secret; those are deployment concerns.
type Catalog struct {
	mu      sync.RWMutex
	entries map[string]map[uint64]catalogEntry
}

func NewCatalog() *Catalog {
	return &Catalog{entries: make(map[string]map[uint64]catalogEntry)}
}

// Register validates and stores one immutable descriptor revision.
func (catalog *Catalog) Register(descriptor Descriptor) (Identity, error) {
	if catalog == nil {
		return Identity{}, fmt.Errorf("register plugin descriptor: nil catalog")
	}
	canonical, err := descriptor.Canonical()
	if err != nil {
		return Identity{}, err
	}
	identity, err := canonical.Identity()
	if err != nil {
		return Identity{}, err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if catalog.entries == nil {
		catalog.entries = make(map[string]map[uint64]catalogEntry)
	}
	revisions := catalog.entries[identity.Name]
	if revisions == nil {
		revisions = make(map[uint64]catalogEntry)
		catalog.entries[identity.Name] = revisions
	}
	if existing, found := revisions[identity.Revision]; found {
		if existing.identity.Digest != identity.Digest {
			return Identity{}, fmt.Errorf(
				"plugin %s revision %d is immutable: catalog has %s, registration has %s",
				identity.Name, identity.Revision, existing.identity.Digest, identity.Digest,
			)
		}
		return Identity{}, fmt.Errorf("plugin %s revision %d is already registered",
			identity.Name, identity.Revision)
	}
	revisions[identity.Revision] = catalogEntry{
		descriptor: canonical.Clone(), identity: identity,
	}
	return identity, nil
}

// Resolve returns the exact descriptor named by an immutable identity.
func (catalog *Catalog) Resolve(identity Identity) (Descriptor, error) {
	if catalog == nil {
		return Descriptor{}, fmt.Errorf("resolve plugin descriptor: nil catalog")
	}
	if err := ValidateIdentity(identity); err != nil {
		return Descriptor{}, err
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	entry, found := catalog.entries[identity.Name][identity.Revision]
	if !found {
		return Descriptor{}, fmt.Errorf("plugin %s revision %d is not registered",
			identity.Name, identity.Revision)
	}
	if entry.identity.Digest != identity.Digest {
		return Descriptor{}, fmt.Errorf("plugin %s revision %d digest is %s, lock requires %s",
			identity.Name, identity.Revision, entry.identity.Digest, identity.Digest)
	}
	return entry.descriptor.Clone(), nil
}

// Latest returns the greatest registered revision for a symbolic name. It is
// used only when deliberately generating a new lock; mounting consumes the
// exact lock and never calls Latest.
func (catalog *Catalog) Latest(name string) (Descriptor, Identity, error) {
	if catalog == nil {
		return Descriptor{}, Identity{}, fmt.Errorf("resolve latest plugin descriptor: nil catalog")
	}
	if !pluginNamePattern.MatchString(name) {
		return Descriptor{}, Identity{}, fmt.Errorf("invalid plugin name %q", name)
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	revisions := catalog.entries[name]
	if len(revisions) == 0 {
		return Descriptor{}, Identity{}, fmt.Errorf("plugin %s is not registered", name)
	}
	var revision uint64
	for candidate := range revisions {
		if candidate > revision {
			revision = candidate
		}
	}
	entry := revisions[revision]
	return entry.descriptor.Clone(), entry.identity, nil
}

// Identities returns a deterministic snapshot without exposing catalog-owned
// descriptor slices.
func (catalog *Catalog) Identities() []Identity {
	if catalog == nil {
		return nil
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	var result []Identity
	for _, revisions := range catalog.entries {
		for _, entry := range revisions {
			result = append(result, entry.identity)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Name != result[right].Name {
			return result[left].Name < result[right].Name
		}
		return result[left].Revision < result[right].Revision
	})
	return result
}
