// This file is only the manifest-plugin adapter. bundle.go prepends the exact
// canonical module from client/reducer/javascript/reducer.mjs before hashing
// and publishing reducer.js; protocol state must not be reimplemented here.

export default {
  name: "openrealtime.presentation.client.reducer",
  revision: 1,
  async mount(context) {
    const connection = context.services.get("presentation.client.connection");
    if (!connection) throw new Error("connection service is unavailable");

    const machine = new ClientReducer(DEFAULT_LIMITS);
    const listeners = new Set();
    const inspectionListeners = new Set();
    const protocolListeners = new Set();
    const startedAt = performance.now();
    let commandCursor = 0;
    let localItem = 0;
    let retryTimer;
    let disposed = false;
    let diagnostic = "";
    let inspectionAccess = null;

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
      const sessionID = String(access.session_id ?? "");
      const path = String(access.path ?? "");
      const token = String(access.token ?? "");
      const expiresAtMS = Number(access.expires_at_ms ?? 0);
      const canonicalPath = `/openrealtime/v1/sessions/${sessionID}/live`;
      if (!/^[A-Za-z0-9._:-]{1,256}$/.test(sessionID) || path !== canonicalPath ||
          byteLength(path) > DEFAULT_LIMITS.max_string_bytes ||
          !/^mgmt_[A-Za-z0-9_-]+$/.test(token) || byteLength(token) > 512 ||
          !Number.isSafeInteger(expiresAtMS) || expiresAtMS <= 0) {
        throw new Error("session inspection access is malformed");
      }
      return Object.freeze({ session_id: sessionID, path, token, expires_at_ms: expiresAtMS });
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
