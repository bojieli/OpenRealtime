import { pathToFileURL } from "node:url";

const modules = process.argv.slice(2);
if (modules.length !== 4) throw new Error("management view module paths are required");

class FakeNode {
  constructor(tagName = "node") {
    this.tagName = tagName.toUpperCase();
    this.children = [];
    this.dataset = {};
    this.listeners = new Map();
    this._text = "";
    this.value = "";
    this.disabled = false;
  }
  set textContent(value) { this._text = String(value ?? ""); this.children = []; }
  get textContent() { return this._text + this.children.map((child) => child.textContent).join(""); }
  set innerHTML(_value) { throw new Error("management view attempted HTML interpretation"); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this._text = ""; this.children = [...children]; }
  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) ?? new Set();
    listeners.add(listener); this.listeners.set(type, listeners);
  }
  removeEventListener(type, listener) { this.listeners.get(type)?.delete(listener); }
  async dispatch(type) {
    const event = { preventDefault() {} };
    for (const listener of [...(this.listeners.get(type) ?? [])]) await listener(event);
  }
}
globalThis.Node = FakeNode;
globalThis.document = Object.freeze({ createElement: (name) => new FakeNode(name) });

const slots = new Map();
const slotService = Object.freeze({
  register(name, value) {
    if (slots.has(name)) throw new Error(`duplicate slot ${name}`);
    slots.set(name, value);
    return () => slots.delete(name);
  },
});
let workspaceSnapshot = {
  epoch: 0, document: { path: "agent.ortg", source: "graph agent {\n}\n", revision: 1 },
  phase: "idle", error: "", sourceRead: null, analysis: null, compiled: null, rendering: null,
  publication: null,
};
const workspaceListeners = new Set();
const readCalls = [];
const publicationCalls = [];
const renameCalls = [];
const edgeRemovalCalls = [];
const edgeCreationCalls = [];
let formatCalls = 0;
const workspace = Object.freeze({
  snapshot: () => structuredClone(workspaceSnapshot),
  canRead: () => true,
  canPublish: () => true,
  subscribe(listener) { workspaceListeners.add(listener); listener(structuredClone(workspaceSnapshot));
    return () => workspaceListeners.delete(listener); },
  setDocument(path, source, revision) {
    workspaceSnapshot = { ...workspaceSnapshot, epoch: workspaceSnapshot.epoch + 1,
      document: { path, source, revision } };
    for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
  },
  async analyze() {}, async compile() {}, async render() {},
  async renameNode(expectedFingerprint, selected, replacement) {
    const graph = workspaceSnapshot.compiled?.graph;
    if (!graph || graph.fingerprint !== expectedFingerprint ||
        !graph.nodes?.some((node) => node.id === selected)) {
      throw new Error("fake workspace received a stale node selection");
    }
    renameCalls.push({ expectedFingerprint, selected, replacement });
    workspaceSnapshot = { ...workspaceSnapshot, epoch: workspaceSnapshot.epoch + 1,
      document: { ...workspaceSnapshot.document,
        source: workspaceSnapshot.document.source.replaceAll(selected, replacement),
        revision: workspaceSnapshot.document.revision + 1 },
      phase: "renamed", sourceRead: null, analysis: null, compiled: null, rendering: null,
      publication: null };
    for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
    return structuredClone(workspaceSnapshot);
  },
  async removeEdge(expectedFingerprint, selected) {
    const graph = workspaceSnapshot.compiled?.graph;
    if (!graph || graph.fingerprint !== expectedFingerprint ||
        !graph.edges?.some((edge) => edge.id === selected)) {
      throw new Error("fake workspace received a stale edge selection");
    }
    edgeRemovalCalls.push({ expectedFingerprint, selected });
    workspaceSnapshot = { ...workspaceSnapshot, epoch: workspaceSnapshot.epoch + 1,
      document: { ...workspaceSnapshot.document,
        source: workspaceSnapshot.document.source.split("\n")
          .filter((line) => !line.includes("edge optional =")).join("\n"),
        revision: workspaceSnapshot.document.revision + 1 },
      phase: "edge-removed", sourceRead: null, analysis: null, compiled: null, rendering: null,
      publication: null };
    for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
    return structuredClone(workspaceSnapshot);
  },
  async createEdge(expectedFingerprint, selected, from, to, delivery) {
    const graph = workspaceSnapshot.compiled?.graph;
    const endpoint = (value, direction) => graph?.nodes?.some((node) => node.id === value?.node &&
      node.ports?.some((port) => port.name === value?.port && port.direction === direction));
    if (!graph || graph.fingerprint !== expectedFingerprint ||
        graph.edges?.some((edge) => edge.id === selected) ||
        !endpoint(from, "output") || !endpoint(to, "input") || delivery !== "lossless") {
      throw new Error("fake workspace received invalid edge-creation selection");
    }
    edgeCreationCalls.push({ expectedFingerprint, selected, from, to, delivery });
    const statement = `    edge ${selected} = ${from.node}.${from.port} -> ${to.node}.${to.port};\n`;
    workspaceSnapshot = { ...workspaceSnapshot, epoch: workspaceSnapshot.epoch + 1,
      document: { ...workspaceSnapshot.document,
        source: workspaceSnapshot.document.source.replace("}\n", `${statement}}\n`),
        revision: workspaceSnapshot.document.revision + 1 },
      phase: "edge-created", sourceRead: null, analysis: null, compiled: null, rendering: null,
      publication: null };
    for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
    return structuredClone(workspaceSnapshot);
  },
  async format() {
    const edit = workspaceSnapshot.analysis?.formatting?.edits?.[0];
    if (!edit) throw new Error("fake workspace has no formatter edit");
    formatCalls++;
    workspaceSnapshot = { ...workspaceSnapshot, epoch: workspaceSnapshot.epoch + 1,
      document: { ...workspaceSnapshot.document, source: edit.new_text }, phase: "formatted",
      analysis: null, compiled: null, rendering: null, publication: null, sourceRead: null };
    for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
    return structuredClone(workspaceSnapshot);
  },
  async load(rootIdentity, path) {
    readCalls.push({ rootIdentity, path });
    const source = "graph ui_loaded {\n}\n";
    const result = { format_version: 1, root_identity: rootIdentity, path,
      source_digest: `sha256:${"f".repeat(64)}`, source_bytes: source.length,
      result_digest: `sha256:${"a".repeat(64)}` };
    workspaceSnapshot = { ...workspaceSnapshot, epoch: workspaceSnapshot.epoch + 1,
      document: { path, source, revision: workspaceSnapshot.document.revision },
      phase: "loaded", sourceRead: result, publication: null };
    for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
    return structuredClone(workspaceSnapshot);
  },
  async publish(mode, rootIdentity, expectedSourceDigest = "") {
    publicationCalls.push({ mode, rootIdentity, expectedSourceDigest,
      path: workspaceSnapshot.document.path, source: workspaceSnapshot.document.source });
    const receipt = { format_version: 1, root_identity: rootIdentity, mode,
      path: workspaceSnapshot.document.path, previous_source_digest: expectedSourceDigest,
      source_digest: `sha256:${"d".repeat(64)}`, source_bytes: workspaceSnapshot.document.source.length,
      cleanup_pending: false, receipt_digest: `sha256:${"e".repeat(64)}` };
    workspaceSnapshot = { ...workspaceSnapshot, phase: "published", publication: receipt };
    for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
    return structuredClone(workspaceSnapshot);
  },
});
let operatorSecret = "";
const operatorListeners = new Set();
const operatorControl = Object.freeze({
  subscribe(listener) { operatorListeners.add(listener); listener({ available: false, expires_at_ms: 0, generation: 0 });
    return () => operatorListeners.delete(listener); },
  replace(secret) {
    operatorSecret = secret;
    for (const listener of operatorListeners) listener({ available: true, expires_at_ms: 0, generation: 1 });
  },
  clear() { operatorSecret = ""; },
});
const services = new Map([
  ["presentation.client.slots", slotService],
  ["presentation.client.authoring_workspace", workspace],
  ["presentation.client.management_operator_control", operatorControl],
  ["presentation.client.management_static", Object.freeze({
    async graph(fingerprint) { return { id: "fixture", revision: 1, fingerprint,
      nodes: [{ element: { name: "test.Element", revision: 1, digest: `sha256:${"a".repeat(64)}` } }] }; },
    async element() { return { name: "test.Element", revision: 1 }; },
    async valuesSchema() { return { digest: `sha256:${"b".repeat(64)}`, complete: true,
      nodes: [], unresolved: [] }; },
  })],
]);
const disposers = [];

for (const module of modules) {
  const plugin = (await import(pathToFileURL(module))).default;
  const local = [];
  await plugin.mount({
    services: { get: (name) => services.get(name) },
    lifecycle: { defer(_name, dispose) { local.push(dispose); } },
  });
  disposers.push(async () => { for (const dispose of [...local].reverse()) await dispose(); });
}

for (const name of ["management.authority", "authoring.editor", "authoring.configuration", "authoring.canvas"]) {
  if (!slots.has(name)) throw new Error(`replaceable management view omitted slot ${name}`);
}

function find(node, predicate) {
  if (predicate(node)) return node;
  for (const child of node.children) {
    const value = find(child, predicate);
    if (value) return value;
  }
  return null;
}
function tags(node, result = []) {
  result.push(node.tagName);
  for (const child of node.children) tags(child, result);
  return result;
}

const authorityView = slots.get("management.authority");
const password = find(authorityView, (node) => node.tagName === "INPUT");
const form = find(authorityView, (node) => node.tagName === "FORM");
password.value = "operator_dom_secret";
await form.dispatch("submit");
if (operatorSecret !== "operator_dom_secret" || password.value !== "" ||
    authorityView.textContent.includes(operatorSecret)) {
  throw new Error("operator configuration renderer retained or displayed a bearer value");
}

const editor = slots.get("authoring.editor");
const sourcePath = find(editor, (entry) => entry.name === "authoring-path");
const sourceText = find(editor, (entry) => entry.name === "authoring-source");
const sourceRoot = find(editor, (entry) => entry.name === "authoring-root-identity");
const predecessor = find(editor, (entry) => entry.name === "authoring-expected-digest");
const loadSource = find(editor, (entry) => entry.dataset.action === "load");
const formatSource = find(editor, (entry) => entry.dataset.action === "format");
const createSource = find(editor, (entry) => entry.dataset.action === "publish-create");
sourcePath.value = "ui-created.ortg";
sourceText.value = "graph ui_created {\n}\n";
sourceRoot.value = `sha256:${"c".repeat(64)}`;
await createSource.dispatch("click");
if (publicationCalls.length !== 1 || publicationCalls[0].mode !== "create" ||
    publicationCalls[0].rootIdentity !== sourceRoot.value ||
    publicationCalls[0].path !== sourcePath.value || publicationCalls[0].source !== sourceText.value ||
    !editor.textContent.includes(`sha256:${"e".repeat(64)}`)) {
  throw new Error("authoring editor did not keep mediated publication explicit and receipt-bound");
}

sourceText.value = "graph unsaved_local_text {\n}\n";
await loadSource.dispatch("click");
if (readCalls.length !== 1 || readCalls[0].rootIdentity !== sourceRoot.value ||
    readCalls[0].path !== sourcePath.value || sourceText.value !== "graph ui_loaded {\n}\n" ||
    predecessor.value !== `sha256:${"f".repeat(64)}` ||
    !editor.textContent.includes(`sha256:${"a".repeat(64)}`)) {
  throw new Error("authoring editor did not load exact rooted source and payload-free evidence");
}

const noncanonicalSource = "graph ui_loaded  {\n}\n";
const canonicalSource = "graph ui_loaded {\n}\n";
workspaceSnapshot = { ...workspaceSnapshot, epoch: workspaceSnapshot.epoch + 1,
  document: { ...workspaceSnapshot.document, source: noncanonicalSource }, phase: "analyzed",
  analysis: { parsed: true, recovered: false, canonical: false,
    diagnostics: { items: [], total: 0 }, catalog: { elements: [], total: 0 },
    formatting: { edits: [{ new_text: canonicalSource }] } } };
for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
if (!formatSource || formatSource.disabled) throw new Error("applicable browser formatter stayed disabled");
sourceText.value = `${noncanonicalSource}unsaved`;
await formatSource.dispatch("click");
if (formatCalls !== 0 || !editor.textContent.includes("analyze the current source before formatting")) {
  throw new Error("stale visible source reached the workspace formatter");
}
sourceText.value = noncanonicalSource;
await formatSource.dispatch("click");
if (formatCalls !== 1 || sourceText.value !== canonicalSource ||
    workspaceSnapshot.document.source !== canonicalSource || workspaceSnapshot.phase !== "formatted" ||
    !formatSource.disabled) {
  throw new Error("authoring editor did not apply the analyzed formatter edit atomically");
}

const malicious = '<img src=x onerror="globalThis.compromised=true">';
const propertyName = "mode/tilde~";
workspaceSnapshot = {
  ...workspaceSnapshot, phase: "analyzed",
  document: { path: "ui-created.ortg", source: `graph fixture {
    test.Element :: zeta;
    test.Element :: alpha;
    edge optional = alpha.out -> zeta.in;
}
`, revision: 1 },
  analysis: { diagnostics: { total: 1, items: [{
    code: "E_MARKUP", severity: "error", path: "ui-created.ortg",
    span: { start: { offset: 0, line: 1, column: 1 }, end: { offset: 5, line: 1, column: 6 } },
    message: malicious, notes: [malicious],
  }] }, catalog: { total: 1, elements: [{
    identity: { name: "test.Element", revision: 1, digest: `sha256:${"a".repeat(64)}` },
    config: { schema_status: "resolved", artifact: "agent.values.yaml", resolved: true,
      inline_topology_values: false, empty_object_only: false, properties_complete: true,
      schema_digest: `sha256:${"b".repeat(64)}`, schema_reference: malicious, schema_id: malicious,
      additional_properties: { type: malicious },
      properties: [{ name: propertyName, pointer: "#/properties/mode~1tilde~0", types: ["string"],
        required: true, title: malicious, description: malicious, format: "uri-reference",
        default: null, enum: [malicious, "safe"], schema: { type: "string", title: malicious } }] },
  }] } },
  compiled: { graph: { id: "fixture", revision: 1, fingerprint: `sha256:${"c".repeat(64)}`,
    nodes: [
      { id: "zeta", element: { name: "test.Element" },
        ports: [{ name: "in", direction: "input" }] },
      { id: "alpha", element: { name: "test.Element" },
        ports: [{ name: "out", direction: "output" }] },
    ], edges: [{ id: "optional", from: { node: "alpha", port: "out" },
      to: { node: "zeta", port: "in" } }] } },
  rendering: { fingerprint: `sha256:${"c".repeat(64)}`, format: "mermaid", text: `<svg onload=alert(1)>` },
};
for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));

const configuration = slots.get("authoring.configuration");
const canvas = slots.get("authoring.canvas");
if (!configuration.textContent.includes(malicious) || tags(configuration).includes("IMG") ||
    !editor.textContent.includes(`[E_MARKUP] error`) || !editor.textContent.includes(malicious) ||
    tags(editor).includes("IMG") ||
    !canvas.textContent.includes("<svg onload=alert(1)>") || tags(canvas).includes("SVG") ||
    globalThis.compromised) {
  throw new Error("authoring metadata or render output crossed the text-only view boundary");
}
for (const expected of ["properties complete: true", "additional properties:",
  "pointer: #/properties/mode~1tilde~0", "title:", "format: uri-reference", "default: null",
  `enum: [${JSON.stringify(malicious)},\"safe\"]`, "schema:"]) {
  if (!configuration.textContent.includes(expected)) {
    throw new Error(`configuration renderer omitted exact Authoring metadata ${expected}`);
  }
}

const canvasNodes = find(canvas, (entry) => entry.dataset.role === "nodes");
const nodeIDs = canvasNodes.children.map((entry) => entry.dataset.node);
if (JSON.stringify(nodeIDs) !== JSON.stringify(["alpha", "zeta"])) {
  throw new Error(`authoring canvas node controls are not deterministic: ${JSON.stringify(nodeIDs)}`);
}
const alpha = canvasNodes.children[0];
const canvasEdges = find(canvas, (entry) => entry.dataset.role === "edges");
if (JSON.stringify(canvasEdges.children.map((entry) => entry.dataset.edge)) !== JSON.stringify(["optional"])) {
  throw new Error("authoring canvas edge controls are not deterministic");
}
const renameInput = find(canvas, (entry) => entry.name === "authoring-node-name");
const renameNode = find(canvas, (entry) => entry.dataset.action === "rename-node");
await alpha.dispatch("click");
renameInput.value = "beta";
await renameNode.dispatch("click");
if (renameCalls.length !== 1 || renameCalls[0].expectedFingerprint !== `sha256:${"c".repeat(64)}` ||
    renameCalls[0].selected !== "alpha" || renameCalls[0].replacement !== "beta" ||
    workspaceSnapshot.phase !== "renamed" || workspaceSnapshot.document.revision !== 2 ||
    !workspaceSnapshot.document.source.includes(":: beta;") || workspaceSnapshot.compiled !== null ||
    !canvas.textContent.includes("Renamed alpha")) {
  throw new Error("authoring canvas did not initiate a fingerprint-bound graph node rename");
}

workspaceSnapshot = { ...workspaceSnapshot, phase: "compiled",
  compiled: { graph: { id: "fixture", revision: 2, fingerprint: `sha256:${"d".repeat(64)}`,
    nodes: [
      { id: "zeta", element: { name: "test.Element" },
        ports: [{ name: "in", direction: "input" }] },
      { id: "beta", element: { name: "test.Element" },
        ports: [{ name: "out", direction: "output" }] },
    ], edges: [{ id: "optional", from: { node: "beta", port: "out" },
      to: { node: "zeta", port: "in" } }] } } };
for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
const optional = find(canvas, (entry) => entry.dataset.action === "select-edge");
const removeEdge = find(canvas, (entry) => entry.dataset.action === "remove-edge");
await optional.dispatch("click");
await removeEdge.dispatch("click");
if (edgeRemovalCalls.length !== 1 ||
    edgeRemovalCalls[0].expectedFingerprint !== `sha256:${"d".repeat(64)}` ||
    edgeRemovalCalls[0].selected !== "optional" || workspaceSnapshot.phase !== "edge-removed" ||
    workspaceSnapshot.document.revision !== 3 || workspaceSnapshot.document.source.includes("edge optional") ||
    workspaceSnapshot.compiled !== null || !canvas.textContent.includes("Removed optional")) {
  throw new Error("authoring canvas did not initiate a fingerprint-bound edge removal");
}

workspaceSnapshot = { ...workspaceSnapshot, phase: "compiled",
  compiled: { graph: { id: "fixture", revision: 3, fingerprint: `sha256:${"e".repeat(64)}`,
    nodes: [
      { id: "zeta", element: { name: "test.Element" },
        ports: [{ name: "in", direction: "input" }] },
      { id: "beta", element: { name: "test.Element" },
        ports: [{ name: "out", direction: "output" }] },
    ], edges: [] } } };
for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
const outputPort = find(canvas, (entry) => entry.dataset.action === "select-edge-from");
const inputPort = find(canvas, (entry) => entry.dataset.action === "select-edge-to");
const edgeName = find(canvas, (entry) => entry.name === "authoring-edge-name");
const createEdge = find(canvas, (entry) => entry.dataset.action === "create-edge");
if (!outputPort || !inputPort || !edgeName || !createEdge ||
    outputPort.dataset.endpoint !== "beta.out" || inputPort.dataset.endpoint !== "zeta.in") {
  throw new Error("authoring canvas did not derive deterministic directional edge endpoints");
}
await outputPort.dispatch("click");
await inputPort.dispatch("click");
edgeName.value = "restored";
await edgeName.dispatch("input");
await createEdge.dispatch("click");
if (edgeCreationCalls.length !== 1 ||
    edgeCreationCalls[0].expectedFingerprint !== `sha256:${"e".repeat(64)}` ||
    edgeCreationCalls[0].selected !== "restored" ||
    edgeCreationCalls[0].from.node !== "beta" || edgeCreationCalls[0].from.port !== "out" ||
    edgeCreationCalls[0].to.node !== "zeta" || edgeCreationCalls[0].to.port !== "in" ||
    edgeCreationCalls[0].delivery !== "lossless" || workspaceSnapshot.phase !== "edge-created" ||
    workspaceSnapshot.document.revision !== 4 ||
    !workspaceSnapshot.document.source.includes("edge restored = beta.out -> zeta.in;") ||
    workspaceSnapshot.compiled !== null || !canvas.textContent.includes("Created restored")) {
  throw new Error("authoring canvas did not initiate a fingerprint-bound edge creation");
}

for (const dispose of disposers.reverse()) await dispose();
if (slots.size !== 0 || workspaceListeners.size !== 0 || operatorListeners.size !== 0) {
  throw new Error("management view provider loss retained slots, listeners, or DOM effects");
}
if ([...new Set([...slots.values()].flatMap(tags))].includes("IMG")) {
  throw new Error("disposed metadata renderer created executable markup");
}
