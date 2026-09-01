import { pathToFileURL } from "node:url";

const [operatorPath, transportPath, staticPath, authoringPath, readingPath, publicationPath,
  workspacePath, reducerPath, goFixtureJSON] = process.argv.slice(2);
if (![operatorPath, transportPath, staticPath, authoringPath, readingPath, publicationPath,
  workspacePath, reducerPath, goFixtureJSON].every(Boolean)) {
  throw new Error("management client module paths are required");
}
const goFixture = JSON.parse(goFixtureJSON);

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
  kind: "network.connect", resource: "host-management",
  operations: ["static", "authoring", "source-read", "publication"],
}]);
await mount(staticPath);
await mount(authoringPath);
await mount(readingPath);
await mount(publicationPath);
await mount(workspacePath);

const control = services.get("presentation.client.management_operator_control");
const catalog = services.get("presentation.client.management_static");
const authoring = services.get("presentation.client.management_authoring");
const reading = services.get("presentation.client.source_reading");
const publication = services.get("presentation.client.source_publication");
const workspace = services.get("presentation.client.authoring_workspace");
if (!control || !catalog || !authoring || !reading || !publication || !workspace ||
    !workspace.canRead() || !workspace.canPublish()) {
  throw new Error("management services were not published");
}

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

const bytes = (value) => new TextEncoder().encode(value);
const uint64 = (value) => {
  const result = new Uint8Array(8);
  new DataView(result.buffer).setBigUint64(0, BigInt(value), false);
  return result;
};
const joined = (parts) => {
  const result = new Uint8Array(parts.reduce((total, part) => total + part.byteLength, 0));
  let offset = 0;
  for (const part of parts) { result.set(part, offset); offset += part.byteLength; }
  return result;
};
const sha256 = async (value) => {
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", value));
  return `sha256:${[...sum].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
};
async function publicationReceipt(request, cleanup = false) {
  const sourceBytes = bytes(request.source);
  const sourceDigest = await sha256(sourceBytes);
  const previous = request.mode === "update" ? request.expected_source_digest : "";
  const receipt = {
    format_version: 1, root_identity: request.root_identity, mode: request.mode, path: request.path,
    source_digest: sourceDigest, source_bytes: sourceBytes.byteLength,
  };
  if (previous) receipt.previous_source_digest = previous;
  if (cleanup) receipt.cleanup_pending = true;
  const parts = [bytes("openrealtime.management.source-write-receipt/v1\0")];
  for (const field of [request.root_identity, request.mode, request.path, previous, sourceDigest]) {
    const fieldBytes = bytes(field);
    parts.push(uint64(fieldBytes.byteLength), fieldBytes);
  }
  parts.push(uint64(sourceBytes.byteLength), Uint8Array.of(cleanup ? 1 : 0));
  receipt.receipt_digest = await sha256(joined(parts));
  return receipt;
}

async function sourceReadResult(request, source) {
  const sourceBytes = bytes(source);
  const sourceDigest = await sha256(sourceBytes);
  const result = {
    format_version: 1, root_identity: request.root_identity, path: request.path, source,
    source_digest: sourceDigest, source_bytes: sourceBytes.byteLength,
  };
  const parts = [bytes("openrealtime.management.source-read-result/v1\0")];
  for (const field of [request.root_identity, request.path, sourceDigest]) {
    const fieldBytes = bytes(field);
    parts.push(uint64(fieldBytes.byteLength), fieldBytes);
  }
  parts.push(uint64(sourceBytes.byteLength));
  result.result_digest = await sha256(joined(parts));
  return result;
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
  diagnostics: { items: [{ code: "W_FIXTURE", severity: "warning", path: "fixture.ortg",
    span: { start: { offset: 0, line: 1, column: 1 }, end: { offset: 5, line: 1, column: 6 } },
    message: "<img src=x onerror=globalThis.compromised=true>", notes: ["text-only note"] }], total: 1 },
  catalog: { elements: [{
    identity: { name: "test.Element", revision: 1, digest: elementDigest }, topology_declaration: "test.Element",
    ports: [], reaction: {}, config: { artifact: "schema://test.Element", resolved: true,
      inline_topology_values: false, empty_object_only: false, schema_status: "resolved",
      schema_reference: "schema://test.Element", schema_id: "test.Element", schema_digest: schemaDigest,
      properties_complete: true, properties: [{ name: "safe", pointer: "#/properties/safe",
        types: ["string"], title: "Safe value", description: "Exact metadata", format: "uri-reference",
        default: null, enum: [null, "safe"], schema: { type: "string" } }],
      additional_properties: false },
  }], total: 1 },
  formatting: { path: "fixture.ortg", source_digest: sourceDigest, edits: [] },
};
handlers.push(() => response(analysis, `authoring:analyze:${sourceDigest}`));
const analyzed = await authoring.analyze({ path: "fixture.ortg", source, revision: 1 });
if (analyzed.source_digest !== sourceDigest) throw new Error("analysis was not rebound to source identity");
if (analyzed.catalog.elements[0].config.properties[0].default !== null ||
    analyzed.catalog.elements[0].config.additional_properties !== false) {
  throw new Error("complete configuration metadata did not survive the client boundary");
}
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

const malformedDiagnostic = structuredClone(analysis);
malformedDiagnostic.diagnostics.items[0].span.start.line = 2;
handlers.push(() => response(malformedDiagnostic, `authoring:analyze:${sourceDigest}`));
await authoring.analyze({ path: "fixture.ortg", source, revision: 1 }).then(
  () => { throw new Error("diagnostic with a forged source position was accepted"); },
  (error) => { if (!String(error).includes("source boundary")) throw error; },
);

const malformedAnalysis = structuredClone(analysis);
malformedAnalysis.catalog.elements[0].config.properties[0].pointer = "#/properties/other";
handlers.push(() => response(malformedAnalysis, `authoring:analyze:${sourceDigest}`));
await authoring.analyze({ path: "fixture.ortg", source, revision: 1 }).then(
  () => { throw new Error("forged configuration property metadata was accepted"); },
  (error) => { if (!String(error).includes("canonical")) throw error; },
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

const rootIdentity = `sha256:${"e".repeat(64)}`;
const createRequest = {
  format_version: 1, root_identity: rootIdentity, mode: "create", path: "browser/created.ortg",
  source: "graph browser_created {\n}\n",
};
const createReceipt = await publicationReceipt(createRequest);
handlers.push(() => response(createReceipt, `authoring:write:${createReceipt.receipt_digest}`));
const created = await publication.publish(createRequest);
const createFetch = requests.at(-1);
if (created.source_digest !== createReceipt.source_digest || created.previous_source_digest !== "" ||
    !createFetch.url.endsWith("/authoring/write") ||
    createFetch.options.headers["OpenRealtime-Management-Token"] !== operatorOne ||
    createFetch.options.body.includes(operatorOne) || createFetch.url.includes(rootIdentity)) {
  throw new Error("source create did not preserve its resource-scoped authority and receipt identity");
}

const forgedReceipt = { ...createReceipt, receipt_digest: otherDigest };
handlers.push(() => response(forgedReceipt, `authoring:write:${otherDigest}`));
await publication.publish(createRequest).then(
  () => { throw new Error("forged source publication receipt was accepted"); },
  (error) => { if (!String(error).includes("receipt")) throw error; },
);

const updatedSource = "graph browser_updated {\n}\n";
const updateRequest = { ...createRequest, mode: "update", source: updatedSource,
  expected_source_digest: created.source_digest };
const updateReceipt = await publicationReceipt(updateRequest, true);
handlers.push(() => response(updateReceipt, `authoring:write:${updateReceipt.receipt_digest}`));
workspace.setDocument(updateRequest.path, updatedSource, 1);
await workspace.publish("update", rootIdentity, created.source_digest);
const publishedSnapshot = workspace.snapshot();
if (publishedSnapshot.phase !== "published" ||
    publishedSnapshot.publication?.receipt_digest !== updateReceipt.receipt_digest ||
    !publishedSnapshot.publication.cleanup_pending) {
  throw new Error("authoring workspace did not retain the exact payload-free publication receipt");
}

const readRequest = {
  format_version: 1, root_identity: rootIdentity, path: updateRequest.path,
};
const expectedRead = await sourceReadResult(readRequest, updatedSource);
handlers.push(() => response(expectedRead, `authoring:read:${expectedRead.result_digest}`));
const read = await reading.read(readRequest);
const readFetch = requests.at(-1);
if (read.source !== updatedSource || read.source_digest !== updateReceipt.source_digest ||
    !readFetch.url.endsWith("/authoring/read") || readFetch.url.includes(rootIdentity) ||
    readFetch.options.headers["OpenRealtime-Management-Token"] !== operatorOne ||
    readFetch.options.body.includes(operatorOne)) {
  throw new Error("source read did not preserve exact bytes and its resource-scoped authority boundary");
}

const forgedRead = { ...expectedRead, source_digest: otherDigest };
handlers.push(() => response(forgedRead, `authoring:read:${expectedRead.result_digest}`));
await reading.read(readRequest).then(
  () => { throw new Error("forged source-read result was accepted"); },
  (error) => { if (!String(error).includes("source digest")) throw error; },
);

workspace.setDocument(updateRequest.path, "graph unsaved_local_text {\n}\n", 1);
handlers.push(() => response(expectedRead, `authoring:read:${expectedRead.result_digest}`));
await workspace.load(rootIdentity, updateRequest.path);
const loadedSnapshot = workspace.snapshot();
if (loadedSnapshot.phase !== "loaded" || loadedSnapshot.document.source !== updatedSource ||
    loadedSnapshot.sourceRead?.source_digest !== expectedRead.source_digest ||
    Object.hasOwn(loadedSnapshot.sourceRead ?? {}, "source") || loadedSnapshot.publication !== null) {
  throw new Error("authoring workspace did not replace local text with exact payload-free read evidence");
}

handlers.push(() => response(goFixture.read_result, goFixture.read_evidence));
const crossLanguageRead = await reading.read(goFixture.read_request);
if (crossLanguageRead.result_digest !== goFixture.read_result.result_digest ||
    crossLanguageRead.source !== goFixture.read_result.source ||
    crossLanguageRead.source_bytes !== bytes(goFixture.read_result.source).byteLength) {
  throw new Error("browser source-read validation diverged from the canonical Go result format");
}

handlers.push(() => response(goFixture.receipt, goFixture.evidence));
const crossLanguageReceipt = await publication.publish(goFixture.request);
if (crossLanguageReceipt.receipt_digest !== goFixture.receipt.receipt_digest ||
    crossLanguageReceipt.source_bytes !== bytes(goFixture.request.source).byteLength ||
    crossLanguageReceipt.cleanup_pending !== true) {
  throw new Error("browser publication validation diverged from the canonical Go receipt format");
}
const beforeInvalidPublish = requests.length;
await publication.publish({ ...updateRequest, expected_source_digest: updateReceipt.source_digest }).then(
  () => { throw new Error("unchanged stale-bound source update was accepted"); },
  () => {},
);
if (requests.length !== beforeInvalidPublish) {
  throw new Error("invalid source update reached the network");
}
for (const invalid of [
  { ...createRequest, path: "../escape.ortg" },
  { ...createRequest, root_identity: `sha256:${"E".repeat(64)}` },
  { ...createRequest, source: "graph \ud800 {\n}\n" },
  { ...createRequest, expected_source_digest: graphDigest },
  { ...updateRequest, expected_source_digest: undefined },
  { ...createRequest, unexpected: true },
]) {
  await publication.publish(invalid).then(
    () => { throw new Error("invalid source publication request was accepted"); },
    () => {},
  );
}
if (requests.length !== beforeInvalidPublish) {
  throw new Error("invalid source publication request reached the network");
}

const beforeInvalidRead = requests.length;
for (const invalid of [
  { ...readRequest, path: "../escape.ortg" },
  { ...readRequest, root_identity: `sha256:${"E".repeat(64)}` },
  { ...readRequest, path: "unicode/\ud800.ortg" },
  { ...readRequest, unexpected: true },
]) {
  await reading.read(invalid).then(
    () => { throw new Error("invalid source-read request was accepted"); },
    () => {},
  );
}
if (requests.length !== beforeInvalidRead) {
  throw new Error("invalid source-read request reached the network");
}

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
