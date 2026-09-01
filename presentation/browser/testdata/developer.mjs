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
const STATIC_GRAPH_FINGERPRINT = process.env.STATIC_GRAPH_FINGERPRINT ?? "";
const EFFECTS_ENABLED = (process.env.EXPECT_EFFECTS ?? "1") === "1";
const CLIENT_TRANSPORT = process.env.CLIENT_TRANSPORT ?? "websocket";
if (!new Set(["websocket", "webrtc"]).has(CLIENT_TRANSPORT)) {
  throw new Error("developer client transport fixture is invalid");
}
if (!OPERATOR_CAPABILITY || !OPERATOR_CAPABILITY_ROTATED || !AUTHORING_SOURCE ||
    !/^sha256:[0-9a-f]{64}$/.test(STATIC_GRAPH_FINGERPRINT)) {
  throw new Error("developer management E2E fixture is incomplete");
}
const profile = mkdtempSync(join(tmpdir(), "openrealtime-developer-client-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
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
      "video-controls", "transport-diagnostics", "inspection-view", "trace-view",
    ] : [
      "slots", "transport", "reducer", "session-configuration", "debug-session",
      ...(EFFECTS_ENABLED ? ["effects", "artifact-references"] : []), "inspection", "view",
      ...(EFFECTS_ENABLED ? ["confirmation-view", "artifact-view"] : []),
      "inspection-view", "trace-view",
    ];
  expectedMounted.push(
      "management-operator", "management-transport", "management-static", "management-authoring",
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
        const forbidden = ["effects", "artifact-references", "confirmation-view", "artifact-view"];
        const services = Object.values(live.entries).flatMap((entry) => entry.services ?? []);
        return !forbidden.some((name) =>
          Object.hasOwn(live.entries, name) || window.__openrealtime.mounted.includes(name)) &&
          !(manifest.grants ?? []).some((grant) => forbidden.includes(grant.entry)) &&
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
    view.querySelector('textarea[name=authoring-source]').value = ${JSON.stringify(AUTHORING_SOURCE)};
    view.querySelector('button[data-action=analyze]').click();
  })()`);
  await waitFor("authoring analysis", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "analyzed"`));
  const analyzeMS = performance.now() - analyzeStarted;
  const configurationMetadata = await evaluate(
    `document.querySelector('[data-view=authoring-configuration]')?.textContent ?? ""`);
  check("configuration renderer projects server-validated descriptor metadata",
    configurationMetadata.includes("compat.BindingRuntime") && configurationMetadata.includes("schema://"));

  const compileStarted = performance.now();
  await evaluate(
    `document.querySelector('[data-view=authoring-editor] button[data-action=compile]').click()`);
  await waitFor("authoring compile", () => evaluate(
    `document.querySelector('[data-view=authoring-editor] [data-role=status]')?.textContent === "compiled"`));
  const compileMS = performance.now() - compileStarted;
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
      view?.querySelector('[data-role=rendering]')?.textContent.includes('"reaction"');
  })()`));

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

  const authorityLossStarted = performance.now();
  const authorityLoss = await evaluate(`window.__openrealtime.deactivate("management-operator")`);
  const authorityLossMS = performance.now() - authorityLossStarted;
  check("operator provider loss disposes the complete authoring subtree",
    authorityLoss.entries["management-operator"].state === "inactive" &&
    ["management-transport", "management-static", "management-authoring", "authoring-workspace",
      "management-operator-view", "authoring-editor-view", "authoring-configuration-view",
      "authoring-canvas-view"].every((entry) => authorityLoss.entries[entry].state === "pending") &&
    !(await evaluate(`document.querySelector('[data-view^=authoring]') !== null`)));
  check(`realtime and inspection${EFFECTS_ENABLED ? ", plus signed effects," : ""} survive operator provider loss`,
    authorityLoss.entries.transport.state === "active" && authorityLoss.entries.inspection.state === "active" &&
    (EFFECTS_ENABLED ? authorityLoss.entries.effects?.state === "active" : authorityLoss.entries.effects === undefined));
  const authorityRestoreStarted = performance.now();
  const authorityRestore = await evaluate(`window.__openrealtime.activate("management-operator")`);
  const authorityRestoreMS = performance.now() - authorityRestoreStarted;
  await configureOperator(OPERATOR_CAPABILITY_ROTATED);
  check("operator provider recovery remounts every desired renderer",
    ["management-operator", "management-transport", "management-static", "management-authoring",
      "authoring-workspace", "management-operator-view", "authoring-editor-view",
      "authoring-configuration-view", "authoring-canvas-view"]
      .every((entry) => authorityRestore.entries[entry].state === "active"));
  check("authoring and static management stay inside bounded UI latency",
    staticMS < 5000 && analyzeMS < 10000 && compileMS < 10000 && renderMS < 5000 &&
    authorityLossMS < 2000 && authorityRestoreMS < 2000,
    `static=${staticMS.toFixed(1)}ms analyze=${analyzeMS.toFixed(1)}ms compile=${compileMS.toFixed(1)}ms ` +
      `render=${renderMS.toFixed(1)}ms loss=${authorityLossMS.toFixed(1)}ms ` +
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
    check("server-authorized client effect rendered an artifact", artifact?.status === 200 &&
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
  check("inspection lifecycle performance stays inside release ceilings",
    bootMS < 5000 && inspectMS < 10000 && lossMS < 2000 && restoreMS < 2000 && disposeMS < 2000,
    `boot=${bootMS.toFixed(1)}ms inspect=${inspectMS.toFixed(1)}ms loss=${lossMS.toFixed(1)}ms ` +
      `restore=${restoreMS.toFixed(1)}ms dispose=${disposeMS.toFixed(1)}ms`);
} catch (error) {
  check("run completed", false, error.stack ?? error.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed`);
process.exit(failed.length ? 1 : 0);
