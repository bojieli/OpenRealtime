const MAX_CAPABILITY_BYTES = 512;
const encoder = new TextEncoder();
const OPERATIONS = new Set([
  "graph.read", "descriptor.read", "schema.read",
  "authoring.analyze", "authoring.compile", "authoring.render",
  "authoring.source.read", "authoring.source.create", "authoring.source.update",
]);

function canonicalCapability(value) {
  if (typeof value !== "string" || value.length === 0 || value.trim() !== value ||
      /[\0\r\n]/.test(value) || encoder.encode(value).byteLength > MAX_CAPABILITY_BYTES) {
    throw new Error("operator capability is not canonical");
  }
  return value;
}

export default {
  name: "openrealtime.presentation.client.management-operator-capability",
  revision: 1,
  async mount(context) {
    if (!context.permissions.allows("credential.use", "management-operator", "header")) {
      throw new Error("operator capability provider lacks its deployment grant");
    }
    let disposed = false;
    let generation = 0;
    let current = null;
    let authority = new AbortController();
    let expiryTimer = 0;
    const listeners = new Set();

    const status = () => Object.freeze({
      available: Boolean(current) && (current.expires_at_ms === 0 || Date.now() < current.expires_at_ms),
      expires_at_ms: current?.expires_at_ms ?? 0,
      generation,
    });
    const notify = () => {
      const projection = status();
      for (const listener of listeners) {
        try { listener(projection); } catch {}
      }
    };
    const clearTimer = () => {
      if (expiryTimer) clearTimeout(expiryTimer);
      expiryTimer = 0;
    };
    const expire = (expected) => {
      if (disposed || generation !== expected || !current || current.expires_at_ms === 0) return;
      const remaining = current.expires_at_ms - Date.now();
      if (remaining > 0) {
        expiryTimer = setTimeout(() => expire(expected), Math.min(remaining, 2_147_483_647));
        return;
      }
      current = null;
      authority.abort(new DOMException("operator capability expired", "AbortError"));
      authority = new AbortController();
      notify();
    };
    const rotate = (next) => {
      if (disposed) throw new Error("operator capability provider is disposed");
      clearTimer();
      authority.abort(new DOMException("operator capability changed", "AbortError"));
      authority = new AbortController();
      generation++;
      current = next;
      if (current?.expires_at_ms) expire(generation);
      notify();
      return status();
    };

    const access = Object.freeze({
      lease(operation, resource) {
        if (disposed || !OPERATIONS.has(operation) || typeof resource !== "string" ||
            resource.length === 0 || resource.length > 1024 || resource.trim() !== resource ||
            /[\0\r\n]/.test(resource)) {
          throw new Error("operator capability request is invalid");
        }
        if (current?.expires_at_ms && Date.now() >= current.expires_at_ms) expire(generation);
        if (!current) throw new Error("operator capability is unavailable");
        const leasedGeneration = generation;
        const leasedAuthority = authority;
        return Object.freeze({
          token: current.token,
          generation: leasedGeneration,
          signal: leasedAuthority.signal,
          current: () => !disposed && current !== null && generation === leasedGeneration &&
            authority === leasedAuthority && !leasedAuthority.signal.aborted,
        });
      },
    });
    const control = Object.freeze({
      status,
      replace(token, expiresAtMS = 0) {
        canonicalCapability(token);
        if (!Number.isSafeInteger(expiresAtMS) || expiresAtMS < 0 ||
            (expiresAtMS !== 0 && expiresAtMS <= Date.now())) {
          throw new Error("operator capability expiry is invalid");
        }
        return rotate(Object.freeze({ token, expires_at_ms: expiresAtMS }));
      },
      clear: () => rotate(null),
      subscribe(listener) {
        if (disposed || typeof listener !== "function") {
          throw new Error("operator capability listener is invalid");
        }
        listeners.add(listener);
        try { listener(status()); } catch {}
        return () => listeners.delete(listener);
      },
    });
    context.publish("presentation.client.management_operator_access", access);
    context.publish("presentation.client.management_operator_control", control);
    context.lifecycle.defer("management-operator-capability", () => {
      if (disposed) return;
      disposed = true;
      clearTimer();
      current = null;
      generation++;
      authority.abort(new DOMException("operator capability provider disposed", "AbortError"));
      listeners.clear();
    });
  },
};
