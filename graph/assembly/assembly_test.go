package assembly_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

const (
	assemblyTopology = `graph assembly_launch {
    test.Assembly :: worker;

    input input = worker.in;
    output output = worker.out;
}
`
	assemblySecretReference = "secret://assembly/worker/token"
	assemblySecretLocator   = "ASSEMBLY_PRIVATE_TOKEN"
	assemblySecretValue     = "assembly-private-value"
)

type assemblyFixture struct {
	plan       *graphconfig.Plan
	assembly   graphassembly.Assembly
	descriptor element.Descriptor
	profile    graphruntime.FactoryProfile
	mounts     *atomic.Int64
	resolves   *atomic.Int64
	provider   *assemblySecretProvider
	service    *assemblyService
}

type assemblyService struct{ name string }

type assemblySecretProvider struct {
	resolves *atomic.Int64
	mu       sync.Mutex
	locators []string
}

func (provider *assemblySecretProvider) Resolve(ctx context.Context, locator string) ([]byte, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	provider.resolves.Add(1)
	provider.mu.Lock()
	provider.locators = append(provider.locators, locator)
	provider.mu.Unlock()
	return []byte(assemblySecretValue), nil
}

func (provider *assemblySecretProvider) locatorSnapshot() []string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return slices.Clone(provider.locators)
}

type assemblyFactory struct {
	descriptor       element.Descriptor
	mounts           *atomic.Int64
	service          *assemblyService
	optionalService  *assemblyService
	optionalSelected bool
}

func (factory assemblyFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory assemblyFactory) Mount(
	ctx context.Context,
	mount element.MountContext,
) (element.Runnable, error) {
	factory.mounts.Add(1)
	service, revision, found := mount.Services.Lookup("service.assembly")
	if !found || revision != 1 || service != factory.service {
		return nil, errors.New("assembly service identity drift")
	}
	optional, optionalRevision, optionalFound := mount.Services.Lookup("service.optional")
	if factory.optionalSelected {
		if !optionalFound || optionalRevision != 1 || optional != factory.optionalService {
			return nil, errors.New("selected optional assembly service identity drift")
		}
	} else if optionalFound {
		return nil, errors.New("unselected optional assembly service became ambient mount authority")
	}
	secretService, revision, found := mount.Services.Lookup(graphruntime.SecretServiceName)
	access, correctType := secretService.(graphruntime.SecretAccess)
	if !found || revision != 1 || !correctType {
		return nil, errors.New("exact node-scoped secret access is unavailable")
	}
	value, _, err := access.Resolve(ctx, "token")
	if err != nil {
		return nil, err
	}
	contents, err := value.Bytes()
	if err != nil {
		return nil, err
	}
	if string(contents) != assemblySecretValue {
		return nil, errors.New("secret value drift")
	}
	for index := range contents {
		contents[index] = 0
	}
	return element.RunnableFunc(func(runCtx context.Context) error {
		<-runCtx.Done()
		return context.Cause(runCtx)
	}), nil
}

func TestPreflightSealsExactGraphScopedAssemblyAndPreservesIdentities(t *testing.T) {
	fixture := newAssemblyFixture(t)
	prepared, err := graphassembly.Preflight(context.Background(), fixture.plan, fixture.assembly)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		t.Fatalf("preflight acquired resources: mounts=%d resolves=%d",
			fixture.mounts.Load(), fixture.resolves.Load())
	}
	if err := prepared.Validate(); err != nil {
		t.Fatalf("validate sealed plan: %v", err)
	}
	if prepared.Identity() != fixture.plan.Identity() {
		t.Fatalf("public identity = %+v, want %+v", prepared.Identity(), fixture.plan.Identity())
	}
	private := prepared.PrivateIdentity()
	if private.DeploymentFingerprint() != fixture.plan.DeploymentFingerprint() ||
		private.SecretCatalogFingerprint() != fixture.plan.SecretCatalogFingerprint() {
		t.Fatal("private prepared identity was not preserved")
	}
	if got := fmt.Sprintf("%v", private); got != "<redacted private plan identity>" {
		t.Fatalf("default private identity format = %q", got)
	}

	assertNoAssemblySecrets(t, mustJSON(t, prepared), fixture)
	assertNoAssemblySecrets(t, mustJSON(t, prepared.Graph()), fixture)
	binding, found := prepared.PrivateBinding("worker")
	if !found || binding.SecretReferences()["token"] != assemblySecretReference {
		t.Fatal("explicit private deployment binding is incomplete")
	}
	assertNoAssemblySecrets(t, mustJSON(t, binding), fixture)
	// The prepared handoff owns snapshots of every contribution. Mutating the
	// caller's assembly containers after successful preflight cannot redirect
	// the eventual acquisition.
	rotated := fixture.assembly.SecretCatalog.Secrets[assemblySecretReference]
	rotated.Locator = "POST_PREFLIGHT_REDIRECT"
	fixture.assembly.SecretCatalog.Secrets[assemblySecretReference] = rotated
	fixture.assembly.SecretProviders[0].Provider = nil
	fixture.assembly.Dependencies[1].Service = &assemblyService{name: "redirect"}
	fixture.assembly.Implementations[0].Factory = nil

	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	if fixture.mounts.Load() != 1 || fixture.resolves.Load() != 1 {
		t.Fatalf("mount acquisition calls: mounts=%d resolves=%d",
			fixture.mounts.Load(), fixture.resolves.Load())
	}
	if got := fixture.provider.locatorSnapshot(); !slices.Equal(got, []string{assemblySecretLocator}) {
		t.Fatalf("provider locators = %v", got)
	}
	deploymentIdentity, found := mounted.DeploymentIdentity()
	if !found || deploymentIdentity.ID != "deployment://assembly_launch" ||
		deploymentIdentity.Digest != fixture.plan.Identity().PublicDeploymentDigest {
		t.Fatalf("mounted deployment identity = %+v, found=%t", deploymentIdentity, found)
	}
	mountedPrivate, found := mounted.PrivateIdentity()
	if !found || mountedPrivate.DeploymentFingerprint() != fixture.plan.DeploymentFingerprint() ||
		mountedPrivate.SecretCatalogFingerprint() != fixture.plan.SecretCatalogFingerprint() {
		t.Fatal("mounted private identity was not preserved")
	}
	evidence, found := mounted.DeploymentEvidence()
	if !found || len(evidence.Secrets) != 1 ||
		evidence.PrivateDeploymentFingerprint != fixture.plan.DeploymentFingerprint() ||
		evidence.SecretCatalogFingerprint != fixture.plan.SecretCatalogFingerprint() ||
		evidence.Secrets[0].Provider != "env" ||
		evidence.Secrets[0].Runtime != fixture.assembly.SecretProviders[0].Artifact {
		t.Fatalf("mounted secret provider evidence = %+v, found=%t", evidence, found)
	}
	assertNoAssemblySecretMaterial(t, mustJSON(t, evidence))
}

func TestPreflightFreezesMountScopedDependencyWithoutAcquiringService(t *testing.T) {
	fixture := newAssemblyFixtureModeSelectionScope(
		t, true, false, graphconfig.DependencyScopeMount,
	)
	prepared, err := graphassembly.Preflight(context.Background(), fixture.plan, fixture.assembly)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		t.Fatalf("mount-scoped preflight acquired resources: mounts=%d resolves=%d",
			fixture.mounts.Load(), fixture.resolves.Load())
	}
	var artifact inspect.ArtifactIdentity
	found := false
	for _, dependency := range prepared.Public().Dependencies {
		if dependency.Name == "service.assembly" {
			artifact = dependency.Artifact
			found = dependency.Scope == graphconfig.DependencyScopeMount
		}
	}
	if !found {
		t.Fatalf("prepared dependencies = %+v", prepared.Public().Dependencies)
	}
	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{
		Dependencies: []graphruntime.PreparedMountDependency{{
			Name: "service.assembly", Artifact: artifact, Service: fixture.service,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.mounts.Load() != 1 || fixture.resolves.Load() != 1 {
		t.Fatalf("mount acquisition calls: mounts=%d resolves=%d",
			fixture.mounts.Load(), fixture.resolves.Load())
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchUsesOnlySealedGraphNativePlan(t *testing.T) {
	fixture := newAssemblyFixture(t)
	mounted, err := graphassembly.Launch(
		context.Background(), fixture.plan, fixture.assembly,
		graphruntime.PreparedMountOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.mounts.Load() != 1 || fixture.resolves.Load() != 1 {
		t.Fatalf("launch calls: mounts=%d resolves=%d",
			fixture.mounts.Load(), fixture.resolves.Load())
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogSelectsExplicitOptionalDependencyEndToEnd(t *testing.T) {
	fixture := newAssemblyFixtureModeSelection(t, true, true)
	resolution := fixture.plan.Resolution()
	if len(resolution.Dependencies) != 3 ||
		resolution.Dependencies[0].Name != graphruntime.SecretServiceName ||
		resolution.Dependencies[1].Name != "service.assembly" ||
		resolution.Dependencies[2].Name != "service.optional" {
		t.Fatalf("selected optional dependency resolution = %+v", resolution.Dependencies)
	}
	if len(fixture.assembly.Dependencies) != 3 ||
		fixture.assembly.Dependencies[2].Name != "service.optional" {
		t.Fatalf("selected optional assembly = %+v", fixture.assembly.Dependencies)
	}
	mounted, err := graphassembly.Launch(
		context.Background(), fixture.plan, fixture.assembly,
		graphruntime.PreparedMountOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.mounts.Load() != 1 || fixture.resolves.Load() != 1 {
		t.Fatalf("optional launch acquisition calls: mounts=%d resolves=%d",
			fixture.mounts.Load(), fixture.resolves.Load())
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogDiscoveryAndSelectionAreDeterministicAndGraphScoped(t *testing.T) {
	fixture := newAssemblyFixture(t)
	extraProfile := fixture.profile
	extraProfile.Reference = "impl/unused"
	extraProfile.Artifact = assemblyArtifact("runtime/unused", "1")
	extraService := &assemblyService{name: "unused"}
	catalog := graphassembly.Catalog{
		Implementations: append(slices.Clone(fixture.assembly.Implementations), graphruntime.FactoryRegistration{
			Profile: extraProfile,
			Factory: assemblyFactory{
				descriptor: fixture.descriptor, mounts: fixture.mounts, service: extraService,
			},
		}),
		Dependencies: append(slices.Clone(fixture.assembly.Dependencies), graphassembly.Dependency{
			Name: "service.unused", Artifact: assemblyArtifact("service/unused", "1"), Service: extraService,
		}),
		SecretProviders: append(slices.Clone(fixture.assembly.SecretProviders), graphassembly.SecretProvider{
			Name: "vault", Artifact: assemblyArtifact("secret/vault", "1"), Provider: fixture.provider,
		}),
	}
	discovery, err := catalog.Discovery()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := discovery.Snapshot()
	if len(snapshot.Implementations) != 2 ||
		snapshot.Implementations[0].Reference != "impl/assembly" ||
		snapshot.Implementations[1].Reference != "impl/unused" ||
		len(snapshot.Dependencies) != 3 ||
		snapshot.Dependencies[0].Name != graphruntime.SecretServiceName ||
		snapshot.Dependencies[1].Name != "service.assembly" ||
		snapshot.Dependencies[2].Name != "service.unused" {
		t.Fatalf("deterministic discovery snapshot = %+v", snapshot)
	}

	selected, err := catalog.Select(fixture.plan, fixture.assembly.SecretCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Implementations) != 1 ||
		selected.Implementations[0].Profile.Reference != fixture.profile.Reference ||
		len(selected.Dependencies) != 2 ||
		selected.Dependencies[0].Name != graphruntime.SecretServiceName ||
		selected.Dependencies[1].Name != "service.assembly" ||
		len(selected.SecretProviders) != 1 || selected.SecretProviders[0].Name != "env" {
		t.Fatalf("graph-scoped selection = %+v", selected)
	}
	// Catalog container/profile mutation cannot redirect a completed selection.
	catalog.Implementations[0].Profile.ResourceKeys[0] = "redirected"
	catalog.Dependencies[0].Service = extraService
	catalog.SecretProviders[0].Provider = nil
	if selected.Implementations[0].Profile.ResourceKeys[0] != "cpu" ||
		selected.Dependencies[1].Service != fixture.service || selected.SecretProviders[0].Provider == nil {
		t.Fatal("selected assembly aliases mutable catalog contribution containers")
	}
	if _, err := graphassembly.Preflight(context.Background(), fixture.plan, selected); err != nil {
		t.Fatalf("preflight selected catalog: %v", err)
	}
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		t.Fatal("catalog discovery/selection/preflight acquired runtime resources")
	}
}

func TestCatalogSelectionFailsClosedOnMissingAmbiguousAndDriftedContributions(t *testing.T) {
	fixture := newAssemblyFixture(t)
	base := graphassembly.Catalog{
		Implementations: slices.Clone(fixture.assembly.Implementations),
		Dependencies:    slices.Clone(fixture.assembly.Dependencies),
		SecretProviders: slices.Clone(fixture.assembly.SecretProviders),
	}
	tests := map[string]struct {
		mutate func(*graphassembly.Catalog)
		want   string
	}{
		"missing implementation": {
			mutate: func(value *graphassembly.Catalog) { value.Implementations = nil },
			want:   `implementation catalog is missing "impl/assembly"`,
		},
		"ambiguous implementation": {
			mutate: func(value *graphassembly.Catalog) {
				value.Implementations = append(value.Implementations, value.Implementations[0])
			},
			want: "already registered",
		},
		"drifted implementation": {
			mutate: func(value *graphassembly.Catalog) {
				value.Implementations[0].Profile.Artifact = assemblyArtifact("runtime/drift", "1")
			},
			want: "drifted from the frozen plan",
		},
		"missing dependency": {
			mutate: func(value *graphassembly.Catalog) { value.Dependencies = value.Dependencies[1:] },
			want:   "dependency catalog is missing",
		},
		"drifted dependency": {
			mutate: func(value *graphassembly.Catalog) {
				value.Dependencies[0].Artifact = assemblyArtifact("service/drift", "1")
			},
			want: "drifted from the frozen plan",
		},
		"drifted dependency scope": {
			mutate: func(value *graphassembly.Catalog) {
				for index := range value.Dependencies {
					if value.Dependencies[index].Name == "service.assembly" {
						value.Dependencies[index].Scope = graphconfig.DependencyScopeMount
						value.Dependencies[index].Service = nil
					}
				}
			},
			want: "drifted from the frozen plan",
		},
		"missing provider": {
			mutate: func(value *graphassembly.Catalog) { value.SecretProviders = nil },
			want:   `secret provider catalog is missing "env"`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Implementations = slices.Clone(base.Implementations)
			candidate.Dependencies = slices.Clone(base.Dependencies)
			candidate.SecretProviders = slices.Clone(base.SecretProviders)
			test.mutate(&candidate)
			if _, err := candidate.Select(fixture.plan, fixture.assembly.SecretCatalog); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("selection error = %v, want %q", err, test.want)
			}
			if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
				t.Fatal("rejected catalog selection acquired runtime resources")
			}
		})
	}
}

func TestStandardDependenciesPublishExactRuntimeOwnedIdentities(t *testing.T) {
	dependencies := graphassembly.StandardDependencies()
	wantNames := []string{
		graphruntime.ClockServiceName,
		graphruntime.SecretServiceName,
		graphruntime.SequenceServiceName,
	}
	if len(dependencies) != len(wantNames) {
		t.Fatalf("standard dependencies = %+v", dependencies)
	}
	for index, dependency := range dependencies {
		if dependency.Name != wantNames[index] ||
			dependency.Scope != graphconfig.DependencyScopeMount ||
			!dependency.RuntimeOwned || dependency.Service != nil {
			t.Fatalf("standard dependency %d = %+v", index, dependency)
		}
		if err := dependency.Artifact.Validate(); err != nil {
			t.Fatalf("standard dependency %s: %v", dependency.Name, err)
		}
		one, found := graphassembly.StandardDependency(dependency.Name)
		if !found || one != dependency {
			t.Fatalf("standard dependency lookup %s = %+v, found=%t", dependency.Name, one, found)
		}
	}
	if _, found := graphassembly.StandardDependency("runtime.unknown"); found {
		t.Fatal("unknown runtime service resolved to a standard dependency")
	}
	dependencies[0].Name = "mutated"
	again := graphassembly.StandardDependencies()
	if again[0].Name != graphruntime.ClockServiceName {
		t.Fatal("standard dependency inventory aliases caller mutation")
	}
	if _, err := (graphassembly.Catalog{Dependencies: again}).Discovery(); err != nil {
		t.Fatalf("standard dependency discovery: %v", err)
	}
}

func TestPreflightRejectsMissingExcessAmbiguousAndDriftedInventoryBeforeAcquisition(t *testing.T) {
	fixture := newAssemblyFixture(t)
	tests := []struct {
		name   string
		mutate func(*graphassembly.Assembly)
		want   string
	}{
		{
			name: "missing implementation",
			mutate: func(value *graphassembly.Assembly) {
				value.Implementations = nil
			},
			want: "implementation inventory is not exact: missing",
		},
		{
			name: "excess implementation",
			mutate: func(value *graphassembly.Assembly) {
				extra := value.Implementations[0]
				extra.Profile.Reference = "impl/excess"
				value.Implementations = append(value.Implementations, extra)
			},
			want: "implementation inventory is not exact: excess",
		},
		{
			name: "ambiguous implementation",
			mutate: func(value *graphassembly.Assembly) {
				value.Implementations = append(value.Implementations, value.Implementations[0])
			},
			want: "implementation inventory is ambiguous",
		},
		{
			name: "implementation profile drift",
			mutate: func(value *graphassembly.Assembly) {
				value.Implementations[0].Profile.Artifact = assemblyArtifact("runtime/drift", "1")
			},
			want: "drifted from the frozen resolution",
		},
		{
			name: "nil factory",
			mutate: func(value *graphassembly.Assembly) {
				value.Implementations[0].Factory = nil
			},
			want: "nil factory",
		},
		{
			name: "missing dependency",
			mutate: func(value *graphassembly.Assembly) {
				value.Dependencies = value.Dependencies[1:]
			},
			want: "dependency inventory is not exact: missing",
		},
		{
			name: "excess dependency",
			mutate: func(value *graphassembly.Assembly) {
				value.Dependencies = append(value.Dependencies, graphassembly.Dependency{
					Name: "service.excess", Artifact: assemblyArtifact("service/excess", "1"),
					Service: &assemblyService{name: "excess"},
				})
			},
			want: "dependency inventory is not exact: excess",
		},
		{
			name: "ambiguous dependency",
			mutate: func(value *graphassembly.Assembly) {
				value.Dependencies = append(value.Dependencies, value.Dependencies[0])
			},
			want: "dependency inventory is ambiguous",
		},
		{
			name: "dependency artifact drift",
			mutate: func(value *graphassembly.Assembly) {
				value.Dependencies[0].Artifact = assemblyArtifact("service/drift", "1")
			},
			want: "artifact drifted from the frozen resolution",
		},
		{
			name: "dependency scope drift",
			mutate: func(value *graphassembly.Assembly) {
				for index := range value.Dependencies {
					if value.Dependencies[index].Name == "service.assembly" {
						value.Dependencies[index].Scope = graphconfig.DependencyScopeMount
						value.Dependencies[index].Service = nil
					}
				}
			},
			want: "scope drifted from the frozen resolution",
		},
		{
			name: "nil dependency service",
			mutate: func(value *graphassembly.Assembly) {
				value.Dependencies[1].Service = nil
			},
			want: "nil service",
		},
		{
			name: "runtime dependency carries service",
			mutate: func(value *graphassembly.Assembly) {
				value.Dependencies[0].Service = &assemblyService{name: "spoof"}
			},
			want: "runtime-owned dependency cannot carry a service",
		},
		{
			name: "runtime ownership drift",
			mutate: func(value *graphassembly.Assembly) {
				value.Dependencies[0].RuntimeOwned = false
				value.Dependencies[0].Service = &assemblyService{name: "spoof"}
			},
			want: "use RegisterRuntimeOwned",
		},
		{
			name: "missing secret provider",
			mutate: func(value *graphassembly.Assembly) {
				value.SecretProviders = nil
			},
			want: "secret provider inventory is not exact: missing",
		},
		{
			name: "excess secret provider",
			mutate: func(value *graphassembly.Assembly) {
				value.SecretProviders = append(value.SecretProviders, graphassembly.SecretProvider{
					Name: "vault", Artifact: assemblyArtifact("secret/vault", "1"),
					Provider: value.SecretProviders[0].Provider,
				})
			},
			want: "secret provider inventory is not exact: excess",
		},
		{
			name: "ambiguous secret provider",
			mutate: func(value *graphassembly.Assembly) {
				value.SecretProviders = append(value.SecretProviders, value.SecretProviders[0])
			},
			want: "secret provider inventory is ambiguous",
		},
		{
			name: "nil secret provider",
			mutate: func(value *graphassembly.Assembly) {
				value.SecretProviders[0].Provider = nil
			},
			want: "nil provider",
		},
		{
			name: "secret catalog drift",
			mutate: func(value *graphassembly.Assembly) {
				binding := value.SecretCatalog.Secrets[assemblySecretReference]
				binding.Locator = "ROTATED_PRIVATE_LOCATOR"
				value.SecretCatalog.Secrets[assemblySecretReference] = binding
			},
			want: "secret catalog drifted",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneAssembly(fixture.assembly)
			test.mutate(&candidate)
			if _, err := graphassembly.Preflight(context.Background(), fixture.plan, candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("preflight error = %v, want substring %q", err, test.want)
			}
			if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
				t.Fatalf("rejected preflight acquired resources: mounts=%d resolves=%d",
					fixture.mounts.Load(), fixture.resolves.Load())
			}
		})
	}
}

func TestPreflightValidatesContextPlanAndCatalogPresenceWithoutAcquisition(t *testing.T) {
	fixture := newAssemblyFixture(t)
	cancelled, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("cancelled preflight"))
	tests := []struct {
		name     string
		ctx      context.Context
		plan     *graphconfig.Plan
		assembly graphassembly.Assembly
		want     string
	}{
		{name: "nil context", plan: fixture.plan, assembly: fixture.assembly, want: "nil context"},
		{name: "cancelled context", ctx: cancelled, plan: fixture.plan, assembly: fixture.assembly, want: "cancelled preflight"},
		{name: "nil plan", ctx: context.Background(), assembly: fixture.assembly, want: "nil plan"},
		{
			name: "missing secret catalog", ctx: context.Background(), plan: fixture.plan,
			assembly: func() graphassembly.Assembly {
				value := cloneAssembly(fixture.assembly)
				value.SecretCatalog = nil
				return value
			}(),
			want: "requires its exact graph-scoped secret catalog",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := graphassembly.Preflight(test.ctx, test.plan, test.assembly); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("preflight error = %v, want substring %q", err, test.want)
			}
		})
	}
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		t.Fatal("input validation acquired a runtime resource")
	}
}

func TestPreflightRejectsAmbientSecretCatalogAndProvidersForPlanWithoutSecrets(t *testing.T) {
	fixture := newAssemblyFixtureMode(t, false)
	if _, err := graphassembly.Preflight(context.Background(), fixture.plan, fixture.assembly); err != nil {
		t.Fatalf("preflight no-secret assembly: %v", err)
	}

	withCatalog := cloneAssembly(fixture.assembly)
	withCatalog.SecretCatalog = &graphsecret.Document{
		APIVersion: graphsecret.APIVersion,
		Catalog:    "ambient",
		Secrets:    map[string]graphsecret.Binding{},
	}
	if _, err := graphassembly.Preflight(context.Background(), fixture.plan, withCatalog); err == nil || !strings.Contains(err.Error(), "secret catalog is excess") {
		t.Fatalf("ambient catalog error = %v", err)
	}

	withProvider := cloneAssembly(fixture.assembly)
	withProvider.SecretProviders = []graphassembly.SecretProvider{{
		Name: "ambient", Artifact: assemblyArtifact("secret/ambient", "1"),
		Provider: fixture.provider,
	}}
	if _, err := graphassembly.Preflight(context.Background(), fixture.plan, withProvider); err == nil || !strings.Contains(err.Error(), "secret provider inventory is not exact: excess") {
		t.Fatalf("ambient provider error = %v", err)
	}
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		t.Fatal("ambient secret rejection acquired a runtime resource")
	}
}

func TestPreflightIsDeterministicAndConcurrentWithoutAcquisition(t *testing.T) {
	fixture := newAssemblyFixture(t)
	want, err := graphassembly.Preflight(context.Background(), fixture.plan, fixture.assembly)
	if err != nil {
		t.Fatal(err)
	}
	wantPublic := mustJSON(t, want.Public())

	const workers = 24
	const iterations = 20
	errorsChannel := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				prepared, prepareErr := graphassembly.Preflight(
					context.Background(), fixture.plan, fixture.assembly,
				)
				if prepareErr != nil {
					errorsChannel <- prepareErr
					return
				}
				if got := mustJSONNoTB(prepared.Public()); got != wantPublic {
					errorsChannel <- fmt.Errorf("public preparation drifted")
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		t.Fatalf("concurrent preflight acquired resources: mounts=%d resolves=%d",
			fixture.mounts.Load(), fixture.resolves.Load())
	}
}

func FuzzPreflightRejectsImplementationInventoryDriftBeforeAcquisition(f *testing.F) {
	fixture := newAssemblyFixture(f)
	f.Add("impl/other")
	f.Add(" impl/assembly")
	f.Add("impl/assembly\x00other")
	f.Fuzz(func(t *testing.T, reference string) {
		if reference == fixture.profile.Reference {
			reference += "/drift"
		}
		candidate := cloneAssembly(fixture.assembly)
		candidate.Implementations[0].Profile.Reference = reference
		if _, err := graphassembly.Preflight(context.Background(), fixture.plan, candidate); err == nil {
			t.Fatal("drifted implementation reference passed exact preflight")
		}
		if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
			t.Fatal("fuzzed rejected preflight acquired a runtime resource")
		}
	})
}

func BenchmarkPreflightExactAssembly(b *testing.B) {
	fixture := newAssemblyFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := graphassembly.Preflight(context.Background(), fixture.plan, fixture.assembly); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		b.Fatal("preflight benchmark acquired runtime resources")
	}
}

func BenchmarkCatalogSelectExactAssembly(b *testing.B) {
	fixture := newAssemblyFixture(b)
	catalog := graphassembly.Catalog{
		Implementations: slices.Clone(fixture.assembly.Implementations),
		Dependencies: append(
			[]graphassembly.Dependency{fixture.assembly.Dependencies[1]},
			graphassembly.StandardDependencies()...,
		),
		SecretProviders: slices.Clone(fixture.assembly.SecretProviders),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := catalog.Select(fixture.plan, fixture.assembly.SecretCatalog); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		b.Fatal("catalog selection benchmark acquired runtime resources")
	}
}

func BenchmarkCatalogDiscovery(b *testing.B) {
	fixture := newAssemblyFixture(b)
	catalog := graphassembly.Catalog{
		Implementations: slices.Clone(fixture.assembly.Implementations),
		Dependencies:    graphassembly.StandardDependencies(),
		SecretProviders: slices.Clone(fixture.assembly.SecretProviders),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := catalog.Discovery(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if fixture.mounts.Load() != 0 || fixture.resolves.Load() != 0 {
		b.Fatal("catalog discovery benchmark acquired runtime resources")
	}
}

func BenchmarkLaunchExactAssembly(b *testing.B) {
	fixture := newAssemblyFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		mounted, err := graphassembly.Launch(
			context.Background(), fixture.plan, fixture.assembly,
			graphruntime.PreparedMountOptions{},
		)
		if err != nil {
			b.Fatal(err)
		}
		if err := mounted.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func newAssemblyFixture(testingTB testing.TB) assemblyFixture {
	return newAssemblyFixtureMode(testingTB, true)
}

func newAssemblyFixtureMode(testingTB testing.TB, withSecrets bool) assemblyFixture {
	return newAssemblyFixtureModeSelection(testingTB, withSecrets, false)
}

func newAssemblyFixtureModeSelection(
	testingTB testing.TB,
	withSecrets bool,
	selectOptional bool,
) assemblyFixture {
	return newAssemblyFixtureModeSelectionScope(
		testingTB, withSecrets, selectOptional, graphconfig.DependencyScopeProcess,
	)
}

func newAssemblyFixtureModeSelectionScope(
	testingTB testing.TB,
	withSecrets bool,
	selectOptional bool,
	dependencyScope graphconfig.DependencyScope,
) assemblyFixture {
	testingTB.Helper()
	valueType := element.Event(element.Named("test.AssemblyValue"))
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Assembly",
		Revision:      1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction: element.Reaction{
			Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1,
		},
		Dependencies: []element.Dependency{
			{Name: "service.assembly"},
			{Name: "service.optional", Optional: true},
			{Name: graphruntime.SecretServiceName},
		},
	}
	descriptors := resolve.NewCatalog()
	if err := descriptors.Register(descriptor); err != nil {
		testingTB.Fatal(err)
	}
	implementationArtifact := assemblyArtifact("runtime/assembly", "1")
	dependencyArtifact := assemblyArtifact("service/assembly", "1")
	profile := graphruntime.FactoryProfile{
		Reference: "impl/assembly", Artifact: implementationArtifact,
		Placements: []string{"worker-pool"}, Transports: []string{"in-process"},
		ResourceKeys: []string{"cpu"}, SecretSlots: []string{"token"},
		Capabilities: []inspect.CapabilityIdentity{{
			Name: "assembly-capability", Contract: "Event<test.AssemblyValue>",
			Provider: assemblyArtifact("provider/assembly", "1"),
		}},
	}
	mounts := &atomic.Int64{}
	resolves := &atomic.Int64{}
	service := &assemblyService{name: "exact"}
	optionalService := &assemblyService{name: "optional"}
	provider := &assemblySecretProvider{resolves: resolves}
	primaryDependency := graphassembly.Dependency{
		Name: "service.assembly", Artifact: dependencyArtifact, Scope: dependencyScope,
	}
	if dependencyScope == graphconfig.DependencyScopeProcess {
		primaryDependency.Service = service
	}
	pluginCatalog := graphassembly.Catalog{
		Implementations: []graphruntime.FactoryRegistration{{
			Profile: profile,
			Factory: assemblyFactory{
				descriptor: descriptor, mounts: mounts, service: service,
				optionalService: optionalService, optionalSelected: selectOptional,
			},
		}},
		Dependencies: append([]graphassembly.Dependency{
			primaryDependency,
			{Name: "service.optional", Artifact: assemblyArtifact("service/optional", "1"),
				Scope: graphconfig.DependencyScopeProcess, Service: optionalService},
		}, graphassembly.StandardDependencies()...),
		SecretProviders: []graphassembly.SecretProvider{{
			Name: "env", Artifact: assemblyArtifact("secret/env", "1"), Provider: provider,
		}},
	}
	discovery, err := pluginCatalog.Discovery()
	if err != nil {
		testingTB.Fatalf("build fixture plugin discovery: %v", err)
	}
	secretCatalog := graphsecret.Document{
		APIVersion: graphsecret.APIVersion,
		Catalog:    "assembly_launch",
		Secrets: map[string]graphsecret.Binding{
			assemblySecretReference: {Provider: "env", Locator: assemblySecretLocator},
		},
	}
	options := graphconfig.Options{Catalog: descriptors, Discovery: discovery}
	if selectOptional {
		options.OptionalDependencies = []string{"service.optional"}
	}
	if withSecrets {
		options.SecretCatalog = &secretCatalog
	}
	topology := graphconfig.Artifact{
		Path: "agent.ortg", Encoding: graphconfig.ORTG, Data: []byte(assemblyTopology),
	}
	updated, err := graphconfig.UpdateLock(context.Background(), topology, graphconfig.Artifact{}, options)
	if err != nil {
		testingTB.Fatalf("update fixture lock: %v", err)
	}
	lock, err := updated.Lock().Marshal()
	if err != nil {
		testingTB.Fatal(err)
	}
	deploymentData := []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: assembly_launch
nodes:
  worker:
    implementation: impl/assembly
    placement: worker-pool
    transport: in-process
    resources:
      cpu: "2"
`)
	if withSecrets {
		deploymentData = []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: assembly_launch
nodes:
  worker:
    implementation: impl/assembly
    placement: worker-pool
    transport: in-process
    resources:
      cpu: "2"
    secrets:
      token: secret://assembly/worker/token
`)
	}
	plan, err := graphconfig.Create(context.Background(), graphconfig.Artifacts{
		Topology: topology,
		Values: graphconfig.Artifact{
			Path: "agent.values.yaml", Encoding: graphconfig.YAML,
			Data: []byte("apiVersion: openrealtime.ai/config/v1alpha1\ngraph: assembly_launch\nnodes: {}\n"),
		},
		Lock: graphconfig.Artifact{Path: "openrealtime.lock", Encoding: graphconfig.JSON, Data: lock},
		Deployment: graphconfig.Artifact{
			Path: "agent.deployment.yaml", Encoding: graphconfig.YAML,
			Data: deploymentData,
		},
	}, options)
	if err != nil {
		testingTB.Fatalf("create fixture plan: %v", err)
	}

	var selectedSecretCatalog *graphsecret.Document
	if withSecrets {
		selectedSecretCatalog = &secretCatalog
	}
	assembly, err := pluginCatalog.Select(plan, selectedSecretCatalog)
	if err != nil {
		testingTB.Fatalf("select fixture plugin assembly: %v", err)
	}
	return assemblyFixture{
		plan: plan, descriptor: descriptor, profile: profile,
		mounts: mounts, resolves: resolves, provider: provider, service: service,
		assembly: assembly,
	}
}

func cloneAssembly(source graphassembly.Assembly) graphassembly.Assembly {
	result := source
	result.Implementations = slices.Clone(source.Implementations)
	result.Dependencies = slices.Clone(source.Dependencies)
	result.SecretProviders = slices.Clone(source.SecretProviders)
	if source.SecretCatalog != nil {
		catalog := *source.SecretCatalog
		catalog.Secrets = make(map[string]graphsecret.Binding, len(source.SecretCatalog.Secrets))
		for reference, binding := range source.SecretCatalog.Secrets {
			catalog.Secrets[reference] = binding
		}
		result.SecretCatalog = &catalog
	}
	return result
}

func assemblyArtifact(id, revision string) inspect.ArtifactIdentity {
	digest := sha256.Sum256([]byte(id + "\x00" + revision))
	return inspect.ArtifactIdentity{
		ID: id, Revision: revision, Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func mustJSON(t testing.TB, value any) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func mustJSONNoTB(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return "marshal-error:" + err.Error()
	}
	return string(payload)
}

func assertNoAssemblySecrets(t testing.TB, payload string, fixture assemblyFixture) {
	t.Helper()
	for _, forbidden := range []string{
		assemblySecretReference,
		assemblySecretLocator,
		assemblySecretValue,
		fixture.plan.DeploymentFingerprint(),
		fixture.plan.SecretCatalogFingerprint(),
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("public payload contains private material %q: %s", forbidden, payload)
		}
	}
}

func assertNoAssemblySecretMaterial(t testing.TB, payload string) {
	t.Helper()
	for _, forbidden := range []string{
		assemblySecretReference,
		assemblySecretLocator,
		assemblySecretValue,
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("evidence payload contains secret material %q: %s", forbidden, payload)
		}
	}
}
