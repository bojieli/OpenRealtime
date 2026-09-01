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
    throw new Error("source-read path is not canonical");
  }
  const parts = value.split("/");
  if (parts.some((part) => part === "" || part === "." || part === "..") ||
      parts.at(-1).startsWith(".openrealtime-authoring-")) {
    throw new Error("source-read path is not canonical");
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

function request(value) {
  only(value, ["format_version", "root_identity", "path"], "source-read request");
  if (value.format_version !== undefined && value.format_version !== 1) {
    throw new Error("source-read version is invalid");
  }
  return Object.freeze({
    format_version: 1,
    root_identity: digest(value.root_identity, "source-read root identity"),
    path: path(value.path),
  });
}

async function result(input, response, evidence) {
  only(response, ["format_version", "root_identity", "path", "source", "source_digest",
    "source_bytes", "result_digest"], "source-read result");
  if (response.format_version !== 1 || response.root_identity !== input.root_identity ||
      response.path !== input.path || typeof response.source !== "string" ||
      !wellFormed(response.source)) {
    throw new Error("source-read result names another source");
  }
  const sourceBytes = encoder.encode(response.source);
  if (sourceBytes.byteLength === 0 || sourceBytes.byteLength > MAX_SOURCE_BYTES ||
      !Number.isSafeInteger(response.source_bytes) || response.source_bytes !== sourceBytes.byteLength) {
    throw new Error("source-read result has invalid source bytes");
  }
  const sourceDigest = await hash(sourceBytes);
  if (response.source_digest !== sourceDigest) {
    throw new Error("source-read result source digest is invalid");
  }
  digest(response.source_digest, "source-read source digest");
  digest(response.result_digest, "source-read result digest");
  const parts = [encoder.encode("openrealtime.management.source-read-result/v1\0")];
  for (const field of [response.root_identity, response.path, response.source_digest]) {
    parts.push(...framed(field));
  }
  parts.push(uint64(response.source_bytes));
  const expectedResult = await hash(joined(parts));
  if (response.result_digest !== expectedResult || evidence !== `authoring:read:${expectedResult}`) {
    throw new Error("source-read result digest or host evidence is invalid");
  }
  return Object.freeze({
    format_version: 1,
    root_identity: response.root_identity,
    path: response.path,
    source: response.source,
    source_digest: response.source_digest,
    source_bytes: response.source_bytes,
    result_digest: expectedResult,
  });
}

export default {
  name: "openrealtime.presentation.client.management-source-reading",
  revision: 1,
  async mount(context) {
    const transport = context.services.get("presentation.client.management_transport");
    if (!transport) throw new Error("source-reading transport is unavailable");
    let disposed = false;
    context.publish("presentation.client.source_reading", Object.freeze({
      async read(value) {
        if (disposed) throw new Error("source-reading client is disposed");
        const requested = request(value);
        const response = await transport.authoring("read", requested);
        if (disposed) throw new Error("source-reading client is disposed");
        return result(requested, response.value, response.identity);
      },
    }));
    context.lifecycle.defer("management-source-reading", () => { disposed = true; });
  },
};
