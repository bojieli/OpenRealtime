package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/internal/testgate"
	"github.com/bojieli/OpenRealtime/macos"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
	"github.com/coder/websocket"
)

// TestPublicCompanionCommandRunsRealBrowserAndNativeClients closes the command
// seam left by the focused supervisor and host-composition tests. It builds the
// public executable, lets `companion` supervise real `serve` and `present`
// child processes, drives real Chromium over WebRTC, and then drives the exact
// shipped native observer endpoint directory over WebSocket. Both sessions
// must appear in one unchanged clean server before shutdown releases every
// listener and generated native configuration file.
func TestPublicCompanionCommandRunsRealBrowserAndNativeClients(t *testing.T) {
	if !testgate.Release() {
		t.Skip("set OPENREALTIME_RELEASE_GATE=1 for the real public companion command gate")
	}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "openrealtime")
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	build := exec.Command(goBinary, "build", "-trimpath", "-o", binary, "./cmd/openrealtime")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public companion executable: %v\n%s", err, output)
	}

	serverAddress := companionFreeAddress(t)
	webRTCAddress := companionFreeAddress(t)
	presentationAddress := companionFreeAddress(t)
	model := "companion-public-command-e2e"
	secret := "companion-public-command-secret-must-stay-inside-host"
	output := &companionLockedBuffer{}
	process, err := startCompanionProcess(companionRuntime{
		executable: binary,
		environment: append(os.Environ(),
			"OPENREALTIME_COMPANION_E2E_TOKEN="+secret,
		),
	}, []string{
		"companion",
		"-server-listen", serverAddress,
		"-webrtc-listen", webRTCAddress,
		"-presentation-listen", presentationAddress,
		"-model", model,
		"-token-env", "OPENREALTIME_COMPANION_E2E_TOKEN",
		"-client", "none",
		"-ready-timeout", "90s",
		"-shutdown-timeout", "5s",
	}, output)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = process.stop(15 * time.Second)
		}
	})

	serverURL := "http://" + serverAddress
	webRTCURL := "http://" + webRTCAddress
	presentationURL := "http://" + presentationAddress
	// This companion runs with a bearer token, so the health inventory the
	// validators below read is served only to a caller that presents it.
	credential := secret
	readyContext, cancelReady := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelReady()
	client := &http.Client{Timeout: 2 * time.Second}
	if err := waitCompanionHTTPReady(
		readyContext, client, serverURL+"/healthz", credential, process,
		func(status int, header http.Header, body []byte) error {
			return validateCompanionServerHealth(status, header, body, model)
		},
	); err != nil {
		t.Fatalf("public companion server readiness: %v\n%s", err, output.String())
	}
	if err := waitCompanionHTTPReady(
		readyContext, client, webRTCURL+"/healthz", credential, process,
		func(status int, header http.Header, body []byte) error {
			return validateCompanionWebRTCHealth(
				status, header, body, "ws://"+serverAddress+"/v1/realtime",
			)
		},
	); err != nil {
		t.Fatalf("public companion WebRTC readiness: %v\n%s", err, output.String())
	}
	browserBundle, err := presentationbrowser.ObserverDeveloperWebRTCBundle()
	if err != nil {
		t.Fatal(err)
	}
	if err := waitCompanionHTTPReady(
		readyContext, client, presentationURL+"/client/v1/manifest", "", process,
		func(status int, header http.Header, body []byte) error {
			if status != http.StatusOK {
				return fmt.Errorf("manifest status is %d", status)
			}
			if err := validateCompanionJSONMediaType(header, "manifest"); err != nil {
				return err
			}
			manifest, err := presentation.ParseManifest(body)
			if err != nil {
				return err
			}
			if manifest.Fingerprint != browserBundle.Manifest.Fingerprint ||
				manifest.Plan.Fingerprint != browserBundle.Manifest.Plan.Fingerprint {
				return errors.New("public command served the wrong browser profile")
			}
			return nil
		},
	); err != nil {
		t.Fatalf("public companion presentation readiness: %v\n%s", err, output.String())
	}
	if !waitCompanionOutput(output, "OpenRealtime companion ready", 5*time.Second) {
		t.Fatalf("public companion omitted its ready record:\n%s", output.String())
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("public companion exposed its bearer credential")
	}

	for _, path := range []string{"/", "/client/v1/manifest"} {
		if status := companionCommandHTTPStatus(t, client, serverURL+path); status != http.StatusNotFound {
			t.Fatalf("clean server route %s status = %d, want 404", path, status)
		}
	}
	if status := companionCommandHTTPStatus(t, client, presentationURL); status != http.StatusOK {
		t.Fatalf("presentation root status = %d, want 200", status)
	}

	nativeFile := companionCommandOutputValue(t, output.String(), "native file")
	payload, err := os.ReadFile(nativeFile)
	if err != nil {
		t.Fatalf("read generated native endpoint directory: %v", err)
	}
	var nativeDirectory presentation.EndpointDirectory
	if err := json.Unmarshal(payload, &nativeDirectory); err != nil {
		t.Fatal(err)
	}
	if err := nativeDirectory.Validate(); err != nil {
		t.Fatal(err)
	}
	nativeBundle, err := macos.NewNativeBundleForDistributionWithEndpointDirectory(
		macos.NativeObserverDeveloperDistribution, nativeDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}

	node := companionCommandExecutable(t, "node")
	chromium := companionCommandBrowser(t)
	_, cdpPort, err := net.SplitHostPort(companionFreeAddress(t))
	if err != nil {
		t.Fatal(err)
	}
	browserContext, cancelBrowser := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelBrowser()
	browser := exec.CommandContext(
		browserContext, node,
		filepath.Join(repositoryRoot, "cmd", "openrealtime", "testdata", "companion_browser.mjs"),
		presentationURL,
	)
	browserSnapshot := filepath.Join(t.TempDir(), "browser-live.json")
	browser.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+cdpPort,
		"OPENREALTIME_COMPANION_BROWSER_SNAPSHOT="+browserSnapshot)
	browserOutput, err := browser.CombinedOutput()
	t.Logf("public companion browser:\n%s", browserOutput)
	if err != nil {
		t.Fatalf("drive public companion browser WebRTC client: %v", err)
	}
	if info, err := os.Lstat(browserSnapshot); err != nil || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 || info.Size() == 0 {
		t.Fatalf("browser did not publish one private exact management response: info=%v err=%v", info, err)
	}

	companionCommandRunNativeProbe(t, nativeBundle)
	waitCompanionCommandSessions(t, client, serverURL+"/metrics", 2, 2)
	if strings.Contains(output.String(), secret) {
		t.Fatal("public companion child logs exposed its bearer credential")
	}

	if err := process.stop(15 * time.Second); err != nil {
		t.Fatalf("stop public companion command: %v\n%s", err, output.String())
	}
	stopped = true
	t.Logf("public companion lifecycle:\n%s", output.String())
	if _, err := os.Stat(nativeFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated native endpoint file remains after shutdown: %v", err)
	}
	for _, address := range []string{serverAddress, webRTCAddress, presentationAddress} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("companion listener %s survived shutdown: %v", address, err)
		}
		_ = listener.Close()
	}
}

func companionCommandRunNativeProbe(t *testing.T, bundle *macos.NativeBundle) {
	t.Helper()
	endpoint, err := bundle.EndpointDirectory.Require(
		presentation.EndpointRealtimeWebSocket, presentation.ProtocolRealtimeWebSocket,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, endpoint.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	created := false
	for {
		_, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "session.created":
			created = true
			update, err := json.Marshal(map[string]any{
				"type": "session.update",
				"session": map[string]any{
					"type": "realtime", "modalities": []string{"text"}, "tools": []any{},
					"openrealtime": map[string]any{
						"version": 1, "supports": []any{}, "observers": []any{},
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := connection.Write(ctx, websocket.MessageText, update); err != nil {
				t.Fatal(err)
			}
		case "session.updated":
			if !created {
				t.Fatal("native probe received session.updated before session.created")
			}
			if err := connection.Close(websocket.StatusNormalClosure, "native profile probe complete"); err != nil &&
				websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatal(err)
			}
			return
		case "error":
			t.Fatalf("native profile probe received protocol error: %s", payload)
		}
	}
}

func waitCompanionCommandSessions(
	t *testing.T, client *http.Client, endpoint string, started, completed int64,
) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		request, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			var metrics struct {
				SessionsStarted   int64 `json:"sessions_started"`
				SessionsCompleted int64 `json:"sessions_completed"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&metrics)
			closeErr := response.Body.Close()
			if response.StatusCode == http.StatusOK && decodeErr == nil && closeErr == nil &&
				metrics.SessionsStarted == started && metrics.SessionsCompleted == completed {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("one server did not complete exactly %d browser/native sessions", completed)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func companionCommandHTTPStatus(t *testing.T, client *http.Client, endpoint string) int {
	t.Helper()
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read %s: %v", endpoint, errors.Join(readErr, closeErr))
	}
	return response.StatusCode
}

func companionCommandOutputValue(t *testing.T, output, label string) string {
	t.Helper()
	prefix := "  " + label
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if value == "" || !filepath.IsAbs(value) {
			t.Fatalf("companion %s is not an absolute path: %q", label, value)
		}
		return value
	}
	t.Fatalf("companion output omitted %s:\n%s", label, output)
	return ""
}

func waitCompanionOutput(output *companionLockedBuffer, wanted string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), wanted) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return strings.Contains(output.String(), wanted)
}

func companionCommandExecutable(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("release companion gate requires %s: %v", name, err)
	}
	return path
}

func companionCommandBrowser(t *testing.T) string {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("CHROMIUM")); configured != "" {
		if filepath.IsAbs(configured) {
			if info, err := os.Stat(configured); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return configured
			}
		}
		t.Fatalf("CHROMIUM is not an absolute executable: %q", configured)
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Fatal("release companion gate requires Chromium")
	return ""
}
