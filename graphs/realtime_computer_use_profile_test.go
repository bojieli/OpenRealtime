package graphs_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
)

func TestRealtimeComputerUseApplicationProfileResolvesExactGraphWithoutResources(t *testing.T) {
	target := computeruse.Target{
		Name: "profile-browser", Sources: []string{realtimecu.SourceScreen},
		Width: 1280, Height: 720,
	}
	descriptor := testRealtimeCUDescriptor()
	applicationArtifact := testRealtimeCUArtifact("profile-application", "1")
	providerArtifact := testRealtimeCUArtifact("profile-provider", "2")
	runtimeArtifact := testRealtimeCUArtifact("profile-runtime", "3")
	modelArtifact := testRealtimeCUArtifact("profile-model", "4")
	observerArtifact := testRealtimeCUArtifact("profile-observer", "5")
	gatewayArtifact := testRealtimeCUArtifact("profile-gateway", "6")
	modelReference := "model://test/realtime-cu/profile/v1"
	observerReference := "observer://test/realtime-cu/profile/v1"
	observerName := "profile-audiovisual-observer"
	observerSources := []string{
		realtimecu.SourceCamera, realtimecu.SourceMicrophone, realtimecu.SourceScreen,
	}
	observer := newTestRealtimeCUObserver(observerName)
	var modelAcquisitions, observerAcquisitions atomic.Int32
	modelFactory := func(context.Context, legacy.Options) (continuation.Provider, error) {
		modelAcquisitions.Add(1)
		return &testRealtimeCUModel{descriptor: descriptor}, nil
	}
	observerFactory := func(context.Context, legacy.Options) (realtimecu.Observer, error) {
		observerAcquisitions.Add(1)
		return observer, nil
	}

	registration, err := graphs.RealtimeComputerUseApplicationRegistration(
		realtimecu.ApplicationRegistrationConfig{
			ApplicationArtifact: applicationArtifact,
			ProviderArtifact:    providerArtifact,
			RuntimeArtifact:     runtimeArtifact,
			Models: []realtimecu.ModelFactoryRegistration{{
				ApplicationModelSelection: realtimecu.ApplicationModelSelection{
					Reference: modelReference, Artifact: modelArtifact, Descriptor: descriptor,
				},
				Factory: modelFactory,
			}},
			Observers: []realtimecu.ObserverFactoryRegistration{{
				ApplicationObserverSelection: realtimecu.ApplicationObserverSelection{
					Reference: observerReference, Name: observerName, Artifact: observerArtifact,
					Sources: observerSources,
				},
				Factory: observerFactory,
			}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if registration.Reference != realtimecu.ApplicationReference ||
		registration.Artifact != applicationArtifact ||
		registration.ProviderArtifact != providerArtifact {
		t.Fatalf("Realtime-CU application registration = %+v", registration)
	}
	application := realtimecu.ApplicationConfig{
		FormatVersion: realtimecu.ApplicationFormatVersion,
		Model: realtimecu.ApplicationModelSelection{
			Reference: modelReference, Artifact: modelArtifact, Descriptor: descriptor,
		},
		Observer: realtimecu.ApplicationObserverSelection{
			Reference: observerReference, Name: observerName, Artifact: observerArtifact,
			Sources: observerSources,
		},
		Target: target,
	}
	payload := marshalRealtimeCUApplication(t, application)

	// Produce the immutable plan identity without acquiring either factory. A
	// checked profile must repeat that exact identity before it can resolve.
	previewConfig, err := graphs.RealtimeComputerUseLaunchConfig(realtimecu.PluginConfig{
		RuntimeArtifact: runtimeArtifact,
		Model: realtimecu.ModelPlugin{
			Reference: modelReference, Artifact: modelArtifact,
			Descriptor: descriptor, Factory: modelFactory,
		},
		Observer: realtimecu.ObserverPlugin{
			Reference: observerReference, Name: observerName, Artifact: observerArtifact,
			Sources: observerSources, Factory: observerFactory,
		},
		Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := graphlaunch.New(context.Background(), previewConfig)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion,
		Name:          "openrealtime.realtime-cu.profile-test",
		Revision:      1,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: realtimecu.ApplicationReference, Artifact: applicationArtifact,
			},
			Configuration: payload,
		},
		Plan: preview.Plan.Identity(),
		Adapter: launchprofile.AdapterSelection{
			Reference: realtimecu.AdapterReference, RuntimeArtifact: runtimeArtifact,
			ProfileName: realtimecu.ProfileName, ProfileRevision: realtimecu.ProfileRevision,
		},
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.realtime-cu.profile-test", ProfileRevision: 1,
			ProviderArtifact: providerArtifact, GatewayArtifact: gatewayArtifact,
			Model: "realtime-cu-profile-test", TranscriptionModel: "realtime-cu-observer",
			ValidateWire: true, InspectionTokenTTLMS: 30_000, MaxAudioFrameBytes: 1 << 20,
			VideoLimits: openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := launchprofile.NewRegistry([]launchprofile.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	composition, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: profile, Applications: registry, GatewayArtifact: gatewayArtifact,
		})
	if err != nil {
		t.Fatal(err)
	}
	if composition.GraphPlan.Identity() != profile.Plan ||
		composition.ServerBundle.Profile.Name != profile.Server.ProfileName {
		t.Fatal("Realtime-CU profile resolution lost its graph or server identity")
	}
	if modelAcquisitions.Load() != 0 || observerAcquisitions.Load() != 0 {
		t.Fatalf("profile resolution acquired resources: model=%d observer=%d",
			modelAcquisitions.Load(), observerAcquisitions.Load())
	}

	unknown := make(map[string]any)
	if err := json.Unmarshal(payload, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["unknown"] = true
	unknownPayload, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		payload json.RawMessage
		want    string
	}{
		{name: "unknown field", payload: unknownPayload, want: "unknown field"},
		{name: "model registry missing", payload: mutateRealtimeCUApplication(t, application, func(config *realtimecu.ApplicationConfig) {
			config.Model.Reference = "model://test/realtime-cu/uninstalled/v1"
		}), want: "model registry is missing"},
		{name: "model artifact drift", payload: mutateRealtimeCUApplication(t, application, func(config *realtimecu.ApplicationConfig) {
			config.Model.Artifact.Revision = "v2"
		}), want: "artifact or descriptor drifted"},
		{name: "model descriptor drift", payload: mutateRealtimeCUApplication(t, application, func(config *realtimecu.ApplicationConfig) {
			config.Model.Descriptor.Model = "realtime-cu-drifted"
		}), want: "artifact or descriptor drifted"},
		{name: "observer registry missing", payload: mutateRealtimeCUApplication(t, application, func(config *realtimecu.ApplicationConfig) {
			config.Observer.Reference = "observer://test/realtime-cu/uninstalled/v1"
		}), want: "observer registry is missing"},
		{name: "observer artifact drift", payload: mutateRealtimeCUApplication(t, application, func(config *realtimecu.ApplicationConfig) {
			config.Observer.Artifact.Revision = "v2"
		}), want: "identity or source contract drifted"},
		{name: "observer source escalation", payload: mutateRealtimeCUApplication(t, application, func(config *realtimecu.ApplicationConfig) {
			config.Observer.Sources = append(config.Observer.Sources, "desktop")
		}), want: "sources must be exactly"},
		{name: "target source escalation", payload: mutateRealtimeCUApplication(t, application, func(config *realtimecu.ApplicationConfig) {
			config.Target.Sources = append(config.Target.Sources, realtimecu.SourceCamera)
		}), want: "own only screen"},
		{name: "model effect authority escalation", payload: mutateRealtimeCUApplication(t, application, func(config *realtimecu.ApplicationConfig) {
			config.Model.Descriptor.ToolAuthority = continuation.ToolAuthorityExecute
		}), want: "proposal-only and silent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			drifted := profile.Clone()
			drifted.Application.Configuration = test.payload
			drifted, err = launchprofile.Freeze(drifted)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := serverplugin.NewProfileGraphBundle(context.Background(),
				serverplugin.ProfileGraphBundleConfig{
					Profile: drifted, Applications: registry, GatewayArtifact: gatewayArtifact,
				}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("profile drift error = %v, want %q", err, test.want)
			}
			if modelAcquisitions.Load() != 0 || observerAcquisitions.Load() != 0 {
				t.Fatalf("rejected profile acquired resources: model=%d observer=%d",
					modelAcquisitions.Load(), observerAcquisitions.Load())
			}
		})
	}
}

func marshalRealtimeCUApplication(t *testing.T, config realtimecu.ApplicationConfig) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mutateRealtimeCUApplication(
	t *testing.T, source realtimecu.ApplicationConfig,
	mutate func(*realtimecu.ApplicationConfig),
) json.RawMessage {
	t.Helper()
	copy := source
	copy.Observer.Sources = append([]string(nil), source.Observer.Sources...)
	copy.Target.Sources = append([]string(nil), source.Target.Sources...)
	mutate(&copy)
	return marshalRealtimeCUApplication(t, copy)
}
