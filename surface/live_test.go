package surface_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/console"
	"github.com/bojieli/OpenRealtime/surface"
)

// The live run: real models, a real browser, and nothing scripted.
//
// The committed gate next door proves the surface carries every channel. It
// cannot prove the harder thing, which is that a model actually chooses to use
// them - a scripted provider issues the call the test asked for, so a run
// against one says nothing about whether the tools are described well enough
// that a model reaches for them, or whether what it puts in them is usable.
// That question only has an answer against a real model, and the answer
// changes with the model, so this is a run you do rather than a gate you pass.
//
// It needs a server that is already up, because the interesting
// configuration - which provider, which model, which recogniser - is the thing
// being tested and does not belong hard-coded in a test file. Start one and
// point this at it:
//
//	openrealtime serve -listen 127.0.0.1:18765 \
//	  -fast-provider vllm -fast-url http://127.0.0.1:8000/v1 -fast-model qwen-fast \
//	  -slow-provider vllm -slow-url http://127.0.0.1:8000/v1 -slow-model qwen-fast \
//	  -asr-provider sensevoice \
//	  -tts-provider openai-compatible -tts-url http://127.0.0.1:8081/v1/audio/speech
//
//	OPENREALTIME_LIVE_ENDPOINT=ws://127.0.0.1:18765/v1/realtime \
//	  go test ./surface/ -run TestLive -v
//
// The endpoint's own provider flags decide which models answer. The slow
// provider is the one that has to be able to call tools: the fast one
// structurally cannot, which is the whole differentiator, so a deployment
// whose slow endpoint refuses tool calls will fail this run at the first
// assertion and should.
func TestLiveSurfaceAgainstRealModels(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("OPENREALTIME_LIVE_ENDPOINT"))
	if endpoint == "" {
		t.Skip("set OPENREALTIME_LIVE_ENDPOINT to a running server to run the live surface test")
	}
	node, chromium := requireBrowser(t)

	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve the root: %v", err)
	}
	// Something with an answer in it that the model cannot know without
	// reading the file. A run where the agent guessed and a run where it read
	// look the same unless the answer is arbitrary.
	if err := os.WriteFile(filepath.Join(resolved, "notes.txt"),
		[]byte("Project Queqiao\nDeadline: Friday 14 November\nOwner: the developer\nStatus: amber\n"),
		0o644); err != nil {
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

	files, err := console.NewHost(console.HostConfig{
		Root: resolved, Enabled: []string{"read_file", "list_directory"},
	})
	if err != nil {
		t.Fatalf("tool host: %v", err)
	}
	server, err := surface.New(surface.Config{
		Endpoint: endpoint, WebRTCEndpoint: os.Getenv("OPENREALTIME_LIVE_WEBRTC"),
		Token: os.Getenv("OPENREALTIME_TOKEN"), Files: files, Browser: browserContext,
	})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	local := newLoopbackServer(t, server)

	driver, err := filepath.Abs(filepath.Join("testdata", "live.mjs"))
	if err != nil {
		t.Fatalf("locate the driver: %v", err)
	}
	runCtx, cancelRun := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelRun()
	command := exec.CommandContext(runCtx, node, driver, local)
	command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freePort(t))
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		t.Fatalf("the live run failed: %v", err)
	}
	fmt.Fprintln(os.Stderr, "live surface run complete")
}
