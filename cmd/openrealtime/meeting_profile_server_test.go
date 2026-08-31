package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverprofile "github.com/bojieli/OpenRealtime/server"
	"github.com/coder/websocket"
)

// TestMeetingProductionProfileUsesTheSharedAuthenticatedRealtimeServer proves
// that the Meeting application is only a graph/session plug-in. It is served
// by the same Realtime and inspection APIs as every other application and does
// not install a Meeting-specific page, route, listener, or management plane.
func TestMeetingProductionProfileUsesTheSharedAuthenticatedRealtimeServer(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "fixture-background-credential")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("OPENREALTIME_LOCAL_API_KEY", "fixture-local-credential")
	t.Setenv("OPENREALTIME_ASR_API_KEY", "fixture-asr-credential")
	t.Setenv("OPENREALTIME_TTS_API_KEY", "fixture-tts-credential")

	executable := meetingProfileExecutable()
	deployments := meetingProfileDeployments()
	verifier := &fixtureMeetingDeploymentVerifier{identities: deployments}
	options := defaultMeetingProfileOptions()
	options.deployments = deployments
	options.verifier = verifier
	frozen, err := freezeProductionMeetingProfile(context.Background(), options, executable)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := newServeMeetingRegistration(
		context.Background(), executable, deployments, verifier,
	)
	if err != nil {
		t.Fatal(err)
	}
	applications, err := launchprofile.NewRegistry([]launchprofile.Registration{selected.Application})
	if err != nil {
		t.Fatal(err)
	}

	const deploymentToken = "meeting-profile-deployment-token"
	resolveCalls := 0
	bundle, err := serverprofile.NewProfileGraphBundle(context.Background(),
		serverprofile.ProfileGraphBundleConfig{
			Profile: frozen.Profile, Applications: applications,
			GatewayArtifact: executable,
			Gateway:         gateway.Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
			ResolveToken: func(ctx context.Context, name string) (string, error) {
				if err := context.Cause(ctx); err != nil {
					return "", err
				}
				resolveCalls++
				if name != options.tokenEnv {
					t.Fatalf("resolved token environment = %q, want %q", name, options.tokenEnv)
				}
				return deploymentToken, nil
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 1 || bundle.GraphPlan.Identity() != frozen.Profile.Plan ||
		len(bundle.Readiness) != 3 {
		t.Fatalf("Meeting server composition calls=%d plan=%+v readiness=%d",
			resolveCalls, bundle.GraphPlan.Identity(), len(bundle.Readiness))
	}

	realm, err := bundle.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		t.Fatal("Meeting Realtime endpoint accepted a connection without deployment authentication")
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
	created := client.awaitType(5*time.Second, "session.created")
	if created["type"] != "session.created" {
		t.Fatalf("Meeting session creation = %+v", created)
	}
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
			},
			"openrealtime": map[string]any{
				"version":  openrealtime.Version,
				"supports": []string{string(openrealtime.FeatureVideoInput)},
				"debug":    map[string]any{"enabled": true, "categories": []string{"session"}},
			},
		},
	})
	updated := client.awaitType(5*time.Second, "session.updated")
	updatedSession, ok := updated["session"].(map[string]any)
	if !ok {
		t.Fatalf("Meeting session.updated = %+v", updated)
	}
	extension, ok := updatedSession["openrealtime"].(map[string]any)
	if !ok {
		t.Fatalf("Meeting OpenRealtime negotiation = %+v", updatedSession)
	}
	video, ok := extension["video"].(map[string]any)
	fps, fpsOK := video["fps_cap"].(float64)
	if !ok || !fpsOK || int(fps) != (frozen.Configuration.Foreground.FrameRateMilliHz+999)/1_000 {
		t.Fatalf("Meeting negotiated video cadence = %+v, configured %d millihertz",
			video, frozen.Configuration.Foreground.FrameRateMilliHz)
	}
	access := meetingInspectionAccess(t, updated)
	live := awaitMeetingExactLiveResolution(t, endpoint, deploymentToken, access, frozen)
	graph := frozen.Plan.Graph()
	if live.GraphID != graph.ID || live.GraphRevision != graph.Revision ||
		live.Fingerprint != graph.Fingerprint || live.Adapter == nil ||
		live.Adapter.Implementation != frozen.Profile.Adapter.Reference ||
		live.Adapter.Runtime != frozen.Profile.Adapter.RuntimeArtifact ||
		live.Adapter.ProfileFingerprint == "" || len(live.Nodes) != len(graph.Nodes) {
		t.Fatalf("Meeting live graph evidence = %+v, graph=%+v", live, graph)
	}
}

func awaitMeetingExactLiveResolution(
	t testing.TB,
	endpoint string,
	deploymentToken string,
	access openrealtime.InspectionAccess,
	frozen frozenMeetingProfile,
) inspect.Live {
	t.Helper()
	client := bench.LiveInspectionClient{
		Endpoint: endpoint, DeploymentToken: deploymentToken,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for {
		snapshot, err := client.Snapshot(context.Background(), access)
		if err != nil {
			t.Fatalf("read authenticated Meeting inspection: %v", err)
		}
		resolution, err := bench.AuthorExpectedResolutionFromInspection(
			frozen.Plan.Graph(), frozen.Execution.Graph.Configuration, snapshot,
		)
		if err == nil {
			if !reflect.DeepEqual(resolution, frozen.Resolution) {
				t.Fatalf("authenticated Meeting resolution drifted\nfrozen: %+v\n  live: %+v",
					frozen.Resolution, resolution)
			}
			return snapshot
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("Meeting inspection never reached exact live resolution: %v", lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type meetingProfileWireClient struct {
	t          testing.TB
	connection *websocket.Conn
}

func (client *meetingProfileWireClient) send(event map[string]any) {
	client.t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		client.t.Fatal(err)
	}
	if err := client.connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		client.t.Fatal(err)
	}
}

func (client *meetingProfileWireClient) awaitType(
	timeout time.Duration, wanted string,
) map[string]any {
	client.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, payload, err := client.connection.Read(ctx)
		if err != nil {
			client.t.Fatalf("read Meeting profile endpoint: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			client.t.Fatal(err)
		}
		if event["type"] == "error" {
			client.t.Fatalf("Meeting profile endpoint error: %+v", event)
		}
		if event["type"] == wanted {
			return event
		}
	}
}

func meetingInspectionAccess(t testing.TB, updated map[string]any) openrealtime.InspectionAccess {
	t.Helper()
	session, ok := updated["session"].(map[string]any)
	if !ok {
		t.Fatalf("Meeting session.updated omitted session: %+v", updated)
	}
	extension, ok := session["openrealtime"].(map[string]any)
	if !ok {
		t.Fatalf("Meeting session.updated omitted OpenRealtime extension: %+v", session)
	}
	debug, ok := extension["debug"].(map[string]any)
	if !ok {
		t.Fatalf("Meeting session.updated omitted debug response: %+v", extension)
	}
	raw, found := debug["inspection"]
	if !found || raw == nil {
		t.Fatalf("Meeting session did not receive inspection capability: %+v", debug)
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
		t.Fatalf("Meeting inspection capability is incomplete: %+v", access)
	}
	return access
}
