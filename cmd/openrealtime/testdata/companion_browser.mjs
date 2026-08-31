import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import {
  closeSync, constants, fstatSync, fsyncSync, mkdtempSync, openSync, rmSync, writeSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { isAbsolute, join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const pageURL = process.argv[2];
const snapshotPath = process.env.OPENREALTIME_COMPANION_BROWSER_SNAPSHOT ?? "";
const port = Number(process.env.CDP_PORT ?? 19888);
const profileParent = process.env.OPENREALTIME_COMPANION_BROWSER_PROFILE_PARENT ?? tmpdir();
if (!isAbsolute(profileParent) || profileParent.includes("\0") ||
    profileParent.includes("\r") || profileParent.includes("\n")) {
  throw new Error("browser profile parent is not canonical");
}
const profile = mkdtempSync(join(profileParent, "openrealtime-companion-command-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new", `--remote-debugging-port=${port}`, "--no-sandbox", "--disable-gpu",
  "--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream",
  "--autoplay-policy=no-user-gesture-required", `--user-data-dir=${profile}`, "about:blank",
], { stdio: ["ignore", "pipe", "pipe"] });
let chromiumErrors = "";
chromium.stderr.on("data", (chunk) => { chromiumErrors += chunk.toString(); });

async function debuggerEndpoint() {
  for (let attempt = 0; attempt < 120; attempt++) {
    try {
      const response = await fetch(`http://127.0.0.1:${port}/json/version`);
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
        frame.error ? pending.reject(new Error(JSON.stringify(frame.error))) : pending.resolve(frame.result);
      } else if (frame.method) {
        for (const handler of client.#handlers.get(frame.method) ?? []) {
          handler(frame.params, frame.sessionId);
        }
      }
    });
    return client;
  }
  on(method, handler) {
    const handlers = this.#handlers.get(method) ?? [];
    handlers.push(handler); this.#handlers.set(method, handlers);
  }
  send(method, params = {}, sessionId) {
    const id = this.#next++;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject });
      this.#socket.send(JSON.stringify({ id, method, params, sessionId }));
    });
  }
}

const checks = [];
function check(name, ok, detail = "") {
  checks.push({ name, ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? ` — ${detail}` : ""}`);
}

function writeExclusive(path, bytes) {
  if (!isAbsolute(path) || path.includes("\0") || path.includes("\r") || path.includes("\n")) {
    throw new Error("browser management snapshot path is not canonical");
  }
  const descriptor = openSync(path, constants.O_WRONLY | constants.O_CREAT |
    constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  try {
    let offset = 0;
    while (offset < bytes.byteLength) {
      const written = writeSync(descriptor, bytes, offset, bytes.byteLength - offset);
      if (written <= 0) throw new Error("browser management snapshot write made no progress");
      offset += written;
    }
    fsyncSync(descriptor);
    const info = fstatSync(descriptor);
    if (!info.isFile() || info.nlink !== 1 || (info.mode & 0o777) !== 0o600 ||
        info.size !== bytes.byteLength) {
      throw new Error("browser management snapshot is not one exact private file");
    }
  } finally {
    closeSync(descriptor);
  }
}

try {
  const browser = await CDP.connect(await debuggerEndpoint());
  const { targetId } = await browser.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await browser.send("Target.attachToTarget", { targetId, flatten: true });
  const call = (method, params) => browser.send(method, params, sessionId);
  const exceptions = []; const failures = [];
  const managementResponses = new Map();
  const managementFailures = [];
  browser.on("Runtime.exceptionThrown", (event, eventSessionID) => {
    if (eventSessionID === sessionId) exceptions.push(
      event.exceptionDetails.exception?.description ?? event.exceptionDetails.text);
  });
  browser.on("Network.loadingFailed", (event, eventSessionID) => {
    if (eventSessionID !== sessionId) return;
    if (managementResponses.has(event.requestId)) {
      managementFailures.push(event.errorText ?? "management response load failed");
    } else if (!event.canceled) failures.push(event.errorText);
  });
  await call("Runtime.enable"); await call("Network.enable"); await call("Page.enable");
  const pageOrigin = new URL(pageURL).origin;
  browser.on("Network.responseReceived", (event, eventSessionID) => {
    if (eventSessionID !== sessionId) return;
    try {
      const url = new URL(event.response.url);
      if (url.origin !== pageOrigin || event.response.status !== 200 ||
          event.response.mimeType !== "application/json" || url.search || url.hash ||
          !/^\/client\/v1\/management\/sessions\/[A-Za-z0-9._%:-]+\/live$/.test(url.pathname)) return;
      managementResponses.set(event.requestId, { url: url.href, bytes: null, complete: false });
    } catch (error) {
      managementFailures.push(error?.message ?? String(error));
    }
  });
  browser.on("Network.loadingFinished", (event, eventSessionID) => {
    if (eventSessionID !== sessionId) return;
    const response = managementResponses.get(event.requestId);
    if (!response) return;
    call("Network.getResponseBody", { requestId: event.requestId }).then((body) => {
      response.bytes = body.base64Encoded
        ? Buffer.from(body.body, "base64") : Buffer.from(body.body, "utf8");
      response.complete = true;
    }).catch((error) => managementFailures.push(error?.message ?? String(error)));
  });
  await call("Page.navigate", { url: pageURL });
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
    console.error(`timed out waiting for ${name}`);
    return false;
  };

  const booted = await waitFor("descriptor-locked boot", () => evaluate(
    `document.getElementById("openrealtime-root")?.dataset.state === "ready"`));
  check("descriptor-locked browser profile booted", booted);
  check("exact observer WebRTC plugin population mounted", JSON.stringify(await evaluate(
    `window.__openrealtime?.mounted`)) === JSON.stringify([
      "slots", "media", "transport", "reducer", "session-configuration", "video", "debug-session",
      "inspection", "view", "video-controls", "transport-diagnostics", "inspection-view", "trace-view",
      "management-operator", "management-transport", "management-static", "management-authoring",
      "authoring-workspace", "management-operator-view", "authoring-editor-view",
      "authoring-configuration-view", "authoring-canvas-view",
    ]));
  check("manifest selects only the WebRTC realtime route", await evaluate(`(() => {
    const endpoints = window.__openrealtime?.manifest?.endpoints ?? [];
    return endpoints.some((entry) => entry.name === "realtime.webrtc" &&
      entry.method === "POST" && entry.path === "/client/v1/realtime/calls") &&
      !endpoints.some((entry) => entry.name === "realtime.websocket");
  })()`));
  await evaluate(`document.getElementById("connect").click()`);
  const connected = await waitFor("WebRTC connection", () => evaluate(
    `document.getElementById("state")?.textContent === "connected"`));
  check("browser connected through real WebRTC relay", connected,
    await evaluate(`document.getElementById("error")?.textContent ?? ""`));
  check("transport diagnostics are live", await waitFor("transport diagnostics", () => evaluate(
    `document.getElementById("transport-stats")?.dataset.audioBytes !== undefined`)));
  check("negotiated session inspection uses the same server", await waitFor("inspection", () => evaluate(`(() => {
    const view = document.querySelector('[data-view=inspection]');
    return view?.querySelector('#availability')?.textContent === "live" &&
      (view?.querySelector('#identity')?.textContent ?? "").includes("sha256:");
  })()`)));
  const browserSessionID = await evaluate(
    `document.querySelector('[data-view=inspection]')?.dataset.sessionId ?? ""`);
  check("browser exposes its negotiated session identity", browserSessionID.startsWith("sess_"));
  const expectedManagementPath = `/client/v1/management/sessions/${encodeURIComponent(browserSessionID)}/live`;
  const captured = await waitFor("exact browser management response", async () =>
    [...managementResponses.values()].some((value) =>
      value.complete && new URL(value.url).pathname === expectedManagementPath));
  const managementEvidence = [...managementResponses.values()].find((value) =>
    value.complete && new URL(value.url).pathname === expectedManagementPath);
  check("browser captured its exact authenticated management response",
    captured && managementEvidence?.bytes?.byteLength > 0 && managementFailures.length === 0,
    managementFailures.join("; "));
  if (!managementEvidence?.bytes?.byteLength) {
    throw new Error("browser did not retain an exact management response body");
  }
  writeExclusive(snapshotPath, managementEvidence.bytes);
  const managementDigest = `sha256:${createHash("sha256").update(managementEvidence.bytes).digest("hex")}`;
  console.log(`OPENREALTIME_COMPANION_BROWSER_PROOF ${JSON.stringify({
    schema: "openrealtime/browser/hosted-companion-proof/v3",
    nonce: process.env.OPENREALTIME_COMPANION_PROOF_NONCE ?? "",
    session_id: browserSessionID,
    transport: "webrtc",
    manifest_fingerprint: await evaluate(`window.__openrealtime.manifest.fingerprint`),
    plan_fingerprint: await evaluate(`window.__openrealtime.manifest.plan.fingerprint`),
    endpoint: pageURL,
    management: {
      session_id: browserSessionID,
      resource: "live",
      url: managementEvidence.url,
      payload_bytes: managementEvidence.bytes.byteLength,
      payload_digest: managementDigest,
    },
  })}`);
  check("no browser exception", exceptions.length === 0, exceptions.join("; "));
  check("no uncancelled network failure", failures.length === 0, failures.join("; "));
  await evaluate(`window.__openrealtime.dispose()`);
  check("browser plugin graph disposed", await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`) === "disposed" &&
    (await evaluate(`window.__openrealtime.mounted.length`)) === 0);
} catch (error) {
  check("command browser run completed", false, error.stack ?? error.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = checks.filter((entry) => !entry.ok);
console.log(`\n${checks.length - failed.length}/${checks.length} checks passed`);
process.exit(failed.length === 0 ? 0 : 1);
