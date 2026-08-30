package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

const (
	preparedSecretLocator = "PRIVATE_PREPARED_LOCATOR"
	preparedSecretValue   = "private-prepared-credential"
)

type trackingPreparedSecretProvider struct {
	mu       sync.Mutex
	raw      [][]byte
	locators []string
	failure  error
	started  chan struct{}
	release  <-chan struct{}
	once     sync.Once
}

func (provider *trackingPreparedSecretProvider) Resolve(
	ctx context.Context,
	locator string,
) ([]byte, error) {
	provider.mu.Lock()
	provider.locators = append(provider.locators, locator)
	provider.mu.Unlock()
	if provider.started != nil {
		provider.once.Do(func() { close(provider.started) })
	}
	if provider.release != nil {
		select {
		case <-provider.release:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	if provider.failure != nil {
		return nil, provider.failure
	}
	raw := []byte(preparedSecretValue)
	provider.mu.Lock()
	provider.raw = append(provider.raw, raw)
	provider.mu.Unlock()
	return raw, nil
}

func (provider *trackingPreparedSecretProvider) providerBuffersErased() bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.raw) == 0 {
		return false
	}
	for _, raw := range provider.raw {
		for _, value := range raw {
			if value != 0 {
				return false
			}
		}
	}
	return true
}

type secretPreparedFactory struct {
	descriptor       element.Descriptor
	profile          graphruntime.FactoryProfile
	mounts           *atomic.Int64
	resolveMount     bool
	failAfterResolve bool
	access           chan<- graphruntime.SecretAccess
	handles          *[]*graphsecret.Value
}

func (factory secretPreparedFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory secretPreparedFactory) Mount(
	ctx context.Context,
	mount element.MountContext,
) (element.Runnable, error) {
	if factory.mounts != nil {
		factory.mounts.Add(1)
	}
	service, revision, found := mount.Services.Lookup(graphruntime.SecretServiceName)
	access, correct := service.(graphruntime.SecretAccess)
	if !found || revision != 1 || !correct {
		return nil, errors.New("missing exact node-scoped secret service")
	}
	if factory.access != nil {
		factory.access <- access
	}
	if factory.resolveMount {
		value, _, err := access.Resolve(ctx, "token")
		if err != nil {
			return nil, err
		}
		if factory.handles != nil {
			*factory.handles = append(*factory.handles, value)
		}
		contents, err := value.Bytes()
		if err != nil {
			return nil, err
		}
		if string(contents) != preparedSecretValue {
			return nil, errors.New("resolved unexpected secret value")
		}
		for index := range contents {
			contents[index] = 0
		}
		if factory.failAfterResolve {
			return nil, errors.New("intentional mount abort")
		}
	}
	if mount.Resolution != nil {
		if err := mount.Resolution.Capabilities(preparedLiveCapabilities(factory.profile)); err != nil {
			return nil, err
		}
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}), nil
}

func preparedLiveCapabilities(profile graphruntime.FactoryProfile) []element.CapabilityResolution {
	result := make([]element.CapabilityResolution, len(profile.Capabilities))
	for index, capability := range profile.Capabilities {
		result[index] = element.CapabilityResolution{
			Name: capability.Name, Contract: capability.Contract,
			ProviderID: capability.Provider.ID, ProviderRevision: capability.Provider.Revision,
			ProviderDigest: capability.Provider.Digest,
		}
		if capability.Adapter != nil {
			result[index].AdapterID = capability.Adapter.ID
			result[index].AdapterRevision = capability.Adapter.Revision
			result[index].AdapterDigest = capability.Adapter.Digest
		}
	}
	return result
}

func secretPreparedAssembly(
	t *testing.T,
	fixture prepareFixture,
	factory element.Factory,
	provider graphsecret.Provider,
) graphruntime.PlanPreparation {
	t.Helper()
	return secretPreparedAssemblyForTB(t, fixture, factory, provider)
}

func TestPreparedSecretResolutionCarriesExactRedactedLiveTraceAndReplayEvidence(t *testing.T) {
	fixture := newPrepareFixture(t)
	provider := &trackingPreparedSecretProvider{}
	var handles []*graphsecret.Value
	mounts := &atomic.Int64{}
	factory := secretPreparedFactory{
		descriptor: fixture.descriptor, profile: fixture.profile, mounts: mounts,
		resolveMount: true, handles: &handles,
	}
	assembly := secretPreparedAssembly(t, fixture, factory, provider)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 0 || len(provider.raw) != 0 {
		t.Fatal("preparation resolved a provider or mounted a factory")
	}
	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{
		TraceRecording: &graphruntime.TraceRecordingConfig{
			SessionCorrelationKey: bytes.Repeat([]byte{0x5a}, 32),
			CaptureInterval:       time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mounts.Load() != 1 || len(handles) != 1 || !provider.providerBuffersErased() {
		t.Fatalf("mounts=%d handles=%d provider-erased=%t",
			mounts.Load(), len(handles), provider.providerBuffersErased())
	}

	live := mounted.Live()
	if live.Deployment == nil {
		t.Fatal("live inspection omitted deployment evidence")
	}
	wantPublic, found := mounted.DeploymentIdentity()
	if !found || live.Deployment.Public != wantPublic ||
		live.Deployment.PrivateDeploymentFingerprint != fixture.plan.DeploymentFingerprint() ||
		live.Deployment.SecretCatalogFingerprint != fixture.plan.SecretCatalogFingerprint() ||
		len(live.Deployment.Secrets) != 1 ||
		live.Deployment.Secrets[0].Node != "worker" || live.Deployment.Secrets[0].Slot != "token" ||
		live.Deployment.Secrets[0].Provider != "env" ||
		live.Deployment.Secrets[0].Runtime != fixture.secretProvider {
		t.Fatalf("live deployment evidence = %+v", live.Deployment)
	}
	if err := live.Deployment.ValidateExact(); err != nil {
		t.Fatal(err)
	}

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := handles[0].Bytes(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("secret handle remained readable after unmount: %v", err)
	}
	trace, err := mounted.RecordedTrace()
	if err != nil {
		t.Fatal(err)
	}
	if trace.Deployment == nil || !reflect.DeepEqual(*trace.Deployment, *live.Deployment) {
		t.Fatalf("recorded deployment = %+v, live = %+v", trace.Deployment, live.Deployment)
	}
	replayer, err := inspect.NewTraceReplayer(mounted.Graph(), trace)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Deployment == nil || !reflect.DeepEqual(*replayed.Deployment, *live.Deployment) {
		t.Fatalf("replayed deployment = %+v", replayed.Deployment)
	}

	for label, payload := range map[string]any{
		"live": live, "trace": trace, "replay": replayed,
	} {
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, forbidden := range []string{
			preparedSecretReference, "PREPARED_TOKEN", preparedSecretLocator, preparedSecretValue,
		} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("%s evidence leaked %q: %s", label, forbidden, encoded)
			}
		}
		for _, required := range []string{
			fixture.plan.DeploymentFingerprint(), fixture.plan.SecretCatalogFingerprint(),
		} {
			if !strings.Contains(string(encoded), required) {
				t.Fatalf("%s evidence omitted opaque identity %q", label, required)
			}
		}
	}
}

func TestPreparePlanRejectsSecretCatalogAndProviderDriftBeforeAcquisition(t *testing.T) {
	fixture := newPrepareFixture(t)
	provider := &trackingPreparedSecretProvider{}
	mounts := &atomic.Int64{}
	factory := secretPreparedFactory{
		descriptor: fixture.descriptor, profile: fixture.profile, mounts: mounts,
	}
	base := secretPreparedAssembly(t, fixture, factory, provider)

	tests := []struct {
		name   string
		mutate func(*graphruntime.PlanPreparation)
		want   string
	}{
		{name: "missing catalog", mutate: func(value *graphruntime.PlanPreparation) {
			value.SecretCatalog = nil
		}, want: "exact graph-scoped secret catalog"},
		{name: "locator drift", mutate: func(value *graphruntime.PlanPreparation) {
			copy := fixture.secretCatalog
			copy.Secrets = map[string]graphsecret.Binding{
				preparedSecretReference: {Provider: "env", Locator: preparedSecretLocator},
			}
			value.SecretCatalog = &copy
		}, want: "drifted from the frozen plan"},
		{name: "missing provider registry", mutate: func(value *graphruntime.PlanPreparation) {
			value.SecretProviders = nil
		}, want: "provider registry"},
		{name: "unregistered provider", mutate: func(value *graphruntime.PlanPreparation) {
			value.SecretProviders = graphsecret.NewRegistry()
		}, want: "unregistered provider"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assembly := base
			test.mutate(&assembly)
			if _, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if mounts.Load() != 0 || len(provider.raw) != 0 {
		t.Fatalf("drift acquired resources: mounts=%d provider-values=%d", mounts.Load(), len(provider.raw))
	}
}

func TestSecretProviderErrorsAreSanitizedAndNodeSlotsAreScoped(t *testing.T) {
	fixture := newPrepareFixture(t)
	privateProviderError := fmt.Errorf("provider failed at %s with %s", preparedSecretLocator, preparedSecretValue)
	provider := &trackingPreparedSecretProvider{failure: privateProviderError}
	factory := secretPreparedFactory{
		descriptor: fixture.descriptor, profile: fixture.profile, resolveMount: true,
	}
	assembly := secretPreparedAssembly(t, fixture, factory, provider)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err == nil || !errors.Is(err, graphruntime.ErrSecretResolution) {
		t.Fatalf("mount error = %v", err)
	}
	for _, forbidden := range []string{
		preparedSecretReference, preparedSecretLocator, preparedSecretValue, privateProviderError.Error(),
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("sanitized mount error leaked %q: %v", forbidden, err)
		}
	}

	var abortedHandles []*graphsecret.Value
	provider = &trackingPreparedSecretProvider{}
	factory = secretPreparedFactory{
		descriptor: fixture.descriptor, profile: fixture.profile,
		resolveMount: true, failAfterResolve: true, handles: &abortedHandles,
	}
	assembly = secretPreparedAssembly(t, fixture, factory, provider)
	prepared, err = graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{}); err == nil ||
		!strings.Contains(err.Error(), "intentional mount abort") {
		t.Fatalf("mount abort error = %v", err)
	}
	if len(abortedHandles) != 1 {
		t.Fatalf("mount abort handles = %d", len(abortedHandles))
	}
	if _, err := abortedHandles[0].Bytes(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("mount abort retained readable secret: %v", err)
	}

	accessChannel := make(chan graphruntime.SecretAccess, 1)
	provider = &trackingPreparedSecretProvider{}
	factory = secretPreparedFactory{
		descriptor: fixture.descriptor, profile: fixture.profile, access: accessChannel,
	}
	assembly = secretPreparedAssembly(t, fixture, factory, provider)
	prepared, err = graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	access := <-accessChannel
	if value, _, err := access.Resolve(context.Background(), "other"); err == nil || value != nil ||
		!strings.Contains(err.Error(), "not bound") {
		t.Fatalf("cross-slot resolution = value %v, error %v", value, err)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSecretAccessRepeatedConcurrentResolutionAndCloseRace(t *testing.T) {
	fixture := newPrepareFixture(t)
	provider := &trackingPreparedSecretProvider{}
	accessChannel := make(chan graphruntime.SecretAccess, 1)
	factory := secretPreparedFactory{
		descriptor: fixture.descriptor, profile: fixture.profile, access: accessChannel,
	}
	assembly := secretPreparedAssembly(t, fixture, factory, provider)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	access := <-accessChannel
	const workers = 32
	errorsChannel := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, _, resolveErr := access.Resolve(context.Background(), "token")
			if resolveErr != nil {
				errorsChannel <- resolveErr
				return
			}
			contents, readErr := value.Bytes()
			if readErr != nil || string(contents) != preparedSecretValue {
				errorsChannel <- fmt.Errorf("read resolved secret: %v", readErr)
			}
			for index := range contents {
				contents[index] = 0
			}
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for workerErr := range errorsChannel {
		t.Error(workerErr)
	}
	if evidence, found := mounted.DeploymentEvidence(); !found || len(evidence.Secrets) != 1 {
		t.Fatalf("repeated resolution evidence = %+v, found=%t", evidence, found)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !provider.providerBuffersErased() {
		t.Fatal("provider-returned buffers were not erased")
	}

	// A provider that completes after unmount cannot publish a live handle:
	// lifecycle registration fails closed and the just-created value is erased.
	started := make(chan struct{})
	release := make(chan struct{})
	provider = &trackingPreparedSecretProvider{started: started, release: release}
	accessChannel = make(chan graphruntime.SecretAccess, 1)
	factory = secretPreparedFactory{
		descriptor: fixture.descriptor, profile: fixture.profile, access: accessChannel,
	}
	assembly = secretPreparedAssembly(t, fixture, factory, provider)
	prepared, err = graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		t.Fatal(err)
	}
	mounted, err = prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	access = <-accessChannel
	type resolutionResult struct {
		value *graphsecret.Value
		err   error
	}
	result := make(chan resolutionResult, 1)
	go func() {
		value, _, resolveErr := access.Resolve(context.Background(), "token")
		result <- resolutionResult{value: value, err: resolveErr}
	}()
	<-started
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	resolved := <-result
	if resolved.err == nil || resolved.value != nil || !strings.Contains(resolved.err.Error(), "lifecycle") {
		t.Fatalf("post-close resolution = value %v, error %v", resolved.value, resolved.err)
	}
	if !provider.providerBuffersErased() {
		t.Fatal("post-close provider buffer was not erased")
	}
}

func BenchmarkPreparedPlanSecretMountEvidence(b *testing.B) {
	fixture := newPrepareFixture(b)
	provider := &trackingPreparedSecretProvider{}
	factory := secretPreparedFactory{
		descriptor: fixture.descriptor, profile: fixture.profile, resolveMount: true,
	}
	assembly := secretPreparedAssemblyForTB(b, fixture, factory, provider)
	prepared, err := graphruntime.PreparePlan(context.Background(), fixture.plan, assembly)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		mounted, mountErr := prepared.Mount(context.Background(), graphruntime.PreparedMountOptions{})
		if mountErr != nil {
			b.Fatal(mountErr)
		}
		if evidence, found := mounted.DeploymentEvidence(); !found || len(evidence.Secrets) != 1 {
			b.Fatal("mounted secret evidence is incomplete")
		}
		if closeErr := mounted.Close(context.Background()); closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}

func secretPreparedAssemblyForTB(
	testingTB testing.TB,
	fixture prepareFixture,
	factory element.Factory,
	provider graphsecret.Provider,
) graphruntime.PlanPreparation {
	testingTB.Helper()
	assembly, _ := fixture.exactAssembly(testingTB, fixture.profile, fixture.dependencyArtifact)
	registry := graphruntime.NewRegistry()
	if err := registry.RegisterFactory(graphruntime.FactoryRegistration{
		Profile: fixture.profile, Factory: factory,
	}); err != nil {
		testingTB.Fatal(err)
	}
	providers := graphsecret.NewRegistry()
	if err := providers.Register("env", fixture.secretProvider, provider); err != nil {
		testingTB.Fatal(err)
	}
	assembly.Factories = registry
	assembly.SecretProviders = providers
	return assembly
}
