package console_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/console"
	"github.com/bojieli/OpenRealtime/internal/testserver"
)

// The console is the only client that exercises the whole system at once, and
// most of what it does cannot be checked from Go: capture, playout, the client
// half of the chunk framing, and whether a real browser's WebRTC stack talks to
// the adapter at all.
//
// So this test assembles the real thing - the real gateway, the real cascade
// binding, the real WebRTC adapter, the real console - puts scripted providers
// where the models would be, and drives it with Chromium.
//
// It skips when Chromium or Node is missing rather than failing, because the
// gate has to run offline on a machine with neither.

func requireBrowser(t *testing.T) (string, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the browser test")
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
		t.Skip("chromium is not installed; skipping the browser test")
	}
	return node, chromium
}

func startConsoleServer(t *testing.T, protocolURL, adapterURL, root string) string {
	t.Helper()
	host, err := console.NewHost(console.HostConfig{
		Root: root, Enabled: []string{"read_file", "list_directory", "write_file"},
	})
	if err != nil {
		t.Fatalf("tool host: %v", err)
	}
	server, err := console.New(console.Config{
		Endpoint: protocolURL, WebRTCEndpoint: adapterURL, Host: host,
	})
	if err != nil {
		t.Fatalf("console: %v", err)
	}
	// The page must be served from a loopback address for the browser to treat
	// it as a secure context and hand it a microphone, which httptest gives.
	local := httptest.NewServer(server.Handler())
	t.Cleanup(local.Close)
	return local.URL
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

func TestTheConsoleDrivesARealSession(t *testing.T) {
	node, chromium := requireBrowser(t)

	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve the root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(resolved, "notes.txt"),
		[]byte("The deadline moved to Friday.\nOwner: the developer.\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stack := testserver.Start(t, testserver.Config{
		Transcript:    "please read the notes file",
		ToolName:      "read_file",
		ToolArguments: `{"path":"notes.txt"}`,
	})
	consoleURL := startConsoleServer(t, stack.ProtocolURL, stack.AdapterURL, resolved)

	driver, err := filepath.Abs(filepath.Join("testdata", "browser.mjs"))
	if err != nil {
		t.Fatalf("locate the driver: %v", err)
	}

	for _, mode := range []string{"websocket", "webrtc"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, node, driver, consoleURL, mode)
			command.Env = append(os.Environ(),
				"CHROMIUM="+chromium,
				"CDP_PORT="+freePort(t))
			output, err := command.CombinedOutput()
			t.Log("\n" + string(output))
			if err != nil {
				t.Fatalf("the browser run failed: %v", err)
			}
		})
	}
}

// A console started with no tool host still has to be a complete client for
// everything except tool use, and the page has to say so rather than offering
// tools that cannot run.
func TestTheConsoleWithoutToolsStillServesAWorkingPage(t *testing.T) {
	t.Parallel()
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url()})
	response, err := http.Get(local.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected the page, got %d", response.StatusCode)
	}
}
