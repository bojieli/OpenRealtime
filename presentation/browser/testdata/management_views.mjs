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
  phase: "idle", error: "", analysis: null, compiled: null, rendering: null,
};
const workspaceListeners = new Set();
const workspace = Object.freeze({
  snapshot: () => structuredClone(workspaceSnapshot),
  subscribe(listener) { workspaceListeners.add(listener); listener(structuredClone(workspaceSnapshot));
    return () => workspaceListeners.delete(listener); },
  setDocument(path, source, revision) {
    workspaceSnapshot = { ...workspaceSnapshot, epoch: workspaceSnapshot.epoch + 1,
      document: { path, source, revision } };
    for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));
  },
  async analyze() {}, async compile() {}, async render() {},
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

const malicious = '<img src=x onerror="globalThis.compromised=true">';
const propertyName = "mode/tilde~";
workspaceSnapshot = {
  ...workspaceSnapshot, phase: "analyzed",
  analysis: { catalog: { total: 1, elements: [{
    identity: { name: "test.Element", revision: 1, digest: `sha256:${"a".repeat(64)}` },
    config: { schema_status: "resolved", artifact: "agent.values.yaml", resolved: true,
      inline_topology_values: false, empty_object_only: false, properties_complete: true,
      schema_digest: `sha256:${"b".repeat(64)}`, schema_reference: malicious, schema_id: malicious,
      additional_properties: { type: malicious },
      properties: [{ name: propertyName, pointer: "#/properties/mode~1tilde~0", types: ["string"],
        required: true, title: malicious, description: malicious, format: "uri-reference",
        default: null, enum: [malicious, "safe"], schema: { type: "string", title: malicious } }] },
  }] } },
  compiled: { graph: { id: "fixture", revision: 1, fingerprint: `sha256:${"c".repeat(64)}` } },
  rendering: { fingerprint: `sha256:${"c".repeat(64)}`, format: "mermaid", text: `<svg onload=alert(1)>` },
};
for (const listener of workspaceListeners) listener(structuredClone(workspaceSnapshot));

const configuration = slots.get("authoring.configuration");
const canvas = slots.get("authoring.canvas");
if (!configuration.textContent.includes(malicious) || tags(configuration).includes("IMG") ||
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

for (const dispose of disposers.reverse()) await dispose();
if (slots.size !== 0 || workspaceListeners.size !== 0 || operatorListeners.size !== 0) {
  throw new Error("management view provider loss retained slots, listeners, or DOM effects");
}
if ([...new Set([...slots.values()].flatMap(tags))].includes("IMG")) {
  throw new Error("disposed metadata renderer created executable markup");
}
