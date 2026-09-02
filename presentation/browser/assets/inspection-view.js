const DIGEST = /^sha256:[0-9a-f]{64}$/;
const MAX_ROWS = 65_536;
const MAX_TEXT = 65_536;
const MAX_CAUSAL_PARENTS = 64;
const DELTA_PAGE_LIMIT = 256;
const CAUSE_KINDS = new Set([
  "observation", "state_revision", "policy", "model_run",
]);
const DECISION_KINDS = new Set([
  "succeeded", "rejected", "denied", "canceled", "timed_out", "failed", "ignored",
]);
const DECISION_OPERATIONS = new Set([
  "accepted", "action", "admit", "already_committed", "attest", "authorize", "cancel",
  "candidate", "commit", "committed", "complete", "confirm", "context", "execute", "fence",
  "join", "lookup", "prepare", "promote", "proposal", "provenance", "queue", "result",
  "rejected", "retry", "select", "timeout",
]);

function object(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} is not an object`);
  }
  return value;
}

function text(value, label, empty = false) {
  if (typeof value !== "string" || value.length > MAX_TEXT || (!empty && value.length === 0) ||
      value.includes("\0") || value.includes("\r") || value.includes("\n")) {
    throw new Error(`${label} is not canonical text`);
  }
  return value;
}

function integer(value, label, minimum = 0) {
  if (!Number.isSafeInteger(value) || value < minimum) throw new Error(`${label} is not a safe integer`);
  return value;
}

function boolean(value, label) {
  if (value === undefined) return false;
  if (typeof value !== "boolean") throw new Error(`${label} is not boolean`);
  return value;
}

function rows(value, label, required = true) {
  if (value === undefined && !required) return [];
  if (!Array.isArray(value) || value.length > MAX_ROWS) throw new Error(`${label} is not a bounded array`);
  return value;
}

function strings(value, label) {
  const result = rows(value ?? [], label).map((entry, index) => text(entry, `${label} ${index}`));
  if (new Set(result).size !== result.length) throw new Error(`${label} repeats a value`);
  return result;
}

function sequence(value, label) {
  return rows(value ?? [], label).map((entry, index) => text(entry, `${label} ${index}`));
}

function identity(value, label) {
  object(value, label);
  const name = text(value.name, `${label} name`);
  const revision = integer(value.revision, `${label} revision`, 1);
  if (typeof value.digest !== "string" || !DIGEST.test(value.digest)) {
    throw new Error(`${label} digest is not canonical`);
  }
  return Object.freeze({ name, revision, digest: value.digest });
}

function sameIdentity(left, right) {
  return left.name === right.name && left.revision === right.revision && left.digest === right.digest;
}

function endpoint(value, label) {
  const source = object(value, label);
  return Object.freeze({
    node: text(source.node, `${label} node`),
    port: text(source.port, `${label} port`),
    lane: text(source.lane ?? "", `${label} lane`, true),
  });
}

function reaction(value, nodeID) {
  const source = object(value ?? {}, `node ${nodeID} reaction`);
  return Object.freeze({
    triggers: Object.freeze(strings(source.triggers, `node ${nodeID} triggers`)),
    sampledState: Object.freeze(strings(source.sampled_state, `node ${nodeID} sampled state`)),
    interrupts: Object.freeze(strings(source.interrupts, `node ${nodeID} interrupts`)),
    outcomes: Object.freeze(strings(source.outcomes, `node ${nodeID} outcomes`)),
    maxConcurrency: integer(source.max_concurrency ?? 0, `node ${nodeID} max concurrency`),
    breaksCycles: boolean(source.breaks_cycles, `node ${nodeID} causal break`),
  });
}

function effects(value, nodeID) {
  return Object.freeze(rows(value ?? [], `node ${nodeID} effects`).map((entry, index) => {
    const source = object(entry, `node ${nodeID} effect ${index}`);
    return Object.freeze({
      name: text(source.name, `node ${nodeID} effect ${index} name`),
      external: boolean(source.external, `node ${nodeID} effect ${index} external`),
      authority: text(source.authority ?? "", `node ${nodeID} effect ${index} authority`, true),
      reversible: boolean(source.reversible, `node ${nodeID} effect ${index} reversible`),
    });
  }));
}

function staticProjection(value) {
  const source = object(value, "static session model");
  const graphID = text(source.graph_id, "static graph ID");
  const revision = integer(source.revision, "static graph revision", 1);
  if (typeof source.fingerprint !== "string" || !DIGEST.test(source.fingerprint)) {
    throw new Error("static graph fingerprint is not canonical");
  }
  const seen = new Set();
  const nodes = rows(source.nodes, "static nodes").map((entry, index) => {
    const sourceNode = object(entry, `static node ${index}`);
    const id = text(sourceNode.id, `static node ${index} ID`);
    if (seen.has(id)) throw new Error(`static model repeats node ${id}`);
    seen.add(id);
    return Object.freeze({
      id,
      element: identity(sourceNode.element, `node ${id} element`),
      implementation: text(sourceNode.implementation ?? "", `node ${id} implementation`, true),
      reaction: reaction(sourceNode.reaction, id),
      effects: effects(sourceNode.effects, id),
    });
  });
  if (nodes.length === 0) throw new Error("static session model has no nodes");
  const edgeIDs = new Set();
  const edges = rows(source.edges ?? [], "static edges").map((entry, index) => {
    const edge = object(entry, `static edge ${index}`);
    const id = text(edge.id, `static edge ${index} ID`);
    if (edgeIDs.has(id)) throw new Error(`static model repeats edge ${id}`);
    edgeIDs.add(id);
    if (edge.delivery !== "lossless" && edge.delivery !== "lossy") {
      throw new Error(`static edge ${id} has invalid delivery`);
    }
    return Object.freeze({
      id,
      from: endpoint(edge.from, `static edge ${id} source`),
      to: endpoint(edge.to, `static edge ${id} target`),
      type: text(edge.type, `static edge ${id} type`),
      role: text(edge.role, `static edge ${id} role`),
      delivery: edge.delivery,
      depth: integer(edge.depth, `static edge ${id} depth`, 1),
    });
  });
  const boundaryNames = new Set();
  const boundaries = rows(source.boundaries ?? [], "static boundaries").map((entry, index) => {
    const boundary = object(entry, `static boundary ${index}`);
    const name = text(boundary.name, `static boundary ${index} name`);
    if (boundaryNames.has(name)) throw new Error(`static model repeats boundary ${name}`);
    boundaryNames.add(name);
    if (boundary.direction !== "input" && boundary.direction !== "output") {
      throw new Error(`static boundary ${name} has invalid direction`);
    }
    return Object.freeze({
      name, direction: boundary.direction,
      endpoint: endpoint(boundary.endpoint, `static boundary ${name} endpoint`),
      type: text(boundary.type, `static boundary ${name} type`),
      role: text(boundary.role, `static boundary ${name} role`),
    });
  });
  return Object.freeze({
    graphID, revision, fingerprint: source.fingerprint,
    nodes: Object.freeze(nodes), edges: Object.freeze(edges), boundaries: Object.freeze(boundaries),
  });
}

function liveNode(value, id) {
  const source = object(value, `live node ${id}`);
  if ((source.last_trigger_id ?? "") !== "" || (source.last_outcome ?? "") !== "") {
    throw new Error(`live node ${id} contains an unredacted item identity`);
  }
  if (source.error !== undefined && source.error !== "" && source.error !== "redacted") {
    throw new Error(`live node ${id} contains an unredacted error`);
  }
  const resolution = object(source.resolution, `live node ${id} resolution`);
  const firstTriggerNS = integer(source.first_trigger_ns ?? 0, `live node ${id} first trigger`);
  const firstOutputNS = integer(source.first_output_ns ?? 0, `live node ${id} first output`);
  const completionNS = integer(source.completion_ns ?? 0, `live node ${id} completion`);
  if (firstTriggerNS !== 0 &&
      ((firstOutputNS !== 0 && firstOutputNS < firstTriggerNS) ||
       (completionNS !== 0 && completionNS < firstTriggerNS))) {
    throw new Error(`live node ${id} contains impossible trigger-relative timing`);
  }
  let authorityDecision = null;
  if (source.authority_decision !== undefined) {
    const decision = object(source.authority_decision, `live node ${id} authority decision`);
    const kind = text(decision.kind, `live node ${id} authority decision kind`);
    const operation = text(decision.operation, `live node ${id} authority decision operation`);
    const atNS = integer(decision.at_ns, `live node ${id} authority decision time`);
    if (!DECISION_KINDS.has(kind) || !DECISION_OPERATIONS.has(operation) ||
        (firstTriggerNS !== 0 && atNS < firstTriggerNS) ||
        (firstOutputNS !== 0 && atNS < firstOutputNS) ||
        (completionNS !== 0 && atNS < completionNS)) {
      throw new Error(`live node ${id} contains invalid authority-decision evidence`);
    }
    authorityDecision = Object.freeze({
      kind, operation, atNS,
      crossed: boolean(decision.crossed, `live node ${id} authority decision crossed`),
    });
  }
  return Object.freeze({
    id,
    state: text(source.state, `live node ${id} state`),
    activeRuns: integer(source.active_runs, `live node ${id} active runs`),
    firstTriggerNS,
    firstOutputNS,
    completionNS,
    cancellationNS: integer(source.cancellation_ns ?? 0, `live node ${id} cancellation`),
    authorityDecision,
    element: identity(resolution.element, `live node ${id} element`),
  });
}

function edgeProjection(value, id) {
  const source = object(value, `live edge ${id}`);
  if ((source.last_item_id ?? "") !== "") throw new Error(`live edge ${id} contains an unredacted item identity`);
  const occupancy = integer(source.occupancy, `live edge ${id} occupancy`);
  const highWater = integer(source.high_water, `live edge ${id} high water`);
  const enqueued = integer(source.enqueued, `live edge ${id} enqueued`);
  const dequeued = integer(source.dequeued, `live edge ${id} dequeued`);
  if (highWater < occupancy || dequeued > enqueued || enqueued - dequeued !== occupancy) {
    throw new Error(`live edge ${id} contains impossible queue telemetry`);
  }
  return Object.freeze({
    occupancy,
    high_water: highWater,
    enqueued,
    dequeued,
    dropped: integer(source.dropped, `live edge ${id} dropped`),
    backpressure: integer(source.backpressure, `live edge ${id} backpressure`),
    queue_wait_ns: integer(source.queue_wait_ns ?? 0, `live edge ${id} queue wait`),
  });
}

function flowProjection(value, id, causalAssertions, causalIdentities) {
  const source = object(value, `live flow ${id}`);
  if (!/^flow_[0-9]{6}$/.test(id) || source.correlation !== id) {
    throw new Error("live flow contains an unredacted correlation");
  }
  const edges = sequence(source.edges, `live flow ${id} edges`);
  if (edges.length === 0) throw new Error(`live flow ${id} has no traversed edges`);
  const edgeNS = rows(source.edge_ns ?? [], `live flow ${id} edge times`).map(
    (entry, index) => integer(entry, `live flow ${id} edge time ${index}`));
  if (edgeNS.length !== 0 && edgeNS.length !== edges.length) {
    throw new Error(`live flow ${id} edge timestamps do not match its traversed edges`);
  }
  const causalStages = rows(source.causal_stages ?? [], `live flow ${id} causal stages`).map(
    (entry, index) => {
      const stage = object(entry, `live flow ${id} causal stage ${index}`);
      const item = text(stage.item, `live flow ${id} causal stage ${index} item`);
      if (!/^cause_[0-9]{6}$/.test(item)) {
        throw new Error(`live flow ${id} contains an unredacted causal item identity`);
      }
      const parents = strings(stage.parents, `live flow ${id} causal stage ${index} parents`);
      const kind = text(stage.kind ?? "", `live flow ${id} causal stage ${index} kind`, true);
      if (kind !== "" && !CAUSE_KINDS.has(kind)) {
        throw new Error(`live flow ${id} contains an invalid semantic cause kind`);
      }
      if (parents.length > MAX_CAUSAL_PARENTS ||
          parents.some((parent) => !/^cause_[0-9]{6}$/.test(parent) || parent === item)) {
        throw new Error(`live flow ${id} contains invalid causal parents`);
      }
      causalIdentities.add(item);
      for (const parent of parents) causalIdentities.add(parent);
      if (causalIdentities.size > MAX_ROWS) {
        throw new Error("live flows contain too many causal identities");
      }
      const previous = causalAssertions.get(item);
      if (previous !== undefined &&
          (previous.kind !== kind || previous.parents.length !== parents.length ||
           previous.parents.some((parent, parentIndex) => parent !== parents[parentIndex]))) {
        throw new Error(`live flow ${id} rewrites causal parents or semantic cause kind for ${item}`);
      }
      causalAssertions.set(item, Object.freeze({ kind, parents: Object.freeze(parents) }));
      return Object.freeze({ item, kind, parents: Object.freeze(parents) });
    });
  if (causalStages.length !== 0 && causalStages.length !== edges.length) {
    throw new Error(`live flow ${id} causal stages do not match its traversed edges`);
  }
  const firstNS = integer(source.first_ns ?? 0, `live flow ${id} first time`);
  const lastNS = integer(source.last_ns ?? 0, `live flow ${id} last time`);
  if (lastNS < firstNS) {
    throw new Error(`live flow ${id} contains impossible traversal timing`);
  }
  for (let index = 0; index < edgeNS.length; index++) {
    if ((index > 0 && edgeNS[index] < edgeNS[index - 1]) ||
        edgeNS[index] < firstNS || edgeNS[index] > lastNS) {
      throw new Error(`live flow ${id} contains impossible edge timing`);
    }
  }
  return Object.freeze({
    correlation: id,
    edges: Object.freeze(edges),
    edge_ns: Object.freeze(edgeNS),
    causal_stages: Object.freeze(causalStages),
    first_ns: firstNS,
    last_ns: lastNS,
    truncated: boolean(source.truncated, `live flow ${id} truncated`),
  });
}

function liveProjection(value) {
  const source = object(value, "live session snapshot");
  if (source.format_version !== 1 || typeof source.fingerprint !== "string" ||
      !DIGEST.test(source.fingerprint)) {
    throw new Error("live session snapshot has an invalid format or fingerprint");
  }
  const graphID = text(source.graph_id, "live graph ID");
  const revision = integer(source.graph_revision, "live graph revision", 1);
  if (source.error !== undefined && source.error !== "" && source.error !== "redacted") {
    throw new Error("live graph contains an unredacted error");
  }
  const nodeObject = object(source.nodes, "live nodes");
  const entries = Object.entries(nodeObject);
  if (entries.length === 0 || entries.length > MAX_ROWS) throw new Error("live nodes are not bounded");
  const nodes = new Map();
  for (const [rawID, value] of entries) {
    const id = text(rawID, "live node ID");
    nodes.set(id, liveNode(value, id));
  }
  const edgeObject = object(source.edges ?? {}, "live edges");
  if (Object.keys(edgeObject).length > MAX_ROWS) throw new Error("live edges are not bounded");
  const edges = {};
  for (const [rawID, value] of Object.entries(edgeObject)) {
    const id = text(rawID, "live edge ID");
    edges[id] = edgeProjection(value, id);
  }
  const flowObject = object(source.flows ?? {}, "live flows");
  if (Object.keys(flowObject).length > MAX_ROWS) throw new Error("live flows are not bounded");
  const flows = {};
  const causalAssertions = new Map();
  const causalIdentities = new Set();
  for (const [rawID, value] of Object.entries(flowObject)) {
    const id = text(rawID, "live flow ID");
    flows[id] = flowProjection(value, id, causalAssertions, causalIdentities);
  }
  const safeNodes = {};
  for (const [id, observed] of nodes) {
    const safeNode = {
      state: observed.state,
      active_runs: observed.activeRuns,
      first_trigger_ns: observed.firstTriggerNS,
      first_output_ns: observed.firstOutputNS,
      completion_ns: observed.completionNS,
      cancellation_ns: observed.cancellationNS,
      resolution: Object.freeze({ element: observed.element }),
    };
    if (observed.authorityDecision !== null) {
      safeNode.authority_decision = Object.freeze({
        kind: observed.authorityDecision.kind,
        operation: observed.authorityDecision.operation,
        crossed: observed.authorityDecision.crossed,
        at_ns: observed.authorityDecision.atNS,
      });
    }
    safeNodes[id] = Object.freeze(safeNode);
  }
  return Object.freeze({
    graphID, revision, fingerprint: source.fingerprint,
    state: text(source.state, "live graph state"), nodes, edges: Object.freeze(edges),
    flows: Object.freeze(flows),
    snapshot: Object.freeze({
      state: source.state, nodes: Object.freeze(safeNodes), edges: Object.freeze(edges),
      flows: Object.freeze(flows), dropped: integer(source.trace_dropped ?? 0, "live trace dropped"),
    }),
  });
}

function deltaProjection(value, sessionID, live) {
  const source = object(value, "session delta page");
  const graph = object(source.graph, "session delta graph");
  const after = integer(source.after, "session delta cursor");
  const next = integer(source.next, "session delta next cursor");
  if (source.format_version !== 1 || source.session_id !== sessionID || after !== 0 || next < after ||
      graph.format_version !== 1 || text(graph.id, "session delta graph ID") !== live.graphID ||
      integer(graph.revision, "session delta graph revision", 1) !== live.revision ||
      typeof graph.fingerprint !== "string" || !DIGEST.test(graph.fingerprint) ||
      graph.fingerprint !== live.fingerprint) {
    throw new Error("session delta page names another request or graph");
  }
  const compacted = boolean(source.compacted, "session delta compaction");
  const dropped = integer(source.dropped ?? 0, "session delta dropped count");
  let baselineSequence = 0;
  if (source.baseline !== undefined) {
    const baseline = object(source.baseline, "session delta baseline");
    baselineSequence = integer(baseline.sequence, "session delta baseline sequence", 1);
    if (baselineSequence > next) throw new Error("session delta baseline exceeds its next cursor");
  }
  const events = rows(source.events, "session delta events");
  if (events.length > DELTA_PAGE_LIMIT) throw new Error("session delta page exceeds its requested limit");
  let previous = Math.max(after, baselineSequence);
  for (let index = 0; index < events.length; index++) {
    const event = object(events[index], `session delta event ${index}`);
    const current = integer(event.sequence, `session delta event ${index} sequence`, 1);
    if (current <= previous || current > next) {
      throw new Error(`session delta event ${index} has an invalid sequence`);
    }
    previous = current;
  }
  if (events.length > 0 && previous !== next) {
    throw new Error("session delta cursor does not name its last event");
  }
  return Object.freeze({
    sessionID, after, next, compacted, dropped, baselineSequence, eventCount: events.length,
  });
}

function joinedProjection(liveValue, modelValue) {
  const live = liveProjection(liveValue);
  const model = staticProjection(modelValue);
  if (live.graphID !== model.graphID || live.revision !== model.revision ||
      live.fingerprint !== model.fingerprint || live.nodes.size !== model.nodes.length) {
    throw new Error("static model and live snapshot name different graphs");
  }
  const nodes = model.nodes.map((declared) => {
    const observed = live.nodes.get(declared.id);
    if (!observed || !sameIdentity(declared.element, observed.element)) {
      throw new Error(`static and live identity differ for node ${declared.id}`);
    }
    return Object.freeze({ declared, observed });
  });
  const declaredEdges = new Set(model.edges.map((edge) => edge.id));
  const expectedLiveEdges = new Set(declaredEdges);
  for (const boundary of model.boundaries) expectedLiveEdges.add(`boundary:${boundary.name}`);
  for (const edgeID of Object.keys(live.edges)) {
    if (!expectedLiveEdges.has(edgeID)) {
      throw new Error(`live snapshot contains undeclared edge ${edgeID}`);
    }
  }
  for (const edgeID of expectedLiveEdges) {
    if (!live.edges[edgeID]) throw new Error(`live snapshot omits declared edge ${edgeID}`);
  }
  const edges = model.edges.map((declared) => {
    const observed = live.edges[declared.id];
    if (observed.occupancy > declared.depth || observed.high_water > declared.depth) {
      throw new Error(`static and live depth disagree for edge ${declared.id}`);
    }
    return Object.freeze({ declared, observed });
  });
  const edgeByID = new Map(model.edges.map((edge) => [edge.id, edge]));
  const flows = Object.keys(live.flows).sort().map((id) => {
    const observed = live.flows[id];
    const stages = observed.edges.map((edgeID, index) => {
      const declared = edgeByID.get(edgeID);
      if (!declared) {
        throw new Error(`live flow ${id} contains unknown internal edge ${edgeID}`);
      }
      return Object.freeze({
        index: index + 1, declared,
        causal: observed.causal_stages.length === 0 ? null : observed.causal_stages[index],
        atNS: observed.edge_ns.length === 0 ? null : observed.edge_ns[index],
        deltaNS: observed.edge_ns.length === 0 || index === 0
          ? null : observed.edge_ns[index] - observed.edge_ns[index - 1],
      });
    });
    return Object.freeze({ observed, stages: Object.freeze(stages) });
  });
  return Object.freeze({
    live, model, nodes: Object.freeze(nodes), edges: Object.freeze(edges),
    flows: Object.freeze(flows),
  });
}

function node(name, value = "") {
  const result = document.createElement(name);
  result.textContent = value;
  return result;
}

function line(parent, label, value) {
  const row = node("p");
  const term = node("strong", `${label}: `);
  row.append(term, node("span", value));
  parent.append(row);
}

function list(value) {
  return value.length === 0 ? "none" : value.join(", ");
}

function timestamp(value) {
  return value === 0 ? "not observed" : `${value} ns from mount clock`;
}

function triggerLatency(trigger, value) {
  if (value === 0) return "not observed";
  if (trigger === 0) return "first trigger unavailable";
  if (value < trigger) return "observed before first trigger";
  return `${value - trigger} ns after first trigger`;
}

function endpointText(value) {
  return `${value.node}.${value.port}${value.lane ? `[${value.lane}]` : ""}`;
}

function queueWait(value) {
  if (value.dequeued === 0) return `${value.queue_wait_ns} ns cumulative; no dequeues`;
  return `${value.queue_wait_ns} ns cumulative; ${Math.floor(value.queue_wait_ns / value.dequeued)} ns per dequeue`;
}

function renderJoined(nodeContainer, edgeContainer, flowContainer, joined) {
  nodeContainer.replaceChildren();
  edgeContainer.replaceChildren();
  flowContainer.replaceChildren();
  for (const { declared, observed } of joined.nodes) {
    const card = node("article");
    card.dataset.nodeId = declared.id;
    card.dataset.state = observed.state;
    card.dataset.activeRuns = String(observed.activeRuns);
    card.append(node("h3", declared.id));
    line(card, "Element", `${declared.element.name}@${declared.element.revision}`);
    line(card, "Live state", observed.state);
    line(card, "Active runs", String(observed.activeRuns));
    const contract = node("div");
    contract.dataset.role = "reaction-contract";
    contract.append(node("h4", "Declared reaction"));
    line(contract, "Triggers", list(declared.reaction.triggers));
    line(contract, "Sampled state", list(declared.reaction.sampledState));
    line(contract, "Interrupts", list(declared.reaction.interrupts));
    line(contract, "Outcomes", list(declared.reaction.outcomes));
    line(contract, "Max concurrency", String(declared.reaction.maxConcurrency));
    line(contract, "Causal break", declared.reaction.breaksCycles ? "declared" : "not declared");
    card.append(contract);
    const timing = node("div");
    timing.dataset.role = "reaction-timing";
    timing.append(node("h4", "Observed timing"));
    line(timing, "First trigger", timestamp(observed.firstTriggerNS));
    line(timing, "First output", timestamp(observed.firstOutputNS));
    line(timing, "Trigger to first output",
      triggerLatency(observed.firstTriggerNS, observed.firstOutputNS));
    line(timing, "Completion", timestamp(observed.completionNS));
    line(timing, "Trigger to completion",
      triggerLatency(observed.firstTriggerNS, observed.completionNS));
    line(timing, "Cancellation", timestamp(observed.cancellationNS));
    line(timing, "Trigger to cancellation",
      triggerLatency(observed.firstTriggerNS, observed.cancellationNS));
    card.append(timing);
    const authority = node("div");
    authority.dataset.role = "effect-authority";
    authority.append(node("h4", "Declared effects and authority"));
    if (declared.effects.length === 0) {
      authority.append(node("p", "No effects declared."));
    } else {
      for (const effect of declared.effects) {
        line(authority, effect.name,
          `${effect.external ? "external" : "lifecycle"}; authority ${effect.authority || "none"}; ` +
          `${effect.reversible ? "reversible" : "not reversible"}`);
      }
    }
    card.append(authority);
    const decision = node("div");
    decision.dataset.role = "authority-decision";
    decision.append(node("h4", "Latest live authority decision"));
    if (observed.authorityDecision === null) {
      decision.append(node("p", "No authority decision observed."));
    } else {
      line(decision, "Outcome", observed.authorityDecision.kind);
      line(decision, "Operation", observed.authorityDecision.operation);
      line(decision, "Irreversible boundary",
        observed.authorityDecision.crossed ? "crossed" : "not crossed");
      line(decision, "Observed", `${observed.authorityDecision.atNS} ns from mount clock`);
    }
    card.append(decision);
    nodeContainer.append(card);
  }
  for (const { declared, observed } of joined.edges) {
    const card = node("article");
    card.dataset.edgeId = declared.id;
    card.dataset.delivery = declared.delivery;
    card.dataset.depth = String(declared.depth);
    card.dataset.occupancy = String(observed.occupancy);
    card.append(node("h3", declared.id));
    line(card, "Route", `${endpointText(declared.from)} → ${endpointText(declared.to)}`);
    line(card, "Contract", `${declared.type}; role ${declared.role}`);
    line(card, "Delivery", `${declared.delivery}; depth ${declared.depth}`);
    line(card, "Occupancy", `${observed.occupancy}/${declared.depth}`);
    line(card, "High water", `${observed.high_water}/${declared.depth}`);
    line(card, "Enqueued / dequeued", `${observed.enqueued} / ${observed.dequeued}`);
    line(card, "Dropped", String(observed.dropped));
    line(card, "Backpressure", String(observed.backpressure));
    line(card, "Queue wait", queueWait(observed));
    edgeContainer.append(card);
  }
  for (const { observed, stages } of joined.flows) {
    const card = node("article");
    card.dataset.flowId = observed.correlation;
    card.dataset.stageCount = String(stages.length);
    card.dataset.truncated = String(observed.truncated);
    card.append(node("h3", observed.correlation));
    line(card, "First traversal", timestamp(observed.first_ns));
    line(card, "Last traversal", timestamp(observed.last_ns));
    line(card, "Elapsed", `${observed.last_ns - observed.first_ns} ns`);
    line(card, "Retention", observed.truncated
      ? "truncated at a retention bound or refused stage" : "complete");
    const path = node("ol");
    path.dataset.role = "flow-stages";
    for (const stage of stages) {
      const edge = stage.declared;
      const timing = stage.atNS === null ? "time unavailable" :
        `${stage.atNS} ns from mount clock; ${stage.deltaNS === null
          ? "first retained stage" : `+${stage.deltaNS} ns`}`;
      const causal = stage.causal === null ? "causal lineage unavailable" :
        `causal ${stage.causal.kind === "" ? "" : `${stage.causal.kind} `}${stage.causal.item} ← ${stage.causal.parents.length === 0
          ? "root" : stage.causal.parents.join(", ")}`;
      path.append(node("li", `Stage ${stage.index}: ${endpointText(edge.from)} → ${endpointText(edge.to)} ` +
        `via ${edge.id} (${edge.type}; ${edge.delivery}); ${timing}; ${causal}`));
    }
    card.append(path);
    flowContainer.append(card);
  }
}

export default {
  name: "openrealtime.presentation.client.inspection-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const inspection = context.services.get("presentation.client.inspection");
    if (!slots || !inspection || typeof inspection.model !== "function" ||
        typeof inspection.deltas !== "function") {
      throw new Error("inspection view dependencies are unavailable");
    }
    const section = node("section");
    section.dataset.view = "inspection";
    const style = node("style", `
      section[data-view=inspection] { max-width:64rem; margin:1rem auto 2rem; padding:1rem;
        border:1px solid #8885; border-radius:.7rem; font-family:system-ui,sans-serif; }
      section[data-view=inspection] header { display:flex; align-items:center; gap:.75rem; }
      section[data-view=inspection] article { margin:.75rem 0; padding:.75rem; border:1px solid #8884;
        border-radius:.5rem; }
      section[data-view=inspection] article p { margin:.25rem 0; overflow-wrap:anywhere; }
      section[data-view=inspection] pre { overflow:auto; max-height:20rem; padding:.75rem;
        background:#7771; white-space:pre-wrap; }
    `);
    const header = node("header");
    header.append(node("h2", "Live graph"));
    const availability = node("span", "waiting");
    availability.id = "availability";
    const refresh = node("button", "Refresh");
    refresh.id = "refresh";
    refresh.disabled = true;
    header.append(availability, refresh);
    const identityView = node("p", "No scoped inspection capability.");
    identityView.id = "identity";
    const contractAvailability = node("p", "Static reaction contracts are unavailable.");
    contractAvailability.id = "contract-availability";
    const deltaAvailability = node("p", "Delta journal is unavailable.");
    deltaAvailability.id = "delta-availability";
    deltaAvailability.dataset.state = "waiting";
    const nodeList = node("div");
    nodeList.dataset.role = "inspection-nodes";
    const edgeList = node("div");
    edgeList.dataset.role = "inspection-edges";
    const flowList = node("div");
    flowList.dataset.role = "inspection-flows";
    const details = node("details");
    details.append(node("summary", "Raw redacted live evidence"));
    const snapshot = node("pre");
    snapshot.id = "snapshot";
    details.append(snapshot);
    section.append(style, header, identityView, contractAvailability, deltaAvailability,
      node("h3", "Nodes"), nodeList, node("h3", "Channels"), edgeList,
      node("h3", "Correlated flows"), flowList, details);
    let generation = 0;

    const load = async () => {
      const current = ++generation;
      refresh.disabled = true;
      availability.textContent = "loading";
      const sessionID = section.dataset.sessionId;
      const [liveResult, modelResult, deltaResult] = await Promise.allSettled([
        inspection.live(), inspection.model(), inspection.deltas(0, DELTA_PAGE_LIMIT),
      ]);
      if (current !== generation) return;
      try {
        if (liveResult.status !== "fulfilled") throw liveResult.reason;
        const live = liveProjection(liveResult.value);
        identityView.textContent = `${live.graphID} · r${live.revision} · ${live.fingerprint}`;
        snapshot.textContent = JSON.stringify(live.snapshot, null, 2);
        if (modelResult.status === "fulfilled") {
          const joined = joinedProjection(liveResult.value, modelResult.value);
          renderJoined(nodeList, edgeList, flowList, joined);
          contractAvailability.textContent = "Exact static reaction/effect contracts joined to live node evidence.";
          contractAvailability.dataset.state = "joined";
        } else {
          nodeList.replaceChildren();
          edgeList.replaceChildren();
          flowList.replaceChildren();
          contractAvailability.textContent = "Live evidence available; exact static reaction contracts are unavailable.";
          contractAvailability.dataset.state = "unavailable";
        }
        try {
          if (deltaResult.status !== "fulfilled") throw deltaResult.reason;
          const delta = deltaProjection(deltaResult.value, sessionID, live);
          deltaAvailability.textContent = `Delta journal: session ${delta.sessionID}; cursor ${delta.after} → ${delta.next}; ` +
            `${delta.eventCount} event${delta.eventCount === 1 ? "" : "s"}; ` +
            `${delta.baselineSequence === 0 ? "no baseline" : `baseline ${delta.baselineSequence}`}; ` +
            `${delta.compacted ? "compacted" : "continuous"}; ${delta.dropped} dropped.`;
          deltaAvailability.dataset.state = "loaded";
          deltaAvailability.dataset.after = String(delta.after);
          deltaAvailability.dataset.next = String(delta.next);
          deltaAvailability.dataset.events = String(delta.eventCount);
          deltaAvailability.dataset.baseline = String(delta.baselineSequence);
        } catch {
          deltaAvailability.textContent = "Delta journal is unavailable or does not match this session.";
          deltaAvailability.dataset.state = "invalid";
          delete deltaAvailability.dataset.after;
          delete deltaAvailability.dataset.next;
          delete deltaAvailability.dataset.events;
          delete deltaAvailability.dataset.baseline;
        }
        availability.textContent = "live";
      } catch (error) {
        snapshot.textContent = "";
        nodeList.replaceChildren();
        edgeList.replaceChildren();
        flowList.replaceChildren();
        availability.textContent = "unavailable";
        identityView.textContent = error?.message ?? String(error);
        contractAvailability.textContent = "Static/live evidence could not be joined safely.";
        contractAvailability.dataset.state = "invalid";
        deltaAvailability.textContent = "Delta journal could not be joined safely.";
        deltaAvailability.dataset.state = "invalid";
        delete deltaAvailability.dataset.after;
        delete deltaAvailability.dataset.next;
        delete deltaAvailability.dataset.events;
        delete deltaAvailability.dataset.baseline;
      } finally {
        if (current === generation) refresh.disabled = !inspection.available();
      }
    };
    const changed = inspection.subscribe((access) => {
      generation++;
      section.dataset.sessionId = access?.session_id ?? "";
      refresh.disabled = !access;
      availability.textContent = access ? "available" : "waiting";
      identityView.textContent = access
        ? `session ${access.session_id} · capability expires ${new Date(access.expires_at_ms).toISOString()}`
        : "No scoped inspection capability.";
      contractAvailability.textContent = "Static reaction contracts are unavailable.";
      contractAvailability.dataset.state = "waiting";
      deltaAvailability.textContent = "Delta journal is unavailable.";
      deltaAvailability.dataset.state = "waiting";
      delete deltaAvailability.dataset.after;
      delete deltaAvailability.dataset.next;
      delete deltaAvailability.dataset.events;
      delete deltaAvailability.dataset.baseline;
      snapshot.textContent = "";
      nodeList.replaceChildren();
      edgeList.replaceChildren();
      flowList.replaceChildren();
      if (access) load();
    });
    refresh.addEventListener("click", load);
    const unregister = slots.register("inspection.graph", section, 100);
    context.lifecycle.defer("inspection-view-events", () => refresh.removeEventListener("click", load));
    context.lifecycle.defer("inspection-view-access", changed);
    context.lifecycle.defer("inspection-view-slot", unregister);
  },
};
