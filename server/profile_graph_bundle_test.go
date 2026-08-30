package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	"github.com/bojieli/OpenRealtime/gateway"
	adaptivevideo "github.com/bojieli/OpenRealtime/graph/binding/adaptivevideo"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/perception"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/coder/websocket"
)

func TestProfileGraphBundleExactMatchesPluginsBeforeTokenOrResources(t *testing.T) {
	var acquisitions atomic.Int64
	providerDescriptor := perceptionelements.VisualProviderDescriptor{
		Name: "profiled-graph-narrator", Revision: "model-test-1",
		Digest: "sha256:" + strings.Repeat("1", 64),
	}
	dependency, err := adaptivevideo.NewProviderDependency(
		serverArtifact("go://openrealtime/test/profile-provider-registry", "build-1", "2"),
		[]adaptivevideo.ProviderRegistration{{
			Reference: "visual.youtube.narrator.v1", Descriptor: providerDescriptor,
			Factory: func() (perceptionelements.VisualProvider, error) {
				acquisitions.Add(1)
				return &deterministicVisualProvider{
					descriptor: providerDescriptor, narrated: make(chan perception.Frame, 1),
				}, nil
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	adapterArtifact := serverArtifact(
		"go://openrealtime/graph-adapters/profile-adaptive-video", "build-profile-1", "3",
	)
	launchConfig := adaptiveVideoLaunchConfig(t, dependency, adapterArtifact)
	var readinessCalls atomic.Int64
	launchConfig.Readiness = []graphlaunch.ReadinessCheck{{
		Name:  "visual.youtube.narrator.v1",
		Check: func(context.Context) error { readinessCalls.Add(1); return nil },
	}}
	preview, err := graphlaunch.New(context.Background(), launchConfig)
	if err != nil {
		t.Fatal(err)
	}
	if acquisitions.Load() != 0 || readinessCalls.Load() != 0 || len(preview.Readiness) != 1 {
		t.Fatal("profile preview acquired a provider resource")
	}

	applicationArtifact := serverArtifact(
		"go://openrealtime/graph-applications/adaptive-video", "build-profile-1", "4",
	)
	providerArtifact := serverArtifact(
		"go://openrealtime/server-providers/profiled-graph", "build-profile-1", "5",
	)
	gatewayArtifact := serverArtifact(
		"go://openrealtime/server-gateways/profiled-realtime", "build-profile-1", "6",
	)
	profile, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion,
		Name:          "openrealtime.launch.adaptive-video-test",
		Revision:      1,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: "application.openrealtime.adaptive-video.v1", Artifact: applicationArtifact,
			},
			Configuration: json.RawMessage(
				`{"enabled":true,"max_frames":3,"profile":"adaptive-video","tags":["screen","youtube"]}`,
			),
		},
		Plan: preview.Plan.Identity(),
		Adapter: launchprofile.AdapterSelection{
			Reference:       launchConfig.Adapter.Reference,
			RuntimeArtifact: launchConfig.Adapter.RuntimeArtifact,
			ProfileName:     launchConfig.Adapter.ProfileName, ProfileRevision: launchConfig.Adapter.ProfileRevision,
		},
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.profiled-graph-test", ProfileRevision: 1,
			ProviderArtifact: providerArtifact,
			GatewayArtifact:  gatewayArtifact,
			Model:            "profiled-graph-e2e", TranscriptionModel: "profiled-perception-e2e",
			TokenEnvironment: "OPENREALTIME_PROFILE_TEST_TOKEN", ValidateWire: true,
			InspectionTokenTTLMS: 60_000, MaxAudioFrameBytes: 1 << 20,
			VideoLimits: openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	yamlProfile, err := launchprofile.MarshalYAML(profile)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := launchprofile.ParseYAML("profile.yaml", yamlProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !documentsEqual(profile, parsed) {
		t.Fatalf("profile YAML round trip drifted:\n%s", yamlProfile)
	}
	for _, mutation := range []struct {
		name string
		from string
		to   string
	}{
		{name: "unknown", from: "name: openrealtime.launch.adaptive-video-test\n", to: "name: openrealtime.launch.adaptive-video-test\nunknown: true\n"},
		{name: "duplicate", from: "name: openrealtime.launch.adaptive-video-test\n", to: "name: openrealtime.launch.adaptive-video-test\nname: duplicate\n"},
	} {
		t.Run("strict YAML rejects "+mutation.name, func(t *testing.T) {
			tampered := bytes.Replace(yamlProfile, []byte(mutation.from), []byte(mutation.to), 1)
			if _, err := launchprofile.ParseYAML("tampered.yaml", tampered); err == nil {
				t.Fatalf("strict profile accepted %s field", mutation.name)
			}
		})
	}

	var applicationCalls atomic.Int64
	registry, err := launchprofile.NewRegistry([]launchprofile.Registration{{
		Reference: profile.Application.Reference, Artifact: applicationArtifact,
		ProviderArtifact: providerArtifact,
		Factory: func(_ context.Context, source json.RawMessage) (graphlaunch.Config, error) {
			applicationCalls.Add(1)
			decoder := json.NewDecoder(bytes.NewReader(source))
			decoder.DisallowUnknownFields()
			var config struct {
				Profile   string   `json:"profile"`
				Enabled   bool     `json:"enabled"`
				MaxFrames int      `json:"max_frames"`
				Tags      []string `json:"tags"`
			}
			if err := decoder.Decode(&config); err != nil {
				return graphlaunch.Config{}, err
			}
			if config.Profile != "adaptive-video" || !config.Enabled || config.MaxFrames != 3 ||
				len(config.Tags) != 2 || config.Tags[0] != "screen" || config.Tags[1] != "youtube" {
				return graphlaunch.Config{}, errors.New("wrong application profile")
			}
			return launchConfig, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var tokenCalls atomic.Int64
	resolveToken := func(_ context.Context, environment string) (string, error) {
		tokenCalls.Add(1)
		if environment != "OPENREALTIME_PROFILE_TEST_TOKEN" {
			return "", errors.New("unexpected token environment")
		}
		return "profile-test-token", nil
	}

	if _, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: profile, Applications: registry,
			GatewayArtifact: serverArtifact(
				"go://openrealtime/server-gateways/profiled-realtime", "build-profile-drift", "6",
			),
			ResolveToken: resolveToken,
		}); err == nil || !strings.Contains(err.Error(), "installed gateway artifact drifted") {
		t.Fatalf("drifted gateway artifact error = %v", err)
	}
	providerDrift := profile.Clone()
	providerDrift.Server.ProviderArtifact.Revision = "build-profile-drift"
	providerDrift, err = launchprofile.Freeze(providerDrift)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: providerDrift, Applications: registry, GatewayArtifact: gatewayArtifact,
			ResolveToken: resolveToken,
		}); err == nil || !strings.Contains(err.Error(), "session-provider artifact drifted") {
		t.Fatalf("drifted provider artifact error = %v", err)
	}
	if applicationCalls.Load() != 0 || tokenCalls.Load() != 0 || acquisitions.Load() != 0 {
		t.Fatal("drifted installed artifact reached a factory, token, or provider")
	}

	drifted := profile.Clone()
	drifted.Application.Artifact.Revision = "build-profile-drift"
	drifted, err = launchprofile.Freeze(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: drifted, Applications: registry, GatewayArtifact: gatewayArtifact,
			ResolveToken: resolveToken,
		}); err == nil || !strings.Contains(err.Error(), "runtime artifact drifted") {
		t.Fatalf("drifted application error = %v", err)
	}
	if applicationCalls.Load() != 0 || tokenCalls.Load() != 0 || acquisitions.Load() != 0 {
		t.Fatal("drifted profile reached a factory, token, or provider")
	}

	unknownConfiguration := profile.Clone()
	unknownConfiguration.Application.Configuration = json.RawMessage(
		`{"enabled":true,"max_frames":3,"profile":"adaptive-video","tags":["screen","youtube"],"unknown":true}`,
	)
	unknownConfiguration, err = launchprofile.Freeze(unknownConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: unknownConfiguration, Applications: registry, GatewayArtifact: gatewayArtifact,
			ResolveToken: resolveToken,
		}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("plugin-owned unknown configuration error = %v", err)
	}
	if applicationCalls.Load() != 1 || tokenCalls.Load() != 0 || acquisitions.Load() != 0 {
		t.Fatal("invalid plugin configuration reached token or provider acquisition")
	}

	if _, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: profile, Applications: registry, GatewayArtifact: gatewayArtifact,
			ResolveToken: resolveToken,
			Gateway:      gateway.Config{Model: "competing-model"},
		}); err == nil || !strings.Contains(err.Error(), "settings belong to the launch profile") {
		t.Fatalf("competing gateway setting error = %v", err)
	}
	if applicationCalls.Load() != 1 || tokenCalls.Load() != 0 || acquisitions.Load() != 0 {
		t.Fatal("competing gateway setting reached application, token, or provider")
	}

	cancelCtx, cancel := context.WithCancelCause(context.Background())
	resolverCancellation := errors.New("profile token resolver cancelled launch")
	if _, err := serverplugin.NewProfileGraphBundle(cancelCtx,
		serverplugin.ProfileGraphBundleConfig{
			Profile: profile, Applications: registry, GatewayArtifact: gatewayArtifact,
			ResolveToken: func(_ context.Context, _ string) (string, error) {
				tokenCalls.Add(1)
				cancel(resolverCancellation)
				return "profile-test-token", nil
			},
		}); !errors.Is(err, resolverCancellation) {
		t.Fatalf("resolver cancellation error = %v", err)
	}
	if applicationCalls.Load() != 2 || tokenCalls.Load() != 1 || acquisitions.Load() != 0 {
		t.Fatal("cancelled token resolution acquired a provider or lost call evidence")
	}

	composition, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: profile, Applications: registry, GatewayArtifact: gatewayArtifact,
			ResolveToken: resolveToken,
		})
	if err != nil {
		t.Fatal(err)
	}
	if applicationCalls.Load() != 3 || tokenCalls.Load() != 2 || acquisitions.Load() != 0 {
		t.Fatalf("composition calls application=%d token=%d provider=%d",
			applicationCalls.Load(), tokenCalls.Load(), acquisitions.Load())
	}
	if composition.GraphPlan.Identity() != profile.Plan ||
		composition.ServerBundle.Profile.Name != profile.Server.ProfileName ||
		len(composition.Readiness) != 1 || readinessCalls.Load() != 0 {
		t.Fatal("profiled composition lost an exact public identity")
	}
	if err := composition.Readiness[0].Check(context.Background()); err != nil || readinessCalls.Load() != 1 {
		t.Fatalf("selected readiness callback = error %v calls %d", err, readinessCalls.Load())
	}

	realm, err := composition.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close profiled realm: %v", err)
		}
	})
	if acquisitions.Load() != 0 {
		t.Fatal("server mount acquired a per-session provider")
	}

	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)
	header := http.Header{"Authorization": []string{"Bearer profile-test-token"}}
	connection, _, err := websocket.Dial(
		context.Background(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/realtime?model=profiled-graph-e2e",
		&websocket.DialOptions{HTTPHeader: header},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "profile test complete")
	})
	created := awaitAdaptiveEvent(t, connection, "session.created")
	if created["type"] != "session.created" || acquisitions.Load() != 1 {
		t.Fatalf("profiled session created=%v acquisitions=%d", created["type"], acquisitions.Load())
	}
}

func documentsEqual(left, right launchprofile.Document) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
