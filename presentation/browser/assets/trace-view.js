export default {
  name: "openrealtime.presentation.client.trace-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const inspection = context.services.get("presentation.client.inspection");
    if (!slots || !inspection) throw new Error("trace view dependencies are unavailable");
    const section = document.createElement("section");
    section.dataset.view = "trace";
    section.innerHTML = `<style>
      section[data-view=trace] { max-width:54rem; margin:1rem auto 2rem; padding:1rem;
        border:1px solid #8885; border-radius:.7rem; font-family:system-ui,sans-serif; }
      section[data-view=trace] header { display:flex; align-items:center; gap:.75rem; }
      section[data-view=trace] ol { max-height:18rem; overflow:auto; padding-left:2rem; }
      section[data-view=trace] li { margin:.3rem 0; font-family:ui-monospace,monospace; }
    </style><header><h2>Causal trace</h2><span id="trace-state">waiting</span>
      <button id="trace-refresh" disabled>Refresh</button></header>
      <p id="trace-identity">No scoped inspection capability.</p><ol id="trace-events"></ol>`;
    const status = section.querySelector("#trace-state");
    const identity = section.querySelector("#trace-identity");
    const events = section.querySelector("#trace-events");
    const refresh = section.querySelector("#trace-refresh");
    let generation = 0;

    const render = async () => {
      const current = ++generation;
      refresh.disabled = true;
      status.textContent = "loading";
      try {
        const trace = await inspection.trace();
        if (current !== generation) return;
        identity.textContent = `${trace.graph?.id ?? "unknown graph"} · ${trace.fingerprint ?? "unattested"}`;
        const rows = [];
        for (const snapshot of trace.snapshots ?? []) {
          rows.push({ sequence: snapshot.sequence, at_ns: snapshot.at_ns, kind: "snapshot" });
        }
        for (const event of trace.events ?? []) {
          rows.push({ sequence: event.sequence, at_ns: event.at_ns, kind: event.kind });
        }
        rows.sort((left, right) => left.sequence - right.sequence);
        events.replaceChildren(...rows.slice(-512).map((row) => {
          const item = document.createElement("li");
          item.dataset.sequence = String(row.sequence);
          item.textContent = `#${row.sequence} · ${row.kind} · ${row.at_ns} ns`;
          return item;
        }));
        status.textContent = `live · ${rows.length} records`;
      } catch (error) {
        if (current !== generation) return;
        events.replaceChildren();
        status.textContent = "unavailable";
        identity.textContent = error?.message ?? String(error);
      } finally {
        if (current === generation) refresh.disabled = !inspection.available();
      }
    };
    const changed = inspection.subscribe((access) => {
      generation++;
      refresh.disabled = !access;
      status.textContent = access ? "available" : "waiting";
      identity.textContent = access
        ? `session ${access.session_id} · payload-free trace`
        : "No scoped inspection capability.";
      events.replaceChildren();
      if (access) render();
    });
    refresh.addEventListener("click", render);
    const unregister = slots.register("inspection.trace", section, 110);
    context.lifecycle.defer("trace-view-events", () => refresh.removeEventListener("click", render));
    context.lifecycle.defer("trace-view-access", changed);
    context.lifecycle.defer("trace-view-slot", unregister);
  },
};
