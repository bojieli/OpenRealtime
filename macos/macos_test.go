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
			"TransportDiagnosticsPublisher", "URLSessionWebSocketDelegate",
			"didOpenWithProtocol", "WebSocket opening handshake timed out",
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
			"OPENREALTIME_HOSTED_COMPANION_", "ProcessInfo.processInfo.systemUptime",
			"Darwin.exit(EXIT_FAILURE)",
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

func TestNativeRealtimeOpenWaitUsesTheHandshakeDelegateAndCannotHangOnPing(t *testing.T) {
	transport, err := os.ReadFile("Sources/OpenRealtimeMac/RealtimeClient.swift")
	if err != nil {
		t.Fatal(err)
	}
	source := string(transport)
	for _, required := range []string{
		"URLSessionWebSocketDelegate", "didOpenWithProtocol", "withTaskCancellationHandler",
		"DispatchWorkItem", "sessionDelegate?.cancel", "delegate.waitUntilOpen",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("native realtime transport lost bounded handshake delegate contract %q", required)
		}
	}
	for _, forbidden := range []string{"sendPing", "withThrowingTaskGroup"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("native realtime transport retains cancellation-unsafe open probe %q", forbidden)
		}
	}
}

func TestHostedCompanionGateBindsBrowserAndNativePhasesBeforeFinalCredentialScan(t *testing.T) {
	content, err := os.ReadFile("verify-hosted-companion.sh")
	if err != nil {
		t.Fatal(err)
	}
	gate := string(content)
	helperStart := strings.Index(gate, "assert_frozen_companion_phase() {")
	helperEnd := strings.Index(gate, "\n}\n\nassert_no_private_material() {")
	if helperStart < 0 || helperEnd <= helperStart {
		t.Fatal("hosted companion frozen-phase helper is not one bounded shell function")
	}
	helper := gate[helperStart:helperEnd]
	for _, fragment := range []string{
		"server_identity", "${process_validator}",
		"listener_owner 18765", "listener_owner 18766", "listener_owner 18767",
		"listener_owner 18768", "${initial_process_receipt}",
	} {
		if !strings.Contains(helper, fragment) {
			t.Errorf("hosted companion phase receipt no longer contains %q", fragment)
		}
	}

	browserProof := strings.Index(gate, `browser_session_id="$(python3`)
	afterBrowser := strings.Index(gate, `assert_frozen_companion_phase "after browser completion"`)
	manifestRead := strings.Index(gate, `manifest_resource="$(find`)
	beforeNative := strings.Index(gate, `assert_frozen_companion_phase "immediately before native launch"`)
	nativeLaunch := strings.Index(gate, `env -u OPENREALTIME_HOSTED_COMPANION_TOKEN \
OPENREALTIME_NATIVE_PROFILE=observer-developer`)
	if browserProof < 0 || afterBrowser <= browserProof || manifestRead <= afterBrowser ||
		beforeNative <= manifestRead || nativeLaunch <= beforeNative {
		t.Fatalf(
			"hosted companion phase checks are not ordered around browser proof and native launch: proof=%d after=%d manifest=%d before=%d launch=%d",
			browserProof, afterBrowser, manifestRead, beforeNative, nativeLaunch,
		)
	}
	between := strings.TrimSpace(gate[beforeNative+len(
		`assert_frozen_companion_phase "immediately before native launch"`,
	) : nativeLaunch])
	if between != "" {
		t.Fatalf("native launch is no longer immediately preceded by its frozen phase check: %q", between)
	}

	const scan = "\nassert_no_private_material\n"
	firstScan := strings.Index(gate, scan)
	if firstScan < 0 {
		t.Fatal("hosted companion gate has no credential scan")
	}
	secondRelative := strings.Index(gate[firstScan+len(scan):], scan)
	if secondRelative < 0 {
		t.Fatal("hosted companion gate does not repeat its credential scan")
	}
	secondScan := firstScan + len(scan) + secondRelative
	if strings.Index(gate[secondScan+len(scan):], scan) >= 0 {
		t.Fatal("hosted companion gate has an unexpected third credential scan")
	}
	applicationWait := strings.Index(gate, `if ! wait "${application_pid}"; then`)
	companionWait := strings.Index(gate, `if ! wait "${companion_pid}"; then`)
	if firstScan >= applicationWait || applicationWait < 0 || companionWait <= applicationWait ||
		secondScan <= companionWait {
		t.Fatalf(
			"credential scans do not bracket quiescent app and companion logs: first=%d app_wait=%d companion_wait=%d final=%d",
			firstScan, applicationWait, companionWait, secondScan,
		)
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

// The reducer always projects session.openrealtime.video, stating zeroes when
// the capability was not enabled. The native video boundary used to validate
// those zeroes against its own ceiling on every snapshot, report a ceiling
// breach that never happened, and schedule a teardown; the teardown emitted an
// unconditional "browser closed" source update that the session refused, and
// the refusal was another snapshot. One connection to a binding without a video
// observer therefore span a source-update/error loop for the life of the
// session. These are the exact guards that keep that loop closed.
func TestNativeVideoIsSilentUntilVideoInputIsNegotiated(t *testing.T) {
	providers, err := os.ReadFile("Sources/OpenRealtimeMac/NativePresentationProviders.swift")
	if err != nil {
		t.Fatal(err)
	}
	browser, err := os.ReadFile("Sources/OpenRealtimeMac/BrowserUseBridge.swift")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		// Limits are only read once video.input is in the negotiated set.
		"if offered, let raw = extensionObject?[\"video\"] as? [String: Any] {",
		// A refused ceiling never tears down on its own; the single teardown
		// path below stays guarded by the active set.
		"if !enabled, !active.isEmpty { Task { [weak self] in await self?.stopAll() } }",
		// Source declarations are wire traffic only on a session that has them.
		"if negotiated {\n            transport.updateVideoSource(",
	} {
		if !strings.Contains(string(providers), fragment) {
			t.Errorf("native video boundary no longer contains %q", fragment)
		}
	}
	if strings.Contains(string(providers), "enabled = false\n                Task { [weak self] in await self?.stopAll() }") {
		t.Error("native video boundary schedules an unguarded teardown from limit validation")
	}
	if !strings.Contains(string(browser), "if declared { onSource?(\"browser\", \"closed\", 0, 0) }") {
		t.Error("browser capture announces a close for a source it never declared active")
	}
}
