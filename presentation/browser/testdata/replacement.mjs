import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const PAGE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19312);
const profile = mkdtempSync(join(tmpdir(), "openrealtime-client-replacement-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
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
  browser.on("Runtime.exceptionThrown", (value) => exceptions.push(
    value.exceptionDetails.exception?.description ?? value.exceptionDetails.text));
  await call("Runtime.enable"); await call("Page.enable");
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

  await waitFor("replaceable client boot", () => evaluate(
    `document.getElementById("openrealtime-root")?.dataset.state === "ready"`));
  const initial = await evaluate(`(() => ({
    state: window.__openrealtime.live(),
    manifest: window.__openrealtime.manifest,
    provider: document.getElementById("openrealtime-root").dataset.replaceableProvider,
    consumer: document.getElementById("openrealtime-root").dataset.replaceableConsumer,
  }))()`);
  check("replaceable client starts on the exact v1 implementation",
    initial.provider === "v1" && initial.consumer === "v1" && initial.state.sequence === 1 &&
    initial.state.entries.replaceable.implementation === "browser-esm:replaceable-v1.js");
  check("live client exposes exact plan, manifest, and artifact identities",
    initial.state.fingerprint === initial.manifest.plan.fingerprint &&
    initial.state.manifest_fingerprint === initial.manifest.fingerprint &&
    initial.state.entries.replaceable.artifact.digest.startsWith("sha256:"));

  const invalid = await evaluate(`(async () => {
    const candidate = await fetch("/test/replacement-v2.json", {cache:"no-store"}).then((value) => value.json());
    candidate.fingerprint = "sha256:" + "0".repeat(64);
    let error = "";
    try { await window.__openrealtime.replace("replaceable", candidate); }
    catch (failure) { error = failure?.message ?? String(failure); }
    const root = document.getElementById("openrealtime-root");
    return {error, live: window.__openrealtime.live(), provider: root.dataset.replaceableProvider,
      consumer: root.dataset.replaceableConsumer, disposals: Number(root.dataset.providerV1Disposals || "0")};
  })()`);
  check("unsealed replacement is refused before live teardown",
    invalid.error.includes("fingerprint") && invalid.live.sequence === 1 &&
    invalid.provider === "v1" && invalid.consumer === "v1" && invalid.disposals === 0,
    invalid.error);

  const payloadBearing = await evaluate(`(async () => {
    const candidate = await fetch("/test/replacement-v2.json", {cache:"no-store"}).then((value) => value.json());
    candidate.implementations.find((row) => row.entry === "replaceable").artifact.state_payload = {secret:"no"};
    candidate.fingerprint = "";
    const encoded = new TextEncoder().encode(JSON.stringify(candidate));
    const bytes = new Uint8Array(await crypto.subtle.digest("SHA-256", encoded));
    candidate.fingerprint = "sha256:" + [...bytes].map((value) => value.toString(16).padStart(2, "0")).join("");
    let error = "";
    try { await window.__openrealtime.replace("replaceable", candidate); }
    catch (failure) { error = failure?.message ?? String(failure); }
    const root = document.getElementById("openrealtime-root");
    return {error, sequence: window.__openrealtime.live().sequence,
      provider: root.dataset.replaceableProvider, consumer: root.dataset.replaceableConsumer,
      disposals: Number(root.dataset.providerV1Disposals || "0")};
  })()`);
  check("sealed payload-bearing artifact evidence is refused before teardown",
    payloadBearing.error.includes("unknown or invalid fields") && payloadBearing.sequence === 1 &&
    payloadBearing.provider === "v1" && payloadBearing.consumer === "v1" &&
    payloadBearing.disposals === 0, payloadBearing.error);

  const wrongIdentity = await evaluate(`(async () => {
    const candidate = await fetch("/test/replacement-wrong-identity.json", {cache:"no-store"})
      .then((value) => value.json());
    let error = "";
    try { await window.__openrealtime.replace("replaceable", candidate); }
    catch (failure) { error = failure?.message ?? String(failure); }
    const root = document.getElementById("openrealtime-root");
    return {error, sequence: window.__openrealtime.live().sequence,
      provider: root.dataset.replaceableProvider, consumer: root.dataset.replaceableConsumer,
      disposals: Number(root.dataset.providerV1Disposals || "0"),
      candidateMounted: root.dataset.wrongIdentityMounted === "yes"};
  })()`);
  check("authenticated module identity substitution is refused before teardown",
    wrongIdentity.error.includes("module identity mismatch") && wrongIdentity.sequence === 1 &&
    wrongIdentity.provider === "v1" && wrongIdentity.consumer === "v1" &&
    wrongIdentity.disposals === 0 && !wrongIdentity.candidateMounted, wrongIdentity.error);

  const failed = await evaluate(`(async () => {
    const candidate = await fetch("/test/replacement-failure.json", {cache:"no-store"}).then((value) => value.json());
    let error = "";
    try { await window.__openrealtime.replace("replaceable", candidate); }
    catch (failure) { error = failure?.message ?? String(failure); }
    const root = document.getElementById("openrealtime-root");
    return {error, live: window.__openrealtime.live(), manifest: window.__openrealtime.manifest,
      provider: root.dataset.replaceableProvider, consumer: root.dataset.replaceableConsumer,
      v1Mounts: Number(root.dataset.providerV1Mounts || "0"),
      v1Disposals: Number(root.dataset.providerV1Disposals || "0"),
      failedMounts: Number(root.dataset.failedCandidateMounts || "0"),
      failedDisposals: Number(root.dataset.failedCandidateDisposals || "0"),
      consumerMounts: Number(root.dataset.replaceableConsumerMounts || "0"),
      consumerDisposals: Number(root.dataset.replaceableConsumerDisposals || "0")};
  })()`);
  check("failed candidate activation restores the exact predecessor composition",
    failed.error.includes("rolled back") && failed.live.sequence === 1 &&
    failed.live.manifest_fingerprint === initial.manifest.fingerprint &&
    failed.manifest.fingerprint === initial.manifest.fingerprint &&
    failed.provider === "v1" && failed.consumer === "v1" &&
    failed.live.entries.replaceable.implementation === "browser-esm:replaceable-v1.js",
    failed.error);
  check("candidate and predecessor scopes are each disposed exactly once during rollback",
    failed.v1Mounts === 2 && failed.v1Disposals === 1 &&
    failed.failedMounts === 1 && failed.failedDisposals === 1 &&
    failed.consumerMounts === 2 && failed.consumerDisposals === 1,
    `v1=${failed.v1Mounts}/${failed.v1Disposals} candidate=${failed.failedMounts}/${failed.failedDisposals} ` +
      `consumer=${failed.consumerMounts}/${failed.consumerDisposals}`);

  const replaced = await evaluate(`(async () => {
    const candidate = await fetch("/test/replacement-v2.json", {cache:"no-store"}).then((value) => value.json());
    const receipt = await window.__openrealtime.replace("replaceable", candidate);
    const root = document.getElementById("openrealtime-root");
    return {candidate, receipt, live: window.__openrealtime.live(),
      manifest: window.__openrealtime.manifest,
      provider: root.dataset.replaceableProvider, consumer: root.dataset.replaceableConsumer,
      v1Disposals: Number(root.dataset.providerV1Disposals || "0"),
      v2Mounts: Number(root.dataset.providerV2Mounts || "0"),
      consumerMounts: Number(root.dataset.replaceableConsumerMounts || "0"),
      consumerDisposals: Number(root.dataset.replaceableConsumerDisposals || "0")};
  })()`);
  check("authenticated v2 bytes replace only the selected implementation",
    replaced.provider === "v2" && replaced.consumer === "v2" &&
    replaced.live.sequence === 2 && replaced.live.fingerprint === initial.state.fingerprint &&
    replaced.live.manifest_fingerprint === replaced.candidate.fingerprint &&
    replaced.live.entries.replaceable.implementation === "browser-esm:replaceable-v2.js" &&
    replaced.live.entries.replaceable.artifact.digest ===
      replaced.candidate.implementations.find((row) => row.entry === "replaceable").artifact.digest);
  check("replacement receipt is exact and payload-free",
    replaced.receipt.format_version === 1 && replaced.receipt.entry === "replaceable" &&
    replaced.receipt.plan_fingerprint === initial.state.fingerprint &&
    replaced.receipt.before_manifest_fingerprint === initial.manifest.fingerprint &&
    replaced.receipt.after_manifest_fingerprint === replaced.candidate.fingerprint &&
    replaced.receipt.before_sequence === 1 && replaced.receipt.after_sequence === 2 &&
    replaced.receipt.before_implementation.implementation === "browser-esm:replaceable-v1.js" &&
    replaced.receipt.after_implementation.implementation === "browser-esm:replaceable-v2.js" &&
    !JSON.stringify(replaced.receipt).includes("replaceableProvider"));
  check("successful replacement remounts only the exact dependency closure",
    replaced.v1Disposals === 2 && replaced.v2Mounts === 1 &&
    replaced.consumerMounts === 3 && replaced.consumerDisposals === 2);
  check("manifest getter advances to the sealed candidate",
    replaced.manifest.fingerprint === replaced.candidate.fingerprint &&
    replaced.manifest.implementations.find((row) => row.entry === "replaceable").implementation ===
      "browser-esm:replaceable-v2.js");
  check("no uncaught browser exception", exceptions.length === 0, exceptions.join("; "));

  await evaluate(`window.__openrealtime.dispose()`);
  const closed = await evaluate(`(() => ({
    live: window.__openrealtime.live(), mounted: window.__openrealtime.mounted,
    children: document.getElementById("openrealtime-root").childElementCount,
    provider: document.getElementById("openrealtime-root").dataset.replaceableProvider ?? "",
    consumer: document.getElementById("openrealtime-root").dataset.replaceableConsumer ?? "",
  }))()`);
  check("replaced composition closes with zero scoped ownership",
    closed.live.state === "closed" && closed.mounted.length === 0 && closed.children === 0 &&
    closed.provider === "" && closed.consumer === "" &&
    Object.values(closed.live.entries).every((entry) => entry.effects === 0 && entry.services.length === 0));
} catch (error) {
  check("run completed", false, error.stack ?? error.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed`);
process.exit(failed.length ? 1 : 0);
