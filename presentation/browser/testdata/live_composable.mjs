import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const PAGE_URL = process.argv[2];
const CHALLENGE = process.env.LIVE_PRESENTATION_CHALLENGE;
const PORT = Number(process.env.CDP_PORT ?? 19311);
if (!PAGE_URL || !CHALLENGE) {
  console.error("usage: LIVE_PRESENTATION_CHALLENGE=... node live_composable.mjs <presentation URL>");
  process.exit(2);
}

const profile = mkdtempSync(join(tmpdir(), "openrealtime-live-presentation-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new", `--remote-debugging-port=${PORT}`, "--no-sandbox", "--disable-gpu",
  `--user-data-dir=${profile}`, "about:blank",
], { stdio: ["ignore", "pipe", "pipe"] });
let chromiumErrors = "";
chromium.stderr.on("data", (chunk) => { chromiumErrors += chunk.toString(); });

async function debuggerEndpoint() {
  for (let attempt = 0; attempt < 120; attempt++) {
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
        const pending = client.#pending.get(frame.id); client.#pending.delete(frame.id);
        frame.error ? pending.reject(new Error(JSON.stringify(frame.error))) : pending.resolve(frame.result);
      } else if (frame.method) {
        for (const handler of client.#handlers.get(frame.method) ?? []) handler(frame.params);
      }
    });
    return client;
  }
  on(method, handler) {
    const handlers = this.#handlers.get(method) ?? []; handlers.push(handler); this.#handlers.set(method, handlers);
  }
  send(method, params = {}, sessionId) {
    const id = this.#next++;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject });
      this.#socket.send(JSON.stringify({ id, method, params, sessionId }));
    });
  }
}

const results = [];
const check = (name, ok, detail = "") => {
  results.push({ name, ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? ` — ${detail}` : ""}`);
};

try {
  const browser = await CDP.connect(await debuggerEndpoint());
  const { targetId } = await browser.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await browser.send("Target.attachToTarget", { targetId, flatten: true });
  const call = (method, params) => browser.send(method, params, sessionId);
  const exceptions = []; const failedRequests = [];
  browser.on("Runtime.exceptionThrown", (value) => exceptions.push(
    value.exceptionDetails.exception?.description ?? value.exceptionDetails.text));
  browser.on("Network.loadingFailed", (value) => failedRequests.push(value.errorText));
  await call("Runtime.enable"); await call("Network.enable"); await call("Page.enable");
  await call("Page.navigate", { url: PAGE_URL });
  const evaluate = async (expression) => {
    const { result, exceptionDetails } = await call("Runtime.evaluate", {
      expression, awaitPromise: true, returnByValue: true,
    });
    if (exceptionDetails) throw new Error(exceptionDetails.exception?.description ?? exceptionDetails.text);
    return result.value;
  };
  const waitFor = async (name, predicate, timeout = 90000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      if (await predicate()) return true;
      await sleep(200);
    }
    console.log(`timed out waiting for ${name}`); return false;
  };

  await waitFor("descriptor-locked client boot", () => evaluate(
    `document.getElementById("openrealtime-root")?.dataset.state === "ready"`));
  check("descriptor-locked client booted", await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`) === "ready");
  check("exact composed plugin population mounted", JSON.stringify(await evaluate(
    `window.__openrealtime?.mounted`)) === JSON.stringify([
      "slots", "transport", "reducer", "session-configuration", "view", "release-challenge",
    ]));
  check("client plan identity is visible", (await evaluate(
    `document.getElementById("openrealtime-root").dataset.clientFingerprint`))?.startsWith("sha256:"));
  check("manifest declares only the public relative realtime endpoint", await evaluate(`(() => {
    const endpoints = window.__openrealtime?.manifest?.endpoints;
    return Array.isArray(endpoints) && endpoints.length === 1 &&
      endpoints[0].name === "realtime.websocket" && endpoints[0].method === "GET" &&
      endpoints[0].path === "/client/v1/realtime";
  })()`));
  check("all client plugins report active", await evaluate(`(() => {
    const live = window.__openrealtime?.live();
    return live?.state === "active" && Object.values(live.entries).every((entry) =>
      entry.state === "active" && entry.error === "");
  })()`));
  check("no module request failed", failedRequests.length === 0, failedRequests.join("; "));
  check("no boot exception", exceptions.length === 0, exceptions.join("; "));

  await evaluate(`document.getElementById("connect").click()`);
  await waitFor("live Realtime connection", () => evaluate(
    `document.getElementById("state").textContent === "connected"`), 120000);
  check("client connected through the public relay", await evaluate(
    `document.getElementById("state").textContent`) === "connected");
  await waitFor("challenge tool negotiation", () => evaluate(
    `document.getElementById("openrealtime-root").dataset.challengeToolNegotiated === "yes"`), 120000);
  check("descriptor-supplied challenge tool was negotiated", await evaluate(
    `document.getElementById("openrealtime-root").dataset.challengeToolNegotiated`) === "yes");
  await evaluate(`(() => {
    const input = document.getElementById("text");
    input.value = "Use read_release_challenge exactly once, then reply with exactly the value it returned and nothing else.";
    input.form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  })()`);
  await waitFor("real-model challenge tool call", () => evaluate(
    `document.getElementById("openrealtime-root").dataset.challengeToolCalls === "1"`), 360000);
  check("real model chose the unpredictable challenge tool", await evaluate(
    `document.getElementById("openrealtime-root").dataset.challengeToolCalls`) === "1");
  await waitFor("real-model challenge response", () => evaluate(
    `[...document.querySelectorAll("article[data-role=assistant]")].some((node) => node.textContent.includes(${JSON.stringify(CHALLENGE)}))`),
    360000);
  check("typed live turn rendered", await evaluate(
    `[...document.querySelectorAll("article[data-role=user]")].some((node) =>
      node.textContent.includes("read_release_challenge") && !node.textContent.includes(${JSON.stringify(CHALLENGE)}))`));
  const assistant = await evaluate(
    `[...document.querySelectorAll("article[data-role=assistant]")].map((node) => node.textContent).find((text) => text.includes(${JSON.stringify(CHALLENGE)})) ?? ""`);
  check("real model returned the unpredictable tool result", assistant.includes(CHALLENGE),
    `assistant response bytes=${new TextEncoder().encode(assistant).length}`);
  check("challenge tool was called exactly once through response completion", await evaluate(
    `document.getElementById("openrealtime-root").dataset.challengeToolCalls`) === "1");
  check("client reports no protocol error", await evaluate(
    `document.getElementById("error").textContent`) === "");
  check("no runtime exception", exceptions.length === 0, exceptions.join("; "));
  check("no network request failed", failedRequests.length === 0, failedRequests.join("; "));
  await evaluate(`window.__openrealtime.dispose()`);
  check("client lifecycle disposed", await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`) === "disposed");
  check("client slots cleared", await evaluate(
    `document.getElementById("openrealtime-root").childElementCount`) === 0);
} catch (error) {
  check("run completed", false, error.stack ?? error.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed`);
if (failed.length === 0) console.log("composable presentation Chromium checks completed");
process.exit(failed.length ? 1 : 0);
