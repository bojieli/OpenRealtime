const MAX_TOOLS = 128;
const MAX_NAME_BYTES = 128;
const MAX_DESCRIPTION_BYTES = 4096;
const MAX_PARAMETERS_BYTES = 64 << 10;
const MAX_AUTHORITY_BYTES = 8192;
const MAX_PROTOCOL_ERROR_BYTES = 4096;
const MAX_WIRE_BYTES = 48 << 20;
const RECONNECT_DELAYS_MS = Object.freeze([100, 250, 500, 1000, 2000]);

const encoder = new TextEncoder();
const bytes = (value) => encoder.encode(value).length;
const own = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
const isObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
const idPattern = /^[A-Za-z0-9_.:-]{1,128}$/;
const toolPattern = /^[A-Za-z][A-Za-z0-9_.-]{0,127}$/;
const digestPattern = /^sha256:[0-9a-f]{64}$/;
const scopePattern = /^[0-9a-f]{32}$/;
const noncePattern = /^[0-9a-f]{32}$/;
const EFFECTS_PROTOCOL = "openrealtime.client-effects.v1";

function onlyKeys(value, allowed, label) {
  if (!isObject(value)) throw new Error(`${label} must be an object`);
  for (const key of Object.keys(value)) {
    if (!allowed.includes(key)) throw new Error(`${label} contains unknown field ${key}`);
  }
  return value;
}

function boundedString(value, label, limit, allowEmpty = false) {
  if (typeof value !== "string" || (!allowEmpty && !value) || bytes(value) > limit) {
    throw new Error(`${label} is not a bounded string`);
  }
  return value;
}

function positiveInteger(value, label, maximum) {
  if (!Number.isSafeInteger(value) || value < 1 || value > maximum) {
    throw new Error(`${label} is outside its declared bound`);
  }
  return value;
}

function validateLimits(value) {
  onlyKeys(value, [
    "max_message_bytes", "max_result_bytes", "max_in_flight", "max_calls",
    "confirmation_timeout_ms", "execution_timeout_ms",
  ], "effect limits");
  const result = Object.freeze({
    max_message_bytes: positiveInteger(value.max_message_bytes, "max_message_bytes", MAX_WIRE_BYTES),
    max_result_bytes: positiveInteger(value.max_result_bytes, "max_result_bytes", 1 << 20),
    max_in_flight: positiveInteger(value.max_in_flight, "max_in_flight", 256),
    max_calls: positiveInteger(value.max_calls, "max_calls", 16384),
    confirmation_timeout_ms: positiveInteger(value.confirmation_timeout_ms, "confirmation_timeout_ms", 300000),
    execution_timeout_ms: positiveInteger(value.execution_timeout_ms, "execution_timeout_ms", 300000),
  });
  if (result.max_result_bytes >= result.max_message_bytes ||
      result.max_calls < result.max_in_flight ||
      result.confirmation_timeout_ms < 100 || result.execution_timeout_ms < 100) {
    throw new Error("effect limits are internally inconsistent");
  }
  return result;
}

function validateDeclaration(value, codec) {
  onlyKeys(value, [
    "name", "description", "parameters", "host_confirmation", "session_confirmation",
    "target", "mutating", "channel", "digest",
  ], "effect declaration");
  const name = boundedString(value.name, "effect name", MAX_NAME_BYTES);
  if (!toolPattern.test(name)) throw new Error("effect name is not canonical");
  const description = boundedString(value.description, "effect description", MAX_DESCRIPTION_BYTES);
  if (description.trim() !== description) throw new Error("effect description is not canonical");
  if (!isObject(value.parameters)) throw new Error("effect parameters must be an object schema");
  const parameters = codec.parse(codec.stable(value.parameters));
  if (parameters.type !== "object" || parameters.additionalProperties !== false ||
      bytes(codec.stable(parameters)) > MAX_PARAMETERS_BYTES) {
    throw new Error("effect parameters must be a bounded closed object schema");
  }
  if (!["never", "policy", "always"].includes(value.host_confirmation) ||
      value.session_confirmation !== "never") {
    throw new Error("effect confirmation declaration is invalid");
  }
  const target = value.target === undefined ? "" : boundedString(value.target, "effect target", 256);
  if (target && (/\s/.test(target) || target.trim() !== target)) {
    throw new Error("effect target is not canonical");
  }
  if (typeof value.mutating !== "boolean" ||
      !["tool", "computer", "artifact", "download"].includes(value.channel) ||
      typeof value.digest !== "string" || !digestPattern.test(value.digest)) {
    throw new Error("effect declaration metadata is invalid");
  }
  return Object.freeze({
    name, description, parameters, host_confirmation: value.host_confirmation,
    session_confirmation: "never", target, mutating: value.mutating,
    channel: value.channel, digest: value.digest,
  });
}

function validateReady(value, codec, expectedCatalogDigest) {
  onlyKeys(value, ["type", "version", "scope_id", "catalog_digest", "tools", "limits"], "effect ready message");
  if (value.type !== "ready" || value.version !== 1 ||
      value.catalog_digest !== expectedCatalogDigest ||
      typeof value.scope_id !== "string" || !scopePattern.test(value.scope_id) ||
      !Array.isArray(value.tools) || value.tools.length > MAX_TOOLS) {
    throw new Error("effect ready message is invalid");
  }
  const names = new Set();
  const tools = value.tools.map((tool) => {
    const declaration = validateDeclaration(tool, codec);
    if (names.has(declaration.name)) throw new Error(`effect ready message repeats ${declaration.name}`);
    names.add(declaration.name);
    return declaration;
  });
  return Object.freeze({
    version: 1, scope_id: value.scope_id, catalog_digest: value.catalog_digest,
    tools: Object.freeze(tools), limits: validateLimits(value.limits),
  });
}

function sessionTool(declaration) {
  const extension = {
    confirm: declaration.session_confirmation,
    client_effect: { version: 1, declaration_digest: declaration.digest },
  };
  if (declaration.target) extension.target = declaration.target;
  return {
    type: "function", name: declaration.name, description: declaration.description,
    parameters: structuredClone(declaration.parameters), openrealtime: extension,
  };
}

function effectEndpoint(manifest) {
  const matches = (manifest.endpoints ?? []).filter((endpoint) => endpoint.name === "effects.local");
  if (matches.length !== 1 || matches[0].method !== "GET" || matches[0].path !== "/client/v1/effects" ||
      matches[0].protocol !== EFFECTS_PROTOCOL || !digestPattern.test(matches[0].catalog_digest ?? "")) {
    throw new Error("the locked client manifest has no exact local effect endpoint");
  }
  const endpoint = new URL(matches[0].path, globalThis.location?.href ?? "http://127.0.0.1/");
  endpoint.protocol = endpoint.protocol === "https:" ? "wss:" : "ws:";
  return Object.freeze({ href: endpoint.href, catalog_digest: matches[0].catalog_digest });
}

function cloneSnapshot(value) {
  return Object.freeze(structuredClone(value));
}

export default {
  name: "openrealtime.presentation.client.effects",
  revision: 1,
  async mount(context) {
    const state = context.services.get("presentation.client.session_state");
    const events = context.services.get("presentation.client.protocol_events");
    const codec = context.services.get("presentation.client.strict_json");
    const configuration = context.services.get("presentation.client.session_configuration");
    if (!state || !events || !codec || !configuration) {
      throw new Error("effect client dependencies are unavailable");
    }
    if (!context.permissions.allows("network.connect", "host-effects", "websocket")) {
      throw new Error("effect client lacks its host-effects WebSocket grant");
    }
    const endpoint = effectEndpoint(context.manifest);
    const listeners = new Set();
    const pending = new Map();
    const confirmations = new Map();
    let socket;
    let generation = 0;
    let reconnectTimer;
    let reconnectAttempt = 0;
    let activeSessionID = "";
    let ready;
    let negotiated = false;
    let contributionDispose;
    let contributionIdentity = "";
    let phase = "idle";
    let diagnostic = "";
    let disposed = false;
    let calls = 0;
    let results = 0;
    let refused = 0;
    let reconnects = 0;

    const snapshot = () => cloneSnapshot({
      phase, negotiated, session_id: activeSessionID, scope_id: ready?.scope_id ?? "",
      catalog_digest: ready?.catalog_digest ?? endpoint.catalog_digest,
      declarations: ready?.tools ?? [], limits: ready?.limits ?? null,
      pending: [...pending.values()], confirmations: [...confirmations.values()],
      diagnostic, stats: { calls, results, refused, reconnects },
    });
    const publish = (event) => {
      const current = snapshot();
      for (const listener of listeners) {
        try { listener(current, event ? structuredClone(event) : undefined); } catch {}
      }
    };
    const report = (error) => {
      const message = error?.message ?? String(error);
      diagnostic = bytes(message) <= MAX_PROTOCOL_ERROR_BYTES
        ? message : "effect client diagnostic exceeds its bound";
      publish({ type: "diagnostic", message: diagnostic });
    };
    const clearContribution = () => {
      contributionDispose?.();
      contributionDispose = undefined;
      contributionIdentity = "";
    };
    const enabled = () => state.snapshot().session.openrealtime.enabled.includes("client.effects");
    const syncContribution = () => {
      if (disposed || !ready) {
        clearContribution();
        return;
      }
      const fragment = {
        supports: ["client.effects"],
        tools: enabled() ? ready.tools.map(sessionTool) : [],
      };
      const identity = codec.stable(fragment);
      if (identity === contributionIdentity) return;
      clearContribution();
      contributionDispose = configuration.contribute(fragment);
      contributionIdentity = identity;
    };
    const failPending = (reason) => {
      for (const call of pending.values()) {
        try { state.toolResult(call.id, "failed", "", reason); } catch {}
      }
      pending.clear();
      confirmations.clear();
      publish({ type: "provider_lost", reason });
    };
    const closeSocket = (reason, preserveContribution = false) => {
      clearTimeout(reconnectTimer);
      reconnectTimer = undefined;
      generation++;
      const current = socket;
      socket = undefined;
      if (current && current.readyState < WebSocket.CLOSING) current.close(1000, reason.slice(0, 120));
      ready = undefined;
      negotiated = false;
      phase = disposed ? "closed" : "idle";
      if (!preserveContribution) clearContribution();
      failPending(reason);
    };
    const send = (message) => {
      if (!socket || socket.readyState !== WebSocket.OPEN || !ready) {
        throw new Error("effect provider is not ready");
      }
      const encoded = codec.stable(message);
      if (bytes(encoded) > ready.limits.max_message_bytes) {
        throw new Error("effect request exceeds the provider message bound");
      }
      socket.send(encoded);
    };
    const complete = (message) => {
      const call = pending.get(message.id);
      if (!call) throw new Error(`effect result names unknown call ${message.id ?? ""}`);
      if (message.error === undefined && message.channel !== call.channel) {
        throw new Error("effect result channel does not match the admitted declaration");
      }
      if ((message.artifact !== undefined && call.channel !== "artifact") ||
          (message.download !== undefined && call.channel !== "download")) {
        throw new Error("effect result resource does not match the admitted channel");
      }
      pending.delete(message.id);
      confirmations.delete(message.id);
      results++;
      const output = message.output === undefined ? "" : boundedString(
        message.output, "effect output", ready.limits.max_result_bytes, true,
      );
      if (message.error !== undefined) {
        onlyKeys(message.error, ["code", "message"], "effect result error");
        const code = boundedString(message.error.code, "effect error code", 128);
        const detail = boundedString(message.error.message, "effect error message", MAX_PROTOCOL_ERROR_BYTES);
        refused++;
        state.toolResult(message.id, code === "confirmation_declined" ? "declined" : "failed", "", detail);
      } else {
        state.toolResult(message.id, "done", output, "");
      }
      publish(message);
    };
    const receive = (source, expectedGeneration) => {
      if (disposed || expectedGeneration !== generation) return;
      if (typeof source !== "string" || bytes(source) > MAX_WIRE_BYTES) {
        throw new Error("effect provider sent an invalid or oversized text message");
      }
      const message = codec.parse(source);
      if (!isObject(message) || typeof message.type !== "string") {
        throw new Error("effect provider message requires a type");
      }
      if (message.type === "ready") {
        if (ready) throw new Error("effect provider sent ready more than once");
        ready = validateReady(message, codec, endpoint.catalog_digest);
        negotiated = false;
        phase = "ready";
        reconnectAttempt = 0;
        diagnostic = "";
        syncContribution();
        publish({ type: "ready" });
        return;
      }
      if (!ready || bytes(source) > ready.limits.max_message_bytes) {
        throw new Error("effect provider sent a message before ready or above its declared bound");
      }
      switch (message.type) {
      case "confirm": {
        onlyKeys(message, ["type", "id", "name", "arguments", "nonce", "confirm", "target", "channel"], "effect confirmation");
        const call = pending.get(message.id);
        const declaration = ready.tools.find((tool) => tool.name === message.name);
        if (!call || !declaration || message.id !== call.id || message.name !== call.name ||
            typeof message.nonce !== "string" || !noncePattern.test(message.nonce) ||
            message.confirm !== declaration.host_confirmation ||
            (message.target ?? "") !== declaration.target || message.channel !== declaration.channel ||
            codec.stable(message.arguments) !== call.arguments) {
          throw new Error("effect confirmation does not bind the pending declaration and arguments");
        }
        const confirmation = Object.freeze({
          id: call.id, name: call.name, arguments: structuredClone(message.arguments),
          nonce: message.nonce, confirm: message.confirm, target: declaration.target,
          channel: declaration.channel, answered: false,
        });
        confirmations.set(call.id, confirmation);
        publish(message);
        return;
      }
      case "result":
        onlyKeys(message, ["type", "id", "channel", "output", "error", "artifact", "download"], "effect result");
        if (typeof message.id !== "string" || !idPattern.test(message.id) ||
            (message.artifact !== undefined && message.download !== undefined)) {
          throw new Error("effect result is invalid");
        }
        complete(message);
        return;
      case "error":
        onlyKeys(message, ["type", "id", "error"], "effect protocol error");
        if (message.id && pending.has(message.id)) {
          complete({ type: "result", id: message.id, error: message.error });
        } else {
          onlyKeys(message.error, ["code", "message"], "effect protocol error detail");
          throw new Error(boundedString(message.error.message, "effect protocol error", MAX_PROTOCOL_ERROR_BYTES));
        }
        return;
      default:
        throw new Error(`effect provider sent unknown message ${message.type}`);
      }
    };
    const scheduleReconnect = () => {
      if (disposed || !activeSessionID || reconnectTimer !== undefined) return;
      const delay = RECONNECT_DELAYS_MS[Math.min(reconnectAttempt, RECONNECT_DELAYS_MS.length - 1)];
      reconnectAttempt++;
      reconnectTimer = setTimeout(() => {
        reconnectTimer = undefined;
        reconnects++;
        openSocket();
      }, delay);
    };
    const openSocket = () => {
      if (disposed || !activeSessionID || socket) return;
      phase = reconnectAttempt ? "reconnecting" : "connecting";
      const expectedGeneration = ++generation;
      const candidate = new WebSocket(endpoint.href);
      socket = candidate;
      candidate.addEventListener("open", () => {
        if (expectedGeneration !== generation) return;
        phase = "handshaking";
        publish({ type: "socket_open" });
      });
      candidate.addEventListener("message", (event) => {
        if (disposed || expectedGeneration !== generation) return;
        try { receive(event.data, expectedGeneration); } catch (error) {
          report(error);
          if (candidate.readyState < WebSocket.CLOSING) candidate.close(1002, "invalid effect protocol");
        }
      });
      candidate.addEventListener("error", () => report(new Error("effect provider connection failed")));
      candidate.addEventListener("close", () => {
        if (expectedGeneration !== generation) return;
        socket = undefined;
        ready = undefined;
        negotiated = false;
        phase = "idle";
        clearContribution();
        failPending("effect provider disconnected");
        scheduleReconnect();
      });
      publish({ type: "connecting" });
    };
    const settleLocally = (event, reason) => {
      refused++;
      try { state.toolResult(event.call_id, "failed", "", reason); } catch (error) { report(error); }
    };
    const onProtocolEvent = (event) => {
      if (event?.type === "session.updated") {
        const enabled = event.session?.openrealtime?.enabled;
        const tools = event.session?.tools;
        const accepted = Boolean(ready && Array.isArray(enabled) && enabled.includes("client.effects") &&
          Array.isArray(tools) && ready.tools.every((declaration) => tools.some((tool) =>
            tool?.name === declaration.name && tool?.openrealtime?.client_effect?.version === 1 &&
            tool.openrealtime.client_effect.declaration_digest === declaration.digest)));
        if (accepted !== negotiated) {
          negotiated = accepted;
          publish({ type: accepted ? "negotiated" : "negotiation_lost" });
        }
        return;
      }
      if (event?.type !== "response.function_call_arguments.done" || !ready) return;
      const declaration = ready.tools.find((tool) => tool.name === event.name);
      if (!declaration) return;
      try {
        if (!enabled()) throw new Error("client effects were not negotiated by the realtime server");
        if (!idPattern.test(event.call_id ?? "")) throw new Error("effect call ID is not canonical");
        const extension = event.openrealtime?.client_effect;
        onlyKeys(extension, ["version", "declaration_digest", "authority"], "client effect authority");
        if (extension.version !== 1 || extension.declaration_digest !== declaration.digest ||
            typeof extension.authority !== "string" || !extension.authority ||
            bytes(extension.authority) > MAX_AUTHORITY_BYTES || /\s/.test(extension.authority)) {
          throw new Error("effect call lacks exact bounded server authority");
        }
        const argumentsValue = codec.parse(boundedString(
          event.arguments, "effect arguments", ready.limits.max_message_bytes,
        ));
        if (!isObject(argumentsValue)) throw new Error("effect arguments must be one JSON object");
        const canonicalArguments = codec.stable(argumentsValue);
        if (pending.size >= ready.limits.max_in_flight || calls >= ready.limits.max_calls) {
          throw new Error("effect client reached its declared call bound");
        }
        const call = Object.freeze({
          id: event.call_id, name: declaration.name, arguments: canonicalArguments,
          declaration_digest: declaration.digest, channel: declaration.channel,
        });
        pending.set(call.id, call);
        calls++;
        send({
          type: "call", session_id: activeSessionID, id: call.id, name: call.name,
          arguments: argumentsValue, authority: extension.authority,
        });
        publish({ type: "call", id: call.id, name: call.name });
      } catch (error) {
        pending.delete(event.call_id);
        settleLocally(event, error?.message ?? "effect call was refused");
      }
    };
    const offEvents = events.subscribe(onProtocolEvent);
    const offState = state.subscribe((current) => {
      const nextSession = current.connection.phase === "connected" ? current.session.id : "";
      if (nextSession !== activeSessionID) {
        closeSocket(nextSession ? "realtime session replaced" : "realtime session unavailable");
        activeSessionID = nextSession;
        if (activeSessionID) openSocket();
      } else {
        try { syncContribution(); } catch (error) { report(error); }
      }
    });
    const service = Object.freeze({
      snapshot,
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("effect listener must be a function");
        listeners.add(listener);
        try { listener(snapshot()); } catch {}
        return () => listeners.delete(listener);
      },
      decide(id, approved) {
        if (disposed) throw new Error("effect client is disposed");
        const confirmation = confirmations.get(id);
        if (!confirmation || confirmation.answered || typeof approved !== "boolean") {
          throw new Error("effect decision does not match one pending confirmation");
        }
        send({ type: "decide", id, nonce: confirmation.nonce, approved });
        confirmations.set(id, Object.freeze({ ...confirmation, answered: true }));
        publish({ type: "decision", id, approved });
      },
    });
    context.publish("presentation.client.tools_effects", service);
    context.lifecycle.defer("effects-client", () => {
      disposed = true;
      offState();
      offEvents();
      clearTimeout(reconnectTimer);
      reconnectTimer = undefined;
      closeSocket("effect client disposed");
      activeSessionID = "";
      listeners.clear();
    });
  },
};
