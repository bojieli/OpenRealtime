// Run the bundled official SDK in Chromium against an OpenRealtime WebRTC
// adapter, and report what happened.
//
//   node browser.mjs <adapter sdp url>
//
// The page is served from a different origin than the adapter, because that is
// what every real deployment looks like: the adapter is a port on a server and
// the application is a site. A test that served both from one origin would
// pass while telling you nothing about whether anyone can actually do this.

import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { setTimeout as sleep } from "node:timers/promises";

const here = dirname(fileURLToPath(import.meta.url));
const ADAPTER = process.argv[2] ?? "http://127.0.0.1:8766/v1/realtime/calls";
const CDP_PORT = Number(process.env.CDP_PORT ?? 19224);

// A static server for the page, on its own origin.
const pages = {
  "/": ["webrtc.html", "text/html; charset=utf-8"],
  "/webrtc.html": ["webrtc.html", "text/html; charset=utf-8"],
  "/dist/webrtc.js": ["dist/webrtc.js", "text/javascript; charset=utf-8"],
};
const site = createServer(async (request, response) => {
  const entry = pages[request.url.split("?")[0]];
  if (!entry) {
    response.writeHead(404).end("not found");
    return;
  }
  try {
    const body = await readFile(join(here, entry[0]));
    response.writeHead(200, { "Content-Type": entry[1] }).end(body);
  } catch (failure) {
    response.writeHead(500).end(String(failure));
  }
});
await new Promise((resolve) => site.listen(0, "127.0.0.1", resolve));
const siteURL = `http://127.0.0.1:${site.address().port}/`;

const profile = mkdtempSync(join(tmpdir(), "openrealtime-sdk-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new",
  `--remote-debugging-port=${CDP_PORT}`,
  "--no-sandbox", "--disable-gpu",
  "--use-fake-device-for-media-stream",
  "--use-fake-ui-for-media-stream",
  "--autoplay-policy=no-user-gesture-required",
  `--user-data-dir=${profile}`,
  "about:blank",
], { stdio: ["ignore", "pipe", "pipe"] });
let chromiumErrors = "";
chromium.stderr.on("data", (chunk) => { chromiumErrors += chunk.toString(); });

async function debuggerEndpoint() {
  for (let attempt = 0; attempt < 80; attempt++) {
    try {
      const response = await fetch(`http://127.0.0.1:${CDP_PORT}/json/version`);
      return (await response.json()).webSocketDebuggerUrl;
    } catch {
      await sleep(250);
    }
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
function check(name, ok, detail = "") {
  results.push({ name, ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? `  — ${detail}` : ""}`);
}

try {
  const browser = await CDP.connect(await debuggerEndpoint());
  const { targetId } = await browser.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await browser.send("Target.attachToTarget", { targetId, flatten: true });
  const call = (method, params) => browser.send(method, params, sessionId);

  const consoleLines = [];
  browser.on("Runtime.consoleAPICalled", (params) =>
    consoleLines.push(params.args.map((a) => a.value ?? a.description ?? a.type).join(" ")));
  browser.on("Runtime.exceptionThrown", (params) =>
    consoleLines.push("EXCEPTION " + (params.exceptionDetails.exception?.description
      ?? params.exceptionDetails.text)));

  await call("Runtime.enable");
  await call("Page.enable");
  await call("Page.navigate", { url: siteURL });

  const evaluate = async (expression, ms = 180000) => {
    const { result, exceptionDetails } = await call("Runtime.evaluate", {
      expression, awaitPromise: true, returnByValue: true, timeout: ms,
    });
    if (exceptionDetails) {
      throw new Error(exceptionDetails.exception?.description ?? exceptionDetails.text);
    }
    return result.value;
  };

  for (let attempt = 0; attempt < 60; attempt++) {
    if (await evaluate("Boolean(window.__ready)")) break;
    await sleep(250);
  }
  check("the bundled SDK loads in a browser", await evaluate("Boolean(window.__ready)"),
    consoleLines.slice(0, 3).join(" | "));

  const run = await evaluate(`window.__run(${JSON.stringify({
    baseUrl: ADAPTER, apiKey: process.env.OPENREALTIME_TOKEN ?? "not-a-real-key",
    prompt: "What is the weather in Cambridge? Use your tool.",
  })})`);

  for (const result of run ?? []) {
    check(result.name, result.ok, result.detail);
  }
  if (consoleLines.length) {
    console.log("  page console:", consoleLines.slice(0, 6).join(" | "));
  }
} catch (failure) {
  check("the run completed", false, failure.message);
} finally {
  chromium.kill("SIGKILL");
  site.close();
  // Chromium's children can still be writing into the profile when the parent
  // is killed, so removing it straight away races them and throws ENOTEMPTY -
  // which failed a CI run whose every check had already passed. Retry briefly,
  // and never let temp-directory cleanup decide the outcome of a test.
  try {
    rmSync(profile, { recursive: true, force: true, maxRetries: 50, retryDelay: 100 });
  } catch {}
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (official SDK, WebRTC)`);
process.exit(failed.length === 0 ? 0 : 1);
