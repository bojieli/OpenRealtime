// A turn that produces nothing must say so in the console.
//
// The server reports it twice - status "incomplete" on the wire for the
// client, a warning naming the knob in the log for the operator. This checks
// the first: that the console does not throw the status away and leave a
// person looking at a conversation where their question simply went
// unanswered.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const CONSOLE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19240);
const profile = mkdtempSync(join(tmpdir(), "openrealtime-incomplete-"));
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

  await evaluate(`document.getElementById('connect').click()`);
  await waitFor("the session to open", async () =>
    ["connected", "listening", "thinking", "responding"].includes(
      await evaluate(`document.getElementById('state').textContent`)));

  await evaluate(`
    document.getElementById('typed').value = 'say something';
    document.getElementById('compose').dispatchEvent(new Event('submit', {cancelable: true}));`);

  const notices = () => evaluate(`
    [...document.querySelectorAll('.turn.notice')].map(t => t.querySelector('.text').textContent)`);
  await waitFor("the console to report the empty turn", async () =>
    (await notices()).some((text) => text.includes("no output")));

  const reported = await notices();
  check("the console reports a turn that produced nothing",
    reported.some((text) => text.includes("no output")), JSON.stringify(reported));
  check("it names the reason the server gave",
    reported.some((text) => text.includes("max_output_tokens")), JSON.stringify(reported));

  const stats = await evaluate(`
    Object.fromEntries([...document.querySelectorAll('#stats dt')].map(
      (dt, i) => [dt.textContent, document.querySelectorAll('#stats dd')[i].textContent]))`);
  check("the outcome is recorded in the stats",
    (stats["last turn"] ?? "").includes("incomplete"), JSON.stringify(stats["last turn"]));

  // The session is not over. A turn that said nothing is a turn, not a failure.
  check("the session is still connected",
    ["connected", "listening", "thinking", "responding"].includes(
      await evaluate(`document.getElementById('state').textContent`)));
} catch (failure) {
  check("the run completed", false, failure.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (incomplete turn)`);
process.exit(failed.length === 0 ? 0 : 1);
