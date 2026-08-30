package graphnative_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/graph"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	meetinggraph "github.com/bojieli/OpenRealtime/meeting/graphnative"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
)

type meetingApplicationFixture struct {
	host            meetinggraph.ApplicationHostConfig
	config          meetinggraph.ApplicationConfig
	gatewayArtifact inspect.ArtifactIdentity
}

func newMeetingApplicationFixture(
	t testing.TB, fixture providerFixture,
) meetingApplicationFixture {
	t.Helper()
	options := fixture.config.PlanOptions
	options.Revision = 0
	options.OptionalDependencies = nil
	applicationArtifact := dependencyArtifact("meeting-application")
	providerArtifact := dependencyArtifact("meeting-session-provider")
	adapter := fixture.config.Adapter
	return meetingApplicationFixture{
		host: meetinggraph.ApplicationHostConfig{
			ApplicationArtifact: applicationArtifact,
			ProviderArtifact:    providerArtifact,
			Artifacts:           fixture.config.Artifacts,
			PlanOptions:         options,
			Plugins:             fixture.config.Plugins,
			SecretCatalog:       fixture.config.SecretCatalog,
			Adapters:            []meetinggraph.AdapterPluginConfig{adapter},
			Inspection:          fixture.config.Inspection,
			ShutdownTimeout:     fixture.config.ShutdownTimeout,
			TraceRecording:      fixture.config.TraceRecording,
		},
		config: meetinggraph.ApplicationConfig{
			FormatVersion: meetinggraph.ApplicationFormatVersion,
			Artifacts: meetinggraph.ApplicationArtifactPaths{
				Topology:   fixture.config.Artifacts.Topology.Path,
				Values:     fixture.config.Artifacts.Values.Path,
				Lock:       fixture.config.Artifacts.Lock.Path,
				Channels:   fixture.config.Artifacts.Channels.Path,
				Deployment: fixture.config.Artifacts.Deployment.Path,
			},
			Plan: meetinggraph.ApplicationPlanSelection{Revision: 1},
			Adapter: launchprofile.AdapterSelection{
				Reference: adapter.Reference, RuntimeArtifact: adapter.Artifact,
				ProfileName: adapter.Profile.Name, ProfileRevision: adapter.Profile.Revision,
			},
		},
		gatewayArtifact: dependencyArtifact("meeting-realtime-gateway"),
	}
}

func marshalMeetingApplicationConfig(
	t testing.TB, config meetinggraph.ApplicationConfig,
) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func freezeMeetingLaunchProfile(
	t testing.TB,
	registration launchprofile.Registration,
	application meetingApplicationFixture,
) (launchprofile.Document, *graphconfig.Plan) {
	t.Helper()
	raw := marshalMeetingApplicationConfig(t, application.config)
	launch, err := registration.Factory(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := graphlaunch.New(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion,
		Name:          "openrealtime.meeting.fixture",
		Revision:      1,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: registration.Reference, Artifact: registration.Artifact,
			},
			Configuration: raw,
		},
		Plan:    prepared.Plan.Identity(),
		Adapter: application.config.Adapter,
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.meeting-fixture", ProfileRevision: 1,
			ProviderArtifact:   registration.ProviderArtifact,
			GatewayArtifact:    application.gatewayArtifact,
			Model:              "meeting-graph-fixture",
			TranscriptionModel: "meeting-transcription-fixture",
			ValidateWire:       true, InspectionTokenTTLMS: 60_000,
			MaxAudioFrameBytes: 1 << 20, VideoLimits: openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile, prepared.Plan
}

func TestMeetingApplicationRegistrationResolvesExactProfileWithoutResources(t *testing.T) {
	fixture := newProviderFixture(t)
	application := newMeetingApplicationFixture(t, fixture)
	application.host.Adapters[0].Profile.AdditionalObservers = []string{"meeting-notes"}
	registration, err := meetinggraph.NewApplicationRegistration(application.host)
	if err != nil {
		t.Fatal(err)
	}
	if registration.Reference != meetinggraph.ApplicationReference ||
		registration.Artifact != application.host.ApplicationArtifact ||
		registration.ProviderArtifact != application.host.ProviderArtifact {
		t.Fatalf("meeting application registration = %+v", registration)
	}

	// Every mutable host input must have been snapshotted by registration.
	application.host.Artifacts.Topology.Data[0] ^= 0xff
	application.host.Adapters[0].Profile.AdditionalObservers[0] = "mutated-observer"
	application.host.Plugins.Assembly.Dependencies[0].Name = "mutated-dependency"
	application.host.SecretCatalog.Secrets["mutated"] = application.host.SecretCatalog.Secrets["missing"]

	profile, previewPlan := freezeMeetingLaunchProfile(t, registration, application)
	registry, err := launchprofile.NewRegistry([]launchprofile.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Resolve(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Plan.Identity() != previewPlan.Identity() ||
		resolved.Binding.Name() != application.config.Adapter.ProfileName {
		t.Fatalf("resolved meeting profile plan=%+v binding=%q",
			resolved.Plan.Identity(), resolved.Binding.Name())
	}
	if observers := resolved.Binding.Capabilities().Observers; !slices.Equal(observers, []string{"meeting-notes", "screen"}) {
		t.Fatalf("snapshotted meeting observers = %v", observers)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatalf("profile resolution acquired mount=%d adapter=%d",
			fixture.mountFactories.Load(), fixture.adapterFactories.Load())
	}
}

func TestMeetingApplicationConfigurationFailsClosedBeforeSessionFactories(t *testing.T) {
	fixture := newProviderFixture(t)
	application := newMeetingApplicationFixture(t, fixture)
	registration, err := meetinggraph.NewApplicationRegistration(application.host)
	if err != nil {
		t.Fatal(err)
	}
	valid := marshalMeetingApplicationConfig(t, application.config)
	mutate := func(change func(*meetinggraph.ApplicationConfig)) json.RawMessage {
		config := application.config
		config.Plan.OptionalDependencies = slices.Clone(config.Plan.OptionalDependencies)
		change(&config)
		return marshalMeetingApplicationConfig(t, config)
	}
	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "unknown-field", raw: append([]byte(`{"unknown":true,`), valid[1:]...)},
		{name: "duplicate-field", raw: append([]byte(`{"format_version":1,`), valid[1:]...)},
		{name: "trailing-value", raw: append(slices.Clone(valid), []byte(`{}`)...)},
		{name: "format-version", raw: mutate(func(config *meetinggraph.ApplicationConfig) {
			config.FormatVersion++
		})},
		{name: "zero-plan-revision", raw: mutate(func(config *meetinggraph.ApplicationConfig) {
			config.Plan.Revision = 0
		})},
		{name: "unsorted-optional-dependencies", raw: mutate(func(config *meetinggraph.ApplicationConfig) {
			config.Plan.OptionalDependencies = []string{"z", "a"}
		})},
		{name: "duplicate-optional-dependencies", raw: mutate(func(config *meetinggraph.ApplicationConfig) {
			config.Plan.OptionalDependencies = []string{"a", "a"}
		})},
		{name: "artifact-path-drift", raw: mutate(func(config *meetinggraph.ApplicationConfig) {
			config.Artifacts.Topology = "different.ortg"
		})},
		{name: "unknown-adapter", raw: mutate(func(config *meetinggraph.ApplicationConfig) {
			config.Adapter.Reference = "go://openrealtime/meeting-adapters/unknown/v1"
		})},
		{name: "adapter-artifact-drift", raw: mutate(func(config *meetinggraph.ApplicationConfig) {
			config.Adapter.RuntimeArtifact.Revision = "different-build"
		})},
		{name: "adapter-profile-drift", raw: mutate(func(config *meetinggraph.ApplicationConfig) {
			config.Adapter.ProfileRevision++
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := registration.Factory(context.Background(), test.raw); err == nil {
				t.Fatal("meeting application configuration unexpectedly resolved")
			}
			if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
				t.Fatalf("rejected configuration acquired mount=%d adapter=%d",
					fixture.mountFactories.Load(), fixture.adapterFactories.Load())
			}
		})
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	want := errors.New("cancel meeting application resolution")
	cancel(want)
	if _, err := registration.Factory(ctx, valid); !errors.Is(err, want) {
		t.Fatalf("cancelled application resolution error = %v", err)
	}
	if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
		t.Fatal("cancelled application resolution acquired a session resource")
	}
}

func TestMeetingApplicationRegistrationRejectsAmbiguousHostInventory(t *testing.T) {
	tests := []struct {
		name   string
		change func(*meetinggraph.ApplicationHostConfig)
		match  string
	}{
		{name: "no-adapter", change: func(host *meetinggraph.ApplicationHostConfig) {
			host.Adapters = nil
		}, match: "at least one exact adapter"},
		{name: "duplicate-adapter", change: func(host *meetinggraph.ApplicationHostConfig) {
			host.Adapters = append(host.Adapters, host.Adapters[0])
		}, match: "installed more than once"},
		{name: "host-revision-authority", change: func(host *meetinggraph.ApplicationHostConfig) {
			host.PlanOptions.Revision = 2
		}, match: "belong to profile JSON"},
		{name: "host-loader-authority", change: func(host *meetinggraph.ApplicationHostConfig) {
			host.PlanOptions.Loader = graph.FileLoader{}
		}, match: "only safety limits"},
		{name: "invalid-application-artifact", change: func(host *meetinggraph.ApplicationHostConfig) {
			host.ApplicationArtifact = inspect.ArtifactIdentity{ID: "application-without-revision"}
		}, match: "application artifact"},
		{name: "invalid-provider-artifact", change: func(host *meetinggraph.ApplicationHostConfig) {
			host.ProviderArtifact = inspect.ArtifactIdentity{ID: "provider-without-revision"}
		}, match: "provider artifact"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProviderFixture(t)
			application := newMeetingApplicationFixture(t, fixture)
			test.change(&application.host)
			_, err := meetinggraph.NewApplicationRegistration(application.host)
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("NewApplicationRegistration() error = %v, want %q", err, test.match)
			}
			if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
				t.Fatal("rejected host inventory acquired a session resource")
			}
		})
	}
}

func TestMeetingProfileCompositionRejectsIdentityDriftBeforeSessionFactories(t *testing.T) {
	fixture := newProviderFixture(t)
	application := newMeetingApplicationFixture(t, fixture)
	registration, err := meetinggraph.NewApplicationRegistration(application.host)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := freezeMeetingLaunchProfile(t, registration, application)
	registry, err := launchprofile.NewRegistry([]launchprofile.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name            string
		change          func(*launchprofile.Document)
		gatewayArtifact inspect.ArtifactIdentity
	}{
		{name: "application-artifact", change: func(profile *launchprofile.Document) {
			profile.Application.Artifact = dependencyArtifact("different-meeting-application")
		}},
		{name: "provider-artifact", change: func(profile *launchprofile.Document) {
			profile.Server.ProviderArtifact = dependencyArtifact("different-session-provider")
		}},
		{name: "adapter-selection", change: func(profile *launchprofile.Document) {
			profile.Adapter.RuntimeArtifact = dependencyArtifact("different-meeting-adapter")
		}},
		{name: "installed-gateway-artifact", gatewayArtifact: dependencyArtifact("different-realtime-gateway")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			drifted := profile.Clone()
			if test.change != nil {
				test.change(&drifted)
				var err error
				drifted, err = launchprofile.Freeze(drifted)
				if err != nil {
					t.Fatal(err)
				}
			}
			gatewayArtifact := test.gatewayArtifact
			if gatewayArtifact == (inspect.ArtifactIdentity{}) {
				gatewayArtifact = application.gatewayArtifact
			}
			if _, err := serverplugin.NewProfileGraphBundle(
				context.Background(), serverplugin.ProfileGraphBundleConfig{
					Profile: drifted, Applications: registry,
					GatewayArtifact: gatewayArtifact, Gateway: gateway.Config{},
				},
			); err == nil {
				t.Fatal("drifted meeting profile unexpectedly composed")
			}
			if fixture.mountFactories.Load() != 0 || fixture.adapterFactories.Load() != 0 {
				t.Fatalf("drifted profile acquired mount=%d adapter=%d",
					fixture.mountFactories.Load(), fixture.adapterFactories.Load())
			}
		})
	}
}
