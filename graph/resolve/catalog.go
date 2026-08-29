// Package resolve pins symbolic element references to immutable descriptors.
// Resolution is deliberately pure: it never loads code, opens a provider, or
// acquires any runtime resource.
package resolve

import (
	"fmt"
	"sort"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
)

// Catalog is an in-memory collection of immutable descriptor revisions. It is
// safe for concurrent readers and registrations during process startup.
type Catalog struct {
	mu      sync.RWMutex
	byName  map[string]map[uint64]element.Descriptor
	byExact map[element.Identity]element.Descriptor
}

// NewCatalog returns an empty descriptor catalog.
func NewCatalog() *Catalog {
	return &Catalog{
		byName:  make(map[string]map[uint64]element.Descriptor),
		byExact: make(map[element.Identity]element.Descriptor),
	}
}

// Register validates and stores an immutable descriptor. Re-registering the
// exact same descriptor is idempotent; changing content without incrementing
// Revision is rejected.
func (catalog *Catalog) Register(descriptor element.Descriptor) error {
	identity, err := descriptor.Identity()
	if err != nil {
		return err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if catalog.byName == nil {
		catalog.byName = make(map[string]map[uint64]element.Descriptor)
	}
	if catalog.byExact == nil {
		catalog.byExact = make(map[element.Identity]element.Descriptor)
	}
	revisions := catalog.byName[identity.Name]
	if revisions == nil {
		revisions = make(map[uint64]element.Descriptor)
		catalog.byName[identity.Name] = revisions
	}
	if existing, found := revisions[identity.Revision]; found {
		existingIdentity, identityErr := existing.Identity()
		if identityErr != nil {
			return fmt.Errorf("catalog contains invalid descriptor %s@%d: %w",
				identity.Name, identity.Revision, identityErr)
		}
		if existingIdentity.Digest != identity.Digest {
			return fmt.Errorf("element %s revision %d is immutable: catalog has %s, registration has %s",
				identity.Name, identity.Revision, existingIdentity.Digest, identity.Digest)
		}
		return nil
	}
	stored := descriptor.Clone()
	revisions[identity.Revision] = stored
	catalog.byExact[identity] = stored
	return nil
}

// Latest returns the highest registered revision for name.
func (catalog *Catalog) Latest(name string) (element.Descriptor, bool) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	revisions := catalog.byName[name]
	var (
		latestRevision uint64
		latest         element.Descriptor
	)
	for revision, descriptor := range revisions {
		if revision > latestRevision {
			latestRevision = revision
			latest = descriptor.Clone()
		}
	}
	return latest, latestRevision != 0
}

// Exact returns the descriptor whose complete identity matches identity.
func (catalog *Catalog) Exact(identity element.Identity) (element.Descriptor, bool) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	descriptor, found := catalog.byExact[identity]
	return descriptor.Clone(), found
}

// Revision returns a descriptor by symbolic name and revision, regardless of
// digest. It is used to produce a useful stale-lock diagnostic.
func (catalog *Catalog) Revision(name string, revision uint64) (element.Descriptor, bool) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	descriptor, found := catalog.byName[name][revision]
	return descriptor.Clone(), found
}

// Names returns all registered symbolic names in lexical order.
func (catalog *Catalog) Names() []string {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	names := make([]string, 0, len(catalog.byName))
	for name := range catalog.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
