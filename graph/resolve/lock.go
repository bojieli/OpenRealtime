package resolve

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const LockFormatVersion = 1

// Entry pins one symbolic reference to an immutable descriptor identity.
// Reference and Identity.Name differ only when a future alias or contract
// reference resolves to an implementation descriptor.
type Entry struct {
	Reference string           `json:"reference" yaml:"reference"`
	Identity  element.Identity `json:"identity" yaml:"identity"`
}

// Lock is the generated, reviewable resolution artifact consumed by normal
// builds. Entries are canonicalized when encoded.
type Lock struct {
	FormatVersion uint64  `json:"format_version" yaml:"format_version"`
	Entries       []Entry `json:"elements" yaml:"elements"`
}

// NewLock creates an empty lock at the current format version.
func NewLock() Lock { return Lock{FormatVersion: LockFormatVersion} }

// Validate checks identities and uniqueness without requiring canonical input
// ordering.
func (lock Lock) Validate() error {
	if lock.FormatVersion != LockFormatVersion {
		return fmt.Errorf("resolution lock format %d is unsupported; want %d",
			lock.FormatVersion, LockFormatVersion)
	}
	seen := make(map[string]struct{}, len(lock.Entries))
	for _, entry := range lock.Entries {
		if entry.Reference == "" {
			return errors.New("resolution lock contains an empty reference")
		}
		if _, duplicate := seen[entry.Reference]; duplicate {
			return fmt.Errorf("resolution lock repeats reference %q", entry.Reference)
		}
		seen[entry.Reference] = struct{}{}
		if err := element.ValidateIdentity(entry.Identity); err != nil {
			return fmt.Errorf("resolution lock reference %s: %w", entry.Reference, err)
		}
	}
	return nil
}

// Lookup returns the identity pinned for reference.
func (lock Lock) Lookup(reference string) (element.Identity, bool) {
	for _, entry := range lock.Entries {
		if entry.Reference == reference {
			return entry.Identity, true
		}
	}
	return element.Identity{}, false
}

// Canonical returns a validated, independently owned lock with sorted entries.
func (lock Lock) Canonical() (Lock, error) {
	if err := lock.Validate(); err != nil {
		return Lock{}, err
	}
	result := lock
	result.Entries = slices.Clone(lock.Entries)
	sort.Slice(result.Entries, func(left, right int) bool {
		return result.Entries[left].Reference < result.Entries[right].Reference
	})
	return result, nil
}

// Marshal returns deterministic, newline-terminated JSON suitable for a
// generated openrealtime.lock file.
func (lock Lock) Marshal() ([]byte, error) {
	canonical, err := lock.Canonical()
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode resolution lock: %w", err)
	}
	return append(payload, '\n'), nil
}

// ParseLock decodes strict JSON and rejects unknown fields and trailing data.
func ParseLock(source []byte) (Lock, error) {
	if err := strictjson.Validate(source); err != nil {
		return Lock{}, fmt.Errorf("decode resolution lock: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var lock Lock
	if err := decoder.Decode(&lock); err != nil {
		return Lock{}, fmt.Errorf("decode resolution lock: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Lock{}, errors.New("decode resolution lock: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return Lock{}, fmt.Errorf("decode resolution lock trailing data: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return Lock{}, err
	}
	return lock, nil
}

// Equal reports semantic equality independent of entry ordering.
func (lock Lock) Equal(other Lock) bool {
	left, leftErr := lock.Marshal()
	right, rightErr := other.Marshal()
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
