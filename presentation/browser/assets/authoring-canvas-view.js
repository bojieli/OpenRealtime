function node(name, text = "") {
  const value = document.createElement(name);
  value.textContent = text;
  return value;
}

const busyPhases = new Set([
  "reading", "analyzing", "formatting", "renaming", "compiling", "rendering", "publishing",
]);

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
    const nodes = node("div");
    nodes.dataset.role = "nodes";
    const renameControls = node("div");
    renameControls.dataset.role = "node-rename";
    const renameLabel = node("label", "Rename selected node ");
    const replacement = node("input");
    replacement.name = "authoring-node-name";
    replacement.maxLength = 256;
    replacement.placeholder = "new_node_name";
    renameLabel.append(replacement);
    const rename = node("button", "Rename");
    rename.type = "button";
    rename.dataset.action = "rename-node";
    const renameStatus = node("p", "Select a compiled node to rename it.");
    renameStatus.dataset.role = "rename-status";
    renameControls.append(renameLabel, rename, renameStatus);
    const rendering = node("pre");
    rendering.dataset.role = "rendering";
    section.append(controls, identity, nodes, renameControls, rendering);

    let selectedFingerprint = "";
    let selectedNode = "";
    let canvasBusy = false;
    let nodeEvents = [];
    const clearNodeEvents = () => {
      for (const [button, listener] of nodeEvents) button.removeEventListener("click", listener);
      nodeEvents = [];
    };
    const markSelection = () => {
      for (const [button] of nodeEvents) {
        button.dataset.selected = String(button.dataset.node === selectedNode &&
          button.dataset.fingerprint === selectedFingerprint);
      }
      rename.disabled = !selectedNode || canvasBusy;
      replacement.disabled = !selectedNode || canvasBusy;
    };
    const changed = workspace.subscribe((snapshot) => {
      const graph = snapshot.compiled?.graph;
      identity.textContent = graph
        ? `${graph.id} · r${graph.revision} · ${graph.fingerprint}`
        : "Compile a graph to render it.";
      const result = snapshot.rendering;
      rendering.textContent = result ? (result.text ?? JSON.stringify(result.model, null, 2)) : "";
      canvasBusy = busyPhases.has(snapshot.phase);
      model.disabled = mermaid.disabled = dot.disabled = !graph || canvasBusy;

      if (!graph || graph.fingerprint !== selectedFingerprint ||
          !(graph.nodes ?? []).some((entry) => entry.id === selectedNode)) {
        selectedFingerprint = "";
        selectedNode = "";
        replacement.value = "";
      }
      clearNodeEvents();
      nodes.replaceChildren();
      for (const graphNode of [...(graph?.nodes ?? [])].sort((left, right) => left.id.localeCompare(right.id))) {
        const button = node("button", `${graphNode.id} · ${graphNode.element?.name ?? "unknown element"}`);
        button.type = "button";
        button.dataset.action = "select-node";
        button.dataset.node = graphNode.id;
        button.dataset.fingerprint = graph.fingerprint;
        const select = () => {
          selectedFingerprint = graph.fingerprint;
          selectedNode = graphNode.id;
          replacement.value = "";
          renameStatus.textContent = `Selected ${graphNode.id}.`;
          markSelection();
        };
        button.addEventListener("click", select);
        nodeEvents.push([button, select]);
        nodes.append(button);
      }
      markSelection();
      if (!graph && snapshot.phase === "renamed") {
        renameStatus.textContent = "Node renamed. Compile the updated graph to continue.";
      } else if (!graph && snapshot.phase !== "error") {
        renameStatus.textContent = "Select a compiled node to rename it.";
      }
    });
    const run = (format) => async () => {
      try { await workspace.render(format); }
      catch (error) { rendering.textContent = error?.message ?? String(error); }
    };
    const modelClick = run("model");
    const mermaidClick = run("mermaid");
    const dotClick = run("dot");
    const renameClick = async () => {
      const fingerprint = selectedFingerprint;
      const current = selectedNode;
      try {
        renameStatus.textContent = `Renaming ${current}…`;
        await workspace.renameNode(fingerprint, current, replacement.value);
        renameStatus.textContent = `Renamed ${current}. Compile the updated graph to continue.`;
      } catch (error) {
        renameStatus.textContent = error?.message ?? String(error);
      }
    };
    model.addEventListener("click", modelClick);
    mermaid.addEventListener("click", mermaidClick);
    dot.addEventListener("click", dotClick);
    rename.addEventListener("click", renameClick);
    const unregister = slots.register("authoring.canvas", section, 40);
    context.lifecycle.defer("authoring-canvas-events", () => {
      model.removeEventListener("click", modelClick);
      mermaid.removeEventListener("click", mermaidClick);
      dot.removeEventListener("click", dotClick);
      rename.removeEventListener("click", renameClick);
      clearNodeEvents();
      replacement.value = "";
      rendering.textContent = "";
    });
    context.lifecycle.defer("authoring-canvas-state", changed);
    context.lifecycle.defer("authoring-canvas-slot", unregister);
  },
};
