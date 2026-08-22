// A session that takes the floor must be drivable.
//
// turn_detection: null means the client will say where its turns end. The
// server then correctly stops endpointing on silence and waits to be told - so
// a client that can declare it and not drive it leaves the session hanging
// forever, with audio flowing and nothing ever happening. The console could
// declare it from its session editor long before it could send the two events
// that finish a turn.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const CONSOLE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19260);
const profile = mkdtempSync(join(tmpdir(), "openrealtime-manualturns-"));
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

  // The event log hides audio by default, and this test reads it as evidence.
  await evaluate(`document.getElementById('filter-audio').checked = false`);

  // Take the floor, the way a person would: edit the session before connecting.
  await evaluate(`
    (() => {
      const box = document.getElementById('session-json');
      const session = JSON.parse(box.value);
      session.audio = session.audio ?? {};
      session.audio.input = session.audio.input ?? {};
      session.audio.input.turn_detection = null;
      box.value = JSON.stringify(session, null, 2);
      return true;
    })()`);
  await evaluate(`document.getElementById('connect').click()`);
  await waitFor("the session to open", async () =>
    ["connected", "listening", "thinking", "responding"].includes(
      await evaluate(`document.getElementById('state').textContent`)));

  await waitFor("the console to notice it holds the floor", async () =>
    !(await evaluate(`document.getElementById('endturn').disabled`)));
  check("the console offers the control only this session needs",
    !(await evaluate(`document.getElementById('endturn').disabled`)));
  check("and says so, rather than leaving a person to wonder",
    (await evaluate(`document.getElementById('media-state').textContent`)).includes("declares its own turns"));

  // Audio is flowing and the server is correctly not answering. Nothing should
  // have happened yet: that is the state a client that cannot drive this is
  // stuck in forever.
  await waitFor("audio to reach the protocol", async () =>
    (await evaluate(`
      [...document.querySelectorAll('.event')].filter(e => e.dataset.direction === 'out')
        .map(e => e.querySelector('.name').textContent)
        .some(n => n.includes('input_audio_buffer.append'))`)));
  const before = await evaluate(`
    [...document.querySelectorAll('.event')].map(e => e.querySelector('.name').textContent)
      .filter(n => n.includes('response.created')).length`);
  check("the server does not answer a turn the client has not ended", before === 0, `${before}`);

  await evaluate(`document.getElementById('endturn').click()`);

  const sent = () => evaluate(`
    [...document.querySelectorAll('.event')].filter(e => e.dataset.direction === 'out')
      .map(e => e.querySelector('.name').textContent)`);
  await waitFor("the turn to be committed", async () =>
    (await sent()).some((n) => n.includes('input_audio_buffer.commit')));
  const out = await sent();
  check("ending the turn says where it ended",
    out.some((n) => n.includes('input_audio_buffer.commit')));
  check("and asks for the answer",
    out.some((n) => n.includes('response.create')));

  await waitFor("the agent to answer", async () =>
    (await evaluate(`
      [...document.querySelectorAll('.turn.agent')].map(t => t.querySelector('.text').textContent)`))
      .some((t) => t.trim().length > 0));
  check("the agent answers a turn the client declared",
    (await evaluate(`
      [...document.querySelectorAll('.turn.agent')].map(t => t.querySelector('.text').textContent)`))
      .some((t) => t.trim().length > 0));
} catch (failure) {
  check("the run completed", false, failure.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (manual turns)`);
process.exit(failed.length === 0 ? 0 : 1);
