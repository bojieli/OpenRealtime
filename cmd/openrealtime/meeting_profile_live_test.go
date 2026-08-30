package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/coder/websocket"
)

// TestMeetingProductionProfileUsesAuthenticatedLiveDeploymentsAndInspection
// is opt-in because it independently hashes the actual Qwen, SenseVoice, and
// Fish Speech deployments before opening the normal shared Realtime server.
// The Meeting application contributes a graph/session plug-in only: no
// Meeting-specific listener, UI, or management route is installed.
func TestMeetingProductionProfileUsesAuthenticatedLiveDeploymentsAndInspection(t *testing.T) {
	if os.Getenv("OPENREALTIME_MEETING_LOCAL_DEPLOYMENT_E2E") != "1" {
		t.Skip("set OPENREALTIME_MEETING_LOCAL_DEPLOYMENT_E2E=1 with strict local backends running")
	}
	verifier, err := newLocalMeetingDeploymentVerifier()
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := verifier.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	options := defaultMeetingProfileOptions()
	options.deployments = deployments
	options.verifier = verifier
	const deploymentToken = "meeting-live-profile-deployment-token"
	t.Setenv(options.tokenEnv, deploymentToken)
	t.Setenv(meetingLocalDeploymentEnvironment, "1")
	t.Setenv(realtimeCULocalDeploymentEnvironment, "")
	frozen, err := freezeProductionMeetingProfile(
		context.Background(), options, artifacts.Gateway,
	)
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeServeProfileTestDocument(t, frozen.Profile)
	serveOptions := serveProfileTestOptions()
	serveOptions.launchProfile = profilePath
	composition, err := newProductionProfiledServeComposition(
		context.Background(), serveOptions,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if composition.GatewayToken != deploymentToken || composition.Host.Meeting == nil ||
		composition.Host.RealtimeCU != nil ||
		composition.Graph.GraphPlan.Identity() != frozen.Profile.Plan ||
		len(composition.Graph.Readiness) != 3 {
		t.Fatalf("Meeting production composition token=%t meeting=%p realtime-cu=%p plan=%+v readiness=%d",
			composition.GatewayToken == deploymentToken, composition.Host.Meeting,
			composition.Host.RealtimeCU, composition.Graph.GraphPlan.Identity(),
			len(composition.Graph.Readiness))
	}
	realm, err := composition.Graph.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close Meeting server realm: %v", err)
		}
	})
	server := httptest.NewServer(realm.Handler())
	t.Cleanup(server.Close)
	for _, path := range []string{"/", "/ui", "/meeting", "/meeting-assistant", "/scenario"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("Meeting plug-in installed route %q: status %d", path, response.StatusCode)
		}
	}
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/v1/realtime?model=" + frozen.Profile.Server.Model
	unauthorized, response, err := websocket.Dial(context.Background(), endpoint, nil)
	if unauthorized != nil {
		_ = unauthorized.Close(websocket.StatusPolicyViolation, "authentication required")
		t.Fatal("Meeting endpoint accepted a connection without deployment authentication")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Meeting endpoint response=%v error=%v", response, err)
	}
	_ = response.Body.Close()

	header := http.Header{"Authorization": []string{"Bearer " + deploymentToken}}
	connection, _, err := websocket.Dial(context.Background(), endpoint,
		&websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "Meeting profile test complete")
	})
	client := &meetingProfileWireClient{t: t, connection: connection}
	client.awaitType(30*time.Second, "session.created")
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
			},
			"openrealtime": map[string]any{
				"version": openrealtime.Version,
				"supports": []string{
					string(openrealtime.FeatureVideoInput), string(openrealtime.FeatureObservations),
				},
				"observers": []string{"screen"},
				"debug":     map[string]any{"enabled": true, "categories": []string{"session"}},
			},
		},
	})
	updated := client.awaitType(30*time.Second, "session.updated")
	access := meetingInspectionAccess(t, updated)
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, server.URL+access.Path, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+deploymentToken)
	request.Header.Set(gateway.InspectionTokenHeader, access.Token)
	inspectionResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer inspectionResponse.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(inspectionResponse.Body, 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	if inspectionResponse.StatusCode != http.StatusOK {
		t.Fatalf("Meeting authenticated inspection status=%d payload=%s",
			inspectionResponse.StatusCode, payload)
	}
	var live inspect.Live
	if err := json.Unmarshal(payload, &live); err != nil {
		t.Fatal(err)
	}
	graph := frozen.Plan.Graph()
	if live.GraphID != graph.ID || live.GraphRevision != graph.Revision ||
		live.Fingerprint != graph.Fingerprint || live.Adapter == nil ||
		live.Adapter.Implementation != frozen.Profile.Adapter.Reference ||
		live.Adapter.Runtime != frozen.Profile.Adapter.RuntimeArtifact ||
		live.Adapter.ProfileFingerprint == "" || len(live.Nodes) != len(graph.Nodes) {
		t.Fatalf("Meeting live graph evidence = %+v, graph=%+v", live, graph)
	}
}
