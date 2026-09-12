import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const PAGE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19311);
const OPERATOR_CAPABILITY = process.env.OPERATOR_CAPABILITY ?? "";
const OPERATOR_CAPABILITY_ROTATED = process.env.OPERATOR_CAPABILITY_ROTATED ?? "";
const AUTHORING_SOURCE = process.env.AUTHORING_SOURCE ?? "";
const AUTHORING_UPDATED_SOURCE = process.env.AUTHORING_UPDATED_SOURCE ?? "";
const AUTHORING_YAML_SOURCE = process.env.AUTHORING_YAML_SOURCE ?? "";
const AUTHORING_JSON_SOURCE = process.env.AUTHORING_JSON_SOURCE ?? "";
const SOURCE_ROOT_IDENTITY = process.env.SOURCE_ROOT_IDENTITY ?? "";
const STATIC_GRAPH_FINGERPRINT = process.env.STATIC_GRAPH_FINGERPRINT ?? "";
const EFFECTS_ENABLED = (process.env.EXPECT_EFFECTS ?? "1") === "1";
const REDUCER_REPLACEMENT_PATH = process.env.REDUCER_REPLACEMENT_PATH ?? "";
const EFFECTS_REPLACEMENT_PATH = process.env.EFFECTS_REPLACEMENT_PATH ?? "";
const CLIENT_TRANSPORT = process.env.CLIENT_TRANSPORT ?? "websocket";
const RETAINED_WORKSPACE_PATH = "replacement-retained.ortg";
const SHIPPED_REPLACEMENT_IMPLEMENTATIONS = Object.freeze({
  "slots": "browser-esm:slots-v2.js",
  "transport": "browser-esm:transport-websocket-v2.js",
  "session-configuration": "browser-esm:session-configuration-v2.js",
  "effects": "browser-esm:effects-client-v2.js",
  "artifact-references": "browser-esm:artifact-references-v2.js",
  "debug-session": "browser-esm:debug-session-v2.js",
  "inspection": "browser-esm:inspection-client-v2.js",
  "management-operator": "browser-esm:management-operator-capability-v2.js",
  "management-transport": "browser-esm:management-transport-v2.js",
  "management-static": "browser-esm:management-static-v2.js",
  "management-authoring": "browser-esm:management-authoring-v2.js",
  "management-source-reading": "browser-esm:management-source-reading-v2.js",
  "management-source-publication": "browser-esm:management-source-publication-v2.js",
  "authoring-workspace": "browser-esm:authoring-workspace-v2.js",
  "view": "browser-esm:text-view-v2.js",
  "confirmation-view": "browser-esm:confirmation-view-v2.js",
  "artifact-view": "browser-esm:artifact-view-v2.js",
  "inspection-view": "browser-esm:inspection-view-v2.js",
  "trace-view": "browser-esm:trace-view-v2.js",
  "timeline-view": "browser-esm:timeline-view-v2.js",
  "management-operator-view": "browser-esm:management-operator-view-v2.js",
  "authoring-editor-view": "browser-esm:authoring-editor-view-v2.js",
  "authoring-configuration-view": "browser-esm:authoring-configuration-view-v2.js",
  "authoring-canvas-view": "browser-esm:authoring-canvas-view-v2.js",
});
const SHIPPED_PREDECESSOR_IMPLEMENTATIONS = Object.freeze({
  "slots": "browser-esm:slots.js",
  "transport": "browser-esm:transport-websocket.js",
  "session-configuration": "browser-esm:session-configuration.js",
  "effects": "browser-esm:effects-client.js",
  "artifact-references": "browser-esm:artifact-references.js",
  "debug-session": "browser-esm:debug-session.js",
  "inspection": "browser-esm:inspection-client.js",
  "management-operator": "browser-esm:management-operator-capability.js",
  "management-transport": "browser-esm:management-transport.js",
  "management-static": "browser-esm:management-static.js",
  "management-authoring": "browser-esm:management-authoring.js",
  "management-source-reading": "browser-esm:management-source-reading.js",
  "management-source-publication": "browser-esm:management-source-publication.js",
  "authoring-workspace": "browser-esm:authoring-workspace.js",
  "view": "browser-esm:text-view.js",
  "confirmation-view": "browser-esm:confirmation-view.js",
  "artifact-view": "browser-esm:artifact-view.js",
  "inspection-view": "browser-esm:inspection-view.js",
  "trace-view": "browser-esm:trace-view.js",
  "timeline-view": "browser-esm:timeline-view.js",
  "management-operator-view": "browser-esm:management-operator-view.js",
  "authoring-editor-view": "browser-esm:authoring-editor-view.js",
  "authoring-configuration-view": "browser-esm:authoring-configuration-view.js",
  "authoring-canvas-view": "browser-esm:authoring-canvas-view.js",
});
if (!new Set(["websocket", "webrtc"]).has(CLIENT_TRANSPORT)) {
  throw new Error("developer client transport fixture is invalid");
}
if (!OPERATOR_CAPABILITY || !OPERATOR_CAPABILITY_ROTATED || !AUTHORING_SOURCE ||
    !AUTHORING_YAML_SOURCE || !AUTHORING_JSON_SOURCE ||
    !/^sha256:[0-9a-f]{64}$/.test(STATIC_GRAPH_FINGERPRINT) ||
    (EFFECTS_ENABLED && (!AUTHORING_UPDATED_SOURCE ||
      !/^sha256:[0-9a-f]{64}$/.test(SOURCE_ROOT_IDENTITY)))) {
  throw new Error("developer management E2E fixture is incomplete");
}
if (REDUCER_REPLACEMENT_PATH && (CLIENT_TRANSPORT !== "websocket" ||
    !REDUCER_REPLACEMENT_PATH.startsWith("/test/"))) {
  throw new Error("developer reducer replacement fixture is invalid");
}
if (EFFECTS_REPLACEMENT_PATH && (!EFFECTS_ENABLED || CLIENT_TRANSPORT !== "websocket" ||
    !REDUCER_REPLACEMENT_PATH || !EFFECTS_REPLACEMENT_PATH.startsWith("/test/"))) {
  throw new Error("developer shipped-consumer replacement fixture is invalid");
}
const profile = mkdtempSync(join(tmpdir(), "openrealtime-developer-client-"));
const chromium = // A fresh profile makes every launch a first run, so Chromium spends real time
// on GCM registration, component updates, and PKI metadata before it settles.
// Every page these drivers open is served from a loopback listener in the test
// process, so a browser reaching the network is doing work nothing asked for -
// and on a CI runner that work is what the waits below end up queued behind.
// bench/meeting and bench/realtimecu already launch Chromium this way.
spawn(process.env.CHROMIUM ?? "chromium", [
  "--disable-background-networking", "--disable-component-update",
  "--disable-default-apps", "--no-first-run", "--disable-sync",
  "--disable-client-side-phishing-detection",
  "--headless=new", `--remote-debugging-port=${PORT}`, "--no-sandbox", "--disable-gpu",
  "--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream",
  "--autoplay-policy=no-user-gesture-required",
  `--user-data-dir=${profile}`, "about:blank",
], { stdio: ["ignore", "pipe", "pipe"] });
let chromiumErrors = "";
chromium.stderr.on("data", (chunk) => { chromiumErrors += chunk.toString(); });

async function endpoint() {
  for (let attempt = 0; attempt < 100; attempt++) {
    try {
      const response = await fetch(`http://127.0.0.1:${PORT}/json/version`);
      return (await response.json()).webSocketDebuggerUrl;
    } catch { await sleep(100); }
  }
  throw new Error(`Chromium did not start:\n${chromiumErrors}`);
}

class CDP {
  #socket; #next = 1; #pending = new Map(); #handlers = new Map();
  static async connect(url) {
    const client = new CDP();
    client.#socket = new WebSocket(url);
    await new Promise((resolve, reject) => {
      client.#socket.addEventListener("open", resolve, { once: true });
      client.#socket.addEventListener("error", reject, { once: true });
    });
    client.#socket.addEventListener("message", (message) => {
      const frame = JSON.parse(message.data);
      if (frame.id && client.#pending.has(frame.id)) {
        const pending = client.#pending.get(frame.id);
        client.#pending.delete(frame.id);
        clearTimeout(pending.timer);
        frame.error ? pending.reject(new Error(JSON.stringify(frame.error))) : pending.resolve(frame.result);
      } else if (frame.method) {
        for (const handler of client.#handlers.get(frame.method) ?? []) handler(frame.params);
      }
    });
    return client;
  }
  on(method, handler) {
    const rows = this.#handlers.get(method) ?? [];
    rows.push(handler); this.#handlers.set(method, rows);
  }
  send(method, params = {}, sessionId) {
    const id = this.#next++;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.#pending.delete(id);
        reject(new Error(`CDP ${method} timed out`));
      }, 20000);
      this.#pending.set(id, { resolve, reject, timer });
      this.#socket.send(JSON.stringify({ id, method, params, sessionId }));
    });
  }
}

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? ` — ${detail}` : ""}`);
}

try {
  const browser = await CDP.connect(await endpoint());
  const { targetId } = await browser.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await browser.send("Target.attachToTarget", { targetId, flatten: true });
  const call = (method, params) => browser.send(method, params, sessionId);
  const exceptions = []; const failedRequests = []; const browserConsole = [];
  browser.on("Runtime.exceptionThrown", (value) =>
    exceptions.push(value.exceptionDetails.exception?.description ?? value.exceptionDetails.text));
  browser.on("Runtime.consoleAPICalled", (value) => {
    for (const argument of value.args ?? []) {
      if (typeof argument.value === "string") browserConsole.push(argument.value);
    }
  });
  // Provider withdrawal and capability rotation deliberately abort their
  // owned fetches. CDP reports those as loading failures even though bounded
  // cancellation is the behavior under test; only uncancelled failures are a
  // transport/resource regression.
  browser.on("Network.loadingFailed", (value) => {
    if (!value.canceled) failedRequests.push(value.errorText);
  });
  await call("Runtime.enable"); await call("Network.enable"); await call("Page.enable");
  const navigationStarted = performance.now();
  await call("Page.navigate", { url: PAGE_URL });
  const evaluate = async (expression) => {
    const { result, exceptionDetails } = await call("Runtime.evaluate", {
      expression, awaitPromise: true, returnByValue: true,
    });
    if (exceptionDetails) throw new Error(exceptionDetails.exception?.description ?? exceptionDetails.text);
    return result.value;
  };
  const waitFor = async (name, predicate, timeout = 30000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      if (await predicate()) return true;
      await sleep(100);
    }
    console.log(`timed out waiting for ${name}`);
    return false;
  };
  const waitForValue = async (name, read, timeout = 30000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      const value = await read();
      if (value) return value;
      await sleep(100);
    }
    console.log(`timed out waiting for ${name}`);
    return "";
  };

  await waitFor("developer profile boot", () => evaluate(
    `document.getElementById("openrealtime-root")?.dataset.state === "ready"`));
  const bootMS = performance.now() - navigationStarted;
  check("locked developer client booted", await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`) === "ready");
  const expectedMounted = CLIENT_TRANSPORT === "webrtc" ? [
      "slots", "media", "transport", "reducer", "session-configuration", "video", "debug-session",
      ...(EFFECTS_ENABLED ? ["effects", "artifact-references"] : []), "inspection", "view",
      ...(EFFECTS_ENABLED ? ["confirmation-view", "artifact-view"] : []),
      "video-controls", "transport-diagnostics", "inspection-view", "trace-view", "timeline-view",
    ] : [
      "slots", "transport", "reducer", "session-configuration", "debug-session",
      ...(EFFECTS_ENABLED ? ["effects", "artifact-references"] : []), "inspection", "view",
      ...(EFFECTS_ENABLED ? ["confirmation-view", "artifact-view"] : []),
      "inspection-view", "trace-view", "timeline-view",
    ];
  expectedMounted.push(
      "management-operator", "management-transport", "management-static", "management-authoring",
      ...(EFFECTS_ENABLED ? ["management-source-reading", "management-source-publication"] : []),
      "authoring-workspace", "management-operator-view", "authoring-editor-view",
      "authoring-configuration-view", "authoring-canvas-view",
  );
  check(`${expectedMounted.length} replaceable plugins mounted`, JSON.stringify(await evaluate(
    `window.__openrealtime?.mounted`)) === JSON.stringify(expectedMounted));
  if (!EFFECTS_ENABLED) {
    check("observer profile exposes no effects, artifacts, confirmations, or effect endpoint",
      await evaluate(`(() => {
        const live = window.__openrealtime.live();
        const manifest = window.__openrealtime.manifest;
        const forbidden = ["effects", "artifact-references", "confirmation-view", "artifact-view",
          "management-source-reading", "management-source-publication"];
        const services = Object.values(live.entries).flatMap((entry) => entry.services ?? []);
        const sourceGrant = (manifest.grants ?? []).some((grant) => grant.entry === "management-transport" &&
          (grant.permissions ?? []).some((permission) => permission.kind === "network.connect" &&
            permission.resource === "host-management" &&
            (permission.operations ?? []).some((operation) =>
              operation === "source-read" || operation === "publication")));
        const editor = document.querySelector('[data-view=authoring-editor]');
        return !forbidden.some((name) =>
          Object.hasOwn(live.entries, name) || window.__openrealtime.mounted.includes(name)) &&
          !(manifest.grants ?? []).some((grant) => forbidden.includes(grant.entry)) &&
          !sourceGrant && editor?.querySelector('button[data-action=load]')?.disabled === true &&
          editor?.querySelector('button[data-action=publish-create]')?.disabled === true &&
          editor?.querySelector('button[data-action=publish-update]')?.disabled === true &&
          !services.includes("presentation.client.tools_effects") &&
          !services.includes("presentation.client.artifacts") &&
          !(manifest.endpoints ?? []).some((endpoint) => endpoint.name === "effects.local") &&
          document.querySelector('[data-view=artifacts], [data-view=confirmations]') === null;
      })()`));
  }
  check("no module request failed", failedRequests.length === 0, failedRequests.join("; "));

  const connectStarted = performance.now();
  await evaluate(`document.getElementById("connect").click()`);
  await waitFor("realtime connection", () => evaluate(
    `document.getElementById("state").textContent === "connected"`));
  check("developer client uses the common realtime endpoint", await evaluate(
    `document.getElementById("state").textContent`) === "connected");

  const identity = await waitForValue("live graph inspection", () => evaluate(`(() => {
    const view = document.querySelector('[data-view=inspection]');
    const exact = view?.querySelector('#identity')?.textContent ?? "";
    return view?.querySelector('#availability')?.textContent === "live" &&
      view?.querySelector('#identity')?.textContent.includes("sha256:") &&
      view?.querySelector('#snapshot')?.textContent.includes("resolution") ? exact : "";
  })()`));
  check("management API returned the exact compatibility graph", identity.includes("compat_") &&
    identity.includes("sha256:"), identity);
  check("management snapshot rendered resolved nodes", await waitFor(
    "resolved management snapshot", () => evaluate(
      `document.querySelector('[data-view=inspection] #snapshot')?.textContent.includes('resolution')`)));
  check("exact static reaction and effect contracts joined live node evidence", await waitFor(
    "joined inspection model", () => evaluate(`(() => {
      const view = document.querySelector('[data-view=inspection]');
      const cards = view?.querySelectorAll('[data-role=inspection-nodes] article') ?? [];
      const edges = view?.querySelectorAll('[data-role=inspection-edges] article') ?? [];
      return view?.querySelector('#contract-availability')?.dataset.state === "joined" &&
        cards.length > 0 && [...cards].every((card) =>
          card.querySelector('[data-role=reaction-contract]') &&
          card.querySelector('[data-role=reaction-timing]') &&
          card.querySelector('[data-role=effect-authority]')) &&
        [...edges].every((edge) =>
          Number(edge.dataset.depth) > 0 && Number(edge.dataset.occupancy) >= 0 &&
          ["lossless", "lossy"].includes(edge.dataset.delivery) &&
          edge.textContent.includes("Queue wait:"));
    })()`)));
  check("bounded resumable session deltas are joined to the exact live graph", await waitFor(
    "session delta journal", () => evaluate(`(() => {
      const delta = document.querySelector('[data-view=inspection] #delta-availability');
      if (delta?.dataset.state !== "loaded" || delta.dataset.after !== "0") return false;
      const next = Number(delta.dataset.next);
      const events = Number(delta.dataset.events);
      const baseline = Number(delta.dataset.baseline);
      return Number.isSafeInteger(next) && next >= 0 && Number.isSafeInteger(events) &&
        events >= 0 && events <= 256 && Number.isSafeInteger(baseline) && baseline >= 0 &&
        baseline <= next && delta.textContent.includes("Delta journal: session ");
    })()`)));
  const traceIdentity = await waitForValue("causal trace", () => evaluate(`(() => {
    const view = document.querySelector('[data-view=trace]');
    const exact = view?.querySelector('#trace-identity')?.textContent ?? "";
    return view?.querySelector('#trace-state')?.textContent.startsWith("live") &&
      exact.includes("sha256:") ? exact : "";
  })()`));
  const inspectMS = performance.now() - connectStarted;
  check("payload-free causal trace is independently rendered", traceIdentity.includes("sha256:"));
  check("narrow capability is not rendered", !(await evaluate(`document.body.textContent`)).includes("mgmt_"));
  check("narrow capability is absent from the immutable manifest", !(await evaluate(
    `JSON.stringify(window.__openrealtime.manifest)`)).includes("mgmt_"));

  const configureOperator = (capability, expiryMS = 0) => evaluate(`(() => {
    const view = document.querySelector('[data-view=management-operator]');
    const input = view?.querySelector('input[name=operator-capability]');
    const expiry = view?.querySelector('input[name=operator-expiry-ms]');
    if (!input || !expiry) return null;
    input.value = ${JSON.stringify(capability)};
    expiry.value = ${JSON.stringify(String(expiryMS))};
    input.form.dispatchEvent(new Event("submit", { bubbles:true, cancelable:true }));
    return { cleared: input.value === "", expiryCleared: expiry.value === "0",
      status: view.querySelector('[data-role=status]')?.textContent ?? "" };
  })()`);
  const loadStatic = () => evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-configuration]');
    const input = view?.querySelector('input[name=management-graph-fingerprint]');
    const button = view?.querySelector('button[data-action=load-static]');
    if (!input || !button) return false;
    input.value = ${JSON.stringify(STATIC_GRAPH_FINGERPRINT)};
    button.click();
    return view.querySelector('[data-role=catalog-status]')?.textContent === "loading";
  })()`);
  const staticStatus = () => evaluate(
    `document.querySelector('[data-view=authoring-configuration] [data-role=catalog-status]')?.textContent ?? ""`);

  const authorityStarted = performance.now();
  const configured = await configureOperator(OPERATOR_CAPABILITY);
  check("operator capability enters only the private provider", configured?.cleared && configured.expiryCleared &&
    configured.status.startsWith("Configured"));
  await loadStatic();
  await waitFor("exact static management catalog", async () => (await staticStatus()) === "loaded");
  const staticMS = performance.now() - authorityStarted;
  const staticCatalog = await evaluate(
    `document.querySelector('[data-view=authoring-configuration] [data-role=catalog-result]')?.textContent ?? ""`);
  check("static graph, descriptor, and values schema share exact identities",
    staticCatalog.includes(STATIC_GRAPH_FINGERPRINT) && staticCatalog.includes("compat.BindingRuntime") &&
    staticCatalog.includes('"values_schema"'));

  const analyzeStarted = performance.now();
  await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-editor]');
    view.querySelector('input[name=authoring-path]').value = "browser-authoring.ortg";
    view.querySelector('textarea[name=authoring-source]').value = ${JSON.stringify(`${AUTHORING_SOURCE}\n`)};
    view.querySelector('button[data-action=analyze]').click();
  })()`);
  await waitFor("authoring analysis", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "analyzed"`));
  const analyzeMS = performance.now() - analyzeStarted;
  const configurationMetadata = await evaluate(
    `document.querySelector('[data-view=authoring-configuration]')?.textContent ?? ""`);
  check("configuration renderer projects server-validated descriptor metadata",
    configurationMetadata.includes("compat.BindingRuntime") && configurationMetadata.includes("schema://"));
  const authoringDiagnostics = await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-editor]');
    return { status: view?.querySelector('[data-role=diagnostic-status]')?.textContent ?? "",
      count: view?.querySelector('[data-role=diagnostics]')?.children.length ?? -1,
      text: view?.querySelector('[data-role=diagnostics]')?.textContent ?? "" };
  })()`);
  check("browser editor renders the exact bounded diagnostic report as text",
    authoringDiagnostics.status === "1 of 1 diagnostics" && authoringDiagnostics.count === 1 &&
      authoringDiagnostics.text.includes("[E_NON_CANONICAL_SOURCE] error") &&
      authoringDiagnostics.text.includes("format it before position-sensitive editor operations"),
    JSON.stringify(authoringDiagnostics));

  const formatStarted = performance.now();
  check("browser formatter is available only for the analyzed noncanonical source", await evaluate(`(() => {
    const button = document.querySelector('[data-view=authoring-editor] button[data-action=format]');
    return button && !button.disabled;
  })()`));
  await evaluate(
    `document.querySelector('[data-view=authoring-editor] button[data-action=format]').click()`);
  await waitFor("authoring format", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "formatted"`));
  const formatMS = performance.now() - formatStarted;
  check("browser formatter installs the exact canonical bytes and invalidates stale analysis", await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-editor]');
    return view?.querySelector('textarea[name=authoring-source]').value === ${JSON.stringify(AUTHORING_SOURCE)} &&
      view.querySelector('button[data-action=format]').disabled &&
      view.querySelector('[data-role=diagnostic-status]').textContent ===
        "Analyze a graph to inspect diagnostics.";
  })()`));

  const compileStarted = performance.now();
  await evaluate(
    `document.querySelector('[data-view=authoring-editor] button[data-action=compile]').click()`);
  await waitFor("authoring compile", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "compiled"`));
  const compileMS = performance.now() - compileStarted;
  const originalAuthoringFingerprint = await evaluate(`document.querySelector(
    '[data-view=authoring-canvas] button[data-action=select-node][data-node=runtime]')?.dataset.fingerprint ?? ""`);
  check("compiled canvas exposes the exact selectable node", /^sha256:[0-9a-f]{64}$/.test(
    originalAuthoringFingerprint));

  const renameStarted = performance.now();
  await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-canvas]');
    const selected = view.querySelector('button[data-action=select-node][data-node=runtime]');
    const replacement = view.querySelector('input[name=authoring-node-name]');
    if (!selected || !replacement) throw new Error("compiled runtime node is not selectable");
    selected.click();
    replacement.value = "renamed_runtime";
    view.querySelector('button[data-action=rename-node]').click();
  })()`);
  await waitFor("authoring canvas node rename", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "renamed"`));
  const renameMS = performance.now() - renameStarted;
  check("canvas rename installs exact graph-wide source and invalidates the old compile", await evaluate(`(() => {
    const editor = document.querySelector('[data-view=authoring-editor]');
    const canvas = document.querySelector('[data-view=authoring-canvas]');
    const source = editor?.querySelector('textarea[name=authoring-source]')?.value ?? "";
    return source.includes(":: renamed_runtime;") && source.includes("= renamed_runtime.") &&
      !/\\bruntime\\b/.test(source) &&
      canvas?.querySelectorAll('button[data-action=select-node]').length === 0;
  })()`));

  const renamedCompileStarted = performance.now();
  await evaluate(
    `document.querySelector('[data-view=authoring-editor] button[data-action=compile]').click()`);
  await waitFor("renamed authoring compile", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "compiled"`));
  const renamedCompileMS = performance.now() - renamedCompileStarted;
  check("renamed source recompiles at the next revision and changes fingerprint", await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-canvas]');
    const renamed = view?.querySelector(
      'button[data-action=select-node][data-node=renamed_runtime]');
    return renamed?.dataset.fingerprint && renamed.dataset.fingerprint !==
      ${JSON.stringify(originalAuthoringFingerprint)} && view.textContent.includes("· r2 ·") &&
      !view.querySelector('button[data-action=select-node][data-node=runtime]');
  })()`));

  const renderStarted = performance.now();
  await evaluate(
    `document.querySelector('[data-view=authoring-canvas] button[data-action=render-model]').click()`);
  await waitFor("authoring canvas render", () => evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-canvas]');
    return view?.querySelector('[data-role=rendering]')?.textContent.includes('"nodes"') &&
      view?.querySelector('[data-role=rendering]')?.textContent.includes('"reaction"') &&
      view.textContent.includes("sha256:");
  })()`));
  const renderMS = performance.now() - renderStarted;
  check("compile and canvas render remain bound to one graph fingerprint", await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-canvas]');
    return view?.textContent.includes("browser_authoring") &&
      view?.querySelector('[data-role=rendering]')?.textContent.includes('"nodes"') &&
      view?.querySelector('[data-role=rendering]')?.textContent.includes('"reaction"') &&
      view?.querySelector('[data-role=rendering]')?.textContent.includes('"renamed_runtime"');
  })()`));

  const renamedAuthoringFingerprint = await evaluate(`document.querySelector(
    '[data-view=authoring-canvas] button[data-action=select-edge][data-edge=optional]')?.dataset.fingerprint ?? ""`);
  check("compiled canvas exposes the exact selectable edge",
    /^sha256:[0-9a-f]{64}$/.test(renamedAuthoringFingerprint) &&
      renamedAuthoringFingerprint !== originalAuthoringFingerprint);
  const edgeRemovalStarted = performance.now();
  await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-canvas]');
    const selected = view.querySelector('button[data-action=select-edge][data-edge=optional]');
    const remove = view.querySelector('button[data-action=remove-edge]');
    if (!selected || !remove) throw new Error("compiled optional edge is not selectable");
    selected.click();
    remove.click();
  })()`);
  await waitFor("authoring canvas edge removal", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "edge-removed"`));
  const edgeRemovalMS = performance.now() - edgeRemovalStarted;
  check("canvas edge removal installs exact canonical source and invalidates the old compile", await evaluate(`(() => {
    const editor = document.querySelector('[data-view=authoring-editor]');
    const canvas = document.querySelector('[data-view=authoring-canvas]');
    const source = editor?.querySelector('textarea[name=authoring-source]')?.value ?? "";
    return source.includes(":: renamed_runtime;") && !source.includes("edge optional =") &&
      canvas?.querySelectorAll('button[data-action=select-edge]').length === 0;
  })()`));

  const edgeCompileStarted = performance.now();
  await evaluate(
    `document.querySelector('[data-view=authoring-editor] button[data-action=compile]').click()`);
  await waitFor("edge-removed authoring compile", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "compiled"`));
  const edgeCompileMS = performance.now() - edgeCompileStarted;
  check("edge-removed source recompiles at the next revision under a changed fingerprint", await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-canvas]');
    const renamed = view?.querySelector('button[data-action=select-node][data-node=renamed_runtime]');
    return renamed?.dataset.fingerprint && renamed.dataset.fingerprint !==
      ${JSON.stringify(renamedAuthoringFingerprint)} && view.textContent.includes("· r3 ·") &&
      !view.querySelector('button[data-action=select-edge][data-edge=optional]');
  })()`));

  const edgeRemovedAuthoringFingerprint = await evaluate(`document.querySelector(
    '[data-view=authoring-canvas] button[data-action=select-edge-from][data-endpoint="producer.out"]'
  )?.dataset.fingerprint ?? ""`);
  check("compiled canvas exposes deterministic directional ports for edge creation",
    /^sha256:[0-9a-f]{64}$/.test(edgeRemovedAuthoringFingerprint) &&
      edgeRemovedAuthoringFingerprint !== renamedAuthoringFingerprint && await evaluate(`(() => {
        const view = document.querySelector('[data-view=authoring-canvas]');
        return Boolean(view?.querySelector(
          'button[data-action=select-edge-to][data-endpoint="sink.in"]'));
      })()`));
  const edgeCreationStarted = performance.now();
  await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-canvas]');
    const from = view.querySelector(
      'button[data-action=select-edge-from][data-endpoint="producer.out"]');
    const to = view.querySelector(
      'button[data-action=select-edge-to][data-endpoint="sink.in"]');
    const name = view.querySelector('input[name=authoring-edge-name]');
    const create = view.querySelector('button[data-action=create-edge]');
    if (!from || !to || !name || !create) throw new Error("compiled edge endpoints are not selectable");
    from.click();
    to.click();
    name.value = "restored";
    name.dispatchEvent(new Event("input", { bubbles: true }));
    create.click();
  })()`);
  await waitFor("authoring canvas edge creation", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "edge-created"`));
  const edgeCreationMS = performance.now() - edgeCreationStarted;
  check("canvas edge creation installs exact canonical source and invalidates the predecessor compile",
    await evaluate(`(() => {
      const editor = document.querySelector('[data-view=authoring-editor]');
      const canvas = document.querySelector('[data-view=authoring-canvas]');
      const source = editor?.querySelector('textarea[name=authoring-source]')?.value ?? "";
      return source.includes(":: renamed_runtime;") &&
        source.includes("edge restored = producer.out -> sink.in;") &&
        !source.includes("edge optional =") &&
        canvas?.querySelectorAll('button[data-action=select-edge]').length === 0;
    })()`));

  const edgeCreatedCompileStarted = performance.now();
  await evaluate(
    `document.querySelector('[data-view=authoring-editor] button[data-action=compile]').click()`);
  await waitFor("edge-created authoring compile", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "compiled"`));
  const edgeCreatedCompileMS = performance.now() - edgeCreatedCompileStarted;
  check("edge-created source recompiles at revision four under its server-produced fingerprint",
    await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-canvas]');
      const restored = view?.querySelector('button[data-action=select-edge][data-edge=restored]');
      return restored?.dataset.fingerprint && restored.dataset.fingerprint !==
        ${JSON.stringify(edgeRemovedAuthoringFingerprint)} && view.textContent.includes("· r4 ·") &&
        view.querySelector('button[data-action=select-node][data-node=renamed_runtime]') &&
        !view.querySelector('button[data-action=select-edge][data-edge=optional]');
    })()`));

  if (EFFECTS_ENABLED) {
    await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-editor]');
      view.querySelector('input[name=authoring-root-identity]').value = ${JSON.stringify(SOURCE_ROOT_IDENTITY)};
      view.querySelector('button[data-action=publish-create]').click();
    })()`);
    await waitFor("mediated browser source create", () => evaluate(`(() => {
      const receipt = document.querySelector(
        '[data-view=authoring-editor] [data-role=publication-receipt]')?.textContent;
      try { return JSON.parse(receipt).mode === "create"; } catch { return false; }
    })()`));
    const createdSource = await evaluate(`JSON.parse(document.querySelector(
      '[data-view=authoring-editor] [data-role=publication-receipt]').textContent)`);
    check("browser create crosses only the rooted mediated publication boundary",
      createdSource.root_identity === SOURCE_ROOT_IDENTITY && createdSource.path === "browser-authoring.ortg" &&
      createdSource.mode === "create" && /^sha256:[0-9a-f]{64}$/.test(createdSource.receipt_digest));

    await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-editor]');
      view.querySelector('textarea[name=authoring-source]').value = ${JSON.stringify(AUTHORING_UPDATED_SOURCE)};
      view.querySelector('button[data-action=publish-update]').click();
    })()`);
    await waitFor("stale-digest-bound browser source update", () => evaluate(`(() => {
      const receipt = document.querySelector(
        '[data-view=authoring-editor] [data-role=publication-receipt]')?.textContent;
      try { return JSON.parse(receipt).mode === "update"; } catch { return false; }
    })()`));
    const updatedSource = await evaluate(`JSON.parse(document.querySelector(
      '[data-view=authoring-editor] [data-role=publication-receipt]').textContent)`);
    check("browser update binds the exact predecessor and advances the visible receipt",
      updatedSource.previous_source_digest === createdSource.source_digest &&
      updatedSource.source_digest !== createdSource.source_digest && updatedSource.path === createdSource.path &&
      updatedSource.root_identity === createdSource.root_identity);

    await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-editor]');
      view.querySelector('textarea[name=authoring-source]').value = "graph unsaved_local_text {\\n}\\n";
      view.querySelector('button[data-action=load]').click();
    })()`);
    await waitFor("rooted browser source load", () => evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-editor]');
      const result = view?.querySelector('[data-role=source-read-result]')?.textContent;
      try { return view.querySelector('[data-role=status]').textContent === "loaded" &&
        JSON.parse(result).source_digest === ${JSON.stringify(updatedSource.source_digest)}; }
      catch { return false; }
    })()`));
    const loadedSource = await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-editor]');
      return {
        source: view.querySelector('textarea[name=authoring-source]').value,
        predecessor: view.querySelector('input[name=authoring-expected-digest]').value,
        result: JSON.parse(view.querySelector('[data-role=source-read-result]').textContent),
      };
    })()`);
    check("browser load replaces unsaved text with exact rooted source and payload-free evidence",
      loadedSource.source === AUTHORING_UPDATED_SOURCE &&
      loadedSource.predecessor === updatedSource.source_digest &&
      loadedSource.result.source_digest === updatedSource.source_digest &&
      loadedSource.result.root_identity === SOURCE_ROOT_IDENTITY &&
      loadedSource.result.path === updatedSource.path && loadedSource.result.source === undefined &&
      /^sha256:[0-9a-f]{64}$/.test(loadedSource.result.result_digest));
  }

  const exerciseNormalizedCanvas = async (label, path, source) => {
    const started = performance.now();
    await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-editor]');
      view.querySelector('input[name=authoring-path]').value = ${JSON.stringify(path)};
      view.querySelector('textarea[name=authoring-source]').value = ${JSON.stringify(source)};
      view.querySelector('button[data-action=compile]').click();
    })()`);
    await waitFor(`${label} normalized compile`, () => evaluate(`(() => {
      const editor = document.querySelector('[data-view=authoring-editor]');
      const canvas = document.querySelector('[data-view=authoring-canvas]');
      return editor?.querySelector('[data-role=status]')?.textContent === "compiled" &&
        canvas?.querySelector('button[data-action=select-node][data-node=runtime]');
    })()`));

    await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-canvas]');
      const selected = view.querySelector('button[data-action=select-node][data-node=runtime]');
      const replacement = view.querySelector('input[name=authoring-node-name]');
      selected.click();
      replacement.value = ${JSON.stringify(`${label}_runtime`)};
      view.querySelector('button[data-action=rename-node]').click();
    })()`);
    await waitFor(`${label} normalized rename`, () => evaluate(
      `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "renamed"`));
    await evaluate(
      `document.querySelector('[data-view=authoring-editor] button[data-action=compile]').click()`);
    await waitFor(`${label} renamed normalized compile`, () => evaluate(`(() => {
      const editor = document.querySelector('[data-view=authoring-editor]');
      const canvas = document.querySelector('[data-view=authoring-canvas]');
      return editor?.querySelector('[data-role=status]')?.textContent === "compiled" &&
        canvas?.querySelector('button[data-action=select-edge][data-edge=optional]');
    })()`));

    await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-canvas]');
      view.querySelector('button[data-action=select-edge][data-edge=optional]').click();
      view.querySelector('button[data-action=remove-edge]').click();
    })()`);
    await waitFor(`${label} normalized edge removal`, () => evaluate(
      `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "edge-removed"`));
    await evaluate(
      `document.querySelector('[data-view=authoring-editor] button[data-action=compile]').click()`);
    await waitFor(`${label} edge-less normalized compile`, () => evaluate(`(() => {
      const editor = document.querySelector('[data-view=authoring-editor]');
      const canvas = document.querySelector('[data-view=authoring-canvas]');
      return editor?.querySelector('[data-role=status]')?.textContent === "compiled" &&
        canvas?.querySelector('button[data-action=select-edge-from][data-endpoint="producer.out"]') &&
        canvas?.querySelector('button[data-action=select-edge-to][data-endpoint="sink.in"]');
    })()`));

    await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-canvas]');
      view.querySelector('button[data-action=select-edge-from][data-endpoint="producer.out"]').click();
      view.querySelector('button[data-action=select-edge-to][data-endpoint="sink.in"]').click();
      const name = view.querySelector('input[name=authoring-edge-name]');
      name.value = ${JSON.stringify(`restored_${label}`)};
      name.dispatchEvent(new Event("input", { bubbles: true }));
      view.querySelector('button[data-action=create-edge]').click();
    })()`);
    await waitFor(`${label} normalized edge creation`, () => evaluate(
      `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "edge-created"`));
    await evaluate(
      `document.querySelector('[data-view=authoring-editor] button[data-action=compile]').click()`);
    await waitFor(`${label} edge-created normalized compile`, () => evaluate(`(() => {
      const editor = document.querySelector('[data-view=authoring-editor]');
      const canvas = document.querySelector('[data-view=authoring-canvas]');
      return editor?.querySelector('[data-role=status]')?.textContent === "compiled" &&
        canvas?.querySelector('button[data-action=select-edge][data-edge=${`restored_${label}`}]');
    })()`));
    const result = await evaluate(`(() => {
      const editor = document.querySelector('[data-view=authoring-editor]');
      const canvas = document.querySelector('[data-view=authoring-canvas]');
      return { path: editor.querySelector('input[name=authoring-path]').value,
        source: editor.querySelector('textarea[name=authoring-source]').value,
        fingerprint: canvas.querySelector(
          'button[data-action=select-edge][data-edge=${`restored_${label}`}]')?.dataset.fingerprint ?? "" };
    })()`);
    return { ...result, elapsed: performance.now() - started };
  };

  const normalizedYAML = await exerciseNormalizedCanvas("yaml", "browser-authoring.yaml", AUTHORING_YAML_SOURCE);
  const normalizedJSON = await exerciseNormalizedCanvas("json", "browser-authoring.json", AUTHORING_JSON_SOURCE);
  const yamlExact = normalizedYAML.source.includes("    - id: yaml_runtime\n") &&
    normalizedYAML.source.includes("      endpoint: yaml_runtime.") &&
    normalizedYAML.source.includes("    - id: restored_yaml\n") &&
    !normalizedYAML.source.includes("    - id: runtime\n") &&
    !normalizedYAML.source.includes("    - id: optional\n");
  let jsonExact = false;
  try {
    const value = JSON.parse(normalizedJSON.source);
    jsonExact = value.graph.nodes.some((node) => node.id === "json_runtime") &&
      !value.graph.nodes.some((node) => node.id === "runtime") &&
      value.graph.boundaries.filter((boundary) => boundary.endpoint.startsWith("json_runtime.")).length > 0 &&
      value.graph.edges.length === 1 && value.graph.edges[0].id === "restored_json";
  } catch {}
  check("real canvas mutates and recompiles canonical normalized YAML",
    normalizedYAML.path === "browser-authoring.yaml" && yamlExact &&
      /^sha256:[0-9a-f]{64}$/.test(normalizedYAML.fingerprint));
  check("real canvas mutates and recompiles canonical normalized JSON",
    normalizedJSON.path === "browser-authoring.json" && jsonExact &&
      /^sha256:[0-9a-f]{64}$/.test(normalizedJSON.fingerprint));

  const rotated = await configureOperator(OPERATOR_CAPABILITY_ROTATED);
  await loadStatic();
  await waitFor("rotated operator catalog read", async () => (await staticStatus()) === "loaded");
  check("operator capability rotation preserves service identity", rotated?.status.includes("generation 2") &&
    (await staticStatus()) === "loaded");

  await configureOperator("mgmt_invalid_operator_capability");
  await loadStatic();
  await waitFor("unauthorized operator failure", async () => (await staticStatus()) === "unavailable");
  check("invalid operator authority fails closed", (await evaluate(
    `document.querySelector('[data-view=authoring-configuration] [data-role=catalog-result]')?.textContent ?? ""`))
    .includes("returned 404"));

  await configureOperator(OPERATOR_CAPABILITY_ROTATED, 150);
  await sleep(250);
  await loadStatic();
  await waitFor("expired operator failure", async () => (await staticStatus()) === "unavailable");
  check("client-side operator expiry revokes reads before fetch", (await evaluate(
    `document.querySelector('[data-view=management-operator] [data-role=status]')?.textContent ?? ""`)) ===
    "Not configured" && (await evaluate(
      `document.querySelector('[data-view=authoring-configuration] [data-role=catalog-result]')?.textContent ?? ""`))
      .includes("unavailable"));

  await configureOperator(OPERATOR_CAPABILITY_ROTATED);
  await loadStatic();
  await waitFor("operator recovery", async () => (await staticStatus()) === "loaded");
  const managementMarkup = await evaluate(`document.documentElement.outerHTML`);
  const managementManifest = await evaluate(`JSON.stringify(window.__openrealtime.manifest)`);
  const managementLive = await evaluate(`JSON.stringify(window.__openrealtime.live())`);
  check("operator bearers never enter DOM, manifest, browser logs, or serialized live state",
    ![OPERATOR_CAPABILITY, OPERATOR_CAPABILITY_ROTATED, "mgmt_invalid_operator_capability"].some(
      (secret) => managementMarkup.includes(secret) || managementManifest.includes(secret) ||
        managementLive.includes(secret) || browserConsole.some((row) => row.includes(secret))));

  const retainedWorkspacePrepared = await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-editor]');
    const path = view?.querySelector('input[name=authoring-path]');
    const source = view?.querySelector('textarea[name=authoring-source]');
    const compile = view?.querySelector('button[data-action=compile]');
    if (!path || !source || !compile) return false;
    path.value = ${JSON.stringify(RETAINED_WORKSPACE_PATH)};
    source.value = ${JSON.stringify(AUTHORING_SOURCE)};
    compile.click();
    return true;
  })()`);
  check("authoring workspace prepared durable suspension state", retainedWorkspacePrepared && await waitFor(
    "durable workspace compile", () => evaluate(
      `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "compiled"`)));

  const authorityLossStarted = performance.now();
  const authorityLoss = await evaluate(`window.__openrealtime.deactivate("management-operator")`);
  const authorityLossMS = performance.now() - authorityLossStarted;
  check("operator provider loss disposes the complete authoring subtree",
    authorityLoss.entries["management-operator"].state === "inactive" &&
    ["management-transport", "management-static", "management-authoring",
      ...(EFFECTS_ENABLED ? ["management-source-reading", "management-source-publication"] : []),
      "authoring-workspace",
      "management-operator-view", "authoring-editor-view", "authoring-configuration-view",
      "authoring-canvas-view"].every((entry) => authorityLoss.entries[entry].state === "pending") &&
    !(await evaluate(`document.querySelector('[data-view^=authoring]') !== null`)) &&
    !JSON.stringify(authorityLoss).includes(RETAINED_WORKSPACE_PATH));
  check(`realtime and inspection${EFFECTS_ENABLED ? ", plus signed effects," : ""} survive operator provider loss`,
    authorityLoss.entries.transport.state === "active" && authorityLoss.entries.inspection.state === "active" &&
    (EFFECTS_ENABLED ? authorityLoss.entries.effects?.state === "active" : authorityLoss.entries.effects === undefined));
  const authorityRestoreStarted = performance.now();
  const authorityRestore = await evaluate(`window.__openrealtime.activate("management-operator")`);
  const authorityRestoreMS = performance.now() - authorityRestoreStarted;
  const recoveredOperatorStatus = await evaluate(
    `document.querySelector('[data-view=management-operator] [data-role=status]')?.textContent ?? ""`);
  await loadStatic();
  await waitFor("restored operator catalog read", async () => (await staticStatus()) === "loaded");
  check("operator provider recovery remounts every desired renderer",
    ["management-operator", "management-transport", "management-static", "management-authoring",
      ...(EFFECTS_ENABLED ? ["management-source-reading", "management-source-publication"] : []),
      "authoring-workspace", "management-operator-view", "authoring-editor-view",
      "authoring-configuration-view", "authoring-canvas-view"]
      .every((entry) => authorityRestore.entries[entry].state === "active") &&
    recoveredOperatorStatus.startsWith("Configured · generation ") &&
    (await staticStatus()) === "loaded");
  const recoveredWorkspace = await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-editor]');
    return {
      path: view?.querySelector('input[name=authoring-path]')?.value ?? "",
      source: view?.querySelector('textarea[name=authoring-source]')?.value ?? "",
      status: view?.querySelector('[data-role=status]')?.textContent ?? "",
    };
  })()`);
  check("provider recovery restores the durable authoring document without derived state",
    recoveredWorkspace.path === RETAINED_WORKSPACE_PATH &&
    recoveredWorkspace.source === AUTHORING_SOURCE && recoveredWorkspace.status === "idle",
    JSON.stringify({path: recoveredWorkspace.path, status: recoveredWorkspace.status}));
  check("authoring and static management stay inside bounded UI latency",
    staticMS < 5000 && analyzeMS < 10000 && formatMS < 5000 && compileMS < 10000 &&
    renameMS < 10000 && renamedCompileMS < 10000 && renderMS < 5000 &&
    edgeRemovalMS < 10000 && edgeCompileMS < 10000 && edgeCreationMS < 10000 &&
    edgeCreatedCompileMS < 10000 && normalizedYAML.elapsed < 20000 && normalizedJSON.elapsed < 20000 &&
    authorityLossMS < 2000 && authorityRestoreMS < 2000,
    `static=${staticMS.toFixed(1)}ms analyze=${analyzeMS.toFixed(1)}ms format=${formatMS.toFixed(1)}ms ` +
      `compile=${compileMS.toFixed(1)}ms rename=${renameMS.toFixed(1)}ms ` +
      `renamed-compile=${renamedCompileMS.toFixed(1)}ms ` +
      `render=${renderMS.toFixed(1)}ms edge-remove=${edgeRemovalMS.toFixed(1)}ms ` +
      `edge-compile=${edgeCompileMS.toFixed(1)}ms edge-create=${edgeCreationMS.toFixed(1)}ms ` +
      `edge-created-compile=${edgeCreatedCompileMS.toFixed(1)}ms normalized-yaml=${normalizedYAML.elapsed.toFixed(1)}ms ` +
      `normalized-json=${normalizedJSON.elapsed.toFixed(1)}ms loss=${authorityLossMS.toFixed(1)}ms ` +
      `restore=${authorityRestoreMS.toFixed(1)}ms`);

  const lossStarted = performance.now();
  const afterLoss = await evaluate(`window.__openrealtime.deactivate("inspection")`);
  const lossMS = performance.now() - lossStarted;
  check("provider loss disposes its dependent view", afterLoss.entries.inspection.state === "inactive" &&
    afterLoss.entries["inspection-view"].state === "pending" && afterLoss.entries["trace-view"].state === "pending" &&
    !(await evaluate(
      `document.querySelector('[data-view=inspection]') !== null`)));
  const afterInspectionLossMounted = expectedMounted.filter((entry) =>
    !new Set(["inspection", "inspection-view", "trace-view"]).has(entry));
  check("unrelated conversation plugins survive provider loss", JSON.stringify(await evaluate(
    `window.__openrealtime.mounted`)) === JSON.stringify(afterInspectionLossMounted));
  const restoreStarted = performance.now();
  const afterRestore = await evaluate(`window.__openrealtime.activate("inspection")`);
  const restoreMS = performance.now() - restoreStarted;
  await waitFor("inspection remount", () => evaluate(
    `document.querySelector('[data-view=inspection] #availability')?.textContent === "live"`));
  check("provider recovery remounts the desired dependent", afterRestore.entries.inspection.state === "active" &&
    afterRestore.entries["inspection-view"].state === "active");
  check("payload-free lifecycle state cannot expose the capability", !JSON.stringify(afterRestore).includes("mgmt_"));

  let reducerReplacementSequence = 0;
  if (REDUCER_REPLACEMENT_PATH) {
    const reducerReplacement = await evaluate(`(async () => {
      const before = window.__openrealtime.live();
      const candidate = await fetch(${JSON.stringify(REDUCER_REPLACEMENT_PATH)}, {cache:"no-store"})
        .then((response) => response.json());
      const connectionBefore = document.getElementById("state")?.textContent ?? "";
      const receipt = await window.__openrealtime.replace("reducer", candidate);
      const after = window.__openrealtime.live();
      return {
        before, receipt, after, candidateFingerprint: candidate.fingerprint,
        candidatePlanFingerprint: candidate.plan.fingerprint,
        manifestFingerprint: window.__openrealtime.manifest.fingerprint,
        mounted: window.__openrealtime.mounted,
        connectionBefore,
        connectionAfter: document.getElementById("state")?.textContent ?? "",
        changed: Object.keys(after.entries).filter((entry) =>
          before.entries[entry].implementation !== after.entries[entry].implementation),
      };
    })()`);
    reducerReplacementSequence = reducerReplacement.after.sequence;
    const reducerTransfer = (reducerReplacement.receipt.state_transfers ?? []).find(
      (row) => row.entry === "reducer");
    const reducerWorkspaceTransfer = (reducerReplacement.receipt.state_transfers ?? []).find(
      (row) => row.entry === "authoring-workspace");
    check("stateful reducer replacement retains the live transport and protocol session",
      reducerReplacement.before.entries.transport.state === "active" &&
      reducerReplacement.after.entries.transport.state === "active" &&
      reducerReplacement.before.entries.transport.implementation ===
        reducerReplacement.after.entries.transport.implementation &&
      reducerReplacement.connectionBefore === "connected" &&
      reducerReplacement.connectionAfter === "connected" &&
      JSON.stringify(reducerReplacement.changed) === JSON.stringify(["reducer"]) &&
      reducerReplacement.after.entries.reducer.implementation === "browser-esm:reducer-v2.js");
    check("reducer replacement receipt proves exact private and durable state transfer",
      reducerReplacement.receipt.format_version === 3 &&
      reducerReplacement.receipt.entry === "reducer" &&
      reducerReplacement.receipt.before_implementation.implementation === "browser-esm:reducer.js" &&
      reducerReplacement.receipt.after_implementation.implementation === "browser-esm:reducer-v2.js" &&
      reducerReplacement.receipt.plan_fingerprint === reducerReplacement.before.fingerprint &&
      reducerReplacement.receipt.before_manifest_fingerprint ===
        reducerReplacement.before.manifest_fingerprint &&
      reducerReplacement.receipt.after_manifest_fingerprint === reducerReplacement.candidateFingerprint &&
      reducerReplacement.receipt.before_sequence === reducerReplacement.before.sequence &&
      reducerReplacement.receipt.after_sequence === reducerReplacement.after.sequence &&
      reducerReplacement.after.sequence === reducerReplacement.before.sequence + 1 &&
      reducerReplacement.candidatePlanFingerprint === reducerReplacement.before.fingerprint &&
      reducerReplacement.manifestFingerprint === reducerReplacement.candidateFingerprint &&
      reducerReplacement.receipt.state_transfers?.length === 2 &&
      reducerTransfer?.schema?.name === "presentation.client.reducer.state" &&
      reducerTransfer.schema.revision === 1 &&
      reducerTransfer.schema.digest ===
        "sha256:640ca5e7a3fcb2638dd114be3affa7035514eadcb037c77c323b32abad906f26" &&
      reducerTransfer.before_state_digest === reducerTransfer.after_state_digest &&
      reducerTransfer.migrator_implementation === "browser-esm:reducer-v2.js" &&
      reducerWorkspaceTransfer?.schema?.name === "presentation.client.authoring_workspace.state" &&
      reducerWorkspaceTransfer.schema.revision === 1 &&
      reducerWorkspaceTransfer.schema.digest ===
        "sha256:8dcc2b5181390a1a61b50a3bb07390326bc60e6839a22f9667e3d40247147b78" &&
      reducerWorkspaceTransfer.before_state_digest === reducerWorkspaceTransfer.after_state_digest &&
      reducerWorkspaceTransfer.migrator_implementation === "" &&
      !JSON.stringify(reducerReplacement.receipt).includes(RETAINED_WORKSPACE_PATH) &&
      !JSON.stringify(reducerReplacement.receipt).includes("mgmt_"));
    check("reducer replacement remounts every dependent against restored state",
      JSON.stringify(reducerReplacement.mounted) === JSON.stringify(expectedMounted) &&
      Object.entries(reducerReplacement.after.entries).every(([, entry]) =>
        entry.desired && entry.state === "active") &&
      await waitFor("reducer replacement inspection", () => evaluate(
        `document.querySelector('[data-view=inspection] #availability')?.textContent === "live"`)));
    const reducerMarkup = await evaluate(`document.documentElement.outerHTML`);
    check("reducer migration keeps session inspection authority private",
      ![OPERATOR_CAPABILITY, OPERATOR_CAPABILITY_ROTATED, "mgmt_invalid_operator_capability"].some(
        (secret) => JSON.stringify(reducerReplacement).includes(secret) ||
          reducerMarkup.includes(secret) || browserConsole.some((row) => row.includes(secret))));
  }

  let capabilityReplacementSequence = 0;
  if (EFFECTS_REPLACEMENT_PATH) {
    await waitFor("initial effect negotiation", () => evaluate(
      `document.querySelector('[data-view=effect-confirmations] p')?.dataset.negotiated === "true"`));
    const replacement = await evaluate(`(async () => {
      const before = window.__openrealtime.live();
      const candidate = await fetch(${JSON.stringify(EFFECTS_REPLACEMENT_PATH)}, {cache:"no-store"})
        .then((response) => response.json());
      const receipt = await window.__openrealtime.replaceMany(
        ${JSON.stringify(Object.keys(SHIPPED_REPLACEMENT_IMPLEMENTATIONS))}, candidate);
      const after = window.__openrealtime.live();
      return {
        before, receipt, after, candidateFingerprint: candidate.fingerprint,
        candidatePlanFingerprint: candidate.plan.fingerprint,
        manifestFingerprint: window.__openrealtime.manifest.fingerprint,
        mounted: window.__openrealtime.mounted,
        connectionState: document.getElementById("state")?.textContent ?? "",
        changed: Object.keys(after.entries).filter((entry) =>
          before.entries[entry].implementation !== after.entries[entry].implementation),
      };
    })()`);
    const transitions = replacement.receipt.transitions ?? [];
    const transitionEntries = transitions.map((row) => row.entry).sort();
    const expectedReplacementEntries = Object.keys(SHIPPED_REPLACEMENT_IMPLEMENTATIONS).sort();
    capabilityReplacementSequence = replacement.after.sequence;
    check("atomic replacement selected every shipped capability and view candidate",
      replacement.after.sequence === replacement.before.sequence + 1 &&
      replacement.after.fingerprint === replacement.before.fingerprint &&
      replacement.candidatePlanFingerprint === replacement.before.fingerprint &&
      replacement.after.manifest_fingerprint === replacement.candidateFingerprint &&
      replacement.manifestFingerprint === replacement.candidateFingerprint &&
      JSON.stringify(replacement.changed.sort()) === JSON.stringify(expectedReplacementEntries) &&
      expectedReplacementEntries.every((entry) => replacement.after.entries[entry].implementation ===
        SHIPPED_REPLACEMENT_IMPLEMENTATIONS[entry]));
    const workspaceTransfer = (replacement.receipt.state_transfers ?? []).find(
      (row) => row.entry === "authoring-workspace");
    const operatorTransfer = (replacement.receipt.state_transfers ?? []).find(
      (row) => row.entry === "management-operator");
    const reducerTransfer = (replacement.receipt.state_transfers ?? []).find(
      (row) => row.entry === "reducer");
    check("shipped consumer replacement receipt is exact and payload-free",
      replacement.receipt.format_version === 3 &&
      replacement.receipt.plan_fingerprint === replacement.before.fingerprint &&
      replacement.receipt.before_manifest_fingerprint === replacement.before.manifest_fingerprint &&
      replacement.receipt.after_manifest_fingerprint === replacement.candidateFingerprint &&
      replacement.receipt.before_sequence === replacement.before.sequence &&
      replacement.receipt.after_sequence === replacement.after.sequence &&
      replacement.before.sequence === reducerReplacementSequence &&
      JSON.stringify(transitionEntries) === JSON.stringify(expectedReplacementEntries) &&
      transitions.every((row) => row.before_implementation.implementation ===
        SHIPPED_PREDECESSOR_IMPLEMENTATIONS[row.entry] &&
        row.after_implementation.implementation === SHIPPED_REPLACEMENT_IMPLEMENTATIONS[row.entry]) &&
      replacement.receipt.state_transfers?.length === 3 &&
      reducerTransfer?.schema?.name === "presentation.client.reducer.state" &&
      reducerTransfer.schema.revision === 1 &&
      reducerTransfer.schema.digest ===
        "sha256:640ca5e7a3fcb2638dd114be3affa7035514eadcb037c77c323b32abad906f26" &&
      reducerTransfer.before_state_digest === reducerTransfer.after_state_digest &&
      reducerTransfer.migrator_implementation === "" &&
      operatorTransfer?.schema?.name === "presentation.client.management_operator.state" &&
      operatorTransfer.schema.revision === 1 &&
      operatorTransfer.schema.digest ===
        "sha256:1f2f9868c1ea32703909c321437e2ac44980d6cb06968ab753a72627514934a1" &&
      operatorTransfer.before_state_digest === operatorTransfer.after_state_digest &&
      operatorTransfer.migrator_implementation ===
        SHIPPED_REPLACEMENT_IMPLEMENTATIONS["management-operator"] &&
      workspaceTransfer?.schema?.name === "presentation.client.authoring_workspace.state" &&
      workspaceTransfer.schema.revision === 1 &&
      workspaceTransfer.schema.digest ===
        "sha256:8dcc2b5181390a1a61b50a3bb07390326bc60e6839a22f9667e3d40247147b78" &&
      workspaceTransfer.before_state_digest === workspaceTransfer.after_state_digest &&
      workspaceTransfer.migrator_implementation ===
        SHIPPED_REPLACEMENT_IMPLEMENTATIONS["authoring-workspace"] &&
      !JSON.stringify(replacement.receipt).includes(RETAINED_WORKSPACE_PATH) &&
      ![OPERATOR_CAPABILITY, OPERATOR_CAPABILITY_ROTATED, "mgmt_invalid_operator_capability"].some(
        (secret) => JSON.stringify(replacement.receipt).includes(secret)));
    check("shipped consumer union closure remounted without disturbing unrelated plugins",
      JSON.stringify(replacement.mounted) === JSON.stringify(expectedMounted) &&
      expectedReplacementEntries.every((entry) => replacement.after.entries[entry].state === "active") &&
      Object.entries(replacement.after.entries).every(([, entry]) => entry.desired));
    check("replacement views rebound the retained live services and workspace",
      await evaluate(`(() => [
        "#connect", '[data-view="effect-confirmations"]', '[data-view="artifacts"]',
        '[data-view="inspection"]', '[data-view="trace"]', '[data-view="management-operator"]',
        '[data-view="authoring-editor"]', '[data-view="authoring-configuration"]',
        '[data-view="authoring-canvas"]',
      ].every((selector) => document.querySelector(selector) !== null))()`));
    check("replacement WebSocket transport retires the active session at its safe point",
      replacement.connectionState === "disconnected");
    await evaluate(`document.getElementById("connect").click()`);
    check("replacement WebSocket transport establishes a fresh protocol session", await waitFor(
      "replacement WebSocket connection", () => evaluate(
        `document.getElementById("state")?.textContent === "connected"`)));
    check("replacement inspection client rejoins the retained live session", await waitFor(
      "replacement inspection client", () => evaluate(
        `document.querySelector('[data-view=inspection] #availability')?.textContent === "live"`)));
    await loadStatic();
    await waitFor("replacement static management client", async () => (await staticStatus()) === "loaded");
    check("replacement static management client rebinds operator authority",
      (await staticStatus()) === "loaded");
    const restoredWorkspace = await evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-editor]');
      return {
        path: view?.querySelector('input[name=authoring-path]')?.value ?? "",
        source: view?.querySelector('textarea[name=authoring-source]')?.value ?? "",
        status: view?.querySelector('[data-role=status]')?.textContent ?? "",
      };
    })()`);
    check("replacement management clients preserve the durable authoring document",
      restoredWorkspace.path === RETAINED_WORKSPACE_PATH &&
      restoredWorkspace.source === AUTHORING_SOURCE && restoredWorkspace.status === "idle",
      JSON.stringify({path: restoredWorkspace.path, status: restoredWorkspace.status}));
    const replacementMarkup = await evaluate(`document.documentElement.outerHTML`);
    check("operator implementation replacement keeps the restored capability private",
      ![OPERATOR_CAPABILITY, OPERATOR_CAPABILITY_ROTATED, "mgmt_invalid_operator_capability"].some(
        (secret) => JSON.stringify(replacement).includes(secret) ||
          replacementMarkup.includes(secret) || browserConsole.some((row) => row.includes(secret))));
    await waitFor("replacement effect negotiation", () => evaluate(
      `document.querySelector('[data-view=effect-confirmations] p')?.dataset.negotiated === "true"`));
    check("replacement effect provider renegotiated its signed catalog",
      await evaluate(`document.querySelector('[data-view=effect-confirmations] p')?.dataset.providerPhase`) ===
        "ready" && await evaluate(
          `document.querySelector('[data-view=effect-confirmations] p')?.dataset.negotiated`) === "true");
  }

  await evaluate(`(() => {
    const input = document.getElementById("text"); input.value = "inspect this session";
    input.form.dispatchEvent(new Event("submit", {bubbles:true, cancelable:true}));
  })()`);
  await waitFor("assistant response", () => evaluate(
    `[...document.querySelectorAll("article[data-role=assistant]")].some((row) => row.textContent.length > 12)`));
  check("conversation remains independent of inspection", await evaluate(
    `[...document.querySelectorAll("article[data-role=user]")].some((row) => row.textContent.includes("inspect this session"))`));

  if (EFFECTS_ENABLED) {
    await waitFor("sealed artifact effect", () => evaluate(
      `document.querySelector('[data-view=artifacts] iframe') !== null`));
    await sleep(500);
    const artifact = await evaluate(`(async () => {
      const frame = document.querySelector('[data-view=artifacts] iframe');
      if (!frame) return null;
      const source = new URL(frame.src);
      const response = await fetch(source.href, { cache: "no-store" });
      const content = await response.arrayBuffer();
      const hash = [...new Uint8Array(await crypto.subtle.digest("SHA-256", content))]
        .map((value) => value.toString(16).padStart(2, "0")).join("");
      const stale = new URL(source.href);
      stale.searchParams.set("version", String(Number(source.searchParams.get("version")) + 1));
      const staleResponse = await fetch(stale.href, { cache: "no-store" });
      return {
        source: source.href,
        version: source.searchParams.get("version"),
        digest: source.searchParams.get("digest"),
        sandbox: frame.getAttribute("sandbox"),
        referrerPolicy: frame.referrerPolicy,
        status: response.status,
        mediaType: response.headers.get("content-type"),
        csp: response.headers.get("content-security-policy") ?? "",
        etag: response.headers.get("etag"),
        responseVersion: response.headers.get("x-openrealtime-artifact-version"),
        responseDigest: response.headers.get("x-openrealtime-artifact-digest"),
        computedDigest: "sha256:" + hash,
        content: new TextDecoder().decode(content),
        staleStatus: staleResponse.status,
        escaped: document.body.dataset.artifactEscaped ?? "",
        rendered: document.documentElement.outerHTML,
        manifest: JSON.stringify(window.__openrealtime.manifest),
      };
    })()`);
    check(`${EFFECTS_REPLACEMENT_PATH ? "replacement s" : "s"}erver-authorized client effect rendered an artifact`,
      artifact?.status === 200 &&
      artifact.content.includes("sealed browser artifact"), artifact?.source ?? "missing artifact");
    check("artifact view pins an exact immutable revision", artifact?.version === "1" &&
      /^sha256:[0-9a-f]{64}$/.test(artifact?.digest ?? "") && artifact.digest === artifact.responseDigest &&
      artifact.digest === artifact.computedDigest && artifact.responseVersion === artifact.version &&
      artifact.etag === `"${artifact.digest}"` && artifact.staleStatus === 412);
    check("artifact stays inside both browser and response sandboxes", artifact?.sandbox === "allow-scripts" &&
      artifact.referrerPolicy === "no-referrer" && artifact.mediaType === "text/html; charset=utf-8" &&
      artifact.csp.includes("sandbox allow-scripts") && artifact.csp.includes("default-src 'none'") &&
      artifact.csp.includes("connect-src 'none'") && !artifact.csp.includes("allow-same-origin") &&
      artifact.escaped === "");
    check("opaque effect authority is absent from UI, manifest, and browser logs",
      !artifact.rendered.includes("ore1.") && !artifact.manifest.includes("ore1.") &&
      !browserConsole.some((row) => row.includes("ore1.")));
    if (EFFECTS_REPLACEMENT_PATH) {
      const afterArtifact = await evaluate(`window.__openrealtime.live()`);
      check("replaced effects/artifact implementations remained active after real execution",
        afterArtifact.sequence === capabilityReplacementSequence &&
        afterArtifact.entries.effects.state === "active" &&
        afterArtifact.entries.effects.implementation === "browser-esm:effects-client-v2.js" &&
        afterArtifact.entries["artifact-references"].state === "active" &&
        afterArtifact.entries["artifact-references"].implementation ===
          "browser-esm:artifact-references-v2.js");
    }
  } else {
    check("observer conversation cannot surface an artifact or confirmation view", await evaluate(
      `document.querySelector('[data-view=artifacts], [data-view=confirmations]') === null`));
  }
  check("artifact navigation introduced no unexpected request failure", failedRequests.length === 0,
    failedRequests.join("; "));
  check("no runtime exception", exceptions.length === 0, exceptions.join("; "));

  const disposeStarted = performance.now();
  await evaluate(`window.__openrealtime.dispose()`);
  const disposeMS = performance.now() - disposeStarted;
  check("developer client lifecycle disposed", await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`) === "disposed");
  check("all developer view slots were cleared", await evaluate(
    `document.getElementById("openrealtime-root").childElementCount`) === 0);
  if (EFFECTS_REPLACEMENT_PATH) {
    const disposed = await evaluate(`({live:window.__openrealtime.live(),
      mounted:window.__openrealtime.mounted})`);
    check("replaced capability lifecycle released every browser-owned effect",
      disposed.live.state === "closed" && disposed.mounted.length === 0 &&
      Object.values(disposed.live.entries).every((entry) => entry.state === "inactive" && entry.effects === 0));
  }
  check("inspection lifecycle performance stays inside release ceilings",
    bootMS < 5000 && inspectMS < 10000 && lossMS < 2000 && restoreMS < 2000 && disposeMS < 2000,
    `boot=${bootMS.toFixed(1)}ms inspect=${inspectMS.toFixed(1)}ms loss=${lossMS.toFixed(1)}ms ` +
      `restore=${restoreMS.toFixed(1)}ms dispose=${disposeMS.toFixed(1)}ms`);
} catch (error) {
  check("run completed", false, error.stack ?? error.message);
} finally {
  chromium.kill("SIGKILL");
  // Chromium's children can still be writing into the profile when the parent
  // is killed, so removing it straight away races them and throws ENOTEMPTY -
  // which failed a CI run whose every check had already passed. Retry briefly,
  // and never let temp-directory cleanup decide the outcome of a test.
  try {
    rmSync(profile, { recursive: true, force: true, maxRetries: 50, retryDelay: 100 });
  } catch {}
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed`);
process.exit(failed.length ? 1 : 0);
