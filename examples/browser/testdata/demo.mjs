// The demo, run.
//
// Its own test says a page that had drifted would be worse than no demo
// because it looks like it works - and then checks that by grepping the served
// bytes for event names. Greppable and runnable are different properties: a
// syntax error, a renamed element, or a flow that no longer completes all
// survive a grep intact, and this is the file client authors copy and the
// first thing anyone points at a deployment.
//
// So this loads it in a browser against a real adapter in front of a real
// gateway, and asks whether a person would have got a conversation.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const CONSOLE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19270);
const profile = mkdtempSync(join(tmpdir(), "openrealtime-demo-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new", `--remote-debugging-port=${PORT}`,
  "--no-sandbox", "--disable-gpu",
  "--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream",
  ...(process.env.DEMO_FAKE_AUDIO
    ? [`--use-file-for-fake-audio-capture=${process.env.DEMO_FAKE_AUDIO}%noloop`]
    : []),
  "--autoplay-policy=no-user-gesture-required",
  `--user-data-dir=${profile}`, "about:blank",
], { stdio: ["ignore", "pipe", "pipe"] });
let chromiumErrors = "";
chromium.stderr.on("data", (chunk) => { chromiumErrors += chunk.toString(); });

async function debuggerEndpoint() {
  for (let attempt = 0; attempt < 80; attempt++) {
    try {
      const response = await fetch(`http://127.0.0.1:${PORT}/json/version`);
      return (await response.json()).webSocketDebuggerUrl;
    } catch { await sleep(250); }
  }
  throw new Error(`chromium never opened a debugging port:\n${chromiumErrors}`);
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
        const { resolve, reject } = client.#pending.get(frame.id);
        client.#pending.delete(frame.id);
        frame.error ? reject(new Error(JSON.stringify(frame.error))) : resolve(frame.result);
      } else if (frame.method) {
        (client.#handlers.get(frame.method) ?? []).forEach((handler) => handler(frame.params));
      }
    });
    return client;
  }

  on(method, handler) {
    if (!this.#handlers.has(method)) this.#handlers.set(method, []);
    this.#handlers.get(method).push(handler);
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
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? `  — ${detail}` : ""}`);
};

try {
  const browser = await CDP.connect(await debuggerEndpoint());
  const { targetId } = await browser.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await browser.send("Target.attachToTarget", { targetId, flatten: true });
  const call = (method, params) => browser.send(method, params, sessionId);
  // The check a grep cannot make. An uncaught exception here means the page
  // was served intact and does not run.
  const pageErrors = [];
  browser.on("Runtime.exceptionThrown", (params) =>
    pageErrors.push(params.exceptionDetails.exception?.description
      ?? params.exceptionDetails.text));
  await call("Runtime.enable");
  await call("Page.enable");
  await call("Page.navigate", { url: CONSOLE_URL });
  await sleep(2000);

  const evaluate = async (expression) => {
    const { result, exceptionDetails } = await call("Runtime.evaluate", {
      expression, awaitPromise: true, returnByValue: true, timeout: 60000,
    });
    if (exceptionDetails) throw new Error(exceptionDetails.exception?.description ?? exceptionDetails.text);
    return result.value;
  };
  const waitFor = async (what, predicate, ms = 45000) => {
    const deadline = Date.now() + ms;
    while (Date.now() < deadline) {
      if (await predicate()) return true;
      await sleep(250);
    }
    console.log(`  (timed out waiting for ${what})`);
    return false;
  };

  // Nothing in the page may have thrown on load. This is the check a grep
  // cannot make: the bytes can contain every event name and still not parse.
  check("the page loads and its script runs", pageErrors.length === 0,
    pageErrors.join("; "));

  await evaluate(`document.getElementById('connect').click()`);
  await waitFor("the session to open", async () =>
    !["not connected", "asking for the microphone", "negotiating"].includes(
      await evaluate(`document.getElementById('state').textContent`)));
  const state = await evaluate(`document.getElementById('state').textContent`);
  check("it negotiates WebRTC and opens a session",
    !state.includes("could not") && !state.includes("refused"), `state=${state}`);

  // Deliberately stopping at connected rather than driving a whole
  // conversation. Reaching it means the offer was accepted, the answer
  // applied, and the data channel opened - every step a grep cannot check, and
  // the step that was actually broken. Asserting a full turn as well would put
  // a recogniser's timing inside a test about whether a page runs.
  await waitFor("the data channel to open", async () =>
    (await evaluate(`document.getElementById('state').textContent`)) === "connected");
  check("it reaches a connected session",
    (await evaluate(`document.getElementById('state').textContent`)) === "connected",
    await evaluate(`document.getElementById('state').textContent`));

  check("nothing threw during the session", pageErrors.length === 0, pageErrors.join("; "));
} catch (failure) {
  check("the run completed", false, failure.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (demo)`);
process.exit(failed.length === 0 ? 0 : 1);
