import { pathToFileURL } from "node:url";

const [operatorPath, transportPath, staticPath, authoringPath, workspacePath, reducerPath] = process.argv.slice(2);
if (![operatorPath, transportPath, staticPath, authoringPath, workspacePath, reducerPath].every(Boolean)) {
  throw new Error("management client module paths are required");
}

Object.defineProperty(globalThis, "location", {
  value: Object.freeze({ origin: "http://127.0.0.1:17777" }), configurable: true,
});
const reducer = await import(pathToFileURL(reducerPath));
const codec = Object.freeze({ parse: reducer.parseStrictJSON, stable: reducer.stableJSON });
const manifest = Object.freeze({ endpoints: [
  { name: "management.static", method: "GET", path: "/client/v1/management",
    protocol: "openrealtime.management.v1" },
  { name: "management.authoring", method: "POST", path: "/client/v1/management/authoring",
    protocol: "openrealtime.management.v1" },
] });
const services = new Map([
  ["presentation.client.strict_json", codec],
  // This token is deliberately present to prove the authoring subtree has no
  // dependency on the session-scoped inspection capability.
  ["presentation.client.inspection_access", Object.freeze({
    current: () => ({ token: "mgmt_session_must_not_cross" }), subscribe: () => () => {},
  })],
]);
const mounted = [];

async function mount(path, permissions = []) {
  const published = [];
  const disposers = [];
  const plugin = (await import(pathToFileURL(path))).default;
  await plugin.mount({
    manifest,
    permissions: { allows: (kind, resource, operation) => permissions.some((permission) =>
      permission.kind === kind && permission.resource === resource && permission.operations.includes(operation)) },
    services: { get: (name) => services.get(name) },
    publish(name, value) { services.set(name, value); published.push(name); },
    lifecycle: { defer(_name, dispose) { disposers.push(dispose); } },
  });
  const dispose = async () => {
    for (const disposer of [...disposers].reverse()) await disposer();
    for (const name of published) services.delete(name);
  };
  mounted.push(dispose);
  return { plugin, dispose };
}

const operatorMount = await mount(operatorPath, [{
  kind: "credential.use", resource: "management-operator", operations: ["header"],
}]);
await mount(transportPath, [{
  kind: "network.connect", resource: "host-management", operations: ["static", "authoring"],
}]);
await mount(staticPath);
await mount(authoringPath);
await mount(workspacePath);

const control = services.get("presentation.client.management_operator_control");
const catalog = services.get("presentation.client.management_static");
const authoring = services.get("presentation.client.management_authoring");
const workspace = services.get("presentation.client.authoring_workspace");
if (!control || !catalog || !authoring || !workspace) throw new Error("management services were not published");

const operatorOne = "operator_secret_one";
const operatorTwo = "operator_secret_two";
control.replace(operatorOne, Date.now() + 60_000);
if (JSON.stringify(control.status()).includes(operatorOne) || JSON.stringify(workspace.snapshot()).includes(operatorOne)) {
  throw new Error("a serialized management projection retained the operator capability");
}

const handlers = [];
const requests = [];
globalThis.fetch = async (url, options) => {
  requests.push({ url: String(url), options });
  const handler = handlers.shift();
  if (!handler) throw new Error("unexpected management request");
  return handler(String(url), options);
};

function response(payload, identity, headers = {}) {
  const body = typeof payload === "string" ? payload : JSON.stringify(payload);
  return new Response(body, { status: 200, headers: {
    "Content-Type": "application/json", "Content-Length": String(new TextEncoder().encode(body).byteLength),
    "OpenRealtime-Management-Identity": identity, ...headers,
  } });
}

const graphDigest = `sha256:${"a".repeat(64)}`;
const otherDigest = `sha256:${"b".repeat(64)}`;
const elementDigest = `sha256:${"c".repeat(64)}`;
const pluginDigest = `sha256:${"d".repeat(64)}`;
const graph = {
  format_version: 1, id: "fixture", revision: 1, fingerprint: graphDigest,
  nodes: [{ id: "node", element: { name: "test.Element", revision: 1, digest: elementDigest },
    ports: [{ name: "out", direction: "output", type: { kind: "event", element: { kind: "named", name: "test.Value" } },
      cardinality: "one", default_depth: 1 }] }],
};

handlers.push(() => response(graph, `graph:${graphDigest}`));
const exactGraph = await catalog.graph(graphDigest);
const first = requests.at(-1);
if (exactGraph.fingerprint !== graphDigest || !first.url.endsWith(`/graphs/sha256%3A${"a".repeat(64)}`) ||
    first.options.headers["OpenRealtime-Management-Token"] !== operatorOne || first.options.redirect !== "error" ||
    first.options.referrerPolicy !== "no-referrer" || first.url.includes(operatorOne) ||
    JSON.stringify(first.options.body ?? "").includes(operatorOne)) {
  throw new Error("static graph read did not preserve its exact identity and authority boundary");
}

handlers.push(() => response({ ...graph, fingerprint: otherDigest }, `graph:${graphDigest}`));
await catalog.graph(graphDigest).then(
  () => { throw new Error("mismatched graph response was accepted"); },
  (error) => { if (!String(error).includes("identity")) throw error; },
);

const duplicate = JSON.stringify(graph).replace('{"format_version":1', '{"format_version":1,"format_version":1');
handlers.push(() => response(duplicate, `graph:${graphDigest}`));
await catalog.graph(graphDigest).then(
  () => { throw new Error("duplicate-key management JSON was accepted"); },
  () => {},
);

handlers.push(() => response({}, `graph:${graphDigest}`, { "Content-Length": String((64 << 20) + 1) }));
await catalog.graph(graphDigest).then(
  () => { throw new Error("oversized management response was accepted"); },
  (error) => { if (!String(error).includes("limit")) throw error; },
);

handlers.push(() => new Response("{}", { status: 200, headers: {
  "Content-Type": "text/plain", "OpenRealtime-Management-Identity": `graph:${graphDigest}`,
} }));
await catalog.graph(graphDigest).then(
  () => { throw new Error("non-JSON management response was accepted"); },
  () => {},
);

const elementDescriptor = { format_version: 1, name: "test.Element", revision: 1,
  ports: [{ name: "out", direction: "output", type: { kind: "event", element: { kind: "named", name: "test.Value" } },
    cardinality: "one" }], reaction: {} };
handlers.push(() => response(elementDescriptor, `element:test.Element@1:${elementDigest}`));
await catalog.element({ name: "test.Element", revision: 1, digest: elementDigest });

const pluginDescriptor = { format_version: 1, name: "test.Plugin", revision: 1,
  realm: "client", platforms: ["browser"], provides: [{ name: "test.Service", revision: 1, digest: pluginDigest }] };
handlers.push(() => response(pluginDescriptor, `plugin:test.Plugin@1:${pluginDigest}`));
await catalog.plugin({ name: "test.Plugin", revision: 1, digest: pluginDigest });

const schemaText = '{"type":"object"}\n';
const schemaHash = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(schemaText)));
const schemaDigest = `sha256:${[...schemaHash].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
handlers.push(() => response({ format_version: 1, schema: schemaText, digest: schemaDigest,
  complete: true, nodes: [], contracts: [], unresolved: [] }, `schema:${graphDigest}:${schemaDigest}`));
const schema = await catalog.valuesSchema(graphDigest);
if (schema.digest !== schemaDigest) throw new Error("values schema was not rebound to its content digest");

const source = "graph fixture {\n}\n";
const sourceHash = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(source)));
const sourceDigest = `sha256:${[...sourceHash].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
const analysis = {
  source_digest: sourceDigest, parsed: true, recovered: false, canonical: true,
  diagnostics: { items: [], total: 0 },
  catalog: { elements: [{
    identity: { name: "test.Element", revision: 1, digest: elementDigest }, topology_declaration: "test.Element",
    ports: [], reaction: {}, config: { artifact: "schema://test.Element", resolved: true,
      inline_topology_values: false, empty_object_only: false, schema_status: "resolved",
      schema_reference: "schema://test.Element", schema_id: "test.Element", schema_digest: schemaDigest,
      properties_complete: true, properties: [{ name: "safe", pointer: "#/properties/safe",
        types: ["string"], schema: { type: "string" } }] },
  }], total: 1 },
  formatting: { path: "fixture.ortg", source_digest: sourceDigest, edits: [] },
};
handlers.push(() => response(analysis, `authoring:analyze:${sourceDigest}`));
const analyzed = await authoring.analyze({ path: "fixture.ortg", source, revision: 1 });
if (analyzed.source_digest !== sourceDigest) throw new Error("analysis was not rebound to source identity");
const analyzeRequest = requests.at(-1);
if (analyzeRequest.options.headers["OpenRealtime-Management-Token"] !== operatorOne ||
    analyzeRequest.options.body.includes(operatorOne) || analyzeRequest.url.includes(operatorOne) ||
    analyzeRequest.options.body.includes("mgmt_session_must_not_cross")) {
  throw new Error("authoring request disclosed or reused a session capability");
}
handlers.push(() => response(analysis, `authoring:analyze:${otherDigest}`));
await authoring.analyze({ path: "fixture.ortg", source, revision: 1 }).then(
  () => { throw new Error("analysis with mismatched host evidence was accepted"); },
  (error) => { if (!String(error).includes("identity")) throw error; },
);

const compiled = { graph, lock: { format_version: 1, elements: [{ reference: "test.Element",
  identity: { name: "test.Element", revision: 1, digest: elementDigest } }] } };
handlers.push(() => response(compiled, `authoring:compile:${graphDigest}`));
const compileResult = await authoring.compile({ path: "fixture.ortg", source, revision: 1 });
if (compileResult.graph.fingerprint !== graphDigest) throw new Error("compile result lost graph identity");

handlers.push(() => response({ fingerprint: graphDigest, format: "model", model: { nodes: [] } },
  `authoring:render:model:${graphDigest}`));
const rendered = await authoring.render(graph, "model");
if (rendered.fingerprint !== graphDigest) throw new Error("render result lost graph identity");
handlers.push(() => response({ fingerprint: graphDigest, format: "model", model: { nodes: [] } },
  `authoring:render:model:${otherDigest}`));
await authoring.render(graph, "model").then(
  () => { throw new Error("rendering with mismatched host evidence was accepted"); },
  (error) => { if (!String(error).includes("identity")) throw error; },
);

const operatorExpiring = "operator_secret_expiring";
control.replace(operatorExpiring, Date.now() + 5);
await new Promise((resolve) => setTimeout(resolve, 20));
const beforeExpiredRead = requests.length;
await catalog.graph(graphDigest).then(
  () => { throw new Error("expired operator capability remained usable"); },
  (error) => { if (!String(error).includes("unavailable")) throw error; },
);
if (requests.length !== beforeExpiredRead || control.status().available) {
  throw new Error("operator capability expiry reached fetch or stayed observable");
}
control.replace(operatorOne, Date.now() + 60_000);

handlers.push((_url, options) => new Promise((_resolve, reject) => {
  options.signal.addEventListener("abort", () => reject(options.signal.reason ??
    new DOMException("aborted", "AbortError")), { once: true });
}));
const pending = catalog.graph(graphDigest).then(
  () => { throw new Error("rotated operator request unexpectedly completed"); },
  (error) => error,
);
control.replace(operatorTwo, Date.now() + 120_000);
const canceled = await pending;
if (canceled?.name !== "AbortError" || JSON.stringify(control.status()).includes(operatorTwo)) {
  throw new Error("operator capability rotation did not cancel reads without disclosure");
}

handlers.push((_url, options) => new Promise((_resolve, reject) => {
  options.signal.addEventListener("abort", () => reject(options.signal.reason ??
    new DOMException("aborted", "AbortError")), { once: true });
}));
const lost = catalog.graph(graphDigest).then(
  () => { throw new Error("provider-loss request unexpectedly completed"); },
  (error) => error,
);
await operatorMount.dispose();
const providerLoss = await lost;
if (providerLoss?.name !== "AbortError" || control.status().available) {
  throw new Error("operator provider loss did not revoke in-flight authority");
}

for (const dispose of mounted.slice(1).reverse()) await dispose();
await catalog.graph(graphDigest).then(
  () => { throw new Error("disposed static management service remained usable"); },
  () => {},
);
const serialized = JSON.stringify({ manifest, workspace: workspace.snapshot?.(), status: control.status() });
for (const secret of [operatorOne, operatorTwo, operatorExpiring, "mgmt_session_must_not_cross"]) {
  if (serialized.includes(secret) || requests.some((request) => request.url.includes(secret))) {
    throw new Error("management capability escaped into URL, manifest, or serialized live state");
  }
}
