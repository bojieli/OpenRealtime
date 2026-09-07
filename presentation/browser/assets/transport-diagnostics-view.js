export default {
  name: "openrealtime.presentation.client.transport-diagnostics-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const diagnostics = context.services.get("presentation.client.transport_diagnostics");
    if (!slots || !diagnostics) throw new Error("transport diagnostics dependencies are unavailable");
    const connection = context.services.get("presentation.client.connection");
    const section = document.createElement("section");
    section.dataset.view = "transport-diagnostics";
    section.innerHTML = `<style>
      [data-view=transport-diagnostics] { max-width:none; margin:0 auto 1rem; padding:0 1rem; }
      [data-view=transport-diagnostics] output { font:12px ui-monospace,monospace; }
          #room-debug-log { max-height:180px; overflow:auto; white-space:pre-wrap; font:12px ui-monospace,monospace; color:#afc3dc; }
    </style><h2>Connection & debug logs</h2><p id="connection-latency">Connection latency: awaiting connection</p><output id="transport-stats">pending</output><pre id="room-debug-log" aria-label="Debug logs"></pre>`;
    const output = section.querySelector("#transport-stats");
    const log = section.querySelector("#room-debug-log");
    const latency = section.querySelector("#connection-latency");
    const rows = [];
    const append = (message) => {
      rows.push(`${new Date().toISOString().slice(11, 23)} ${message}`);
      if (rows.length > 200) rows.shift();
      const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 24;
      log.textContent = rows.join("\n");
      if (atBottom) log.scrollTop = log.scrollHeight;
    };
    let started, setupMS;
    const unsubscribeState = connection.onState((state, detail) => {
      if (state === "connecting") { started = performance.now(); setupMS = undefined; }
      if (state === "connected" && started !== undefined) setupMS = Math.round(performance.now() - started);
      append(`Connection: ${state}${detail ? " · " + detail : ""}`);
    });
    const unsubscribeEvents = connection.subscribe((message) => {
      try {
        const event = typeof message === "string" ? JSON.parse(message) : message;
        if (event.type?.includes("delta") || event.type?.includes("audio_buffer.append")) return;
        append(`Received: ${event.type ?? "event"}${event.error?.message ? " · " + event.error.message : ""}`);
      } catch { append("Received an unreadable protocol event"); }
    });
    context.lifecycle.defer("room-debug-subscriptions", () => { unsubscribeState(); unsubscribeEvents(); });
    let disposed = false;
    let timer;
    let reading = false;
    const poll = async () => {
      if (disposed || reading) return;
      reading = true;
      try {
        const snapshot = await diagnostics.snapshot();
        if (disposed) return;
        latency.textContent = `Connection latency (round trip): ${snapshot.state === "connected" && snapshot.round_trip_time_ms > 0 ? snapshot.round_trip_time_ms + " ms" : "awaiting measurement"} · Join time: ${setupMS === undefined ? "—" : setupMS + " ms"}`;
        output.dataset.audioBytes = String(snapshot.audio_bytes_received);
        output.dataset.audioPackets = String(snapshot.audio_packets_received);
        output.dataset.audioLost = String(snapshot.audio_packets_lost);
        output.dataset.queueBytes = String(snapshot.data_channel_buffered_bytes);
        output.textContent = `${snapshot.state}; audio=${snapshot.audio_bytes_received} B/` +
          `${snapshot.audio_packets_received} packets; lost=${snapshot.audio_packets_lost}; ` +
          `data-queue=${snapshot.data_channel_buffered_bytes} B; rtt=${snapshot.round_trip_time_ms} ms`;
      } catch (error) {
        if (!disposed) output.textContent = error?.message ?? String(error);
      } finally {
        reading = false;
      }
    };
    const unregister = slots.register("inspection.transport", section, 30);
    timer = setInterval(poll, 500);
    await poll();
    context.lifecycle.defer("transport-diagnostics-poll", () => {
      disposed = true;
      clearInterval(timer);
    });
    context.lifecycle.defer("transport-diagnostics-slot", unregister);
  },
};
