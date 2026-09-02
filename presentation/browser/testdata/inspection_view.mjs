import { pathToFileURL } from "node:url";

const modulePath = process.argv[2];
if (!modulePath) throw new Error("inspection view module path is required");

class FakeNode {
  constructor(tagName = "node") {
    this.tagName = tagName.toUpperCase();
    this.children = [];
    this.dataset = {};
    this.listeners = new Map();
    this._text = "";
    this.id = "";
    this.disabled = false;
  }
  set textContent(value) { this._text = String(value ?? ""); this.children = []; }
  get textContent() { return this._text + this.children.map((child) => child.textContent).join(""); }
  set innerHTML(_value) { throw new Error("inspection view attempted HTML interpretation"); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this._text = ""; this.children = [...children]; }
  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) ?? new Set();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }
  removeEventListener(type, listener) { this.listeners.get(type)?.delete(listener); }
  async dispatch(type) {
    for (const listener of [...(this.listeners.get(type) ?? [])]) await listener({ preventDefault() {} });
  }
}

globalThis.Node = FakeNode;
globalThis.document = Object.freeze({ createElement: (name) => new FakeNode(name) });

function find(node, predicate) {
  if (predicate(node)) return node;
  for (const child of node.children) {
    const match = find(child, predicate);
    if (match) return match;
  }
  return null;
}

function tags(node, result = []) {
  result.push(node.tagName);
  for (const child of node.children) tags(child, result);
  return result;
}

const fingerprint = `sha256:${"a".repeat(64)}`;
const elementDigest = `sha256:${"b".repeat(64)}`;
const malicious = `<img src=x onerror="globalThis.compromised=true">`;
const nodeID = `operator${malicious}`;
const edgeID = `channel${malicious}`;
const baseLive = {
  format_version: 1,
  graph_id: "joined_operator",
  graph_revision: 3,
  fingerprint,
  state: "running",
  nodes: {
    [nodeID]: {
      state: "running",
      active_runs: 2,
      first_trigger_ns: 100,
      first_output_ns: 120,
      completion_ns: 180,
      cancellation_ns: 160,
      authority_decision: {
        kind: "succeeded", operation: "select", crossed: false, at_ns: 180,
      },
      resolution: {
        element: { name: "test.Operator", revision: 1, digest: elementDigest },
      },
    },
  },
  edges: {
    [edgeID]: {
      occupancy: 1, high_water: 3, enqueued: 7, dequeued: 6,
      dropped: 2, backpressure: 1, queue_wait_ns: 300,
    },
  },
  flows: {
    flow_000001: {
      correlation: "flow_000001", edges: [edgeID, edgeID],
      edge_ns: [110, 150], first_ns: 110, last_ns: 150, truncated: false,
      causal_stages: [
		{ item: "cause_000002", parents: ["cause_000001"], kind: "observation" },
		{ item: "cause_000003", parents: ["cause_000002", "cause_000001"], kind: "state_revision" },
      ],
    },
  },
  trace_dropped: 0,
};
const baseModel = {
  graph_id: "joined_operator",
  revision: 3,
  fingerprint,
  nodes: [{
    id: nodeID,
    element: { name: "test.Operator", revision: 1, digest: elementDigest },
    implementation: "go.test.operator.v1",
    ports: [
      { name: "trigger", direction: "input", type: "Trigger(test.Value)", cardinality: "one", role: "trigger" },
      { name: "done", direction: "output", type: "Event(test.Value)", cardinality: "one", role: "outcome" },
    ],
    reaction: {
      triggers: ["trigger"], sampled_state: ["context"], interrupts: ["cancel"],
      outcomes: ["done"], max_concurrency: 4, breaks_cycles: true,
    },
    effects: [{ name: "computer.click", external: true, authority: malicious, reversible: false }],
  }],
  edges: [{
    id: edgeID, from: { node: nodeID, port: "done" }, to: { node: nodeID, port: "trigger" },
    type: "Event(test.Value)", role: "data", delivery: "lossy", depth: 4,
  }],
};
const baseDelta = {
  format_version: 1,
  session_id: "sess-joined",
  graph: { format_version: 1, id: "joined_operator", revision: 3, fingerprint },
  after: 0,
  next: 2,
  baseline: { sequence: 1 },
  events: [{ sequence: 2 }],
};

let live = structuredClone(baseLive);
let model = structuredClone(baseModel);
let delta = structuredClone(baseDelta);
let modelUnavailable = false;
let accessListener;
let deltaCalls = 0;
const inspection = Object.freeze({
  available: () => true,
  subscribe(listener) {
    accessListener = listener;
    listener({ session_id: "sess-joined", expires_at_ms: Date.now() + 60_000 });
    return () => { accessListener = undefined; };
  },
  async live() { return structuredClone(live); },
  async model() {
    if (modelUnavailable) throw new Error("static model unavailable");
    return structuredClone(model);
  },
  async deltas(after, limit) {
    if (after !== 0 || limit !== 256) throw new Error("inspection view issued an unbounded delta request");
    deltaCalls++;
    return structuredClone(delta);
  },
});

const slots = new Map();
const slotService = Object.freeze({
  register(name, value) {
    if (slots.has(name)) throw new Error(`duplicate slot ${name}`);
    slots.set(name, value);
    return () => slots.delete(name);
  },
});
const disposers = [];
const plugin = (await import(pathToFileURL(modulePath))).default;
await plugin.mount({
  services: { get: (name) => new Map([
    ["presentation.client.slots", slotService],
    ["presentation.client.inspection", inspection],
  ]).get(name) },
  lifecycle: { defer(_name, dispose) { disposers.push(dispose); } },
});
await new Promise((resolve) => setImmediate(resolve));

const section = slots.get("inspection.graph");
if (!section) throw new Error("inspection view omitted its replaceable slot");
const availability = find(section, (entry) => entry.id === "availability");
const contract = find(section, (entry) => entry.id === "contract-availability");
const deltaAvailability = find(section, (entry) => entry.id === "delta-availability");
const refresh = find(section, (entry) => entry.id === "refresh");
const card = find(section, (entry) => entry.dataset.nodeId === nodeID);
const edgeCard = find(section, (entry) => entry.dataset.edgeId === edgeID);
const flowCard = find(section, (entry) => entry.dataset.flowId === "flow_000001");
if (availability?.textContent !== "live" || contract?.dataset.state !== "joined" ||
    deltaAvailability?.dataset.state !== "loaded" || deltaAvailability?.dataset.after !== "0" ||
    deltaAvailability?.dataset.next !== "2" || deltaAvailability?.dataset.events !== "1" ||
    deltaAvailability?.dataset.baseline !== "1" || deltaCalls !== 1 ||
    card?.dataset.activeRuns !== "2" || card?.dataset.state !== "running" ||
    edgeCard?.dataset.delivery !== "lossy" || edgeCard?.dataset.depth !== "4" ||
    edgeCard?.dataset.occupancy !== "1" || flowCard?.dataset.stageCount !== "2" ||
    flowCard?.dataset.truncated !== "false") {
  throw new Error("inspection view did not join exact static and live node evidence");
}
for (const expected of [
  "Triggers: trigger", "Sampled state: context", "Interrupts: cancel", "Outcomes: done",
  "Max concurrency: 4", "Causal break: declared", "First trigger: 100 ns from mount clock",
  "First output: 120 ns from mount clock", "Trigger to first output: 20 ns after first trigger",
  "Completion: 180 ns from mount clock", "Trigger to completion: 80 ns after first trigger",
  "Cancellation: 160 ns from mount clock", "Trigger to cancellation: 60 ns after first trigger",
  `computer.click: external; authority ${malicious}; not reversible`,
  "Outcome: succeeded", "Operation: select", "Irreversible boundary: not crossed",
  "Observed: 180 ns from mount clock",
  `Route: ${nodeID}.done → ${nodeID}.trigger`, "Contract: Event(test.Value); role data",
  "Delivery: lossy; depth 4", "Occupancy: 1/4", "High water: 3/4",
  "Enqueued / dequeued: 7 / 6", "Dropped: 2", "Backpressure: 1",
  "Queue wait: 300 ns cumulative; 50 ns per dequeue",
  "First traversal: 110 ns from mount clock", "Last traversal: 150 ns from mount clock",
  "Elapsed: 40 ns", "Retention: complete",
  `Stage 1: ${nodeID}.done → ${nodeID}.trigger via ${edgeID} (Event(test.Value); lossy); ` +
	"110 ns from mount clock; first retained stage; causal observation cause_000002 ← cause_000001",
  `Stage 2: ${nodeID}.done → ${nodeID}.trigger via ${edgeID} (Event(test.Value); lossy); ` +
	"150 ns from mount clock; +40 ns; causal state_revision cause_000003 ← cause_000002, cause_000001",
]) {
  if (!section.textContent.includes(expected)) throw new Error(`joined view omitted ${expected}`);
}
if (tags(section).includes("IMG") || globalThis.compromised) {
  throw new Error("inspection evidence crossed the text-only rendering boundary");
}

delta.graph.fingerprint = `sha256:${"c".repeat(64)}`;
await refresh.dispatch("click");
if (availability.textContent !== "live" || deltaAvailability.dataset.state !== "invalid" ||
    !section.textContent.includes("does not match this session")) {
  throw new Error("inspection view accepted a delta page for another graph");
}
delta = structuredClone(baseDelta);

model.fingerprint = `sha256:${"c".repeat(64)}`;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" || contract.dataset.state !== "invalid" ||
    find(section, (entry) => entry.dataset.nodeId === nodeID)) {
  throw new Error("inspection view rendered a mismatched static/live join");
}

model = structuredClone(baseModel);
live = structuredClone(baseLive);
live.nodes[nodeID].last_trigger_id = malicious;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" || !section.textContent.includes("unredacted item identity")) {
  throw new Error("inspection view accepted an unredacted live item identity");
}

live = structuredClone(baseLive);
modelUnavailable = true;
await refresh.dispatch("click");
if (availability.textContent !== "live" || contract.dataset.state !== "unavailable" ||
    find(section, (entry) => entry.dataset.nodeId === nodeID)) {
  throw new Error("inspection view confused live-only evidence with a joined contract view");
}

modelUnavailable = false;
live = structuredClone(baseLive);
live.nodes[nodeID].first_output_ns = 99;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("impossible trigger-relative timing")) {
  throw new Error("inspection view accepted impossible trigger-relative timing");
}

live = structuredClone(baseLive);
live.nodes[nodeID].authority_decision.kind = malicious;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("invalid authority-decision evidence")) {
  throw new Error("inspection view accepted a payload-shaped authority decision");
}

live = structuredClone(baseLive);
live.nodes[nodeID].authority_decision.operation = malicious;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("invalid authority-decision evidence")) {
  throw new Error("inspection view accepted a payload-shaped authority operation");
}

live = structuredClone(baseLive);
live.nodes[nodeID].authority_decision.at_ns = 119;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("invalid authority-decision evidence")) {
  throw new Error("inspection view accepted an authority decision before first output");
}

live = structuredClone(baseLive);
delete live.nodes[nodeID].authority_decision;
await refresh.dispatch("click");
if (availability.textContent !== "live" ||
    !section.textContent.includes("No authority decision observed.")) {
  throw new Error("inspection view rejected legacy live evidence without an authority decision");
}

live = structuredClone(baseLive);
live.nodes[nodeID].first_trigger_ns = 0;
live.nodes[nodeID].first_output_ns = 0;
live.nodes[nodeID].completion_ns = 0;
live.nodes[nodeID].authority_decision.at_ns = 0;
await refresh.dispatch("click");
if (availability.textContent !== "live" ||
    !section.textContent.includes("Observed: 0 ns from mount clock")) {
  throw new Error("inspection view rejected a zero-origin authority decision");
}

live = structuredClone(baseLive);
live.edges[edgeID].high_water = 5;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("static and live depth disagree")) {
  throw new Error("inspection view accepted queue telemetry beyond the declared depth");
}

live = structuredClone(baseLive);
live.edges[edgeID].dequeued = 7;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("impossible queue telemetry")) {
  throw new Error("inspection view accepted internally inconsistent queue counters");
}

live = structuredClone(baseLive);
live.edges["boundary:invented"] = {
  occupancy: 0, high_water: 0, enqueued: 0, dequeued: 0, dropped: 0, backpressure: 0,
};
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("contains undeclared edge boundary:invented")) {
  throw new Error("inspection view accepted an undeclared boundary queue");
}

live = structuredClone(baseLive);
delete live.edges[edgeID];
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes(`omits declared edge ${edgeID}`)) {
  throw new Error("inspection view accepted a missing declared channel");
}

live = structuredClone(baseLive);
live.flows.flow_000001.edges = ["invented"];
live.flows.flow_000001.edge_ns = [110];
live.flows.flow_000001.causal_stages = live.flows.flow_000001.causal_stages.slice(0, 1);
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("contains unknown internal edge invented")) {
  throw new Error("inspection view accepted a flow through an undeclared edge");
}

live = structuredClone(baseLive);
live.flows.flow_000001.last_ns = 109;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("impossible traversal timing")) {
  throw new Error("inspection view accepted impossible flow timing");
}

live = structuredClone(baseLive);
live.flows.flow_000001.edge_ns = [110];
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("edge timestamps do not match")) {
  throw new Error("inspection view accepted an incomplete edge timestamp sequence");
}

live = structuredClone(baseLive);
live.flows.flow_000001.edge_ns = [150, 110];
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("impossible edge timing")) {
  throw new Error("inspection view accepted regressing per-edge timing");
}

live = structuredClone(baseLive);
live.flows.flow_000001.causal_stages[0].item = malicious;
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("unredacted causal item identity")) {
  throw new Error("inspection view accepted a payload-shaped causal item identity");
}

live = structuredClone(baseLive);
live.flows.flow_000001.causal_stages[0].kind = "provider request text";
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("invalid semantic cause kind")) {
  throw new Error("inspection view accepted a payload-shaped semantic cause kind");
}

live = structuredClone(baseLive);
live.flows.flow_000001.causal_stages[0].parents[0] = "cause_000002";
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("invalid causal parents")) {
  throw new Error("inspection view accepted a self-parent causal assertion");
}

live = structuredClone(baseLive);
live.flows.flow_000001.causal_stages[0].parents = Array.from(
  { length: 65 }, (_entry, index) => `cause_${String(index + 100).padStart(6, "0")}`);
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("invalid causal parents")) {
  throw new Error("inspection view accepted an unbounded causal-parent assertion");
}

live = structuredClone(baseLive);
live.flows.flow_000001.causal_stages.pop();
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("causal stages do not match")) {
  throw new Error("inspection view accepted incomplete causal-stage coverage");
}

live = structuredClone(baseLive);
live.flows.flow_000002 = structuredClone(live.flows.flow_000001);
live.flows.flow_000002.correlation = "flow_000002";
live.flows.flow_000002.causal_stages[0].parents[0] = "cause_000004";
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("rewrites causal parents")) {
  throw new Error("inspection view accepted conflicting cross-flow causal assertions");
}

live = structuredClone(baseLive);
live.flows.flow_000002 = structuredClone(live.flows.flow_000001);
live.flows.flow_000002.correlation = "flow_000002";
live.flows.flow_000002.causal_stages[0].kind = "policy";
await refresh.dispatch("click");
if (availability.textContent !== "unavailable" ||
    !section.textContent.includes("semantic cause kind")) {
  throw new Error("inspection view accepted conflicting cross-flow semantic classification");
}

live = structuredClone(baseLive);
delete live.flows.flow_000001.causal_stages[0].kind;
delete live.flows.flow_000001.causal_stages[1].kind;
await refresh.dispatch("click");
if (availability.textContent !== "live" ||
    !section.textContent.includes("causal cause_000002 ← cause_000001")) {
  throw new Error("inspection view rejected legacy unclassified causal stages");
}

live = structuredClone(baseLive);
delete live.flows.flow_000001.causal_stages;
await refresh.dispatch("click");
if (availability.textContent !== "live" ||
    !section.textContent.includes("causal lineage unavailable")) {
  throw new Error("inspection view rejected legacy flow evidence without causal stages");
}

live = structuredClone(baseLive);
live.flows.flow_000001.first_ns = 0;
live.flows.flow_000001.last_ns = 0;
live.flows.flow_000001.edge_ns = [0, 0];
await refresh.dispatch("click");
if (availability.textContent !== "live" ||
    !find(section, (entry) => entry.dataset.flowId === "flow_000001") ||
    !section.textContent.includes("First traversal: not observed")) {
  throw new Error("inspection view rejected a valid zero-origin mount clock");
}

accessListener(null);
if (availability.textContent !== "waiting" || section.dataset.sessionId !== "" ||
    deltaAvailability.dataset.state !== "waiting" || deltaAvailability.dataset.next !== undefined ||
    find(section, (entry) => entry.dataset.nodeId === nodeID) ||
    find(section, (entry) => entry.dataset.flowId === "flow_000001")) {
  throw new Error("inspection capability loss retained session evidence");
}
for (const dispose of [...disposers].reverse()) await dispose();
if (slots.size !== 0 || accessListener !== undefined) {
  throw new Error("inspection view disposal retained a slot or capability listener");
}
