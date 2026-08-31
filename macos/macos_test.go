package macos

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The Apple frameworks are compiled in the macOS CI job. This Linux-side
// contract test keeps the native client from quietly losing one of the
// capability or safety boundaries while that runner is not in front of a
// contributor.
func TestNativeDeveloperClientDeclaresEveryCapabilityBoundary(t *testing.T) {
	required := map[string][]string{
		"Package.swift": {
			".product(name: \"OpenRealtimeClientCore\", package: \"OpenRealtimeClientCore\")",
		},
		"Sources/OpenRealtimeMac/RealtimeClient.swift": {
			"URLSessionWebSocketTask", "input_audio_buffer.append",
			"openrealtime.input_video_frame.append", "strictJSON.parse",
			"TransportDiagnosticsPublisher",
		},
		"Sources/OpenRealtimeMac/NativeReducerController.swift": {
			"RealtimeReducerService", "\"kind\": \"inbound\"", "\"kind\": \"tool_result\"",
			"next_retry_ms", "maxVirtualTimeMS", "inspectionAccess?.validate", "inspectionAccess?.capture",
			"inspectionAccess?.clear",
			"protocolEvents?.publish", "sessionConfiguration?.observe",
		},
		"Sources/OpenRealtimeMac/NativeClientAssembly.swift": {
			"NativeClientManifest.decodeStrict", "NativeClientComposition",
			"NativeEndpointDirectory.decodeStrict", "validate(selectedBy:",
			"NativeClientProviderRegistry", "NativeClientProviderFactoryRegistration",
			"NativeProviderFactoryContext", "SessionInspectionClient",
			"SessionConfigurationService", "NativeViewBoundary", "NativeViewServices",
			"macos.swiftui-observer-view.v1", "inspection.mount", "inspection.dispose",
			"client.deactivate", "configuration.contribute", "onDispose",
		},
		"../client/reducer/swift/NativeClientCore.swift": {
			"SessionInspectionAccessService", "SessionInspectionAccessProjection",
			"OpenRealtime-Management-Token", "configure(managementEndpoint:",
			"maximumResponseBytes = 32 << 20", "willPerformHTTPRedirection",
			"ValidatedProtocolEventService", "SessionConfigurationService",
			"TransportDiagnosticsService", "ClientArtifactsService",
			"ClientEffectInvocationEncoder", "maximumAuthorityBytes",
			"ProtocolEventPresentation", "redacted secret",
			"openrealtime.client-effects.v1", "/client/v1/effects",
			"NativeEffectEndpointResolver", "NativeEndpointDirectory", "declaration.catalogDigest",
			"providerLost", "remount",
		},
		"Sources/OpenRealtimeMac/NativePresentationProviders.swift": {
			"NativeMediaBoundary", "NativeVideoBoundary", "startBrowser",
			"BrowserUseController", "configuration.contribute",
		},
		"Sources/OpenRealtimeMac/HostEffects.swift": {
			"NativeEffectsBoundary", "response.function_call_arguments.done",
			"client.effects", "catalog_digest",
			"declaration_digest", "NativeEffectEndpointResolver.websocketURL",
			"ClientEffectInvocationEncoder.encode", "maximumWireBytes", "decideConfirmation",
			"willPerformHTTPRedirection",
		},
		"Sources/OpenRealtimeMac/HostedArtifacts.swift": {
			"NativeArtifactsBoundary", "ClientArtifactsPublisher",
			"/HostResources", "SHA256.hash", "expectedContentLength",
			"willPerformHTTPRedirection", "artifactHTML", "cachedDownload", "artifactEndpoint",
		},
		"Sources/OpenRealtimeMac/AudioIO.swift": {
			"AVAudioEngine", "pcmFormatInt16", "24_000", "interrupt()", ".dataPlayedBack",
		},
		"Sources/OpenRealtimeMac/MediaCapture.swift": {
			"ScreenCaptureKit", "AVCaptureSession", "maxFrameBytes", "timestamp",
		},
		"Sources/OpenRealtimeMac/BrowserUseBridge.swift": {
			"BrowserUseBridge", "click_element", "capture", "uv",
		},
		"Sources/OpenRealtimeMac/DeveloperModel.swift": {
			"effects?.configure()", "media.subscribe", "video.subscribe", "artifactService.subscribe",
			"artifactService?.configure()", "artifactService.artifactHTML",
			"let endpoint: String", "assembly.endpointDirectory.endpoint",
			"assembly.view.services.transportDiagnostics.subscribe", "inspection.subscribe",
			"inspection.deltas(after: 0, limit: 256)",
		},
		"Sources/OpenRealtimeMac/Models.swift": {
			"ProtocolEventPresentation.redacted", "safeProtocolPayload",
		},
		"Sources/OpenRealtimeMac/OpenRealtimeMacApp.swift": {
			"OPENREALTIME_NATIVE_ENDPOINT_DIRECTORY", "nativeEndpointDirectoryData",
			"isRegularFileKey", "1 << 20", "--openrealtime-connect-on-launch",
			"--openrealtime-hosted-smoke=", "OPENREALTIME_HOSTED_COMPANION_PROOF",
		},
		"Sources/OpenRealtimeMac/ContentView.swift": {
			"Server debug timeline", "Raw protocol log", "Generated files",
			"Canonical session inspection", "Bounded deltas", "Causal trace",
		},
		"Info.plist": {
			"NSCameraUsageDescription", "NSMicrophoneUsageDescription",
		},
	}
	for name, fragments := range required {
		content, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(content), fragment) {
				t.Errorf("%s no longer contains %q", name, fragment)
			}
		}
	}
	for _, retired := range []string{
		"Sources/OpenRealtimeMac/DesktopComputer.swift",
		"Sources/OpenRealtimeMac/ToolHost.swift",
	} {
		if _, err := os.Lstat(retired); !os.IsNotExist(err) {
			t.Errorf("retired built-in native effect implementation remains at %s: %v", retired, err)
		}
	}
	if _, err := os.Stat("verify-hosted-companion.sh"); err != nil {
		t.Fatalf("hosted companion gate is missing: %v", err)
	}
	if output, err := exec.Command("bash", "-n", "verify-hosted-companion.sh").CombinedOutput(); err != nil {
		t.Fatalf("hosted companion gate is not valid shell: %v\n%s", err, output)
	}
	if runtime.GOOS != "darwin" {
		if output, err := exec.Command("bash", "./verify-hosted-companion.sh").CombinedOutput(); err == nil {
			t.Fatalf("hosted companion gate unexpectedly passed off macOS: %s", output)
		} else if !strings.Contains(string(output), "requires macOS") {
			t.Fatalf("hosted companion gate did not fail at its platform boundary: %v\n%s", err, output)
		}
	}
}

func TestNativeNormalPathNeverInfersEndpointsOrPlacesCredentialsInURLs(t *testing.T) {
	files := []string{
		"Sources/OpenRealtimeMac/DeveloperModel.swift",
		"Sources/OpenRealtimeMac/NativeReducerController.swift",
		"Sources/OpenRealtimeMac/HostEffects.swift",
		"Sources/OpenRealtimeMac/HostedArtifacts.swift",
		"Sources/OpenRealtimeMac/NativeClientAssembly.swift",
	}
	for _, name := range files {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"configure(realtimeEndpoint:", "managementOrigin", "resourceOrigin",
			"legacySameOrigin", "legacyManagementBase", "client.unbind()",
		} {
			if strings.Contains(string(content), forbidden) {
				t.Errorf("normal native source %s retains endpoint inference %q", name, forbidden)
			}
		}
	}
	model, err := os.ReadFile("Sources/OpenRealtimeMac/DeveloperModel.swift")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(model), "token=") || strings.Contains(string(model), "?token") {
		t.Fatal("native model places a credential in an endpoint URL")
	}
	view, err := os.ReadFile("Sources/OpenRealtimeMac/ContentView.swift")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(view), "$model.endpoint") {
		t.Fatal("native view can mutate immutable endpoint-directory wiring")
	}
}

func TestNativeModelDoesNotReimplementProtocolReducer(t *testing.T) {
	model, err := os.ReadFile("Sources/OpenRealtimeMac/DeveloperModel.swift")
	if err != nil {
		t.Fatal(err)
	}
	transport, err := os.ReadFile("Sources/OpenRealtimeMac/RealtimeClient.swift")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"speechBuffer", "textBuffer", "responseOpen", "sendUserText", "answerTool",
		"output_audio_buffer.clear", "client.send([\"type\": \"response.create\"])",
		"LocalToolHost", "requestConfirmation", "response.function_call_arguments.done",
		"openrealtime.debug.event", "DesktopComputerController",
		"workspaceRoot", "computerMode", "accessibilityPermission",
	} {
		if strings.Contains(string(model), fragment) || strings.Contains(string(transport), fragment) {
			t.Errorf("native adapter reintroduced protocol state/command %q", fragment)
		}
	}
}

func TestNativeHostEffectsNeverFabricateOrPersistAuthority(t *testing.T) {
	effects, err := os.ReadFile("Sources/OpenRealtimeMac/HostEffects.swift")
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := os.ReadFile("Sources/OpenRealtimeMac/HostedArtifacts.swift")
	if err != nil {
		t.Fatal(err)
	}
	core, err := os.ReadFile("../client/reducer/swift/NativeClientCore.swift")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"private var authority", "let authority = UUID", "authority = UUID",
		"@Published var authority", "Authorization", "Bearer ",
		"LocalToolHost", "DesktopComputerController",
	} {
		if strings.Contains(string(effects), fragment) || strings.Contains(string(artifacts), fragment) ||
			strings.Contains(string(core), fragment) {
			t.Errorf("native host boundary contains forbidden authority path %q", fragment)
		}
	}
	for source, fragments := range map[string][]string{
		string(core): {
			"authorityValue[\"authority\"]", "\"authority\": authority",
			"private let wire: Data", "private let canonicalArguments: Data",
		},
		string(effects): {
			"ClientEffectInvocationEncoder.encode", "declaredEndpoint.catalogDigest",
			"message[\"catalog_digest\"]",
		},
	} {
		for _, fragment := range fragments {
			if !strings.Contains(source, fragment) {
				t.Errorf("native host effects lost exact validation %q", fragment)
			}
		}
	}
}

func TestSignedNativeGateFailsClosedOffMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the explicit signed_macos_e2e build-tag gate owns the macOS runner")
	}
	if _, err := os.Stat("verify-signed-e2e.sh"); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("bash", "./verify-signed-e2e.sh").CombinedOutput(); err == nil {
		t.Fatalf("signed native gate unexpectedly passed without runner credentials: %s", output)
	} else if !strings.Contains(string(output), "requires a macOS runner") {
		t.Fatalf("signed native gate did not fail on the platform boundary: %v\n%s", err, output)
	}
}

func TestBrowserUseBridgeIsPinnedAndUsesItsMarkRenderer(t *testing.T) {
	project, err := os.ReadFile("BrowserUseBridge/pyproject.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(project), "browser-use==0.12.6") {
		t.Fatal("the browser grounding dependency must be exactly pinned")
	}
	bridge, err := os.ReadFile("BrowserUseBridge/bridge.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"BrowserSession", "create_highlighted_screenshot_async", "selector_map", "ClickElementEvent",
	} {
		if !strings.Contains(string(bridge), fragment) {
			t.Errorf("bridge does not use browser-use %s", fragment)
		}
	}
}
