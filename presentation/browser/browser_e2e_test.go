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
	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/internal/testgate"
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
		{Operation: management.RenameDocument, Resource: "authoring"},
		{Operation: management.RemoveDocumentEdge, Resource: "authoring"},
		{Operation: management.CreateDocumentEdge, Resource: "authoring"},
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
	replacementModules := []struct {
		entry, source, candidate string
	}{
		{"slots", "slots.js", "slots-v2.js"},
		{"transport", "transport-websocket.js", "transport-websocket-v2.js"},
		{"session-configuration", "session-configuration.js", "session-configuration-v2.js"},
		{"effects", "effects-client.js", "effects-client-v2.js"},
		{"artifact-references", "artifact-references.js", "artifact-references-v2.js"},
		{"debug-session", "debug-session.js", "debug-session-v2.js"},
		{"inspection", "inspection-client.js", "inspection-client-v2.js"},
		{"management-operator", "management-operator-capability.js", "management-operator-capability-v2.js"},
		{"management-transport", "management-transport.js", "management-transport-v2.js"},
		{"management-static", "management-static.js", "management-static-v2.js"},
		{"management-authoring", "management-authoring.js", "management-authoring-v2.js"},
		{"management-source-reading", "management-source-reading.js", "management-source-reading-v2.js"},
		{"management-source-publication", "management-source-publication.js", "management-source-publication-v2.js"},
		{"authoring-workspace", "authoring-workspace.js", "authoring-workspace-v2.js"},
		{"view", "text-view.js", "text-view-v2.js"},
		{"confirmation-view", "confirmation-view.js", "confirmation-view-v2.js"},
		{"artifact-view", "artifact-view.js", "artifact-view-v2.js"},
		{"inspection-view", "inspection-view.js", "inspection-view-v2.js"},
		{"trace-view", "trace-view.js", "trace-view-v2.js"},
		{"management-operator-view", "management-operator-view.js", "management-operator-view-v2.js"},
		{"authoring-editor-view", "authoring-editor-view.js", "authoring-editor-view-v2.js"},
		{"authoring-configuration-view", "authoring-configuration-view.js", "authoring-configuration-view-v2.js"},
		{"authoring-canvas-view", "authoring-canvas-view.js", "authoring-canvas-view-v2.js"},
	}
	alternatives := make([]presentationbrowser.DeveloperImplementationAlternative, 0, len(replacementModules)+1)
	for _, module := range replacementModules {
		source, readErr := os.ReadFile(filepath.Join("assets", module.source))
		if readErr != nil {
			t.Fatal(readErr)
		}
		candidate := append(append([]byte(nil), source...),
			[]byte("\n// shipped "+module.entry+" replacement candidate\n")...)
		alternatives = append(alternatives, presentationbrowser.DeveloperImplementationAlternative{
			Entry: module.entry, Entrypoint: module.candidate, Source: candidate,
		})
	}
	reducerCore, err := clientreducer.JavaScriptSource()
	if err != nil {
		t.Fatal(err)
	}
	reducerAdapter, err := os.ReadFile(filepath.Join("assets", "reducer-adapter.js"))
	if err != nil {
		t.Fatal(err)
	}
	reducerCandidate := make([]byte, 0, len(reducerCore)+len(reducerAdapter)+64)
	reducerCandidate = append(reducerCandidate, reducerCore...)
	reducerCandidate = append(reducerCandidate, '\n', '\n')
	reducerCandidate = append(reducerCandidate, reducerAdapter...)
	reducerCandidate = append(reducerCandidate, []byte("\n// shipped reducer replacement candidate\n")...)
	alternatives = append(alternatives, presentationbrowser.DeveloperImplementationAlternative{
		Entry: "reducer", Entrypoint: "reducer-v2.js", Source: reducerCandidate,
	})
	bundle, err := presentationbrowser.ComposeDeveloperBundle(
		"openrealtime.browser.developer", effects.CatalogDigest(), alternatives,
	)
	if err != nil {
		t.Fatal(err)
	}
	reducerReplacement := replacementBrowserManifest(t, bundle.Manifest, "reducer", "reducer-v2.js")
	replacement := reducerReplacement
	for _, module := range replacementModules {
		replacement = replacementBrowserManifest(t, replacement, module.entry, module.candidate)
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
	const reducerReplacementPath = "/test/developer-reducer-replacement.json"
	const replacementPath = "/test/developer-shipped-consumers-replacement.json"
	var effectConnectionStarts atomic.Int32
	var activeEffectConnections atomic.Int32
	var realtimeConnectionStarts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == reducerReplacementPath {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			if err := json.NewEncoder(writer).Encode(reducerReplacement); err != nil {
				t.Errorf("encode reducer replacement manifest: %v", err)
			}
			return
		}
		if request.URL.Path == replacementPath {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			if err := json.NewEncoder(writer).Encode(replacement); err != nil {
				t.Errorf("encode shipped-consumer replacement manifest: %v", err)
			}
			return
		}
		if request.URL.Path == "/client/v1/effects" {
			effectConnectionStarts.Add(1)
			activeEffectConnections.Add(1)
			defer activeEffectConnections.Add(-1)
		}
		if request.URL.Path == "/client/v1/realtime" {
			realtimeConnectionStarts.Add(1)
		}
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	driver, err := filepath.Abs(filepath.Join("testdata", "developer.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	authoringYAML, authoringJSON := normalizedAuthoringSources(t, stack.AuthoringSource)
	command := exec.CommandContext(ctx, node, driver, server.URL)
	command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t),
		"EXPECT_EFFECTS=1", "CLIENT_TRANSPORT=websocket",
		"REDUCER_REPLACEMENT_PATH="+reducerReplacementPath,
		"EFFECTS_REPLACEMENT_PATH="+replacementPath,
		"OPERATOR_CAPABILITY="+operatorOne.Token,
		"OPERATOR_CAPABILITY_ROTATED="+operatorTwo.Token,
		"AUTHORING_SOURCE="+stack.AuthoringSource,
		"AUTHORING_UPDATED_SOURCE="+stack.AuthoringSource+"\n",
		"AUTHORING_YAML_SOURCE="+authoringYAML,
		"AUTHORING_JSON_SOURCE="+authoringJSON,
		"SOURCE_ROOT_IDENTITY="+sourceRootIdentity,
		"STATIC_GRAPH_FINGERPRINT="+stack.Graph.Fingerprint,
	)
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		t.Fatalf("developer browser profile failed: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for activeEffectConnections.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if starts, active := effectConnectionStarts.Load(), activeEffectConnections.Load(); starts != 3 || active != 0 {
		t.Fatalf("effects replacement connections started/active = %d/%d, want 3/0", starts, active)
	}
	if starts := realtimeConnectionStarts.Load(); starts != 2 {
		t.Fatalf("realtime WebSocket connections started = %d, want 2", starts)
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
			authoringYAML, authoringJSON := normalizedAuthoringSources(t, stack.AuthoringSource)
			command := exec.CommandContext(ctx, node, driver, server.URL)
			command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t),
				"EXPECT_EFFECTS=0", "CLIENT_TRANSPORT="+test.transport,
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
	webrtcReplacementModules := []struct {
		entry, source, candidate string
	}{
		{"slots", "slots.js", "slots-v2.js"},
		{"media", "media-webrtc.js", "media-webrtc-v2.js"},
		{"transport", "transport-webrtc.js", "transport-webrtc-v2.js"},
		{"session-configuration", "session-configuration.js", "session-configuration-v2.js"},
		{"video", "video-protocol.js", "video-protocol-v2.js"},
		{"debug-session", "debug-session.js", "debug-session-v2.js"},
		{"effects", "effects-client.js", "effects-client-v2.js"},
		{"artifact-references", "artifact-references.js", "artifact-references-v2.js"},
		{"inspection", "inspection-client.js", "inspection-client-v2.js"},
		{"video-controls", "video-controls.js", "video-controls-v2.js"},
		{"transport-diagnostics", "transport-diagnostics-view.js", "transport-diagnostics-view-v2.js"},
	}
	webrtcAlternatives := make([]presentationbrowser.DeveloperImplementationAlternative, 0,
		len(webrtcReplacementModules))
	for _, module := range webrtcReplacementModules {
		source, readErr := os.ReadFile(filepath.Join("assets", module.source))
		if readErr != nil {
			t.Fatal(readErr)
		}
		candidate := append(append([]byte(nil), source...),
			[]byte("\n// shipped "+module.entry+" replacement candidate\n")...)
		webrtcAlternatives = append(webrtcAlternatives,
			presentationbrowser.DeveloperImplementationAlternative{
				Entry: module.entry, Entrypoint: module.candidate, Source: candidate,
			})
	}
	bundle, err := presentationbrowser.ComposeDeveloperWebRTCBundle(
		"openrealtime.browser.developer-webrtc", effects.CatalogDigest(), webrtcAlternatives,
	)
	if err != nil {
		t.Fatal(err)
	}
	clientReplacement := bundle.Manifest
	for _, module := range webrtcReplacementModules {
		clientReplacement = replacementBrowserManifest(t, clientReplacement, module.entry, module.candidate)
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
	const clientReplacementPath = "/test/developer-webrtc-client-replacement.json"
	var realtimeOfferStarts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == clientReplacementPath {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			if err := json.NewEncoder(writer).Encode(clientReplacement); err != nil {
				t.Errorf("encode WebRTC client replacement manifest: %v", err)
			}
			return
		}
		if request.URL.Path == "/client/v1/realtime/calls" {
			realtimeOfferStarts.Add(1)
		}
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	driver, err := filepath.Abs(filepath.Join("testdata", "webrtc.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, driver, server.URL)
	command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t),
		"CLIENT_REPLACEMENT_PATH="+clientReplacementPath)
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		t.Fatalf("developer WebRTC browser profile failed: %v", err)
	}
	if starts := realtimeOfferStarts.Load(); starts != 3 {
		t.Fatalf("realtime WebRTC offers started = %d, want 3", starts)
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

func TestBrowserReplacesAuthenticatedClientImplementationAndRollsBackInChromium(t *testing.T) {
	node, chromium := requireBrowser(t)
	providerV1 := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.replaceable",revision:1,async mount(context){
  increment(context.root, "providerV1Mounts");
  context.root.dataset.replaceableProvider = "v1";
  context.publish("presentation.client.artifacts", Object.freeze({version:"v1"}));
  context.lifecycle.defer("replaceable-v1", () => {
    increment(context.root, "providerV1Disposals");
    if (context.root.dataset.replaceableProvider === "v1") delete context.root.dataset.replaceableProvider;
  });
}};`)
	providerV2 := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.replaceable",revision:1,async mount(context){
  increment(context.root, "providerV2Mounts");
  context.root.dataset.replaceableProvider = "v2";
  context.publish("presentation.client.artifacts", Object.freeze({version:"v2"}));
  context.lifecycle.defer("replaceable-v2", () => {
    increment(context.root, "providerV2Disposals");
    if (context.root.dataset.replaceableProvider === "v2") delete context.root.dataset.replaceableProvider;
  });
}};`)
	providerV3 := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.replaceable",revision:1,async mount(context){
  increment(context.root, "providerV3Mounts");
  context.root.dataset.replaceableProvider = "v3";
  context.publish("presentation.client.artifacts", Object.freeze({version:"v3"}));
  context.lifecycle.defer("replaceable-v3", () => {
    increment(context.root, "providerV3Disposals");
    if (context.root.dataset.replaceableProvider === "v3") delete context.root.dataset.replaceableProvider;
  });
}};`)
	providerFailure := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.replaceable",revision:1,async mount(context){
  increment(context.root, "failedCandidateMounts");
  context.root.dataset.replaceableProvider = "failed";
  context.publish("presentation.client.artifacts", Object.freeze({version:"failed"}));
  context.lifecycle.defer("replaceable-failure", () => {
    increment(context.root, "failedCandidateDisposals");
    if (context.root.dataset.replaceableProvider === "failed") delete context.root.dataset.replaceableProvider;
  });
  throw new Error("intentional replacement activation failure");
}};`)
	providerWrongIdentity := []byte(`
export default {name:"example.client.substituted",revision:1,async mount(context){
  context.root.dataset.wrongIdentityMounted = "yes";
}};`)
	consumer := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.replaceable-view",revision:1,async mount(context){
  const provider = context.services.get("presentation.client.artifacts");
  if (!provider?.version) throw new Error("replaceable provider is unavailable");
  increment(context.root, "replaceableConsumerMounts");
  context.root.dataset.replaceableConsumer = provider.version;
  context.lifecycle.defer("replaceable-consumer", () => {
    increment(context.root, "replaceableConsumerDisposals");
    if (context.root.dataset.replaceableConsumer === provider.version) delete context.root.dataset.replaceableConsumer;
  });
}};`)
	consumerV2 := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.replaceable-view",revision:1,async mount(context){
  const provider = context.services.get("presentation.client.artifacts");
  if (!provider?.version) throw new Error("replaceable provider is unavailable");
  increment(context.root, "multiConsumerMounts");
  context.root.dataset.replaceableConsumer = "view2-" + provider.version;
  context.lifecycle.defer("replaceable-consumer-v2", () => {
    increment(context.root, "multiConsumerDisposals");
    if (context.root.dataset.replaceableConsumer === "view2-" + provider.version) {
      delete context.root.dataset.replaceableConsumer;
    }
  });
}};`)
	consumerFailure := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.replaceable-view",revision:1,async mount(context){
  increment(context.root, "multiFailedConsumerMounts");
  context.lifecycle.defer("replaceable-consumer-failure", () => {
    increment(context.root, "multiFailedConsumerDisposals");
  });
  throw new Error("intentional multi-row consumer activation failure");
}};`)
	statefulV1 := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.stateful",revision:1,async mount(context){
  const state = context.state.restored() ?? {counter:7,owner:"v1"};
  increment(context.root, "statefulV1Mounts");
  context.root.dataset.statefulProvider = state.owner + ":" + state.counter;
  context.state.snapshot(() => {
    increment(context.root, "statefulProviderSnapshots");
    if (context.root.dataset.failStatefulSnapshot === "true") {
      throw new Error("intentional state snapshot failure");
    }
    return {...state};
  });
  context.publish("example.client.stateful", Object.freeze({version:"v1",counter:state.counter}));
  context.lifecycle.defer("stateful-v1", () => {
    increment(context.root, "statefulV1Disposals");
    if (context.root.dataset.statefulProvider === state.owner + ":" + state.counter) {
      delete context.root.dataset.statefulProvider;
    }
  });
}};`)
	statefulV2 := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.stateful",revision:1,
async migrateState(input){
  if (input.source_implementation !== "browser-esm:stateful-v1.js" ||
      input.schema.name !== "example.client.stateful.state" || input.snapshot.owner !== "v1") {
    throw new Error("unexpected state migration input");
  }
  return {counter:input.snapshot.counter + 1,owner:"v2"};
},async mount(context){
  const state = context.state.restored();
  increment(context.root, "statefulV2Mounts");
  context.root.dataset.statefulProvider = state.owner + ":" + state.counter;
  context.state.snapshot(() => ({...state}));
  context.publish("example.client.stateful", Object.freeze({version:"v2",counter:state.counter}));
  context.lifecycle.defer("stateful-v2", () => {
    increment(context.root, "statefulV2Disposals");
    if (context.root.dataset.statefulProvider === state.owner + ":" + state.counter) {
      delete context.root.dataset.statefulProvider;
    }
  });
  if (context.root.dataset.failStatefulV2Mount === "true") {
    throw new Error("intentional stateful recovery failure");
  }
}};`)
	statefulFailure := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.stateful",revision:1,
async migrateState(input){return {counter:input.snapshot.counter + 100,owner:"failure"};},
async mount(context){
  const state = context.state.restored();
  increment(context.root, "statefulFailedMounts");
  context.root.dataset.statefulProvider = state.owner + ":" + state.counter;
  context.state.snapshot(() => ({...state}));
  context.publish("example.client.stateful", Object.freeze({version:"failure",counter:state.counter}));
  context.lifecycle.defer("stateful-failure", () => {
    increment(context.root, "statefulFailedDisposals");
    if (context.root.dataset.statefulProvider === state.owner + ":" + state.counter) {
      delete context.root.dataset.statefulProvider;
    }
  });
  throw new Error("intentional stateful activation failure");
}};`)
	statefulNoMigrator := []byte(`
export default {name:"example.client.stateful",revision:1,async mount(context){
  const state = context.state.restored();
  context.state.snapshot(() => ({...state}));
}};`)
	statefulInvalidMigration := []byte(`
export default {name:"example.client.stateful",revision:1,
async migrateState(){return [];},async mount(){throw new Error("invalid migration mounted");}};`)
	statefulView := []byte(`
const increment = (root, name) => { root.dataset[name] = String(Number(root.dataset[name] || "0") + 1); };
export default {name:"example.client.stateful-view",revision:1,async mount(context){
  const state = context.state.restored() ?? {renders:11};
  const provider = context.services.get("example.client.stateful");
  increment(context.root, "statefulViewMounts");
  context.root.dataset.statefulView = provider.version + ":" + state.renders;
  context.state.snapshot(() => {
    increment(context.root, "statefulViewSnapshots");
    return {...state};
  });
  context.lifecycle.defer("stateful-view", () => {
    increment(context.root, "statefulViewDisposals");
    if (context.root.dataset.statefulView === provider.version + ":" + state.renders) {
      delete context.root.dataset.statefulView;
    }
  });
}};`)
	statefulService := plugin.Contract{
		Name: "example.client.stateful", Revision: 1, Digest: "sha256:" + strings.Repeat("6", 64),
	}
	statefulSchema := plugin.Contract{
		Name: "example.client.stateful.state", Revision: 1,
		Digest: "sha256:" + strings.Repeat("7", 64),
	}
	statefulViewSchema := plugin.Contract{
		Name: "example.client.stateful-view.state", Revision: 1,
		Digest: "sha256:" + strings.Repeat("8", 64),
	}
	bundle, err := presentationbrowser.ComposeTextBundle(
		"openrealtime.browser.replacement-test", []presentationbrowser.ClientModule{
			{
				Entry: "replaceable", Entrypoint: "replaceable-v1.js",
				PluginName: "example.client.replaceable", Source: providerV1,
				Alternatives: []presentationbrowser.ClientModuleAlternative{
					{Entrypoint: "replaceable-v2.js", Source: providerV2},
					{Entrypoint: "replaceable-v3.js", Source: providerV3},
					{Entrypoint: "replaceable-failure.js", Source: providerFailure},
					{Entrypoint: "replaceable-wrong-identity.js", Source: providerWrongIdentity},
				},
				Provides: []plugin.Contract{presentation.ClientArtifactsContract},
			},
			{
				Entry: "replaceable-view", Entrypoint: "replaceable-view.js",
				PluginName: "example.client.replaceable-view", Source: consumer,
				Alternatives: []presentationbrowser.ClientModuleAlternative{
					{Entrypoint: "replaceable-view-v2.js", Source: consumerV2},
					{Entrypoint: "replaceable-view-failure.js", Source: consumerFailure},
				},
				Requires: []plugin.Requirement{{Contract: presentation.ClientArtifactsContract}},
			},
			{
				Entry: "stateful", Entrypoint: "stateful-v1.js",
				PluginName: "example.client.stateful", Source: statefulV1,
				Alternatives: []presentationbrowser.ClientModuleAlternative{
					{Entrypoint: "stateful-v2.js", Source: statefulV2},
					{Entrypoint: "stateful-failure.js", Source: statefulFailure},
					{Entrypoint: "stateful-no-migrator.js", Source: statefulNoMigrator},
					{Entrypoint: "stateful-invalid-migration.js", Source: statefulInvalidMigration},
				},
				Provides: []plugin.Contract{statefulService}, StateSchema: &statefulSchema,
				Lifecycle: plugin.Lifecycle{Snapshot: true, Restore: true},
			},
			{
				Entry: "stateful-view", Entrypoint: "stateful-view.js",
				PluginName: "example.client.stateful-view", Source: statefulView,
				Requires:    []plugin.Requirement{{Contract: statefulService}},
				StateSchema: &statefulViewSchema,
				Lifecycle:   plugin.Lifecycle{Snapshot: true, Restore: true},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	v2 := replacementBrowserManifest(t, bundle.Manifest, "replaceable", "replaceable-v2.js")
	failing := replacementBrowserManifest(t, bundle.Manifest, "replaceable", "replaceable-failure.js")
	wrongIdentity := replacementBrowserManifest(
		t, bundle.Manifest, "replaceable", "replaceable-wrong-identity.js",
	)
	multi := replacementBrowserManifest(t, v2, "replaceable", "replaceable-v3.js")
	multi = replacementBrowserManifest(t, multi, "replaceable-view", "replaceable-view-v2.js")
	multiFailure := replacementBrowserManifest(t, v2, "replaceable", "replaceable-v3.js")
	multiFailure = replacementBrowserManifest(
		t, multiFailure, "replaceable-view", "replaceable-view-failure.js",
	)
	statefulV2Manifest := replacementBrowserManifest(t, multi, "stateful", "stateful-v2.js")
	statefulFailureManifest := replacementBrowserManifest(
		t, multi, "stateful", "stateful-failure.js",
	)
	statefulNoMigratorManifest := replacementBrowserManifest(
		t, multi, "stateful", "stateful-no-migrator.js",
	)
	statefulInvalidMigrationManifest := replacementBrowserManifest(
		t, multi, "stateful", "stateful-invalid-migration.js",
	)

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
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{
			"target": explicitTargetValues(t, "ws://127.0.0.1:1", "", ""),
		},
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var candidate *presentation.ClientManifest
		switch request.URL.Path {
		case "/test/replacement-v2.json":
			candidate = &v2
		case "/test/replacement-failure.json":
			candidate = &failing
		case "/test/replacement-wrong-identity.json":
			candidate = &wrongIdentity
		case "/test/replacement-multi.json":
			candidate = &multi
		case "/test/replacement-multi-failure.json":
			candidate = &multiFailure
		case "/test/replacement-stateful-v2.json":
			candidate = &statefulV2Manifest
		case "/test/replacement-stateful-failure.json":
			candidate = &statefulFailureManifest
		case "/test/replacement-stateful-no-migrator.json":
			candidate = &statefulNoMigratorManifest
		case "/test/replacement-stateful-invalid-migration.json":
			candidate = &statefulInvalidMigrationManifest
		}
		if candidate != nil {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			if err := json.NewEncoder(writer).Encode(candidate); err != nil {
				t.Errorf("encode replacement manifest: %v", err)
			}
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	driver, err := filepath.Abs(filepath.Join("testdata", "replacement.mjs"))
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
		t.Fatalf("browser implementation replacement failed: %v", err)
	}
}

func replacementBrowserManifest(
	t *testing.T, source presentation.ClientManifest, entry, entrypoint string,
) presentation.ClientManifest {
	t.Helper()
	result := source.Clone()
	var digest string
	for _, asset := range result.Assets {
		if asset.Entry == entry && asset.Name == entrypoint {
			digest = asset.Digest
			break
		}
	}
	if digest == "" {
		t.Fatalf("replacement asset %s/%s is absent", entry, entrypoint)
	}
	found := false
	for index := range result.Implementations {
		if result.Implementations[index].Entry != entry {
			continue
		}
		found = true
		result.Implementations[index].Implementation = "browser-esm:" + entrypoint
		result.Implementations[index].Artifact.ID =
			"module://" + strings.TrimSuffix(entrypoint, ".js")
		result.Implementations[index].Artifact.Digest = digest
		result.Implementations[index].Entrypoint = entrypoint
	}
	if !found {
		t.Fatalf("replacement implementation entry %s is absent", entry)
	}
	frozen, err := presentation.FreezeManifest(result)
	if err != nil {
		t.Fatal(err)
	}
	return frozen
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
	missing := func(requirement string) { testgate.Missing(t, requirement) }
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
		"a release gate cannot skip what it was asked to verify") {
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

func normalizedAuthoringSources(t testing.TB, source string) (string, string) {
	t.Helper()
	file, err := syntax.Parse("browser-authoring.ortg", []byte(source))
	if err != nil {
		t.Fatalf("parse browser authoring source for normalized fixtures: %v", err)
	}
	document := manifest.FromSyntax(file)
	yamlSource, err := manifest.MarshalYAML(document)
	if err != nil {
		t.Fatalf("marshal browser authoring YAML fixture: %v", err)
	}
	jsonSource, err := manifest.MarshalJSON(document)
	if err != nil {
		t.Fatalf("marshal browser authoring JSON fixture: %v", err)
	}
	return string(yamlSource), string(jsonSource)
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
