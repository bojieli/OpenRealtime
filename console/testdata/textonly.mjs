// A session that asked for text must show the text.
//
// A text session's words arrive on response.output_text.delta rather than on
// the transcript of speech it did not ask for. A client that only ever
// listened for the transcript renders nothing at all: the turn happens, the
// events arrive, and the conversation stays empty. Nothing had to handle it
// while text-only sessions did not exist, which is exactly why nothing did.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const CONSOLE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19250);
const profile = mkdtempSync(join(tmpdir(), "openrealtime-textonly-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new", `--remote-debugging-port=${PORT}`,
  "--no-sandbox", "--disable-gpu",
  "--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream",
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
  #socket; #next = 1; #pending = new Map();
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
      }
    });
    return client;
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

  // Ask for text before connecting, the way a person would: edit the session
  // the console is about to send.
  await evaluate(`
    (() => {
      const box = document.getElementById('session-json');
      const session = JSON.parse(box.value);
      session.output_modalities = ["text"];
      box.value = JSON.stringify(session, null, 2);
      return true;
    })()`);
  await evaluate(`document.getElementById('connect').click()`);
  await waitFor("the session to open", async () =>
    ["connected", "listening", "thinking", "responding"].includes(
      await evaluate(`document.getElementById('state').textContent`)));

  const negotiated = await evaluate(`
    JSON.parse(document.getElementById('session-json').value).output_modalities`);
  check("the console asked for text", JSON.stringify(negotiated) === '["text"]',
    JSON.stringify(negotiated));

  await evaluate(`
    document.getElementById('typed').value = 'say something';
    document.getElementById('compose').dispatchEvent(new Event('submit', {cancelable: true}));`);

  const agentTurns = () => evaluate(`
    [...document.querySelectorAll('.turn.agent')].map(t => t.querySelector('.text').textContent)`);
  await waitFor("the agent's text", async () =>
    (await agentTurns()).some((text) => text.trim().length > 0));

  const said = await agentTurns();
  check("the agent's text is rendered", said.some((t) => t.trim().length > 0),
    JSON.stringify(said));

  // It arrived as text, not as the transcript of speech.
  const kinds = await evaluate(`
    [...document.querySelectorAll('.event')].map(e => e.querySelector('.name').textContent)`);
  check("it arrived on the text events",
    kinds.some((k) => k.includes("response.output_text.delta")), "");
  check("no speech was synthesised for a text session",
    !kinds.some((k) => k.includes("response.output_audio.delta")), "");

  check("no errors were surfaced", (await evaluate(`
    [...document.querySelectorAll('.turn.notice')].map(t => t.querySelector('.text').textContent)`)).length === 0);
} catch (failure) {
  check("the run completed", false, failure.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (text-only session)`);
process.exit(failed.length === 0 ? 0 : 1);
