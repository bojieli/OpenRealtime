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

func applicationProfilePayload(t *testing.T, config ApplicationConfig) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
