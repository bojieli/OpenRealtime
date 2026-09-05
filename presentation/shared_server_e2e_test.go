package presentation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/macos"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
	"github.com/bojieli/OpenRealtime/presentation/host"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

const (
	sharedFixtureModel       = "shared-presentation-e2e"
	sharedFixtureInput       = "shared presentation contract turn"
	sharedFixtureReply       = "shared server response"
	sharedGraphID            = "shared-presentation-fixture"
	sharedGraphFingerprint   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	sharedValuesFingerprint  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	sharedElementFingerprint = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	sharedRuntimeFingerprint = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

// This test is the local half of the cross-client release contract. It runs a
// real Chromium client and then a native-manifest-derived wire probe against
// one unchanged descriptor-locked server and presentation host. The native
// probe deliberately does not claim to be a signed macOS application run;
// that remains the darwin-only signed gate. It does prove that the exact
// shipped macOS profile, Swift implementation artifacts, endpoint directory,
// and public protocol/management surfaces compose with the same server that
// the browser just used.
func TestBrowserAndMacOSProfilesUseOneUnchangedCleanServerAPI(t *testing.T) {
	node, chromium := requireSharedBrowser(t)
	provider := &sharedPresentationProvider{}
	serverBundle, err := serverplugin.NewBundle(serverplugin.BundleConfig{
		ProfileName: "openrealtime.server.presentation-shared-e2e", ProfileRevision: 1,
		Provider: provider,
		Gateway: gateway.Config{
			Model: sharedFixtureModel, ValidateWire: true, InspectionTokenTTL: 2 * time.Minute,
		},
		ProviderArtifact: sharedArtifact(
			"go://openrealtime/presentation/shared-e2e-provider", "fixture-build-1", "5",
		),
		GatewayArtifact: sharedArtifact(
			"go://openrealtime/presentation/shared-e2e-server", "fixture-build-1", "6",
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	realm, err := serverBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSharedRealm(t, "server", realm.Close) })
	serverHTTP := httptest.NewServer(realm.Handler())
	t.Cleanup(serverHTTP.Close)
	serverWebSocket := "ws" + strings.TrimPrefix(serverHTTP.URL, "http") + "/v1/realtime"
	serverManagement := serverHTTP.URL + management.APIPrefix

	assertServerOwnsNoPresentation(t, serverHTTP.URL)
	serverLive := realm.Live()
	assertActiveRealm(t, "server", serverLive, plugin.ServerRealm, serverBundle.Plan.Fingerprint)
	before := readSharedObservability(t, serverHTTP.URL)
	assertSharedServerHealth(t, before.health, serverBundle.Plan.Fingerprint)
	assertSharedMetrics(t, before.metrics, 0, 0)

	browserBundle, err := presentationbrowser.ObserverDeveloperBundle()
	if err != nil {
		t.Fatal(err)
	}
	hostRealm := mountSharedBrowserHost(
		t, browserBundle, serverWebSocket, serverManagement,
	)
	t.Cleanup(func() { closeSharedRealm(t, "presentation host", hostRealm.Close) })
	hostHandler, err := host.HTTPHandler(hostRealm.Mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	hostHTTP := httptest.NewServer(hostHandler)
	t.Cleanup(hostHTTP.Close)
	assertHostAndServerSurfacesAreSeparate(t, hostHTTP.URL, serverHTTP.URL, browserBundle.Manifest)
	hostBefore := hostRealm.Live()
	assertActiveRealm(
		t, "presentation host", hostBefore, plugin.PresentationHostRealm, hostRealm.Plan().Fingerprint,
	)

	nativeDirectory := sharedNativeEndpointDirectory(t, hostHTTP.URL)
	nativeBundle, err := macos.NewNativeBundleForDistributionWithEndpointDirectory(
		macos.NativeObserverDeveloperDistribution, nativeDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertCommonClientContracts(t, browserBundle, nativeBundle, hostHTTP.URL)

	runSharedBrowser(
		t, node, chromium, hostHTTP.URL, browserBundle.Plan.Fingerprint, sharedGraphFingerprint,
	)
	firstRuntime := provider.awaitSession(t, 0)
	firstRuntime.awaitClosed(t)
	assertRuntimeLifecycle(t, "browser", firstRuntime.operationsSnapshot())
	waitForSharedMetrics(t, serverHTTP.URL, 1, 0)
	middle := readSharedObservability(t, serverHTTP.URL)
	assertSharedServerHealth(t, middle.health, serverBundle.Plan.Fingerprint)
	assertSharedMetrics(t, middle.metrics, 1, 0)
	assertActiveRealm(t, "server after browser", realm.Live(), plugin.ServerRealm, serverLive.Fingerprint)
	assertSameRuntimeSelections(t, "server after browser", serverLive, realm.Live())
	assertSameRuntimeSelections(t, "host after browser", hostBefore, hostRealm.Live())

	runNativeManifestWireProbe(t, nativeBundle, sharedGraphFingerprint)
	secondRuntime := provider.awaitSession(t, 1)
	secondRuntime.awaitClosed(t)
	assertRuntimeLifecycle(t, "macOS manifest probe", secondRuntime.operationsSnapshot())
	if got := provider.sessionCount(); got != 2 {
		t.Fatalf("shared provider created %d sessions, want exactly browser then native", got)
	}
	waitForSharedMetrics(t, serverHTTP.URL, 2, 0)
	after := readSharedObservability(t, serverHTTP.URL)
	assertSharedServerHealth(t, after.health, serverBundle.Plan.Fingerprint)
	assertSharedMetrics(t, after.metrics, 2, 0)
	assertActiveRealm(t, "server after both clients", realm.Live(), plugin.ServerRealm, serverLive.Fingerprint)
	assertSameRuntimeSelections(t, "server after both clients", serverLive, realm.Live())
	assertSameRuntimeSelections(t, "host after both clients", hostBefore, hostRealm.Live())

	closeSharedRealm(t, "presentation host", hostRealm.Close)
	assertClosedRealm(
		t, "presentation host", hostRealm.Live(), plugin.PresentationHostRealm,
		hostRealm.Plan().Fingerprint,
	)
	assertSharedHTTPStatus(t, hostHTTP.URL+"/client/v1/manifest", http.StatusNotFound)
	assertSharedHTTPStatus(t, hostHTTP.URL+"/client/v1/realtime", http.StatusNotFound)
	serverAfterHostClose := readSharedObservability(t, serverHTTP.URL)
	assertSharedServerHealth(t, serverAfterHostClose.health, serverBundle.Plan.Fingerprint)
	assertSharedMetrics(t, serverAfterHostClose.metrics, 2, 0)
	assertActiveRealm(
		t, "server after presentation-host close", realm.Live(), plugin.ServerRealm,
		serverBundle.Plan.Fingerprint,
	)

	closeSharedRealm(t, "server", realm.Close)
	assertClosedRealm(t, "server", realm.Live(), plugin.ServerRealm, serverBundle.Plan.Fingerprint)
	assertSharedHTTPStatus(t, serverHTTP.URL+"/healthz", http.StatusNotFound)
	assertSharedHTTPStatus(t, serverHTTP.URL+"/metrics", http.StatusNotFound)
}

type sharedMountedHost struct {
	*pluginruntime.Mounted
	plan plugin.Plan
}

func (mounted *sharedMountedHost) Plan() plugin.Plan { return mounted.plan.Clone() }

func mountSharedBrowserHost(
	t *testing.T,
	bundle *presentationbrowser.Bundle,
	serverWebSocket string,
	serverManagement string,
) *sharedMountedHost {
	t.Helper()
	router := host.NewRouterFactory()
	target := host.NewEndpointDirectoryFactory()
	credential := host.NewAnonymousCredentialFactory()
	realtimeRelay := host.NewWebSocketRelayFactory(nil)
	managementRelay := host.NewManagementRelayFactory(nil, nil)
	factories := []pluginruntime.Factory{
		bundle.Shell, bundle.ManifestHost, bundle.ModuleStore,
		realtimeRelay, managementRelay, target, credential, router,
	}
	ids := map[string]string{
		bundle.Shell.Descriptor().Name:        "shell",
		bundle.ManifestHost.Descriptor().Name: "manifest",
		bundle.ModuleStore.Descriptor().Name:  "modules",
		realtimeRelay.Descriptor().Name:       "realtime-relay",
		managementRelay.Descriptor().Name:     "management-relay",
		target.Descriptor().Name:              "target",
		credential.Descriptor().Name:          "credential",
		router.Descriptor().Name:              "router",
	}
	plan := compileSharedHostPlan(t, factories, ids)
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.RegisterArtifact(
			factory.Descriptor().Name,
			sharedArtifact(
				"go://openrealtime/presentation/shared-e2e-host/"+ids[factory.Descriptor().Name],
				"fixture-build-1", "7",
			),
			factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	targetValue, err := json.Marshal(host.EndpointDirectoryConfig{
		Endpoints: []presentation.Endpoint{
			{
				Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
				URL: serverWebSocket,
			},
			{
				Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
				URL: serverManagement,
			},
		},
		Model: sharedFixtureModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"target": targetValue},
		Permissions: map[string][]plugin.Permission{
			"realtime-relay": {{
				Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"websocket"},
			}},
			"management-relay": {{
				Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"management"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &sharedMountedHost{Mounted: mounted, plan: plan}
}

func compileSharedHostPlan(
	t *testing.T, factories []pluginruntime.Factory, ids map[string]string,
) plugin.Plan {
	t.Helper()
	catalog := plugin.NewCatalog()
	entries := make([]plugin.ProfileEntry, 0, len(factories))
	for _, factory := range factories {
		descriptor := factory.Descriptor()
		if _, err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
		id := ids[descriptor.Name]
		if id == "" {
			t.Fatalf("shared host has no stable entry ID for %s", descriptor.Name)
		}
		entries = append(entries, plugin.ProfileEntry{ID: id, Plugin: descriptor.Name, Scope: "root"})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "openrealtime.presentation.host.shared-e2e", Revision: 1,
		Realm: plugin.PresentationHostRealm, Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: entries,
		Exports: []plugin.ProfileExport{{
			Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func sharedNativeEndpointDirectory(t *testing.T, hostURL string) presentation.EndpointDirectory {
	t.Helper()
	directory, err := presentation.FreezeEndpointDirectory([]presentation.Endpoint{
		{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "ws" + strings.TrimPrefix(hostURL, "http") + "/client/v1/realtime",
		},
		{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: hostURL + "/client/v1/management",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func assertCommonClientContracts(
	t *testing.T,
	browserBundle *presentationbrowser.Bundle,
	nativeBundle *macos.NativeBundle,
	hostURL string,
) {
	t.Helper()
	if browserBundle.Plan.Realm != plugin.ClientRealm || nativeBundle.Plan.Realm != plugin.ClientRealm ||
		browserBundle.Manifest.Platform != "browser" || nativeBundle.Manifest.Platform != "macos" {
		t.Fatalf("client realms/platforms are not independent compositions: browser=%s/%s native=%s/%s",
			browserBundle.Plan.Realm, browserBundle.Manifest.Platform,
			nativeBundle.Plan.Realm, nativeBundle.Manifest.Platform)
	}
	if browserBundle.Plan.Fingerprint == nativeBundle.Plan.Fingerprint ||
		browserBundle.Manifest.Fingerprint == nativeBundle.Manifest.Fingerprint {
		t.Fatal("platform-specific client compositions unexpectedly share an identity")
	}
	for _, contract := range []plugin.Contract{
		presentation.ClientConnectionContract,
		presentation.ClientStateContract,
		presentation.ClientInspectionAccessContract,
		presentation.ClientInspectionContract,
		presentation.ClientSessionConfigurationContract,
	} {
		if clientProvider(browserBundle.Plan, contract) == "" || clientProvider(nativeBundle.Plan, contract) == "" {
			t.Fatalf("browser or macOS profile omitted shared logical service %s", contract.Name)
		}
	}
	browserEndpoint := manifestEndpoint(t, browserBundle.Manifest, "realtime.websocket")
	if browserEndpoint.Method != http.MethodGet || browserEndpoint.Path != "/client/v1/realtime" ||
		browserEndpoint.Protocol != "" {
		t.Fatalf("browser realtime endpoint = %+v", browserEndpoint)
	}
	nativeEndpoint, err := nativeBundle.EndpointDirectory.Require(
		presentation.EndpointRealtimeWebSocket, presentation.ProtocolRealtimeWebSocket,
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(nativeEndpoint.URL)
	if err != nil || parsed.Path != browserEndpoint.Path ||
		parsed.Host != strings.TrimPrefix(hostURL, "http://") {
		t.Fatalf("native realtime endpoint %q does not select the browser host API %s",
			nativeEndpoint.URL, browserEndpoint.Path)
	}
	implementation := manifestImplementation(t, nativeBundle.Manifest, "transport")
	if implementation.Implementation != "macos.realtime-transport.v2" ||
		!strings.HasPrefix(implementation.Artifact.ID, "native://") ||
		implementation.Artifact.Digest == "" {
		t.Fatalf("native transport implementation is not exact Swift artifact evidence: %+v", implementation)
	}
	managementEndpoint, err := nativeBundle.EndpointDirectory.Require(
		presentation.EndpointManagement, presentation.ProtocolManagement,
	)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, parseErr := url.Parse(managementEndpoint.URL); parseErr != nil ||
		parsed.Path != "/client/v1/management" || parsed.Host != strings.TrimPrefix(hostURL, "http://") {
		t.Fatalf("native management endpoint does not select the shared host: %q", managementEndpoint.URL)
	}
	if err := browserBundle.Manifest.Validate(); err != nil {
		t.Fatalf("browser manifest: %v", err)
	}
	if err := nativeBundle.Manifest.Validate(); err != nil {
		t.Fatalf("native manifest: %v", err)
	}
	if err := nativeBundle.EndpointDirectory.Validate(); err != nil {
		t.Fatalf("native endpoint directory: %v", err)
	}
}

func clientProvider(plan plugin.Plan, contract plugin.Contract) string {
	for _, entry := range plan.Entries {
		for _, provided := range entry.Descriptor.Provides {
			if provided == contract {
				return entry.Entry.ID
			}
		}
	}
	return ""
}

func manifestEndpoint(
	t *testing.T, manifest presentation.ClientManifest, name string,
) presentation.ManifestEndpoint {
	t.Helper()
	var matches []presentation.ManifestEndpoint
	for _, endpoint := range manifest.Endpoints {
		if endpoint.Name == name {
			matches = append(matches, endpoint)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("manifest has %d %q endpoints", len(matches), name)
	}
	return matches[0]
}

func manifestImplementation(
	t *testing.T, manifest presentation.ClientManifest, entry string,
) presentation.ManifestImplementation {
	t.Helper()
	for _, implementation := range manifest.Implementations {
		if implementation.Entry == entry {
			return implementation
		}
	}
	t.Fatalf("manifest has no implementation for %q", entry)
	return presentation.ManifestImplementation{}
}

func runSharedBrowser(
	t *testing.T,
	node string,
	chromium string,
	hostURL string,
	clientFingerprint string,
	graphFingerprint string,
) {
	t.Helper()
	driver, err := filepath.Abs(filepath.Join("browser", "testdata", "shared_server.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, driver, hostURL)
	command.Env = append(os.Environ(),
		"CHROMIUM="+chromium,
		"CDP_PORT="+sharedFreePort(t),
		"CLIENT_PLAN_FINGERPRINT="+clientFingerprint,
		"SESSION_GRAPH_FINGERPRINT="+graphFingerprint,
		"FIXTURE_INPUT="+sharedFixtureInput,
		"FIXTURE_REPLY="+sharedFixtureReply,
	)
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("shared browser contract timed out: %v", ctx.Err())
		}
		t.Fatalf("shared browser contract failed: %v", err)
	}
}

func runNativeManifestWireProbe(
	t *testing.T, bundle *macos.NativeBundle, graphFingerprint string,
) {
	t.Helper()
	realtimeEndpoint, err := bundle.EndpointDirectory.Require(
		presentation.EndpointRealtimeWebSocket, presentation.ProtocolRealtimeWebSocket,
	)
	if err != nil {
		t.Fatal(err)
	}
	managementEndpoint, err := bundle.EndpointDirectory.Require(
		presentation.EndpointManagement, presentation.ProtocolManagement,
	)
	if err != nil {
		t.Fatal(err)
	}
	connection, _, err := websocket.Dial(context.Background(), realtimeEndpoint.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = connection.Close(websocket.StatusNormalClosure, "native contract probe complete")
		}
	}()

	seen := make([]string, 0, 16)
	created := awaitSharedEvent(t, connection, "session.created", &seen)
	if session, ok := created["session"].(map[string]any); !ok || session["id"] == "" {
		t.Fatal("native probe received session.created without a session identity")
	}
	writeSharedEvent(t, connection, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"openrealtime": map[string]any{
				"version":   1,
				"supports":  []any{},
				"observers": []any{},
				"debug":     map[string]any{"enabled": true, "categories": []string{"session"}},
			},
			"tools": []any{},
		},
	})
	updated := awaitSharedEvent(t, connection, "session.updated", &seen)
	access := sharedInspectionAccess(t, updated)
	assertInspectionBoundary(t, managementEndpoint.URL, access, graphFingerprint)

	writeSharedEvent(t, connection, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": sharedFixtureInput}},
		},
	})
	writeSharedEvent(t, connection, map[string]any{"type": "response.create"})
	awaitSharedEvent(t, connection, "conversation.item.created", &seen)
	awaitSharedEvent(t, connection, "response.created", &seen)
	delta := awaitSharedEvent(t, connection, "response.output_audio_transcript.delta", &seen)
	if delta["delta"] != sharedFixtureReply {
		t.Fatalf("native probe response delta = %#v", delta["delta"])
	}
	awaitSharedEvent(t, connection, "response.done", &seen)
	assertEventSubsequence(t, "native inbound", seen, []string{
		"session.created", "session.updated", "conversation.item.created",
		"response.created", "response.output_audio_transcript.delta", "response.done",
	})
	if err := connection.Close(websocket.StatusNormalClosure, "native contract probe complete"); err != nil &&
		websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("close native manifest probe: %v", err)
	}
	closed = true
	assertInspectionRevoked(t, managementEndpoint.URL, access)
}

func writeSharedEvent(t *testing.T, connection *websocket.Conn, event map[string]any) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func awaitSharedEvent(
	t *testing.T, connection *websocket.Conn, want string, seen *[]string,
) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		kind, payload, err := connection.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("await %s: %v", want, err)
		}
		if kind != websocket.MessageText || strictjson.Validate(payload) != nil {
			t.Fatalf("await %s received a non-strict text event", want)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("await %s: %v", want, err)
		}
		eventType, _ := event["type"].(string)
		*seen = append(*seen, eventType)
		if eventType == "error" {
			t.Fatalf("await %s received a server error", want)
		}
		if eventType == want {
			return event
		}
	}
	t.Fatalf("timed out awaiting %s; event types=%v", want, *seen)
	return nil
}

func sharedInspectionAccess(t *testing.T, updated map[string]any) openrealtime.InspectionAccess {
	t.Helper()
	session, ok := updated["session"].(map[string]any)
	if !ok {
		t.Fatal("session.updated omitted its session object")
	}
	extension, ok := session["openrealtime"].(map[string]any)
	if !ok {
		t.Fatal("session.updated omitted its OpenRealtime negotiation")
	}
	debug, ok := extension["debug"].(map[string]any)
	if !ok {
		t.Fatal("session.updated omitted its debug negotiation")
	}
	payload, err := json.Marshal(debug["inspection"])
	if err != nil {
		t.Fatal(err)
	}
	var access openrealtime.InspectionAccess
	if err := json.Unmarshal(payload, &access); err != nil {
		t.Fatal(err)
	}
	if access.SessionID == "" || access.Token == "" || access.Path == "" || access.ExpiresAtMS <= time.Now().UnixMilli() {
		t.Fatal("session inspection negotiation was absent or incomplete")
	}
	return access
}

func assertInspectionBoundary(
	t *testing.T, base string, access openrealtime.InspectionAccess, graphFingerprint string,
) {
	t.Helper()
	path := base + "/sessions/" + url.PathEscape(access.SessionID) + "/live"
	status, payload, header := getSharedManagement(t, path, "mgmt_invalid")
	if status == http.StatusOK || header.Get("Cache-Control") != "no-store" ||
		bytes.Contains(payload, []byte(access.Token)) {
		t.Fatalf("invalid inspection capability was not rejected without retention: status=%d", status)
	}
	status, payload, header = getSharedManagement(t, path, access.Token)
	if status != http.StatusOK || header.Get("Cache-Control") != "no-store" ||
		header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("authorized inspection response = status %d headers %v", status, header)
	}
	if strictjson.Validate(payload) != nil || bytes.Contains(payload, []byte(sharedFixtureInput)) ||
		bytes.Contains(payload, []byte(sharedFixtureReply)) || bytes.Contains(payload, []byte(access.Token)) {
		t.Fatal("inspection response was invalid or retained a session payload/capability")
	}
	var snapshot inspect.Live
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := management.ValidateSessionSnapshot(snapshot); err != nil {
		t.Fatalf("inspection response is not exact live evidence: %v", err)
	}
	if snapshot.Fingerprint != graphFingerprint {
		t.Fatalf("inspection graph fingerprint = %s, want %s", snapshot.Fingerprint, graphFingerprint)
	}
}

func assertInspectionRevoked(
	t *testing.T, base string, access openrealtime.InspectionAccess,
) {
	t.Helper()
	path := base + "/sessions/" + url.PathEscape(access.SessionID) + "/live"
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, payload, header := getSharedManagement(t, path, access.Token)
		if status != http.StatusOK {
			if header.Get("Cache-Control") != "no-store" || bytes.Contains(payload, []byte(access.Token)) {
				t.Fatalf("revoked inspection response retained its capability: status=%d", status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("session close did not revoke native inspection capability")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getSharedManagement(t *testing.T, target string, capability string) (int, []byte, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(management.CapabilityHeader, capability)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 32<<20+1))
	if err != nil || len(payload) > 32<<20 {
		t.Fatal("management response exceeded its test contract")
	}
	return response.StatusCode, payload, response.Header.Clone()
}

type sharedObservability struct {
	health  map[string]any
	metrics gateway.MetricsSnapshot
}

func readSharedObservability(t *testing.T, base string) sharedObservability {
	t.Helper()
	healthPayload, healthHeader := getSharedJSON(t, base+"/healthz", http.StatusOK)
	if healthHeader.Get("Content-Type") != "application/json" {
		t.Fatalf("health content type = %q", healthHeader.Get("Content-Type"))
	}
	var health map[string]any
	if err := json.Unmarshal(healthPayload, &health); err != nil {
		t.Fatal(err)
	}
	metricsPayload, metricsHeader := getSharedJSON(t, base+"/metrics", http.StatusOK)
	if metricsHeader.Get("Content-Type") != "application/json" {
		t.Fatalf("metrics content type = %q", metricsHeader.Get("Content-Type"))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metricsPayload, &fields); err != nil {
		t.Fatal(err)
	}
	// Every field is named, and adding one is meant to be a decision rather
	// than a diff nobody looked at. The rule the list encodes is that this
	// endpoint carries counts and never content, so a new entry has to be a
	// number that says nothing about what was said: sessions_in_flight is how
	// many sessions exist, sessions_rejected is how many were refused at
	// capacity, and neither can carry a word of a conversation.
	wantFields := []string{
		"audio_frames_in", "audio_frames_out", "sessions_completed", "sessions_failed",
		"sessions_in_flight", "sessions_rejected", "sessions_started", "tool_calls_out",
		"video_frames_dropped", "video_frames_in",
	}
	gotFields := make([]string, 0, len(fields))
	for name := range fields {
		gotFields = append(gotFields, name)
	}
	sort.Strings(gotFields)
	if strings.Join(gotFields, "\x00") != strings.Join(wantFields, "\x00") {
		t.Fatalf("metrics schema widened from payload-free counters: %v", gotFields)
	}
	decoder := json.NewDecoder(bytes.NewReader(metricsPayload))
	decoder.DisallowUnknownFields()
	var metrics gateway.MetricsSnapshot
	if err := decoder.Decode(&metrics); err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{healthPayload, metricsPayload} {
		if bytes.Contains(payload, []byte(sharedFixtureInput)) ||
			bytes.Contains(payload, []byte(sharedFixtureReply)) || bytes.Contains(payload, []byte("mgmt_")) {
			t.Fatal("operational observability retained conversation or capability payload")
		}
	}
	return sharedObservability{health: health, metrics: metrics}
}

func getSharedJSON(t *testing.T, target string, wantStatus int) ([]byte, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil || len(payload) > 4<<20 {
		t.Fatalf("read bounded JSON %s: %v", target, err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("GET %s status = %d, want %d", target, response.StatusCode, wantStatus)
	}
	if err := strictjson.Validate(payload); err != nil {
		t.Fatalf("GET %s returned non-strict JSON: %v", target, err)
	}
	return payload, response.Header.Clone()
}

func assertSharedServerHealth(t *testing.T, health map[string]any, fingerprint string) {
	t.Helper()
	if health["status"] != "ok" || health["binding"] != "shared-presentation-fixture" {
		t.Fatalf("shared server health = %+v", health)
	}
	protocol, ok := health["protocol"].(map[string]any)
	if !ok || protocol["openai_realtime"] != "pinned" {
		t.Fatalf("shared server omitted pinned OpenAI Realtime compatibility: %+v", protocol)
	}
	raw, err := json.Marshal(health["server_profile"])
	if err != nil {
		t.Fatal(err)
	}
	var live pluginruntime.Live
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	assertActiveRealm(t, "health server profile", live, plugin.ServerRealm, fingerprint)
}

func assertSharedMetrics(
	t *testing.T, metrics gateway.MetricsSnapshot, completed uint64, failed uint64,
) {
	t.Helper()
	if metrics.SessionsStarted != completed || metrics.SessionsCompleted != completed ||
		metrics.SessionsFailed != failed || metrics.AudioFramesIn != 0 || metrics.VideoFramesIn != 0 ||
		metrics.VideoFramesDropped != 0 || metrics.AudioFramesOut != 0 || metrics.ToolCallsOut != 0 {
		t.Fatalf("shared server metrics = %+v, want completed=%d failed=%d and no media/tool traffic",
			metrics, completed, failed)
	}
}

func waitForSharedMetrics(t *testing.T, base string, completed uint64, failed uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		observed := readSharedObservability(t, base)
		if observed.metrics.SessionsStarted == completed &&
			observed.metrics.SessionsCompleted == completed && observed.metrics.SessionsFailed == failed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for completed sessions: %+v", observed.metrics)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertServerOwnsNoPresentation(t *testing.T, serverURL string) {
	t.Helper()
	for _, path := range []string{"/", "/client/v1/manifest", "/client/v1/modules/not-a-module"} {
		response, err := http.Get(serverURL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("clean server unexpectedly owns presentation route %s: %d", path, response.StatusCode)
		}
	}
}

func assertHostAndServerSurfacesAreSeparate(
	t *testing.T, hostURL string, serverURL string, manifest presentation.ClientManifest,
) {
	t.Helper()
	payload, _ := getSharedJSON(t, hostURL+"/client/v1/manifest", http.StatusOK)
	served, err := presentation.ParseManifest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if served.Fingerprint != manifest.Fingerprint {
		t.Fatalf("host served manifest %s, want %s", served.Fingerprint, manifest.Fingerprint)
	}
	for _, path := range []string{"/healthz", "/metrics"} {
		response, err := http.Get(hostURL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("presentation host unexpectedly owns server observability route %s: %d",
				path, response.StatusCode)
		}
	}
	assertServerOwnsNoPresentation(t, serverURL)
}

func assertActiveRealm(
	t *testing.T, label string, live pluginruntime.Live, realm plugin.Realm, fingerprint string,
) {
	t.Helper()
	if live.FormatVersion != pluginruntime.LiveFormatVersion || live.Realm != realm ||
		live.Fingerprint != fingerprint || live.State != "active" || len(live.Entries) == 0 {
		t.Fatalf("%s live evidence = %+v", label, live)
	}
	for id, entry := range live.Entries {
		if entry.State != "active" || !entry.Desired || entry.Error != "" ||
			entry.Runtime.Validate() != nil || entry.Implementation == "" {
			t.Fatalf("%s entry %s is not exact and active: %+v", label, id, entry)
		}
	}
}

func assertSameRuntimeSelections(
	t *testing.T, label string, before pluginruntime.Live, after pluginruntime.Live,
) {
	t.Helper()
	if before.Fingerprint != after.Fingerprint || before.Realm != after.Realm ||
		len(before.Entries) != len(after.Entries) {
		t.Fatalf("%s changed realm identity", label)
	}
	for id, prior := range before.Entries {
		current, found := after.Entries[id]
		if !found || current.Identity != prior.Identity || current.Implementation != prior.Implementation ||
			current.Runtime != prior.Runtime || current.State != "active" || current.Error != "" {
			t.Fatalf("%s changed runtime selection %s: before=%+v after=%+v",
				label, id, prior, current)
		}
	}
}

func assertClosedRealm(
	t *testing.T, label string, live pluginruntime.Live, realm plugin.Realm, fingerprint string,
) {
	t.Helper()
	if live.FormatVersion != pluginruntime.LiveFormatVersion || live.Realm != realm ||
		live.Fingerprint != fingerprint || live.State != "closed" || len(live.Entries) == 0 {
		t.Fatalf("%s closed evidence = %+v", label, live)
	}
	for id, entry := range live.Entries {
		if entry.State != "closed" || entry.Workers != 0 || entry.Effects != 0 ||
			len(entry.Services) != 0 || entry.Error != "" {
			t.Fatalf("%s entry %s retained ownership: %+v", label, id, entry)
		}
	}
	for name, exported := range live.Exports {
		if exported.Available {
			t.Fatalf("%s export %s remained available after close: %+v", label, name, exported)
		}
	}
}

func assertRuntimeLifecycle(t *testing.T, label string, operations []string) {
	t.Helper()
	assertEventSubsequence(t, label+" provider", operations, []string{
		"start", "session.update", "conversation.item.create", "response.create", "close",
	})
}

func assertEventSubsequence(t *testing.T, label string, actual []string, want []string) {
	t.Helper()
	cursor := 0
	for _, value := range actual {
		if cursor < len(want) && value == want[cursor] {
			cursor++
		}
	}
	if cursor != len(want) {
		t.Fatalf("%s lifecycle = %v, missing ordered suffix %v", label, actual, want[cursor:])
	}
}

func closeSharedRealm(
	t *testing.T, label string, close func(context.Context) error,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := close(ctx); err != nil {
		t.Errorf("close %s: %v", label, err)
	}
}

func assertSharedHTTPStatus(t *testing.T, target string, want int) {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != want {
		t.Fatalf("GET %s status = %d, want %d", target, response.StatusCode, want)
	}
}

func requireSharedBrowser(t *testing.T) (string, string) {
	t.Helper()
	missing := func(requirement string) {
		message := requirement + " is not installed"
		if os.Getenv("OPENREALTIME_RELEASE_GATE") != "" {
			t.Fatal(message + "; the release gate requires the shared browser/native server contract")
		}
		t.Skip(message)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		missing("node")
	}
	chromium := os.Getenv("CHROMIUM")
	if chromium == "" {
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if found, err := exec.LookPath(candidate); err == nil {
				chromium = found
				break
			}
		}
	}
	if chromium == "" {
		missing("chromium")
	}
	return node, chromium
}

func sharedFreePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func sharedArtifact(id string, revision string, digit string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: id, Revision: revision, Digest: "sha256:" + strings.Repeat(digit, 64),
	}
}

type sharedPresentationProvider struct {
	mu       sync.Mutex
	sessions []*sharedPresentationRuntime
}

func (*sharedPresentationProvider) Name() string { return "shared-presentation-fixture" }

func (*sharedPresentationProvider) Ownership() binding.Ownership {
	return binding.Ownership{
		Perception: binding.OwnerEngine, FastCognition: binding.OwnerEngine,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerEngine,
		Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
	}
}

func (*sharedPresentationProvider) Capabilities() binding.Capabilities {
	return binding.Capabilities{FastSlow: true}
}

func (provider *sharedPresentationProvider) Start(
	_ context.Context, options binding.Options,
) (binding.Runtime, error) {
	if options.Sink == nil || options.SessionID == "" {
		return nil, errors.New("shared presentation fixture requires a session sink and identity")
	}
	runtime := &sharedPresentationRuntime{
		sessionID: options.SessionID, sink: options.Sink, closed: make(chan struct{}),
		operations: []string{"start"},
	}
	provider.mu.Lock()
	provider.sessions = append(provider.sessions, runtime)
	provider.mu.Unlock()
	return runtime, nil
}

func (provider *sharedPresentationProvider) sessionCount() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return len(provider.sessions)
}

func (provider *sharedPresentationProvider) awaitSession(
	t *testing.T, index int,
) *sharedPresentationRuntime {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		provider.mu.Lock()
		if len(provider.sessions) > index {
			runtime := provider.sessions[index]
			provider.mu.Unlock()
			return runtime
		}
		provider.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("shared provider did not create session %d", index)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type sharedPresentationRuntime struct {
	sessionID string
	sink      binding.Sink

	mu         sync.Mutex
	input      string
	operations []string
	responses  uint64
	closed     chan struct{}
	closeOnce  sync.Once
	sequence   atomic.Uint64
}

func (runtime *sharedPresentationRuntime) record(operation string) {
	runtime.mu.Lock()
	runtime.operations = append(runtime.operations, operation)
	runtime.mu.Unlock()
}

func (runtime *sharedPresentationRuntime) operationsSnapshot() []string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]string(nil), runtime.operations...)
}

func (runtime *sharedPresentationRuntime) awaitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-runtime.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shared presentation runtime did not close")
	}
}

func (runtime *sharedPresentationRuntime) Update(context.Context, binding.Settings) error {
	runtime.record("session.update")
	return nil
}

func (*sharedPresentationRuntime) Audio(context.Context, perception.Frame) error {
	return binding.ErrUnsupported
}

func (*sharedPresentationRuntime) Video(context.Context, perception.Frame) error {
	return binding.ErrUnsupported
}

func (runtime *sharedPresentationRuntime) Text(_ context.Context, input binding.TextInput) error {
	if input.Role != "user" || strings.TrimSpace(input.Text) == "" {
		return errors.New("shared presentation fixture accepts one non-empty user message")
	}
	runtime.mu.Lock()
	runtime.input = input.Text
	runtime.operations = append(runtime.operations, "conversation.item.create")
	runtime.mu.Unlock()
	return nil
}

func (*sharedPresentationRuntime) ToolResult(context.Context, trajectory.ToolResult) error {
	return binding.ErrUnsupported
}

func (*sharedPresentationRuntime) CommitAudio(context.Context) error { return binding.ErrUnsupported }

func (runtime *sharedPresentationRuntime) CreateResponse(ctx context.Context) error {
	runtime.mu.Lock()
	if runtime.input == "" {
		runtime.mu.Unlock()
		return errors.New("response.create requires a committed text item")
	}
	runtime.responses++
	response := runtime.responses
	runtime.input = ""
	runtime.operations = append(runtime.operations, "response.create")
	runtime.mu.Unlock()
	utterance := action.Utterance{
		ID: fmt.Sprintf("shared-%s-%d", runtime.sessionID, response), Text: sharedFixtureReply,
	}
	if err := runtime.sink.TurnBegin(ctx); err != nil {
		return err
	}
	if err := runtime.sink.SpeechBegin(ctx, utterance); err != nil {
		return err
	}
	if err := runtime.sink.SpeechText(ctx, utterance, sharedFixtureReply); err != nil {
		return err
	}
	if err := runtime.sink.SpeechEnd(ctx, utterance, action.Outcome{Completed: true}); err != nil {
		return err
	}
	return runtime.sink.TurnEnd(ctx, binding.TurnOutcome{})
}

func (*sharedPresentationRuntime) Cancel(context.Context, string) error { return nil }

func (*sharedPresentationRuntime) Truncate(context.Context, binding.Truncation) error { return nil }

func (*sharedPresentationRuntime) Trajectory() trajectory.Snapshot { return trajectory.Snapshot{} }

func (*sharedPresentationRuntime) Status() binding.Status {
	return binding.Status{Binding: "shared-presentation-fixture"}
}

func (runtime *sharedPresentationRuntime) Close(context.Context, error) error {
	runtime.closeOnce.Do(func() {
		runtime.record("close")
		close(runtime.closed)
	})
	return nil
}

func (runtime *sharedPresentationRuntime) Live() inspect.Live {
	configuration := inspect.ArtifactIdentity{
		ID: "values://openrealtime/presentation/shared-e2e", Revision: "fixture-values-1",
		Digest: sharedValuesFingerprint,
	}
	return inspect.Live{
		FormatVersion: inspect.LiveFormatVersion,
		GraphID:       sharedGraphID, GraphRevision: 1, Fingerprint: sharedGraphFingerprint,
		Configuration: &configuration, Sequence: runtime.sequence.Add(1),
		ObservedAt: time.Now().UTC(), State: "mounted",
		Nodes: map[string]inspect.NodeLive{"session": {
			State: "mounted",
			Resolution: &inspect.NodeResolution{
				Element: element.Identity{
					Name: "openrealtime.presentation.shared-e2e", Revision: 1,
					Digest: sharedElementFingerprint,
				},
				Implementation: "go.shared-presentation-fixture.v1",
				Runtime: inspect.ArtifactIdentity{
					ID:       "go://openrealtime/presentation/shared-e2e-runtime",
					Revision: "fixture-build-1", Digest: sharedRuntimeFingerprint,
				},
				RuntimeEvidence: inspect.EvidenceRegistered,
			},
		}},
		Edges: map[string]inspect.EdgeLive{}, Flows: map[string]inspect.FlowLive{},
	}
}

var (
	_ serverplugin.SessionProvider = (*sharedPresentationProvider)(nil)
	_ binding.Runtime              = (*sharedPresentationRuntime)(nil)
)
