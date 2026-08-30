function node(name, text = "") {
  const value = document.createElement(name);
  value.textContent = text;
  return value;
}

export default {
  name: "openrealtime.presentation.client.authoring-canvas-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const workspace = context.services.get("presentation.client.authoring_workspace");
    if (!slots || !workspace) throw new Error("authoring canvas view dependencies are unavailable");
    const section = node("section");
    section.dataset.view = "authoring-canvas";
    section.append(node("h2", "Graph canvas"));
    const controls = node("div");
    const model = node("button", "Model");
    const mermaid = node("button", "Mermaid");
    const dot = node("button", "DOT");
    model.dataset.action = "render-model";
    mermaid.dataset.action = "render-mermaid";
    dot.dataset.action = "render-dot";
    controls.append(model, mermaid, dot);
    const identity = node("p", "Compile a graph to render it.");
    const rendering = node("pre");
    rendering.dataset.role = "rendering";
    section.append(controls, identity, rendering);
    const changed = workspace.subscribe((snapshot) => {
      const graph = snapshot.compiled?.graph;
      identity.textContent = graph
        ? `${graph.id} · r${graph.revision} · ${graph.fingerprint}`
        : "Compile a graph to render it.";
      const result = snapshot.rendering;
      rendering.textContent = result ? (result.text ?? JSON.stringify(result.model, null, 2)) : "";
      model.disabled = mermaid.disabled = dot.disabled = !graph;
    });
    const run = (format) => async () => {
      try { await workspace.render(format); }
      catch (error) { rendering.textContent = error?.message ?? String(error); }
    };
    const modelClick = run("model");
    const mermaidClick = run("mermaid");
    const dotClick = run("dot");
    model.addEventListener("click", modelClick);
    mermaid.addEventListener("click", mermaidClick);
    dot.addEventListener("click", dotClick);
    const unregister = slots.register("authoring.canvas", section, 40);
    context.lifecycle.defer("authoring-canvas-events", () => {
      model.removeEventListener("click", modelClick);
      mermaid.removeEventListener("click", mermaidClick);
      dot.removeEventListener("click", dotClick);
      rendering.textContent = "";
    });
    context.lifecycle.defer("authoring-canvas-state", changed);
    context.lifecycle.defer("authoring-canvas-slot", unregister);
  },
};
