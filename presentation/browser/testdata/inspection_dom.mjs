// Render the inspection-view fixture in real Chromium and print the serialised
// DOM on stdout.
//
// This exists because `--dump-dom` does not work on every Chromium this suite
// has to run against. On the project's Linux CI runner it produced no output at
// all - not a partial document, zero bytes - while the browser stayed alive
// until the test's deadline killed it, so the only thing the test could report
// was its own kill signal. Every other browser check in this package drives
// Chromium over the DevTools protocol and then kills it, which works on that
// same runner, so this fixture is rendered the same way: navigate, wait for the
// view to mark itself ready, read documentElement.outerHTML, leave.
//
// The Go test owns the assertions. This driver's only contract is that stdout
// is the document and nothing else, so anything diagnostic goes to stderr.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const PAGE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19410);
const profile = mkdtempSync(join(tmpdir(), "openrealtime-inspection-dom-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new", `--remote-debugging-port=${PORT}`, "--no-sandbox", "--disable-gpu",
  "--disable-dev-shm-usage", "--no-first-run", "--disable-background-networking",
  "--disable-component-update", "--disable-sync", "--disable-default-apps",
  "--disable-client-side-phishing-detection",
  `--user-data-dir=${profile}`, "about:blank",
], { stdio: ["ignore", "pipe", "pipe"] });
let chromiumErrors = "";
chromium.stdout.on("data", (chunk) => { process.stderr.write(chunk); });
chromium.stderr.on("data", (chunk) => { chromiumErrors += chunk.toString(); });

async function endpoint() {
  for (let attempt = 0; attempt < 300; attempt++) {
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

let status = 1;
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

  // The fixture marks the view ready on its root element. Waiting for that
  // rather than for load alone is what makes the serialised document worth
  // asserting on: a document read before the module ran would be missing every
  // value the Go test checks, and would fail as though the view were wrong.
  //
  // Only the root element's flag counts. A view may mark its own section
  // ready when it mounts, and a selector matching any element read the
  // timeline before its events were rendered.
  const deadline = Date.now() + 60000;
  let ready = false;
  while (Date.now() < deadline) {
    const flag = await evaluate(`document.documentElement.dataset.ready ?? ""`);
    if (flag === "true") { ready = true; break; }
    if (flag === "false") {
      throw new Error(`the fixture reported the view did not settle: ${await evaluate(`document.documentElement.outerHTML`)}`);
    }
    await sleep(100);
  }
  if (!ready) {
    throw new Error(`inspection view never reported ready; exceptions=${exceptions.join("; ")} ` +
      `failedRequests=${failedRequests.join("; ")}`);
  }
  process.stdout.write(await evaluate(`document.documentElement.outerHTML`));
  status = 0;
} catch (error) {
  process.stderr.write(`${error?.stack ?? error}\n${chromiumErrors}\n`);
} finally {
  chromium.kill("SIGKILL");
  // Chromium's children can still be writing into the profile when the parent
  // is killed, so removing it straight away races them and throws ENOTEMPTY -
  // which failed a CI run whose every check had already passed. Retry briefly,
  // and never let temp-directory cleanup decide the outcome of a test.
  try {
    rmSync(profile, { recursive: true, force: true, maxRetries: 50, retryDelay: 100 });
  } catch {}
  process.exit(status);
}
