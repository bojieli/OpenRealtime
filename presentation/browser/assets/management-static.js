const DIGEST = /^sha256:[0-9a-f]{64}$/;
const SYMBOL = /^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$/;
const MAX_ROWS = 65_536;
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

function exactIdentity(value, label) {
  object(value, label);
  only(value, ["name", "revision", "digest"], label);
  if (typeof value.name !== "string" || !SYMBOL.test(value.name) ||
      !Number.isSafeInteger(value.revision) || value.revision < 1) {
    throw new Error(`${label} is invalid`);
  }
  return Object.freeze({ name: value.name, revision: value.revision,
    digest: digest(value.digest, `${label} digest`) });
}

function boundedRows(value, label, required = true) {
  if (value === undefined && !required) return [];
  if (!Array.isArray(value) || value.length > MAX_ROWS) throw new Error(`${label} is not a bounded array`);
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

function validateGraph(value, fingerprint, evidence) {
  only(value, ["format_version", "id", "revision", "fingerprint", "lineage", "nodes", "edges",
    "boundaries", "scopes"], "graph IR");
  if (value.format_version !== 1 || typeof value.id !== "string" || value.id.trim() === "" ||
      !Number.isSafeInteger(value.revision) || value.revision < 1 || value.fingerprint !== fingerprint ||
      evidence !== `graph:${fingerprint}`) {
    throw new Error("graph IR changed immutable identity");
  }
  const nodes = boundedRows(value.nodes, "graph nodes");
  if (nodes.length === 0) throw new Error("graph IR has no nodes");
  boundedRows(value.lineage, "graph lineage", false);
  boundedRows(value.edges, "graph edges", false);
  boundedRows(value.boundaries, "graph boundaries", false);
  boundedRows(value.scopes, "graph scopes", false);
  const ids = new Set();
  for (const node of nodes) {
    object(node, "graph node");
    if (typeof node.id !== "string" || node.id.length === 0 || ids.has(node.id)) {
      throw new Error("graph IR has an invalid or duplicate node identity");
    }
    ids.add(node.id);
    exactIdentity(node.element, "graph element identity");
    if (!Array.isArray(node.ports) || node.ports.length === 0 || node.ports.length > MAX_ROWS) {
      throw new Error("graph node has invalid ports");
    }
  }
  return frozen(value);
}

function validateDescriptor(value, requested, kind, evidence) {
  const fields = kind === "element"
    ? ["format_version", "name", "revision", "generics", "ports", "reaction", "state_schema",
      "config_schema", "dependencies", "effects", "composite_fingerprint"]
    : ["format_version", "name", "revision", "realm", "platforms", "provides", "requires", "events",
      "config_schema", "state_schema", "permissions", "assets", "lifecycle"];
  only(value, fields, `${kind} descriptor`);
  if (value.format_version !== 1 || value.name !== requested.name || value.revision !== requested.revision ||
      evidence !== `${kind}:${requested.name}@${requested.revision}:${requested.digest}`) {
    throw new Error(`${kind} descriptor changed immutable identity`);
  }
  if (kind === "element") {
    if (!Array.isArray(value.ports) || value.ports.length === 0 || value.ports.length > MAX_ROWS) {
      throw new Error("element descriptor ports are invalid");
    }
  } else if (!Array.isArray(value.platforms) || value.platforms.length === 0 ||
      value.platforms.length > MAX_ROWS || !new Set(["server", "presentation_host", "client"]).has(value.realm)) {
    throw new Error("plugin descriptor platform or realm is invalid");
  }
  return frozen(value);
}

async function validateSchema(value, fingerprint, evidence) {
  only(value, ["format_version", "schema", "digest", "complete", "nodes", "contracts", "unresolved"],
    "values-schema resource");
  if (value.format_version !== 1 || !DIGEST.test(value.digest) ||
      evidence !== `schema:${fingerprint}:${value.digest}` ||
      typeof value.schema !== "string" || encoder.encode(value.schema).byteLength > (64 << 20) ||
      typeof value.complete !== "boolean") {
    throw new Error("values schema changed immutable identity");
  }
  const unresolved = boundedRows(value.unresolved, "unresolved schemas");
  boundedRows(value.nodes, "schema nodes");
  boundedRows(value.contracts, "schema contracts");
  if (value.complete !== (unresolved.length === 0)) throw new Error("values schema completeness is inconsistent");
  const bytes = encoder.encode(value.schema);
  const hash = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  const actual = `sha256:${[...hash].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
  if (actual !== value.digest) throw new Error("values schema content digest is invalid");
  return frozen(value);
}

export default {
  name: "openrealtime.presentation.client.management-static",
  revision: 1,
  async mount(context) {
    const transport = context.services.get("presentation.client.management_transport");
    if (!transport) throw new Error("static management transport is unavailable");
    let disposed = false;
    const ready = () => {
      if (disposed) throw new Error("static management client is disposed");
    };
    context.publish("presentation.client.management_static", Object.freeze({
      async graph(fingerprint) {
        ready();
        digest(fingerprint, "graph fingerprint");
        const response = await transport.static("graph", { fingerprint });
        ready();
        return validateGraph(response.value, fingerprint, response.identity);
      },
      async element(identity) {
        ready();
        const requested = exactIdentity(identity, "element identity");
        const response = await transport.static("element", requested);
        ready();
        return validateDescriptor(response.value, requested, "element", response.identity);
      },
      async plugin(identity) {
        ready();
        const requested = exactIdentity(identity, "plugin identity");
        const response = await transport.static("plugin", requested);
        ready();
        return validateDescriptor(response.value, requested, "plugin", response.identity);
      },
      async valuesSchema(fingerprint) {
        ready();
        digest(fingerprint, "values-schema fingerprint");
        const response = await transport.static("schema", { fingerprint });
        ready();
        return validateSchema(response.value, fingerprint, response.identity);
      },
    }));
    context.lifecycle.defer("management-static", () => { disposed = true; });
  },
};
