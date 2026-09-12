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

	// Ninety seconds, the same bound every other Chromium launch in this package
	// uses. Thirty was an outlier, and it is a bound on starting a browser far
	// more than on rendering this fixture: the work here is one --dump-dom of a
	// static module and finishes in about a second locally. On a shared CI
	// runner already carrying another test package, a cold headless start alone
	// can pass thirty seconds, and this test failed at 30.14s - the deadline
	// killing Chromium mid-render, reported as though the view were wrong.
	// Nothing about what is asserted below changes.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
