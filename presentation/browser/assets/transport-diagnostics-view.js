export default {
  name: "openrealtime.presentation.client.transport-diagnostics-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const diagnostics = context.services.get("presentation.client.transport_diagnostics");
    if (!slots || !diagnostics) throw new Error("transport diagnostics dependencies are unavailable");
    const section = document.createElement("section");
    section.dataset.view = "transport-diagnostics";
    section.innerHTML = `<style>
      [data-view=transport-diagnostics] { max-width:54rem; margin:0 auto 1rem; padding:0 1rem; }
      [data-view=transport-diagnostics] output { font:12px ui-monospace,monospace; }
    </style><h2>Transport</h2><output id="transport-stats">pending</output>`;
    const output = section.querySelector("#transport-stats");
    let disposed = false;
    let timer;
    let reading = false;
    const poll = async () => {
      if (disposed || reading) return;
      reading = true;
      try {
        const snapshot = await diagnostics.snapshot();
        if (disposed) return;
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
    timer = setInterval(poll, 100);
    await poll();
    context.lifecycle.defer("transport-diagnostics-poll", () => {
      disposed = true;
      clearInterval(timer);
    });
    context.lifecycle.defer("transport-diagnostics-slot", unregister);
  },
};
