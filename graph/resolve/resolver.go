package resolve

import (
	"fmt"
	"slices"
	"sort"

	"github.com/bojieli/OpenRealtime/element"
)

// Mode controls whether resolution consumes an existing lock or deliberately
// updates it. Normal compilation must use Locked.
type Mode uint8

const (
	Locked Mode = iota
	Update
)

// Result contains descriptors keyed by the requested symbolic references and
// the exact lock covering those references.
type Result struct {
	Descriptors map[string]element.Descriptor
	Lock        Lock
}

// Resolve pins each unique reference. Update deliberately selects the latest
// registered revision and emits a new minimal lock; Locked verifies every
// existing digest and rejects missing entries.
func Resolve(catalog *Catalog, references []string, lock Lock, mode Mode) (Result, error) {
	if catalog == nil {
		return Result{}, fmt.Errorf("resolve element descriptors: nil catalog")
	}
	unique := slices.Clone(references)
	sort.Strings(unique)
	unique = slices.Compact(unique)
	if mode == Locked {
		if err := lock.Validate(); err != nil {
			return Result{}, err
		}
	} else if mode != Update {
		return Result{}, fmt.Errorf("resolve element descriptors: invalid mode %d", mode)
	}

	result := Result{
		Descriptors: make(map[string]element.Descriptor, len(unique)),
		Lock:        NewLock(),
	}
	for _, reference := range unique {
		if reference == "" {
			return Result{}, fmt.Errorf("resolve element descriptors: empty reference")
		}
		var descriptor element.Descriptor
		switch mode {
		case Update:
			var found bool
			descriptor, found = catalog.Latest(reference)
			if !found {
				return Result{}, fmt.Errorf("element %q is not registered", reference)
			}
		case Locked:
			identity, found := lock.Lookup(reference)
			if !found {
				return Result{}, fmt.Errorf("element %q is absent from the resolution lock; run an explicit graph update", reference)
			}
			if identity.Name != reference {
				return Result{}, fmt.Errorf("element %q is locked to descriptor %q; aliases are not supported by this resolver version",
					reference, identity.Name)
			}
			var exact bool
			descriptor, exact = catalog.Exact(identity)
			if !exact {
				if atRevision, foundAtRevision := catalog.Revision(identity.Name, identity.Revision); foundAtRevision {
					actual, digestErr := atRevision.Identity()
					if digestErr != nil {
						return Result{}, digestErr
					}
					return Result{}, fmt.Errorf("stale resolution lock for %s@%d: locked %s, catalog has %s; descriptor revisions are immutable",
						identity.Name, identity.Revision, identity.Digest, actual.Digest)
				}
				return Result{}, fmt.Errorf("locked element %s@%d (%s) is unavailable",
					identity.Name, identity.Revision, identity.Digest)
			}
		}
		identity, err := descriptor.Identity()
		if err != nil {
			return Result{}, err
		}
		result.Descriptors[reference] = descriptor
		result.Lock.Entries = append(result.Lock.Entries, Entry{Reference: reference, Identity: identity})
	}
	return result, nil
}
