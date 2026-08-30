package server_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	"github.com/bojieli/OpenRealtime/gateway"
	adaptivevideo "github.com/bojieli/OpenRealtime/graph/binding/adaptivevideo"
	"github.com/bojieli/OpenRealtime/perception"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/coder/websocket"
)

func TestGraphBundleComposesGenericLauncherIntoExactServerRealm(t *testing.T) {
	var acquisitions atomic.Int64
	providerDescriptor := perceptionelements.VisualProviderDescriptor{
		Name: "graph-bundle-narrator", Revision: "model-test-1",
		Digest: "sha256:" + strings.Repeat("6", 64),
	}
	dependency, err := adaptivevideo.NewProviderDependency(
		serverArtifact("go://openrealtime/test/graph-bundle-provider-registry", "build-1", "7"),
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
		"go://openrealtime/graph-adapters/adaptive-video", "build-graph-bundle", "8",
	)
	composition, err := serverplugin.NewGraphBundle(
		context.Background(),
		serverplugin.GraphBundleConfig{
			Graph: adaptiveVideoLaunchConfig(t, dependency, adapterArtifact),
			Server: serverplugin.BundleConfig{
				ProfileName: "openrealtime.server.graph-bundle-test", ProfileRevision: 1,
				Gateway: gateway.Config{Model: "graph-bundle-e2e", ValidateWire: true},
				ProviderArtifact: serverArtifact(
					"go://openrealtime/server-providers/graph-native", "build-1", "9",
				),
				GatewayArtifact: serverArtifact(
					"go://openrealtime/server-gateways/realtime", "build-1", "a",
				),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if acquisitions.Load() != 0 {
		t.Fatalf("resource acquisition during graph/server composition = %d", acquisitions.Load())
	}
	if composition.GraphPlan == nil || composition.ServerBundle == nil ||
		composition.GraphPlan.Identity().PlanFingerprint == "" ||
		composition.ServerBundle.Plan.Fingerprint == "" {
		t.Fatalf("graph bundle omitted immutable identities: %+v", composition)
	}

	realm, err := composition.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close graph bundle realm: %v", err)
		}
	})
	if acquisitions.Load() != 0 {
		t.Fatalf("resource acquisition during server mount = %d", acquisitions.Load())
	}

	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)
	connection, _, err := websocket.Dial(
		context.Background(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/realtime?model=graph-bundle-e2e",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "test complete")
	})
	created := awaitAdaptiveEvent(t, connection, "session.created")
	if created["type"] != "session.created" {
		t.Fatalf("graph bundle first event = %+v", created)
	}
	if acquisitions.Load() != 1 {
		t.Fatalf("per-session provider acquisitions = %d, want 1", acquisitions.Load())
	}
}

func TestGraphBundleRejectsCompetingOrCancelledProviderAuthority(t *testing.T) {
	config := serverplugin.GraphBundleConfig{Server: serverplugin.BundleConfig{
		Provider: serverTestProvider{},
	}}
	if _, err := serverplugin.NewGraphBundle(context.Background(), config); err == nil ||
		!strings.Contains(err.Error(), "provider must be supplied only by graph launch") {
		t.Fatalf("competing graph/server provider error = %v", err)
	}
	if _, err := serverplugin.NewGraphBundle(nil, serverplugin.GraphBundleConfig{}); err == nil ||
		!strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil graph bundle context error = %v", err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("cancel graph bundle"))
	if _, err := serverplugin.NewGraphBundle(ctx, serverplugin.GraphBundleConfig{}); err == nil ||
		err.Error() != "cancel graph bundle" {
		t.Fatalf("cancelled graph bundle error = %v", err)
	}
}
