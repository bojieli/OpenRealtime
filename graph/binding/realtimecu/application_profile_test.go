package realtimecu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestApplicationRegistrationSnapshotsFactoriesAndRejectsDriftBeforeConstructor(t *testing.T) {
	plugin := validTestPluginConfig()
	applicationArtifact := testArtifact("application-profile", "d")
	providerArtifact := testArtifact("provider-profile", "e")
	modelSelection := ApplicationModelSelection{
		Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
		Descriptor: plugin.Model.Descriptor,
	}
	policySelection := applicationPolicySelection(plugin)
	observerSelection := ApplicationObserverSelection{
		Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
		Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
	}
	var modelAcquisitions, policyAcquisitions, observerAcquisitions, constructorCalls atomic.Int32
	modelFactory := func(context.Context, legacy.Options) (continuation.Provider, error) {
		modelAcquisitions.Add(1)
		return nil, nil
	}
	observerFactory := func(context.Context, legacy.Options) (Observer, error) {
		observerAcquisitions.Add(1)
		return nil, nil
	}
	policyFactory := func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
		policyAcquisitions.Add(1)
		return nil, nil
	}
	source := ApplicationRegistrationConfig{
		ApplicationArtifact: applicationArtifact,
		ProviderArtifact:    providerArtifact,
		RuntimeArtifact:     plugin.RuntimeArtifact,
		Models: []ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection, Factory: modelFactory,
		}},
		Policies: []PolicyFactoryRegistration{{
			ApplicationPolicySelection: policySelection, Factory: policyFactory,
		}},
		Observers: []ObserverFactoryRegistration{{
			ApplicationObserverSelection: ApplicationObserverSelection{
				Reference: observerSelection.Reference, Name: observerSelection.Name,
				Artifact: observerSelection.Artifact, Sources: slices.Clone(observerSelection.Sources),
			},
			Factory: observerFactory,
		}},
	}
	var resolved PluginConfig
	registration, err := NewApplicationRegistration(source,
		func(config PluginConfig) (graphlaunch.Config, error) {
			constructorCalls.Add(1)
			resolved = config
			return graphlaunch.Config{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{registration}); err != nil {
		t.Fatalf("returned registration failed generic registry validation: %v", err)
	}

	// The registration owns its exact inventory. Mutating caller slices after
	// construction cannot alter the identity checked by Factory.
	source.Models[0].Descriptor.Model = "mutated-after-registration"
	source.Observers[0].Sources[0] = "mutated-after-registration"
	payload := applicationProfilePayload(t, ApplicationConfig{
		FormatVersion: ApplicationFormatVersion,
		Model:         modelSelection, SettlementPolicy: policySelection,
		Observer: observerSelection, Target: plugin.Target,
	})
	if _, err := registration.Factory(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if constructorCalls.Load() != 1 || modelAcquisitions.Load() != 0 || policyAcquisitions.Load() != 0 ||
		observerAcquisitions.Load() != 0 {
		t.Fatalf("valid resolution calls constructor=%d model=%d policy=%d observer=%d",
			constructorCalls.Load(), modelAcquisitions.Load(), policyAcquisitions.Load(), observerAcquisitions.Load())
	}
	if resolved.RuntimeArtifact != plugin.RuntimeArtifact ||
		resolved.Model.Reference != modelSelection.Reference ||
		resolved.Model.Artifact != modelSelection.Artifact ||
		resolved.Model.Descriptor != modelSelection.Descriptor ||
		resolved.SettlementPolicy.Reference != SettlementPolicyReference ||
		resolved.SettlementPolicy.Artifact != policySelection.Artifact ||
		resolved.SettlementPolicy.Descriptor != policySelection.Descriptor ||
		resolved.Observer.Reference != observerSelection.Reference ||
		resolved.Observer.Artifact != observerSelection.Artifact ||
		!slices.Equal(resolved.Observer.Sources, observerSelection.Sources) {
		t.Fatalf("resolved registration identity = %+v", resolved)
	}

	invalid := []struct {
		name   string
		config ApplicationConfig
		want   string
	}{
		{name: "model artifact", config: func() ApplicationConfig {
			config := ApplicationConfig{FormatVersion: ApplicationFormatVersion, Model: modelSelection, SettlementPolicy: policySelection, Observer: observerSelection, Target: plugin.Target}
			config.Model.Artifact.Revision = "v2"
			return config
		}(), want: "artifact or descriptor drifted"},
		{name: "model descriptor", config: func() ApplicationConfig {
			config := ApplicationConfig{FormatVersion: ApplicationFormatVersion, Model: modelSelection, SettlementPolicy: policySelection, Observer: observerSelection, Target: plugin.Target}
			config.Model.Descriptor.Model = "drifted-model"
			return config
		}(), want: "artifact or descriptor drifted"},
		{name: "observer artifact", config: func() ApplicationConfig {
			config := ApplicationConfig{FormatVersion: ApplicationFormatVersion, Model: modelSelection, SettlementPolicy: policySelection, Observer: observerSelection, Target: plugin.Target}
			config.Observer.Artifact.Revision = "v2"
			return config
		}(), want: "identity or source contract drifted"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := registration.Factory(context.Background(), applicationProfilePayload(t, test.config)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("registration drift error = %v, want %q", err, test.want)
			}
		})
	}
	if _, err := registration.Factory(context.Background(), json.RawMessage(
		`{"format_version":2,"format_version":2}`,
	)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate configuration error = %v", err)
	}
	if constructorCalls.Load() != 1 || modelAcquisitions.Load() != 0 || policyAcquisitions.Load() != 0 ||
		observerAcquisitions.Load() != 0 {
		t.Fatalf("rejected resolution crossed boundary: constructor=%d model=%d policy=%d observer=%d",
			constructorCalls.Load(), modelAcquisitions.Load(), policyAcquisitions.Load(), observerAcquisitions.Load())
	}
}

func TestApplicationRegistrationRechecksCancellationAtConstructorBoundary(t *testing.T) {
	plugin := validTestPluginConfig()
	modelSelection := ApplicationModelSelection{
		Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
		Descriptor: plugin.Model.Descriptor,
	}
	policySelection := applicationPolicySelection(plugin)
	observerSelection := ApplicationObserverSelection{
		Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
		Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
	}
	var constructorCalls atomic.Int32
	var cancel context.CancelFunc
	registration, err := NewApplicationRegistration(ApplicationRegistrationConfig{
		ApplicationArtifact: testArtifact("application-profile-cancel", "d"),
		ProviderArtifact:    testArtifact("provider-profile-cancel", "e"),
		RuntimeArtifact:     plugin.RuntimeArtifact,
		Models: []ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection,
			Factory:                   func(context.Context, legacy.Options) (continuation.Provider, error) { return nil, nil },
		}},
		Policies: []PolicyFactoryRegistration{{
			ApplicationPolicySelection: policySelection,
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				return nil, nil
			},
		}},
		Observers: []ObserverFactoryRegistration{{
			ApplicationObserverSelection: observerSelection,
			Factory:                      func(context.Context, legacy.Options) (Observer, error) { return nil, nil },
		}},
	}, func(PluginConfig) (graphlaunch.Config, error) {
		constructorCalls.Add(1)
		cancel()
		return graphlaunch.Config{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := applicationProfilePayload(t, ApplicationConfig{
		FormatVersion: ApplicationFormatVersion, Model: modelSelection,
		SettlementPolicy: policySelection, Observer: observerSelection, Target: plugin.Target,
	})

	preCanceled, preCancel := context.WithCancel(context.Background())
	preCancel()
	if _, err := registration.Factory(preCanceled, payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-constructor cancellation error = %v", err)
	}
	if constructorCalls.Load() != 0 {
		t.Fatalf("pre-canceled resolution called constructor %d times", constructorCalls.Load())
	}

	ctx, cancelResolution := context.WithCancel(context.Background())
	cancel = cancelResolution
	if _, err := registration.Factory(ctx, payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-constructor cancellation error = %v", err)
	}
	if constructorCalls.Load() != 1 {
		t.Fatalf("constructor calls after in-boundary cancellation = %d", constructorCalls.Load())
	}
}

func TestApplicationRegistrationRetainsSelectedReadinessAtStartupAndSessionOpen(t *testing.T) {
	plugin := validTestPluginConfig()
	modelSelection := ApplicationModelSelection{
		Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
		Descriptor: plugin.Model.Descriptor,
	}
	policySelection := applicationPolicySelection(plugin)
	observerSelection := ApplicationObserverSelection{
		Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
		Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
	}
	readinessFailure := errors.New("deployment drifted")
	var fail atomic.Bool
	var modelReadiness, policyReadiness, observerReadiness atomic.Int32
	var modelFactory, policyFactory, observerFactory atomic.Int32
	check := func(counter *atomic.Int32) func(context.Context) error {
		return func(ctx context.Context) error {
			counter.Add(1)
			if fail.Load() {
				return readinessFailure
			}
			return context.Cause(ctx)
		}
	}
	var resolved PluginConfig
	registration, err := NewApplicationRegistration(ApplicationRegistrationConfig{
		ApplicationArtifact: testArtifact("application-profile-readiness", "d"),
		ProviderArtifact:    testArtifact("provider-profile-readiness", "e"),
		RuntimeArtifact:     plugin.RuntimeArtifact,
		Models: []ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection,
			Readiness:                 check(&modelReadiness),
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				modelFactory.Add(1)
				return nil, nil
			},
		}},
		Policies: []PolicyFactoryRegistration{{
			ApplicationPolicySelection: policySelection,
			Readiness:                  check(&policyReadiness),
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				policyFactory.Add(1)
				return nil, nil
			},
		}},
		Observers: []ObserverFactoryRegistration{{
			ApplicationObserverSelection: observerSelection,
			Readiness:                    check(&observerReadiness),
			Factory: func(context.Context, legacy.Options) (Observer, error) {
				observerFactory.Add(1)
				return nil, nil
			},
		}},
	}, func(config PluginConfig) (graphlaunch.Config, error) {
		resolved = config
		return graphlaunch.Config{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := applicationProfilePayload(t, ApplicationConfig{
		FormatVersion: ApplicationFormatVersion, Model: modelSelection,
		SettlementPolicy: policySelection, Observer: observerSelection, Target: plugin.Target,
	})
	launchConfig, err := registration.Factory(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(launchConfig.Readiness) != 3 {
		t.Fatalf("selected readiness checks = %d, want 3", len(launchConfig.Readiness))
	}
	for _, readiness := range launchConfig.Readiness {
		if err := readiness.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := resolved.Model.Factory(context.Background(), legacy.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.SettlementPolicy.Factory(context.Background(), legacy.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.Observer.Factory(context.Background(), legacy.Options{}); err != nil {
		t.Fatal(err)
	}
	if modelReadiness.Load() != 2 || policyReadiness.Load() != 2 || observerReadiness.Load() != 2 ||
		modelFactory.Load() != 1 || policyFactory.Load() != 1 || observerFactory.Load() != 1 {
		t.Fatalf("ready model=%d policy=%d observer=%d factories model=%d policy=%d observer=%d",
			modelReadiness.Load(), policyReadiness.Load(), observerReadiness.Load(),
			modelFactory.Load(), policyFactory.Load(), observerFactory.Load())
	}

	fail.Store(true)
	for _, readiness := range launchConfig.Readiness {
		if err := readiness.Check(context.Background()); !errors.Is(err, readinessFailure) {
			t.Fatalf("drifted startup readiness error = %v", err)
		}
	}
	if _, err := resolved.Model.Factory(context.Background(), legacy.Options{}); !errors.Is(err, readinessFailure) {
		t.Fatalf("drifted model factory error = %v", err)
	}
	if _, err := resolved.SettlementPolicy.Factory(context.Background(), legacy.Options{}); !errors.Is(err, readinessFailure) {
		t.Fatalf("drifted settlement policy factory error = %v", err)
	}
	if _, err := resolved.Observer.Factory(context.Background(), legacy.Options{}); !errors.Is(err, readinessFailure) {
		t.Fatalf("drifted observer factory error = %v", err)
	}
	if modelFactory.Load() != 1 || policyFactory.Load() != 1 || observerFactory.Load() != 1 {
		t.Fatal("drifted deployment crossed a provider factory boundary")
	}
}

type applicationProfileTestRetainer struct{}

func (*applicationProfileTestRetainer) Retain(
	reference trajectory.MediaRef, _ []byte,
) (trajectory.MediaRef, error) {
	return reference, nil
}

func TestApplicationRegistrationDefersResourceAwareObserverUntilSessionOpen(t *testing.T) {
	plugin := validTestPluginConfig()
	modelSelection := ApplicationModelSelection{
		Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
		Descriptor: plugin.Model.Descriptor,
	}
	policySelection := applicationPolicySelection(plugin)
	observerSelection := ApplicationObserverSelection{
		Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
		Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
	}
	var readiness, acquisitions atomic.Int32
	var gotRetainer any
	var resolved PluginConfig
	registration, err := NewApplicationRegistration(ApplicationRegistrationConfig{
		ApplicationArtifact: testArtifact("application-profile-resource-observer", "d"),
		ProviderArtifact:    testArtifact("provider-profile-resource-observer", "e"),
		RuntimeArtifact:     plugin.RuntimeArtifact,
		Models: []ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return nil, nil
			},
		}},
		Policies: []PolicyFactoryRegistration{{
			ApplicationPolicySelection: policySelection,
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				return nil, nil
			},
		}},
		Observers: []ObserverFactoryRegistration{{
			ApplicationObserverSelection: observerSelection,
			Readiness: func(ctx context.Context) error {
				readiness.Add(1)
				return context.Cause(ctx)
			},
			ResourceFactory: func(
				_ context.Context, _ legacy.Options, resources ObserverResources,
			) (Observer, error) {
				acquisitions.Add(1)
				gotRetainer = resources.Retainer
				return nil, nil
			},
		}},
	}, func(config PluginConfig) (graphlaunch.Config, error) {
		resolved = config
		return graphlaunch.Config{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := applicationProfilePayload(t, ApplicationConfig{
		FormatVersion: ApplicationFormatVersion, Model: modelSelection,
		SettlementPolicy: policySelection, Observer: observerSelection, Target: plugin.Target,
	})
	launchConfig, err := registration.Factory(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if acquisitions.Load() != 0 || readiness.Load() != 0 {
		t.Fatalf("profile resolution acquired resource observer: readiness=%d factories=%d",
			readiness.Load(), acquisitions.Load())
	}
	if resolved.Observer.Factory != nil || resolved.Observer.ResourceFactory == nil {
		t.Fatalf("resolved observer factory modes plain=%v resource=%v",
			resolved.Observer.Factory != nil, resolved.Observer.ResourceFactory != nil)
	}
	if len(launchConfig.Readiness) != 1 {
		t.Fatalf("selected readiness checks = %d, want 1", len(launchConfig.Readiness))
	}
	if err := launchConfig.Readiness[0].Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	retainer := &applicationProfileTestRetainer{}
	if _, err := resolved.Observer.ResourceFactory(context.Background(), legacy.Options{}, ObserverResources{
		Retainer: retainer,
	}); err != nil {
		t.Fatal(err)
	}
	if readiness.Load() != 2 || acquisitions.Load() != 1 || gotRetainer != retainer {
		t.Fatalf("resource observer readiness=%d factories=%d retainer=%T",
			readiness.Load(), acquisitions.Load(), gotRetainer)
	}
}

func TestApplicationRegistrationRejectsAmbiguousObserverFactories(t *testing.T) {
	plugin := validTestPluginConfig()
	policy := PolicyFactoryRegistration{
		ApplicationPolicySelection: applicationPolicySelection(plugin),
		Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
			return nil, nil
		},
	}
	observer := ObserverFactoryRegistration{
		ApplicationObserverSelection: ApplicationObserverSelection{
			Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
			Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
		},
	}
	model := ModelFactoryRegistration{
		ApplicationModelSelection: ApplicationModelSelection{
			Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
			Descriptor: plugin.Model.Descriptor,
		},
		Factory: func(context.Context, legacy.Options) (continuation.Provider, error) { return nil, nil },
	}
	plain := func(context.Context, legacy.Options) (Observer, error) { return nil, nil }
	resource := func(context.Context, legacy.Options, ObserverResources) (Observer, error) { return nil, nil }
	for _, testCase := range []struct {
		name     string
		plain    func(context.Context, legacy.Options) (Observer, error)
		resource func(context.Context, legacy.Options, ObserverResources) (Observer, error)
	}{
		{name: "neither"},
		{name: "both", plain: plain, resource: resource},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configured := observer
			configured.Factory = testCase.plain
			configured.ResourceFactory = testCase.resource
			_, err := NewApplicationRegistration(ApplicationRegistrationConfig{
				ApplicationArtifact: testArtifact("application-profile-factory-mode-"+testCase.name, "d"),
				ProviderArtifact:    testArtifact("provider-profile-factory-mode-"+testCase.name, "e"),
				RuntimeArtifact:     plugin.RuntimeArtifact, Models: []ModelFactoryRegistration{model},
				Policies:  []PolicyFactoryRegistration{policy},
				Observers: []ObserverFactoryRegistration{configured},
			}, func(PluginConfig) (graphlaunch.Config, error) { return graphlaunch.Config{}, nil })
			if err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("factory-mode error = %v, want exactly one", err)
			}
		})
	}
}

func TestApplicationRegistrationResolvesParameterizedSettlementPolicyWithoutOpeningIt(t *testing.T) {
	plugin := validTestPluginConfig()
	selection := applicationPolicySelection(plugin)
	configuration := json.RawMessage(
		`{"format_version":1,"mode":"strict","token_environment":"OPENREALTIME_SETTLEMENT_KEY"}`,
	)
	selection.Configuration = slices.Clone(configuration)
	var describeCalls, readinessCalls, factoryCalls, constructorCalls atomic.Int32
	var receivedMu sync.Mutex
	var received [][]byte
	record := func(raw json.RawMessage) error {
		receivedMu.Lock()
		defer receivedMu.Unlock()
		if !slices.Equal(raw, configuration) {
			return errors.New("parameterized callback received drifted configuration")
		}
		received = append(received, slices.Clone(raw))
		if len(raw) > 0 {
			raw[0] = '!'
		}
		return nil
	}
	policy := PolicyFactoryRegistration{
		ApplicationPolicySelection: ApplicationPolicySelection{
			Reference: selection.Reference, Artifact: selection.Artifact,
		},
		DescribeConfiguration: func(raw json.RawMessage) (policyelements.SemanticDeciderDescriptor, error) {
			describeCalls.Add(1)
			if err := record(raw); err != nil {
				return policyelements.SemanticDeciderDescriptor{}, err
			}
			return selection.Descriptor, nil
		},
		ReadinessConfiguration: func(_ context.Context, raw json.RawMessage) error {
			readinessCalls.Add(1)
			return record(raw)
		},
		FactoryConfiguration: func(
			_ context.Context, _ legacy.Options, raw json.RawMessage,
		) (policyelements.SemanticDecider, error) {
			factoryCalls.Add(1)
			return nil, record(raw)
		},
	}
	source := applicationRegistrationConfigWithPolicy(plugin, policy)
	var resolved PluginConfig
	registration, err := NewApplicationRegistration(source, func(config PluginConfig) (graphlaunch.Config, error) {
		constructorCalls.Add(1)
		resolved = config
		return graphlaunch.Config{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if describeCalls.Load() != 0 || readinessCalls.Load() != 0 || factoryCalls.Load() != 0 {
		t.Fatal("registration construction crossed a parameterized policy callback boundary")
	}
	payload := applicationProfilePayload(t, ApplicationConfig{
		FormatVersion: ApplicationFormatVersion,
		Model: ApplicationModelSelection{
			Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
			Descriptor: plugin.Model.Descriptor,
		},
		SettlementPolicy: selection,
		Observer: ApplicationObserverSelection{
			Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
			Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
		},
		Target: plugin.Target,
	})
	launchConfig, err := registration.Factory(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if describeCalls.Load() != 1 || readinessCalls.Load() != 0 || factoryCalls.Load() != 0 || constructorCalls.Load() != 1 {
		t.Fatalf("resolution callbacks describe=%d ready=%d factory=%d constructor=%d",
			describeCalls.Load(), readinessCalls.Load(), factoryCalls.Load(), constructorCalls.Load())
	}
	if resolved.SettlementPolicy.Reference != SettlementPolicyReference ||
		resolved.SettlementPolicy.Descriptor != selection.Descriptor ||
		resolved.SettlementPolicy.Artifact != selection.Artifact {
		t.Fatalf("resolved settlement policy = %+v", resolved.SettlementPolicy)
	}
	if len(launchConfig.Readiness) != 1 {
		t.Fatalf("parameterized policy readiness checks = %d, want 1", len(launchConfig.Readiness))
	}
	if err := launchConfig.Readiness[0].Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.SettlementPolicy.Factory(context.Background(), legacy.Options{}); err != nil {
		t.Fatal(err)
	}
	if describeCalls.Load() != 1 || readinessCalls.Load() != 2 || factoryCalls.Load() != 1 {
		t.Fatalf("opened callbacks describe=%d ready=%d factory=%d",
			describeCalls.Load(), readinessCalls.Load(), factoryCalls.Load())
	}
	receivedMu.Lock()
	defer receivedMu.Unlock()
	if len(received) != 4 {
		t.Fatalf("parameterized policy configuration deliveries = %d, want 4", len(received))
	}
	for index, raw := range received {
		if !slices.Equal(raw, configuration) {
			t.Fatalf("configuration delivery %d drifted: %q", index, raw)
		}
	}
}

func TestApplicationRegistrationRejectsSettlementPolicyInventoryAmbiguity(t *testing.T) {
	plugin := validTestPluginConfig()
	plain := PolicyFactoryRegistration{
		ApplicationPolicySelection: applicationPolicySelection(plugin),
		Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
			return nil, nil
		},
	}
	parameterized := PolicyFactoryRegistration{
		ApplicationPolicySelection: ApplicationPolicySelection{
			Reference: plain.Reference, Artifact: plain.Artifact,
		},
		DescribeConfiguration: func(json.RawMessage) (policyelements.SemanticDeciderDescriptor, error) {
			return plugin.SettlementPolicy.Descriptor, nil
		},
		FactoryConfiguration: func(
			context.Context, legacy.Options, json.RawMessage,
		) (policyelements.SemanticDecider, error) {
			return nil, nil
		},
		ReadinessConfiguration: func(context.Context, json.RawMessage) error { return nil },
	}
	tests := []struct {
		name     string
		policies []PolicyFactoryRegistration
		want     string
	}{
		{name: "missing", want: "requires model, settlement policy, and observer"},
		{name: "duplicate", policies: []PolicyFactoryRegistration{plain, plain}, want: "registered more than once"},
		{name: "partial parameterized", policies: []PolicyFactoryRegistration{func() PolicyFactoryRegistration {
			copy := parameterized
			copy.FactoryConfiguration = nil
			return copy
		}()}, want: "partial or mixed"},
		{name: "mixed parameterized", policies: []PolicyFactoryRegistration{func() PolicyFactoryRegistration {
			copy := parameterized
			copy.Factory = plain.Factory
			return copy
		}()}, want: "partial or mixed"},
		{name: "plain with configuration", policies: []PolicyFactoryRegistration{func() PolicyFactoryRegistration {
			copy := plain
			copy.Configuration = json.RawMessage(`{"mode":"ambiguous"}`)
			return copy
		}()}, want: "configuration without a parameterized factory"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			source := applicationRegistrationConfigWithPolicy(plugin, plain)
			source.Policies = testCase.policies
			_, err := NewApplicationRegistration(source, func(PluginConfig) (graphlaunch.Config, error) {
				t.Fatal("invalid settlement policy inventory reached constructor")
				return graphlaunch.Config{}, nil
			})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("inventory error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestApplicationResolutionRejectsSettlementIdentityConfigurationAndSecretDriftBeforeOpen(t *testing.T) {
	plugin := validTestPluginConfig()
	selection := applicationPolicySelection(plugin)
	selection.Configuration = json.RawMessage(`{"mode":"strict"}`)
	var describes, factories, constructors atomic.Int32
	policy := PolicyFactoryRegistration{
		ApplicationPolicySelection: ApplicationPolicySelection{
			Reference: selection.Reference, Artifact: selection.Artifact,
		},
		DescribeConfiguration: func(raw json.RawMessage) (policyelements.SemanticDeciderDescriptor, error) {
			describes.Add(1)
			if bytes.Contains(raw, []byte(`"api_key"`)) || bytes.Contains(raw, []byte(`"token"`)) {
				return policyelements.SemanticDeciderDescriptor{}, errors.New("inline credential is forbidden")
			}
			return selection.Descriptor, nil
		},
		FactoryConfiguration: func(
			context.Context, legacy.Options, json.RawMessage,
		) (policyelements.SemanticDecider, error) {
			factories.Add(1)
			return nil, nil
		},
		ReadinessConfiguration: func(context.Context, json.RawMessage) error { return nil },
	}
	registration, err := NewApplicationRegistration(
		applicationRegistrationConfigWithPolicy(plugin, policy),
		func(PluginConfig) (graphlaunch.Config, error) {
			constructors.Add(1)
			return graphlaunch.Config{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	base := ApplicationConfig{
		FormatVersion: ApplicationFormatVersion,
		Model: ApplicationModelSelection{
			Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
			Descriptor: plugin.Model.Descriptor,
		},
		SettlementPolicy: selection,
		Observer: ApplicationObserverSelection{
			Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
			Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
		},
		Target: plugin.Target,
	}
	tests := []struct {
		name   string
		mutate func(*ApplicationConfig)
		want   string
	}{
		{name: "unknown host reference", mutate: func(config *ApplicationConfig) {
			config.SettlementPolicy.Reference = "provider.test.realtime-cu.missing-policy.v1"
		}, want: "registry is missing"},
		{name: "artifact drift", mutate: func(config *ApplicationConfig) {
			config.SettlementPolicy.Artifact.Revision = "v2"
		}, want: "artifact or descriptor drifted"},
		{name: "configuration digest drift", mutate: func(config *ApplicationConfig) {
			config.SettlementPolicy.Descriptor.ConfigurationDigest = "sha256:" + strings.Repeat("8", 64)
		}, want: "descriptor drifted from its configuration"},
		{name: "inline secret", mutate: func(config *ApplicationConfig) {
			config.SettlementPolicy.Configuration = json.RawMessage(`{"api_key":"literal-secret"}`)
		}, want: "inline credential is forbidden"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			config := base
			config.SettlementPolicy.Configuration = slices.Clone(base.SettlementPolicy.Configuration)
			testCase.mutate(&config)
			if _, err := registration.Factory(
				context.Background(), applicationProfilePayload(t, config),
			); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("resolution error = %v, want %q", err, testCase.want)
			}
		})
	}
	unknown := append([]byte(nil), applicationProfilePayload(t, base)...)
	unknown[len(unknown)-1] = ','
	unknown = append(unknown, []byte(`"api_key":"literal-secret"`)...)
	unknown = append(unknown, '}')
	if _, err := registration.Factory(context.Background(), unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("top-level secret field error = %v", err)
	}
	if factories.Load() != 0 || constructors.Load() != 0 {
		t.Fatalf("rejected settlement selections crossed factory=%d constructor=%d",
			factories.Load(), constructors.Load())
	}
	if describes.Load() != 2 {
		t.Fatalf("configuration descriptions = %d, want only digest and secret cases", describes.Load())
	}
}

func applicationProfilePayload(t *testing.T, config ApplicationConfig) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func applicationPolicySelection(plugin PluginConfig) ApplicationPolicySelection {
	return ApplicationPolicySelection{
		Reference: "provider.test.realtime-cu.settlement-policy.v1",
		Artifact:  plugin.SettlementPolicy.Artifact, Descriptor: plugin.SettlementPolicy.Descriptor,
	}
}

func applicationRegistrationConfigWithPolicy(
	plugin PluginConfig, policy PolicyFactoryRegistration,
) ApplicationRegistrationConfig {
	return ApplicationRegistrationConfig{
		ApplicationArtifact: testArtifact("application-profile-helper", "a"),
		ProviderArtifact:    testArtifact("provider-profile-helper", "b"),
		RuntimeArtifact:     plugin.RuntimeArtifact,
		Models: []ModelFactoryRegistration{{
			ApplicationModelSelection: ApplicationModelSelection{
				Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
				Descriptor: plugin.Model.Descriptor,
			},
			Factory: plugin.Model.Factory,
		}},
		Policies: []PolicyFactoryRegistration{policy},
		Observers: []ObserverFactoryRegistration{{
			ApplicationObserverSelection: ApplicationObserverSelection{
				Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
				Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
			},
			Factory: plugin.Observer.Factory,
		}},
	}
}
