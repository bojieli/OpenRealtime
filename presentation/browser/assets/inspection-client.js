const MAX_MANAGEMENT_BYTES = 32 << 20;
const MAX_STREAM_CHUNKS = 32_768;

function endpoint(context) {
  const matches = (context.manifest.endpoints ?? []).filter(
    (candidate) => candidate.name === "management.sessions");
  if (matches.length !== 1 || matches[0].method !== "GET" ||
      matches[0].path !== "/client/v1/management/sessions" ||
      matches[0].protocol !== "openrealtime.management.v1" || matches[0].catalog_digest) {
    throw new Error("management session endpoint has another immutable contract");
  }
  return matches[0].path;
}

function canonicalSession(value) {
  const session = String(value ?? "");
  if (!/^[A-Za-z0-9._:-]{1,256}$/.test(session)) {
    throw new Error("management access has an invalid session identity");
  }
  return session;
}

function canonicalNegotiatedPath(session) {
  return `/openrealtime/v1/sessions/${session}/live`;
}

async function readBounded(response) {
  const declared = response.headers.get("Content-Length");
  if (declared !== null) {
    if (!/^(0|[1-9][0-9]*)$/.test(declared)) {
      throw new Error("inspection response has invalid length");
    }
    const length = Number(declared);
    if (!Number.isSafeInteger(length) || length > MAX_MANAGEMENT_BYTES) {
      throw new Error("inspection response exceeds limit");
    }
  }
  if (!response.body?.getReader) {
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength > MAX_MANAGEMENT_BYTES) throw new Error("inspection response exceeds limit");
    return bytes;
  }
  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!(value instanceof Uint8Array)) throw new Error("inspection response stream is invalid");
      chunks.push(value);
      total += value.byteLength;
      if (total > MAX_MANAGEMENT_BYTES || chunks.length > MAX_STREAM_CHUNKS) {
        throw new Error("inspection response exceeds limit");
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

export default {
  name: "openrealtime.presentation.client.inspection",
  revision: 1,
  async mount(context) {
    if (!context.permissions.allows("network.connect", "host-management", "http")) {
      throw new Error("inspection client lacks its deployment grant");
    }
    const accessSource = context.services.get("presentation.client.inspection_access");
    const codec = context.services.get("presentation.client.strict_json");
    if (!accessSource || !codec) throw new Error("inspection access or strict JSON service is unavailable");
    const base = endpoint(context);
    let access = accessSource.current();
    const listeners = new Set();
    const reads = new Set();
    const projectedAccess = () => access ? Object.freeze({
      session_id: access.session_id,
      expires_at_ms: access.expires_at_ms,
    }) : null;
    const notify = () => {
      for (const listener of listeners) {
        try { listener(projectedAccess()); } catch {}
      }
    };
    const stopAccess = accessSource.subscribe((next) => {
      const changed = access?.token !== next?.token || access?.session_id !== next?.session_id ||
        access?.path !== next?.path ||
        access?.expires_at_ms !== next?.expires_at_ms;
      // The reducer publishes on every protocol event. An unchanged narrow
      // capability is not a management lifecycle event: notifying here made
      // views discard a completed snapshot and issue another read for every
      // text/audio/tool delta, creating visible flicker and needless load.
      if (!changed) return;
      for (const controller of reads) controller.abort();
      reads.clear();
      access = next ? Object.freeze({ ...next }) : null;
      notify();
    });

    const read = async (resource, parameters = {}) => {
      const current = access;
      if (!current) throw new Error("session inspection is unavailable");
      if (Date.now() >= current.expires_at_ms) throw new Error("session inspection capability expired");
      if (!new Set(["live", "deltas", "trace"]).has(resource)) {
        throw new Error("unsupported inspection resource");
      }
      const session = canonicalSession(current.session_id);
      if (current.path !== canonicalNegotiatedPath(session)) {
        throw new Error("session inspection capability is bound to another management path");
      }
      const url = new URL(`${base}/${encodeURIComponent(session)}/${resource}`, location.origin);
      for (const [name, value] of Object.entries(parameters)) {
        if (!new Set(["after", "limit"]).has(name) || !Number.isSafeInteger(value) || value < 0 ||
            (name === "limit" && (value === 0 || value > 4096))) {
          throw new Error("invalid bounded inspection query");
        }
        url.searchParams.set(name, String(value));
      }
      if (url.origin !== location.origin || url.username || url.password || url.hash) {
        throw new Error("inspection client constructed a non-canonical URL");
      }
      const controller = new AbortController();
      reads.add(controller);
      try {
        const response = await fetch(url, {
          method: "GET",
          cache: "no-store",
          credentials: "same-origin",
          redirect: "error",
          referrerPolicy: "no-referrer",
          headers: { Accept: "application/json", "OpenRealtime-Management-Token": current.token },
          signal: controller.signal,
        });
        if (!response.ok) {
          try { await response.body?.cancel(); } catch {}
          throw new Error(`inspection ${resource} returned ${response.status}`);
        }
        const mediaType = (response.headers.get("Content-Type") ?? "").split(";", 1)[0].trim().toLowerCase();
        if (mediaType !== "application/json") {
          try { await response.body?.cancel(); } catch {}
          throw new Error("inspection response is not JSON");
        }
        const bytes = await readBounded(response);
        if (access !== current) throw new Error("session inspection capability changed during request");
        const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
        return codec.parse(text);
      } finally {
        reads.delete(controller);
      }
    };

    context.publish("presentation.client.inspection", Object.freeze({
      available: () => Boolean(access),
      access: () => access ? {
        session_id: access.session_id,
        expires_at_ms: access.expires_at_ms,
      } : null,
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("inspection listener must be a function");
        listeners.add(listener);
        try { listener(projectedAccess()); } catch {}
        return () => listeners.delete(listener);
      },
      live: () => read("live"),
      deltas: (after = 0, limit = 256) => read("deltas", { after, limit }),
      trace: () => read("trace"),
    }));
    context.lifecycle.defer("inspection-access", () => {
      stopAccess();
      for (const controller of reads) controller.abort();
      reads.clear();
      access = null;
      listeners.clear();
    });
  },
};
