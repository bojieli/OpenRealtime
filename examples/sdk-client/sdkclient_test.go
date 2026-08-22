package sdkclient

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/internal/testserver"
)

// The project claims an official OpenAI Realtime client connects unchanged.
// This is the thing that checks it: the published @openai/agents-realtime
// package, configured the way its own documentation says to, with one URL
// pointed at a local server.
//
// It runs a tool-using turn on both transports, because tool use is where an
// implementation is most likely to diverge and because the claim is specific
// about it.
//
// The dependencies are npm's, so the test skips rather than fails when they
// are absent - the gate has to run offline. `npm install` in this directory is
// what makes it run.

func requireClient(t *testing.T) (node string, chromium string) {
	t.Helper()
	found, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the official SDK test")
	}
	node = found
	if _, err := os.Stat("node_modules/@openai/agents-realtime"); err != nil {
		t.Skip("the official SDK is not installed; run `npm install` in examples/sdk-client to include this test")
	}
	chromium = os.Getenv("CHROMIUM")
	if chromium == "" {
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if path, err := exec.LookPath(candidate); err == nil {
				chromium = path
				break
			}
		}
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

func run(t *testing.T, node, script string, arguments []string, environment []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, node, append([]string{script}, arguments...)...)
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		t.Fatalf("the official SDK run failed: %v", err)
	}
}

func TestTheOfficialSDKCompletesAToolUsingSessionOverWebSocket(t *testing.T) {
	node, _ := requireClient(t)
	stack := testserver.Start(t, testserver.Config{
		Transcript:    "what is the weather in Cambridge",
		ToolName:      "get_weather",
		ToolArguments: `{"city":"Cambridge"}`,
	})
	run(t, node, "websocket.mjs", []string{stack.ProtocolURL}, nil)
}

func TestTheOfficialSDKCompletesAToolUsingSessionOverWebRTC(t *testing.T) {
	node, chromium := requireClient(t)
	if chromium == "" {
		t.Skip("chromium is not installed; the SDK's WebRTC transport only runs in a browser")
	}
	if err := buildBundle(t); err != nil {
		t.Skipf("could not build the browser bundle: %v", err)
	}

	// The page is served from its own origin, which is what every real
	// deployment looks like: the adapter is a port on a server and the
	// application is a site. A same-origin test would pass while proving
	// nothing about whether a browser can reach the adapter at all.
	stack := testserver.Start(t, testserver.Config{
		Transcript:     "what is the weather in Cambridge",
		ToolName:       "get_weather",
		ToolArguments:  `{"city":"Cambridge"}`,
		AllowedOrigins: []string{"*"},
	})
	run(t, node, "browser.mjs", []string{stack.AdapterURL},
		[]string{"CHROMIUM=" + chromium, "CDP_PORT=" + freePort(t)})
}

// buildBundle produces the browser build of the SDK client if it is missing.
//
// It is built rather than committed: two and a half megabytes of generated
// JavaScript in the tree would be a thing to keep in step with a dependency by
// hand, and the point of this example is that the client is the published
// package rather than a copy of it.
func buildBundle(t *testing.T) error {
	t.Helper()
	bundle := filepath.Join("dist", "webrtc.js")
	source, err := os.Stat("webrtc.mjs")
	if err != nil {
		return err
	}
	if built, err := os.Stat(bundle); err == nil && built.ModTime().After(source.ModTime()) {
		return nil
	}
	esbuild := filepath.Join("node_modules", "esbuild", "bin", "esbuild")
	if _, err := os.Stat(esbuild); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Run the bundler directly rather than through node. npm installs a native
	// executable here on most platforms and a shell shim on the rest, and both
	// run themselves; handing an ELF binary to node fails with a syntax error
	// on its first byte.
	//
	// This skipped rather than failed, so the suite reported a compatibility
	// claim it had not checked - and it passed everywhere a previous bundle
	// happened to be lying around, which is every tree anyone had already run
	// it in. OPENREALTIME_RELEASE_GATE is what caught it, by refusing to treat
	// a skip as a pass.
	command := exec.CommandContext(ctx, esbuild, "webrtc.mjs",
		"--bundle", "--format=esm", "--outfile="+bundle, "--log-level=warning")
	if output, err := command.CombinedOutput(); err != nil {
		t.Log(string(output))
		return err
	}
	return nil
}
