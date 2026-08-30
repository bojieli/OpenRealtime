export default {
  name: "openrealtime.presentation.client.confirmation-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const effects = context.services.get("presentation.client.tools_effects");
    if (!slots || !effects) throw new Error("confirmation view dependencies are unavailable");
    const section = document.createElement("section");
    section.dataset.view = "effect-confirmations";
    const heading = document.createElement("h2");
    heading.textContent = "Confirm actions";
    const status = document.createElement("p");
    const list = document.createElement("div");
    section.append(heading, status, list);

    const render = (snapshot) => {
      status.dataset.providerPhase = snapshot.phase;
      status.dataset.negotiated = snapshot.negotiated ? "true" : "false";
      status.textContent = snapshot.diagnostic ||
        `${snapshot.phase}${snapshot.negotiated ? " · negotiated" : ""} · ${snapshot.confirmations.length} pending`;
      list.replaceChildren(...snapshot.confirmations.filter((row) => !row.answered).map((row) => {
        const article = document.createElement("article");
        article.dataset.callId = row.id;
        const title = document.createElement("strong");
        title.textContent = row.name;
        const target = document.createElement("p");
        target.textContent = row.target ? `Target: ${row.target}` : "No external target";
        const argumentsView = document.createElement("pre");
        argumentsView.textContent = JSON.stringify(row.arguments, null, 2);
        const approve = document.createElement("button");
        approve.type = "button";
        approve.dataset.decision = "approve";
        approve.textContent = "Approve";
        const decline = document.createElement("button");
        decline.type = "button";
        decline.dataset.decision = "decline";
        decline.textContent = "Decline";
        article.append(title, target, argumentsView, approve, decline);
        return article;
      }));
    };
    const decide = (event) => {
      const button = event.target.closest("button[data-decision]");
      const id = button?.closest("article[data-call-id]")?.dataset.callId;
      if (!id) return;
      try { effects.decide(id, button.dataset.decision === "approve"); }
      catch (error) { status.textContent = error?.message ?? "decision failed"; }
    };
    list.addEventListener("click", decide);
    const unregister = slots.register("effect.confirmation", section, 30);
    const unsubscribe = effects.subscribe(render);
    context.lifecycle.defer("confirmation-view-events", () => list.removeEventListener("click", decide));
    context.lifecycle.defer("confirmation-view-state", unsubscribe);
    context.lifecycle.defer("confirmation-view-slot", unregister);
  },
};
