package runtime_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

const preparedTopology = `graph prepared_launch {
    test.Prepared :: worker;

    input input = worker.in;
    output output = worker.out;
}
`

const preparedSecretReference = "secret://providers/prepared/token"

type prepareFixture struct {
	plan               *graphconfig.Plan
	descriptor         element.Descriptor
	profile            graphruntime.FactoryProfile
	dependencyArtifact inspect.ArtifactIdentity
	dependencyName     string
	dependencyScope    graphconfig.DependencyScope
	secretCatalog      graphsecret.Document
	secretArtifact     inspect.ArtifactIdentity
	secretProvider     inspect.ArtifactIdentity
}

type preparedService struct{ name string }

type preparedSecretProvider struct {
	resolves *atomic.Int64
}

func (provider preparedSecretProvider) Resolve(ctx context.Context, _ string) ([]byte, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if provider.resolves != nil {
		provider.resolves.Add(1)
	}
	return []byte("prepared-private-value"), nil
}

type countedPreparedFactory struct {
	descriptor element.Descriptor
	mounts     *atomic.Int64
}

type dependencyCapturingPreparedFactory struct {
	countedPreparedFactory
	dependency string
	seen       chan<- string
}

type rejectingPreparedFactory struct {
	countedPreparedFactory
	validations *atomic.Int64
}

type mutatingPreparedFactory struct {
	countedPreparedFactory
	validations *atomic.Int64
	mu          sync.Mutex
	retained    json.RawMessage
}

func (factory rejectingPreparedFactory) ValidateConfig(json.RawMessage) error {
	if factory.validations != nil {
		factory.validations.Add(1)
	}
	return fmt.Errorf("resource-free validator refusal")
}

func (factory *mutatingPreparedFactory) ValidateConfig(value json.RawMessage) error {
	if factory.validations != nil {
		factory.validations.Add(1)
	}
	factory.mu.Lock()
	factory.retained = value
	if len(value) == 2 {
		copy(value, []byte("[]"))
	}
	factory.mu.Unlock()
	return nil
}

func (factory *mutatingPreparedFactory) Mount(
	ctx context.Context,
	mount element.MountContext,
) (element.Runnable, error) {
	if string(mount.Config) != "{}" {
		return nil, fmt.Errorf("mounted config = %s, want frozen empty object", mount.Config)
	}
	return factory.countedPreparedFactory.Mount(ctx, mount)
}

func (factory *mutatingPreparedFactory) mutateRetained() {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if len(factory.retained) == 2 {
		copy(factory.retained, []byte("42"))
	}
}

func (factory countedPreparedFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory countedPreparedFactory) Mount(
	context.Context,
	element.MountContext,
) (element.Runnable, error) {
	if factory.mounts != nil {
		factory.mounts.Add(1)
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}), nil
}

func (factory dependencyCapturingPreparedFactory) Mount(
	ctx context.Context,
	mount element.MountContext,
) (element.Runnable, error) {
	value, revision, found := mount.Services.Lookup(factory.dependency)
	service, correctType := value.(*preparedService)
	if !found || revision != 1 || !correctType {
		return nil, fmt.Errorf("mount dependency %s is unavailable", factory.dependency)
	}
	if factory.seen != nil {
		factory.seen <- service.name
	}
	return factory.countedPreparedFactory.Mount(ctx, mount)
}

type mutablePreparedFactory struct {
	mu         sync.RWMutex
	descriptor element.Descriptor
	mounts     *atomic.Int64
}

type reportingPreparedFactory struct {
	countedPreparedFactory
	capabilities []element.CapabilityResolution
}

func (factory reportingPreparedFactory) Mount(
	ctx context.Context,
	mount element.MountContext,
) (element.Runnable, error) {
	if factory.mounts != nil {
		factory.mounts.Add(1)
	}
	if mount.Resolution == nil {
		return nil, fmt.Errorf("missing resolution reporter")
	}
	if err := mount.Resolution.Capabilities(factory.capabilities); err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}), nil
}

func (factory *mutablePreparedFactory) Descriptor() element.Descriptor {
	factory.mu.RLock()
	defer factory.mu.RUnlock()
	return factory.descriptor.Clone()
}

func (factory *mutablePreparedFactory) Mount(
	context.Context,
	element.MountContext,
) (element.Runnable, error) {
	factory.mounts.Add(1)
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}), nil
}

func (factory *mutablePreparedFactory) mutate(descriptor element.Descriptor) {
	factory.mu.Lock()
	factory.descriptor = descriptor
	factory.mu.Unlock()
}

func TestPreparePlanSealsExactRuntimeAndMountsWithoutLegacyBinding(t *testing.T) {
	fixture := newPrepareFixture(t)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)

	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 0 {
		t.Fatal("preparation acquired a factory resource")
	}
	if err := prepared.Validate(); err != nil {
		t.Fatalf("validate sealed preparation: %v", err)
	}
	if prepared.Identity() != fixture.plan.Identity() ||
		prepared.Graph().Fingerprint != fixture.plan.Identity().GraphFingerprint {
		t.Fatalf("prepared identity = %+v", prepared.Identity())
	}
	public := prepared.Public()
	if len(public.Nodes) != 1 || public.Nodes[0].Reference != fixture.profile.Reference ||
		public.Nodes[0].Transport != "in-process" || public.Nodes[0].Placement != "worker-pool" ||
		!reflect.DeepEqual(public.Nodes[0].ResourceKeys, []string{"cpu"}) ||
		!reflect.DeepEqual(public.Nodes[0].SecretSlots, []string{"token"}) ||
		!preparedDependenciesContain(public.Dependencies, fixture.dependencyName) ||
		!preparedDependenciesContain(public.Dependencies, graphruntime.SecretServiceName) {
		t.Fatalf("public preparation = %+v", public)
	}

	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 1 {
		t.Fatalf("mount calls = %d, want 1", mounts.Load())
	}
	live := mounted.Live()
	if live.Configuration == nil || live.Configuration.Digest != fixture.plan.Identity().ValuesDigest ||
		live.Configuration.ID != "values://prepared_launch" {
		t.Fatalf("mounted configuration identity = %+v", live.Configuration)
	}
	resolution := live.Nodes["worker"].Resolution
	if resolution == nil || resolution.CapabilitiesEvidence != inspect.EvidenceRegistered ||
		!reflect.DeepEqual(resolution.Capabilities, fixture.profile.Capabilities) {
		t.Fatalf("mounted registered capabilities = %+v", resolution)
	}
	deploymentIdentity, found := mounted.DeploymentIdentity()
	if !found || deploymentIdentity.ID != "deployment://prepared_launch" ||
		deploymentIdentity.Digest != fixture.plan.Identity().PublicDeploymentDigest {
		t.Fatalf("mounted public deployment identity = %+v, found=%t", deploymentIdentity, found)
	}
	mountedPrivate, found := mounted.PrivateIdentity()
	if !found || mountedPrivate.DeploymentFingerprint() != fixture.plan.DeploymentFingerprint() ||
		mountedPrivate.SecretCatalogFingerprint() != fixture.plan.SecretCatalogFingerprint() {
		t.Fatalf("mounted private identity = %s, found=%t", mountedPrivate, found)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparePlanRunsConfigValidatorBeforeResourceAcquisition(t *testing.T) {
	fixture := newPrepareFixture(t)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	validations := &atomic.Int64{}
	resolves := &atomic.Int64{}

	registry := graphruntime.NewRegistry()
	if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
		Profile: fixture.profile,
		Factory: rejectingPreparedFactory{
			countedPreparedFactory: countedPreparedFactory{
				descriptor: fixture.descriptor,
				mounts:     mounts,
			},
			validations: validations,
		},
	}); err != nil {
		t.Fatal(err)
	}
	providers := graphsecret.NewRegistry()
	if err := providers.Register(
		"env",
		fixture.secretProvider,
		preparedSecretProvider{resolves: resolves},
	); err != nil {
		t.Fatal(err)
	}
	assembly.Factories = registry
	assembly.SecretProviders = providers

	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err == nil || !strings.Contains(err.Error(), "resource-free validator refusal") {
		t.Fatalf("PreparePlan error = %v, want config-validator refusal", err)
	}
	if prepared != nil {
		t.Fatal("PreparePlan returned a sealed plan after config-validator refusal")
	}
	if got := validations.Load(); got != 1 {
		t.Fatalf("config-validator calls = %d, want 1", got)
	}
	if got := mounts.Load(); got != 0 {
		t.Fatalf("factory mount calls = %d, want 0", got)
	}
	if got := resolves.Load(); got != 0 {
		t.Fatalf("secret-provider resolve calls = %d, want 0", got)
	}
}

func TestPreparePlanIsolatesConfigValidatorMutationAndRetention(t *testing.T) {
	fixture := newPrepareFixture(t)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	validations := &atomic.Int64{}
	factory := &mutatingPreparedFactory{
		countedPreparedFactory: countedPreparedFactory{
			descriptor: fixture.descriptor,
			mounts:     mounts,
		},
		validations: validations,
	}
	registry := graphruntime.NewRegistry()
	if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
		Profile: fixture.profile,
		Factory: factory,
	}); err != nil {
		t.Fatal(err)
	}
	assembly.Factories = registry

	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	if got := validations.Load(); got != 1 {
		t.Fatalf("config-validator calls after preparation = %d, want 1", got)
	}
	// Mutate the slice retained by implementation code after PreparePlan has
	// returned. Neither this delayed write nor the immediate write in
	// ValidateConfig may alias the sealed values snapshot.
	factory.mutateRetained()
	if err := prepared.Validate(); err != nil {
		t.Fatalf("validate preparation after retained config mutation: %v", err)
	}

	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err != nil {
		t.Fatalf("mount preparation after retained config mutation: %v", err)
	}
	if got := validations.Load(); got != 2 {
		t.Fatalf("config-validator calls after mount = %d, want 2", got)
	}
	if got := mounts.Load(); got != 1 {
		t.Fatalf("factory mount calls = %d, want 1", got)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedPlanSeparatesPublicAndPrivateDeploymentIdentity(t *testing.T) {
	fixture := newPrepareFixture(t)
	assembly, _ := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}

	private := prepared.PrivateIdentity()
	if private.DeploymentFingerprint() != fixture.plan.DeploymentFingerprint() ||
		private.SecretCatalogFingerprint() != fixture.plan.SecretCatalogFingerprint() {
		t.Fatalf("private identity did not survive preparation: %s", private)
	}
	binding, found := prepared.PrivateBinding("worker")
	if !found || binding.Resources()["cpu"] != "2" ||
		binding.SecretReferences()["token"] != preparedSecretReference {
		t.Fatalf("private binding = %s, found=%t", binding, found)
	}

	payload, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	privateJSON, err := json.Marshal(private)
	if err != nil {
		t.Fatal(err)
	}
	bindingJSON, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		preparedSecretReference,
		fixture.plan.DeploymentFingerprint(),
		fixture.plan.SecretCatalogFingerprint(),
		`"cpu":"2"`,
	} {
		if strings.Contains(string(payload), forbidden) || strings.Contains(string(privateJSON), forbidden) ||
			strings.Contains(string(bindingJSON), forbidden) {
			t.Fatalf("default projection leaked %q: public=%s private=%s binding=%s",
				forbidden, payload, privateJSON, bindingJSON)
		}
	}

	// Every public/private accessor is isolated from the sealed handoff.
	public := prepared.Public()
	public.Nodes[0].ResourceKeys[0] = "mutated"
	public.Nodes[0].Capabilities[0].Provider.ID = "mutated"
	resources := binding.Resources()
	resources["cpu"] = "999"
	secrets := binding.SecretReferences()
	secrets["token"] = "secret://mutated/value"
	again := prepared.Public()
	againBinding, _ := prepared.PrivateBinding("worker")
	if again.Nodes[0].ResourceKeys[0] != "cpu" ||
		again.Nodes[0].Capabilities[0].Provider.ID != fixture.profile.Capabilities[0].Provider.ID ||
		againBinding.Resources()["cpu"] != "2" ||
		againBinding.SecretReferences()["token"] != preparedSecretReference {
		t.Fatal("prepared accessors retained caller aliases")
	}
}

func TestPreparePlanFailsClosedBeforeMountOnResolutionDrift(t *testing.T) {
	fixture := newPrepareFixture(t)
	tests := map[string]func(*graphruntime.FactoryProfile, *inspect.ArtifactIdentity){
		"implementation artifact": func(profile *graphruntime.FactoryProfile, _ *inspect.ArtifactIdentity) {
			profile.Artifact = preparedArtifact("runtime/other", "2")
		},
		"capability": func(profile *graphruntime.FactoryProfile, _ *inspect.ArtifactIdentity) {
			profile.Capabilities[0].Provider = preparedArtifact("provider/other", "2")
		},
		"transport": func(profile *graphruntime.FactoryProfile, _ *inspect.ArtifactIdentity) {
			profile.Transports = []string{"sidecar-v4"}
		},
		"placement": func(profile *graphruntime.FactoryProfile, _ *inspect.ArtifactIdentity) {
			profile.Placements = []string{"other-pool"}
		},
		"resource surface": func(profile *graphruntime.FactoryProfile, _ *inspect.ArtifactIdentity) {
			profile.ResourceKeys = []string{"gpu"}
		},
		"secret surface": func(profile *graphruntime.FactoryProfile, _ *inspect.ArtifactIdentity) {
			profile.SecretSlots = []string{"other"}
		},
		"dependency artifact": func(_ *graphruntime.FactoryProfile, dependency *inspect.ArtifactIdentity) {
			*dependency = preparedArtifact("service/other", "2")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			profile := clonePreparedProfile(fixture.profile)
			dependency := fixture.dependencyArtifact
			mutate(&profile, &dependency)
			assembly, mounts := fixture.exactAssembly(t, profile, dependency)
			if _, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly); err == nil ||
				!strings.Contains(err.Error(), "drifted from the frozen resolution") {
				t.Fatalf("drift error = %v", err)
			}
			if mounts.Load() != 0 {
				t.Fatalf("drift mounted %d factories", mounts.Load())
			}
		})
	}
}

func TestPreparePlanRejectsIncompleteRegistrationsAndDescriptorMutation(t *testing.T) {
	fixture := newPrepareFixture(t)
	mounts := &atomic.Int64{}
	registry := graphruntime.NewRegistry()
	if err := registry.RegisterArtifact(
		fixture.profile.Reference,
		fixture.profile.Artifact,
		countedPreparedFactory{descriptor: fixture.descriptor, mounts: mounts},
	); err != nil {
		t.Fatal(err)
	}
	dependencies := graphruntime.NewDependencyRegistry()
	if err := dependencies.Register(
		fixture.dependencyName, fixture.dependencyArtifact, &preparedService{name: "prepared"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := graphruntime.PreparePlan(context.Background(), fixture.plan, graphruntime.PlanPreparation{
		Factories: registry, Dependencies: dependencies,
	}); err == nil || !strings.Contains(err.Error(), "no exact factory profile") {
		t.Fatalf("incomplete registration error = %v", err)
	}

	mutable := &mutablePreparedFactory{descriptor: fixture.descriptor, mounts: mounts}
	registry = graphruntime.NewRegistry()
	if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
		Profile: fixture.profile, Factory: mutable,
	}); err != nil {
		t.Fatal(err)
	}
	changed := fixture.descriptor.Clone()
	changed.Reaction.MaxConcurrency++
	mutable.mutate(changed)
	if _, err := graphruntime.PreparePlan(context.Background(), fixture.plan, graphruntime.PlanPreparation{
		Factories: registry, Dependencies: dependencies,
	}); err == nil || !strings.Contains(err.Error(), "descriptor mutated") {
		t.Fatalf("descriptor mutation error = %v", err)
	}
	if mounts.Load() != 0 {
		t.Fatalf("invalid registrations mounted %d factories", mounts.Load())
	}
}

func TestPreparedMountRechecksDescriptorBeforeAcquisitionAndLiveCapabilities(t *testing.T) {
	fixture := newPrepareFixture(t)
	baseAssembly, _ := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)

	t.Run("descriptor mutation", func(t *testing.T) {
		mounts := &atomic.Int64{}
		factory := &mutablePreparedFactory{descriptor: fixture.descriptor, mounts: mounts}
		registry := graphruntime.NewRegistry()
		if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
			Profile: fixture.profile, Factory: factory,
		}); err != nil {
			t.Fatal(err)
		}
		prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, graphruntime.PlanPreparation{
			Factories: registry, Dependencies: baseAssembly.Dependencies,
			SecretCatalog: baseAssembly.SecretCatalog, SecretProviders: baseAssembly.SecretProviders,
		})
		if err != nil {
			t.Fatal(err)
		}
		changed := fixture.descriptor.Clone()
		changed.Reaction.MaxConcurrency++
		factory.mutate(changed)
		if _, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{}); err == nil ||
			!strings.Contains(err.Error(), "descriptor mutated") {
			t.Fatalf("mount descriptor drift error = %v", err)
		}
		if mounts.Load() != 0 {
			t.Fatalf("descriptor drift acquired %d factory resources", mounts.Load())
		}
	})

	t.Run("live capability mismatch", func(t *testing.T) {
		mounts := &atomic.Int64{}
		registry := graphruntime.NewRegistry()
		factory := reportingPreparedFactory{
			countedPreparedFactory: countedPreparedFactory{descriptor: fixture.descriptor, mounts: mounts},
			capabilities: []element.CapabilityResolution{{
				Name: "prepared-capability", Contract: "Event<test.Value>",
				ProviderID:       "provider/different",
				ProviderRevision: "1",
				ProviderDigest:   preparedArtifact("provider/different", "1").Digest,
			}},
		}
		if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
			Profile: fixture.profile, Factory: factory,
		}); err != nil {
			t.Fatal(err)
		}
		prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, graphruntime.PlanPreparation{
			Factories: registry, Dependencies: baseAssembly.Dependencies,
			SecretCatalog: baseAssembly.SecretCatalog, SecretProviders: baseAssembly.SecretProviders,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{}); err == nil ||
			!strings.Contains(err.Error(), "live capability identities differ") {
			t.Fatalf("live capability drift error = %v", err)
		}
		if mounts.Load() != 1 {
			t.Fatalf("live handshake calls = %d, want 1", mounts.Load())
		}
	})
}

func TestPreparedMountAcceptsExactLiveCapabilitiesWhenProfileDefersSelection(t *testing.T) {
	fixture := newPrepareFixtureWithCapabilities(t, "service.prepared", nil)
	baseAssembly, _ := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	mounts := &atomic.Int64{}
	liveCapabilities := []element.CapabilityResolution{{
		Name: "selected-live", Contract: "Event<test.Value>",
		ProviderID:       "provider/live",
		ProviderRevision: "7",
		ProviderDigest:   preparedArtifact("provider/live", "7").Digest,
	}}
	registry := graphruntime.NewRegistry()
	if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
		Profile: fixture.profile,
		Factory: reportingPreparedFactory{
			countedPreparedFactory: countedPreparedFactory{descriptor: fixture.descriptor, mounts: mounts},
			capabilities:           liveCapabilities,
		},
	}); err != nil {
		t.Fatal(err)
	}
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, graphruntime.PlanPreparation{
		Factories: registry, Dependencies: baseAssembly.Dependencies,
		SecretCatalog: baseAssembly.SecretCatalog, SecretProviders: baseAssembly.SecretProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	if capabilities := prepared.Public().Nodes[0].Capabilities; capabilities != nil {
		t.Fatalf("deferred capability profile became registered evidence: %+v", capabilities)
	}
	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resolution := mounted.Live().Nodes["worker"].Resolution
	if resolution == nil || resolution.CapabilitiesEvidence != inspect.EvidenceLive ||
		len(resolution.Capabilities) != 1 || resolution.Capabilities[0].Name != "selected-live" ||
		resolution.Capabilities[0].Provider.ID != "provider/live" {
		t.Fatalf("live capability resolution = %+v", resolution)
	}
	if mounts.Load() != 1 {
		t.Fatalf("live capability handshake calls = %d, want 1", mounts.Load())
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparePlanValidatesInputsAndMissingDependencies(t *testing.T) {
	fixture := newPrepareFixture(t)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		plan     *graphconfig.Plan
		assembly graphruntime.PlanPreparation
		want     string
	}{
		{name: "nil context", plan: fixture.plan, assembly: assembly, want: "nil context"},
		{name: "cancelled", ctx: cancelled, plan: fixture.plan, assembly: assembly, want: "context canceled"},
		{name: "nil plan", ctx: context.Background(), assembly: assembly, want: "nil plan"},
		{name: "nil factories", ctx: context.Background(), plan: fixture.plan, want: "factory registry"},
		{name: "missing dependency", ctx: context.Background(), plan: fixture.plan,
			assembly: graphruntime.PlanPreparation{Factories: assembly.Factories}, want: "not registered"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := graphruntime.PreparePlan(test.ctx, test.plan, test.assembly)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if mounts.Load() != 0 {
		t.Fatalf("invalid preparations mounted %d factories", mounts.Load())
	}
}

func TestPreparedPlanIsDeterministicAcrossRepeatedAndConcurrentPreparation(t *testing.T) {
	fixture := newPrepareFixture(t)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	want, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 100; iteration++ {
		got, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
		if err != nil {
			t.Fatalf("repeat %d: %v", iteration, err)
		}
		gotJSON, _ := json.Marshal(got)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("repeat %d differs:\n%s\n%s", iteration, gotJSON, wantJSON)
		}
	}

	const workers = 16
	const iterations = 16
	failures := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				got, prepareErr := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
				if prepareErr != nil {
					failures <- prepareErr
					return
				}
				gotJSON, marshalErr := json.Marshal(got)
				if marshalErr != nil || string(gotJSON) != string(wantJSON) {
					failures <- fmt.Errorf("nondeterministic concurrent preparation: marshal=%v", marshalErr)
					return
				}
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	if mounts.Load() != 0 {
		t.Fatalf("resource-free repeated preparation mounted %d factories", mounts.Load())
	}
}

func TestRegisterFactoryRejectsNonCanonicalOrUnboundedProfiles(t *testing.T) {
	fixture := newPrepareFixture(t)
	factory := countedPreparedFactory{descriptor: fixture.descriptor}
	tests := map[string]graphruntime.FactoryProfile{
		"reference whitespace": func() graphruntime.FactoryProfile {
			profile := clonePreparedProfile(fixture.profile)
			profile.Reference += " "
			return profile
		}(),
		"missing transport": func() graphruntime.FactoryProfile {
			profile := clonePreparedProfile(fixture.profile)
			profile.Transports = nil
			return profile
		}(),
		"too many resources": func() graphruntime.FactoryProfile {
			profile := clonePreparedProfile(fixture.profile)
			profile.ResourceKeys = make([]string, 4_097)
			for index := range profile.ResourceKeys {
				profile.ResourceKeys[index] = fmt.Sprintf("resource-%04d", index)
			}
			return profile
		}(),
	}
	for name, profile := range tests {
		t.Run(name, func(t *testing.T) {
			if err := graphruntime.NewRegistry().RegisterFactory(graphruntime.FactoryRegistration{
				Profile: profile, Factory: factory,
			}); err == nil {
				t.Fatal("expected invalid profile to fail")
			}
		})
	}
}

func TestDependencyRegistrySeparatesRuntimeOwnedCoeffects(t *testing.T) {
	registry := graphruntime.NewDependencyRegistry()
	artifact := preparedArtifact("runtime/clock", "1")
	if err := registry.Register(
		graphruntime.ClockServiceName, artifact, &preparedService{name: "shadow"},
	); err == nil || !strings.Contains(err.Error(), "RegisterRuntimeOwned") {
		t.Fatalf("runtime-owned shadow registration error = %v", err)
	}
	if err := registry.RegisterRuntimeOwned(graphruntime.ClockServiceName, artifact); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterRuntimeOwned("service.external", artifact); err == nil ||
		!strings.Contains(err.Error(), "not runtime-owned") {
		t.Fatalf("external runtime-owned registration error = %v", err)
	}
}

func TestPreparePlanAttestsRuntimeOwnedDependencyWithoutShadowService(t *testing.T) {
	fixture := newPrepareFixtureForDependency(t, graphruntime.ClockServiceName)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 1 {
		t.Fatalf("runtime-owned dependency mount calls = %d", mounts.Load())
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedMountRequiresExactMountScopedDependencyOverlayBeforeAcquisition(t *testing.T) {
	fixture := newPrepareFixtureForDependencyScope(
		t, "service.prepared", graphconfig.DependencyScopeMount,
	)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 0 {
		t.Fatal("mount-scoped dependency preflight acquired a factory resource")
	}
	dependencies := prepared.Public().Dependencies
	if !preparedDependenciesContainScope(
		dependencies, fixture.dependencyName, graphconfig.DependencyScopeMount,
	) {
		t.Fatalf("prepared dependencies = %+v", dependencies)
	}

	exact := graphruntime.PreparedMountDependency{
		Name: fixture.dependencyName, Artifact: fixture.dependencyArtifact,
		Service: &preparedService{name: "exact"},
	}
	tests := []struct {
		name         string
		dependencies []graphruntime.PreparedMountDependency
		want         string
	}{
		{name: "missing", want: "missing [service.prepared]"},
		{
			name: "excess",
			dependencies: []graphruntime.PreparedMountDependency{
				exact,
				{Name: "service.excess", Artifact: preparedArtifact("service/excess", "1"), Service: &preparedService{name: "excess"}},
			},
			want: "excess [service.excess]",
		},
		{
			name:         "duplicate",
			dependencies: []graphruntime.PreparedMountDependency{exact, exact},
			want:         "ambiguous",
		},
		{
			name: "artifact drift",
			dependencies: []graphruntime.PreparedMountDependency{{
				Name: fixture.dependencyName, Artifact: preparedArtifact("service/drift", "1"),
				Service: exact.Service,
			}},
			want: "artifact drifted",
		},
		{
			name: "nil service",
			dependencies: []graphruntime.PreparedMountDependency{{
				Name: fixture.dependencyName, Artifact: fixture.dependencyArtifact,
			}},
			want: "nil service",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{
				Dependencies: test.dependencies,
			}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("mount overlay error = %v, want %q", err, test.want)
			}
			if mounts.Load() != 0 {
				t.Fatalf("rejected overlay mounted %d factories", mounts.Load())
			}
		})
	}

	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{
		Dependencies: []graphruntime.PreparedMountDependency{exact},
	})
	if err != nil {
		t.Fatalf("exact mount overlay: %v", err)
	}
	if mounts.Load() != 1 {
		t.Fatalf("exact overlay mounted %d factories, want 1", mounts.Load())
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedMountOverlayCannotShadowProcessDependency(t *testing.T) {
	fixture := newPrepareFixture(t)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{
		Dependencies: []graphruntime.PreparedMountDependency{{
			Name: fixture.dependencyName, Artifact: fixture.dependencyArtifact,
			Service: &preparedService{name: "shadow"},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot shadow a process-scoped dependency") {
		t.Fatalf("process shadow error = %v", err)
	}
	if mounts.Load() != 0 {
		t.Fatalf("process shadow mounted %d factories", mounts.Load())
	}
}

func TestPreparedPlanConcurrentMountsInjectDistinctMountServices(t *testing.T) {
	fixture := newPrepareFixtureForDependencyScope(
		t, "service.prepared", graphconfig.DependencyScopeMount,
	)
	assembly, mounts := fixture.exactAssembly(t, fixture.profile, fixture.dependencyArtifact)
	seen := make(chan string, 2)
	registry := graphruntime.NewRegistry()
	if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
		Profile: fixture.profile,
		Factory: dependencyCapturingPreparedFactory{
			countedPreparedFactory: countedPreparedFactory{
				descriptor: fixture.descriptor, mounts: mounts,
			},
			dependency: fixture.dependencyName, seen: seen,
		},
	}); err != nil {
		t.Fatal(err)
	}
	assembly.Factories = registry
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		mounted *graphruntime.Mounted
		err     error
	}
	results := make(chan result, 2)
	for _, name := range []string{"first", "second"} {
		service := &preparedService{name: name}
		go func() {
			mounted, mountErr := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{
				Dependencies: []graphruntime.PreparedMountDependency{{
					Name: fixture.dependencyName, Artifact: fixture.dependencyArtifact, Service: service,
				}},
			})
			results <- result{mounted: mounted, err: mountErr}
		}()
	}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if err := result.mounted.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(seen)
	got := make([]string, 0, 2)
	for name := range seen {
		got = append(got, name)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("per-mount services = %v", got)
	}
	if mounts.Load() != 2 {
		t.Fatalf("concurrent mounts = %d, want 2", mounts.Load())
	}
}

func FuzzPreparePlanRejectsExactProfileDrift(f *testing.F) {
	fixture := newPrepareFixture(f)
	f.Add(uint8(0), []byte("same"))
	f.Add(uint8(1), []byte("artifact"))
	f.Add(uint8(2), []byte("capability"))
	f.Add(uint8(3), []byte("transport"))
	f.Fuzz(func(t *testing.T, selector uint8, entropy []byte) {
		profile := clonePreparedProfile(fixture.profile)
		drifted := selector%6 != 0
		switch selector % 6 {
		case 0:
		case 1:
			profile.Artifact = fuzzPreparedArtifact("runtime/fuzz", entropy)
		case 2:
			profile.Capabilities[0].Provider = fuzzPreparedArtifact("provider/fuzz", entropy)
		case 3:
			profile.Transports = []string{"sidecar-v4"}
		case 4:
			profile.ResourceKeys = []string{"gpu"}
		case 5:
			profile.SecretSlots = []string{"credential"}
		}
		assembly, mounts := fixture.exactAssembly(t, profile, fixture.dependencyArtifact)
		_, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
		if drifted && err == nil {
			t.Fatal("drifted profile was accepted")
		}
		if !drifted && err != nil {
			t.Fatalf("exact profile was rejected: %v", err)
		}
		if mounts.Load() != 0 {
			t.Fatalf("fuzz preparation mounted %d factories", mounts.Load())
		}
	})
}

func BenchmarkPreparePlanExactRuntimeResolution(b *testing.B) {
	fixture := newPrepareFixture(b)
	assembly, mounts := fixture.exactAssembly(b, fixture.profile, fixture.dependencyArtifact)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if mounts.Load() != 0 {
		b.Fatalf("benchmark preparation mounted %d factories", mounts.Load())
	}
}

func BenchmarkPreparedPlanVerifySealedHandoff(b *testing.B) {
	fixture := newPrepareFixture(b)
	assembly, mounts := fixture.exactAssembly(b, fixture.profile, fixture.dependencyArtifact)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if err := prepared.Validate(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if mounts.Load() != 0 {
		b.Fatalf("benchmark validation mounted %d factories", mounts.Load())
	}
}

func newPrepareFixture(testingTB testing.TB) prepareFixture {
	return newPrepareFixtureForDependency(testingTB, "service.prepared")
}

func newPrepareFixtureForDependency(testingTB testing.TB, dependencyName string) prepareFixture {
	scope := graphconfig.DependencyScopeProcess
	if dependencyName == graphruntime.ClockServiceName ||
		dependencyName == graphruntime.SequenceServiceName ||
		dependencyName == graphruntime.SecretServiceName {
		scope = graphconfig.DependencyScopeMount
	}
	return newPrepareFixtureForDependencyScope(testingTB, dependencyName, scope)
}

func newPrepareFixtureForDependencyScope(
	testingTB testing.TB,
	dependencyName string,
	scope graphconfig.DependencyScope,
) prepareFixture {
	capability := inspect.CapabilityIdentity{
		Name: "prepared-capability", Contract: "Event<test.Value>",
		Provider: preparedArtifact("provider/prepared", "1"),
	}
	return newPrepareFixtureWithCapabilitiesAndScope(
		testingTB, dependencyName, []inspect.CapabilityIdentity{capability}, scope,
	)
}

func newPrepareFixtureWithCapabilities(
	testingTB testing.TB,
	dependencyName string,
	capabilities []inspect.CapabilityIdentity,
) prepareFixture {
	return newPrepareFixtureWithCapabilitiesAndScope(
		testingTB, dependencyName, capabilities, graphconfig.DependencyScopeProcess,
	)
}

func newPrepareFixtureWithCapabilitiesAndScope(
	testingTB testing.TB,
	dependencyName string,
	capabilities []inspect.CapabilityIdentity,
	dependencyScope graphconfig.DependencyScope,
) prepareFixture {
	testingTB.Helper()
	descriptor := preparedDescriptor(dependencyName)
	identity, err := descriptor.Identity()
	if err != nil {
		testingTB.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := catalog.Register(descriptor); err != nil {
		testingTB.Fatal(err)
	}
	implementationArtifact := preparedArtifact("runtime/prepared", "1")
	dependencyArtifact := preparedArtifact("service/prepared", "1")
	secretArtifact := preparedArtifact("runtime/secrets", "1")
	secretProvider := preparedArtifact("secret-provider/env", "1")
	profile := graphruntime.FactoryProfile{
		Reference: "impl/prepared", Artifact: implementationArtifact,
		Placements: []string{"worker-pool"}, Transports: []string{"in-process"},
		ResourceKeys: []string{"cpu"}, SecretSlots: []string{"token"},
		Capabilities: capabilities,
	}
	discovery := graphconfig.NewStaticDiscovery()
	if err := discovery.RegisterImplementation(graphconfig.ImplementationResolution{
		Reference: profile.Reference, Contract: identity, Artifact: profile.Artifact,
		Evidence: inspect.EvidenceRegistered, Placements: profile.Placements,
		Transports: profile.Transports, ResourceKeys: profile.ResourceKeys,
		SecretSlots: profile.SecretSlots, Capabilities: profile.Capabilities,
	}); err != nil {
		testingTB.Fatal(err)
	}
	if err := discovery.RegisterDependency(graphconfig.DependencyResolution{
		Name: dependencyName, Artifact: dependencyArtifact, Scope: dependencyScope,
	}); err != nil {
		testingTB.Fatal(err)
	}
	if dependencyName != graphruntime.SecretServiceName {
		if err := discovery.RegisterDependency(graphconfig.DependencyResolution{
			Name: graphruntime.SecretServiceName, Artifact: secretArtifact,
			Scope: graphconfig.DependencyScopeMount,
		}); err != nil {
			testingTB.Fatal(err)
		}
	}
	secretCatalog := graphsecret.Document{
		APIVersion: graphsecret.APIVersion, Catalog: "prepared_launch",
		Secrets: map[string]graphsecret.Binding{
			preparedSecretReference: {Provider: "env", Locator: "PREPARED_TOKEN"},
		},
	}
	options := graphconfig.Options{
		Catalog: catalog, Discovery: discovery,
		SecretCatalog: &secretCatalog,
	}
	topology := graphconfig.Artifact{Path: "agent.ortg", Data: []byte(preparedTopology)}
	updated, err := graphconfig.UpdateLock(context.Background(), topology, graphconfig.Artifact{}, options)
	if err != nil {
		testingTB.Fatalf("update preparation fixture lock: %v", err)
	}
	lock, err := updated.Lock().Marshal()
	if err != nil {
		testingTB.Fatal(err)
	}
	plan, err := graphconfig.Create(context.Background(), graphconfig.Artifacts{
		Topology: topology,
		Values: graphconfig.Artifact{Path: "agent.values.yaml", Data: []byte(`apiVersion: openrealtime.ai/config/v1alpha1
graph: prepared_launch
nodes: {}
`)},
		Lock: graphconfig.Artifact{Path: "openrealtime.lock", Data: lock},
		Deployment: graphconfig.Artifact{Path: "agent.deployment.yaml", Data: []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: prepared_launch
nodes:
  worker:
    implementation: impl/prepared
    placement: worker-pool
    transport: in-process
    resources:
      cpu: "2"
    secrets:
      token: secret://providers/prepared/token
`)},
	}, options)
	if err != nil {
		testingTB.Fatalf("create preparation fixture: %v", err)
	}
	return prepareFixture{
		plan: plan, descriptor: descriptor, profile: profile, dependencyArtifact: dependencyArtifact,
		dependencyName: dependencyName, dependencyScope: dependencyScope, secretCatalog: secretCatalog,
		secretArtifact: secretArtifact, secretProvider: secretProvider,
	}
}

func preparedDescriptor(dependencyName string) element.Descriptor {
	value := element.Event(element.Named("test.Value"))
	dependencies := []element.Dependency{{Name: dependencyName}}
	if dependencyName != graphruntime.SecretServiceName {
		dependencies = append(dependencies, element.Dependency{Name: graphruntime.SecretServiceName})
	}
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Prepared", Revision: 1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction:     element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1},
		Dependencies: dependencies,
	}
}

func (fixture prepareFixture) exactAssembly(
	testingTB testing.TB,
	profile graphruntime.FactoryProfile,
	dependencyArtifact inspect.ArtifactIdentity,
) (graphruntime.PlanPreparation, *atomic.Int64) {
	testingTB.Helper()
	mounts := &atomic.Int64{}
	registry := graphruntime.NewRegistry()
	if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
		Profile: profile,
		Factory: countedPreparedFactory{descriptor: fixture.descriptor, mounts: mounts},
	}); err != nil {
		testingTB.Fatal(err)
	}
	dependencies := graphruntime.NewDependencyRegistry()
	if fixture.dependencyName == graphruntime.ClockServiceName ||
		fixture.dependencyName == graphruntime.SequenceServiceName ||
		fixture.dependencyName == graphruntime.SecretServiceName {
		if err := dependencies.RegisterRuntimeOwned(fixture.dependencyName, dependencyArtifact); err != nil {
			testingTB.Fatal(err)
		}
	} else if fixture.dependencyScope == graphconfig.DependencyScopeMount {
		if err := dependencies.RegisterMountScoped(fixture.dependencyName, dependencyArtifact); err != nil {
			testingTB.Fatal(err)
		}
	} else {
		if err := dependencies.Register(
			fixture.dependencyName, dependencyArtifact, &preparedService{name: "prepared"},
		); err != nil {
			testingTB.Fatal(err)
		}
	}
	if fixture.dependencyName != graphruntime.SecretServiceName {
		if err := dependencies.RegisterRuntimeOwned(
			graphruntime.SecretServiceName,
			fixture.secretArtifact,
		); err != nil {
			testingTB.Fatal(err)
		}
	}
	providers := graphsecret.NewRegistry()
	if err := providers.Register("env", fixture.secretProvider, preparedSecretProvider{}); err != nil {
		testingTB.Fatal(err)
	}
	return graphruntime.PlanPreparation{
		Factories: registry, Dependencies: dependencies,
		SecretCatalog: &fixture.secretCatalog, SecretProviders: providers,
	}, mounts
}

func preparedDependenciesContain(dependencies []graphruntime.PreparedDependency, name string) bool {
	for _, dependency := range dependencies {
		if dependency.Name == name {
			return true
		}
	}
	return false
}

func preparedDependenciesContainScope(
	dependencies []graphruntime.PreparedDependency,
	name string,
	scope graphconfig.DependencyScope,
) bool {
	for _, dependency := range dependencies {
		if dependency.Name == name && dependency.Scope == scope {
			return true
		}
	}
	return false
}

func preparedArtifact(id, revision string) inspect.ArtifactIdentity {
	digest := sha256.Sum256([]byte(id + "\x00" + revision))
	return inspect.ArtifactIdentity{
		ID: id, Revision: revision, Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func fuzzPreparedArtifact(id string, entropy []byte) inspect.ArtifactIdentity {
	digest := sha256.Sum256(entropy)
	return inspect.ArtifactIdentity{
		ID: id, Revision: "fuzz:1", Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func clonePreparedProfile(source graphruntime.FactoryProfile) graphruntime.FactoryProfile {
	payload, err := json.Marshal(source)
	if err != nil {
		panic(err)
	}
	var result graphruntime.FactoryProfile
	if err := json.Unmarshal(payload, &result); err != nil {
		panic(err)
	}
	return result
}
