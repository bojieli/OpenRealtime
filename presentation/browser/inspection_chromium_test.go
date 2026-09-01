package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestInspectionViewRendersExactChannelTelemetryInChromium(t *testing.T) {
	chromium := requireInspectionChromium(t)
	module, err := browserModule("inspection-view.js")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			response.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = response.Write([]byte(`<!doctype html><html><body><script type="module" src="/fixture.js"></script></body></html>`))
		case "/inspection-view.js":
			response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = response.Write(module)
		case "/fixture.js":
			response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = response.Write([]byte(inspectionChromiumFixture))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, chromium,
		"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--user-data-dir="+t.TempDir(), "--virtual-time-budget=2000", "--dump-dom", server.URL,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render inspection channel telemetry in Chromium: %v\n%s", err, output)
	}
	document := string(output)
	for _, expected := range []string{
		`data-ready="true"`, `data-edge-id="channel"`, `data-delivery="lossy"`,
		`data-depth="4"`, `data-occupancy="1"`, "Delivery: ", "lossy; depth 4",
		"Occupancy: ", "1/4", "Dropped: ", "2", "Backpressure: ", "1",
		"Queue wait: ", "300 ns cumulative; 50 ns per dequeue",
	} {
		if !strings.Contains(document, expected) {
			t.Fatalf("Chromium channel view omitted %q:\n%s", expected, document)
		}
	}
	if strings.Contains(document, "data-error=") {
		t.Fatalf("Chromium channel view reported an error:\n%s", document)
	}
}

func requireInspectionChromium(t testing.TB) string {
	t.Helper()
	chromium := os.Getenv("CHROMIUM")
	if chromium == "" {
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if found, err := exec.LookPath(candidate); err == nil {
				chromium = found
				break
			}
		}
	}
	if chromium != "" {
		return chromium
	}
	if os.Getenv("OPENREALTIME_RELEASE_GATE") != "" {
		t.Fatal("chromium is not installed; the release gate requires the real Chromium presentation tests")
	}
	t.Skip("chromium is not installed")
	return ""
}

const inspectionChromiumFixture = `
import plugin from "/inspection-view.js";
const fingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const elementDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const live = {
  format_version: 1, graph_id: "channel_operator", graph_revision: 1, fingerprint,
  state: "running",
  nodes: { worker: {
    state: "running", active_runs: 0, first_trigger_ns: 100, first_output_ns: 120,
    completion_ns: 180,
    resolution: { element: { name: "test.Worker", revision: 1, digest: elementDigest } },
  } },
  edges: { channel: {
    occupancy: 1, high_water: 3, enqueued: 7, dequeued: 6,
    dropped: 2, backpressure: 1, queue_wait_ns: 300,
  } },
  flows: {}, trace_dropped: 0,
};
const model = {
  graph_id: "channel_operator", revision: 1, fingerprint,
  nodes: [{
    id: "worker", element: { name: "test.Worker", revision: 1, digest: elementDigest },
    ports: [
      { name: "in", direction: "input", type: "Event(test.Value)", cardinality: "one", role: "trigger" },
      { name: "out", direction: "output", type: "Event(test.Value)", cardinality: "one", role: "outcome" },
    ],
    reaction: { triggers: ["in"], sampled_state: [], interrupts: [], outcomes: ["out"],
      max_concurrency: 1, breaks_cycles: false }, effects: [],
  }],
  edges: [{ id: "channel", from: { node: "worker", port: "out" },
    to: { node: "worker", port: "in" }, type: "Event(test.Value)", role: "data",
    delivery: "lossy", depth: 4 }],
};
const slots = { register(_name, value) { document.body.append(value); return () => value.remove(); } };
const inspection = {
  available: () => true,
  subscribe(listener) {
    listener({ session_id: "sess-channel", expires_at_ms: Date.now() + 60_000 });
    return () => {};
  },
  async live() { return structuredClone(live); },
  async model() { return structuredClone(model); },
};
try {
  await plugin.mount({
    services: { get(name) { return name === "presentation.client.slots" ? slots :
      name === "presentation.client.inspection" ? inspection : undefined; } },
    lifecycle: { defer() {} },
  });
  for (let attempt = 0; attempt < 50; attempt++) {
    if (document.querySelector('[data-edge-id="channel"]')) break;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  document.documentElement.dataset.ready = String(Boolean(document.querySelector('[data-edge-id="channel"]')));
} catch (error) {
  document.documentElement.dataset.error = error?.message ?? String(error);
}
`
