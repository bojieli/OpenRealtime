package browser_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	effectauthority "github.com/bojieli/OpenRealtime/authority"
	clientreducer "github.com/bojieli/OpenRealtime/client/reducer"
	"github.com/bojieli/OpenRealtime/internal/testserver"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
	"github.com/bojieli/OpenRealtime/presentation/host"
	"github.com/coder/websocket"
)

func TestDeveloperBrowserProfileUsesCanonicalManagementAPIInChromium(t *testing.T) {
	node, chromium := requireBrowser(t)
	operatorAuthority := management.NewCapabilityRegistry()
	const sourceRootIdentity = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	sourceRoot := t.TempDir()
	sourcePublisher, err := management.NewRootedSourcePublisher(management.RootedSourcePublisherOptions{
		Root: sourceRoot, RootIdentity: sourceRootIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sourcePublisher.Close(); err != nil {
			t.Errorf("close browser source publisher: %v", err)
		}
	})
	effects, err := host.NewEffectsFactory(host.EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := effectauthority.NewSealedEffectReceipts(effectauthority.EffectReceiptOptions{
		TTL: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receipts.Close() })
	effectAuthority, err := host.NewEffectReceiptAuthorityFactory(receipts)
	if err != nil {
		t.Fatal(err)
	}
	stack := testserver.Start(t, testserver.Config{
		GraphInspection: true,
		TraceRecording:  true,
		ToolName:        "display_artifact",
		ToolArguments: `{"artifact_id":"browser-sealed","title":"Sealed browser artifact",` +
			`"html":"<!doctype html><html><body><main id=\"sealed\">sealed browser artifact</main>` +
			`<script>try{top.document.body.dataset.artifactEscaped='yes'}catch{}` +
			`document.body.dataset.executed='yes'</script></body></html>"}`,
		ClientEffectIssuer:   receipts,
		ManagementAuthorizer: operatorAuthority,
		SourceReading:        sourcePublisher,
		SourcePublication:    sourcePublisher,
	})
	operatorGrants := []management.Grant{
		{Operation: management.ReadGraph, Resource: stack.Graph.Fingerprint},
		{Operation: management.ReadDescriptor, Resource: fmt.Sprintf("element:%s@%d:%s",
			stack.ElementIdentity.Name, stack.ElementIdentity.Revision, stack.ElementIdentity.Digest)},
		{Operation: management.ReadSchema, Resource: stack.Graph.Fingerprint},
		{Operation: management.AnalyzeDocument, Resource: "authoring"},
		{Operation: management.CompileDocument, Resource: "authoring"},
		{Operation: management.RenderGraph, Resource: "authoring"},
		{Operation: management.ReadSource, Resource: sourceRootIdentity},
		{Operation: management.CreateSource, Resource: sourceRootIdentity},
		{Operation: management.UpdateSource, Resource: sourceRootIdentity},
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
	bundle, err := presentationbrowser.DeveloperBundleWithEffectsCatalog(effects.CatalogDigest())
	if err != nil {
		t.Fatal(err)
	}
	router := host.NewRouterFactory()
	target := host.NewEndpointDirectoryFactory()
	credential := host.NewAnonymousCredentialFactory()
	relay := host.NewWebSocketRelayFactory(nil)
	managementRelay := host.NewManagementRelayFactory(nil, nil)
	artifactStore := host.NewArtifactStoreFactory()
	downloadStore := host.NewDownloadStoreFactory()
	factories := []pluginruntime.Factory{
		bundle.Shell, bundle.ManifestHost, bundle.ModuleStore,
		relay, managementRelay, artifactStore, downloadStore, effectAuthority, effects,
		target, credential, router,
	}
	ids := map[string]string{
		bundle.Shell.Descriptor().Name:        "shell",
		bundle.ManifestHost.Descriptor().Name: "manifest",
		bundle.ModuleStore.Descriptor().Name:  "modules",
		relay.Descriptor().Name:               "relay",
		managementRelay.Descriptor().Name:     "management",
		artifactStore.Descriptor().Name:       "artifact-store",
		downloadStore.Descriptor().Name:       "download-store",
		effectAuthority.Descriptor().Name:     "effect-authority",
		effects.Descriptor().Name:             "effects",
		target.Descriptor().Name:              "target",
		credential.Descriptor().Name:          "credential",
		router.Descriptor().Name:              "router",
	}
	plan := compileHostPlan(t, factories, ids)
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	targetValues := explicitTargetValues(t, stack.ProtocolURL, "", managementTarget(t, stack.ProtocolURL))
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{
			"target": targetValues, "artifact-store": json.RawMessage(`{}`),
			"download-store": json.RawMessage(`{}`), "effects": json.RawMessage(`{}`),
		},
		Permissions: map[string][]plugin.Permission{
			"relay": {{
				Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"websocket"},
			}},
			"management": {{
				Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"management"},
			}},
			"artifact-store": {{
				Kind: "storage.memory", Resource: "presentation-artifacts", Operations: []string{"publish"},
			}},
			"download-store": {{
				Kind: "storage.memory", Resource: "presentation-downloads", Operations: []string{"publish"},
			}},
			"effects": {
				{Kind: "effect.local", Resource: "presentation-artifact", Operations: []string{"execute"}},
				{Kind: "effect.local", Resource: "presentation-download", Operations: []string{"execute"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := host.HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	driver, err := filepath.Abs(filepath.Join("testdata", "developer.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, driver, server.URL)
	command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t),
		"EXPECT_EFFECTS=1", "CLIENT_TRANSPORT=websocket",
		"OPERATOR_CAPABILITY="+operatorOne.Token,
		"OPERATOR_CAPABILITY_ROTATED="+operatorTwo.Token,
		"AUTHORING_SOURCE="+stack.AuthoringSource,
		"AUTHORING_UPDATED_SOURCE="+stack.AuthoringSource+"\n",
		"SOURCE_ROOT_IDENTITY="+sourceRootIdentity,
		"STATIC_GRAPH_FINGERPRINT="+stack.Graph.Fingerprint,
	)
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		t.Fatalf("developer browser profile failed: %v", err)
	}
	published, err := os.ReadFile(filepath.Join(sourceRoot, "browser-authoring.ortg"))
	if err != nil || string(published) != stack.AuthoringSource+"\n" {
		t.Fatalf("browser source publication = %q, %v", published, err)
	}
}

func TestObserverDeveloperProfilesExerciseManagementWithoutEffectsInChromium(t *testing.T) {
	node, chromium := requireBrowser(t)
	tests := []struct {
		name      string
		transport string
		operation string
		bundle    func() (*presentationbrowser.Bundle, error)
		relay     func() pluginruntime.Factory
	}{
		{
			name: "websocket", transport: "websocket", operation: "websocket",
			bundle: presentationbrowser.ObserverDeveloperBundle,
			relay:  func() pluginruntime.Factory { return host.NewWebSocketRelayFactory(nil) },
		},
		{
			name: "webrtc", transport: "webrtc", operation: "http",
			bundle: presentationbrowser.ObserverDeveloperWebRTCBundle,
			relay:  func() pluginruntime.Factory { return host.NewWebRTCRelayFactory(nil, nil) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operatorAuthority := management.NewCapabilityRegistry()
			stack := testserver.Start(t, testserver.Config{
				GraphInspection: true, TraceRecording: true, ManagementAuthorizer: operatorAuthority,
			})
			operatorGrants := []management.Grant{
				{Operation: management.ReadGraph, Resource: stack.Graph.Fingerprint},
				{Operation: management.ReadDescriptor, Resource: fmt.Sprintf("element:%s@%d:%s",
					stack.ElementIdentity.Name, stack.ElementIdentity.Revision, stack.ElementIdentity.Digest)},
				{Operation: management.ReadSchema, Resource: stack.Graph.Fingerprint},
				{Operation: management.AnalyzeDocument, Resource: "authoring"},
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
			bundle, err := test.bundle()
			if err != nil {
				t.Fatal(err)
			}
			router := host.NewRouterFactory()
			target := host.NewEndpointDirectoryFactory()
			credential := host.NewAnonymousCredentialFactory()
			relay := test.relay()
			managementRelay := host.NewManagementRelayFactory(nil, nil)
			factories := []pluginruntime.Factory{
				bundle.Shell, bundle.ManifestHost, bundle.ModuleStore,
				relay, managementRelay, target, credential, router,
			}
			ids := map[string]string{
				bundle.Shell.Descriptor().Name:        "shell",
				bundle.ManifestHost.Descriptor().Name: "manifest",
				bundle.ModuleStore.Descriptor().Name:  "modules",
				relay.Descriptor().Name:               "relay",
				managementRelay.Descriptor().Name:     "management",
				target.Descriptor().Name:              "target",
				credential.Descriptor().Name:          "credential",
				router.Descriptor().Name:              "router",
			}
			plan := compileHostPlan(t, factories, ids)
			registry := pluginruntime.NewRegistry()
			for _, factory := range factories {
				if err := registry.Register("", factory); err != nil {
					t.Fatal(err)
				}
			}
			websocket, webrtc := stack.ProtocolURL, ""
			if test.transport == "webrtc" {
				websocket, webrtc = "", stack.AdapterURL
			}
			targetValues := explicitTargetValues(
				t, websocket, webrtc, managementTarget(t, stack.ProtocolURL),
			)
			mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
				Plan: plan, Registry: registry,
				Values: map[string]json.RawMessage{"target": targetValues},
				Permissions: map[string][]plugin.Permission{
					"relay": {{
						Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{test.operation},
					}},
					"management": {{
						Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"management"},
					}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mounted.Close(context.Background()) })
			handler, err := host.HTTPHandler(mounted, "http")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()

			driver, err := filepath.Abs(filepath.Join("testdata", "developer.mjs"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, node, driver, server.URL)
			command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t),
				"EXPECT_EFFECTS=0", "CLIENT_TRANSPORT="+test.transport,
				"OPERATOR_CAPABILITY="+operatorOne.Token,
				"OPERATOR_CAPABILITY_ROTATED="+operatorTwo.Token,
				"AUTHORING_SOURCE="+stack.AuthoringSource,
				"STATIC_GRAPH_FINGERPRINT="+stack.Graph.Fingerprint,
			)
			output, err := command.CombinedOutput()
			t.Log("\n" + string(output))
			if err != nil {
				t.Fatalf("observer %s browser profile failed: %v", test.transport, err)
			}
		})
	}
}

func TestDeveloperWebRTCProfileUsesSameServerAPIsAndRecoversMediaInChromium(t *testing.T) {
	node, chromium := requireBrowser(t)
	effects, err := host.NewEffectsFactory(host.EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := effectauthority.NewSealedEffectReceipts(effectauthority.EffectReceiptOptions{
		TTL: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receipts.Close() })
	effectAuthority, err := host.NewEffectReceiptAuthorityFactory(receipts)
	if err != nil {
		t.Fatal(err)
	}
	stack := testserver.Start(t, testserver.Config{
		GraphInspection: true,
		TraceRecording:  true,
		ToolName:        "display_artifact",
		ToolArguments: `{"artifact_id":"webrtc-sealed","title":"Sealed WebRTC artifact",` +
			`"html":"<!doctype html><html><body><main>sealed WebRTC artifact</main></body></html>"}`,
		ClientEffectIssuer: receipts,
	})
	bundle, err := presentationbrowser.DeveloperWebRTCBundleWithEffectsCatalog(effects.CatalogDigest())
	if err != nil {
		t.Fatal(err)
	}
	router := host.NewRouterFactory()
	target := host.NewEndpointDirectoryFactory()
	credential := host.NewAnonymousCredentialFactory()
	relay := host.NewWebRTCRelayFactory(nil, nil)
	managementRelay := host.NewManagementRelayFactory(nil, nil)
	artifactStore := host.NewArtifactStoreFactory()
	downloadStore := host.NewDownloadStoreFactory()
	factories := []pluginruntime.Factory{
		bundle.Shell, bundle.ManifestHost, bundle.ModuleStore,
		relay, managementRelay, artifactStore, downloadStore, effectAuthority, effects,
		target, credential, router,
	}
	ids := map[string]string{
		bundle.Shell.Descriptor().Name:        "shell",
		bundle.ManifestHost.Descriptor().Name: "manifest",
		bundle.ModuleStore.Descriptor().Name:  "modules",
		relay.Descriptor().Name:               "relay",
		managementRelay.Descriptor().Name:     "management",
		artifactStore.Descriptor().Name:       "artifact-store",
		downloadStore.Descriptor().Name:       "download-store",
		effectAuthority.Descriptor().Name:     "effect-authority",
		effects.Descriptor().Name:             "effects",
		target.Descriptor().Name:              "target",
		credential.Descriptor().Name:          "credential",
		router.Descriptor().Name:              "router",
	}
	plan := compileHostPlan(t, factories, ids)
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	targetValues := explicitTargetValues(t, "", stack.AdapterURL, managementTarget(t, stack.ProtocolURL))
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{
			"target": targetValues, "artifact-store": json.RawMessage(`{}`),
			"download-store": json.RawMessage(`{}`), "effects": json.RawMessage(`{}`),
		},
		Permissions: map[string][]plugin.Permission{
			"relay": {{
				Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"http"},
			}},
			"management": {{
				Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"management"},
			}},
			"artifact-store": {{
				Kind: "storage.memory", Resource: "presentation-artifacts", Operations: []string{"publish"},
			}},
			"download-store": {{
				Kind: "storage.memory", Resource: "presentation-downloads", Operations: []string{"publish"},
			}},
			"effects": {
				{Kind: "effect.local", Resource: "presentation-artifact", Operations: []string{"execute"}},
				{Kind: "effect.local", Resource: "presentation-download", Operations: []string{"execute"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := host.HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	driver, err := filepath.Abs(filepath.Join("testdata", "webrtc.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, driver, server.URL)
	command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t))
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		t.Fatalf("developer WebRTC browser profile failed: %v", err)
	}
}

func TestMinimalBrowserProfileBootsAndRunsTextSessionInChromium(t *testing.T) {
	node, chromium := requireBrowser(t)
	var backendConnections atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ordinal := backendConnections.Add(1)
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		_ = connection.Write(request.Context(), websocket.MessageText, []byte(
			`{"type":"session.created","session":{"id":"fixture-session"}}`,
		))
		for {
			_, payload, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(payload, &event) == nil && event.Type == "response.create" {
				_ = connection.Write(request.Context(), websocket.MessageText, []byte(
					`{"type":"response.created","response":{"id":"fixture-response","status":"in_progress"}}`,
				))
				_ = connection.Write(request.Context(), websocket.MessageText, []byte(
					`{"type":"response.output_text.delta","response_id":"fixture-response","item_id":"fixture-output","delta":"hello from fixture"}`,
				))
				_ = connection.Write(request.Context(), websocket.MessageText, []byte(
					`{"type":"response.done","response":{"id":"fixture-response","status":"completed","status_details":null}}`,
				))
				if ordinal == 1 {
					_ = connection.Close(websocket.StatusGoingAway, "exercise bounded reconnect")
					return
				}
			}
		}
	}))
	defer backend.Close()

	bundle, err := presentationbrowser.MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	router := host.NewRouterFactory()
	target := host.NewEndpointDirectoryFactory()
	credential := host.NewAnonymousCredentialFactory()
	relay := host.NewWebSocketRelayFactory(nil)
	factories := []pluginruntime.Factory{
		bundle.Shell, bundle.ManifestHost, bundle.ModuleStore, relay, target, credential, router,
	}
	ids := map[string]string{
		bundle.Shell.Descriptor().Name:        "shell",
		bundle.ManifestHost.Descriptor().Name: "manifest",
		bundle.ModuleStore.Descriptor().Name:  "modules",
		relay.Descriptor().Name:               "relay",
		target.Descriptor().Name:              "target",
		credential.Descriptor().Name:          "credential",
		router.Descriptor().Name:              "router",
	}
	plan := compileHostPlan(t, factories, ids)
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	targetValues := explicitTargetValues(
		t, "ws"+strings.TrimPrefix(backend.URL, "http"), "", "",
	)
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"target": targetValues},
		Permissions: map[string][]plugin.Permission{"relay": {{
			Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"websocket"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := host.HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := clientreducer.CorpusSource()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/test/reducer-vectors.json" {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			_, _ = writer.Write(corpus)
			return
		}
		if request.URL.Path == "/test/backend-connections" {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]int32{"connections": backendConnections.Load()})
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	driver, err := filepath.Abs(filepath.Join("testdata", "minimal.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, driver, server.URL)
	command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t))
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		t.Fatalf("minimal browser profile failed: %v", err)
	}
}

func compileHostPlan(
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
			t.Fatalf("test has no ID for %s", descriptor.Name)
		}
		entries = append(entries, plugin.ProfileEntry{ID: id, Plugin: descriptor.Name, Scope: "root"})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "openrealtime.host.browser-e2e", Revision: 1,
		Realm: plugin.PresentationHostRealm, Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: entries, Exports: []plugin.ProfileExport{{
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

func requireBrowser(t *testing.T) (string, string) {
	t.Helper()
	missing := func(requirement string) {
		message := requirement + " is not installed"
		if os.Getenv("OPENREALTIME_RELEASE_GATE") != "" {
			t.Fatal(message + "; the release gate requires the real Chromium presentation tests")
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

func TestRequireBrowserCannotSkipAReleaseGate(t *testing.T) {
	if os.Getenv("OPENREALTIME_REQUIRE_BROWSER_HELPER") == "1" {
		requireBrowser(t)
		return
	}
	run := func(release bool) ([]byte, error) {
		command := exec.Command(os.Args[0], "-test.v", "-test.run=^TestRequireBrowserCannotSkipAReleaseGate$")
		releaseValue := ""
		if release {
			releaseValue = "1"
		}
		command.Env = []string{
			"PATH=",
			"CHROMIUM=",
			"OPENREALTIME_REQUIRE_BROWSER_HELPER=1",
			"OPENREALTIME_RELEASE_GATE=" + releaseValue,
		}
		return command.CombinedOutput()
	}
	releaseOutput, releaseErr := run(true)
	if releaseErr == nil || !strings.Contains(string(releaseOutput),
		"release gate requires the real Chromium presentation tests") {
		t.Fatalf("release browser prerequisite result: err=%v\n%s", releaseErr, releaseOutput)
	}
	localOutput, localErr := run(false)
	if localErr != nil || !strings.Contains(string(localOutput), "--- SKIP:") {
		t.Fatalf("local browser prerequisite result: err=%v\n%s", localErr, localOutput)
	}
}

func explicitTargetValues(
	t *testing.T,
	websocket string,
	webrtc string,
	managementURL string,
) json.RawMessage {
	t.Helper()
	endpoints := make([]presentation.Endpoint, 0, 3)
	if websocket != "" {
		endpoints = append(endpoints, presentation.Endpoint{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: websocket,
		})
	}
	if webrtc != "" {
		endpoints = append(endpoints, presentation.Endpoint{
			Name: presentation.EndpointRealtimeWebRTC, Protocol: presentation.ProtocolRealtimeWebRTC,
			URL: webrtc,
		})
	}
	if managementURL != "" {
		endpoints = append(endpoints, presentation.Endpoint{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: managementURL,
		})
	}
	payload, err := json.Marshal(host.EndpointDirectoryConfig{Endpoints: endpoints})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func managementTarget(t *testing.T, websocket string) string {
	t.Helper()
	parsed, err := url.Parse(websocket)
	if err != nil || parsed.Host == "" {
		t.Fatalf("test server WebSocket endpoint = %q, %v", websocket, err)
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		t.Fatalf("test server WebSocket endpoint has scheme %q", parsed.Scheme)
	}
	parsed.Path = management.APIPrefix
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}
