const MAX_CONTRIBUTIONS = 64;
const MAX_LIST_ITEMS = 128;
const MAX_CONFIGURATION_BYTES = 64 << 10;

const encoder = new TextEncoder();
const sizeOf = (value) => encoder.encode(JSON.stringify(value)).length;

function strings(values, label) {
  if (values == null) return [];
  if (!Array.isArray(values) || values.length > MAX_LIST_ITEMS) {
    throw new Error(`${label} is outside its item limit`);
  }
  const result = [];
  for (const value of values) {
    if (typeof value !== "string" || !value || encoder.encode(value).length > 1024) {
      throw new Error(`${label} contains an invalid value`);
    }
    result.push(value);
  }
  return [...new Set(result)].sort();
}

function validate(fragment) {
  if (!fragment || typeof fragment !== "object" || Array.isArray(fragment)) {
    throw new Error("session configuration contribution must be an object");
  }
  const allowed = new Set(["supports", "observers", "tools", "debug"]);
  for (const key of Object.keys(fragment)) {
    if (!allowed.has(key)) throw new Error(`unknown session configuration field ${key}`);
  }
  const tools = fragment.tools ?? [];
  if (!Array.isArray(tools) || tools.length > MAX_LIST_ITEMS) {
    throw new Error("session configuration tools are outside their item limit");
  }
  const debug = fragment.debug ?? null;
  if (debug !== null && (!debug || typeof debug !== "object" || Array.isArray(debug) ||
      Object.keys(debug).some((key) => !["enabled", "categories"].includes(key)) ||
      (debug.enabled !== undefined && typeof debug.enabled !== "boolean"))) {
    throw new Error("session configuration debug contribution is invalid");
  }
  const normalized = {
    supports: strings(fragment.supports, "supports"),
    observers: strings(fragment.observers, "observers"),
    tools: structuredClone(tools),
    debug: debug === null ? null : {
      enabled: debug.enabled === true,
      categories: strings(debug.categories, "debug categories"),
    },
  };
  for (const tool of normalized.tools) {
    if (!tool || typeof tool !== "object" || Array.isArray(tool) || tool.type !== "function" ||
        typeof tool.name !== "string" || !tool.name || encoder.encode(tool.name).length > 1024) {
      throw new Error("session configuration contains an invalid function tool");
    }
  }
  if (sizeOf(normalized) > MAX_CONFIGURATION_BYTES) {
    throw new Error("session configuration contribution exceeds its byte limit");
  }
  return Object.freeze(normalized);
}

export default {
  name: "openrealtime.presentation.client.session-configuration",
  revision: 1,
  async mount(context) {
    const state = context.services.get("presentation.client.session_state");
    if (!state) throw new Error("session state is unavailable");
    const contributions = new Map();
    const listeners = new Set();
    let serial = 0;
    let scheduled = false;
    let reconcileTimer;
    let disposed = false;
    let lastSession = "";
    let lastPayload = "";
    let diagnostic = "";

    const merge = () => {
      const supports = new Set();
      const observers = new Set();
      const categories = new Set();
      const tools = new Map();
      let debugEnabled = false;
      for (const fragment of contributions.values()) {
        fragment.supports.forEach((value) => supports.add(value));
        fragment.observers.forEach((value) => observers.add(value));
        if (fragment.debug) {
          debugEnabled ||= fragment.debug.enabled;
          fragment.debug.categories.forEach((value) => categories.add(value));
        }
        for (const tool of fragment.tools) {
          const encoded = JSON.stringify(tool);
          const prior = tools.get(tool.name);
          if (prior && prior.encoded !== encoded) {
            throw new Error(`conflicting declarations for tool ${tool.name}`);
          }
          tools.set(tool.name, { encoded, value: structuredClone(tool) });
        }
      }
      const openrealtime = {
        version: 1,
        supports: [...supports].sort(),
        observers: [...observers].sort(),
      };
      if (debugEnabled || categories.size) {
        openrealtime.debug = { enabled: debugEnabled, categories: [...categories].sort() };
      }
      return {
        type: "realtime",
        openrealtime,
        tools: [...tools.values()].sort((left, right) =>
          left.value.name.localeCompare(right.value.name)).map((entry) => entry.value),
      };
    };
    const snapshot = () => Object.freeze({
      contributions: contributions.size,
      session_id: lastSession,
      diagnostic,
      configuration: merge(),
    });
    const publish = () => {
      const current = snapshot();
      for (const listener of listeners) {
        try { listener(current); } catch {}
      }
    };
    const reconcile = () => {
      scheduled = false;
      reconcileTimer = undefined;
      if (disposed) return;
      try {
        const current = state.snapshot();
        if (current.connection.phase !== "connected" || !current.session.id) {
          lastSession = "";
          lastPayload = "";
          return;
        }
        const configuration = merge();
        const encoded = JSON.stringify(configuration);
        if (encoder.encode(encoded).length > MAX_CONFIGURATION_BYTES) {
          throw new Error("merged session configuration exceeds its byte limit");
        }
        if (lastSession === current.session.id && lastPayload === encoded) return;
        state.updateSession(configuration);
        lastSession = current.session.id;
        lastPayload = encoded;
        diagnostic = "";
      } catch (error) {
        diagnostic = error?.message ?? String(error);
      }
      publish();
    };
    const schedule = () => {
      if (disposed || scheduled) return;
      scheduled = true;
      // Mounts are awaited by the host one at a time. A microtask would run
      // between those mounts and could send an incomplete (usually empty)
      // session.update before dependent plugins contribute their fragments.
      // Coalescing at the next task boundary makes the initial configuration
      // atomic while retaining prompt reconciliation after provider changes.
      reconcileTimer = setTimeout(reconcile, 0);
    };
    const unsubscribeState = state.subscribe(schedule);
    const service = Object.freeze({
      contribute(fragment) {
        if (disposed) throw new Error("session configuration service is disposed");
        if (contributions.size >= MAX_CONTRIBUTIONS) {
          throw new Error("session configuration contribution limit reached");
        }
        const key = ++serial;
        contributions.set(key, validate(fragment));
        try {
          const candidate = merge();
          if (sizeOf(candidate) > MAX_CONFIGURATION_BYTES) {
            throw new Error("merged session configuration exceeds its byte limit");
          }
        } catch (error) {
          contributions.delete(key);
          throw error;
        }
        schedule();
        let active = true;
        return () => {
          if (!active) return;
          active = false;
          contributions.delete(key);
          schedule();
        };
      },
      snapshot,
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("configuration listener must be a function");
        listeners.add(listener);
        try { listener(snapshot()); } catch {}
        return () => listeners.delete(listener);
      },
    });
    context.publish("presentation.client.session_configuration", service);
    context.lifecycle.defer("session-configuration", () => {
      disposed = true;
      if (reconcileTimer !== undefined) clearTimeout(reconcileTimer);
      reconcileTimer = undefined;
      scheduled = false;
      unsubscribeState();
      contributions.clear();
      listeners.clear();
    });
  },
};
