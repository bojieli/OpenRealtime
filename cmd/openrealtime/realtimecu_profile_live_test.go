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

	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/management"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/coder/websocket"
)

// TestRealtimeCUProductionProfileUsesAuthenticatedLiveDeploymentsAndInspection
// is opt-in because it binds the actual loopback Qwen and Whisper listeners
// and independently hashes their selected model/runtime bytes. It exercises
// the normal shared Realtime and live-inspection APIs; no Realtime-CU-specific
// route or UI is installed.
func TestRealtimeCUProductionProfileUsesAuthenticatedLiveDeploymentsAndInspection(t *testing.T) {
	if os.Getenv("OPENREALTIME_CU_LOCAL_DEPLOYMENT_E2E") != "1" {
		t.Skip("set OPENREALTIME_CU_LOCAL_DEPLOYMENT_E2E=1 with strict local backends running")
	}
	verifier, err := newLocalRealtimeCUDeploymentVerifier()
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
	options := defaultRealtimeCUProfileOptions()
	options.deployments = deployments
	options.deploymentVerifier = verifier
	const deploymentToken = "realtime-cu-live-profile-deployment-token"
	t.Setenv(options.tokenEnv, deploymentToken)
	t.Setenv(realtimeCULocalDeploymentEnvironment, "1")
	frozen, err := freezeProductionRealtimeCUProfile(
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
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if composition.GatewayToken != deploymentToken || composition.Host.RealtimeCU == nil ||
		composition.Graph.GraphPlan.Identity() != frozen.Profile.Plan ||
		len(composition.Graph.Readiness) != 2 {
		t.Fatalf("Realtime-CU production composition token=%t host=%p plan=%+v readiness=%d",
			composition.GatewayToken == deploymentToken, composition.Host.RealtimeCU,
			composition.Graph.GraphPlan.Identity(), len(composition.Graph.Readiness))
	}
	realm, err := composition.Graph.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close Realtime-CU server realm: %v", err)
		}
	})
	server := httptest.NewServer(realm.Handler())
	t.Cleanup(server.Close)
	for _, path := range []string{"/", "/ui", "/realtime-cu", "/computer-use", "/scenario"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("Realtime-CU plug-in installed route %q: status %d", path, response.StatusCode)
		}
	}
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/v1/realtime?model=" + frozen.Profile.Server.Model
	unauthorized, response, err := websocket.Dial(context.Background(), endpoint, nil)
	if unauthorized != nil {
		_ = unauthorized.Close(websocket.StatusPolicyViolation, "authentication required")
		t.Fatal("Realtime-CU endpoint accepted a connection without deployment authentication")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Realtime-CU endpoint response=%v error=%v", response, err)
	}
	_ = response.Body.Close()

	header := http.Header{"Authorization": []string{"Bearer " + deploymentToken}}
	connection, _, err := websocket.Dial(context.Background(), endpoint,
		&websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "Realtime-CU profile test complete")
	})
	client := &serveProfileWireClient{t: t, connection: connection}
	client.awaitType(15*time.Second, "session.created")
	waitDefinition, found := computeruse.Lookup(computeruse.Wait)
	if !found {
		t.Fatal("standard computer.wait declaration is unavailable")
	}
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
			},
			"tools": []map[string]any{{
				"type": "function", "name": waitDefinition.Name,
				"description": waitDefinition.Description, "parameters": waitDefinition.Parameters,
				"openrealtime": map[string]any{"confirm": "never", "target": options.targetName},
			}},
			"openrealtime": map[string]any{
				"version": openrealtime.Version,
				"supports": []string{
					string(openrealtime.FeatureVideoInput), string(openrealtime.FeatureObservations),
					string(openrealtime.FeatureComputerUse),
				},
				"observers": []string{realtimeCULocalObserverName},
				"debug":     map[string]any{"enabled": true, "categories": []string{"session"}},
			},
		},
	})
	updated := client.awaitType(15*time.Second, "session.updated")
	access := realtimeCUInspectionAccess(t, updated)
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, server.URL+access.Path, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(management.CapabilityHeader, access.Token)
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
		t.Fatalf("Realtime-CU authenticated inspection status=%d payload=%s",
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
		t.Fatalf("Realtime-CU live graph evidence = %+v, graph=%+v", live, graph)
	}
}

func realtimeCUInspectionAccess(t testing.TB, updated map[string]any) openrealtime.InspectionAccess {
	t.Helper()
	session, ok := updated["session"].(map[string]any)
	if !ok {
		t.Fatalf("Realtime-CU session.updated omitted session: %+v", updated)
	}
	extension, ok := session["openrealtime"].(map[string]any)
	if !ok {
		t.Fatalf("Realtime-CU session.updated omitted OpenRealtime extension: %+v", session)
	}
	debug, ok := extension["debug"].(map[string]any)
	if !ok {
		t.Fatalf("Realtime-CU session.updated omitted debug response: %+v", extension)
	}
	raw, found := debug["inspection"]
	if !found || raw == nil {
		t.Fatalf("Realtime-CU session did not receive inspection capability: %+v", debug)
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var access openrealtime.InspectionAccess
	if err := json.Unmarshal(payload, &access); err != nil {
		t.Fatal(err)
	}
	if access.Path == "" || access.SessionID == "" || access.Token == "" || access.ExpiresAtMS == 0 {
		t.Fatalf("Realtime-CU inspection capability is incomplete: %+v", access)
	}
	return access
}
