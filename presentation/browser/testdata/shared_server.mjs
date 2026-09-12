import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const PAGE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19315);
const CLIENT_PLAN_FINGERPRINT = process.env.CLIENT_PLAN_FINGERPRINT ?? "";
const SESSION_GRAPH_FINGERPRINT = process.env.SESSION_GRAPH_FINGERPRINT ?? "";
const FIXTURE_INPUT = process.env.FIXTURE_INPUT ?? "";
const FIXTURE_REPLY = process.env.FIXTURE_REPLY ?? "";

const profile = mkdtempSync(join(tmpdir(), "openrealtime-shared-server-client-"));
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
    } catch {
      await sleep(100);
    }
  }
  throw new Error(`Chromium did not start:\n${chromiumErrors}`);
}

class CDP {
  #socket;
  #next = 1;
  #pending = new Map();
  #handlers = new Map();

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
        frame.error
          ? pending.reject(new Error(JSON.stringify(frame.error)))
          : pending.resolve(frame.result);
      } else if (frame.method) {
        for (const handler of client.#handlers.get(frame.method) ?? []) handler(frame.params);
      }
    });
    return client;
  }

  on(method, handler) {
    const handlers = this.#handlers.get(method) ?? [];
    handlers.push(handler);
    this.#handlers.set(method, handlers);
  }

  send(method, params = {}, sessionId) {
    const id = this.#next++;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject });
      this.#socket.send(JSON.stringify({ id, method, params, sessionId }));
    });
  }
}

function subsequence(actual, wanted) {
  let cursor = 0;
  for (const value of actual) {
    if (value === wanted[cursor]) cursor++;
    if (cursor === wanted.length) return true;
  }
  return false;
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
  const exceptions = [];
  const realtimeRequests = new Set();
  const sentTypes = [];
  const receivedTypes = [];
  let inspectionNegotiated = false;

  browser.on("Runtime.exceptionThrown", (value) => {
    exceptions.push(value.exceptionDetails.text);
  });
  browser.on("Network.webSocketCreated", ({ requestId, url }) => {
    try {
      if (new URL(url).pathname === "/client/v1/realtime") realtimeRequests.add(requestId);
    } catch {}
  });
  browser.on("Network.webSocketFrameSent", ({ requestId, response }) => {
    if (!realtimeRequests.has(requestId) || response.opcode !== 1) return;
    try {
      const event = JSON.parse(response.payloadData);
      if (typeof event.type === "string") sentTypes.push(event.type);
    } catch {}
  });
  browser.on("Network.webSocketFrameReceived", ({ requestId, response }) => {
    if (!realtimeRequests.has(requestId) || response.opcode !== 1) return;
    try {
      const event = JSON.parse(response.payloadData);
      if (typeof event.type === "string") receivedTypes.push(event.type);
      if (event.type === "session.updated") {
        const access = event.session?.openrealtime?.debug?.inspection;
        inspectionNegotiated = typeof access?.session_id === "string" && access.session_id.length > 0 &&
          typeof access?.path === "string" && access.path.startsWith("/") &&
          typeof access?.token === "string" && /^mgmt_[A-Za-z0-9_-]+$/.test(access.token) &&
          Number.isSafeInteger(access?.expires_at_ms);
      }
    } catch {}
  });

  await call("Runtime.enable");
  await call("Network.enable");
  await call("Page.enable");
  await call("Page.navigate", { url: PAGE_URL });

  const evaluate = async (expression) => {
    const { result, exceptionDetails } = await call("Runtime.evaluate", {
      expression, awaitPromise: true, returnByValue: true,
    });
    if (exceptionDetails) {
      throw new Error(exceptionDetails.exception?.description ?? exceptionDetails.text);
    }
    return result.value;
  };
  const waitFor = async (name, predicate, timeout = 20000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      if (await predicate()) return true;
      await sleep(50);
    }
    console.log(`timed out waiting for ${name}`);
    return false;
  };

  await waitFor("client boot", () => evaluate(
    `document.getElementById("openrealtime-root")?.dataset.state === "ready"`));
  check("browser client mounted as a locked composition", await evaluate(
    `document.getElementById("openrealtime-root")?.dataset.state`) === "ready");
  check("browser plan identity matches the compiled profile", await evaluate(
    `window.__openrealtime?.manifest?.plan?.fingerprint`) === CLIENT_PLAN_FINGERPRINT);
  const initialLive = await evaluate(`window.__openrealtime.live()`);
  check("every browser plugin is independently active", initialLive.state === "active" &&
    Object.values(initialLive.entries).every((entry) =>
      entry.state === "active" && entry.desired === true && entry.error === ""));
  check("browser boot raised no exception", exceptions.length === 0, exceptions.join("; "));

  await evaluate(`document.getElementById("connect").click()`);
  await waitFor("Realtime connection", () => evaluate(
    `document.getElementById("state").textContent === "connected"`));
  check("browser WebSocket transport connected", await evaluate(
    `document.getElementById("state").textContent`) === "connected");
  await waitFor("session update", () => sentTypes.includes("session.update"));
  await waitFor("session inspection", () => evaluate(
    `document.querySelector('[data-view="inspection"] #availability')?.textContent === "live"`));
  check("browser negotiated a narrow inspection capability", inspectionNegotiated);
  check("browser consumed the canonical management API", await evaluate(
    `document.querySelector('[data-view="inspection"] #availability')?.textContent`) === "live");
  check("browser observed the exact session graph", (await evaluate(
    `document.querySelector('[data-view="inspection"] #identity')?.textContent`))?.includes(
      SESSION_GRAPH_FINGERPRINT));
  check("inspection bearer is absent from the presentation", !(await evaluate(
    `/mgmt_[A-Za-z0-9_-]+/.test(document.body.innerText)`)));

  await evaluate(`(() => {
    const input = document.getElementById("text");
    input.value = ${JSON.stringify(FIXTURE_INPUT)};
    input.form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  })()`);
  await waitFor("assistant response", () => evaluate(
    `[...document.querySelectorAll('article[data-role="assistant"]')]
      .some((node) => node.textContent.includes(${JSON.stringify(FIXTURE_REPLY)}))`));
  await waitFor("response.done", () => receivedTypes.includes("response.done"));
  check("browser rendered its committed user item", await evaluate(
    `[...document.querySelectorAll('article[data-role="user"]')]
      .some((node) => node.textContent.includes(${JSON.stringify(FIXTURE_INPUT)}))`));
  check("browser rendered the shared server response", await evaluate(
    `[...document.querySelectorAll('article[data-role="assistant"]')]
      .some((node) => node.textContent.includes(${JSON.stringify(FIXTURE_REPLY)}))`));
  check("browser emitted the exact Realtime command lifecycle", subsequence(sentTypes, [
    "session.update", "conversation.item.create", "response.create",
  ]), sentTypes.join(", "));
  check("browser accepted the exact Realtime server lifecycle", subsequence(receivedTypes, [
    "session.created", "session.updated", "conversation.item.created", "response.created",
    "response.output_audio_transcript.delta", "response.done",
  ]), receivedTypes.join(", "));
  check("browser runtime raised no exception", exceptions.length === 0, exceptions.join("; "));

  await evaluate(`window.__openrealtime.dispose()`);
  const finalLive = await evaluate(`window.__openrealtime.live()`);
  check("browser client scope disposed", finalLive.state === "closed");
  check("browser disposal released every scoped service and effect",
    Object.values(finalLive.entries).every((entry) =>
      entry.state === "inactive" && entry.effects === 0 && entry.services.length === 0));
  check("browser view slots were removed", await evaluate(
    `document.getElementById("openrealtime-root").childElementCount`) === 0);
} catch (error) {
  check("shared browser run completed", false, error.stack ?? error.message);
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
