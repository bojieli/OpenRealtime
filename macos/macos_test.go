package macos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Apple frameworks are compiled in the macOS CI job. This Linux-side
// contract test keeps the native client from quietly losing one of the
// capability or safety boundaries while that runner is not in front of a
// contributor.
func TestNativeDeveloperClientDeclaresEveryCapabilityBoundary(t *testing.T) {
	required := map[string][]string{
		"Sources/OpenRealtimeMac/RealtimeClient.swift": {
			"URLSessionWebSocketTask", "input_audio_buffer.append",
			"openrealtime.input_video_frame.append", "conversation.item.create",
		},
		"Sources/OpenRealtimeMac/AudioIO.swift": {
			"AVAudioEngine", "pcmFormatInt16", "24_000", "interrupt()",
		},
		"Sources/OpenRealtimeMac/MediaCapture.swift": {
			"ScreenCaptureKit", "AVCaptureSession", "maxFrameBytes", "timestamp",
		},
		"Sources/OpenRealtimeMac/DesktopComputer.swift": {
			"AXIsProcessTrustedWithOptions", "selected display", "cghidEventTap",
		},
		"Sources/OpenRealtimeMac/BrowserUseBridge.swift": {
			"BrowserUseBridge", "click_element", "capture", "uv",
		},
		"Sources/OpenRealtimeMac/ToolHost.swift": {
			"resolvingSymlinksInPath", "write_file", "run_command",
			"display_artifact", "publish_download", "maxDownloadBytes",
		},
		"Sources/OpenRealtimeMac/DeveloperModel.swift": {
			"include_payloads", "response.function_call_arguments.done",
			"requestConfirmation", "openrealtime.debug.event",
		},
		"Sources/OpenRealtimeMac/ContentView.swift": {
			"Server debug timeline", "Raw protocol log", "Generated files",
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
