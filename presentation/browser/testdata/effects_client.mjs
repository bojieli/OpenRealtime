import { pathToFileURL } from "node:url";

const [effectsPath, artifactsPath, reducerPath] = process.argv.slice(2);
if (!effectsPath || !artifactsPath || !reducerPath) throw new Error("effects, artifacts, and reducer modules are required");
const effectsPlugin = (await import(pathToFileURL(effectsPath))).default;
const artifactsPlugin = (await import(pathToFileURL(artifactsPath))).default;
const { parseStrictJSON, stableJSON } = await import(pathToFileURL(reducerPath));

globalThis.location = new URL("http://127.0.0.1:8767/");

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances = [];
  constructor(url) {
    this.url = url;
    this.readyState = FakeWebSocket.CONNECTING;
    this.listeners = new Map();
    this.sent = [];
    FakeWebSocket.instances.push(this);
  }
  addEventListener(type, listener) {
    const rows = this.listeners.get(type) ?? [];
    rows.push(listener);
    this.listeners.set(type, rows);
  }
  emit(type, value = {}) {
    for (const listener of this.listeners.get(type) ?? []) listener(value);
  }
  open() { this.readyState = FakeWebSocket.OPEN; this.emit("open"); }
  message(value) { this.emit("message", { data: typeof value === "string" ? value : JSON.stringify(value) }); }
  send(value) {
    if (this.readyState !== FakeWebSocket.OPEN) throw new Error("socket is not open");
    this.sent.push(value);
  }
  close() {
    if (this.readyState === FakeWebSocket.CLOSED) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.emit("close");
  }
}
globalThis.WebSocket = FakeWebSocket;

let current = {
  connection: { phase: "disconnected" },
  session: { id: "", openrealtime: { enabled: [] } },
};
let stateListener;
const toolResults = [];
const state = {
  snapshot: () => structuredClone(current),
  subscribe(listener) {
    stateListener = listener;
    listener(structuredClone(current));
    return () => { stateListener = undefined; };
  },
  toolResult(callID, status, output, error) {
    toolResults.push({ callID, status, output, error });
  },
};
let eventListener;
const events = {
  subscribe(listener) { eventListener = listener; return () => { eventListener = undefined; }; },
};
const contributions = new Map();
let contributionSerial = 0;
const configuration = {
  contribute(fragment) {
    const id = ++contributionSerial;
    contributions.set(id, structuredClone(fragment));
    return () => contributions.delete(id);
  },
};
const codec = { parse: parseStrictJSON, stable: stableJSON };
const catalogDigest = `sha256:${"a".repeat(64)}`;
let effects;
const effectDisposers = [];
await effectsPlugin.mount({
  manifest: { endpoints: [{
    name: "effects.local", method: "GET", path: "/client/v1/effects",
    protocol: "openrealtime.client-effects.v1", catalog_digest: catalogDigest,
  }] },
  services: { get(name) {
    return ({
      "presentation.client.session_state": state,
      "presentation.client.protocol_events": events,
      "presentation.client.strict_json": codec,
      "presentation.client.session_configuration": configuration,
    })[name];
  } },
  permissions: { allows: (kind, resource, operation) =>
    kind === "network.connect" && resource === "host-effects" && operation === "websocket" },
  publish(name, value) {
    if (name !== "presentation.client.tools_effects") throw new Error(`unexpected effect service ${name}`);
    effects = value;
  },
  lifecycle: { defer(_name, dispose) { effectDisposers.push(dispose); } },
});
if (!effects || FakeWebSocket.instances.length !== 0) throw new Error("effect client opened before a realtime session");

let resources;
const artifactDisposers = [];
await artifactsPlugin.mount({
  services: { get: (name) => name === "presentation.client.tools_effects" ? effects : undefined },
  publish(name, value) {
    if (name !== "presentation.client.artifacts") throw new Error(`unexpected resource service ${name}`);
    resources = value;
  },
  lifecycle: { defer(_name, dispose) { artifactDisposers.push(dispose); } },
});
if (!resources) throw new Error("artifact service was not published");

const declaration = {
  name: "display_artifact", description: "Display one isolated artifact.",
  parameters: {
    type: "object", properties: { artifact_id: { type: "string" } },
    required: ["artifact_id"], additionalProperties: false,
  },
  host_confirmation: "always", session_confirmation: "never",
  mutating: false, channel: "artifact",
  digest: `sha256:${"1".repeat(64)}`,
};
const ready = {
  type: "ready", version: 1, scope_id: "0123456789abcdef0123456789abcdef",
  catalog_digest: catalogDigest, tools: [declaration],
  limits: {
    max_message_bytes: 65536, max_result_bytes: 8192, max_in_flight: 4,
    max_calls: 16, confirmation_timeout_ms: 1000, execution_timeout_ms: 1000,
  },
};

current = {
  connection: { phase: "connected" },
  session: { id: "sess_one", openrealtime: { enabled: [] } },
};
stateListener(structuredClone(current));
if (FakeWebSocket.instances.length !== 1 || FakeWebSocket.instances[0].url !== "ws://127.0.0.1:8767/client/v1/effects") {
  throw new Error("effect socket did not use the exact locked same-origin endpoint");
}
const first = FakeWebSocket.instances[0];
first.open();
first.message(ready);
if (contributions.size !== 1) throw new Error("ready did not contribute negotiation support");
let fragment = [...contributions.values()][0];
if (JSON.stringify(fragment.supports) !== JSON.stringify(["client.effects"]) || fragment.tools.length !== 0) {
  throw new Error("tools were advertised before server negotiation");
}

current.session.openrealtime.enabled = ["client.effects"];
stateListener(structuredClone(current));
fragment = [...contributions.values()][0];
if (fragment.tools.length !== 1 || fragment.tools[0].openrealtime.client_effect.declaration_digest !== declaration.digest ||
    fragment.tools[0].openrealtime.confirm !== "never") {
  throw new Error("host declarations were not projected exactly after negotiation");
}
if (effects.snapshot().negotiated) throw new Error("effect client reported negotiation before server acknowledgement");
eventListener({
  type: "session.updated",
  session: {
    openrealtime: { enabled: ["client.effects"] },
    tools: [{
      name: declaration.name,
      openrealtime: { client_effect: { version: 1, declaration_digest: declaration.digest } },
    }],
  },
});
if (!effects.snapshot().negotiated) throw new Error("effect client did not expose acknowledged declarations");

eventListener({
  type: "response.function_call_arguments.done", call_id: "call_missing", name: declaration.name,
  arguments: "{\"artifact_id\":\"one\"}",
});
if (first.sent.length !== 0 || toolResults.at(-1)?.status !== "failed" ||
    !toolResults.at(-1)?.error.includes("authority")) {
  throw new Error("effect call without server authority did not fail closed");
}

const call = {
  type: "response.function_call_arguments.done", call_id: "call_one", name: declaration.name,
  arguments: "{\"artifact_id\":\"one\"}",
  openrealtime: { client_effect: {
    version: 1, declaration_digest: declaration.digest, authority: "opaque.receipt",
  } },
};
eventListener(call);
if (first.sent.length !== 1) throw new Error("authorized effect call was not forwarded");
const forwarded = parseStrictJSON(first.sent[0]);
if (forwarded.type !== "call" || forwarded.session_id !== "sess_one" || forwarded.authority !== "opaque.receipt" ||
    stableJSON(forwarded.arguments) !== "{\"artifact_id\":\"one\"}") {
  throw new Error(`forwarded effect call is wrong: ${first.sent[0]}`);
}

first.message({
  type: "confirm", id: "call_one", name: declaration.name, arguments: { artifact_id: "one" },
  nonce: "abcdef0123456789abcdef0123456789", confirm: "always", channel: "artifact",
});
if (effects.snapshot().confirmations.length !== 1) throw new Error("confirmation was not exposed as state");
effects.decide("call_one", true);
const decision = parseStrictJSON(first.sent.at(-1));
if (decision.type !== "decide" || decision.nonce !== "abcdef0123456789abcdef0123456789" || decision.approved !== true) {
  throw new Error("confirmation view API did not return the provider nonce");
}

const artifact = {
  id: "one", title: "One", path: "/client/v1/artifacts/one",
  digest: `sha256:${"2".repeat(64)}`, bytes: 42, version: 1,
  updated_at: "2026-08-29T00:00:00Z",
};
first.message({ type: "result", id: "call_one", channel: "artifact", output: "displayed", artifact });
if (toolResults.at(-1)?.status !== "done" || toolResults.at(-1)?.output !== "displayed" ||
    resources.snapshot().artifacts[0]?.digest !== artifact.digest) {
  throw new Error("effect result and artifact reference did not reach independent services");
}
first.close();
if (effects.snapshot().negotiated) throw new Error("provider loss retained stale negotiation readiness");

// A realtime session replacement must fence the old socket and create a new
// provider scope; the old socket cannot inject a terminal result afterwards.
current = {
  connection: { phase: "connected" },
  session: { id: "sess_two", openrealtime: { enabled: [] } },
};
stateListener(structuredClone(current));
if (first.readyState !== FakeWebSocket.CLOSED || FakeWebSocket.instances.length !== 2 || contributions.size !== 0 ||
    effects.snapshot().negotiated) {
  throw new Error("realtime session replacement did not replace the effect scope");
}
const beforeStale = toolResults.length;
first.message({ type: "result", id: "call_one", output: "stale" });
if (toolResults.length !== beforeStale) throw new Error("a replaced effect socket mutated client state");

const second = FakeWebSocket.instances[1];
second.open();
second.message({ ...ready, catalog_digest: `sha256:${"b".repeat(64)}` });
if (second.readyState !== FakeWebSocket.CLOSED || contributions.size !== 0 ||
    !effects.snapshot().diagnostic.includes("ready")) {
  throw new Error("effect socket accepted a catalog that was not pinned by its manifest");
}
current = {
  connection: { phase: "disconnected" },
  session: { id: "", openrealtime: { enabled: [] } },
};
stateListener(structuredClone(current));

for (const dispose of [...artifactDisposers].reverse()) await dispose();
for (const dispose of [...effectDisposers].reverse()) await dispose();
if (stateListener !== undefined || eventListener !== undefined || contributions.size !== 0 ||
    effects.snapshot().phase !== "closed" || resources.snapshot().artifacts.length !== 0) {
  throw new Error("effect client resources survived scoped disposal");
}

console.log("effect client composition passed");
