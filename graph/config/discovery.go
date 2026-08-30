package config

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
)

// Discovery is a metadata-only preflight boundary. Implementations must not
// dial providers, open devices, construct factories, or resolve secret values.
// Runtime startup performs a second live handshake against the identities
// frozen here.
type Discovery interface {
	ResolveImplementation(context.Context, ImplementationRequest) (ImplementationResolution, error)
	ResolveDependency(context.Context, string) (DependencyResolution, error)
}

type ImplementationRequest struct {
	NodeID         string
	Reference      string
	Contract       element.Identity
	Placement      string
	Transport      string
	ResourceKeys   []string
	SecretSlotKeys []string
}

// ImplementationResolution proves the registered artifact and exact element
// contract behind one deployment reference. Capabilities are declared or
// registration evidence, never mislabeled as live negotiation. Nil means the
// factory will select and attest its complete capability set during the live
// readiness handshake; a non-nil slice is an exact registered set.
type ImplementationResolution struct {
	Reference    string                       `json:"reference"`
	Contract     element.Identity             `json:"contract"`
	Artifact     inspect.ArtifactIdentity     `json:"artifact"`
	Evidence     inspect.ResolutionEvidence   `json:"evidence"`
	Placements   []string                     `json:"placements,omitempty"`
	Transports   []string                     `json:"transports,omitempty"`
	ResourceKeys []string                     `json:"resource_keys,omitempty"`
	SecretSlots  []string                     `json:"secret_slots,omitempty"`
	Capabilities []inspect.CapabilityIdentity `json:"capabilities,omitempty"`
}

// DependencyScope fixes the lifetime at which a dependency service is
// selected. Process-scoped services are sealed during resource-free
// preparation; mount-scoped services are supplied independently to each
// prepared-plan mount. Scope is part of the immutable resolution identity.
type DependencyScope string

const (
	DependencyScopeProcess DependencyScope = "process"
	DependencyScopeMount   DependencyScope = "mount"
)

func (scope DependencyScope) Validate() error {
	switch scope {
	case DependencyScopeProcess, DependencyScopeMount:
		return nil
	default:
		return fmt.Errorf("dependency scope %q is invalid", scope)
	}
}

type DependencyResolution struct {
	Name     string                   `json:"name"`
	Artifact inspect.ArtifactIdentity `json:"artifact"`
	Scope    DependencyScope          `json:"scope"`
}

// DiscoverySnapshot is a deterministic, independent catalog view suitable for
// management-plane discovery. It contains no factory handles or secret data.
type DiscoverySnapshot struct {
	Implementations []ImplementationResolution `json:"implementations"`
	Dependencies    []DependencyResolution     `json:"dependencies"`
}

// StaticDiscovery is a concurrency-safe immutable-revision catalog. Exact
// idempotent registration is allowed; changing metadata behind a reference is
// rejected.
type StaticDiscovery struct {
	mu              sync.RWMutex
	implementations map[string]ImplementationResolution
	dependencies    map[string]DependencyResolution
}

func NewStaticDiscovery() *StaticDiscovery {
	return &StaticDiscovery{
		implementations: make(map[string]ImplementationResolution),
		dependencies:    make(map[string]DependencyResolution),
	}
}

func (catalog *StaticDiscovery) RegisterImplementation(resolution ImplementationResolution) error {
	if catalog == nil {
		return errors.New("register implementation discovery: nil catalog")
	}
	canonical, err := canonicalImplementation(resolution)
	if err != nil {
		return fmt.Errorf("register implementation discovery: %w", err)
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if catalog.implementations == nil {
		catalog.implementations = make(map[string]ImplementationResolution)
	}
	if previous, found := catalog.implementations[canonical.Reference]; found {
		if !equalImplementation(previous, canonical) {
			return fmt.Errorf("implementation discovery reference %q is immutable", canonical.Reference)
		}
		return nil
	}
	catalog.implementations[canonical.Reference] = canonical
	return nil
}

func (catalog *StaticDiscovery) RegisterDependency(resolution DependencyResolution) error {
	if catalog == nil {
		return errors.New("register dependency discovery: nil catalog")
	}
	canonical, err := canonicalDependency(resolution)
	if err != nil {
		return fmt.Errorf("register dependency discovery: %w", err)
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if catalog.dependencies == nil {
		catalog.dependencies = make(map[string]DependencyResolution)
	}
	if previous, found := catalog.dependencies[canonical.Name]; found {
		if previous != canonical {
			return fmt.Errorf("dependency discovery name %q is immutable", canonical.Name)
		}
		return nil
	}
	catalog.dependencies[canonical.Name] = canonical
	return nil
}

func (catalog *StaticDiscovery) ResolveImplementation(
	ctx context.Context, request ImplementationRequest,
) (ImplementationResolution, error) {
	if ctx == nil {
		return ImplementationResolution{}, errors.New("resolve implementation discovery: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return ImplementationResolution{}, err
	}
	if catalog == nil {
		return ImplementationResolution{}, errors.New("resolve implementation discovery: nil catalog")
	}
	catalog.mu.RLock()
	resolution, found := catalog.implementations[request.Reference]
	catalog.mu.RUnlock()
	if !found {
		return ImplementationResolution{}, fmt.Errorf("implementation %q is absent from discovery", request.Reference)
	}
	return cloneImplementation(resolution), nil
}

func (catalog *StaticDiscovery) ResolveDependency(ctx context.Context, name string) (DependencyResolution, error) {
	if ctx == nil {
		return DependencyResolution{}, errors.New("resolve dependency discovery: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return DependencyResolution{}, err
	}
	if catalog == nil {
		return DependencyResolution{}, errors.New("resolve dependency discovery: nil catalog")
	}
	catalog.mu.RLock()
	resolution, found := catalog.dependencies[name]
	catalog.mu.RUnlock()
	if !found {
		return DependencyResolution{}, fmt.Errorf("dependency %q is absent from discovery", name)
	}
	return resolution, nil
}

func (catalog *StaticDiscovery) Snapshot() DiscoverySnapshot {
	if catalog == nil {
		return DiscoverySnapshot{}
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	result := DiscoverySnapshot{
		Implementations: make([]ImplementationResolution, 0, len(catalog.implementations)),
		Dependencies:    make([]DependencyResolution, 0, len(catalog.dependencies)),
	}
	for _, implementation := range catalog.implementations {
		result.Implementations = append(result.Implementations, cloneImplementation(implementation))
	}
	for _, dependency := range catalog.dependencies {
		result.Dependencies = append(result.Dependencies, dependency)
	}
	sort.Slice(result.Implementations, func(left, right int) bool {
		return result.Implementations[left].Reference < result.Implementations[right].Reference
	})
	sort.Slice(result.Dependencies, func(left, right int) bool {
		return result.Dependencies[left].Name < result.Dependencies[right].Name
	})
	return result
}

// LegacyDescriptorDiscovery is the explicit compatibility adapter for
// in-process deployments whose implementation reference is exactly the locked
// descriptor name. It proves descriptor identity only, reports declared (not
// registered/live) evidence, rejects custom placement/transport, and cannot
// satisfy runtime service dependencies. New deployments should use an actual
// discovery catalog.
type LegacyDescriptorDiscovery struct {
	Catalog *resolve.Catalog
}

func (legacy LegacyDescriptorDiscovery) ResolveImplementation(
	ctx context.Context, request ImplementationRequest,
) (ImplementationResolution, error) {
	if ctx == nil {
		return ImplementationResolution{}, errors.New("legacy descriptor discovery: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return ImplementationResolution{}, err
	}
	if legacy.Catalog == nil {
		return ImplementationResolution{}, errors.New("legacy descriptor discovery: nil descriptor catalog")
	}
	if request.Reference != request.Contract.Name {
		return ImplementationResolution{}, fmt.Errorf(
			"legacy descriptor discovery only accepts implementation %q for contract %q",
			request.Contract.Name, request.Contract.Name)
	}
	if request.Placement != "" || (request.Transport != "" && request.Transport != "in-process") {
		return ImplementationResolution{}, errors.New("legacy descriptor discovery cannot attest custom placement or transport")
	}
	if len(request.ResourceKeys) != 0 || len(request.SecretSlotKeys) != 0 {
		return ImplementationResolution{}, errors.New("legacy descriptor discovery cannot attest resource or secret slots")
	}
	descriptor, found := legacy.Catalog.Exact(request.Contract)
	if !found {
		return ImplementationResolution{}, fmt.Errorf("legacy descriptor discovery cannot resolve %+v", request.Contract)
	}
	identity, err := descriptor.Identity()
	if err != nil || identity != request.Contract {
		return ImplementationResolution{}, fmt.Errorf("legacy descriptor discovery exact lookup changed identity")
	}
	return ImplementationResolution{
		Reference: request.Reference, Contract: request.Contract,
		Artifact: inspect.ArtifactIdentity{
			ID: request.Reference, Revision: strconv.FormatUint(request.Contract.Revision, 10),
			Digest: request.Contract.Digest,
		},
		Evidence:   inspect.EvidenceDeclared,
		Transports: []string{"in-process"},
	}, nil
}

func (LegacyDescriptorDiscovery) ResolveDependency(context.Context, string) (DependencyResolution, error) {
	return DependencyResolution{}, errors.New("legacy descriptor discovery cannot attest service dependencies")
}

func canonicalImplementation(source ImplementationResolution) (ImplementationResolution, error) {
	result := cloneImplementation(source)
	if err := canonicalText("implementation reference", result.Reference); err != nil {
		return ImplementationResolution{}, err
	}
	if err := element.ValidateIdentity(result.Contract); err != nil {
		return ImplementationResolution{}, fmt.Errorf("implementation %s contract: %w", result.Reference, err)
	}
	if err := result.Artifact.Validate(); err != nil {
		return ImplementationResolution{}, fmt.Errorf("implementation %s artifact: %w", result.Reference, err)
	}
	switch result.Evidence {
	case inspect.EvidenceDeclared, inspect.EvidenceRegistered:
	default:
		return ImplementationResolution{}, fmt.Errorf("implementation %s has invalid preflight evidence %q",
			result.Reference, result.Evidence)
	}
	var err error
	result.Placements, err = canonicalStrings("placement", result.Placements)
	if err != nil {
		return ImplementationResolution{}, err
	}
	result.Transports, err = canonicalStrings("transport", result.Transports)
	if err != nil {
		return ImplementationResolution{}, err
	}
	result.ResourceKeys, err = canonicalStrings("resource key", result.ResourceKeys)
	if err != nil {
		return ImplementationResolution{}, err
	}
	result.SecretSlots, err = canonicalStrings("secret slot", result.SecretSlots)
	if err != nil {
		return ImplementationResolution{}, err
	}
	if result.Capabilities != nil {
		result.Capabilities, err = inspect.CanonicalCapabilities(result.Capabilities)
		if err != nil {
			return ImplementationResolution{}, fmt.Errorf("implementation %s capabilities: %w", result.Reference, err)
		}
	}
	return result, nil
}

func canonicalDependency(source DependencyResolution) (DependencyResolution, error) {
	if err := canonicalText("dependency name", source.Name); err != nil {
		return DependencyResolution{}, err
	}
	if err := source.Artifact.Validate(); err != nil {
		return DependencyResolution{}, fmt.Errorf("dependency %s artifact: %w", source.Name, err)
	}
	// DependencyResolution has no service or ownership information from which
	// a missing scope can be inferred safely. Discovery is the first new
	// metadata boundary, so fail closed instead of silently treating a future
	// mount-scoped provider as process-scoped.
	if err := source.Scope.Validate(); err != nil {
		return DependencyResolution{}, fmt.Errorf("dependency %s: %w", source.Name, err)
	}
	return source, nil
}

func canonicalStrings(kind string, source []string) ([]string, error) {
	result := slices.Clone(source)
	for _, value := range result {
		if err := canonicalText(kind, value); err != nil {
			return nil, err
		}
	}
	sort.Strings(result)
	result = slices.Compact(result)
	return result, nil
}

func canonicalText(kind, value string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") || len(value) > 64<<10 {
		return fmt.Errorf("%s %q is not canonical", kind, value)
	}
	return nil
}

func cloneImplementation(source ImplementationResolution) ImplementationResolution {
	result := source
	result.Placements = slices.Clone(source.Placements)
	result.Transports = slices.Clone(source.Transports)
	result.ResourceKeys = slices.Clone(source.ResourceKeys)
	result.SecretSlots = slices.Clone(source.SecretSlots)
	if source.Capabilities == nil {
		result.Capabilities = nil
		return result
	}
	result.Capabilities = make([]inspect.CapabilityIdentity, len(source.Capabilities))
	for index, capability := range source.Capabilities {
		result.Capabilities[index] = capability
		if capability.Adapter != nil {
			adapter := *capability.Adapter
			result.Capabilities[index].Adapter = &adapter
		}
	}
	return result
}

func equalImplementation(left, right ImplementationResolution) bool {
	leftDigest, leftErr := semanticDigest("implementation-resolution", left)
	rightDigest, rightErr := semanticDigest("implementation-resolution", right)
	return leftErr == nil && rightErr == nil && leftDigest == rightDigest
}

func containsCanonical(values []string, wanted string) bool {
	if wanted == "" {
		return true
	}
	index, found := slices.BinarySearch(values, wanted)
	return found && index < len(values)
}

var _ Discovery = (*StaticDiscovery)(nil)
var _ Discovery = LegacyDescriptorDiscovery{}
