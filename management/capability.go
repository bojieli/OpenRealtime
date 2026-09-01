package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	capabilityBytes  = 32
	capabilityPrefix = "mgmt_"
	// MaximumCapabilityTTL is the longest bearer lifetime accepted by the
	// in-memory authority. Callers should normally choose a much shorter,
	// session-scoped lifetime.
	MaximumCapabilityTTL = 24 * time.Hour
)

type Grant struct {
	Operation Operation `json:"operation"`
	Resource  string    `json:"resource"`
}

type Access struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Grants    []Grant   `json:"grants"`
}

type capabilityEntry struct {
	id      uint64
	expires time.Time
	grants  []Grant
}

// CapabilityRegistry is an in-memory narrow-capability authority. It retains
// only SHA-256 lookup keys; the random bearer is returned exactly once by Issue.
type CapabilityRegistry struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]capabilityEntry
	next    uint64
	now     func() time.Time
}

func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{entries: make(map[[sha256.Size]byte]capabilityEntry), now: time.Now}
}

func (registry *CapabilityRegistry) Issue(ttl time.Duration, grants []Grant) (Access, error) {
	access, _, err := registry.issue(ttl, grants)
	return access, err
}

// IssueScoped returns an owner-specific idempotent revoker. The closure holds
// only the capability lookup key and generation, never the plaintext bearer,
// so a session can rotate or dispose its authority without retaining the token
// it already disclosed to its client.
func (registry *CapabilityRegistry) IssueScoped(
	ttl time.Duration, grants []Grant,
) (Access, func(), error) {
	access, lease, err := registry.issue(ttl, grants)
	if err != nil {
		return Access{}, nil, err
	}
	var once sync.Once
	revoke := func() {
		once.Do(func() {
			registry.mu.Lock()
			if current, found := registry.entries[lease.key]; found && current.id == lease.id {
				delete(registry.entries, lease.key)
			}
			registry.mu.Unlock()
		})
	}
	return access, revoke, nil
}

type capabilityLease struct {
	key [sha256.Size]byte
	id  uint64
}

func (registry *CapabilityRegistry) issue(
	ttl time.Duration, grants []Grant,
) (Access, capabilityLease, error) {
	if registry == nil {
		return Access{}, capabilityLease{}, errors.New("issue management capability: nil registry")
	}
	if ttl <= 0 || ttl > MaximumCapabilityTTL {
		return Access{}, capabilityLease{}, fmt.Errorf(
			"issue management capability: TTL must be between 1ns and %s", MaximumCapabilityTTL,
		)
	}
	canonical, err := canonicalGrants(grants)
	if err != nil {
		return Access{}, capabilityLease{}, fmt.Errorf("issue management capability: %w", err)
	}
	now := registry.now().UTC()
	for attempt := 0; attempt < 4; attempt++ {
		raw := make([]byte, capabilityBytes)
		if _, err := rand.Read(raw); err != nil {
			return Access{}, capabilityLease{}, fmt.Errorf("issue management capability: %w", err)
		}
		token := capabilityPrefix + base64.RawURLEncoding.EncodeToString(raw)
		key := sha256.Sum256(raw)
		registry.mu.Lock()
		registry.purgeLocked(now)
		if _, duplicate := registry.entries[key]; !duplicate {
			registry.next++
			entry := capabilityEntry{
				id: registry.next, expires: now.Add(ttl), grants: append([]Grant(nil), canonical...),
			}
			registry.entries[key] = entry
			registry.mu.Unlock()
			return Access{
				Token: token, ExpiresAt: now.Add(ttl), Grants: append([]Grant(nil), canonical...),
			}, capabilityLease{key: key, id: entry.id}, nil
		}
		registry.mu.Unlock()
	}
	return Access{}, capabilityLease{}, errors.New("issue management capability: random collision limit exceeded")
}

func canonicalGrants(grants []Grant) ([]Grant, error) {
	if len(grants) == 0 || len(grants) > 256 {
		return nil, errors.New("one to 256 grants are required")
	}
	result := append([]Grant(nil), grants...)
	seen := make(map[string]struct{}, len(result))
	for _, grant := range result {
		if !validOperation(grant.Operation) {
			return nil, fmt.Errorf("unknown operation %q", grant.Operation)
		}
		if grant.Resource == "" || grant.Resource != strings.TrimSpace(grant.Resource) ||
			len(grant.Resource) > 1024 || strings.ContainsAny(grant.Resource, "\x00\r\n") {
			return nil, fmt.Errorf("operation %s has an invalid resource", grant.Operation)
		}
		key := string(grant.Operation) + "\x00" + grant.Resource
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("grant %s/%s is repeated", grant.Operation, grant.Resource)
		}
		seen[key] = struct{}{}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Operation != result[right].Operation {
			return result[left].Operation < result[right].Operation
		}
		return result[left].Resource < result[right].Resource
	})
	return result, nil
}

func validOperation(operation Operation) bool {
	switch operation {
	case ReadGraph, ReadDescriptor, ReadSchema, ReadSession, ReadTrace,
		AnalyzeDocument, RenameDocument, RemoveDocumentEdge, CreateDocumentEdge, CompileDocument, RenderGraph,
		ReadSource, CreateSource, UpdateSource,
		ApplyCandidate:
		return true
	default:
		return false
	}
}

func (registry *CapabilityRegistry) Authorize(_ context.Context, request AuthorizationRequest) error {
	if registry == nil || request.Capability == "" || !validOperation(request.Operation) ||
		request.Resource == "" || request.Resource != strings.TrimSpace(request.Resource) {
		return ErrUnauthorized
	}
	key, valid := capabilityKey(request.Capability)
	if !valid {
		return ErrUnauthorized
	}
	now := registry.now().UTC()
	registry.mu.Lock()
	registry.purgeLocked(now)
	entry, found := registry.entries[key]
	registry.mu.Unlock()
	if !found || !now.Before(entry.expires) {
		return ErrUnauthorized
	}
	for _, grant := range entry.grants {
		if grant.Operation == request.Operation &&
			(grant.Resource == "*" || grant.Resource == request.Resource) {
			return nil
		}
	}
	return ErrUnauthorized
}

func (registry *CapabilityRegistry) Revoke(token string) {
	key, valid := capabilityKey(token)
	if registry == nil || !valid {
		return
	}
	registry.mu.Lock()
	delete(registry.entries, key)
	registry.mu.Unlock()
}

func (registry *CapabilityRegistry) purgeLocked(now time.Time) {
	for key, entry := range registry.entries {
		if !now.Before(entry.expires) {
			delete(registry.entries, key)
		}
	}
}

func capabilityKey(token string) ([sha256.Size]byte, bool) {
	if !strings.HasPrefix(token, capabilityPrefix) {
		return [sha256.Size]byte{}, false
	}
	encoded := strings.TrimPrefix(token, capabilityPrefix)
	if len(encoded) != base64.RawURLEncoding.EncodedLen(capabilityBytes) {
		return [sha256.Size]byte{}, false
	}
	var raw [capabilityBytes]byte
	count, err := base64.RawURLEncoding.Decode(raw[:], []byte(encoded))
	if err != nil || count != capabilityBytes {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(raw[:]), true
}
