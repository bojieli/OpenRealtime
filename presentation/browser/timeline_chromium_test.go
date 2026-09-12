package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The timeline view draws what the server projected: each `timeline` debug
// event lands in its lane with its words, a start and an end that share a
// span read as one bar, and the list underneath is the same events as text.
// This renders the view in real Chromium over a fake protocol-events service
// and reads the serialised document back.
func TestTimelineViewRendersTheTurnStoryInChromium(t *testing.T) {
	node, chromium := requireInspectionBrowser(t)
	module, err := browserModule("timeline-view.js")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			response.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = response.Write([]byte(`<!doctype html><html><body><script type="module" src="/fixture.js"></script></body></html>`))
		case "/timeline-view.js":
			response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = response.Write(module)
		case "/fixture.js":
			response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = response.Write([]byte(timelineChromiumFixture))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	driver, err := filepath.Abs(filepath.Join("testdata", "inspection_dom.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, driver, server.URL)
	command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+inspectionFreePort(t))
	diagnostics := &strings.Builder{}
	command.Stderr = diagnostics
	rendered, err := command.Output()
	if err != nil {
		t.Fatalf("render the timeline in Chromium: %v\n%s", err, diagnostics.String())
	}
	document := string(rendered)
	for _, expected := range []string{
		`data-view="timeline"`, `data-events="7"`,
		`data-lane="asr" data-kind="partial" data-phase="point"`,
		`partial rev 25 "And yesterday, I met a capybara"`,
		`data-lane="policy" data-kind="speak"`, `speak on partial conf 0.98 47 ms "transcript event: partial`,
		`data-lane="model" data-kind="request" data-phase="start"`,
		`data-lane="model" data-kind="result"`, `result gemini "One."`,
		`data-lane="tts" data-kind="speaking" data-phase="start"`, `speaking start "(payload withheld)"`,
		`data-lane="background" data-kind="scope"`, `scope 35 ms "standing"`,
		"7 events · 7 in window · paused",
	} {
		if !strings.Contains(document, expected) {
			t.Fatalf("Chromium timeline omitted %q:\n%s", expected, document)
		}
	}
	if strings.Contains(document, "data-error=") {
		t.Fatalf("Chromium timeline reported an error:\n%s", document)
	}
}

const timelineChromiumFixture = `
import plugin from "/timeline-view.js";
const base = 1_789_221_570_000;
const listeners = new Set();
const protocol = { subscribe(listener) { listeners.add(listener); return () => listeners.delete(listener); } };
const stateListeners = new Set();
const state = {
  snapshot: () => ({ session: { id: "sess-1" }, connection: { phase: "connected" } }),
  subscribe(listener) { stateListeners.add(listener); listener(state.snapshot(), ""); return () => stateListeners.delete(listener); },
};
const slots = { register(_name, value) { document.body.append(value); return () => value.remove(); } };
const timeline = (offset, attributes, payload, extra = {}) => ({
  type: "openrealtime.debug.event", category: "timeline", name: "timeline",
  event_id: "event_" + offset, timestamp_ms: base + offset, phase: attributes.phase,
  attributes, payload, ...extra,
});
const events = [
  timeline(0, { lane: "asr", kind: "partial", phase: "point", detail: "rev 25" }, { text: "And yesterday, I met a capybara" }),
  timeline(50, { lane: "policy", kind: "speak", phase: "point", detail: "on partial conf 0.98 47 ms" },
    { text: "transcript event: partial\nheard: And yesterday, I met a capybara" }, { duration_ms: 47 }),
  timeline(60, { lane: "model", kind: "request", phase: "start", span: "generation:1", detail: "context v31 speak" }, null),
  timeline(80, { lane: "background", kind: "scope", phase: "point", detail: "35 ms" }, { text: "standing" }, { duration_ms: 35 }),
  timeline(840, { lane: "model", kind: "result", phase: "point", span: "generation:1", detail: "gemini" }, { text: "One." }),
  timeline(845, { lane: "model", kind: "succeeded", phase: "end", span: "generation:1", detail: "782 ms" }, null, { duration_ms: 782 }),
  timeline(850, { lane: "tts", kind: "speaking", phase: "start", span: "utt-1", payloads_redacted: true }, null),
];
try {
  await plugin.mount({
    services: { get(name) {
      return name === "presentation.client.slots" ? slots :
        name === "presentation.client.protocol_events" ? protocol :
        name === "presentation.client.session_state" ? state : undefined;
    } },
    lifecycle: { defer() {} },
  });
  for (const event of events) for (const listener of listeners) listener(event);
  await new Promise((resolve) => setTimeout(resolve, 50));
  // Pause so the list settles on the fixture's window rather than the live edge.
  document.querySelector("#timeline-live").click();
  const scrub = document.querySelector("#timeline-scrub");
  scrub.value = String(base + 1000);
  scrub.dispatchEvent(new Event("input"));
  for (let attempt = 0; attempt < 50; attempt++) {
    if (document.querySelectorAll("#timeline-rows li").length === events.length) break;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  const section = document.querySelector('[data-view="timeline"]');
  document.documentElement.dataset.ready = String(
    document.querySelectorAll("#timeline-rows li").length === events.length && section?.dataset.events === "7");
} catch (error) {
  document.documentElement.dataset.error = error?.message ?? String(error);
}
`
