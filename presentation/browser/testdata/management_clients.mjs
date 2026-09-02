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
  { name: "management.reconciliation", method: "POST", path: "/client/v1/management/reconciliations",
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

async function mount(path, permissions = [], restoredState) {
  const published = [];
  const disposers = [];
  let stateSnapshot;
  let restoredConsumed = false;
  const plugin = (await import(pathToFileURL(path))).default;
  await plugin.mount({
    manifest,
    permissions: { allows: (kind, resource, operation) => permissions.some((permission) =>
      permission.kind === kind && permission.resource === resource && permission.operations.includes(operation)) },
    services: { get: (name) => services.get(name) },
    state: {
      restored() {
        if (restoredConsumed) throw new Error("restored state was consumed twice");
        restoredConsumed = true;
        return restoredState === undefined ? undefined : structuredClone(restoredState);
      },
      snapshot(callback) {
        if (stateSnapshot || typeof callback !== "function") {
          throw new Error("state snapshot callback is invalid or duplicated");
        }
        stateSnapshot = callback;
      },
    },
    publish(name, value) { services.set(name, value); published.push(name); },
    lifecycle: { defer(_name, dispose) { disposers.push(dispose); } },
  });
  const dispose = async () => {
    for (const disposer of [...disposers].reverse()) await disposer();
    for (const name of published) services.delete(name);
  };
  mounted.push(dispose);
  return {
    plugin, dispose,
    snapshot: () => stateSnapshot === undefined ? undefined : structuredClone(stateSnapshot()),
  };
}

const operatorMount = await mount(operatorPath, [{
  kind: "credential.use", resource: "management-operator", operations: ["header"],
}]);
await mount(transportPath, [{
  kind: "network.connect", resource: "host-management",
  operations: ["static", "authoring", "reconciliation", "source-read", "publication"],
}]);
await mount(staticPath);
await mount(authoringPath);
await mount(readingPath);
await mount(publicationPath);
const workspaceMount = await mount(workspacePath);

const control = services.get("presentation.client.management_operator_control");
const transport = services.get("presentation.client.management_transport");
const catalog = services.get("presentation.client.management_static");
const authoring = services.get("presentation.client.management_authoring");
const editing = services.get("presentation.client.management_editing");
const reading = services.get("presentation.client.source_reading");
const publication = services.get("presentation.client.source_publication");
const workspace = services.get("presentation.client.authoring_workspace");
if (!control || !transport || !catalog || !authoring || !editing || !reading || !publication || !workspace ||
    !workspace.canRead() || !workspace.canPublish()) {
  throw new Error("management services were not published");
}
if (typeof workspaceMount.plugin.migrateState !== "function" || workspaceMount.snapshot() === undefined) {
  throw new Error("authoring workspace omitted its state lifecycle");
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
const endPosition = (source) => {
  let offset = 0;
  let line = 1;
  let column = 1;
  for (const character of source) {
    offset += bytes(character).byteLength;
    if (character === "\n") { line++; column = 1; }
    else column++;
  }
  return { offset, line, column };
};
const identifierEdits = (source, selected, replacement) => {
  const edits = [];
  let cursor = 0;
  while (cursor < source.length) {
    const index = source.indexOf(selected, cursor);
    if (index < 0) break;
    const before = source[index - 1] ?? "";
    const after = source[index + selected.length] ?? "";
    if (!/[A-Za-z0-9_-]/.test(before) && !/[A-Za-z0-9_-]/.test(after)) {
      edits.push({
        span: { start: endPosition(source.slice(0, index)),
          end: endPosition(source.slice(0, index + selected.length)) },
        old_text: selected, new_text: replacement,
      });
    }
    cursor = index + selected.length;
  }
  return edits;
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

const reconciliation = {
  session_id: "sess-browser-reconcile", expected_fingerprint: graphDigest, candidate: graph,
  values_fingerprint: `sha256:${"e".repeat(64)}`,
  deployment_fingerprint: `sha256:${"f".repeat(64)}`,
  state_migration: "browser-state-v1",
};
const reconciliationIdentity = `reconciliation:${reconciliation.session_id}:` +
  `${reconciliation.expected_fingerprint}:${reconciliation.candidate.fingerprint}`;
const reconciliationReceipt = {
  format_version: 1, session_id: reconciliation.session_id,
  previous_fingerprint: reconciliation.expected_fingerprint,
  candidate_fingerprint: reconciliation.candidate.fingerprint,
  state: "applied", safe_point_sequence: 29,
};
handlers.push((url, options) => {
  const body = JSON.parse(options.body);
  if (url !== "http://127.0.0.1:17777/client/v1/management/reconciliations" ||
      options.method !== "POST" || body.session_id !== reconciliation.session_id ||
      body.candidate.fingerprint !== graphDigest) {
    throw new Error("reconciliation transport changed the exact request");
  }
  return response(reconciliationReceipt, reconciliationIdentity);
});
const reconciliationResult = await transport.reconcile(reconciliation);
const reconciliationFetch = requests.at(-1);
if (reconciliationResult.value.safe_point_sequence !== 29 ||
    reconciliationResult.identity !== reconciliationIdentity ||
    reconciliationFetch.options.headers["OpenRealtime-Management-Token"] !== operatorOne ||
    reconciliationFetch.url.includes(operatorOne) ||
    reconciliationFetch.options.body.includes(operatorOne) ||
    reconciliationFetch.options.body.includes("mgmt_session_must_not_cross")) {
  throw new Error("reconciliation transport crossed an identity or authority boundary");
}
handlers.push(() => response(reconciliationReceipt,
  `reconciliation:sess-other:${graphDigest}:${graphDigest}`));
await transport.reconcile(reconciliation).then(
  () => { throw new Error("reconciliation with forged host evidence was accepted"); },
  (error) => { if (!String(error).includes("identity")) throw error; },
);
const forgedReconciliationReceipt = { ...reconciliationReceipt, state: "applied\npayload" };
handlers.push(() => response(forgedReconciliationReceipt, reconciliationIdentity));
await transport.reconcile(reconciliation).then(
  () => { throw new Error("payload-shaped reconciliation state was accepted"); },
  (error) => { if (!String(error).includes("identity")) throw error; },
);
const beforeInvalidReconciliation = requests.length;
await transport.reconcile({ ...reconciliation, state_migration: "payload\ntext" }).then(
  () => { throw new Error("invalid reconciliation request was accepted"); },
  () => {},
);
if (requests.length !== beforeInvalidReconciliation) {
  throw new Error("invalid reconciliation request reached the network");
}

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
if (await editing.applyEdits({ path: "fixture.ortg", source, revision: 1 }, analyzed.formatting) !== source) {
  throw new Error("canonical no-op formatter changed source bytes");
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

const noncanonicalSource = "graph fixture_β  {\n}\n";
const canonicalSource = "graph fixture_β {\n}\n";
const noncanonicalDigest = await sha256(bytes(noncanonicalSource));
const noncanonicalAnalysis = structuredClone(analysis);
noncanonicalAnalysis.source_digest = noncanonicalDigest;
noncanonicalAnalysis.canonical = false;
noncanonicalAnalysis.formatting = {
  path: "fixture.ortg", source_digest: noncanonicalDigest, edits: [{
    span: { start: { offset: 0, line: 1, column: 1 }, end: endPosition(noncanonicalSource) },
    old_text: noncanonicalSource, new_text: canonicalSource,
  }],
};
handlers.push(() => response(noncanonicalAnalysis, `authoring:analyze:${noncanonicalDigest}`));
const formatterAnalysis = await authoring.analyze({
  path: "fixture.ortg", source: noncanonicalSource, revision: 1,
});
const requestsBeforeLocalFormat = requests.length;
const formatted = await editing.applyEdits(
  { path: "fixture.ortg", source: noncanonicalSource, revision: 1 }, formatterAnalysis.formatting,
);
if (formatted !== canonicalSource || requests.length !== requestsBeforeLocalFormat) {
  throw new Error("formatter edits were not applied locally to the exact UTF-8 source");
}

workspace.setDocument("fixture.ortg", noncanonicalSource, 1);
handlers.push(() => response(noncanonicalAnalysis, `authoring:analyze:${noncanonicalDigest}`));
await workspace.analyze();
const formatEpoch = workspace.snapshot().epoch;
await workspace.format();
const formattedSnapshot = workspace.snapshot();
if (formattedSnapshot.phase !== "formatted" || formattedSnapshot.document.source !== canonicalSource ||
    formattedSnapshot.document.revision !== 1 || formattedSnapshot.epoch !== formatEpoch + 1 ||
    formattedSnapshot.analysis !== null || formattedSnapshot.compiled !== null ||
    formattedSnapshot.rendering !== null || formattedSnapshot.publication !== null) {
  throw new Error("authoring workspace did not atomically install and invalidate after formatting");
}

const invalidFormatterAnalyses = [];
const wrongFormatPath = structuredClone(noncanonicalAnalysis);
wrongFormatPath.formatting.path = "other.ortg";
invalidFormatterAnalyses.push(wrongFormatPath);
const staleFormatText = structuredClone(noncanonicalAnalysis);
staleFormatText.formatting.edits[0].old_text = canonicalSource;
invalidFormatterAnalyses.push(staleFormatText);
const forgedFormatPosition = structuredClone(noncanonicalAnalysis);
forgedFormatPosition.formatting.edits[0].span.end.column++;
invalidFormatterAnalyses.push(forgedFormatPosition);
const overlappingFormat = structuredClone(noncanonicalAnalysis);
overlappingFormat.formatting.edits.push(structuredClone(overlappingFormat.formatting.edits[0]));
invalidFormatterAnalyses.push(overlappingFormat);
for (const invalid of invalidFormatterAnalyses) {
  handlers.push(() => response(invalid, `authoring:analyze:${noncanonicalDigest}`));
  await authoring.analyze({ path: "fixture.ortg", source: noncanonicalSource, revision: 1 }).then(
    () => { throw new Error("forged formatter edit set was accepted"); },
    () => {},
  );
}
const beforeStaleLocalFormat = requests.length;
await editing.applyEdits(
  { path: "fixture.ortg", source: canonicalSource, revision: 1 }, formatterAnalysis.formatting,
).then(
  () => { throw new Error("stale formatter edit set was accepted"); },
  () => {},
);
if (requests.length !== beforeStaleLocalFormat) {
  throw new Error("invalid local formatter edit reached the network");
}

const renameSource = `graph fixture_rename {
    test.Element :: source;
    input inbound = source.out;
    edge loop = source.out -> source.out;
    output outbound = source.out;
}
`;
const renameDigest = await sha256(bytes(renameSource));
const renameResult = {
  node: "source", new_name: "camera",
  edits: { path: "rename.ortg", source_digest: renameDigest,
    edits: identifierEdits(renameSource, "source", "camera") },
};
if (renameResult.edits.edits.length !== 5) throw new Error("rename fixture lost a graph reference");
handlers.push(() => response(renameResult, `authoring:rename:${renameDigest}`));
const exactRename = await editing.rename(
  { path: "rename.ortg", source: renameSource, revision: 4 }, "source", "camera",
);
const renameFetch = requests.at(-1);
const renameWire = JSON.parse(renameFetch.options.body);
if (!renameFetch.url.endsWith("/authoring/rename") ||
    renameFetch.options.headers["OpenRealtime-Management-Token"] !== operatorOne ||
    renameWire.node !== "source" || renameWire.new_name !== "camera" ||
    renameWire.document.source !== renameSource || renameWire.document.revision !== 4 ||
    exactRename.edits.edits.length !== 5) {
  throw new Error("node rename did not preserve its exact source and authority boundary");
}
const renamedSource = await editing.applyEdits(
  { path: "rename.ortg", source: renameSource, revision: 4 }, exactRename.edits,
);
if (renamedSource !== renameSource.replaceAll("source", "camera")) {
  throw new Error("graph-wide node rename did not apply every exact reference locally");
}

const forgedRenameResults = [];
const wrongRenameName = structuredClone(renameResult);
wrongRenameName.new_name = "other";
forgedRenameResults.push([wrongRenameName, `authoring:rename:${renameDigest}`]);
const wrongRenameText = structuredClone(renameResult);
wrongRenameText.edits.edits[0].new_text = "other";
forgedRenameResults.push([wrongRenameText, `authoring:rename:${renameDigest}`]);
const omittedRenameReference = structuredClone(renameResult);
omittedRenameReference.edits.edits.pop();
forgedRenameResults.push([omittedRenameReference, `authoring:rename:${renameDigest}`]);
const forgedRenamePosition = structuredClone(renameResult);
forgedRenamePosition.edits.edits[0].span.end.column++;
forgedRenameResults.push([forgedRenamePosition, `authoring:rename:${renameDigest}`]);
const overlappingRename = structuredClone(renameResult);
overlappingRename.edits.edits.push(structuredClone(overlappingRename.edits.edits[0]));
forgedRenameResults.push([overlappingRename, `authoring:rename:${renameDigest}`]);
forgedRenameResults.push([renameResult, `authoring:rename:${otherDigest}`]);
for (const [forged, evidence] of forgedRenameResults) {
  handlers.push(() => response(forged, evidence));
  await editing.rename(
    { path: "rename.ortg", source: renameSource, revision: 4 }, "source", "camera",
  ).then(
    () => { throw new Error("forged graph-wide rename was accepted"); },
    () => {},
  );
}

const collisionSource = renameSource.replace(
  "    input inbound", "    test.Element :: camera;\n    input inbound",
);
const beforeInvalidRename = requests.length;
for (const invalid of [
  [{ path: "rename.ortg", source: renameSource, revision: 4 }, "missing", "camera"],
  [{ path: "rename.ortg", source: renameSource, revision: 4 }, "source", "bad.name"],
  [{ path: "rename.ortg", source: collisionSource, revision: 4 }, "source", "camera"],
  [{ path: "rename.ortg", source: renameSource, revision: 4,
    lock: { format_version: 1, elements: [] } }, "source", "camera"],
  [{ path: "rename.ortg", source: renameSource, revision: 4,
    channel_depth: { loop: 1 } }, "source", "camera"],
]) {
  await editing.rename(...invalid).then(
    () => { throw new Error("invalid graph-wide rename was accepted"); },
    () => {},
  );
}
if (requests.length !== beforeInvalidRename) {
  throw new Error("invalid graph-wide rename reached the network");
}

const edgeSource = `graph fixture_edge {
    test.Element :: source;
    test.Element :: sink;
    // removable café edge
    edge optional = source.out -> sink.in;
    // retained graph comment
}
`;
const edgeRemovedSource = `graph fixture_edge {
    test.Element :: source;
    test.Element :: sink;
    // retained graph comment
}
`;
const edgeCreatedSource = `graph fixture_edge {
    test.Element :: source;
    test.Element :: sink;
    edge restored = source.out -> sink.in;
    // retained graph comment
}
`;
const edgeGraphDigest = `sha256:${"7".repeat(64)}`;
const edgeRemovedGraphDigest = `sha256:${"6".repeat(64)}`;
const edgeCreatedGraphDigest = `sha256:${"5".repeat(64)}`;
const edgeDigest = await sha256(bytes(edgeSource));
const edgeResult = {
  edge: "optional",
  edits: { path: "edge.ortg", source_digest: edgeDigest, edits: [{
    span: { start: { offset: 0, line: 1, column: 1 }, end: endPosition(edgeSource) },
    old_text: edgeSource, new_text: edgeRemovedSource,
  }] },
};
handlers.push(() => response(edgeResult, `authoring:edge.remove:${edgeDigest}`));
const exactEdgeRemoval = await editing.removeEdge(
  { path: "edge.ortg", source: edgeSource, revision: 4 }, "optional",
);
const edgeFetch = requests.at(-1);
const edgeWire = JSON.parse(edgeFetch.options.body);
if (!edgeFetch.url.endsWith("/authoring/remove-edge") ||
    edgeFetch.options.headers["OpenRealtime-Management-Token"] !== operatorOne ||
    edgeWire.edge !== "optional" || edgeWire.document.source !== edgeSource ||
    edgeWire.document.revision !== 4 || exactEdgeRemoval.edits.edits.length !== 1) {
  throw new Error("edge removal did not preserve its exact source and authority boundary");
}
const locallyRemovedEdge = await editing.applyEdits(
  { path: "edge.ortg", source: edgeSource, revision: 4 }, exactEdgeRemoval.edits,
);
if (locallyRemovedEdge !== edgeRemovedSource) {
  throw new Error("edge removal did not apply the exact canonical source mutation locally");
}

const forgedEdgeResults = [];
const wrongEdgeIdentity = structuredClone(edgeResult);
wrongEdgeIdentity.edge = "other";
forgedEdgeResults.push([wrongEdgeIdentity, `authoring:edge.remove:${edgeDigest}`]);
const wrongEdgeSource = structuredClone(edgeResult);
wrongEdgeSource.edits.edits[0].new_text += "\n";
forgedEdgeResults.push([wrongEdgeSource, `authoring:edge.remove:${edgeDigest}`]);
const missingEdgeEdit = structuredClone(edgeResult);
missingEdgeEdit.edits.edits = [];
forgedEdgeResults.push([missingEdgeEdit, `authoring:edge.remove:${edgeDigest}`]);
const forgedEdgePosition = structuredClone(edgeResult);
forgedEdgePosition.edits.edits[0].span.end.column++;
forgedEdgeResults.push([forgedEdgePosition, `authoring:edge.remove:${edgeDigest}`]);
forgedEdgeResults.push([edgeResult, `authoring:edge.remove:${otherDigest}`]);
for (const [forged, evidence] of forgedEdgeResults) {
  handlers.push(() => response(forged, evidence));
  await editing.removeEdge(
    { path: "edge.ortg", source: edgeSource, revision: 4 }, "optional",
  ).then(
    () => { throw new Error("forged edge removal was accepted"); },
    () => {},
  );
}
const beforeInvalidEdge = requests.length;
for (const invalid of [
  [{ path: "edge.ortg", source: edgeSource, revision: 4 }, "missing"],
  [{ path: "edge.ortg", source: `${edgeSource}\n`, revision: 4 }, "optional"],
  [{ path: "edge.ortg", source: edgeSource, revision: 4,
    lock: { format_version: 1, elements: [] } }, "optional"],
  [{ path: "edge.ortg", source: edgeSource, revision: 4,
    channel_depth: { optional: 1 } }, "optional"],
]) {
  await editing.removeEdge(...invalid).then(
    () => { throw new Error("invalid edge removal was accepted"); },
    () => {},
  );
}
if (requests.length !== beforeInvalidEdge) {
  throw new Error("invalid edge removal reached the network");
}

const edgeRemovedDigest = await sha256(bytes(edgeRemovedSource));
const edgeCreationResult = {
  edge: "restored", previous_fingerprint: edgeRemovedGraphDigest,
  candidate_fingerprint: edgeCreatedGraphDigest,
  edits: { path: "edge.ortg", source_digest: edgeRemovedDigest, edits: [{
    span: { start: { offset: 0, line: 1, column: 1 }, end: endPosition(edgeRemovedSource) },
    old_text: edgeRemovedSource, new_text: edgeCreatedSource,
  }] },
};
const edgeCreationEvidence = `authoring:edge.create:${edgeRemovedGraphDigest}:` +
  `${edgeCreatedGraphDigest}:${edgeRemovedDigest}`;
handlers.push(() => response(edgeCreationResult, edgeCreationEvidence));
const exactEdgeCreation = await editing.createEdge(
  { path: "edge.ortg", source: edgeRemovedSource, revision: 5 }, edgeRemovedGraphDigest, "restored",
  { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless",
);
const edgeCreationFetch = requests.at(-1);
const edgeCreationWire = JSON.parse(edgeCreationFetch.options.body);
if (!edgeCreationFetch.url.endsWith("/authoring/create-edge") ||
    edgeCreationFetch.options.headers["OpenRealtime-Management-Token"] !== operatorOne ||
    edgeCreationWire.expected_fingerprint !== edgeRemovedGraphDigest ||
    edgeCreationWire.edge !== "restored" || edgeCreationWire.document.source !== edgeRemovedSource ||
    edgeCreationWire.document.revision !== 5 || edgeCreationWire.from.node !== "source" ||
    edgeCreationWire.from.port !== "out" || edgeCreationWire.to.node !== "sink" ||
    edgeCreationWire.to.port !== "in" || edgeCreationWire.delivery !== "lossless" ||
    exactEdgeCreation.candidate_fingerprint !== edgeCreatedGraphDigest) {
  throw new Error("edge creation did not preserve its graph, source, endpoints, and authority boundary");
}
const locallyCreatedEdge = await editing.applyEdits(
  { path: "edge.ortg", source: edgeRemovedSource, revision: 5 }, exactEdgeCreation.edits,
);
if (locallyCreatedEdge !== edgeCreatedSource) {
  throw new Error("edge creation did not apply the exact canonical source mutation locally");
}

const forgedEdgeCreations = [];
const wrongCreatedIdentity = structuredClone(edgeCreationResult);
wrongCreatedIdentity.edge = "other";
forgedEdgeCreations.push([wrongCreatedIdentity, edgeCreationEvidence]);
const wrongCreatedPredecessor = structuredClone(edgeCreationResult);
wrongCreatedPredecessor.previous_fingerprint = otherDigest;
forgedEdgeCreations.push([wrongCreatedPredecessor, edgeCreationEvidence]);
const unchangedCreatedFingerprint = structuredClone(edgeCreationResult);
unchangedCreatedFingerprint.candidate_fingerprint = edgeRemovedGraphDigest;
forgedEdgeCreations.push([unchangedCreatedFingerprint,
  `authoring:edge.create:${edgeRemovedGraphDigest}:${edgeRemovedGraphDigest}:${edgeRemovedDigest}`]);
const wrongCreatedSource = structuredClone(edgeCreationResult);
wrongCreatedSource.edits.edits[0].new_text += "\n";
forgedEdgeCreations.push([wrongCreatedSource, edgeCreationEvidence]);
const missingCreationEdit = structuredClone(edgeCreationResult);
missingCreationEdit.edits.edits = [];
forgedEdgeCreations.push([missingCreationEdit, edgeCreationEvidence]);
const forgedCreationPosition = structuredClone(edgeCreationResult);
forgedCreationPosition.edits.edits[0].span.end.column++;
forgedEdgeCreations.push([forgedCreationPosition, edgeCreationEvidence]);
forgedEdgeCreations.push([edgeCreationResult,
  `authoring:edge.create:${edgeRemovedGraphDigest}:${edgeCreatedGraphDigest}:${otherDigest}`]);
for (const [forged, evidence] of forgedEdgeCreations) {
  handlers.push(() => response(forged, evidence));
  await editing.createEdge(
    { path: "edge.ortg", source: edgeRemovedSource, revision: 5 }, edgeRemovedGraphDigest, "restored",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless",
  ).then(
    () => { throw new Error("forged edge creation was accepted"); },
    () => {},
  );
}
const duplicateCreatedSource = edgeCreatedSource;
const beforeInvalidCreation = requests.length;
for (const invalid of [
  [{ path: "edge.ortg", source: edgeRemovedSource, revision: 5 }, "invalid", "restored",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless"],
  [{ path: "edge.ortg", source: edgeRemovedSource, revision: 5 }, edgeRemovedGraphDigest, "bad.name",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless"],
  [{ path: "edge.ortg", source: edgeRemovedSource, revision: 5 }, edgeRemovedGraphDigest, "restored",
    { node: "missing", port: "out" }, { node: "sink", port: "in" }, "lossless"],
  [{ path: "edge.ortg", source: edgeRemovedSource, revision: 5 }, edgeRemovedGraphDigest, "restored",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "unknown"],
  [{ path: "edge.ortg", source: `${edgeRemovedSource}\n`, revision: 5 }, edgeRemovedGraphDigest, "restored",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless"],
  [{ path: "edge.ortg", source: duplicateCreatedSource, revision: 5 }, edgeRemovedGraphDigest, "restored",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless"],
  [{ path: "edge.ortg", source: edgeRemovedSource, revision: 5,
    lock: { format_version: 1, elements: [] } }, edgeRemovedGraphDigest, "restored",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless"],
  [{ path: "edge.ortg", source: edgeRemovedSource, revision: 5,
    channel_depth: { restored: 1 } }, edgeRemovedGraphDigest, "restored",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless"],
]) {
  await editing.createEdge(...invalid).then(
    () => { throw new Error("invalid edge creation was accepted"); },
    () => {},
  );
}
if (requests.length !== beforeInvalidCreation) {
  throw new Error("invalid edge creation reached the network");
}

const normalizedYAML = `apiVersion: openrealtime.ai/graph/v1alpha1
graph:
  name: normalized
  nodes:
    - id: source
      element: test.Element
    - id: sink
      element: test.Element
  edges:
    - id: optional
      from: source.out
      to: sink.in
      delivery: lossless
`;
const normalizedRenamedYAML = normalizedYAML
  .replace("    - id: source\n", "    - id: camera\n")
  .replace("      from: source.out\n", "      from: camera.out\n");
const normalizedRemovedYAML = `apiVersion: openrealtime.ai/graph/v1alpha1
graph:
  name: normalized
  nodes:
    - id: source
      element: test.Element
    - id: sink
      element: test.Element
`;
const normalizedCreatedYAML = `${normalizedRemovedYAML}  edges:
    - id: restored
      from: source.out
      to: sink.in
      delivery: lossless
`;
const normalizedJSONValue = {
  apiVersion: "openrealtime.ai/graph/v1alpha1",
  graph: {
    name: "normalized",
    nodes: [
      { id: "source", element: "test.Element" },
      { id: "sink", element: "test.Element" },
    ],
    edges: [{ id: "optional", from: "source.out", to: "sink.in", delivery: "lossless" }],
  },
};
const normalizedJSON = `${JSON.stringify(normalizedJSONValue, null, 2)}\n`;
const normalizedRenamedJSONValue = structuredClone(normalizedJSONValue);
normalizedRenamedJSONValue.graph.nodes[0].id = "camera";
normalizedRenamedJSONValue.graph.edges[0].from = "camera.out";
const normalizedRenamedJSON = `${JSON.stringify(normalizedRenamedJSONValue, null, 2)}\n`;
const normalizedRemovedJSONValue = structuredClone(normalizedJSONValue);
delete normalizedRemovedJSONValue.graph.edges;
const normalizedRemovedJSON = `${JSON.stringify(normalizedRemovedJSONValue, null, 2)}\n`;
const normalizedCreatedJSONValue = structuredClone(normalizedRemovedJSONValue);
normalizedCreatedJSONValue.graph.edges = [
  { id: "restored", from: "source.out", to: "sink.in", delivery: "lossless" },
];
const normalizedCreatedJSON = `${JSON.stringify(normalizedCreatedJSONValue, null, 2)}\n`;

for (const [index, fixture] of [
  { name: "YAML", path: "normalized.yaml", source: normalizedYAML,
    renamed: normalizedRenamedYAML, removed: normalizedRemovedYAML, created: normalizedCreatedYAML },
  { name: "JSON", path: "normalized.json", source: normalizedJSON,
    renamed: normalizedRenamedJSON, removed: normalizedRemovedJSON, created: normalizedCreatedJSON },
].entries()) {
  const sourceDigest = await sha256(bytes(fixture.source));
  const rename = {
    node: "source", new_name: "camera",
    edits: { path: fixture.path, source_digest: sourceDigest, edits: [{
      span: { start: { offset: 0, line: 1, column: 1 }, end: endPosition(fixture.source) },
      old_text: fixture.source, new_text: fixture.renamed,
    }] },
  };
  handlers.push(() => response(rename, `authoring:rename:${sourceDigest}`));
  const exactRename = await editing.rename(
    { path: fixture.path, source: fixture.source, revision: 20 }, "source", "camera",
  );
  const renamed = await editing.applyEdits(
    { path: fixture.path, source: fixture.source, revision: 20 }, exactRename.edits,
  );
  if (renamed !== fixture.renamed || exactRename.edits.edits.length !== 1) {
    throw new Error(`normalized ${fixture.name} rename was not one exact document replacement`);
  }

  const noOp = {
    node: "source", new_name: "source",
    edits: { path: fixture.path, source_digest: sourceDigest, edits: [] },
  };
  handlers.push(() => response(noOp, `authoring:rename:${sourceDigest}`));
  const exactNoOp = await editing.rename(
    { path: fixture.path, source: fixture.source, revision: 20 }, "source", "source",
  );
  if (await editing.applyEdits(
    { path: fixture.path, source: fixture.source, revision: 20 }, exactNoOp.edits,
  ) !== fixture.source) {
    throw new Error(`normalized ${fixture.name} no-op rename changed source`);
  }

  const remove = {
    edge: "optional", edits: { path: fixture.path, source_digest: sourceDigest, edits: [{
      span: { start: { offset: 0, line: 1, column: 1 }, end: endPosition(fixture.source) },
      old_text: fixture.source, new_text: fixture.removed,
    }] },
  };
  handlers.push(() => response(remove, `authoring:edge.remove:${sourceDigest}`));
  const exactRemove = await editing.removeEdge(
    { path: fixture.path, source: fixture.source, revision: 20 }, "optional",
  );
  if (await editing.applyEdits(
    { path: fixture.path, source: fixture.source, revision: 20 }, exactRemove.edits,
  ) !== fixture.removed) {
    throw new Error(`normalized ${fixture.name} edge removal changed another source byte`);
  }

  const removedDigest = await sha256(bytes(fixture.removed));
  const predecessor = `sha256:${String(2 + index).repeat(64)}`;
  const candidate = `sha256:${String(4 + index).repeat(64)}`;
  const create = {
    edge: "restored", previous_fingerprint: predecessor, candidate_fingerprint: candidate,
    edits: { path: fixture.path, source_digest: removedDigest, edits: [{
      span: { start: { offset: 0, line: 1, column: 1 }, end: endPosition(fixture.removed) },
      old_text: fixture.removed, new_text: fixture.created,
    }] },
  };
  handlers.push(() => response(create,
    `authoring:edge.create:${predecessor}:${candidate}:${removedDigest}`));
  const exactCreate = await editing.createEdge(
    { path: fixture.path, source: fixture.removed, revision: 21 }, predecessor, "restored",
    { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless",
  );
  if (await editing.applyEdits(
    { path: fixture.path, source: fixture.removed, revision: 21 }, exactCreate.edits,
  ) !== fixture.created) {
    throw new Error(`normalized ${fixture.name} edge creation changed another source byte`);
  }

  const forged = structuredClone(rename);
  forged.edits.edits[0].new_text += "\n";
  handlers.push(() => response(forged, `authoring:rename:${sourceDigest}`));
  await editing.rename(
    { path: fixture.path, source: fixture.source, revision: 20 }, "source", "camera",
  ).then(
    () => { throw new Error(`forged normalized ${fixture.name} rename was accepted`); },
    () => {},
  );
}

const beforeInvalidNormalized = requests.length;
for (const invalid of [
  { path: "normalized.yaml", source: `${normalizedYAML}\n`, revision: 20 },
  { path: "normalized.yaml", source: normalizedYAML.replace("  nodes:\n", "  nodes:\n  nodes:\n"), revision: 20 },
  { path: "normalized.json", source: JSON.stringify(normalizedJSONValue), revision: 20 },
  { path: "normalized.json", source: `${JSON.stringify({
    ...normalizedRemovedJSONValue,
    graph: { ...normalizedRemovedJSONValue.graph, edges: [] },
  }, null, 2)}\n`, revision: 20 },
  { path: "normalized.json", source: normalizedJSON.replace(
    '    "name": "normalized",', '    "name": "normalized",\n    "name": "forged",'), revision: 20 },
]) {
  await editing.rename(invalid, "source", "camera").then(
    () => { throw new Error("noncanonical normalized topology was accepted for mutation"); },
    () => {},
  );
}
if (requests.length !== beforeInvalidNormalized) {
  throw new Error("invalid normalized topology mutation reached the network");
}

const renameGraphDigest = `sha256:${"9".repeat(64)}`;
const renamedGraphDigest = `sha256:${"8".repeat(64)}`;
const renameGraph = structuredClone(graph);
renameGraph.id = "fixture_rename";
renameGraph.revision = 4;
renameGraph.fingerprint = renameGraphDigest;
renameGraph.nodes[0].id = "source";
const renameCompiled = { graph: renameGraph, lock: { format_version: 1, elements: [{
  reference: "test.Element", identity: { name: "test.Element", revision: 1, digest: elementDigest },
}] } };
workspace.setDocument("rename.ortg", renameSource, 4);
handlers.push(() => response(renameCompiled, `authoring:compile:${renameGraphDigest}`));
await workspace.compile();
const renameEpoch = workspace.snapshot().epoch;
handlers.push(() => response(renameResult, `authoring:rename:${renameDigest}`));
await workspace.renameNode(renameGraphDigest, "source", "camera");
const renamedSnapshot = workspace.snapshot();
if (renamedSnapshot.phase !== "renamed" || renamedSnapshot.document.source !== renamedSource ||
    renamedSnapshot.document.revision !== 5 || renamedSnapshot.epoch !== renameEpoch + 1 ||
    renamedSnapshot.sourceRead !== null || renamedSnapshot.analysis !== null ||
    renamedSnapshot.compiled !== null || renamedSnapshot.rendering !== null ||
    renamedSnapshot.publication !== null) {
  throw new Error("authoring workspace did not atomically install and invalidate a node rename");
}
const renamedGraph = structuredClone(renameGraph);
renamedGraph.revision = 5;
renamedGraph.fingerprint = renamedGraphDigest;
renamedGraph.nodes[0].id = "camera";
handlers.push(() => response({ ...renameCompiled, graph: renamedGraph },
  `authoring:compile:${renamedGraphDigest}`));
await workspace.compile();
if (workspace.snapshot().compiled.graph.nodes[0].id !== "camera" ||
    workspace.snapshot().compiled.graph.fingerprint !== renamedGraphDigest) {
  throw new Error("renamed workspace source did not compile under its incremented revision");
}
const beforeStaleSelection = requests.length;
await Promise.resolve().then(() => workspace.renameNode(renameGraphDigest, "source", "other")).then(
  () => { throw new Error("stale canvas selection was accepted"); },
  () => {},
);
if (requests.length !== beforeStaleSelection) throw new Error("stale canvas selection reached the network");

workspace.setDocument("rename.ortg", renameSource, 4);
handlers.push(() => response(renameCompiled, `authoring:compile:${renameGraphDigest}`));
await workspace.compile();
let releaseRename;
handlers.push(() => new Promise((resolve) => {
  releaseRename = () => resolve(response(renameResult, `authoring:rename:${renameDigest}`));
}));
const pendingRename = workspace.renameNode(renameGraphDigest, "source", "camera").then(
  () => { throw new Error("rename completed after the workspace document changed"); },
  (error) => error,
);
for (let attempt = 0; attempt < 20 && !releaseRename; attempt++) {
  await new Promise((resolve) => setTimeout(resolve, 0));
}
if (!releaseRename) throw new Error("deferred rename never reached transport");
workspace.setDocument("replacement.ortg", "graph replacement {\n}\n", 1);
releaseRename();
const staleRename = await pendingRename;
const replacementSnapshot = workspace.snapshot();
if (!String(staleRename).includes("changed during request") ||
    replacementSnapshot.document.path !== "replacement.ortg" ||
    replacementSnapshot.document.source !== "graph replacement {\n}\n" ||
    replacementSnapshot.phase !== "idle") {
  throw new Error("workspace CAS did not preserve a newer document against a late rename");
}

const edgeGraph = structuredClone(graph);
edgeGraph.id = "fixture_edge";
edgeGraph.revision = 4;
edgeGraph.fingerprint = edgeGraphDigest;
edgeGraph.nodes[0].id = "source";
edgeGraph.nodes.push({ ...structuredClone(edgeGraph.nodes[0]), id: "sink",
  ports: [{ ...structuredClone(edgeGraph.nodes[0].ports[0]), name: "in", direction: "input" }] });
edgeGraph.edges = [{ id: "optional", from: { node: "source", port: "out" },
  to: { node: "sink", port: "in" } }];
const edgeCompiled = { graph: edgeGraph, lock: { format_version: 1, elements: [{
  reference: "test.Element", identity: { name: "test.Element", revision: 1, digest: elementDigest },
}] } };
workspace.setDocument("edge.ortg", edgeSource, 4);
handlers.push(() => response(edgeCompiled, `authoring:compile:${edgeGraphDigest}`));
await workspace.compile();
const edgeEpoch = workspace.snapshot().epoch;
handlers.push(() => response(edgeResult, `authoring:edge.remove:${edgeDigest}`));
await workspace.removeEdge(edgeGraphDigest, "optional");
const edgeRemovedSnapshot = workspace.snapshot();
if (edgeRemovedSnapshot.phase !== "edge-removed" ||
    edgeRemovedSnapshot.document.source !== edgeRemovedSource ||
    edgeRemovedSnapshot.document.revision !== 5 || edgeRemovedSnapshot.epoch !== edgeEpoch + 1 ||
    edgeRemovedSnapshot.sourceRead !== null || edgeRemovedSnapshot.analysis !== null ||
    edgeRemovedSnapshot.compiled !== null || edgeRemovedSnapshot.rendering !== null ||
    edgeRemovedSnapshot.publication !== null) {
  throw new Error("authoring workspace did not atomically install and invalidate an edge removal");
}
const edgeRemovedGraph = structuredClone(edgeGraph);
edgeRemovedGraph.revision = 5;
edgeRemovedGraph.fingerprint = edgeRemovedGraphDigest;
edgeRemovedGraph.edges = [];
handlers.push(() => response({ ...edgeCompiled, graph: edgeRemovedGraph },
  `authoring:compile:${edgeRemovedGraphDigest}`));
await workspace.compile();
if (workspace.snapshot().compiled.graph.edges.length !== 0 ||
    workspace.snapshot().compiled.graph.fingerprint !== edgeRemovedGraphDigest) {
  throw new Error("edge-removed workspace source did not compile under its incremented revision");
}
const beforeStaleEdgeSelection = requests.length;
await Promise.resolve().then(() => workspace.removeEdge(edgeRemovedGraphDigest, "optional")).then(
  () => { throw new Error("stale edge selection was accepted"); },
  () => {},
);
if (requests.length !== beforeStaleEdgeSelection) {
  throw new Error("stale edge selection reached the network");
}

const edgeCreationEpoch = workspace.snapshot().epoch;
handlers.push(() => response(edgeCreationResult, edgeCreationEvidence));
await workspace.createEdge(edgeRemovedGraphDigest, "restored",
  { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless");
const edgeCreatedSnapshot = workspace.snapshot();
if (edgeCreatedSnapshot.phase !== "edge-created" ||
    edgeCreatedSnapshot.document.source !== edgeCreatedSource ||
    edgeCreatedSnapshot.document.revision !== 6 || edgeCreatedSnapshot.epoch !== edgeCreationEpoch + 1 ||
    edgeCreatedSnapshot.sourceRead !== null || edgeCreatedSnapshot.analysis !== null ||
    edgeCreatedSnapshot.compiled !== null || edgeCreatedSnapshot.rendering !== null ||
    edgeCreatedSnapshot.publication !== null) {
  throw new Error("authoring workspace did not atomically install and invalidate an edge creation");
}
const edgeCreatedGraph = structuredClone(edgeRemovedGraph);
edgeCreatedGraph.revision = 6;
edgeCreatedGraph.fingerprint = edgeCreatedGraphDigest;
edgeCreatedGraph.edges = [{ id: "restored", from: { node: "source", port: "out" },
  to: { node: "sink", port: "in" } }];
handlers.push(() => response({ ...edgeCompiled, graph: edgeCreatedGraph },
  `authoring:compile:${edgeCreatedGraphDigest}`));
await workspace.compile();
if (workspace.snapshot().compiled.graph.edges[0]?.id !== "restored" ||
    workspace.snapshot().compiled.graph.fingerprint !== edgeCreatedGraphDigest) {
  throw new Error("edge-created workspace source did not compile under its incremented revision");
}
const beforeStaleCreation = requests.length;
await Promise.resolve().then(() => workspace.createEdge(edgeCreatedGraphDigest, "restored",
  { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless")).then(
  () => { throw new Error("duplicate compiled edge identity was accepted for creation"); },
  () => {},
);
await Promise.resolve().then(() => workspace.createEdge(edgeCreatedGraphDigest, "backward",
  { node: "sink", port: "in" }, { node: "source", port: "out" }, "lossless")).then(
  () => { throw new Error("backward compiled edge endpoints were accepted for creation"); },
  () => {},
);
if (requests.length !== beforeStaleCreation) {
  throw new Error("invalid compiled edge creation reached the network");
}

workspace.setDocument("edge.ortg", edgeRemovedSource, 5);
handlers.push(() => response({ ...edgeCompiled, graph: edgeRemovedGraph },
  `authoring:compile:${edgeRemovedGraphDigest}`));
await workspace.compile();
let releaseEdgeCreation;
handlers.push(() => new Promise((resolve) => {
  releaseEdgeCreation = () => resolve(response(edgeCreationResult, edgeCreationEvidence));
}));
const pendingEdgeCreation = workspace.createEdge(edgeRemovedGraphDigest, "restored",
  { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless").then(
  () => { throw new Error("edge creation completed after the workspace document changed"); },
  (error) => error,
);
for (let attempt = 0; attempt < 20 && !releaseEdgeCreation; attempt++) {
  await new Promise((resolve) => setTimeout(resolve, 0));
}
if (!releaseEdgeCreation) throw new Error("deferred edge creation never reached transport");
workspace.setDocument("replacement.ortg", "graph replacement {\n}\n", 1);
releaseEdgeCreation();
const staleEdgeCreation = await pendingEdgeCreation;
const replacementAfterCreation = workspace.snapshot();
if (!String(staleEdgeCreation).includes("changed during request") ||
    replacementAfterCreation.document.path !== "replacement.ortg" ||
    replacementAfterCreation.document.source !== "graph replacement {\n}\n" ||
    replacementAfterCreation.phase !== "idle") {
  throw new Error("workspace CAS did not preserve a newer document against a late edge creation");
}

workspace.setDocument("edge.ortg", edgeSource, 4);
handlers.push(() => response(edgeCompiled, `authoring:compile:${edgeGraphDigest}`));
await workspace.compile();
let releaseEdgeRemoval;
handlers.push(() => new Promise((resolve) => {
  releaseEdgeRemoval = () => resolve(response(edgeResult, `authoring:edge.remove:${edgeDigest}`));
}));
const pendingEdgeRemoval = workspace.removeEdge(edgeGraphDigest, "optional").then(
  () => { throw new Error("edge removal completed after the workspace document changed"); },
  (error) => error,
);
for (let attempt = 0; attempt < 20 && !releaseEdgeRemoval; attempt++) {
  await new Promise((resolve) => setTimeout(resolve, 0));
}
if (!releaseEdgeRemoval) throw new Error("deferred edge removal never reached transport");
workspace.setDocument("replacement.ortg", "graph replacement {\n}\n", 1);
releaseEdgeRemoval();
const staleEdgeRemoval = await pendingEdgeRemoval;
const replacementAfterEdge = workspace.snapshot();
if (!String(staleEdgeRemoval).includes("changed during request") ||
    replacementAfterEdge.document.path !== "replacement.ortg" ||
    replacementAfterEdge.document.source !== "graph replacement {\n}\n" ||
    replacementAfterEdge.phase !== "idle") {
  throw new Error("workspace CAS did not preserve a newer document against a late edge removal");
}

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

const durableWorkspace = workspaceMount.snapshot();
if (!durableWorkspace || Object.keys(durableWorkspace).length !== 1 ||
    Object.keys(durableWorkspace.document ?? {}).sort().join(",") !== "path,revision,source" ||
    Object.hasOwn(durableWorkspace, "compiled") || Object.hasOwn(durableWorkspace, "publication")) {
  throw new Error("authoring workspace snapshot was not the exact durable document boundary");
}
const migrationInput = {
  entry: "authoring-workspace",
  schema: {
    name: "presentation.client.authoring_workspace.state", revision: 1,
    digest: "sha256:8dcc2b5181390a1a61b50a3bb07390326bc60e6839a22f9667e3d40247147b78",
  },
  source_implementation: "browser-esm:authoring-workspace.js",
  snapshot: durableWorkspace,
};
const migratedWorkspace = await workspaceMount.plugin.migrateState(migrationInput);
if (JSON.stringify(migratedWorkspace) !== JSON.stringify(durableWorkspace)) {
  throw new Error("authoring workspace migration changed the durable document");
}
await workspaceMount.plugin.migrateState({
  ...migrationInput, snapshot: { ...durableWorkspace, compiled: {} },
}).then(
  () => { throw new Error("authoring workspace migration accepted derived state"); },
  () => {},
);

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
await editing.applyEdits(
  { path: "fixture.ortg", source: noncanonicalSource, revision: 1 }, formatterAnalysis.formatting,
).then(
  () => { throw new Error("disposed authoring formatter remained usable"); },
  () => {},
);
await editing.removeEdge(
  { path: "edge.ortg", source: edgeSource, revision: 4 }, "optional",
).then(
  () => { throw new Error("disposed authoring edge remover remained usable"); },
  () => {},
);
await editing.createEdge(
  { path: "edge.ortg", source: edgeRemovedSource, revision: 5 }, edgeRemovedGraphDigest, "restored",
  { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless",
).then(
  () => { throw new Error("disposed authoring edge creator remained usable"); },
  () => {},
);
await Promise.resolve().then(() => workspace.format()).then(
  () => { throw new Error("disposed authoring workspace formatter remained usable"); },
  () => {},
);
await Promise.resolve().then(() => workspace.createEdge(edgeRemovedGraphDigest, "restored",
  { node: "source", port: "out" }, { node: "sink", port: "in" }, "lossless")).then(
  () => { throw new Error("disposed authoring workspace edge creator remained usable"); },
  () => {},
);
const serialized = JSON.stringify({ manifest, workspace: workspace.snapshot?.(), status: control.status() });
for (const secret of [operatorOne, operatorTwo, operatorExpiring, "mgmt_session_must_not_cross"]) {
  if (serialized.includes(secret) || requests.some((request) => request.url.includes(secret))) {
    throw new Error("management capability escaped into URL, manifest, or serialized live state");
  }
}
