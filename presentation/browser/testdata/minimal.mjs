import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const PAGE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19310);
const profile = mkdtempSync(join(tmpdir(), "openrealtime-plugin-client-"));
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
  const browser = await CDP.connect(await endpoint());
  const { targetId } = await browser.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await browser.send("Target.attachToTarget", { targetId, flatten: true });
  const call = (method, params) => browser.send(method, params, sessionId);
  const exceptions = []; const failedRequests = [];
  browser.on("Runtime.exceptionThrown", (value) => exceptions.push(value.exceptionDetails.text));
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
  const waitFor = async (name, predicate, timeout = 20000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      if (await predicate()) return true;
      await sleep(100);
    }
    console.log(`timed out waiting for ${name}`); return false;
  };
  await waitFor("plugin client boot", () => evaluate(
    `document.getElementById("openrealtime-root")?.dataset.state === "ready"`));
  check("locked client booted", await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`) === "ready");
  check("four plugins mounted", JSON.stringify(await evaluate(`window.__openrealtime?.mounted`)) ===
    JSON.stringify(["slots", "transport", "reducer", "view"]));
  check("client identity is visible", (await evaluate(
    `document.getElementById("openrealtime-root").dataset.clientFingerprint`))?.startsWith("sha256:"));
  check("no module request failed", failedRequests.length === 0, failedRequests.join("; "));
  check("no boot exception", exceptions.length === 0, exceptions.join("; "));

  const vectors = await evaluate(`(async () => {
    const manifest = window.__openrealtime.manifest;
    const implementation = manifest.implementations.find((row) => row.entry === "reducer");
    const asset = manifest.assets.find((row) => row.entry === "reducer" && row.name === implementation.entrypoint);
    const [moduleResponse, corpusResponse] = await Promise.all([
      fetch(asset.path, {cache: "no-store"}),
      fetch("/test/reducer-vectors.json", {cache: "no-store"}),
    ]);
    if (!moduleResponse.ok || !corpusResponse.ok) throw new Error("vector fixtures were unavailable");
    const moduleBytes = await moduleResponse.arrayBuffer();
    const actualDigest = "sha256:" + [...new Uint8Array(await crypto.subtle.digest("SHA-256", moduleBytes))]
      .map((value) => value.toString(16).padStart(2, "0")).join("");
    if (actualDigest !== asset.digest) throw new Error("vector runner module digest mismatch");
    const moduleURL = URL.createObjectURL(new Blob([moduleBytes], {type: asset.media_type}));
    try {
      const reducer = await import(moduleURL);
      const corpus = reducer.loadCorpusText(await corpusResponse.text());
      const failures = [];
      for (const vector of corpus.vectors) {
        const actual = reducer.runVector(vector, corpus.limits);
        if (reducer.stableJSON(actual) !== reducer.stableJSON(vector.expect)) failures.push(vector.name);
      }
      return {count: corpus.vectors.length, failures};
    } finally {
      URL.revokeObjectURL(moduleURL);
    }
  })()`);
  check("all nine shared reducer vectors executed", vectors.count === 9, `count=${vectors.count}`);
  check("shared reducer vectors conform", vectors.failures.length === 0, vectors.failures.join(", "));

  await evaluate(`document.getElementById("connect").click()`);
  await waitFor("WebSocket connection", () => evaluate(`document.getElementById("state").textContent === "connected"`));
  check("transport connected", await evaluate(`document.getElementById("state").textContent`) === "connected");
  await evaluate(`(() => {
    const input = document.getElementById("text"); input.value = "hello server";
    input.form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  })()`);
  await waitFor("fixture response", () => evaluate(
    `[...document.querySelectorAll("article[data-role=assistant]")].some((node) => node.textContent.includes("hello from fixture"))`));
  check("user turn rendered", await evaluate(
    `[...document.querySelectorAll("article[data-role=user]")].some((node) => node.textContent.includes("hello server"))`));
  check("assistant text rendered", await evaluate(
    `[...document.querySelectorAll("article[data-role=assistant]")].some((node) => node.textContent.includes("hello from fixture"))`));
  await waitFor("bounded transport reconnect", () => evaluate(
    `fetch("/test/backend-connections", {cache:"no-store"}).then((response) => response.json()).then((value) => value.connections >= 2)`));
  check("bounded reconnect opened a second transport", (await evaluate(
    `fetch("/test/backend-connections", {cache:"no-store"}).then((response) => response.json()).then((value) => value.connections)`)) === 2);
  await waitFor("reconnected state", () => evaluate(`document.getElementById("state").textContent === "connected"`));
  check("reducer returned to connected", await evaluate(
    `document.getElementById("state").textContent`) === "connected");
  check("no runtime exception", exceptions.length === 0, exceptions.join("; "));
  await evaluate(`window.__openrealtime.dispose()`);
  check("client lifecycle disposed", await evaluate(
    `document.getElementById("openrealtime-root").dataset.state`) === "disposed");
  check("client slots were cleared", await evaluate(
    `document.getElementById("openrealtime-root").childElementCount`) === 0);
} catch (error) {
  check("run completed", false, error.stack ?? error.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed`);
process.exit(failed.length ? 1 : 0);
