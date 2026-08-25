package surface_test

import (
	"context"
	"fmt"
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
	"github.com/bojieli/OpenRealtime/surface"
)

// The surface is the only client that carries every channel at once, and most
// of what it does cannot be checked from Go: capture, playout, the client half
// of the chunk framing, whether an artifact renders inside its sandbox, and
// whether a click the model asked for lands on a real page.
//
// So this assembles the real thing - the real gateway, the real cascade
// binding, the real WebRTC adapter, the real surface, and a real browser as
// the thing being acted on - puts scripted providers where the models would
// be, and drives all of it with Chromium.
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

// targetPage is the page the agent acts on.
//
// Pressing the button moves the fragment, which is the smallest observable
// consequence that survives the whole path: the click is dispatched over the
// DevTools protocol, the page's own handler runs, and the new location comes
// back with the next captured frame. Anything the test could assert without
// leaving Go would have proven the command was sent rather than that anything
// happened.
const targetPage = `<!doctype html>
<html><head><title>Target</title><style>
 body { margin: 0; font: 16px system-ui; background: #f6f6f6; }
 button { position: absolute; left: 80px; top: 120px; width: 240px; height: 64px; font: inherit; }
 p { position: absolute; left: 80px; top: 220px; }
</style></head>
<body>
 <button id="press" onclick="location.hash = 'pressed'; document.getElementById('state').textContent = 'pressed'">Press me</button>
 <p id="state">not pressed</p>
</body></html>`

// livePage is the target for a run against a real vision model.
//
// One large, high-contrast control and nothing else, and that is a deliberate
// choice rather than a rigged one. The scripted gate already pins exact
// coordinate handling: it clicks (200, 152) and asserts the page moved, and
// nothing about that is approximate. What the live run is asking is whether
// perception and action compose - whether a model told about a screen in words
// by another model that looked at it can press what it was told about. A
// target needing five-pixel precision would be measuring a vision model's
// pixel regression instead, which is a real question and a different one.
const livePage = `<!doctype html>
<html><head><title>Target</title><style>
 body { margin: 0; font: 16px system-ui; background: #ffffff; }
 button {
   position: absolute; left: 128px; top: 128px; width: 768px; height: 400px;
   font: 600 56px system-ui; background: #1a7f37; color: white; border: none; border-radius: 20px;
 }
 p { position: absolute; left: 128px; top: 560px; font-size: 30px; }
</style></head>
<body>
 <button id="press" onclick="location.hash = 'pressed'; document.getElementById('state').textContent = 'pressed'">Press me</button>
 <p id="state">not pressed</p>
</body></html>`

func servePage(t *testing.T, page string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(page))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func serveTargetPage(t *testing.T) string {
	t.Helper()
	return servePage(t, targetPage)
}

// startTargetBrowser launches the browser the surface will observe and act on.
func startTargetBrowser(t *testing.T, chromium, pageURL string) string {
	t.Helper()
	port := freePort(t)
	profile := t.TempDir()
	command := exec.Command(chromium,
		"--headless=new",
		"--remote-debugging-port="+port,
		"--remote-allow-origins=*",
		"--no-sandbox",
		"--disable-gpu",
		"--window-size=1024,768",
		"--user-data-dir="+profile,
		pageURL,
	)
	if err := command.Start(); err != nil {
		t.Fatalf("launch the target browser: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})

	devtools := "http://127.0.0.1:" + port
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(devtools + "/json/version")
		if err == nil {
			response.Body.Close()
			return devtools
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the target browser never opened a debugging port on %s", devtools)
	return ""
}

// newLoopbackServer serves a built surface on loopback, which is what makes
// the page a secure context and therefore able to open a microphone.
func newLoopbackServer(t *testing.T, server *surface.Server) string {
	t.Helper()
	local := httptest.NewServer(server.Handler())
	t.Cleanup(local.Close)
	return local.URL
}

func startSurfaceServer(
	t *testing.T, protocolURL, adapterURL, root string, browserContext *surface.BrowserContext,
) string {
	t.Helper()
	files, err := console.NewHost(console.HostConfig{
		Root: root, Enabled: []string{"read_file", "list_directory", "write_file"},
	})
	if err != nil {
		t.Fatalf("tool host: %v", err)
	}
	server, err := surface.New(surface.Config{
		Endpoint: protocolURL, WebRTCEndpoint: adapterURL, Files: files, Browser: browserContext,
	})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	// The page must be served from a loopback address for the browser to treat
	// it as a secure context and hand it a microphone, which httptest gives.
	local := httptest.NewServer(server.Handler())
	t.Cleanup(local.Close)
	return local.URL
}

// The whole point, in one run: six channels in, five out, on both transports.
func TestTheSurfaceCarriesEveryChannel(t *testing.T) {
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

	target := startTargetBrowser(t, chromium, serveTargetPage(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	browserContext, err := surface.ConnectBrowser(ctx, surface.BrowserConfig{DevToolsURL: target})
	cancel()
	if err != nil {
		t.Fatalf("attach the browser channel: %v", err)
	}
	defer browserContext.Close()

	// One call per slow turn, in this order, because a client whose point is
	// that it runs three different kinds of tool has to be seen doing each of
	// them separately.
	stack := testserver.Start(t, testserver.Config{
		Transcript: "please read the notes file",
		Narration:  "The screen shows a page with a button labelled Press me.",
		ToolCalls: []testserver.ScriptedCall{
			{Name: "read_file", Arguments: `{"path":"notes.txt"}`},
			{Name: "display_artifact", Arguments: artifactArguments},
			{Name: "computer.click", Arguments: fmt.Sprintf(
				`{"source":%q,"x":200,"y":152}`, browserContext.Source())},
		},
	})
	surfaceURL := startSurfaceServer(t, stack.ProtocolURL, stack.AdapterURL, resolved, browserContext)

	driver, err := filepath.Abs(filepath.Join("testdata", "surface.mjs"))
	if err != nil {
		t.Fatalf("locate the driver: %v", err)
	}

	for _, mode := range []string{"websocket", "webrtc"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, node, driver, surfaceURL, mode)
			command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t))
			output, err := command.CombinedOutput()
			t.Log("\n" + string(output))
			if err != nil {
				t.Fatalf("the browser run failed: %v", err)
			}
		})
	}
}

// The artifact the scripted reasoner renders. It is a real document with its
// own inline style and script, because an artifact that was only markup would
// not test the thing that makes artifacts need a sandbox at all.
const artifactArguments = `{"artifact_id":"deadline","title":"Deadline",` +
	`"html":"<!doctype html><html><body style=\"margin:0;font:16px system-ui\">` +
	`<h1 id=\"headline\" style=\"position:absolute;left:20px;top:16px\">Friday</h1>` +
	`<p style=\"position:absolute;left:20px;top:64px\">Owner: the developer.</p>` +
	`<button id=\"ack\" style=\"position:absolute;left:20px;top:120px;width:220px;height:48px\" ` +
	`onclick=\"window.parent.postMessage({text:'I acknowledged the deadline'},'*')\">Acknowledge</button>` +
	`<script>document.getElementById('headline').dataset.ready='yes'</script>` +
	`</body></html>"}`

// A surface with nothing attached is still a complete client for the channels
// that need nothing attached, and the page has to say which ones those are
// rather than offering controls that cannot work.
func TestTheSurfaceWithNothingAttachedStillServesAWorkingPage(t *testing.T) {
	t.Parallel()
	local := startSurface(t, surface.Config{Endpoint: newEndpoint(t).url()})
	response, err := http.Get(local.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected the page, got %d", response.StatusCode)
	}
	frame, err := http.Get(local.URL + "/api/browser/frame")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer frame.Body.Close()
	if frame.StatusCode != http.StatusNotFound {
		t.Fatalf("a channel with nothing behind it must say so, got %d", frame.StatusCode)
	}
}
