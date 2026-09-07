// This file is only the manifest-plugin adapter. bundle.go prepends the exact
// canonical module from client/reducer/javascript/reducer.mjs before hashing
// and publishing reducer.js; protocol state must not be reimplemented here.

const REDUCER_STATE_SCHEMA_NAME = "presentation.client.reducer.state";
const REDUCER_STATE_SCHEMA_REVISION = 1;
const REDUCER_STATE_SCHEMA_DIGEST =
  "sha256:640ca5e7a3fcb2638dd114be3affa7035514eadcb037c77c323b32abad906f26";
const INSPECTION_TOKEN = /^mgmt_[A-Za-z0-9_-]+$/;

function adapterObject(value, fields, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      Object.keys(value).length !== fields.length ||
      !fields.every((field) => Object.hasOwn(value, field))) {
    throw new Error(`${label} is not an exact object`);
  }
  return value;
}

function adapterString(value, label, allowEmpty = true) {
  if (typeof value !== "string" || (!allowEmpty && value.length === 0) ||
      byteLength(value) > DEFAULT_LIMITS.max_string_bytes) {
    throw new Error(`${label} is not a bounded string`);
  }
  return value;
}

function adapterInteger(value, label, maximum = Number.MAX_SAFE_INTEGER) {
  if (!Number.isSafeInteger(value) || value < 0 || value > maximum) {
    throw new Error(`${label} is not a bounded integer`);
  }
  return value;
}

function adapterStrings(value, label) {
  if (!Array.isArray(value) || value.length > DEFAULT_LIMITS.max_tool_calls) {
    throw new Error(`${label} is not a bounded string list`);
  }
  value.forEach((entry, index) => adapterString(entry, `${label}[${index}]`));
}

function checkedInspectionAccess(value, label = "session inspection access") {
  if (value === null) return null;
  adapterObject(value, ["session_id", "path", "token", "expires_at_ms"], label);
  const sessionID = adapterString(value.session_id, `${label}.session_id`, false);
  const path = adapterString(value.path, `${label}.path`, false);
  const token = adapterString(value.token, `${label}.token`, false);
  const expiresAtMS = adapterInteger(value.expires_at_ms, `${label}.expires_at_ms`);
  const canonicalPath = `/openrealtime/v1/sessions/${sessionID}/live`;
  if (!/^[A-Za-z0-9._:-]{1,256}$/.test(sessionID) || path !== canonicalPath ||
      !INSPECTION_TOKEN.test(token) || byteLength(token) > 512 || expiresAtMS === 0) {
    throw new Error(`${label} is malformed`);
  }
  return Object.freeze({ session_id: sessionID, path, token, expires_at_ms: expiresAtMS });
}

function checkedMachineState(value, label) {
  adapterObject(value, [
    "now_ms", "connection", "session", "response", "conversation", "playout", "tools",
    "last_truncation", "last_error", "protocol_log",
  ], label);
  validateSnapshot(value, DEFAULT_LIMITS, label);
  if (byteLength(JSON.stringify(value)) > MAX_CORPUS_BYTES) {
    throw new Error(`${label} exceeds its byte limit`);
  }
  adapterInteger(value.now_ms, `${label}.now_ms`, DEFAULT_LIMITS.max_virtual_time_ms);
  const connection = adapterObject(value.connection,
    ["phase", "transport", "reason", "attempt", "next_retry_ms"], `${label}.connection`);
  if (!new Set(["connected", "disconnected"]).has(connection.phase) ||
      (connection.phase === "connected" && !new Set(["websocket", "webrtc"]).has(connection.transport)) ||
      (connection.phase === "disconnected" && connection.transport !== "") ||
      adapterString(connection.reason, `${label}.connection.reason`) !== connection.reason ||
      adapterInteger(connection.attempt, `${label}.connection.attempt`,
        DEFAULT_LIMITS.max_reconnect_attempts) !== 0 ||
      adapterInteger(connection.next_retry_ms, `${label}.connection.next_retry_ms`,
        DEFAULT_LIMITS.max_virtual_time_ms) !== 0) {
    throw new Error(`${label} is not at a connection safe point`);
  }
  const session = adapterObject(value.session, ["id", "manual_turns", "openrealtime"], `${label}.session`);
  adapterString(session.id, `${label}.session.id`, connection.phase !== "connected");
  if (typeof session.manual_turns !== "boolean" ||
      (connection.phase === "disconnected" && session.id !== "")) {
    throw new Error(`${label}.session is inconsistent with its connection`);
  }
  const negotiation = adapterObject(session.openrealtime,
    ["present", "version", "enabled", "observers", "available_observers", "debug_enabled", "video"],
    `${label}.session.openrealtime`);
  if (typeof negotiation.present !== "boolean" || typeof negotiation.debug_enabled !== "boolean") {
    throw new Error(`${label}.session negotiation flags are invalid`);
  }
  adapterInteger(negotiation.version, `${label}.session.openrealtime.version`);
  adapterStrings(negotiation.enabled, `${label}.session.openrealtime.enabled`);
  adapterStrings(negotiation.observers, `${label}.session.openrealtime.observers`);
  adapterStrings(negotiation.available_observers, `${label}.session.openrealtime.available_observers`);
  const video = adapterObject(negotiation.video,
    ["format", "fps_cap", "max_dimension", "max_frame_bytes"], `${label}.session.openrealtime.video`);
  adapterString(video.format, `${label}.session.openrealtime.video.format`);
  for (const field of ["fps_cap", "max_dimension", "max_frame_bytes"]) {
    adapterInteger(video[field], `${label}.session.openrealtime.video.${field}`);
  }
  const response = adapterObject(value.response, ["id", "open", "status", "reason"], `${label}.response`);
  for (const field of ["id", "status", "reason"]) adapterString(response[field], `${label}.response.${field}`);
  if (response.open !== false) throw new Error(`${label} has an in-flight response`);
  value.conversation.forEach((item, index) => {
    const itemLabel = `${label}.conversation[${index}]`;
    adapterObject(item, ["item_id", "role", "channel", "text", "audio_deltas", "audio_done", "truncated_ms"], itemLabel);
    for (const field of ["item_id", "role", "channel", "text"]) adapterString(item[field], `${itemLabel}.${field}`);
    adapterInteger(item.audio_deltas, `${itemLabel}.audio_deltas`);
    adapterInteger(item.truncated_ms, `${itemLabel}.truncated_ms`);
    if (typeof item.audio_done !== "boolean") throw new Error(`${itemLabel}.audio_done is invalid`);
  });
  const playout = adapterObject(value.playout, ["speaking", "item_id", "played_ms"], `${label}.playout`);
  if (playout.speaking !== false) throw new Error(`${label} has active playout`);
  adapterString(playout.item_id, `${label}.playout.item_id`);
  adapterInteger(playout.played_ms, `${label}.playout.played_ms`);
  value.tools.forEach((tool, index) => {
    const toolLabel = `${label}.tools[${index}]`;
    adapterObject(tool, ["call_id", "name", "arguments", "status", "output", "error"], toolLabel);
    for (const field of ["call_id", "name", "arguments", "status", "output", "error"]) {
      adapterString(tool[field], `${toolLabel}.${field}`);
    }
    if (!new Set(["done", "failed", "declined"]).has(tool.status)) {
      throw new Error(`${label} has a pending or invalid tool call`);
    }
  });
  const truncation = adapterObject(value.last_truncation, ["item_id", "audio_end_ms"], `${label}.last_truncation`);
  adapterString(truncation.item_id, `${label}.last_truncation.item_id`);
  adapterInteger(truncation.audio_end_ms, `${label}.last_truncation.audio_end_ms`);
  adapterString(value.last_error, `${label}.last_error`);
  value.protocol_log.forEach((entry, index) => {
    const entryLabel = `${label}.protocol_log[${index}]`;
    adapterObject(entry, ["at_ms", "direction", "type"], entryLabel);
    adapterInteger(entry.at_ms, `${entryLabel}.at_ms`, DEFAULT_LIMITS.max_virtual_time_ms);
    if (!new Set(["in", "out"]).has(entry.direction)) throw new Error(`${entryLabel}.direction is invalid`);
    adapterString(entry.type, `${entryLabel}.type`, false);
  });
  return structuredClone(value);
}

function reducerAdapterState(value, label = "reducer adapter state") {
  adapterObject(value,
    ["machine", "outbound", "command_cursor", "local_item", "inspection_access"], label);
  const machine = checkedMachineState(value.machine, `${label}.machine`);
  if (!Array.isArray(value.outbound) || value.outbound.length > DEFAULT_LIMITS.max_outbound_events) {
    throw new Error(`${label}.outbound exceeds its item limit`);
  }
  const outbound = value.outbound.map((event, index) => {
    if (!event || typeof event !== "object" || Array.isArray(event) ||
        byteLength(JSON.stringify(event)) > DEFAULT_LIMITS.max_event_bytes) {
      throw new Error(`${label}.outbound[${index}] is invalid`);
    }
    return structuredClone(event);
  });
  const commandCursor = adapterInteger(value.command_cursor, `${label}.command_cursor`, outbound.length);
  if (commandCursor !== outbound.length) throw new Error(`${label} has unsent commands`);
  const localItem = adapterInteger(value.local_item, `${label}.local_item`);
  const inspectionAccess = checkedInspectionAccess(value.inspection_access, `${label}.inspection_access`);
  if ((machine.connection.phase === "disconnected" && inspectionAccess !== null) ||
      (inspectionAccess && inspectionAccess.session_id !== machine.session.id)) {
    throw new Error(`${label} inspection access is inconsistent with its session`);
  }
  return Object.freeze({
    machine, outbound, command_cursor: commandCursor, local_item: localItem,
    inspection_access: inspectionAccess,
  });
}

export default {
  name: "openrealtime.presentation.client.reducer",
  revision: 1,
  async migrateState(input) {
    if (!input || typeof input !== "object" || Array.isArray(input) ||
        Object.keys(input).length !== 4 ||
        !["entry", "schema", "source_implementation", "snapshot"].every(
          (field) => Object.hasOwn(input, field)) ||
        input.entry !== "reducer" ||
        typeof input.source_implementation !== "string" || !input.source_implementation ||
        input.source_implementation.trim() !== input.source_implementation ||
        input.schema?.name !== REDUCER_STATE_SCHEMA_NAME ||
        input.schema?.revision !== REDUCER_STATE_SCHEMA_REVISION ||
        input.schema?.digest !== REDUCER_STATE_SCHEMA_DIGEST) {
      throw new Error("reducer state migration input is invalid");
    }
    return reducerAdapterState(input.snapshot, "reducer migration snapshot");
  },
  async mount(context) {
    const connection = context.services.get("presentation.client.connection");
    if (!connection || typeof connection.state !== "function") {
      throw new Error("connection service is unavailable");
    }
    const restored = context.state.restored();
    const recovery = restored === undefined
      ? null : reducerAdapterState(restored, "restored reducer state");

    const machine = new ClientReducer(DEFAULT_LIMITS);
    const physicalConnected = connection.state() === "connected";
    if (recovery) {
      const machineState = structuredClone(recovery.machine);
      if (!physicalConnected && machineState.connection.phase === "connected") {
        machineState.connection = {
          phase: "disconnected", transport: "", reason: "", attempt: 0, next_retry_ms: 0,
        };
        machineState.session = emptySession();
        machineState.playout = { speaking: false, item_id: "", played_ms: 0 };
        machineState.last_error = "";
      }
      machine.state = machineState;
      machine.commands = structuredClone(recovery.outbound);
    }
    const listeners = new Set();
    const inspectionListeners = new Set();
    const protocolListeners = new Set();
    const startedAt = performance.now();
    let commandCursor = recovery?.command_cursor ?? 0;
    let localItem = recovery?.local_item ?? 0;
    let retryTimer;
    let disposed = false;
    let diagnostic = "";
    let inspectionAccess = physicalConnected && recovery?.inspection_access &&
      Date.now() < recovery.inspection_access.expires_at_ms
      ? recovery.inspection_access : null;

    const publishInspection = (access) => {
      inspectionAccess = access ? Object.freeze({ ...access }) : null;
      for (const listener of inspectionListeners) {
        try { listener(inspectionAccess ? { ...inspectionAccess } : null); } catch {}
      }
    };
    const inspectionProjection = (event) => {
      if (event?.type !== "session.updated") return undefined;
      const debug = event.session?.openrealtime?.debug;
      if (debug?.enabled === false) {
        return null;
      }
      const access = debug?.inspection;
      if (access == null) return undefined;
      return checkedInspectionAccess({
        session_id: String(access.session_id ?? ""),
        path: String(access.path ?? ""),
        token: String(access.token ?? ""),
        expires_at_ms: Number(access.expires_at_ms ?? 0),
      });
    };
    const publishProtocol = (event) => {
      // Each consumer receives its own copy. A view or effect adapter cannot
      // mutate the event seen by the reducer or by another plugin.
      for (const listener of protocolListeners) {
        try { listener(structuredClone(event)); } catch {}
      }
    };

    const now = () => Math.min(
      DEFAULT_LIMITS.max_virtual_time_ms,
      Math.max(machine.snapshot().now_ms, Math.floor(performance.now() - startedAt)),
    );
    const publish = () => {
      const snapshot = machine.snapshot();
      for (const listener of listeners) {
        try { listener(snapshot, diagnostic); } catch {}
      }
    };
    const flush = () => {
      const commands = machine.outbound();
      while (commandCursor < commands.length) connection.send(commands[commandCursor++]);
    };
    const apply = (operation, atMS = now()) => {
      machine.apply(atMS, operation);
      diagnostic = "";
      flush();
      publish();
      return machine.snapshot();
    };

    const report = (error) => {
      const message = error?.message ?? String(error);
      diagnostic = byteLength(message) <= DEFAULT_LIMITS.max_string_bytes
        ? message
        : "client adapter diagnostic exceeds limit";
      publish();
    };

    const recordTransportLoss = (reason) => {
      if (disposed) return;
      publishInspection(null);
      const phase = machine.snapshot().connection.phase;
      if (phase === "connected" || phase === "connecting") {
        apply({ kind: "transport_lost", reason });
      }
      if (machine.snapshot().connection.phase === "reconnecting") retry();
    };

    const retry = () => {
      clearTimeout(retryTimer);
      const snapshot = machine.snapshot();
      if (disposed || snapshot.connection.phase !== "reconnecting") return;
      const delay = Math.max(0, snapshot.connection.next_retry_ms - now());
      retryTimer = setTimeout(async () => {
        if (disposed) return;
        try {
          apply({ kind: "retry" }, snapshot.connection.next_retry_ms);
          await connection.connect();
          if (disposed) return;
          apply({ kind: "connected" });
        } catch (error) {
          if (disposed) return;
          try {
            recordTransportLoss(error?.message ?? "reconnect failed");
          } catch (terminal) {
            report(terminal);
          }
        }
      }, delay);
    };

    const offState = connection.onState((physical, detail = "") => {
      if (disposed || (physical !== "closed" && physical !== "failed")) return;
      const phase = machine.snapshot().connection.phase;
      if (phase !== "connected" && phase !== "connecting") return;
      try {
        recordTransportLoss(detail || `transport ${physical}`);
      } catch (error) {
        report(error);
      }
    });
    const offMessage = connection.subscribe((raw) => {
      try {
        const source = String(raw);
        if (byteLength(source) > DEFAULT_LIMITS.max_event_bytes) {
          throw new Error(`inbound event exceeds ${DEFAULT_LIMITS.max_event_bytes} bytes`);
        }
        const event = parseStrictJSON(source);
        // Validate the capability-bearing projection before mutating either
        // reducer or service state, then publish only after the canonical
        // reducer accepted the complete event. Consumers therefore never see
        // an event that the shared state machine rejected.
        const projectedInspection = inspectionProjection(event);
        apply({ kind: "inbound", event });
        if (projectedInspection !== undefined) publishInspection(projectedInspection);
        publishProtocol(event);
      } catch (error) {
        // Parser/reducer failures are adapter diagnostics, not synthetic
        // server error events. Keep the last valid immutable snapshot and
        // expose the bounded message separately to the view.
        report(error);
      }
    });

    const state = Object.freeze({
      snapshot: () => machine.snapshot(),
      diagnostic: () => diagnostic,
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("state listener must be a function");
        listeners.add(listener);
        try { listener(machine.snapshot(), diagnostic); } catch {}
        return () => listeners.delete(listener);
      },
      async connect() {
        const phase = machine.snapshot().connection.phase;
        if (phase === "connected" || phase === "connecting" || phase === "reconnecting") return;
        apply({ kind: "connect", transport: connection.kind ?? "websocket" });
        try {
          await connection.connect();
          if (disposed) return;
          apply({ kind: "connected" });
        } catch (error) {
          if (disposed) throw error;
          recordTransportLoss(error?.message ?? "connection failed");
          throw error;
        }
      },
      disconnect() {
        clearTimeout(retryTimer);
        apply({ kind: "disconnect", reason: "left room" });
        connection.close();
      },
      sendText(value) {
        const text = String(value).trim();
        if (!text) return;
        apply({ kind: "typed_text", item_id: `local-${++localItem}`, text });
      },
      updateSession(session) {
        const encoded = JSON.stringify(session);
        if (byteLength(encoded) > DEFAULT_LIMITS.max_event_bytes) {
          throw new Error(`session update exceeds ${DEFAULT_LIMITS.max_event_bytes} bytes`);
        }
        // Parse through the canonical duplicate-key/depth checks even when a
        // caller supplied an object: this keeps programmatic and wire-shaped
        // configuration on the same reducer boundary.
        return apply({ kind: "session_update", session: parseStrictJSON(encoded) });
      },
      endTurn: () => apply({ kind: "end_turn" }),
      cancelResponse: () => apply({ kind: "cancel_response" }),
      setPlayout(itemID, playedMS, speaking = true) {
        return apply({ kind: "playout", speaking, item_id: speaking ? itemID : "", played_ms: speaking ? playedMS : 0 });
      },
      toolResult(callID, status, output = "", error = "") {
        return apply({ kind: "tool_result", call_id: callID, status, output, error });
      },
    });
    context.state.snapshot(() => reducerAdapterState({
      machine: machine.snapshot(),
      outbound: machine.outbound(),
      command_cursor: commandCursor,
      local_item: localItem,
      inspection_access: inspectionAccess && Date.now() < inspectionAccess.expires_at_ms
        ? structuredClone(inspectionAccess) : null,
    }, "live reducer state"));
    context.publish("presentation.client.session_state", state);
    context.publish("presentation.client.strict_json", Object.freeze({
      parse: (source) => parseStrictJSON(source),
      stable: (value) => stableJSON(value),
    }));
    const inspection = Object.freeze({
      current: () => inspectionAccess ? { ...inspectionAccess } : null,
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("inspection listener must be a function");
        inspectionListeners.add(listener);
        try { listener(inspectionAccess ? { ...inspectionAccess } : null); } catch {}
        return () => inspectionListeners.delete(listener);
      },
    });
    context.publish("presentation.client.inspection_access", inspection);
    const protocolEvents = Object.freeze({
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("protocol event listener must be a function");
        protocolListeners.add(listener);
        return () => protocolListeners.delete(listener);
      },
    });
    context.publish("presentation.client.protocol_events", protocolEvents);
    context.lifecycle.defer("reducer", () => {
      disposed = true;
      clearTimeout(retryTimer);
      offState();
      offMessage();
      listeners.clear();
      publishInspection(null);
      inspectionListeners.clear();
      protocolListeners.clear();
      const phase = machine.snapshot().connection.phase;
      if (phase !== "disconnected") {
        try { machine.apply(now(), { kind: "disconnect", reason: "client scope disposed" }); } catch {}
      }
    });
  },
};
