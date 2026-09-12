package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/internal/testgate"
)

func TestInspectionViewMatchesClosedInspectionVocabularies(t *testing.T) {
	module, err := browserModule("inspection-view.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(module)
	causeStart := strings.Index(source, "const CAUSE_KINDS")
	kindStart := strings.Index(source, "const DECISION_KINDS")
	operationStart := strings.Index(source, "const DECISION_OPERATIONS")
	contractEnd := strings.Index(source, "function object")
	if causeStart < 0 || kindStart <= causeStart || operationStart <= kindStart || contractEnd <= operationStart {
		t.Fatal("inspection view omits a closed inspection vocabulary")
	}
	assertExact := func(label, section string, expected []string) {
		t.Helper()
		matches := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(section, -1)
		actual := make(map[string]struct{}, len(matches))
		for _, match := range matches {
			actual[match[1]] = struct{}{}
		}
		if len(matches) != len(expected) || len(actual) != len(expected) {
			t.Fatalf("inspection view %s vocabulary = %v, want %v", label, matches, expected)
		}
		for _, value := range expected {
			if _, found := actual[value]; !found {
				t.Fatalf("inspection view omits authority-decision %s %q", label, value)
			}
		}
	}
	kinds := element.SupportedInspectionDecisionKinds()
	wantKinds := make([]string, len(kinds))
	for index, kind := range kinds {
		wantKinds[index] = string(kind)
	}
	operations := element.SupportedInspectionDecisionOperations()
	wantOperations := make([]string, len(operations))
	for index, operation := range operations {
		wantOperations[index] = string(operation)
	}
	causes := element.SupportedInspectionCauseKinds()
	wantCauses := make([]string, len(causes))
	for index, cause := range causes {
		wantCauses[index] = string(cause)
	}
	assertExact("cause kind", source[causeStart:kindStart], wantCauses)
	assertExact("kind", source[kindStart:operationStart], wantKinds)
	assertExact("operation", source[operationStart:contractEnd], wantOperations)
	bound := regexp.MustCompile(`const MAX_CAUSAL_PARENTS = ([0-9]+);`).FindStringSubmatch(source)
	if len(bound) != 2 {
		t.Fatal("inspection view omits its causal-parent bound")
	}
	got, err := strconv.Atoi(bound[1])
	if err != nil || got != inspect.MaximumCausalParentsPerStage {
		t.Fatalf("inspection view causal-parent bound = %q, want %d", bound[1],
			inspect.MaximumCausalParentsPerStage)
	}
}

func TestInspectionViewRendersExactChannelAndFlowTelemetryInChromium(t *testing.T) {
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

	// The document is taken from Chromium's stdout as soon as it is complete,
	// and the browser is then killed, rather than waiting for it to exit.
	//
	// Waiting for the exit was the whole failure. --dump-dom is supposed to
	// print the serialised DOM and quit, and it does locally in about a second;
	// on the CI runner's Chromium the process printed nothing this test could
	// use and never exited, so the deadline killed it and the kill was reported
	// as though the view had rendered wrongly. Raising the deadline from thirty
	// seconds to ninety only moved the number in the failure - it died at
	// 90.16s with "signal: killed". Every other Chromium launch in this package
	// drives the browser and then kills it, and none of them depends on the
	// browser deciding to leave.
	//
	// Nothing about what is asserted changes: the assertions below run on the
	// serialised DOM exactly as before, and a browser that never produces one
	// still fails - at the deadline, with whatever it did print.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, chromium,
		"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		// A fresh profile on a CI runner means Chromium treats every launch as
		// a first run: on the runner this test timed out on, ninety seconds of
		// its output was GCM registration retries, component-updater downloads,
		// and PKI metadata parsing, and it never rendered the fixture at all -
		// nothing on stdout, no DOM, killed at the deadline. None of that work
		// is wanted here. The page under test is served from this process's own
		// loopback listener and imports one local module, so a browser that
		// reaches the network for anything is doing something this test did not
		// ask for.
		"--no-first-run", "--disable-background-networking", "--disable-component-update",
		"--disable-sync", "--disable-default-apps", "--disable-client-side-phishing-detection",
		"--user-data-dir="+t.TempDir(), "--virtual-time-budget=2000", "--dump-dom", server.URL,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := &strings.Builder{}
	command.Stderr = diagnostics
	if err := command.Start(); err != nil {
		t.Fatalf("start Chromium: %v", err)
	}
	rendered := make(chan string, 1)
	go func() {
		document := &strings.Builder{}
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := stdout.Read(buffer)
			if n > 0 {
				document.Write(buffer[:n])
				// </html> is the last thing --dump-dom writes, so the document
				// is whole here and nothing is gained by waiting further.
				if strings.Contains(document.String(), "</html>") {
					break
				}
			}
			if readErr != nil {
				break
			}
		}
		rendered <- document.String()
	}()
	var document string
	select {
	case document = <-rendered:
	case <-ctx.Done():
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	if !strings.Contains(document, "</html>") {
		t.Fatalf("Chromium did not serialise a document within the deadline:\nstdout:\n%s\nstderr:\n%s",
			document, diagnostics.String())
	}
	for _, expected := range []string{
		`data-ready="true"`, `data-edge-id="channel"`, `data-delivery="lossy"`,
		`id="delta-availability" data-state="loaded" data-after="0" data-next="2" data-events="1" data-baseline="1"`,
		"Delta journal: session sess-channel; cursor 0 → 2; 1 event; baseline 1; continuous; 0 dropped.",
		`data-depth="4"`, `data-occupancy="1"`, "Delivery: ", "lossy; depth 4",
		"Occupancy: ", "1/4", "Dropped: ", "2", "Backpressure: ", "1",
		"Queue wait: ", "300 ns cumulative; 50 ns per dequeue",
		"Latest live authority decision", "Outcome: ", "succeeded", "Operation: ", "authorize",
		"Irreversible boundary: ", "crossed", "Observed: ", "180 ns from mount clock",
		`data-flow-id="flow_000001"`, `data-stage-count="2"`, `data-truncated="false"`,
		"First traversal: ", "110 ns from mount clock", "Last traversal: ",
		"150 ns from mount clock", "Elapsed: ", "40 ns", "Retention: ", "complete",
		"Stage 1: worker.out → worker.in via channel (Event(test.Value); lossy); " +
			"110 ns from mount clock; first retained stage; causal observation cause_000002 ← cause_000001",
		"Stage 2: worker.out → worker.in via channel (Event(test.Value); lossy); " +
			"150 ns from mount clock; +40 ns; causal state_revision cause_000003 ← cause_000002, cause_000001",
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
	testgate.Missing(t, "chromium")
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
    authority_decision: { kind: "succeeded", operation: "authorize", crossed: true, at_ns: 180 },
    resolution: { element: { name: "test.Worker", revision: 1, digest: elementDigest } },
  } },
  edges: { channel: {
    occupancy: 1, high_water: 3, enqueued: 7, dequeued: 6,
    dropped: 2, backpressure: 1, queue_wait_ns: 300,
  } },
  flows: { flow_000001: {
    correlation: "flow_000001", edges: ["channel", "channel"],
    edge_ns: [110, 150], first_ns: 110, last_ns: 150, truncated: false,
	causal_stages: [
	  { item: "cause_000002", parents: ["cause_000001"], kind: "observation" },
	  { item: "cause_000003", parents: ["cause_000002", "cause_000001"], kind: "state_revision" },
	],
  } }, trace_dropped: 0,
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
const delta = {
  format_version: 1, session_id: "sess-channel",
  graph: { format_version: 1, id: "channel_operator", revision: 1, fingerprint },
  after: 0, next: 2, baseline: { sequence: 1 }, events: [{ sequence: 2 }],
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
  async deltas(after, limit) {
    if (after !== 0 || limit !== 256) throw new Error("unbounded delta request");
    return structuredClone(delta);
  },
};
try {
  await plugin.mount({
    services: { get(name) { return name === "presentation.client.slots" ? slots :
      name === "presentation.client.inspection" ? inspection : undefined; } },
    lifecycle: { defer() {} },
  });
  for (let attempt = 0; attempt < 50; attempt++) {
    if (document.querySelector('[data-edge-id="channel"]') &&
        document.querySelector('[data-flow-id="flow_000001"]') &&
        document.querySelector('#delta-availability')?.dataset.state === "loaded") break;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  document.documentElement.dataset.ready = String(Boolean(
    document.querySelector('[data-edge-id="channel"]') &&
    document.querySelector('[data-flow-id="flow_000001"]') &&
    document.querySelector('#delta-availability')?.dataset.state === "loaded"));
} catch (error) {
  document.documentElement.dataset.error = error?.message ?? String(error);
}
`
