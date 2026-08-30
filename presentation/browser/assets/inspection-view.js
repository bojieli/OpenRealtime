export default {
  name: "openrealtime.presentation.client.inspection-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const inspection = context.services.get("presentation.client.inspection");
    if (!slots || !inspection) throw new Error("inspection view dependencies are unavailable");
    const section = document.createElement("section");
    section.dataset.view = "inspection";
    section.innerHTML = `<style>
      section[data-view=inspection] { max-width:54rem; margin:1rem auto 2rem; padding:1rem;
        border:1px solid #8885; border-radius:.7rem; font-family:system-ui,sans-serif; }
      section[data-view=inspection] header { display:flex; align-items:center; gap:.75rem; }
      section[data-view=inspection] pre { overflow:auto; max-height:20rem; padding:.75rem;
        background:#7771; white-space:pre-wrap; }
    </style><header><h2>Live graph</h2><span id="availability">waiting</span>
      <button id="refresh" disabled>Refresh</button></header>
      <p id="identity">No scoped inspection capability.</p><pre id="snapshot"></pre>`;
    const availability = section.querySelector("#availability");
    const identity = section.querySelector("#identity");
    const snapshot = section.querySelector("#snapshot");
    const refresh = section.querySelector("#refresh");
    let generation = 0;

    const load = async () => {
      const current = ++generation;
      refresh.disabled = true;
      availability.textContent = "loading";
      try {
        const live = await inspection.live();
        if (current !== generation) return;
        identity.textContent = `${live.graph_id ?? "unknown graph"} · r${live.graph_revision ?? "?"} · ${live.fingerprint ?? "unattested"}`;
        snapshot.textContent = JSON.stringify({
          state: live.state,
          nodes: live.nodes,
          edges: live.edges,
          flows: live.flows,
          dropped: live.trace_dropped,
        }, null, 2);
        availability.textContent = "live";
      } catch (error) {
        if (current !== generation) return;
        snapshot.textContent = "";
        availability.textContent = "unavailable";
        identity.textContent = error?.message ?? String(error);
      } finally {
        if (current === generation) refresh.disabled = !inspection.available();
      }
    };
    const changed = inspection.subscribe((access) => {
      generation++;
      refresh.disabled = !access;
      availability.textContent = access ? "available" : "waiting";
      identity.textContent = access
        ? `session ${access.session_id} · capability expires ${new Date(access.expires_at_ms).toISOString()}`
        : "No scoped inspection capability.";
      snapshot.textContent = "";
      if (access) load();
    });
    refresh.addEventListener("click", load);
    const unregister = slots.register("inspection.graph", section, 100);
    context.lifecycle.defer("inspection-view-events", () => refresh.removeEventListener("click", load));
    context.lifecycle.defer("inspection-view-access", changed);
    context.lifecycle.defer("inspection-view-slot", unregister);
  },
};
