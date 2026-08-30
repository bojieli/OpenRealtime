package realtimecu

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
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
	observerSelection := ApplicationObserverSelection{
		Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
		Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
	}
	var modelAcquisitions, observerAcquisitions, constructorCalls atomic.Int32
	modelFactory := func(context.Context, legacy.Options) (continuation.Provider, error) {
		modelAcquisitions.Add(1)
		return nil, nil
	}
	observerFactory := func(context.Context, legacy.Options) (Observer, error) {
		observerAcquisitions.Add(1)
		return nil, nil
	}
	source := ApplicationRegistrationConfig{
		ApplicationArtifact: applicationArtifact,
		ProviderArtifact:    providerArtifact,
		RuntimeArtifact:     plugin.RuntimeArtifact,
		Models: []ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection, Factory: modelFactory,
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
		Model:         modelSelection,
		Observer:      observerSelection,
		Target:        plugin.Target,
	})
	if _, err := registration.Factory(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if constructorCalls.Load() != 1 || modelAcquisitions.Load() != 0 ||
		observerAcquisitions.Load() != 0 {
		t.Fatalf("valid resolution calls constructor=%d model=%d observer=%d",
			constructorCalls.Load(), modelAcquisitions.Load(), observerAcquisitions.Load())
	}
	if resolved.RuntimeArtifact != plugin.RuntimeArtifact ||
		resolved.Model.Reference != modelSelection.Reference ||
		resolved.Model.Artifact != modelSelection.Artifact ||
		resolved.Model.Descriptor != modelSelection.Descriptor ||
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
			config := ApplicationConfig{FormatVersion: ApplicationFormatVersion, Model: modelSelection, Observer: observerSelection, Target: plugin.Target}
			config.Model.Artifact.Revision = "v2"
			return config
		}(), want: "artifact or descriptor drifted"},
		{name: "model descriptor", config: func() ApplicationConfig {
			config := ApplicationConfig{FormatVersion: ApplicationFormatVersion, Model: modelSelection, Observer: observerSelection, Target: plugin.Target}
			config.Model.Descriptor.Model = "drifted-model"
			return config
		}(), want: "artifact or descriptor drifted"},
		{name: "observer artifact", config: func() ApplicationConfig {
			config := ApplicationConfig{FormatVersion: ApplicationFormatVersion, Model: modelSelection, Observer: observerSelection, Target: plugin.Target}
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
		`{"format_version":1,"format_version":1}`,
	)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate configuration error = %v", err)
	}
	if constructorCalls.Load() != 1 || modelAcquisitions.Load() != 0 ||
		observerAcquisitions.Load() != 0 {
		t.Fatalf("rejected resolution crossed boundary: constructor=%d model=%d observer=%d",
			constructorCalls.Load(), modelAcquisitions.Load(), observerAcquisitions.Load())
	}
}

func TestApplicationRegistrationRechecksCancellationAtConstructorBoundary(t *testing.T) {
	plugin := validTestPluginConfig()
	modelSelection := ApplicationModelSelection{
		Reference: plugin.Model.Reference, Artifact: plugin.Model.Artifact,
		Descriptor: plugin.Model.Descriptor,
	}
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
		Observer: observerSelection, Target: plugin.Target,
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
	observerSelection := ApplicationObserverSelection{
		Reference: plugin.Observer.Reference, Name: plugin.Observer.Name,
		Artifact: plugin.Observer.Artifact, Sources: slices.Clone(plugin.Observer.Sources),
	}
	readinessFailure := errors.New("deployment drifted")
	var fail atomic.Bool
	var modelReadiness, observerReadiness, modelFactory, observerFactory atomic.Int32
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
		Observer: observerSelection, Target: plugin.Target,
	})
	launchConfig, err := registration.Factory(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(launchConfig.Readiness) != 2 {
		t.Fatalf("selected readiness checks = %d, want 2", len(launchConfig.Readiness))
	}
	for _, readiness := range launchConfig.Readiness {
		if err := readiness.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := resolved.Model.Factory(context.Background(), legacy.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.Observer.Factory(context.Background(), legacy.Options{}); err != nil {
		t.Fatal(err)
	}
	if modelReadiness.Load() != 2 || observerReadiness.Load() != 2 ||
		modelFactory.Load() != 1 || observerFactory.Load() != 1 {
		t.Fatalf("ready model=%d observer=%d factories model=%d observer=%d",
			modelReadiness.Load(), observerReadiness.Load(), modelFactory.Load(), observerFactory.Load())
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
	if _, err := resolved.Observer.Factory(context.Background(), legacy.Options{}); !errors.Is(err, readinessFailure) {
		t.Fatalf("drifted observer factory error = %v", err)
	}
	if modelFactory.Load() != 1 || observerFactory.Load() != 1 {
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
		Observer: observerSelection, Target: plugin.Target,
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
				Observers: []ObserverFactoryRegistration{configured},
			}, func(PluginConfig) (graphlaunch.Config, error) { return graphlaunch.Config{}, nil })
			if err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("factory-mode error = %v, want exactly one", err)
			}
		})
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
