import { pathToFileURL } from "node:url";

const modulePath = process.argv[2];
if (!modulePath) throw new Error("inspection client module path is required");

Object.defineProperty(globalThis, "location", {
  value: Object.freeze({ origin: "http://127.0.0.1:17777" }), configurable: true,
});

let access = Object.freeze({
  session_id: "sess_one", path: "/openrealtime/v1/sessions/sess_one/live",
  token: "mgmt_secret_one", expires_at_ms: Date.now() + 60_000,
});
let updateAccess;
const accessSource = Object.freeze({
  current: () => access,
  subscribe(listener) {
    updateAccess = listener;
    listener(access);
    return () => { updateAccess = undefined; };
  },
});
const codec = Object.freeze({ parse: JSON.parse });
const disposers = [];
const published = new Map();
const context = {
  manifest: { endpoints: [{
    name: "management.sessions", method: "GET", path: "/client/v1/management/sessions",
    protocol: "openrealtime.management.v1",
  }] },
  permissions: { allows: (kind, resource, operation) =>
    kind === "network.connect" && resource === "host-management" && operation === "http" },
  services: { get: (name) => new Map([
    ["presentation.client.inspection_access", accessSource],
    ["presentation.client.strict_json", codec],
  ]).get(name) },
  publish(name, service) { published.set(name, service); },
  lifecycle: { defer(_name, dispose) { disposers.push(dispose); } },
};

const requests = [];
let holdNext = false;
let nextResponse;
globalThis.fetch = async (url, options) => {
  requests.push({ url: String(url), options });
  if (holdNext) {
    holdNext = false;
    return new Promise((_resolve, reject) => {
      options.signal.addEventListener("abort", () => reject(options.signal.reason ??
        new DOMException("aborted", "AbortError")), { once: true });
    });
  }
  if (nextResponse) {
    const response = nextResponse;
    nextResponse = undefined;
    return response;
  }
  return new Response(JSON.stringify({ state: "running" }), {
    status: 200, headers: { "Content-Type": "application/json", "Content-Length": "19" },
  });
};

const plugin = (await import(pathToFileURL(modulePath))).default;
await plugin.mount(context);
const inspection = published.get("presentation.client.inspection");
if (!inspection) throw new Error("inspection service was not published");

let notifications = 0;
const stop = inspection.subscribe(() => { notifications++; });
for (let index = 0; index < 100; index++) updateAccess(access);
if (notifications !== 1) {
  throw new Error(`unchanged reducer snapshots produced ${notifications} capability notifications`);
}

const live = await inspection.live();
if (live.state !== "running" || requests.length !== 1 ||
    requests[0].url !== "http://127.0.0.1:17777/client/v1/management/sessions/sess_one/live" ||
    requests[0].options.headers["OpenRealtime-Management-Token"] !== "mgmt_secret_one" ||
    requests[0].options.redirect !== "error" || requests[0].options.referrerPolicy !== "no-referrer" ||
    requests[0].url.includes("mgmt_secret_one")) {
  throw new Error("inspection read did not use the exact session-scoped management API");
}
if (JSON.stringify(inspection.access()).includes("mgmt_secret_one")) {
  throw new Error("inspection projection exposed its bearer capability");
}
const model = await inspection.model();
if (model.state !== "running" || requests.length !== 2 ||
    requests[1].url !== "http://127.0.0.1:17777/client/v1/management/sessions/sess_one/model" ||
    requests[1].options.headers["OpenRealtime-Management-Token"] !== "mgmt_secret_one") {
  throw new Error("inspection model did not use the same session-scoped capability");
}

nextResponse = new Response("{}", { status: 200, headers: {
  "Content-Type": "text/plain", "Content-Length": "2",
} });
await inspection.live().then(
  () => { throw new Error("non-JSON inspection response was accepted"); },
  () => {},
);
nextResponse = new Response("{}", { status: 200, headers: {
  "Content-Type": "application/json", "Content-Length": "invalid",
} });
await inspection.live().then(
  () => { throw new Error("invalid inspection response length was accepted"); },
  () => {},
);
nextResponse = new Response("{}", { status: 200, headers: {
  "Content-Type": "application/json", "Content-Length": String((32 << 20) + 1),
} });
await inspection.live().then(
  () => { throw new Error("oversized inspection response was accepted"); },
  () => {},
);
nextResponse = new Response(JSON.stringify({ state: "model" }), { status: 200, headers: {
  "Content-Type": "application/json", "Content-Length": String((32 << 20) + 1),
} });
if ((await inspection.model()).state !== "model") {
  throw new Error("static session model was incorrectly limited to the live-snapshot ceiling");
}
const canonicalAccess = access;
access = Object.freeze({
  ...canonicalAccess, path: "/v1/realtime/sessions/sess_one/live",
  token: "mgmt_stale_path",
});
updateAccess(access);
await inspection.live().then(
  () => { throw new Error("stale negotiated management path was accepted"); },
  (error) => {
    if (!error.message.includes("another management path")) throw error;
  },
);
access = canonicalAccess;
updateAccess(access);

holdNext = true;
const pending = inspection.trace().then(
  () => { throw new Error("rotated management read unexpectedly completed"); },
  (error) => error,
);
access = Object.freeze({
  session_id: "sess_one", path: "/openrealtime/v1/sessions/sess_one/live",
  token: "mgmt_secret_two", expires_at_ms: Date.now() + 120_000,
});
updateAccess(access);
const canceled = await pending;
if (canceled?.name !== "AbortError" || notifications !== 4) {
  throw new Error("capability rotation did not cancel reads and notify exactly once");
}

stop();
for (const dispose of [...disposers].reverse()) await dispose();
if (inspection.available()) throw new Error("disposed inspection provider retained access");
