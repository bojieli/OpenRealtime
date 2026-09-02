const MAX_CAPABILITY_BYTES = 512;
const encoder = new TextEncoder();
const STATE_SCHEMA_NAME = "presentation.client.management_operator.state";
const STATE_SCHEMA_REVISION = 1;
const STATE_SCHEMA_DIGEST =
  "sha256:1f2f9868c1ea32703909c321437e2ac44980d6cb06968ab753a72627514934a1";
const OPERATIONS = new Set([
  "graph.read", "descriptor.read", "schema.read",
  "authoring.analyze", "authoring.rename", "authoring.edge.remove", "authoring.edge.create",
  "authoring.compile", "authoring.render",
  "authoring.source.read", "authoring.source.create", "authoring.source.update",
  "reconciliation.apply",
]);

function canonicalCapability(value) {
  if (typeof value !== "string" || value.length === 0 || value.trim() !== value ||
      /[\0\r\n]/.test(value) || encoder.encode(value).byteLength > MAX_CAPABILITY_BYTES) {
    throw new Error("operator capability is not canonical");
  }
  return value;
}

function operatorState(value, label = "operator capability state") {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      Object.keys(value).length !== 2 ||
      !Object.hasOwn(value, "capability") || !Object.hasOwn(value, "generation") ||
      !Number.isSafeInteger(value.generation) || value.generation < 0) {
    throw new Error(`${label} is invalid`);
  }
  let capability = null;
  if (value.capability !== null) {
    const candidate = value.capability;
    if (!candidate || typeof candidate !== "object" || Array.isArray(candidate) ||
        Object.keys(candidate).length !== 2 ||
        !Object.hasOwn(candidate, "token") || !Object.hasOwn(candidate, "expires_at_ms") ||
        !Number.isSafeInteger(candidate.expires_at_ms) || candidate.expires_at_ms < 0) {
      throw new Error(`${label} capability is invalid`);
    }
    capability = Object.freeze({
      token: canonicalCapability(candidate.token), expires_at_ms: candidate.expires_at_ms,
    });
  }
  return Object.freeze({ capability, generation: value.generation });
}

export default {
  name: "openrealtime.presentation.client.management-operator-capability",
  revision: 1,
  async migrateState(input) {
    if (!input || typeof input !== "object" || Array.isArray(input) ||
        Object.keys(input).length !== 4 ||
        !["entry", "schema", "source_implementation", "snapshot"].every(
          (field) => Object.hasOwn(input, field)) ||
        input.entry !== "management-operator" ||
        typeof input.source_implementation !== "string" || !input.source_implementation ||
        input.source_implementation.trim() !== input.source_implementation ||
        input.schema?.name !== STATE_SCHEMA_NAME || input.schema?.revision !== STATE_SCHEMA_REVISION ||
        input.schema?.digest !== STATE_SCHEMA_DIGEST) {
      throw new Error("operator capability migration input is invalid");
    }
    return operatorState(input.snapshot, "operator capability migration snapshot");
  },
  async mount(context) {
    if (!context.permissions.allows("credential.use", "management-operator", "header")) {
      throw new Error("operator capability provider lacks its deployment grant");
    }
    const restored = context.state.restored();
    const predecessor = restored === undefined
      ? Object.freeze({ capability: null, generation: 0 })
      : operatorState(restored, "restored operator capability state");
    let disposed = false;
    let generation = predecessor.generation;
    let current = predecessor.capability;
    if (current?.expires_at_ms && Date.now() >= current.expires_at_ms) current = null;
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
      if (generation === Number.MAX_SAFE_INTEGER) {
        throw new Error("operator capability generation is exhausted");
      }
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
    if (current?.expires_at_ms) expire(generation);
    context.state.snapshot(() => {
      if (current?.expires_at_ms && Date.now() >= current.expires_at_ms) expire(generation);
      return {
        capability: current ? { token: current.token, expires_at_ms: current.expires_at_ms } : null,
        generation,
      };
    });
    context.publish("presentation.client.management_operator_access", access);
    context.publish("presentation.client.management_operator_control", control);
    context.lifecycle.defer("management-operator-capability", () => {
      if (disposed) return;
      disposed = true;
      clearTimer();
      current = null;
      if (generation < Number.MAX_SAFE_INTEGER) generation++;
      authority.abort(new DOMException("operator capability provider disposed", "AbortError"));
      listeners.clear();
    });
  },
};
