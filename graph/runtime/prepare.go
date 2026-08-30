package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/deployment"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

const (
	maximumFactoryProfileItems = 4_096
	maximumMountDependencies   = 65_536
	maximumRuntimeTextBytes    = 64 << 10
	defaultInProcessTransport  = "in-process"
)

// FactoryProfile is the immutable startup contract published by one runtime
// implementation. It contains metadata only: registering a profile must not
// dial a provider, resolve a secret, open a device, or otherwise acquire a
// runtime resource. A nil Capabilities slice means that provider capabilities
// are selected by the factory's live readiness handshake. A non-nil slice,
// including an explicitly empty one, is an exact registered capability set
// that live reporting must match.
type FactoryProfile struct {
	Reference    string                       `json:"reference"`
	Artifact     inspect.ArtifactIdentity     `json:"artifact"`
	Placements   []string                     `json:"placements,omitempty"`
	Transports   []string                     `json:"transports"`
	ResourceKeys []string                     `json:"resource_keys,omitempty"`
	SecretSlots  []string                     `json:"secret_slots,omitempty"`
	Capabilities []inspect.CapabilityIdentity `json:"capabilities,omitempty"`
}

// FactoryRegistration couples exact, resource-free startup metadata with the
// factory that will eventually mount the element. Factory.Mount remains the
// first operation in this API that is allowed to acquire resources.
type FactoryRegistration struct {
	Profile FactoryProfile
	Factory element.Factory
}

// RegisterFactory records the complete production startup profile for an
// implementation. Register and RegisterArtifact remain compatibility APIs;
// plans prepared through PreparePlan deliberately refuse those incomplete
// registrations.
func (registry *Registry) RegisterFactory(registration FactoryRegistration) error {
	profile, err := canonicalFactoryProfile(registration.Profile)
	if err != nil {
		return fmt.Errorf("register exact element factory: %w", err)
	}
	if registration.Factory == nil {
		return fmt.Errorf("register exact element factory %q: nil factory", profile.Reference)
	}
	return registry.register(
		profile.Reference,
		profile.Artifact,
		inspect.EvidenceRegistered,
		registration.Factory,
		&profile,
	)
}

func canonicalFactoryProfile(source FactoryProfile) (FactoryProfile, error) {
	result := cloneFactoryProfileValue(source)
	if err := canonicalRuntimeText("factory reference", result.Reference); err != nil {
		return FactoryProfile{}, err
	}
	if err := result.Artifact.Validate(); err != nil {
		return FactoryProfile{}, fmt.Errorf("factory %s artifact: %w", result.Reference, err)
	}
	var err error
	if result.Placements, err = canonicalRuntimeStrings("placement", result.Placements); err != nil {
		return FactoryProfile{}, err
	}
	if result.Transports, err = canonicalRuntimeStrings("transport", result.Transports); err != nil {
		return FactoryProfile{}, err
	}
	if len(result.Transports) == 0 {
		return FactoryProfile{}, fmt.Errorf("factory %s must attest at least one transport", result.Reference)
	}
	if result.ResourceKeys, err = canonicalRuntimeStrings("resource key", result.ResourceKeys); err != nil {
		return FactoryProfile{}, err
	}
	if result.SecretSlots, err = canonicalRuntimeStrings("secret slot", result.SecretSlots); err != nil {
		return FactoryProfile{}, err
	}
	if len(result.Capabilities) > maximumFactoryProfileItems {
		return FactoryProfile{}, fmt.Errorf(
			"factory %s has %d capabilities; maximum is %d",
			result.Reference,
			len(result.Capabilities),
			maximumFactoryProfileItems,
		)
	}
	if result.Capabilities != nil {
		result.Capabilities, err = inspect.CanonicalCapabilities(result.Capabilities)
		if err != nil {
			return FactoryProfile{}, fmt.Errorf("factory %s capabilities: %w", result.Reference, err)
		}
	}
	return result, nil
}

func canonicalRuntimeStrings(kind string, source []string) ([]string, error) {
	if len(source) > maximumFactoryProfileItems {
		return nil, fmt.Errorf("factory profile has %d %ss; maximum is %d", len(source), kind, maximumFactoryProfileItems)
	}
	result := slices.Clone(source)
	for _, value := range result {
		if err := canonicalRuntimeText(kind, value); err != nil {
			return nil, err
		}
	}
	sort.Strings(result)
	return slices.Compact(result), nil
}

func canonicalRuntimeText(kind, value string) error {
	if value == "" || value != strings.TrimSpace(value) ||
		strings.ContainsAny(value, "\x00\r\n") || len(value) > maximumRuntimeTextBytes {
		return fmt.Errorf("%s %q is not canonical", kind, value)
	}
	return nil
}

func cloneFactoryProfile(source *FactoryProfile) *FactoryProfile {
	if source == nil {
		return nil
	}
	result := cloneFactoryProfileValue(*source)
	return &result
}

func cloneFactoryProfileValue(source FactoryProfile) FactoryProfile {
	result := source
	result.Placements = slices.Clone(source.Placements)
	result.Transports = slices.Clone(source.Transports)
	result.ResourceKeys = slices.Clone(source.ResourceKeys)
	result.SecretSlots = slices.Clone(source.SecretSlots)
	result.Capabilities = cloneRuntimeCapabilities(source.Capabilities)
	return result
}

func cloneRuntimeCapabilities(source []inspect.CapabilityIdentity) []inspect.CapabilityIdentity {
	if source == nil {
		return nil
	}
	result := make([]inspect.CapabilityIdentity, len(source))
	for index, capability := range source {
		result[index] = capability
		if capability.Adapter != nil {
			adapter := *capability.Adapter
			result[index].Adapter = &adapter
		}
	}
	return result
}

type dependencyRegistration struct {
	artifact     inspect.ArtifactIdentity
	scope        graphconfig.DependencyScope
	value        any
	runtimeOwned bool
}

// DependencyRegistry binds required runtime services to exact executable or
// provider artifacts. Entries are create-only, so a successful preparation
// can snapshot them without observing a replacement race.
type DependencyRegistry struct {
	mu      sync.RWMutex
	entries map[string]dependencyRegistration
}

func NewDependencyRegistry() *DependencyRegistry {
	return &DependencyRegistry{entries: make(map[string]dependencyRegistration)}
}

func (registry *DependencyRegistry) Register(
	name string,
	artifact inspect.ArtifactIdentity,
	value any,
) error {
	if registry == nil {
		return errors.New("register runtime dependency: nil registry")
	}
	if err := canonicalRuntimeText("dependency name", name); err != nil {
		return err
	}
	if isRuntimeOwnedDependency(name) {
		return fmt.Errorf("register runtime dependency %s: use RegisterRuntimeOwned for runtime-owned coeffects", name)
	}
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("register runtime dependency %s artifact: %w", name, err)
	}
	if nilServiceValue(value) {
		return fmt.Errorf("register runtime dependency %s: nil service", name)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]dependencyRegistration)
	}
	if _, duplicate := registry.entries[name]; duplicate {
		return fmt.Errorf("runtime dependency %q is already registered", name)
	}
	registry.entries[name] = dependencyRegistration{
		artifact: artifact, scope: graphconfig.DependencyScopeProcess, value: value,
	}
	return nil
}

// RegisterMountScoped attests a dependency whose service is selected for each
// PreparedPlan.Mount call. It records only immutable metadata and therefore
// does not acquire, retain, or inspect the eventual mount service.
func (registry *DependencyRegistry) RegisterMountScoped(
	name string,
	artifact inspect.ArtifactIdentity,
) error {
	if registry == nil {
		return errors.New("register mount-scoped dependency: nil registry")
	}
	if err := canonicalRuntimeText("dependency name", name); err != nil {
		return err
	}
	if isRuntimeOwnedDependency(name) {
		return fmt.Errorf(
			"register mount-scoped dependency %s: use RegisterRuntimeOwned for runtime-owned coeffects",
			name,
		)
	}
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("register mount-scoped dependency %s artifact: %w", name, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]dependencyRegistration)
	}
	if _, duplicate := registry.entries[name]; duplicate {
		return fmt.Errorf("runtime dependency %q is already registered", name)
	}
	registry.entries[name] = dependencyRegistration{
		artifact: artifact, scope: graphconfig.DependencyScopeMount,
	}
	return nil
}

// RegisterRuntimeOwned attests the exact runtime artifact that supplies a
// mount-scoped standard coeffect. No caller value is accepted because Mount
// constructs these services itself; accepting one would attest an object that
// the runtime-owned overlay then shadows.
func (registry *DependencyRegistry) RegisterRuntimeOwned(
	name string,
	artifact inspect.ArtifactIdentity,
) error {
	if registry == nil {
		return errors.New("register runtime-owned dependency: nil registry")
	}
	if !isRuntimeOwnedDependency(name) {
		return fmt.Errorf("dependency %q is not runtime-owned", name)
	}
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("register runtime-owned dependency %s artifact: %w", name, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]dependencyRegistration)
	}
	if _, duplicate := registry.entries[name]; duplicate {
		return fmt.Errorf("runtime dependency %q is already registered", name)
	}
	registry.entries[name] = dependencyRegistration{
		artifact: artifact, scope: graphconfig.DependencyScopeMount, runtimeOwned: true,
	}
	return nil
}

func isRuntimeOwnedDependency(name string) bool {
	return name == ClockServiceName || name == SequenceServiceName || name == SecretServiceName
}

func (registry *DependencyRegistry) lookup(name string) (dependencyRegistration, bool) {
	if registry == nil {
		return dependencyRegistration{}, false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	entry, found := registry.entries[name]
	return entry, found
}

// PlanPreparation contains the mutable assembly registries consulted during
// the resource-free preparation phase. The returned PreparedPlan owns sealed
// snapshots and no longer consults these registries.
type PlanPreparation struct {
	Factories       *Registry
	Dependencies    *DependencyRegistry
	SecretCatalog   *graphsecret.Document
	SecretProviders *graphsecret.Registry
}

// PreparedNode is the public, redacted launch projection for one graph node.
// Secret slot names are contracts; secret references and resource values are
// deliberately absent.
type PreparedNode struct {
	NodeID           string                       `json:"node_id"`
	DeploymentDigest string                       `json:"deployment_digest"`
	Reference        string                       `json:"reference"`
	Artifact         inspect.ArtifactIdentity     `json:"artifact"`
	Placement        string                       `json:"placement,omitempty"`
	Transport        string                       `json:"transport"`
	ResourceKeys     []string                     `json:"resource_keys,omitempty"`
	SecretSlots      []string                     `json:"secret_slots,omitempty"`
	Capabilities     []inspect.CapabilityIdentity `json:"capabilities,omitempty"`
}

// PreparedDependency is the exact public identity of a required service.
type PreparedDependency struct {
	Name     string                      `json:"name"`
	Artifact inspect.ArtifactIdentity    `json:"artifact"`
	Scope    graphconfig.DependencyScope `json:"scope"`
}

// PublicPreparation is safe for default inspection and logging. It contains
// the plan's redacted deployment identity but no private deployment
// fingerprint, secret-catalog fingerprint, resource values, or secret
// references.
type PublicPreparation struct {
	Plan         graphconfig.Identity `json:"plan"`
	Nodes        []PreparedNode       `json:"nodes"`
	Dependencies []PreparedDependency `json:"dependencies,omitempty"`
}

// PrivatePlanIdentity is deliberately opaque to generic JSON/logging paths.
// Deployment operators can request each fingerprint explicitly when binding
// private evidence, while its default projection remains redacted.
type PrivatePlanIdentity struct {
	deploymentFingerprint    string
	secretCatalogFingerprint string
}

func (identity PrivatePlanIdentity) DeploymentFingerprint() string {
	return identity.deploymentFingerprint
}

func (identity PrivatePlanIdentity) SecretCatalogFingerprint() string {
	return identity.secretCatalogFingerprint
}

func (PrivatePlanIdentity) String() string   { return "<redacted private plan identity>" }
func (PrivatePlanIdentity) GoString() string { return "<redacted private plan identity>" }

func (PrivatePlanIdentity) MarshalJSON() ([]byte, error) {
	return []byte(`{"redacted":true}`), nil
}

// PrivateNodeBinding is an explicit operator-only view of one deployment
// binding. Its generic string and JSON representations never reveal resource
// values or secret references.
type PrivateNodeBinding struct {
	node deployment.Node
}

func (binding PrivateNodeBinding) Implementation() string { return binding.node.Implementation }
func (binding PrivateNodeBinding) Placement() string      { return binding.node.Placement }
func (binding PrivateNodeBinding) Transport() string      { return binding.node.Transport }

func (binding PrivateNodeBinding) Resources() map[string]string {
	return cloneRuntimeStringMap(binding.node.Resources)
}

func (binding PrivateNodeBinding) SecretReferences() map[string]string {
	return cloneRuntimeStringMap(binding.node.Secrets)
}

func (PrivateNodeBinding) String() string   { return "<redacted private node binding>" }
func (PrivateNodeBinding) GoString() string { return "<redacted private node binding>" }

func (PrivateNodeBinding) MarshalJSON() ([]byte, error) {
	return []byte(`{"redacted":true}`), nil
}

// PreparedPlan is a sealed plan-to-runtime handoff. Preparation resolves and
// checks every factory, capability, transport, resource/secret-slot contract,
// and required dependency before Mount can acquire the first resource.
type PreparedPlan struct {
	plan                *graphconfig.Plan
	identity            graphconfig.Identity
	private             PrivatePlanIdentity
	graph               ir.Graph
	values              map[string]json.RawMessage
	bindings            map[string]deployment.Node
	public              PublicPreparation
	registry            *Registry
	processServices     map[string]any
	dependencies        map[string]PreparedDependency
	mountDependencies   map[string]inspect.ArtifactIdentity
	runtimeDependencies map[string]bool
	secretStore         *graphsecret.Store
	secretBindings      map[string]map[string]string
}

// PreparePlan validates a complete immutable config.Plan against exact live
// assembly registrations. It is metadata-only and never invokes Factory.Mount
// or a service/secret provider.
func PreparePlan(
	ctx context.Context,
	plan *graphconfig.Plan,
	assembly PlanPreparation,
) (*PreparedPlan, error) {
	if ctx == nil {
		return nil, errors.New("prepare graph plan: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, errors.New("prepare graph plan: nil plan")
	}
	if assembly.Factories == nil {
		return nil, errors.New("prepare graph plan: exact factory registry is required")
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("prepare graph plan: %w", err)
	}

	identity := plan.Identity()
	graph := plan.Graph()
	values := plan.Values()
	bindings := plan.Deployment()
	resolution := plan.Resolution()
	byNode := make(map[string]graphconfig.ImplementationResolution, len(resolution.Nodes))
	for _, node := range resolution.Nodes {
		byNode[node.NodeID] = node.Implementation
	}

	sealedRegistry := NewRegistry()
	selectedProfiles := make(map[string]FactoryProfile)
	public := PublicPreparation{
		Plan:  identity,
		Nodes: make([]PreparedNode, 0, len(graph.Nodes)),
	}
	for _, node := range graph.Nodes {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		expected, found := byNode[node.ID]
		if !found {
			return nil, fmt.Errorf("prepare graph plan: resolution is missing node %s", node.ID)
		}
		binding, found := bindings[node.ID]
		if !found {
			return nil, fmt.Errorf("prepare graph plan: deployment is missing node %s", node.ID)
		}
		factory, descriptor, artifact, evidence, profile, err := assembly.Factories.resolveProfile(
			expected.Reference,
			node.Element,
		)
		if err != nil {
			return nil, fmt.Errorf("prepare graph plan node %s: %w", node.ID, err)
		}
		if err := verifyNodeContract(node, descriptor); err != nil {
			return nil, fmt.Errorf("prepare graph plan node %s contract verification: %w", node.ID, err)
		}
		actual := implementationResolution(profile, node.Element)
		if artifact != expected.Artifact || evidence != inspect.EvidenceRegistered ||
			!reflect.DeepEqual(actual, expected) {
			return nil, fmt.Errorf(
				"prepare graph plan node %s implementation %q drifted from the frozen resolution",
				node.ID,
				expected.Reference,
			)
		}
		selectedTransport := binding.Transport
		if selectedTransport == "" {
			selectedTransport = defaultInProcessTransport
		}
		if !containsRuntimeString(profile.Transports, selectedTransport) {
			return nil, fmt.Errorf(
				"prepare graph plan node %s implementation %q does not attest selected transport %q",
				node.ID,
				expected.Reference,
				selectedTransport,
			)
		}
		if binding.Placement != "" && !containsRuntimeString(profile.Placements, binding.Placement) {
			return nil, fmt.Errorf(
				"prepare graph plan node %s implementation %q does not attest selected placement %q",
				node.ID,
				expected.Reference,
				binding.Placement,
			)
		}
		for resource := range binding.Resources {
			if !containsRuntimeString(profile.ResourceKeys, resource) {
				return nil, fmt.Errorf(
					"prepare graph plan node %s implementation %q does not attest resource %q",
					node.ID,
					expected.Reference,
					resource,
				)
			}
		}
		for slot := range binding.Secrets {
			if !containsRuntimeString(profile.SecretSlots, slot) {
				return nil, fmt.Errorf(
					"prepare graph plan node %s implementation %q does not attest secret slot %q",
					node.ID,
					expected.Reference,
					slot,
				)
			}
		}
		if len(binding.Secrets) != 0 && !descriptorRequiresDependency(descriptor, SecretServiceName) {
			return nil, fmt.Errorf(
				"prepare graph plan node %s binds secret slots but implementation %q does not require %q",
				node.ID,
				expected.Reference,
				SecretServiceName,
			)
		}
		// Invoke implementation code only after its complete resource-free
		// profile is proven identical to the frozen plan and deployment
		// selection. Config validation still precedes dependency sealing,
		// secret-store binding, and Factory.Mount.
		// ConfigValidator is implementation code. Give it an isolated value so
		// an implementation that mutates or retains the RawMessage cannot alter
		// the locally owned Plan.Values snapshot that will be sealed below.
		if err := validateFactoryConfig(node, factory, slices.Clone(values[node.ID])); err != nil {
			return nil, fmt.Errorf("prepare graph plan node %s config: %w", node.ID, err)
		}
		if previous, duplicate := selectedProfiles[profile.Reference]; duplicate {
			if !reflect.DeepEqual(previous, profile) {
				return nil, fmt.Errorf("prepare graph plan: implementation %q resolved inconsistently", profile.Reference)
			}
		} else {
			selectedProfiles[profile.Reference] = profile
			if err := sealedRegistry.register(
				profile.Reference,
				profile.Artifact,
				inspect.EvidenceRegistered,
				factory,
				&profile,
			); err != nil {
				return nil, fmt.Errorf("prepare graph plan node %s seal factory: %w", node.ID, err)
			}
		}
		public.Nodes = append(public.Nodes, PreparedNode{
			NodeID: node.ID, DeploymentDigest: node.DeploymentDigest,
			Reference: profile.Reference, Artifact: profile.Artifact,
			Placement: binding.Placement, Transport: selectedTransport,
			ResourceKeys: sortedRuntimeKeys(binding.Resources),
			SecretSlots:  sortedRuntimeKeys(binding.Secrets),
			Capabilities: cloneRuntimeCapabilities(profile.Capabilities),
		})
	}

	serviceValues := make(map[string]any, len(resolution.Dependencies))
	dependencyIdentities := make(map[string]PreparedDependency, len(resolution.Dependencies))
	mountDependencies := make(map[string]inspect.ArtifactIdentity)
	runtimeDependencies := make(map[string]bool)
	for _, expected := range resolution.Dependencies {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		registered, found := assembly.Dependencies.lookup(expected.Name)
		if !found {
			return nil, fmt.Errorf("prepare graph plan: required dependency %q is not registered", expected.Name)
		}
		if registered.artifact != expected.Artifact {
			return nil, fmt.Errorf("prepare graph plan: dependency %q artifact drifted from the frozen resolution", expected.Name)
		}
		if registered.scope != expected.Scope {
			return nil, fmt.Errorf("prepare graph plan: dependency %q scope drifted from the frozen resolution", expected.Name)
		}
		switch expected.Scope {
		case graphconfig.DependencyScopeProcess:
			if registered.runtimeOwned || nilServiceValue(registered.value) {
				return nil, fmt.Errorf("prepare graph plan: process-scoped dependency %q resolved to a nil or mount-owned service", expected.Name)
			}
			serviceValues[expected.Name] = registered.value
		case graphconfig.DependencyScopeMount:
			if !nilServiceValue(registered.value) {
				return nil, fmt.Errorf("prepare graph plan: mount-scoped dependency %q retained a preflight service", expected.Name)
			}
			if registered.runtimeOwned {
				if !isRuntimeOwnedDependency(expected.Name) {
					return nil, fmt.Errorf("prepare graph plan: dependency %q has invalid runtime ownership", expected.Name)
				}
				runtimeDependencies[expected.Name] = true
			} else {
				if isRuntimeOwnedDependency(expected.Name) {
					return nil, fmt.Errorf("prepare graph plan: dependency %q ownership drifted from the runtime contract", expected.Name)
				}
				mountDependencies[expected.Name] = expected.Artifact
			}
		default:
			return nil, fmt.Errorf("prepare graph plan: dependency %q has invalid scope %q", expected.Name, expected.Scope)
		}
		identity := PreparedDependency{
			Name: expected.Name, Artifact: expected.Artifact, Scope: expected.Scope,
		}
		dependencyIdentities[expected.Name] = identity
		public.Dependencies = append(public.Dependencies, identity)
	}
	secretStore, secretBindings, err := prepareSecretStore(plan, bindings, assembly)
	if err != nil {
		return nil, fmt.Errorf("prepare graph plan: %w", err)
	}

	prepared := &PreparedPlan{
		plan: plan, identity: identity,
		private: PrivatePlanIdentity{
			deploymentFingerprint:    plan.DeploymentFingerprint(),
			secretCatalogFingerprint: plan.SecretCatalogFingerprint(),
		},
		// graph, values, bindings, and public are already function-owned
		// snapshots. No caller or implementation retains an alias: the only
		// external callback above received its own values copy. Transfer that
		// ownership into the sealed plan; exported accessors still clone on read.
		graph: graph, values: values, bindings: bindings,
		public: public, registry: sealedRegistry, processServices: serviceValues,
		dependencies: dependencyIdentities, mountDependencies: mountDependencies,
		runtimeDependencies: runtimeDependencies,
		secretStore:         secretStore, secretBindings: secretBindings,
	}
	// The source Plan was fully validated before any registry lookup above.
	// Recheck only the newly sealed handoff here; exported Validate repeats both
	// halves immediately before a later mount.
	if err := prepared.validateSealed(); err != nil {
		return nil, fmt.Errorf("prepare graph plan: internal validation: %w", err)
	}
	return prepared, nil
}

func prepareSecretStore(
	plan *graphconfig.Plan,
	bindings map[string]deployment.Node,
	assembly PlanPreparation,
) (*graphsecret.Store, map[string]map[string]string, error) {
	want := plan.SecretCatalogFingerprint()
	if want == "" {
		if assembly.SecretCatalog != nil {
			return nil, nil, errors.New(
				"secret catalog was supplied but the frozen plan has no secret-catalog identity",
			)
		}
		return nil, nil, nil
	}
	if assembly.SecretCatalog == nil {
		return nil, nil, errors.New("frozen plan requires its exact graph-scoped secret catalog")
	}
	got, err := graphconfig.SecretCatalogFingerprint(assembly.SecretCatalog)
	if err != nil {
		return nil, nil, fmt.Errorf("fingerprint secret catalog: %w", err)
	}
	if got != want {
		return nil, nil, errors.New("secret catalog drifted from the frozen plan")
	}
	if err := deployment.ValidateSecretCatalog(bindings, *assembly.SecretCatalog); err != nil {
		return nil, nil, err
	}

	result := make(map[string]map[string]string)
	for nodeID, binding := range bindings {
		if len(binding.Secrets) == 0 {
			continue
		}
		result[nodeID] = cloneRuntimeStringMap(binding.Secrets)
	}
	if len(result) == 0 {
		return nil, result, nil
	}
	if assembly.SecretProviders == nil {
		return nil, nil, errors.New("secret-bearing plan requires an exact provider registry")
	}
	store, err := graphsecret.NewStore(*assembly.SecretCatalog, assembly.SecretProviders)
	if err != nil {
		return nil, nil, fmt.Errorf("bind secret store: %w", err)
	}
	return store, result, nil
}

func descriptorRequiresDependency(descriptor element.Descriptor, name string) bool {
	for _, dependency := range descriptor.Dependencies {
		if dependency.Name == name && !dependency.Optional {
			return true
		}
	}
	return false
}

func (registry *Registry) resolveProfile(
	reference string,
	identity element.Identity,
) (
	element.Factory,
	element.Descriptor,
	inspect.ArtifactIdentity,
	inspect.ResolutionEvidence,
	FactoryProfile,
	error,
) {
	factory, descriptor, artifact, evidence, err := registry.resolve(reference, identity)
	if err != nil {
		return nil, element.Descriptor{}, inspect.ArtifactIdentity{}, "", FactoryProfile{}, err
	}
	registry.mu.RLock()
	registered, found := registry.factories[reference]
	registry.mu.RUnlock()
	if !found || registered.profile == nil {
		return nil, element.Descriptor{}, inspect.ArtifactIdentity{}, "", FactoryProfile{},
			fmt.Errorf("element implementation %q has no exact factory profile", reference)
	}
	// Registrations are create-only and RegisterFactory stored a canonical
	// defensive copy. This package-private result is read-only; returning the
	// value directly avoids repeatedly cloning its slices during preparation
	// and sealed-handoff verification without exposing them to callers.
	return factory, descriptor, artifact, evidence, *registered.profile, nil
}

func implementationResolution(
	profile FactoryProfile,
	contract element.Identity,
) graphconfig.ImplementationResolution {
	return graphconfig.ImplementationResolution{
		Reference: profile.Reference, Contract: contract, Artifact: profile.Artifact,
		Evidence: inspect.EvidenceRegistered, Placements: profile.Placements,
		Transports: profile.Transports, ResourceKeys: profile.ResourceKeys,
		SecretSlots: profile.SecretSlots, Capabilities: profile.Capabilities,
	}
}

func containsRuntimeString(values []string, wanted string) bool {
	_, found := slices.BinarySearch(values, wanted)
	return found
}

func sortedRuntimeKeys[V any](source map[string]V) []string {
	result := make([]string, 0, len(source))
	for key := range source {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func cloneRuntimeStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneRuntimeBindings(source map[string]deployment.Node) map[string]deployment.Node {
	result := make(map[string]deployment.Node, len(source))
	for id, node := range source {
		node.Resources = cloneRuntimeStringMap(node.Resources)
		node.Secrets = cloneRuntimeStringMap(node.Secrets)
		result[id] = node
	}
	return result
}

func cloneRuntimeValues(source map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(source))
	for id, value := range source {
		result[id] = slices.Clone(value)
	}
	return result
}

func clonePreparedNode(source PreparedNode) PreparedNode {
	result := source
	result.ResourceKeys = slices.Clone(source.ResourceKeys)
	result.SecretSlots = slices.Clone(source.SecretSlots)
	result.Capabilities = cloneRuntimeCapabilities(source.Capabilities)
	return result
}

func clonePublicPreparation(source PublicPreparation) PublicPreparation {
	result := source
	result.Nodes = make([]PreparedNode, len(source.Nodes))
	for index, node := range source.Nodes {
		result.Nodes[index] = clonePreparedNode(node)
	}
	result.Dependencies = slices.Clone(source.Dependencies)
	return result
}

// Identity returns the public plan identity. Its deployment digest is the
// redacted identity frozen into Graph IR.
func (prepared *PreparedPlan) Identity() graphconfig.Identity {
	if prepared == nil {
		return graphconfig.Identity{}
	}
	return prepared.identity
}

func (prepared *PreparedPlan) Graph() ir.Graph {
	if prepared == nil {
		return ir.Graph{}
	}
	graph, err := ir.Freeze(prepared.graph)
	if err != nil {
		return ir.Graph{}
	}
	return graph
}

func (prepared *PreparedPlan) Public() PublicPreparation {
	if prepared == nil {
		return PublicPreparation{}
	}
	return clonePublicPreparation(prepared.public)
}

func (prepared *PreparedPlan) PrivateIdentity() PrivatePlanIdentity {
	if prepared == nil {
		return PrivatePlanIdentity{}
	}
	return prepared.private
}

// PrivateBinding returns an isolated operator-only deployment view for one
// node. The boolean is false for an unknown node.
func (prepared *PreparedPlan) PrivateBinding(nodeID string) (PrivateNodeBinding, bool) {
	if prepared == nil {
		return PrivateNodeBinding{}, false
	}
	node, found := prepared.bindings[nodeID]
	if !found {
		return PrivateNodeBinding{}, false
	}
	node.Resources = cloneRuntimeStringMap(node.Resources)
	node.Secrets = cloneRuntimeStringMap(node.Secrets)
	return PrivateNodeBinding{node: node}, true
}

// DeploymentIdentity returns the redacted deployment artifact carried through
// the mount boundary. It is safe for ordinary inspection and telemetry.
func (mounted *Mounted) DeploymentIdentity() (inspect.ArtifactIdentity, bool) {
	if mounted == nil || mounted.deployment == nil {
		return inspect.ArtifactIdentity{}, false
	}
	return *mounted.deployment, true
}

// DeploymentEvidence returns an isolated copy of the exact public/private
// deployment identity and any provider resolutions observed so far. It never
// contains a deployment secret reference, provider locator, or secret bytes.
func (mounted *Mounted) DeploymentEvidence() (inspect.DeploymentEvidence, bool) {
	if mounted == nil {
		return inspect.DeploymentEvidence{}, false
	}
	mounted.liveMu.Lock()
	defer mounted.liveMu.Unlock()
	if mounted.deploymentEvidence == nil {
		return inspect.DeploymentEvidence{}, false
	}
	return mounted.deploymentEvidence.Clone(), true
}

// PrivateIdentity returns the opaque deployment/secret identity carried by a
// graph-native prepared mount. Generic JSON and formatting remain redacted.
func (mounted *Mounted) PrivateIdentity() (PrivatePlanIdentity, bool) {
	if mounted == nil || mounted.privatePlanIdentity.deploymentFingerprint == "" {
		return PrivatePlanIdentity{}, false
	}
	return mounted.privatePlanIdentity, true
}

func (prepared *PreparedPlan) MarshalJSON() ([]byte, error) {
	if prepared == nil {
		return []byte("null"), nil
	}
	return json.Marshal(prepared.Public())
}

// Validate repeats the complete resource-free handoff verification. It also
// detects descriptor mutation after preparation and is safe for concurrent
// launcher calls.
func (prepared *PreparedPlan) Validate() error {
	if prepared == nil {
		return errors.New("validate prepared graph plan: nil preparation")
	}
	if prepared.plan == nil {
		return errors.New("validate prepared graph plan: source plan is missing")
	}
	if err := prepared.plan.Validate(); err != nil {
		return fmt.Errorf("validate prepared graph plan: %w", err)
	}
	if prepared.plan.Identity() != prepared.identity ||
		prepared.plan.DeploymentFingerprint() != prepared.private.deploymentFingerprint ||
		prepared.plan.SecretCatalogFingerprint() != prepared.private.secretCatalogFingerprint {
		return errors.New("validate prepared graph plan: source identity changed")
	}
	return prepared.validateSealed()
}

func (prepared *PreparedPlan) validateSealed() error {
	if err := prepared.graph.Validate(); err != nil {
		return fmt.Errorf("validate prepared graph plan graph: %w", err)
	}
	if prepared.graph.Fingerprint != prepared.identity.GraphFingerprint {
		return errors.New("validate prepared graph plan: graph identity changed")
	}
	for _, node := range prepared.public.Nodes {
		graphNode, found := runtimeGraphNode(prepared.graph, node.NodeID)
		if !found {
			return fmt.Errorf("validate prepared graph plan: public node %s is absent", node.NodeID)
		}
		_, descriptor, artifact, evidence, profile, err := prepared.registry.resolveProfile(
			node.Reference,
			graphNode.Element,
		)
		if err != nil {
			return fmt.Errorf("validate prepared graph plan node %s: %w", node.NodeID, err)
		}
		if err := verifyNodeContract(graphNode, descriptor); err != nil {
			return fmt.Errorf("validate prepared graph plan node %s: %w", node.NodeID, err)
		}
		if artifact != node.Artifact || evidence != inspect.EvidenceRegistered ||
			profile.Reference != node.Reference || profile.Artifact != node.Artifact ||
			!reflect.DeepEqual(profile.Capabilities, node.Capabilities) ||
			!containsRuntimeString(profile.Transports, node.Transport) {
			return fmt.Errorf("validate prepared graph plan node %s: sealed factory drift", node.NodeID)
		}
	}
	if len(prepared.dependencies) != len(prepared.public.Dependencies) {
		return errors.New("validate prepared graph plan: sealed dependency inventory drift")
	}
	processCount := 0
	mountCount := 0
	runtimeCount := 0
	for _, dependency := range prepared.public.Dependencies {
		identity, found := prepared.dependencies[dependency.Name]
		if !found || identity != dependency {
			return fmt.Errorf("validate prepared graph plan: sealed dependency %q identity drift", dependency.Name)
		}
		switch dependency.Scope {
		case graphconfig.DependencyScopeProcess:
			processCount++
			if prepared.runtimeDependencies[dependency.Name] {
				return fmt.Errorf("validate prepared graph plan: process dependency %q has runtime ownership", dependency.Name)
			}
			if _, found := prepared.mountDependencies[dependency.Name]; found {
				return fmt.Errorf("validate prepared graph plan: process dependency %q has mount metadata", dependency.Name)
			}
			value, found := prepared.processServices[dependency.Name]
			if !found || nilServiceValue(value) {
				return fmt.Errorf("validate prepared graph plan: sealed dependency %q service drift", dependency.Name)
			}
		case graphconfig.DependencyScopeMount:
			if _, found := prepared.processServices[dependency.Name]; found {
				return fmt.Errorf("validate prepared graph plan: mount dependency %q retained a process service", dependency.Name)
			}
			if prepared.runtimeDependencies[dependency.Name] {
				runtimeCount++
				if !isRuntimeOwnedDependency(dependency.Name) {
					return fmt.Errorf("validate prepared graph plan: dependency %q has invalid runtime ownership", dependency.Name)
				}
				if _, found := prepared.mountDependencies[dependency.Name]; found {
					return fmt.Errorf("validate prepared graph plan: runtime dependency %q also requires a caller overlay", dependency.Name)
				}
				continue
			}
			mountCount++
			artifact, found := prepared.mountDependencies[dependency.Name]
			if !found || artifact != dependency.Artifact {
				return fmt.Errorf("validate prepared graph plan: mount dependency %q identity drift", dependency.Name)
			}
		default:
			return fmt.Errorf("validate prepared graph plan: dependency %q has invalid scope %q", dependency.Name, dependency.Scope)
		}
	}
	if len(prepared.processServices) != processCount ||
		len(prepared.mountDependencies) != mountCount ||
		len(prepared.runtimeDependencies) != runtimeCount {
		return errors.New("validate prepared graph plan: sealed dependency scope inventory drift")
	}
	hasSecretBindings := false
	secretNodeCount := 0
	for nodeID, binding := range prepared.bindings {
		sealed, found := prepared.secretBindings[nodeID]
		if len(binding.Secrets) == 0 {
			if found {
				return fmt.Errorf("validate prepared graph plan: node %s has excess sealed secret bindings", nodeID)
			}
			continue
		}
		hasSecretBindings = true
		secretNodeCount++
		if !found || !reflect.DeepEqual(sealed, binding.Secrets) {
			return fmt.Errorf("validate prepared graph plan: node %s secret binding drift", nodeID)
		}
	}
	if len(prepared.secretBindings) != secretNodeCount {
		return errors.New("validate prepared graph plan: excess sealed secret bindings")
	}
	if hasSecretBindings {
		if prepared.secretStore == nil {
			return errors.New("validate prepared graph plan: sealed secret store is missing")
		}
		if !prepared.runtimeDependencies[SecretServiceName] {
			return errors.New("validate prepared graph plan: runtime secret dependency is missing")
		}
	}
	return nil
}

func runtimeGraphNode(graph ir.Graph, id string) (ir.Node, bool) {
	index, found := slices.BinarySearchFunc(graph.Nodes, id, func(node ir.Node, wanted string) int {
		return strings.Compare(node.ID, wanted)
	})
	if !found {
		return ir.Node{}, false
	}
	return graph.Nodes[index], true
}

// PreparedMountDependency supplies one service whose name, artifact, and
// mount scope were frozen by config.Create and PreparePlan. The slice form is
// intentional so duplicate contributions can be rejected instead of being
// overwritten by map construction.
type PreparedMountDependency struct {
	Name     string
	Artifact inspect.ArtifactIdentity
	Service  any
}

// PreparedMountOptions contains runtime supervision/inspection policy and the
// exact caller-supplied service overlay for externally hosted mount-scoped
// dependencies. Topology, values, implementations, dependency identities, and
// process-scoped services come exclusively from the sealed PreparedPlan.
type PreparedMountOptions struct {
	Dependencies    []PreparedMountDependency
	Tracer          Tracer
	Now             func() uint64
	ShutdownTimeout time.Duration
	Inspection      InspectionConfig
	TraceRecording  *TraceRecordingConfig
}

// Mount validates the sealed handoff immediately before delegating to the
// graph runtime. Runtime.Mount again checks every descriptor, dependency, and
// value before it invokes the first Factory.Mount.
func (prepared *PreparedPlan) Mount(
	ctx context.Context,
	options PreparedMountOptions,
) (*Mounted, error) {
	if ctx == nil {
		return nil, errors.New("mount prepared graph plan: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := prepared.Validate(); err != nil {
		return nil, fmt.Errorf("mount prepared graph plan: %w", err)
	}
	services, err := prepared.mountServiceSet(options.Dependencies)
	if err != nil {
		return nil, fmt.Errorf("mount prepared graph plan: %w", err)
	}
	configuration := inspect.ArtifactIdentity{
		ID:       "values://" + prepared.identity.GraphID,
		Revision: graphvalues.APIVersion,
		Digest:   prepared.identity.ValuesDigest,
	}
	deploymentIdentity := inspect.ArtifactIdentity{
		ID:       "deployment://" + prepared.identity.GraphID,
		Revision: deployment.APIVersion,
		Digest:   prepared.identity.PublicDeploymentDigest,
	}
	privateIdentity := prepared.private
	registeredCapabilities := make(map[string][]inspect.CapabilityIdentity, len(prepared.public.Nodes))
	for _, node := range prepared.public.Nodes {
		if node.Capabilities != nil {
			registeredCapabilities[node.NodeID] = cloneRuntimeCapabilities(node.Capabilities)
		}
	}
	return Mount(ctx, Config{
		Graph: prepared.graph, Registry: prepared.registry,
		Values: cloneRuntimeValues(prepared.values), Services: services,
		Configuration: &configuration, Deployment: &deploymentIdentity,
		privatePlanIdentity:    &privateIdentity,
		registeredCapabilities: registeredCapabilities,
		secretStore:            prepared.secretStore,
		secretBindings:         cloneRuntimeNestedStringMap(prepared.secretBindings),
		Tracer:                 options.Tracer, Now: options.Now,
		ShutdownTimeout: options.ShutdownTimeout, Inspection: options.Inspection,
		TraceRecording: options.TraceRecording,
	})
}

func (prepared *PreparedPlan) mountServiceSet(
	source []PreparedMountDependency,
) (*ServiceSet, error) {
	if len(source) > maximumMountDependencies {
		return nil, fmt.Errorf(
			"mount dependency overlay has %d entries; maximum is %d",
			len(source), maximumMountDependencies,
		)
	}
	overlay := slices.Clone(source)
	seen := make(map[string]struct{}, len(overlay))
	selected := make(map[string]struct{}, len(overlay))
	values := make(map[string]any, len(prepared.processServices)+len(overlay))
	for name, service := range prepared.processServices {
		values[name] = service
	}
	excess := make([]string, 0)
	for _, dependency := range overlay {
		if err := canonicalRuntimeText("mount dependency name", dependency.Name); err != nil {
			return nil, err
		}
		if _, duplicate := seen[dependency.Name]; duplicate {
			return nil, fmt.Errorf(
				"mount dependency overlay is ambiguous: %q is contributed more than once",
				dependency.Name,
			)
		}
		seen[dependency.Name] = struct{}{}
		if frozen, found := prepared.dependencies[dependency.Name]; found &&
			frozen.Scope == graphconfig.DependencyScopeProcess {
			return nil, fmt.Errorf(
				"mount dependency %q cannot shadow a process-scoped dependency",
				dependency.Name,
			)
		}
		if prepared.runtimeDependencies[dependency.Name] {
			return nil, fmt.Errorf(
				"mount dependency %q cannot shadow a runtime-owned dependency",
				dependency.Name,
			)
		}
		expected, found := prepared.mountDependencies[dependency.Name]
		if !found {
			excess = append(excess, dependency.Name)
			continue
		}
		if err := dependency.Artifact.Validate(); err != nil {
			return nil, fmt.Errorf("mount dependency %q artifact: %w", dependency.Name, err)
		}
		if dependency.Artifact != expected {
			return nil, fmt.Errorf(
				"mount dependency %q artifact drifted from the frozen resolution",
				dependency.Name,
			)
		}
		if nilServiceValue(dependency.Service) {
			return nil, fmt.Errorf("mount dependency %q has nil service", dependency.Name)
		}
		selected[dependency.Name] = struct{}{}
		values[dependency.Name] = dependency.Service
	}
	missing := make([]string, 0)
	for name := range prepared.mountDependencies {
		if _, found := selected[name]; !found {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 || len(excess) != 0 {
		sort.Strings(missing)
		sort.Strings(excess)
		parts := make([]string, 0, 2)
		if len(missing) != 0 {
			parts = append(parts, "missing ["+strings.Join(missing, ", ")+"]")
		}
		if len(excess) != 0 {
			parts = append(parts, "excess ["+strings.Join(excess, ", ")+"]")
		}
		return nil, fmt.Errorf(
			"mount dependency overlay is not exact: %s", strings.Join(parts, "; "),
		)
	}
	services := NewServiceSet()
	if len(values) != 0 {
		if _, err := services.InstallIfAbsent(values); err != nil {
			return nil, fmt.Errorf("install mount dependency overlay: %w", err)
		}
	}
	return services, nil
}

func cloneRuntimeNestedStringMap(source map[string]map[string]string) map[string]map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]map[string]string, len(source))
	for key, values := range source {
		result[key] = cloneRuntimeStringMap(values)
	}
	return result
}
