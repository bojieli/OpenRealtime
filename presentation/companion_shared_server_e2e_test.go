package presentation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/internal/testserver"
	"github.com/bojieli/OpenRealtime/macos"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
	"github.com/bojieli/OpenRealtime/presentation/host"
	"github.com/coder/websocket"
)

// This is the companion transport contract: one descriptor-locked host
// exposes WebRTC to the real browser composition and WebSocket to the exact
// native observer composition, while both consume the same unchanged server
// and negotiated management plane. The native leg is a manifest-derived wire
// probe on Linux; it deliberately does not claim a signed macOS application.
func TestCompanionHostServesBrowserWebRTCAndNativeWebSocketOnOneServer(t *testing.T) {
	node, chromium := requireSharedBrowser(t)
	operatorAuthority := management.NewCapabilityRegistry()
	stack := testserver.Start(t, testserver.Config{
		GraphInspection: true, TraceRecording: true, ManagementAuthorizer: operatorAuthority,
	})
	operatorGrants := []management.Grant{
		{Operation: management.ReadGraph, Resource: stack.Graph.Fingerprint},
		{Operation: management.ReadDescriptor, Resource: fmt.Sprintf(
			"element:%s@%d:%s",
			stack.ElementIdentity.Name, stack.ElementIdentity.Revision, stack.ElementIdentity.Digest,
		)},
		{Operation: management.ReadSchema, Resource: stack.Graph.Fingerprint},
		{Operation: management.AnalyzeDocument, Resource: "authoring"},
		{Operation: management.RenameDocument, Resource: "authoring"},
		{Operation: management.RemoveDocumentEdge, Resource: "authoring"},
		{Operation: management.CreateDocumentEdge, Resource: "authoring"},
		{Operation: management.CompileDocument, Resource: "authoring"},
		{Operation: management.RenderGraph, Resource: "authoring"},
	}
	operatorOne, revokeOne, err := operatorAuthority.IssueScoped(2*time.Minute, operatorGrants)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(revokeOne)
	operatorTwo, revokeTwo, err := operatorAuthority.IssueScoped(2*time.Minute, operatorGrants)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(revokeTwo)

	browserBundle, err := presentationbrowser.ObserverDeveloperWebRTCBundle()
	if err != nil {
		t.Fatal(err)
	}
	managementURL := companionManagementURL(t, stack.ProtocolURL)
	mounted := mountCompanionDualRelayHost(
		t, browserBundle, stack.ProtocolURL, stack.AdapterURL, managementURL,
	)
	t.Cleanup(func() { closeSharedRealm(t, "companion presentation host", mounted.Close) })
	handler, err := host.HTTPHandler(mounted.Mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	hostHTTP := httptest.NewServer(handler)
	t.Cleanup(hostHTTP.Close)

	nativeDirectory := sharedNativeEndpointDirectory(t, hostHTTP.URL)
	nativeBundle, err := macos.NewNativeBundleForDistributionWithEndpointDirectory(
		macos.NativeObserverDeveloperDistribution, nativeDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertCompanionTransportEndpoints(t, browserBundle, nativeBundle, hostHTTP.URL)
	hostBefore := mounted.Live()

	driver, err := filepath.Abs(filepath.Join("browser", "testdata", "developer.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	authoringYAML, authoringJSON := companionNormalizedAuthoringSources(t, stack.AuthoringSource)
	command := exec.CommandContext(ctx, node, driver, hostHTTP.URL)
	command.Env = append(os.Environ(),
		"CHROMIUM="+chromium,
		"CDP_PORT="+sharedFreePort(t),
		"EXPECT_EFFECTS=0",
		"CLIENT_TRANSPORT=webrtc",
		"OPERATOR_CAPABILITY="+operatorOne.Token,
		"OPERATOR_CAPABILITY_ROTATED="+operatorTwo.Token,
		"AUTHORING_SOURCE="+stack.AuthoringSource,
		"AUTHORING_YAML_SOURCE="+authoringYAML,
		"AUTHORING_JSON_SOURCE="+authoringJSON,
		"STATIC_GRAPH_FINGERPRINT="+stack.Graph.Fingerprint,
	)
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("companion browser WebRTC timed out: %v", ctx.Err())
		}
		t.Fatalf("companion browser WebRTC failed: %v", err)
	}
	assertSameRuntimeSelections(t, "companion host after WebRTC browser", hostBefore, mounted.Live())

	runCompanionNativeInspectionProbe(t, nativeBundle, stack.Graph.Fingerprint)
	assertSameRuntimeSelections(t, "companion host after native WebSocket", hostBefore, mounted.Live())
}

func companionNormalizedAuthoringSources(t testing.TB, source string) (string, string) {
	t.Helper()
	file, err := syntax.Parse("browser-authoring.ortg", []byte(source))
	if err != nil {
		t.Fatalf("parse companion normalized authoring fixture: %v", err)
	}
	document := manifest.FromSyntax(file)
	yamlSource, err := manifest.MarshalYAML(document)
	if err != nil {
		t.Fatalf("marshal companion YAML authoring fixture: %v", err)
	}
	jsonSource, err := manifest.MarshalJSON(document)
	if err != nil {
		t.Fatalf("marshal companion JSON authoring fixture: %v", err)
	}
	return string(yamlSource), string(jsonSource)
}

func mountCompanionDualRelayHost(
	t *testing.T,
	bundle *presentationbrowser.Bundle,
	serverWebSocket, serverWebRTC, serverManagement string,
) *sharedMountedHost {
	t.Helper()
	router := host.NewRouterFactory()
	target := host.NewEndpointDirectoryFactory()
	credential := host.NewAnonymousCredentialFactory()
	webRTCRelay := host.NewWebRTCRelayFactory(nil, nil)
	webSocketRelay := host.NewWebSocketRelayFactory(nil)
	managementRelay := host.NewManagementRelayFactory(nil, nil)
	factories := []pluginruntime.Factory{
		bundle.Shell, bundle.ManifestHost, bundle.ModuleStore,
		webRTCRelay, webSocketRelay, managementRelay, target, credential, router,
	}
	ids := map[string]string{
		bundle.Shell.Descriptor().Name:        "shell",
		bundle.ManifestHost.Descriptor().Name: "manifest",
		bundle.ModuleStore.Descriptor().Name:  "modules",
		webRTCRelay.Descriptor().Name:         "webrtc-relay",
		webSocketRelay.Descriptor().Name:      "websocket-relay",
		managementRelay.Descriptor().Name:     "management-relay",
		target.Descriptor().Name:              "target",
		credential.Descriptor().Name:          "credential",
		router.Descriptor().Name:              "router",
	}
	plan := compileSharedHostPlan(t, factories, ids)
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	targetValue, err := json.Marshal(host.EndpointDirectoryConfig{
		Endpoints: []presentation.Endpoint{
			{Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket, URL: serverWebSocket},
			{Name: presentation.EndpointRealtimeWebRTC, Protocol: presentation.ProtocolRealtimeWebRTC, URL: serverWebRTC},
			{Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement, URL: serverManagement},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"target": targetValue},
		Permissions: map[string][]plugin.Permission{
			"webrtc-relay":     {{Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"http"}}},
			"websocket-relay":  {{Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"websocket"}}},
			"management-relay": {{Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"management"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &sharedMountedHost{Mounted: mounted, plan: plan}
}

func assertCompanionTransportEndpoints(
	t *testing.T,
	browser *presentationbrowser.Bundle,
	native *macos.NativeBundle,
	hostURL string,
) {
	t.Helper()
	browserEndpoint := manifestEndpoint(t, browser.Manifest, "realtime.webrtc")
	if browserEndpoint.Method != "POST" || browserEndpoint.Path != "/client/v1/realtime/calls" {
		t.Fatalf("browser WebRTC endpoint = %+v", browserEndpoint)
	}
	nativeEndpoint, err := native.EndpointDirectory.Require(
		presentation.EndpointRealtimeWebSocket, presentation.ProtocolRealtimeWebSocket,
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(nativeEndpoint.URL)
	if err != nil || parsed.Path != "/client/v1/realtime" || parsed.Host != strings.TrimPrefix(hostURL, "http://") {
		t.Fatalf("native WebSocket endpoint = %q", nativeEndpoint.URL)
	}
	if browserEndpoint.Path == parsed.Path {
		t.Fatal("browser and native transports were inferred through one route")
	}
}

func runCompanionNativeInspectionProbe(
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
			_ = connection.Close(websocket.StatusNormalClosure, "native companion probe complete")
		}
	}()
	seen := make([]string, 0, 4)
	awaitSharedEvent(t, connection, "session.created", &seen)
	writeSharedEvent(t, connection, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"openrealtime": map[string]any{
				"version": 1, "supports": []any{}, "observers": []any{},
				"debug": map[string]any{"enabled": true, "categories": []string{"session"}},
			},
			"tools": []any{},
		},
	})
	updated := awaitSharedEvent(t, connection, "session.updated", &seen)
	access := sharedInspectionAccess(t, updated)
	assertInspectionBoundary(t, managementEndpoint.URL, access, graphFingerprint)
	if err := connection.Close(websocket.StatusNormalClosure, "native companion probe complete"); err != nil &&
		websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatal(err)
	}
	closed = true
	assertInspectionRevoked(t, managementEndpoint.URL, access)
}

func companionManagementURL(t *testing.T, websocketEndpoint string) string {
	t.Helper()
	parsed, err := url.Parse(websocketEndpoint)
	if err != nil || parsed.Host == "" {
		t.Fatalf("parse companion server endpoint: %v", err)
	}
	if parsed.Scheme == "ws" {
		parsed.Scheme = "http"
	} else if parsed.Scheme == "wss" {
		parsed.Scheme = "https"
	} else {
		t.Fatalf("companion server scheme = %q", parsed.Scheme)
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = management.APIPrefix, "", "", ""
	return parsed.String()
}
