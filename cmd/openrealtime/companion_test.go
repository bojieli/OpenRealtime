package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/macos"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
)

func TestCompanionSupervisesExactPublicProcessesWithNoClientLaunch(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	serverAddress := companionFreeAddress(t)
	webRTCAddress := companionFreeAddress(t)
	presentationAddress := companionFreeAddress(t)
	logPath := filepath.Join(t.TempDir(), "children.jsonl")
	secret := "companion-secret-must-not-escape"
	var browserLaunches, macOSLaunches atomic.Int64
	readyChannel := make(chan companionReady, 1)
	output := &companionLockedBuffer{}
	runtime := companionRuntime{
		executable: executable,
		prefix:     []string{"-test.run=^TestCompanionHelperProcess$", "--"},
		environment: append(os.Environ(),
			"OPENREALTIME_COMPANION_HELPER=1",
			"OPENREALTIME_COMPANION_HELPER_LOG="+logPath,
			"OPENREALTIME_COMPANION_TEST_TOKEN="+secret,
		),
		goos:        "linux",
		httpClient:  &http.Client{Timeout: time.Second},
		childOutput: output,
		launchBrowser: func(context.Context, string) error {
			browserLaunches.Add(1)
			return nil
		},
		launchMacOS: func(context.Context, string, string) error {
			macOSLaunches.Add(1)
			return nil
		},
		onReady: func(ready companionReady) { readyChannel <- ready },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runCompanionContext(ctx, []string{
			"-server-listen", serverAddress,
			"-webrtc-listen", webRTCAddress,
			"-presentation-listen", presentationAddress,
			"-token-env", "OPENREALTIME_COMPANION_TEST_TOKEN",
			"-client", "none",
			"-ready-timeout", "10s",
			"-shutdown-timeout", "2s",
			"--", "-binding", "upstream", "-upstream-model", "model with spaces",
		}, output, runtime)
	}()

	var ready companionReady
	select {
	case ready = <-readyChannel:
	case err := <-done:
		t.Fatalf("companion stopped before readiness: %v\n%s", err, output.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("companion readiness timed out\n%s", output.String())
	}
	if ready.ServerURL != "http://"+serverAddress ||
		ready.WebRTCURL != "http://"+webRTCAddress+"/v1/realtime/calls" ||
		ready.PresentationURL != "http://"+presentationAddress ||
		ready.ManagementURL != "http://"+serverAddress+management.APIPrefix {
		t.Fatalf("companion ready projection = %#v", ready)
	}
	if ready.BrowserManifest.Platform != "browser" || ready.BrowserManifest.Fingerprint == "" {
		t.Fatalf("companion manifest = %#v", ready.BrowserManifest)
	}
	if ready.NativeEndpointFile == "" {
		t.Fatalf("custom presentation did not publish native endpoint file: %#v", ready)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(ready.NativeEndpointFile)) })
	payload, err := os.ReadFile(ready.NativeEndpointFile)
	if err != nil {
		t.Fatal(err)
	}
	var directory presentation.EndpointDirectory
	if err := json.Unmarshal(payload, &directory); err != nil {
		t.Fatal(err)
	}
	if err := directory.Validate(); err != nil {
		t.Fatal(err)
	}
	if directory.Fingerprint != ready.NativeEndpointDirectory.Fingerprint {
		t.Fatalf("native file fingerprint = %s, want %s", directory.Fingerprint, ready.NativeEndpointDirectory.Fingerprint)
	}
	if _, err := macos.NewNativeBundleForDistributionWithEndpointDirectory(
		macos.NativeObserverDeveloperDistribution, directory,
	); err != nil {
		t.Fatal(err)
	}
	if browserLaunches.Load() != 0 || macOSLaunches.Load() != 0 {
		t.Fatalf("none policy launched browser=%d macos=%d", browserLaunches.Load(), macOSLaunches.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("companion shutdown: %v\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("companion did not stop children\n%s", output.String())
	}
	if _, err := os.Stat(ready.NativeEndpointFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native endpoint file remains after companion shutdown: %v", err)
	}
	text := output.String()
	for _, required := range []string{
		"[serve] helper serve stdout", "[serve] helper serve stderr",
		"[present] helper present stdout", "[present] helper present stderr",
		"OpenRealtime companion ready", "client       none",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("companion output omitted %q:\n%s", required, text)
		}
	}
	if strings.Contains(text, secret) {
		t.Fatal("companion output exposed the credential value")
	}

	logPayload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logPayload), secret) {
		t.Fatal("companion placed the credential value in child argv")
	}
	var records []companionHelperRecord
	for _, line := range bytes.Split(bytes.TrimSpace(logPayload), []byte{'\n'}) {
		var record companionHelperRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 4 || records[0].Event != "serve-start" ||
		records[1].Event != "present-start" || records[2].Event != "present-stop" ||
		records[3].Event != "serve-stop" {
		t.Fatalf("companion lifecycle = %#v", records)
	}
	serveArguments, presentArguments := records[0].Arguments, records[1].Arguments
	companionRequireArgument(t, serveArguments, "-listen", serverAddress)
	companionRequireArgument(t, serveArguments, "-webrtc-listen", webRTCAddress)
	companionRequireArgument(t, serveArguments, "-webrtc-allow-origin", "http://"+presentationAddress)
	companionRequireArgument(t, serveArguments, "-token-env", "OPENREALTIME_COMPANION_TEST_TOKEN")
	companionRequireArgument(t, serveArguments, "-upstream-model", "model with spaces")
	companionRequireArgument(t, presentArguments, "-endpoint", "ws://"+serverAddress+"/v1/realtime")
	companionRequireArgument(t, presentArguments, "-webrtc-endpoint", "http://"+webRTCAddress+"/v1/realtime/calls")
	companionRequireArgument(t, presentArguments, "-management-endpoint", "http://"+serverAddress+management.APIPrefix)
	if !companionHasArgument(presentArguments, "-native-websocket-relay") {
		t.Fatalf("presentation argv omitted explicit native relay: %#v", presentArguments)
	}
}

func TestCompanionClientLaunchFailureCleansChildrenListenersAndNativeFile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		client     companionClient
		goos       string
		wantPrefix string
	}{
		{name: "browser", client: companionClientBrowser, goos: "linux", wantPrefix: "launch browser companion"},
		{name: "macos", client: companionClientMacOS, goos: "darwin", wantPrefix: "launch macOS companion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			serverAddress := companionFreeAddress(t)
			webRTCAddress := companionFreeAddress(t)
			presentationAddress := companionFreeAddress(t)
			logPath := filepath.Join(t.TempDir(), "children.jsonl")
			application := filepath.Join(t.TempDir(), "OpenRealtime Developer.app")
			if err := os.Mkdir(application, 0o700); err != nil {
				t.Fatal(err)
			}
			launchErr := errors.New("fixture client launch refused")
			var browserLaunches, macOSLaunches atomic.Int64
			var ready companionReady
			runtime := companionRuntime{
				executable: executable,
				prefix:     []string{"-test.run=^TestCompanionHelperProcess$", "--"},
				environment: append(os.Environ(),
					"OPENREALTIME_COMPANION_HELPER=1",
					"OPENREALTIME_COMPANION_HELPER_LOG="+logPath,
				),
				goos:        test.goos,
				httpClient:  &http.Client{Timeout: time.Second},
				childOutput: io.Discard,
				launchBrowser: func(context.Context, string) error {
					browserLaunches.Add(1)
					return launchErr
				},
				launchMacOS: func(context.Context, string, string) error {
					macOSLaunches.Add(1)
					return launchErr
				},
				onReady: func(observed companionReady) { ready = observed },
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			arguments := []string{
				"-server-listen", serverAddress,
				"-webrtc-listen", webRTCAddress,
				"-presentation-listen", presentationAddress,
				"-client", string(test.client),
				"-macos-app", application,
				"-ready-timeout", "10s",
				"-shutdown-timeout", "2s",
			}
			err := runCompanionContext(ctx, arguments, io.Discard, runtime)
			if !errors.Is(err, launchErr) || !strings.Contains(err.Error(), test.wantPrefix) {
				t.Fatalf("client launch error = %v, want wrapped %q", err, test.wantPrefix)
			}
			wantBrowser, wantMacOS := int64(0), int64(0)
			if test.client == companionClientBrowser {
				wantBrowser = 1
			} else {
				wantMacOS = 1
			}
			if browserLaunches.Load() != wantBrowser || macOSLaunches.Load() != wantMacOS {
				t.Fatalf("client launches browser=%d macos=%d, want %d/%d",
					browserLaunches.Load(), macOSLaunches.Load(), wantBrowser, wantMacOS)
			}
			if ready.NativeEndpointFile == "" {
				t.Fatalf("post-readiness failure did not expose generated endpoint identity: %#v", ready)
			}
			if _, err := os.Stat(ready.NativeEndpointFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("native endpoint file remains after launch failure: %v", err)
			}
			for _, address := range []string{serverAddress, webRTCAddress, presentationAddress} {
				listener, err := net.Listen("tcp", address)
				if err != nil {
					t.Fatalf("listener %s remains after launch failure: %v", address, err)
				}
				_ = listener.Close()
			}
			records := companionReadHelperRecords(t, logPath)
			if len(records) != 4 || records[0].Event != "serve-start" ||
				records[1].Event != "present-start" || records[2].Event != "present-stop" ||
				records[3].Event != "serve-stop" {
				t.Fatalf("post-launch-failure lifecycle = %#v", records)
			}
		})
	}
}

func companionReadHelperRecords(t *testing.T, path string) []companionHelperRecord {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []companionHelperRecord
	for _, line := range bytes.Split(bytes.TrimSpace(payload), []byte{'\n'}) {
		var record companionHelperRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestCompanionParserRejectsOwnedServeFlagsAndUnsafeSelections(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "owned listen", arguments: []string{"--", "-listen", "127.0.0.1:9999"}, want: "owned by companion"},
		{name: "owned equals", arguments: []string{"--", "--webrtc-listen=127.0.0.1:9999"}, want: "owned by companion"},
		{name: "missing delimiter", arguments: []string{"value"}, want: "literal --"},
		{name: "unsafe server", arguments: []string{"-server-listen", "0.0.0.0:8765"}, want: "explicit loopback IP"},
		{name: "hostname alias", arguments: []string{"-server-listen", "localhost:8765"}, want: "explicit loopback IP"},
		{name: "zero port", arguments: []string{"-server-listen", "127.0.0.1:0"}, want: "canonical integer"},
		{name: "noncanonical port", arguments: []string{"-server-listen", "127.0.0.1:08765"}, want: "canonical integer"},
		{name: "shared port", arguments: []string{"-server-listen", "127.0.0.1:8767"}, want: "must be distinct"},
		{name: "unknown client", arguments: []string{"-client", "desktop"}, want: "unknown companion client"},
		{name: "token value not environment", arguments: []string{"-token-env", "literal-secret=value"}, want: "environment name is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseCompanionOptions(test.arguments, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompanionParserAcceptsExactClientPoliciesAndDocumentsHelp(t *testing.T) {
	defaultOptions, err := parseCompanionOptions(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if defaultOptions.client != companionClientBrowser {
		t.Fatalf("default companion client = %q", defaultOptions.client)
	}
	for _, policy := range []companionClient{
		companionClientBrowser, companionClientMacOS, companionClientBoth, companionClientNone,
	} {
		t.Run(string(policy), func(t *testing.T) {
			options, err := parseCompanionOptions([]string{"-client", string(policy)}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if options.client != policy {
				t.Fatalf("parsed companion client = %q, want %q", options.client, policy)
			}
		})
	}
	var help bytes.Buffer
	if _, err := parseCompanionOptions([]string{"-help"}, &help); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("companion help error = %v", err)
	}
	for _, required := range []string{
		"client launch policy: browser, macos, both, or none",
		"-macos-app", "-server-listen", "-webrtc-listen", "-presentation-listen",
	} {
		if !strings.Contains(help.String(), required) {
			t.Errorf("companion help omitted %q:\n%s", required, help.String())
		}
	}
}

func TestPublicCompanionHelpReturnsSuccessAndDocumentsPolicies(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if err := run([]string{"companion", "-help"}, &output, &diagnostics); err != nil {
		t.Fatalf("public companion help: %v", err)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("public companion help diagnostics = %q", diagnostics.String())
	}
	for _, required := range []string{
		"client launch policy: browser, macos, both, or none",
		"environment variable holding the server credential",
		"standalone browser/native presentation host",
	} {
		if !strings.Contains(output.String(), required) {
			t.Errorf("public companion help omitted %q:\n%s", required, output.String())
		}
	}
}

func TestCompanionMacOSApplicationPreflightIsSideEffectFreeAndExact(t *testing.T) {
	root := t.TempDir()
	application := filepath.Join(root, "OpenRealtime Developer.app")
	if err := os.Mkdir(application, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preflightCompanionMacOSApplication(application); err != nil {
		t.Fatal(err)
	}
	plainDirectory := filepath.Join(root, "plain")
	if err := os.Mkdir(plainDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	plainFile := filepath.Join(root, "plain.app")
	if err := os.WriteFile(plainFile, []byte("not an app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "linked.app")
	if err := os.Symlink(application, symlink); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"missing":         filepath.Join(root, "missing.app"),
		"plain directory": plainDirectory,
		"plain file":      plainFile,
		"symlink":         symlink,
	} {
		t.Run(name, func(t *testing.T) {
			if err := preflightCompanionMacOSApplication(path); err == nil {
				t.Fatal("invalid macOS application passed preflight")
			}
		})
	}
}

func TestCompanionMacOSLaunchArgumentsAreExactAndNeverShellSplit(t *testing.T) {
	application := "/Applications/OpenRealtime Developer.app"
	endpointFile := "/private/tmp/openrealtime companion/native endpoints.json"
	want := []string{
		"-n", application,
		"--env", "OPENREALTIME_NATIVE_PROFILE=observer-developer",
		"--env", "OPENREALTIME_NATIVE_ENDPOINT_DIRECTORY=" + endpointFile,
	}
	if got := companionMacOSLaunchArguments(application, endpointFile); !slices.Equal(got, want) {
		t.Fatalf("macOS companion launch arguments = %#v, want %#v", got, want)
	}
	want = want[:4]
	if got := companionMacOSLaunchArguments(application, ""); !slices.Equal(got, want) {
		t.Fatalf("bundled-directory macOS launch arguments = %#v, want %#v", got, want)
	}
}

func TestCompanionRejectsNonWebRTCManifestBeforeReady(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := companionRuntime{
		executable: executable,
		prefix:     []string{"-test.run=^TestCompanionHelperProcess$", "--"},
		environment: append(os.Environ(),
			"OPENREALTIME_COMPANION_HELPER=1",
			"OPENREALTIME_COMPANION_HELPER_MANIFEST=websocket",
		),
		goos:        "linux",
		httpClient:  &http.Client{Timeout: time.Second},
		childOutput: io.Discard,
	}
	var output bytes.Buffer
	err = runCompanionContext(ctx, []string{
		"-server-listen", companionFreeAddress(t),
		"-webrtc-listen", companionFreeAddress(t),
		"-presentation-listen", companionFreeAddress(t),
		"-client", "none", "-ready-timeout", "10s", "-shutdown-timeout", "2s",
	}, &output, runtime)
	if err == nil || !strings.Contains(err.Error(), "served manifest does not match browser-developer-webrtc") {
		t.Fatalf("manifest identity error = %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("immutable manifest mismatch waited for the readiness deadline: %v", err)
	}
	if strings.Contains(output.String(), "companion ready") {
		t.Fatal("companion announced readiness for the wrong browser profile")
	}
}

func TestCompanionReadinessStopsOnPermanentValidationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"wrong"}`))
	}))
	defer server.Close()
	process := &companionProcess{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitCompanionHTTPReady(
		ctx, server.Client(), server.URL, "", process,
		func(int, http.Header, []byte) error {
			return permanentCompanionReadiness(errors.New("immutable identity mismatch"))
		},
	)
	if !errors.Is(err, errCompanionPermanentReadiness) ||
		!strings.Contains(err.Error(), "immutable identity mismatch") {
		t.Fatalf("permanent readiness error = %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("permanent readiness failure waited for deadline: %v", err)
	}
}

func TestCompanionReadinessRetriesTransientValidationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"warming"}`))
	}))
	defer server.Close()
	process := &companionProcess{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var validations atomic.Int32
	err := waitCompanionHTTPReady(
		ctx, server.Client(), server.URL, "", process,
		func(int, http.Header, []byte) error {
			if validations.Add(1) == 1 {
				return errors.New("still warming")
			}
			return nil
		},
	)
	if err != nil || validations.Load() != 2 {
		t.Fatalf("transient readiness result = %v after %d validations", err, validations.Load())
	}
}

func TestCompanionReadinessRejectsIdentitySubstitutionAndDuplicateJSON(t *testing.T) {
	header := http.Header{"Content-Type": []string{"application/json"}}
	server := func(model, binding, profile string) []byte {
		return []byte(fmt.Sprintf(`{
			"status":"ok","model":%q,"binding":%q,
			"protocol":{"openai_realtime":"pinned","openrealtime":{"version":1}},
			"server_profile":{"format_version":1,"realm":"server","state":"active","fingerprint":%q}
		}`, model, binding, profile))
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	if err := validateCompanionServerHealth(http.StatusOK, header, server("model", "cascade", digest), "model"); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"model":           server("other", "cascade", digest),
		"binding":         server("model", "", digest),
		"profile digest":  server("model", "cascade", "sha256:no"),
		"missing profile": []byte(`{"status":"ok","model":"model","binding":"cascade","protocol":{"openai_realtime":"pinned","openrealtime":{"version":1}}}`),
		"duplicate":       []byte(`{"status":"ok","status":"warming"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCompanionServerHealth(http.StatusOK, header, body, "model"); err == nil {
				t.Fatal("substituted server readiness was accepted")
			}
		})
	}
	webrtc := []byte(`{"status":"ok","transport":"webrtc","codec":"audio/PCMU","endpoint":"ws://127.0.0.1:8765/v1/realtime"}`)
	if err := validateCompanionWebRTCHealth(
		http.StatusOK, header, webrtc, "ws://127.0.0.1:8765/v1/realtime",
	); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{"websocket", "audio/opus", "ws://127.0.0.1:9999/v1/realtime"} {
		body := bytes.Replace(webrtc, []byte("webrtc"), []byte(replacement), 1)
		if replacement == "audio/opus" {
			body = bytes.Replace(webrtc, []byte("audio/PCMU"), []byte(replacement), 1)
		}
		if strings.HasPrefix(replacement, "ws://") {
			body = bytes.Replace(webrtc, []byte("ws://127.0.0.1:8765/v1/realtime"), []byte(replacement), 1)
		}
		if err := validateCompanionWebRTCHealth(
			http.StatusOK, header, body, "ws://127.0.0.1:8765/v1/realtime",
		); err == nil {
			t.Fatalf("WebRTC substitution %q was accepted", replacement)
		}
	}
}

func TestCompanionShutdownKillsSidecarAfterPresentationLeaderExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group contract is Unix-specific")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	grandchildAddress := companionFreeAddress(t)
	marker := filepath.Join(t.TempDir(), "grandchild-ready")
	readyChannel := make(chan companionReady, 1)
	runtime := companionRuntime{
		executable: executable,
		prefix:     []string{"-test.run=^TestCompanionHelperProcess$", "--"},
		environment: append(os.Environ(),
			"OPENREALTIME_COMPANION_HELPER=1",
			"OPENREALTIME_COMPANION_HELPER_STUBBORN=1",
			"OPENREALTIME_COMPANION_GRANDCHILD_ADDRESS="+grandchildAddress,
			"OPENREALTIME_COMPANION_GRANDCHILD_MARKER="+marker,
		),
		goos:        runtime.GOOS,
		httpClient:  &http.Client{Timeout: time.Second},
		childOutput: io.Discard,
		onReady:     func(ready companionReady) { readyChannel <- ready },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	serverAddress := companionFreeAddress(t)
	webRTCAddress := companionFreeAddress(t)
	presentationAddress := companionFreeAddress(t)
	go func() {
		done <- runCompanionContext(ctx, []string{
			"-server-listen", serverAddress,
			"-webrtc-listen", webRTCAddress,
			"-presentation-listen", presentationAddress,
			"-client", "none", "-ready-timeout", "10s", "-shutdown-timeout", "2s",
		}, io.Discard, runtime)
	}()
	select {
	case <-readyChannel:
	case err := <-done:
		t.Fatalf("stubborn companion stopped before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("stubborn companion did not become ready")
	}
	markerDeadline := time.Now().Add(6 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read grandchild readiness: %v", err)
		}
		if time.Now().After(markerDeadline) {
			t.Fatal("grandchild did not publish readiness")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("process-group shutdown error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("forced companion shutdown did not finish")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		listener, err := net.Listen("tcp", grandchildAddress)
		if err == nil {
			_ = listener.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("presentation grandchild still owns %s after group kill: %v", grandchildAddress, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestCompanionHelperProcess(t *testing.T) {
	if os.Getenv("OPENREALTIME_COMPANION_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		t.Fatal("helper command is absent")
	}
	arguments := append([]string(nil), os.Args[separator+1:]...)
	command := arguments[0]
	arguments = arguments[1:]
	record := func(event string) {
		path := os.Getenv("OPENREALTIME_COMPANION_HELPER_LOG")
		if path == "" {
			return
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(file).Encode(companionHelperRecord{Event: event, Arguments: arguments}); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "serve":
		record("serve-start")
		fmt.Println("helper serve stdout")
		fmt.Fprintln(os.Stderr, "helper serve stderr")
		serverAddress := companionArgumentValue(t, arguments, "-listen")
		model := companionArgumentValue(t, arguments, "-model")
		server := companionHelperServerHealth(t, serverAddress, model)
		webRTC := companionHelperWebRTCHealth(
			t, companionArgumentValue(t, arguments, "-webrtc-listen"),
			"ws://"+serverAddress+"/v1/realtime",
		)
		<-ctx.Done()
		companionHelperShutdown(t, webRTC)
		companionHelperShutdown(t, server)
		record("serve-stop")
	case "present":
		record("present-start")
		fmt.Println("helper present stdout")
		fmt.Fprintln(os.Stderr, "helper present stderr")
		var bundle *presentationbrowser.Bundle
		var err error
		if os.Getenv("OPENREALTIME_COMPANION_HELPER_MANIFEST") == "websocket" {
			bundle, err = presentationbrowser.ObserverDeveloperBundle()
		} else {
			bundle, err = presentationbrowser.ObserverDeveloperWebRTCBundle()
		}
		if err != nil {
			t.Fatal(err)
		}
		payload, err := presentation.MarshalManifest(bundle.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /client/v1/manifest", func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write(payload)
		})
		server := companionHelperServer(t, companionArgumentValue(t, arguments, "-listen"), mux)
		if os.Getenv("OPENREALTIME_COMPANION_HELPER_STUBBORN") == "1" {
			command := exec.Command(
				os.Args[0], "-test.run=^TestCompanionGrandchildProcess$", "--",
				os.Getenv("OPENREALTIME_COMPANION_GRANDCHILD_ADDRESS"),
				os.Getenv("OPENREALTIME_COMPANION_GRANDCHILD_MARKER"),
			)
			command.Env = append(os.Environ(), "OPENREALTIME_COMPANION_GRANDCHILD=1")
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(os.Getenv("OPENREALTIME_COMPANION_GRANDCHILD_MARKER")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("grandchild did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		<-ctx.Done()
		companionHelperShutdown(t, server)
		record("present-stop")
	default:
		t.Fatalf("unknown helper command %q", command)
	}
}

func TestCompanionGrandchildProcess(t *testing.T) {
	if os.Getenv("OPENREALTIME_COMPANION_GRANDCHILD") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+2 >= len(os.Args) {
		t.Fatal("grandchild arguments are absent")
	}
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
	listener, err := net.Listen("tcp", os.Args[separator+1])
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(os.Args[separator+2], []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

type companionHelperRecord struct {
	Event     string   `json:"event"`
	Arguments []string `json:"arguments"`
}

func companionHelperServerHealth(t *testing.T, address, model string) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"status": "ok", "model": model, "binding": "companion-helper",
			"protocol": map[string]any{
				"openai_realtime": "pinned",
				"openrealtime":    map[string]any{"version": 1},
			},
			"server_profile": map[string]any{
				"format_version": 1, "realm": "server", "state": "active",
				"fingerprint": "sha256:" + strings.Repeat("a", 64),
			},
		})
	})
	return companionHelperServer(t, address, mux)
}

func companionHelperWebRTCHealth(t *testing.T, address, upstream string) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"status": "ok", "transport": "webrtc", "codec": "audio/PCMU", "endpoint": upstream,
		})
	})
	return companionHelperServer(t, address, mux)
}

func companionHelperServer(t *testing.T, address string, handler http.Handler) *http.Server {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "helper server:", err)
		}
	}()
	return server
}

func companionHelperShutdown(t *testing.T, server *http.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func companionFreeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func companionArgumentValue(t *testing.T, arguments []string, name string) string {
	t.Helper()
	for index := range arguments {
		if arguments[index] == name && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	t.Fatalf("argument %s is absent from %#v", name, arguments)
	return ""
}

func companionRequireArgument(t *testing.T, arguments []string, name, value string) {
	t.Helper()
	if got := companionArgumentValue(t, arguments, name); got != value {
		t.Fatalf("argument %s = %q, want %q in %#v", name, got, value, arguments)
	}
}

func companionHasArgument(arguments []string, want string) bool {
	for _, argument := range arguments {
		if argument == want {
			return true
		}
	}
	return false
}

type companionLockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *companionLockedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *companionLockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
