const DIGEST = /^sha256:[0-9a-f]{64}$/;
const SYMBOL = /^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$/;
const MAX_SOURCE_BYTES = 1 << 20;
const MAX_ITEMS = 65_536;
const MAX_TEXT_BYTES = 64 << 20;
const encoder = new TextEncoder();

function object(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} is not an object`);
  return value;
}

function only(value, fields, label) {
  object(value, label);
  const allowed = new Set(fields);
  for (const field of Object.keys(value)) {
    if (!allowed.has(field)) throw new Error(`${label} has unknown field ${field}`);
  }
}

function digest(value, label) {
  if (typeof value !== "string" || !DIGEST.test(value)) throw new Error(`${label} is not a canonical digest`);
  return value;
}

function identity(value, label) {
  only(value, ["name", "revision", "digest"], label);
  if (typeof value.name !== "string" || !SYMBOL.test(value.name) ||
      !Number.isSafeInteger(value.revision) || value.revision < 1) throw new Error(`${label} is invalid`);
  digest(value.digest, `${label} digest`);
}

function rows(value, label) {
  if (!Array.isArray(value) || value.length > MAX_ITEMS) throw new Error(`${label} is not a bounded array`);
  return value;
}

function frozen(value) {
  const copy = structuredClone(value);
  const visit = (entry) => {
    if (!entry || typeof entry !== "object" || Object.isFrozen(entry)) return entry;
    for (const child of Object.values(entry)) visit(child);
    return Object.freeze(entry);
  };
  return visit(copy);
}

function document(input, analyze = false) {
  only(input, ["path", "source", "lock", "channel_depth", "revision"], "authoring document");
  if (typeof input.path !== "string" || input.path.length === 0 || input.path.length > 4096 ||
      input.path.trim() !== input.path || /[\0\r\n]/.test(input.path) ||
      typeof input.source !== "string" || encoder.encode(input.source).byteLength === 0 ||
      encoder.encode(input.source).byteLength > MAX_SOURCE_BYTES ||
      !/\.(?:ortg|ya?ml|json)$/i.test(input.path) || (analyze && !/\.ortg$/i.test(input.path)) ||
      (input.revision !== undefined && (!Number.isSafeInteger(input.revision) || input.revision < 0))) {
    throw new Error("authoring document is invalid");
  }
  if (input.channel_depth !== undefined) {
    object(input.channel_depth, "channel-depth overrides");
    for (const [edge, depth] of Object.entries(input.channel_depth)) {
      if (!edge || edge.trim() !== edge || !Number.isSafeInteger(depth) || depth < 1 || depth > 1 << 20) {
        throw new Error("authoring channel-depth override is invalid");
      }
    }
  }
  if (input.lock !== undefined && input.lock !== null) validateLock(input.lock);
  return frozen(input);
}

function validateLock(lock) {
  only(lock, ["format_version", "elements"], "resolution lock");
  if (lock.format_version !== 1) throw new Error("resolution lock format is invalid");
  const entries = rows(lock.elements, "resolution entries");
  const references = new Set();
  for (const entry of entries) {
    only(entry, ["reference", "identity"], "resolution entry");
    if (typeof entry.reference !== "string" || entry.reference.length === 0 || references.has(entry.reference)) {
      throw new Error("resolution lock has an invalid or duplicate reference");
    }
    references.add(entry.reference);
    identity(entry.identity, "resolved element identity");
  }
}

function validateGraph(graph, fingerprint = graph?.fingerprint) {
  object(graph, "graph IR");
  if (graph.format_version !== 1 || typeof graph.id !== "string" || graph.id.trim() === "" ||
      !Number.isSafeInteger(graph.revision) || graph.revision < 1 || graph.fingerprint !== fingerprint ||
      !DIGEST.test(graph.fingerprint) || !Array.isArray(graph.nodes) || graph.nodes.length === 0 ||
      graph.nodes.length > MAX_ITEMS) {
    throw new Error("compiled graph IR is invalid");
  }
  const seen = new Set();
  for (const node of graph.nodes) {
    object(node, "graph node");
    if (typeof node.id !== "string" || node.id.length === 0 || seen.has(node.id) ||
        !Array.isArray(node.ports) || node.ports.length === 0 || node.ports.length > MAX_ITEMS) {
      throw new Error("compiled graph node is invalid");
    }
    seen.add(node.id);
    identity(node.element, "graph element identity");
  }
}

async function sourceDigest(source) {
  const bytes = new Uint8Array(await crypto.subtle.digest("SHA-256", encoder.encode(source)));
  return `sha256:${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function validateReport(report, collection, label) {
  object(report, label);
  const values = rows(report[collection], `${label} items`);
  if (!Number.isSafeInteger(report.total) || report.total < values.length || report.total > MAX_ITEMS ||
      Boolean(report.incomplete) !== (values.length < report.total)) {
    throw new Error(`${label} bounds are invalid`);
  }
  return values;
}

function validateAnalysis(value, requestedDigest, evidence) {
  only(value, ["source_digest", "parsed", "recovered", "canonical", "diagnostics", "catalog", "formatting"],
    "analysis result");
  if (value.source_digest !== requestedDigest || evidence !== `authoring:analyze:${requestedDigest}` ||
      typeof value.parsed !== "boolean" || typeof value.recovered !== "boolean" ||
      typeof value.canonical !== "boolean" || value.parsed === value.recovered ||
      (value.canonical && !value.parsed)) {
    throw new Error("analysis result changed source identity");
  }
  const diagnostics = validateReport(value.diagnostics, "items", "diagnostic report");
  for (const diagnostic of diagnostics) {
    object(diagnostic, "diagnostic");
    if (typeof diagnostic.code !== "string" || diagnostic.code.length === 0 || diagnostic.code.length > 256 ||
        typeof diagnostic.message !== "string" || diagnostic.message.length === 0 ||
        encoder.encode(diagnostic.message).byteLength > (64 << 10)) {
      throw new Error("analysis diagnostic is invalid");
    }
  }
  const metadata = validateReport(value.catalog, "elements", "metadata report");
  let previous = "";
  for (const element of metadata) {
    object(element, "element metadata");
    identity(element.identity, "metadata element identity");
    if (previous && element.identity.name <= previous) throw new Error("element metadata is not canonical");
    previous = element.identity.name;
    object(element.config, "element configuration metadata");
    if (!Array.isArray(element.config.properties) || element.config.properties.length > MAX_ITEMS) {
      throw new Error("configuration property metadata is invalid");
    }
  }
  return frozen(value);
}

function validateCompile(value, input, evidence) {
  only(value, ["graph", "lock"], "compile result");
  validateGraph(value.graph);
  validateLock(value.lock);
  const revision = input.revision || 1;
  if (value.graph.revision !== revision || evidence !== `authoring:compile:${value.graph.fingerprint}`) {
    throw new Error("compile result changed graph identity");
  }
  return frozen(value);
}

function validateRender(value, graph, format, evidence) {
  only(value, ["fingerprint", "format", "model", "text"], "render result");
  if (value.fingerprint !== graph.fingerprint || value.format !== format ||
      evidence !== `authoring:render:${format}:${graph.fingerprint}`) {
    throw new Error("render result changed graph identity");
  }
  if (format === "model") {
    if (!value.model || value.text !== undefined) throw new Error("model rendering has an invalid shape");
  } else if (typeof value.text !== "string" || encoder.encode(value.text).byteLength > MAX_TEXT_BYTES ||
      value.model !== undefined) {
    throw new Error("text rendering has an invalid shape");
  }
  return frozen(value);
}

export default {
  name: "openrealtime.presentation.client.management-authoring",
  revision: 1,
  async mount(context) {
    const transport = context.services.get("presentation.client.management_transport");
    if (!transport) throw new Error("authoring management transport is unavailable");
    let disposed = false;
    const ready = () => {
      if (disposed) throw new Error("authoring management client is disposed");
    };
    context.publish("presentation.client.management_authoring", Object.freeze({
      async analyze(input) {
        ready();
        const exact = document(input, true);
        const fingerprint = await sourceDigest(exact.source);
        const response = await transport.authoring("analyze", exact);
        ready();
        return validateAnalysis(response.value, fingerprint, response.identity);
      },
      async compile(input) {
        ready();
        const exact = document(input, false);
        const response = await transport.authoring("compile", exact);
        ready();
        return validateCompile(response.value, exact, response.identity);
      },
      async render(graph, format = "model") {
        ready();
        validateGraph(graph);
        if (!new Set(["model", "mermaid", "dot"]).has(format)) throw new Error("render format is invalid");
        const response = await transport.authoring("render", { graph, format });
        ready();
        return validateRender(response.value, graph, format, response.identity);
      },
    }));
    context.lifecycle.defer("management-authoring", () => { disposed = true; });
  },
};
