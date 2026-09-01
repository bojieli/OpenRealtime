const MANAGEMENT_PROTOCOL = "openrealtime.management.v1";
const CAPABILITY_HEADER = "OpenRealtime-Management-Token";
const IDENTITY_HEADER = "OpenRealtime-Management-Identity";
const MAX_JSON_BYTES = 64 << 20;
const MAX_SCHEMA_BYTES = 129 << 20;
const MAX_AUTHORING_REQUEST_BYTES = 16 << 20;
const MAX_STREAM_CHUNKS = 65_536;
const encoder = new TextEncoder();

function endpoint(context, name, method, path) {
  const matches = (context.manifest.endpoints ?? []).filter((candidate) => candidate.name === name);
  if (matches.length !== 1) throw new Error(`management endpoint ${name} is not uniquely declared`);
  const declared = matches[0];
  if (declared.method !== method || declared.path !== path || declared.protocol !== MANAGEMENT_PROTOCOL ||
      declared.catalog_digest) {
    throw new Error(`management endpoint ${name} has another immutable contract`);
  }
  return declared.path;
}

function digest(value, label) {
  if (typeof value !== "string" || !/^sha256:[0-9a-f]{64}$/.test(value)) {
    throw new Error(`${label} is not a canonical digest`);
  }
  return value;
}

function identity(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      typeof value.name !== "string" ||
      !/^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$/.test(value.name) ||
      !Number.isSafeInteger(value.revision) || value.revision < 1) {
    throw new Error(`${label} has an invalid symbolic identity`);
  }
  return Object.freeze({
    name: value.name, revision: value.revision, digest: digest(value.digest, `${label} digest`),
  });
}

async function readBounded(response, maximum) {
  const declared = response.headers.get("Content-Length");
  if (declared !== null) {
    if (!/^(0|[1-9][0-9]*)$/.test(declared)) throw new Error("management response has invalid length");
    const length = Number(declared);
    if (!Number.isSafeInteger(length) || length > maximum) {
      throw new Error("management response exceeds its route limit");
    }
  }
  if (!response.body?.getReader) {
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength > maximum) throw new Error("management response exceeds its route limit");
    return bytes;
  }
  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!(value instanceof Uint8Array)) {
        throw new Error("management response stream is invalid");
      }
      chunks.push(value);
      total += value.byteLength;
      if (total > maximum || chunks.length > MAX_STREAM_CHUNKS) {
        throw new Error("management response exceeds its route limit");
      }
    }
  } catch (error) {
    try { await reader.cancel(); } catch {}
    throw error;
  }
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

function staticRequest(action, input) {
  switch (action) {
    case "graph": {
      const fingerprint = digest(input?.fingerprint, "graph fingerprint");
      return { segments: ["graphs", fingerprint], operation: "graph.read", resource: fingerprint,
        maximum: MAX_JSON_BYTES };
    }
    case "element": {
      const exact = identity(input, "element identity");
      return { segments: ["descriptors", "elements", exact.name, String(exact.revision), exact.digest],
        operation: "descriptor.read",
        resource: `element:${exact.name}@${exact.revision}:${exact.digest}`, maximum: MAX_JSON_BYTES };
    }
    case "plugin": {
      const exact = identity(input, "plugin identity");
      return { segments: ["descriptors", "plugins", exact.name, String(exact.revision), exact.digest],
        operation: "descriptor.read",
        resource: `plugin:${exact.name}@${exact.revision}:${exact.digest}`, maximum: MAX_JSON_BYTES };
    }
    case "schema": {
      const fingerprint = digest(input?.fingerprint, "values-schema fingerprint");
      return { segments: ["schemas", "values", fingerprint], operation: "schema.read",
        resource: fingerprint, maximum: MAX_SCHEMA_BYTES };
    }
    default:
      throw new Error("unsupported static management resource");
  }
}

function authoringRequest(action, input) {
  switch (action) {
    case "analyze": return { operation: "authoring.analyze", resource: "authoring" };
    case "compile": return { operation: "authoring.compile", resource: "authoring" };
    case "render": return { operation: "authoring.render", resource: "authoring" };
    case "read": {
      if (!input || typeof input !== "object" || Array.isArray(input) ||
          typeof input.root_identity !== "string" || !/^sha256:[0-9a-f]{64}$/.test(input.root_identity)) {
        throw new Error("source read has an invalid root identity");
      }
      return { operation: "authoring.source.read", resource: input.root_identity };
    }
    case "write": {
      if (!input || typeof input !== "object" || Array.isArray(input) ||
          typeof input.root_identity !== "string" || !/^sha256:[0-9a-f]{64}$/.test(input.root_identity)) {
        throw new Error("source publication has an invalid root identity");
      }
      if (input.mode === "create") {
        return { operation: "authoring.source.create", resource: input.root_identity };
      }
      if (input.mode === "update") {
        return { operation: "authoring.source.update", resource: input.root_identity };
      }
      throw new Error("source publication has an invalid mode");
    }
    default: throw new Error("unsupported authoring operation");
  }
}

export default {
  name: "openrealtime.presentation.client.management-transport",
  revision: 1,
  async mount(context) {
    if (!context.permissions.allows("network.connect", "host-management", "static") ||
        !context.permissions.allows("network.connect", "host-management", "authoring")) {
      throw new Error("management transport lacks its static or authoring deployment grant");
    }
    const capabilities = context.services.get("presentation.client.management_operator_access");
    const codec = context.services.get("presentation.client.strict_json");
    if (!capabilities || !codec) throw new Error("management transport dependencies are unavailable");
    const staticBase = endpoint(context, "management.static", "GET", "/client/v1/management");
    const authoringBase = endpoint(context, "management.authoring", "POST", "/client/v1/management/authoring");
    const active = new Set();
    let disposed = false;

    const execute = async ({ method, base, segments, operation, resource, body, maximum }) => {
      if (disposed) throw new Error("management transport is disposed");
      const lease = capabilities.lease(operation, resource);
      const controller = new AbortController();
      const canceled = () => controller.abort(lease.signal.reason ??
        new DOMException("operator capability changed", "AbortError"));
      if (lease.signal.aborted) canceled();
      else lease.signal.addEventListener("abort", canceled, { once: true });
      active.add(controller);
      try {
        const url = new URL(`${base}/${segments.map(encodeURIComponent).join("/")}`, location.origin);
        if (url.origin !== location.origin || url.username || url.password || url.hash || url.search) {
          throw new Error("management transport constructed a non-canonical URL");
        }
        let encoded;
        if (body !== undefined) {
          encoded = codec.stable(body);
          if (typeof encoded !== "string" || encoder.encode(encoded).byteLength > MAX_AUTHORING_REQUEST_BYTES) {
            throw new Error("authoring request exceeds its route limit");
          }
        }
        const headers = { Accept: "application/json", [CAPABILITY_HEADER]: lease.token };
        if (encoded !== undefined) headers["Content-Type"] = "application/json";
        const response = await fetch(url, {
          method, cache: "no-store", credentials: "same-origin", redirect: "error",
          referrerPolicy: "no-referrer", headers, body: encoded, signal: controller.signal,
        });
        if (!lease.current()) throw new Error("operator capability changed during management request");
        if (!response.ok) {
          try { await response.body?.cancel(); } catch {}
          throw new Error(`management endpoint returned ${response.status}`);
        }
        const mediaType = (response.headers.get("Content-Type") ?? "").split(";", 1)[0].trim().toLowerCase();
        if (mediaType !== "application/json") throw new Error("management response is not JSON");
        const bytes = await readBounded(response, maximum);
        if (!lease.current()) throw new Error("operator capability changed during management response");
        const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
        const value = codec.parse(text);
        const responseIdentity = response.headers.get(IDENTITY_HEADER);
        if (!responseIdentity || responseIdentity.length > 1024 || /[\0\r\n,]/.test(responseIdentity)) {
          throw new Error("management response lacks exact host identity evidence");
        }
        return Object.freeze({ identity: responseIdentity, value });
      } finally {
        active.delete(controller);
        lease.signal.removeEventListener("abort", canceled);
      }
    };

    context.publish("presentation.client.management_transport", Object.freeze({
      static(action, input) {
        const spec = staticRequest(action, input);
        return execute({ method: "GET", base: staticBase, segments: spec.segments,
          operation: spec.operation, resource: spec.resource, maximum: spec.maximum });
      },
      authoring(action, body) {
        const spec = authoringRequest(action, body);
        if (action === "read" &&
            !context.permissions.allows("network.connect", "host-management", "source-read")) {
          throw new Error("management transport lacks its source-read deployment grant");
        }
        if (action === "write" &&
            !context.permissions.allows("network.connect", "host-management", "publication")) {
          throw new Error("management transport lacks its source-publication deployment grant");
        }
        return execute({ method: "POST", base: authoringBase, segments: [action],
          operation: spec.operation, resource: spec.resource, body, maximum: MAX_JSON_BYTES });
      },
    }));
    context.lifecycle.defer("management-transport", () => {
      disposed = true;
      for (const controller of active) {
        controller.abort(new DOMException("management transport disposed", "AbortError"));
      }
      active.clear();
    });
  },
};
