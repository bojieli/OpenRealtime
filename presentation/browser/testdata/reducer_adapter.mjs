import { pathToFileURL } from "node:url";

const modulePath = process.argv[2];
if (!modulePath) throw new Error("reducer adapter module path is required");
const plugin = (await import(pathToFileURL(modulePath))).default;

let inbound;
let physical;
let physicalState = "idle";
let snapshotReducerState;
let restoredReads = 0;
const outbound = [];
const connection = {
  kind: "websocket",
  state: () => physicalState,
  connect: async () => { physicalState = "connected"; },
  send: (event) => outbound.push(structuredClone(event)),
  subscribe(listener) { inbound = listener; return () => { inbound = undefined; }; },
  onState(listener) { physical = listener; return () => { physical = undefined; }; },
};
const services = new Map();
const disposers = [];
await plugin.mount({
  services: { get: (name) => name === "presentation.client.connection" ? connection : undefined },
  publish: (name, value) => services.set(name, value),
  state: {
    restored() {
      restoredReads++;
      if (restoredReads !== 1) throw new Error("reducer restored state was consumed more than once");
      return undefined;
    },
    snapshot(callback) {
      if (typeof callback !== "function" || snapshotReducerState) {
        throw new Error("reducer snapshot provider registration is invalid");
      }
      snapshotReducerState = callback;
    },
  },
  lifecycle: { defer(_name, dispose) { disposers.push(dispose); } },
});
if (typeof plugin.migrateState !== "function" || typeof snapshotReducerState !== "function" ||
    restoredReads !== 1) {
  throw new Error("reducer adapter did not expose its declared state lifecycle");
}
for (const name of [
  "presentation.client.session_state", "presentation.client.inspection_access",
  "presentation.client.protocol_events", "presentation.client.strict_json",
]) if (!services.has(name)) throw new Error(`missing reducer adapter service ${name}`);

const state = services.get("presentation.client.session_state");
const access = services.get("presentation.client.inspection_access");
const protocol = services.get("presentation.client.protocol_events");
const codec = services.get("presentation.client.strict_json");
let duplicate = "";
try { codec.parse('{"value":1,"value":2}'); } catch (error) { duplicate = error.message; }
if (!duplicate.includes("duplicate JSON key")) throw new Error("strict codec accepted duplicate keys");

// One faulty projection must not prevent later plugins from observing the
// same accepted state/event or tear down the transport callback.
const stopBadState = state.subscribe(() => { throw new Error("faulty state projection"); });
const stopBadAccess = access.subscribe(() => { throw new Error("faulty inspection projection"); });
const stopBadEvents = protocol.subscribe(() => { throw new Error("faulty protocol projection"); });
const events = [];
const stopEvents = protocol.subscribe((event) => events.push(event));
await state.connect();
inbound('{"type":"session.created","session":{"id":"session-adapter"}}');
if (events.length !== 1 || events[0].type !== "session.created" ||
    state.snapshot().session.id !== "session-adapter") {
  throw new Error("accepted event did not cross reducer and protocol service atomically");
}
inbound('{"type":"response.output_text.delta","response_id":"absent","item_id":"bad","delta":"x"}');
if (events.length !== 1 || state.snapshot().conversation.length !== 0 || !state.diagnostic()) {
  throw new Error("reducer-rejected event reached a consumer or mutated canonical state");
}

const malformedAccess = JSON.stringify({
  type: "session.updated",
  session: { id: "session-adapter", openrealtime: { debug: { enabled: true, inspection: {
    session_id: "session-adapter", path: "/openrealtime/v1/sessions/session-adapter/live",
    token: "not-a-capability", expires_at_ms: Date.now() + 60_000,
  } } } },
});
inbound(malformedAccess);
if (events.length !== 1 || access.current() !== null) {
  throw new Error("malformed inspection authority escaped before canonical event admission");
}
const stalePathAccess = JSON.stringify({
  type: "session.updated",
  session: { id: "session-adapter", openrealtime: { debug: { enabled: true, inspection: {
    session_id: "session-adapter", path: "/v1/realtime/sessions/session-adapter/live",
    token: "mgmt_valid-capability", expires_at_ms: Date.now() + 60_000,
  } } } },
});
inbound(stalePathAccess);
if (events.length !== 1 || access.current() !== null) {
  throw new Error("stale negotiated management path escaped before canonical event admission");
}

const validAccess = JSON.stringify({
  type: "session.updated",
  session: { id: "session-adapter", openrealtime: {
    version: 1, enabled: [], observers: [], available_observers: [],
    debug: { enabled: true, inspection: {
      session_id: "session-adapter", path: "/openrealtime/v1/sessions/session-adapter/live",
      token: "mgmt_valid-capability", expires_at_ms: Date.now() + 60_000,
    } },
  } },
});
inbound(validAccess);
if (events.length !== 2 || access.current()?.token !== "mgmt_valid-capability") {
  throw new Error("accepted inspection event did not publish its scoped capability");
}
events[1].session.id = "mutated-consumer-copy";
if (state.snapshot().session.id !== "session-adapter") {
  throw new Error("protocol event consumer mutated reducer state");
}

const captured = snapshotReducerState();
if (captured.machine.connection.phase !== "connected" ||
    captured.machine.session.id !== "session-adapter" ||
    captured.outbound.length !== captured.command_cursor || captured.local_item !== 0 ||
    captured.inspection_access?.token !== "mgmt_valid-capability" ||
    JSON.stringify(state.snapshot()).includes("mgmt_valid-capability")) {
  throw new Error("reducer adapter did not capture its bounded private safe-point state");
}
const schema = {
  name: "presentation.client.reducer.state",
  revision: 1,
  digest: "sha256:640ca5e7a3fcb2638dd114be3affa7035514eadcb037c77c323b32abad906f26",
};
const migrated = await plugin.migrateState({
  entry: "reducer",
  schema,
  source_implementation: "browser-esm:reducer.js",
  snapshot: structuredClone(captured),
});
if (JSON.stringify(migrated) !== JSON.stringify(captured)) {
  throw new Error("reducer adapter migration changed canonical state");
}
const rejectsMigration = async (snapshot, message) => {
  let rejected = false;
  try {
    await plugin.migrateState({
      entry: "reducer", schema, source_implementation: "browser-esm:reducer.js", snapshot,
    });
  } catch {
    rejected = true;
  }
  if (!rejected) throw new Error(message);
};
const extraField = structuredClone(captured);
extraField.unexpected = true;
await rejectsMigration(extraField, "reducer migration accepted an unknown state field");
const reconnecting = structuredClone(captured);
reconnecting.machine.connection.phase = "reconnecting";
await rejectsMigration(reconnecting, "reducer migration accepted an in-flight reconnect");
const unsent = structuredClone(captured);
unsent.outbound.push({ type: "response.create" });
await rejectsMigration(unsent, "reducer migration accepted an unsent command");

physicalState = "closed";
physical("closed", "test loss");
if (access.current() !== null || state.snapshot().connection.phase !== "reconnecting") {
  throw new Error("transport loss did not revoke inspection authority");
}
stopEvents();
stopBadEvents();
stopBadAccess();
stopBadState();
for (const dispose of [...disposers].reverse()) await dispose();
if (inbound !== undefined || physical !== undefined || access.current() !== null) {
  throw new Error("reducer adapter retained effects or authority after disposal");
}

console.log("reducer adapter boundary passed");
