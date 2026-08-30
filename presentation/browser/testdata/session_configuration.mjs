import { pathToFileURL } from "node:url";

const modulePath = process.argv[2];
if (!modulePath) throw new Error("session configuration module path is required");
const plugin = (await import(pathToFileURL(modulePath))).default;
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

let current = {
  connection: { phase: "connected" },
  session: { id: "session-one" },
};
let stateListener;
const updates = [];
const state = {
  snapshot: () => structuredClone(current),
  subscribe(listener) {
    stateListener = listener;
    listener(structuredClone(current));
    return () => { stateListener = undefined; };
  },
  updateSession(value) { updates.push(structuredClone(value)); },
};
let service;
const disposers = [];
await plugin.mount({
  services: { get: (name) => name === "presentation.client.session_state" ? state : undefined },
  publish(name, value) {
    if (name !== "presentation.client.session_configuration") throw new Error(`unexpected service ${name}`);
    service = value;
  },
  lifecycle: { defer(_name, dispose) { disposers.push(dispose); } },
});
if (!service) throw new Error("session configuration service was not published");
let notifications = 0;
const stopBadListener = service.subscribe(() => { throw new Error("faulty configuration projection"); });
const stopGoodListener = service.subscribe(() => { notifications++; });

const removeDebug = service.contribute({ debug: { enabled: true, categories: ["session"] } });
const removeVideo = service.contribute({
  supports: ["video.input", "observations", "video.input"], observers: ["video", "audio"],
});
const tool = {
  type: "function", name: "lookup", description: "Look up an item",
  parameters: { type: "object", additionalProperties: false },
};
const removeTool = service.contribute({ tools: [tool] });
const removeSameTool = service.contribute({ tools: [structuredClone(tool)] });
await settle();
if (updates.length !== 1) throw new Error(`coalesced updates = ${updates.length}, want 1`);
const configured = updates[0];
if (JSON.stringify(configured.openrealtime.supports) !== JSON.stringify(["observations", "video.input"]) ||
    JSON.stringify(configured.openrealtime.observers) !== JSON.stringify(["audio", "video"]) ||
    configured.openrealtime.debug?.enabled !== true || configured.tools.length !== 1) {
  throw new Error(`merged configuration is wrong: ${JSON.stringify(configured)}`);
}

let conflict = "";
try {
  service.contribute({ tools: [{ ...tool, description: "a conflicting declaration" }] });
} catch (error) { conflict = error.message; }
if (!conflict.includes("conflicting declarations")) {
  throw new Error(`tool conflict was not refused: ${conflict}`);
}
if (service.snapshot().contributions !== 4) {
  throw new Error("failed contribution was retained");
}

current = { connection: { phase: "connected" }, session: { id: "session-two" } };
stateListener(structuredClone(current));
await settle();
if (updates.length !== 2 || service.snapshot().session_id !== "session-two") {
  throw new Error("new session did not receive the exact merged configuration");
}

current = { connection: { phase: "disconnected" }, session: { id: "" } };
stateListener(structuredClone(current));
removeVideo();
removeTool();
removeSameTool();
await settle();
if (updates.length !== 2) throw new Error("disconnected contribution removal emitted a wire update");
current = { connection: { phase: "connected" }, session: { id: "session-three" } };
stateListener(structuredClone(current));
await settle();
if (updates.length !== 3 || updates[2].openrealtime.supports.length !== 0 || updates[2].tools.length !== 0) {
  throw new Error("provider loss was not reconciled into the replacement session");
}
if (notifications < 2) throw new Error("faulty listener prevented later configuration projections");

removeDebug();
stopGoodListener();
stopBadListener();
for (const dispose of [...disposers].reverse()) await dispose();
if (stateListener !== undefined) throw new Error("state subscription survived disposal");
let disposed = "";
try { service.contribute({ supports: ["late"] }); } catch (error) { disposed = error.message; }
if (!disposed.includes("disposed")) throw new Error("disposed service accepted a contribution");

console.log("session configuration composition passed");
