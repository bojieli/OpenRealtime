import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const PAGE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19312);
const CLIENT_REPLACEMENT_PATH = process.env.CLIENT_REPLACEMENT_PATH ?? "";
const RETAINED_WORKSPACE_PATH = "webrtc-replacement-retained.ortg";
const RETAINED_WORKSPACE_SOURCE = "graph webrtc_replacement_retained {\n}\n";
const REPLACEMENT_IMPLEMENTATIONS = Object.freeze({
  "slots": "browser-esm:slots-v2.js",
  "media": "browser-esm:media-webrtc-v2.js",
  "transport": "browser-esm:transport-webrtc-v2.js",
  "session-configuration": "browser-esm:session-configuration-v2.js",
  "video": "browser-esm:video-protocol-v2.js",
  "debug-session": "browser-esm:debug-session-v2.js",
  "effects": "browser-esm:effects-client-v2.js",
  "artifact-references": "browser-esm:artifact-references-v2.js",
  "inspection": "browser-esm:inspection-client-v2.js",
  "video-controls": "browser-esm:video-controls-v2.js",
  "transport-diagnostics": "browser-esm:transport-diagnostics-view-v2.js",
});
const PREDECESSOR_IMPLEMENTATIONS = Object.freeze({
  "slots": "browser-esm:slots.js",
  "media": "browser-esm:media-webrtc.js",
  "transport": "browser-esm:transport-webrtc.js",
  "session-configuration": "browser-esm:session-configuration.js",
  "video": "browser-esm:video-protocol.js",
  "debug-session": "browser-esm:debug-session.js",
  "effects": "browser-esm:effects-client.js",
  "artifact-references": "browser-esm:artifact-references.js",
  "inspection": "browser-esm:inspection-client.js",
  "video-controls": "browser-esm:video-controls.js",
  "transport-diagnostics": "browser-esm:transport-diagnostics-view.js",
});
if (!CLIENT_REPLACEMENT_PATH.startsWith("/test/")) {
  throw new Error("WebRTC client replacement fixture is invalid");
}
const profile = mkdtempSync(join(tmpdir(), "openrealtime-webrtc-client-"));
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
  "--autoplay-policy=no-user-gesture-required", `--user-data-dir=${profile}`, "about:blank",
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
        client.#pending.delete(frame.id); clearTimeout(pending.timer);
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
        this.#pending.delete(id); reject(new Error(`CDP ${method} timed out`));
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
    console.log(`timed out waiting for ${name}`); return false;
  };
  const waitForValue = async (name, read, timeout = 30000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      const value = await read();
      if (value) return value;
      await sleep(100);
    }
    console.log(`timed out waiting for ${name}`); return "";
  };

  await waitFor("WebRTC profile boot", () => evaluate(
    `document.getElementById("openrealtime-root")?.dataset.state === "ready"`));
  const bootMS = performance.now() - navigationStarted;
  check("locked WebRTC developer client booted", await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`) === "ready");
  check("twenty-eight replaceable plugins mounted", JSON.stringify(await evaluate(
    `window.__openrealtime?.mounted`)) === JSON.stringify([
      "slots", "media", "transport", "reducer", "session-configuration", "video", "debug-session",
      "effects", "artifact-references", "inspection", "view", "confirmation-view", "artifact-view",
      "video-controls", "transport-diagnostics", "inspection-view", "trace-view",
      "management-operator", "management-transport", "management-static", "management-authoring",
      "management-source-reading", "management-source-publication",
      "authoring-workspace", "management-operator-view", "authoring-editor-view",
      "authoring-configuration-view", "authoring-canvas-view",
    ]));
  check("no module request failed", failedRequests.length === 0, failedRequests.join("; "));

  const connectStarted = performance.now();
  await evaluate(`document.getElementById("connect").click()`);
  const connected = await waitFor("WebRTC connection", () => evaluate(
    `document.getElementById("state")?.textContent === "connected"`));
  const connectionDetail = await evaluate(`JSON.stringify({
    state: document.getElementById("state")?.textContent,
    error: document.getElementById("error")?.textContent,
    live: window.__openrealtime.live(),
  })`);
  check("shared reducer connected over WebRTC", connected, connectionDetail);
  if (!connected) throw new Error(`WebRTC did not connect: ${connectionDetail}`);
  const connectMS = performance.now() - connectStarted;
  const inspectionIdentity = await waitForValue("WebRTC graph inspection", () => evaluate(`(() => {
    const view = document.querySelector('[data-view=inspection]');
    const exact = view?.querySelector('#identity')?.textContent ?? "";
    return view?.querySelector('#availability')?.textContent === "live" &&
      exact.includes("sha256:") ? exact : "";
  })()`));
  check("WebRTC session uses the same management API", inspectionIdentity.includes("sha256:"), inspectionIdentity);
  const traceIdentity = await waitForValue("WebRTC causal trace", () => evaluate(`(() => {
    const view = document.querySelector('[data-view=trace]');
    const exact = view?.querySelector('#trace-identity')?.textContent ?? "";
    return view?.querySelector('#trace-state')?.textContent.startsWith("live") &&
      exact.includes("sha256:") ? exact : "";
  })()`));
  check("WebRTC profile renders the same causal trace API", traceIdentity.includes("sha256:"), traceIdentity);

  await waitFor("negotiated video input", () => evaluate(
    `document.getElementById("video-camera")?.disabled === false`));
  const videoStarted = performance.now();
  await evaluate(`document.getElementById("video-camera").click()`);
  const captured = await waitFor("bounded camera frame publication", () => evaluate(
    `Number(document.getElementById("camera-preview")?.dataset.frames) > 0`));
  const firstVideoMS = performance.now() - videoStarted;
  check("camera is a replaceable negotiated media/protocol composition", captured,
    await evaluate(`document.getElementById("video-state")?.textContent ?? ""`));
  await evaluate(`document.getElementById("video-stop").click()`);
  await waitFor("camera scope disposal", () => evaluate(
    `document.getElementById("camera-preview")?.srcObject === null`));
  check("camera stop disposes capture state", await evaluate(`document.getElementById("camera-preview")?.srcObject === null`));
  const screenStarted = performance.now();
  await evaluate(`document.getElementById("video-screen").click()`);
  const screenCaptured = await waitFor("bounded screen frame publication", () => evaluate(
    `Number(document.getElementById("screen-preview")?.dataset.frames) > 0`), 5000);
  const firstScreenMS = performance.now() - screenStarted;
  check("screen is a replaceable negotiated media/protocol composition", screenCaptured,
    await evaluate(`document.getElementById("video-state")?.textContent ?? ""`));
  await evaluate(`document.getElementById("video-stop").click()`);

  const workspacePrepared = await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-editor]');
    const path = view?.querySelector('input[name=authoring-path]');
    const source = view?.querySelector('textarea[name=authoring-source]');
    const analyze = view?.querySelector('button[data-action=analyze]');
    if (!path || !source || !analyze) return false;
    path.value = ${JSON.stringify(RETAINED_WORKSPACE_PATH)};
    source.value = ${JSON.stringify(RETAINED_WORKSPACE_SOURCE)};
    analyze.click();
    return true;
  })()`);
  check("WebRTC workspace prepared durable transport-replacement state", workspacePrepared && await waitFor(
    "WebRTC workspace commit", () => evaluate(`(() => {
      const view = document.querySelector('[data-view=authoring-editor]');
      const status = view?.querySelector('[data-role=status]')?.textContent ?? "";
      return view?.querySelector('input[name=authoring-path]')?.value ===
          ${JSON.stringify(RETAINED_WORKSPACE_PATH)} && status !== "analyzing";
    })()`)));

  const replacementStarted = performance.now();
  const clientReplacement = await evaluate(`(async () => {
    const before = window.__openrealtime.live();
    const candidate = await fetch(${JSON.stringify(CLIENT_REPLACEMENT_PATH)}, {cache:"no-store"})
      .then((response) => response.json());
    const receipt = await window.__openrealtime.replaceMany(
      ${JSON.stringify(Object.keys(REPLACEMENT_IMPLEMENTATIONS))}, candidate);
    const after = window.__openrealtime.live();
    return {
      before, after, receipt, candidateFingerprint: candidate.fingerprint,
      manifestFingerprint: window.__openrealtime.manifest.fingerprint,
      mounted: window.__openrealtime.mounted,
      connectionState: document.getElementById("state")?.textContent ?? "",
      changed: Object.keys(after.entries).filter((entry) =>
        before.entries[entry].implementation !== after.entries[entry].implementation),
    };
  })()`);
  const replacementMS = performance.now() - replacementStarted;
  const transitions = clientReplacement.receipt.transitions ?? [];
  const replacementEntries = Object.keys(REPLACEMENT_IMPLEMENTATIONS).sort();
  const workspaceTransfer = (clientReplacement.receipt.state_transfers ?? []).find(
    (row) => row.entry === "authoring-workspace");
  const reducerTransfer = (clientReplacement.receipt.state_transfers ?? []).find(
    (row) => row.entry === "reducer");
  check("WebRTC reconstructible client implementations replace atomically",
    clientReplacement.after.sequence === clientReplacement.before.sequence + 1 &&
    clientReplacement.after.fingerprint === clientReplacement.before.fingerprint &&
    clientReplacement.after.manifest_fingerprint === clientReplacement.candidateFingerprint &&
    clientReplacement.manifestFingerprint === clientReplacement.candidateFingerprint &&
    clientReplacement.receipt.format_version === 3 &&
    clientReplacement.receipt.plan_fingerprint === clientReplacement.before.fingerprint &&
    clientReplacement.receipt.before_manifest_fingerprint === clientReplacement.before.manifest_fingerprint &&
    clientReplacement.receipt.after_manifest_fingerprint === clientReplacement.candidateFingerprint &&
    JSON.stringify(clientReplacement.changed.sort()) === JSON.stringify(replacementEntries) &&
    JSON.stringify(transitions.map((row) => row.entry).sort()) === JSON.stringify(replacementEntries) &&
    transitions.every((row) => row.before_implementation.implementation ===
      PREDECESSOR_IMPLEMENTATIONS[row.entry] &&
      row.after_implementation.implementation === REPLACEMENT_IMPLEMENTATIONS[row.entry]) &&
    clientReplacement.receipt.state_transfers?.length === 2 &&
    reducerTransfer?.schema?.name === "presentation.client.reducer.state" &&
    reducerTransfer.schema.revision === 1 &&
    reducerTransfer.schema.digest ===
      "sha256:640ca5e7a3fcb2638dd114be3affa7035514eadcb037c77c323b32abad906f26" &&
    reducerTransfer.before_state_digest === reducerTransfer.after_state_digest &&
    reducerTransfer.migrator_implementation === "" &&
    workspaceTransfer?.schema?.name === "presentation.client.authoring_workspace.state" &&
    workspaceTransfer.schema.revision === 1 &&
    workspaceTransfer.schema.digest ===
      "sha256:8dcc2b5181390a1a61b50a3bb07390326bc60e6839a22f9667e3d40247147b78" &&
    workspaceTransfer.before_state_digest === workspaceTransfer.after_state_digest &&
    workspaceTransfer.migrator_implementation === "" &&
    !JSON.stringify(clientReplacement.receipt).includes(RETAINED_WORKSPACE_PATH) &&
    !JSON.stringify(clientReplacement.receipt).includes(RETAINED_WORKSPACE_SOURCE) &&
    !JSON.stringify(clientReplacement.receipt).includes("mgmt_") &&
    !JSON.stringify(clientReplacement.receipt).includes("authority") &&
    replacementEntries.every((entry) => clientReplacement.after.entries[entry].state === "active") &&
    clientReplacement.mounted.length === 28);
  check("replacement slots reconstruct every WebRTC client surface", await evaluate(`(() => [
    "#connect", '[data-view="effect-confirmations"]', '[data-view="artifacts"]',
    '[data-view="inspection"]', '[data-view="trace"]', '[data-view="management-operator"]',
    '[data-view="authoring-editor"]', '[data-view="authoring-configuration"]',
    '[data-view="authoring-canvas"]', "#video-camera", "#transport-stats",
  ].every((selector) => document.querySelector(selector) !== null))()`));
  const restoredWorkspace = await evaluate(`(() => {
    const view = document.querySelector('[data-view=authoring-editor]');
    return {
      path: view?.querySelector('input[name=authoring-path]')?.value ?? "",
      source: view?.querySelector('textarea[name=authoring-source]')?.value ?? "",
      status: view?.querySelector('[data-role=status]')?.textContent ?? "",
    };
  })()`);
  check("WebRTC media/transport replacement preserves durable reducer and workspace state",
    clientReplacement.connectionState === "disconnected" &&
    restoredWorkspace.path === RETAINED_WORKSPACE_PATH &&
    restoredWorkspace.source === RETAINED_WORKSPACE_SOURCE && restoredWorkspace.status === "idle",
    JSON.stringify({connection: clientReplacement.connectionState,
      path: restoredWorkspace.path, status: restoredWorkspace.status}));
  await evaluate(`document.getElementById("connect").click()`);
  check("replacement WebRTC media and transport establish a fresh protocol session", await waitFor(
    "replacement WebRTC connection", () => evaluate(
      `document.getElementById("state")?.textContent === "connected"`)));
  check("replacement session configuration restores scoped inspection", await waitFor(
    "replacement WebRTC inspection", () => evaluate(
      `document.querySelector('[data-view=inspection] #availability')?.textContent === "live"`)));
  check("replacement session configuration renegotiates signed effects", await waitFor(
    "replacement WebRTC effect negotiation", () => evaluate(`(() => {
      const status = document.querySelector('[data-view=effect-confirmations] p');
      return status?.dataset.providerPhase === "ready" && status?.dataset.negotiated === "true";
    })()`)));
  check("replacement WebRTC views rebind live media and diagnostics services", await evaluate(`(() =>
    document.getElementById("video-camera")?.disabled === false &&
    document.getElementById("video-screen")?.disabled === false &&
    document.getElementById("transport-stats") !== null)()`));
  await evaluate(`document.getElementById("video-camera").click()`);
  const replacementCaptured = await waitFor("replacement camera frame publication", () => evaluate(
    `Number(document.getElementById("camera-preview")?.dataset.frames) > 0`));
  check("replacement video controls execute through the retained provider", replacementCaptured);
  await evaluate(`document.getElementById("video-stop").click()`);

  await waitFor("transport diagnostics", () => evaluate(
    `document.getElementById("transport-stats")?.dataset.audioBytes !== undefined`));
  const audioBefore = Number(await evaluate(
    `document.getElementById("transport-stats")?.dataset.audioBytes ?? "0"`));
  const turnStarted = performance.now();
  await evaluate(`(() => {
    const input = document.getElementById("text"); input.value = "answer over WebRTC";
    input.form.dispatchEvent(new Event("submit", {bubbles:true, cancelable:true}));
  })()`);
  const audioReceived = await waitFor("first received response audio", () => evaluate(
    `Number(document.getElementById("transport-stats")?.dataset.audioBytes ?? "0") > ${audioBefore}`), 5000);
  const firstAudioReceiptMS = performance.now() - turnStarted;
  check("typed turn receives response audio over WebRTC", audioReceived,
    `${firstAudioReceiptMS.toFixed(1)}ms to first observed inbound RTP bytes`);
  await waitFor("WebRTC assistant response", () => evaluate(
    `[...document.querySelectorAll("article[data-role=assistant]")].some((row) => row.textContent.length > 12)`));
  check("conversation crosses the WebRTC data channel", await evaluate(
    `[...document.querySelectorAll("article[data-role=user]")].some((row) => row.textContent.includes("answer over WebRTC"))`));
  await waitFor("WebRTC-authorized artifact effect", () => evaluate(
    `document.querySelector('[data-view=artifacts] iframe') !== null`));
  const firstArtifact = await evaluate(`(async () => {
    const frame = document.querySelector('[data-view=artifacts] iframe');
    if (!frame) return null;
    const source = new URL(frame.src);
    const response = await fetch(source.href, { cache: "no-store" });
    const content = await response.arrayBuffer();
    const hash = [...new Uint8Array(await crypto.subtle.digest("SHA-256", content))]
      .map((value) => value.toString(16).padStart(2, "0")).join("");
    return {
      source: source.href, status: response.status,
      version: source.searchParams.get("version"), digest: source.searchParams.get("digest"),
      responseDigest: response.headers.get("x-openrealtime-artifact-digest"),
      computedDigest: "sha256:" + hash, sandbox: frame.getAttribute("sandbox"),
      csp: response.headers.get("content-security-policy") ?? "",
      content: new TextDecoder().decode(content),
      rendered: document.documentElement.outerHTML,
      manifest: JSON.stringify(window.__openrealtime.manifest),
    };
  })()`);
  check("signed client effect crosses the WebRTC protocol session and host socket",
    firstArtifact?.status === 200 && firstArtifact.version === "1" &&
    firstArtifact.content.includes("sealed WebRTC artifact") &&
    firstArtifact.digest === firstArtifact.responseDigest &&
    firstArtifact.digest === firstArtifact.computedDigest, firstArtifact?.source ?? "missing artifact");
  check("WebRTC artifact is sandboxed without receipt disclosure",
    firstArtifact?.sandbox === "allow-scripts" && firstArtifact.csp.includes("sandbox allow-scripts") &&
    !firstArtifact.csp.includes("allow-same-origin") && !firstArtifact.rendered.includes("ore1.") &&
    !firstArtifact.manifest.includes("ore1.") && !browserConsole.some((row) => row.includes("ore1.")));

  const lossStarted = performance.now();
  const afterMediaLoss = await waitForValue("completed response safe point", () => evaluate(`(async () => {
    try {
      return await window.__openrealtime.deactivate("media");
    } catch (error) {
      if ((error?.message ?? String(error)).includes("in-flight response")) return null;
      throw error;
    }
  })()`));
  const lossMS = performance.now() - lossStarted;
  check("media loss cascades through transport and consumers", afterMediaLoss.entries.media.state === "inactive" &&
    afterMediaLoss.entries.transport.state === "pending" && afterMediaLoss.entries.reducer.state === "pending" &&
    afterMediaLoss.entries["management-transport"].state === "pending" &&
    afterMediaLoss.entries["management-operator"].state === "active" &&
    JSON.stringify(await evaluate(`window.__openrealtime.mounted`)) ===
      JSON.stringify(["slots", "management-operator", "management-operator-view"]));
  check("media loss removes every media, session, and authoring dependent view", await evaluate(`(() => {
    const root = document.getElementById("openrealtime-root");
    return root.childElementCount === 1 &&
      root.querySelector('[data-view=management-operator]') !== null &&
      root.querySelector('[data-view^=authoring]') === null;
  })()`));

  const restoreStarted = performance.now();
  const afterMediaRestore = await evaluate(`window.__openrealtime.activate("media")`);
  const restoreMS = performance.now() - restoreStarted;
  check("media recovery remounts the full desired composition", Object.values(afterMediaRestore.entries).every(
    (entry) => entry.state === "active") && (await evaluate(`window.__openrealtime.mounted.length`)) === 28);
  await evaluate(`document.getElementById("connect").click()`);
  await waitFor("WebRTC reconnection after provider recovery", () => evaluate(
    `document.getElementById("state")?.textContent === "connected"`));
  check("a replacement media/transport scope reconnects", await evaluate(
    `document.getElementById("state")?.textContent`) === "connected");
  const effectsNegotiated = await waitFor("replacement effect provider negotiation", () => evaluate(`(() => {
    const status = document.querySelector('[data-view=effect-confirmations] p');
    return status?.dataset.providerPhase === "ready" && status?.dataset.negotiated === "true";
  })()`));
  check("replacement effect provider re-handshakes and re-advertises its declarations", effectsNegotiated,
    await evaluate(`document.querySelector('[data-view=effect-confirmations] p')?.textContent ?? "missing"`));
  await evaluate(`(() => {
    const input = document.getElementById("text"); input.value = "run after provider recovery";
    input.form.dispatchEvent(new Event("submit", {bubbles:true, cancelable:true}));
  })()`);
  const revised = await waitFor("effect recovery after full provider cascade", () => evaluate(
    `new URL(document.querySelector('[data-view=artifacts] iframe')?.src ?? location.href)
      .searchParams.get("version") === "2"`));
  check("replacement reducer/effect/resource scopes execute without stale session state", revised,
    await evaluate(`document.querySelector('[data-view=artifacts] iframe')?.src ?? "missing"`));
  check("no runtime exception", exceptions.length === 0, exceptions.join("; "));

  const disposeStarted = performance.now();
  await evaluate(`window.__openrealtime.dispose()`);
  const disposeMS = performance.now() - disposeStarted;
  const finalLive = await evaluate(`window.__openrealtime.live()`);
  check("WebRTC client lifecycle disposed", finalLive.state === "closed" && (await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`)) === "disposed");
  check("WebRTC client left no mounted plugin", (await evaluate(
    `window.__openrealtime.mounted.length`)) === 0);
  check("WebRTC client released every scoped effect and service",
    Object.values(finalLive.entries).every((entry) => entry.state === "inactive" && !entry.desired &&
      entry.error === "" && entry.effects === 0 && entry.services.length === 0));
  check("client lifecycle performance stays inside release ceilings",
    bootMS < 5000 && connectMS < 15000 && firstVideoMS < 5000 && firstScreenMS < 5000 &&
      replacementMS < 2000 && firstAudioReceiptMS < 5000 &&
      lossMS < 2000 && restoreMS < 2000 && disposeMS < 2000,
    `boot=${bootMS.toFixed(1)}ms connect=${connectMS.toFixed(1)}ms first-video=${firstVideoMS.toFixed(1)}ms first-screen=${firstScreenMS.toFixed(1)}ms ` +
      `view-replacement=${replacementMS.toFixed(1)}ms first-audio-receipt=${firstAudioReceiptMS.toFixed(1)}ms loss=${lossMS.toFixed(1)}ms ` +
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
