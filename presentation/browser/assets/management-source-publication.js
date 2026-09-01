const DIGEST = /^sha256:[0-9a-f]{64}$/;
const MAX_SOURCE_BYTES = 1 << 20;
const MAX_PATH_BYTES = 4096;
const encoder = new TextEncoder();

function object(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} is not an object`);
  }
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
  if (typeof value !== "string" || !DIGEST.test(value)) {
    throw new Error(`${label} is not a canonical digest`);
  }
  return value;
}

function wellFormed(value) {
  for (let index = 0; index < value.length; index++) {
    const unit = value.charCodeAt(index);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      if (++index >= value.length) return false;
      const next = value.charCodeAt(index);
      if (next < 0xdc00 || next > 0xdfff) return false;
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false;
    }
  }
  return true;
}

function path(value) {
  if (typeof value !== "string" || value.length === 0 || !wellFormed(value) ||
      encoder.encode(value).byteLength > MAX_PATH_BYTES || /[\\\0\r\n]/.test(value) ||
      value.startsWith("/") || !/\.(?:ortg|ya?ml|json)$/i.test(value)) {
    throw new Error("source publication path is not canonical");
  }
  const parts = value.split("/");
  if (parts.some((part) => part === "" || part === "." || part === "..") ||
      parts.at(-1).startsWith(".openrealtime-authoring-")) {
    throw new Error("source publication path is not canonical");
  }
  return value;
}

async function hash(value) {
  const bytes = value instanceof Uint8Array ? value : encoder.encode(value);
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  return `sha256:${[...sum].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function uint64(value) {
  const result = new Uint8Array(8);
  new DataView(result.buffer).setBigUint64(0, BigInt(value), false);
  return result;
}

function framed(value) {
  const bytes = encoder.encode(value);
  return [uint64(bytes.byteLength), bytes];
}

function joined(parts) {
  const size = parts.reduce((total, part) => total + part.byteLength, 0);
  const result = new Uint8Array(size);
  let offset = 0;
  for (const part of parts) {
    result.set(part, offset);
    offset += part.byteLength;
  }
  return result;
}

async function request(value) {
  only(value, ["format_version", "root_identity", "mode", "path", "source",
    "expected_source_digest"], "source publication request");
  if (value.format_version !== undefined && value.format_version !== 1) {
    throw new Error("source publication version is invalid");
  }
  const sourcePath = path(value.path);
  if (typeof value.source !== "string" || !wellFormed(value.source)) {
    throw new Error("source publication payload is invalid");
  }
  const sourceBytes = encoder.encode(value.source);
  if (sourceBytes.byteLength === 0 || sourceBytes.byteLength > MAX_SOURCE_BYTES) {
    throw new Error("source publication payload is invalid");
  }
  const sourceDigest = await hash(sourceBytes);
  const exact = {
    format_version: 1,
    root_identity: digest(value.root_identity, "source publication root identity"),
    mode: value.mode,
    path: sourcePath,
    source: value.source,
  };
  switch (value.mode) {
    case "create":
      if (value.expected_source_digest !== undefined && value.expected_source_digest !== "") {
        throw new Error("source create has an expected predecessor digest");
      }
      break;
    case "update":
      exact.expected_source_digest = digest(value.expected_source_digest,
        "source publication predecessor digest");
      if (exact.expected_source_digest === sourceDigest) {
        throw new Error("source update is unchanged");
      }
      break;
    default:
      throw new Error("source publication mode is invalid");
  }
  return Object.freeze({ exact: Object.freeze(exact), sourceBytes: sourceBytes.byteLength, sourceDigest });
}

async function receipt(input, requested, response, evidence) {
  only(response, ["format_version", "root_identity", "mode", "path", "previous_source_digest",
    "source_digest", "source_bytes", "cleanup_pending", "receipt_digest"],
  "source publication receipt");
  const previous = response.previous_source_digest ?? "";
  const cleanup = response.cleanup_pending ?? false;
  const expectedPrevious = input.mode === "update" ? input.expected_source_digest : "";
  if (response.format_version !== 1 || response.root_identity !== input.root_identity ||
      response.mode !== input.mode || response.path !== input.path || previous !== expectedPrevious ||
      response.source_digest !== requested.sourceDigest ||
      response.source_bytes !== requested.sourceBytes || !Number.isSafeInteger(response.source_bytes) ||
      response.source_bytes < 1 || typeof cleanup !== "boolean") {
    throw new Error("source publication receipt names another mutation");
  }
  digest(response.source_digest, "source publication source digest");
  digest(response.receipt_digest, "source publication receipt digest");
  const parts = [encoder.encode("openrealtime.management.source-write-receipt/v1\0")];
  for (const field of [response.root_identity, response.mode, response.path, previous,
    response.source_digest]) {
    parts.push(...framed(field));
  }
  parts.push(uint64(response.source_bytes), Uint8Array.of(cleanup ? 1 : 0));
  const expectedReceipt = await hash(joined(parts));
  if (response.receipt_digest !== expectedReceipt || evidence !== `authoring:write:${expectedReceipt}`) {
    throw new Error("source publication receipt digest or host evidence is invalid");
  }
  return Object.freeze({
    format_version: 1, root_identity: response.root_identity, mode: response.mode, path: response.path,
    previous_source_digest: previous, source_digest: response.source_digest,
    source_bytes: response.source_bytes, cleanup_pending: cleanup, receipt_digest: expectedReceipt,
  });
}

export default {
  name: "openrealtime.presentation.client.management-source-publication",
  revision: 1,
  async mount(context) {
    const transport = context.services.get("presentation.client.management_transport");
    if (!transport) throw new Error("source publication transport is unavailable");
    let disposed = false;
    context.publish("presentation.client.source_publication", Object.freeze({
      async publish(value) {
        if (disposed) throw new Error("source publication client is disposed");
        const requested = await request(value);
        if (disposed) throw new Error("source publication client is disposed");
        const response = await transport.authoring("write", requested.exact);
        if (disposed) throw new Error("source publication client is disposed");
        return receipt(requested.exact, requested, response.value, response.identity);
      },
    }));
    context.lifecycle.defer("management-source-publication", () => { disposed = true; });
  },
};
