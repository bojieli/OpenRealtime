function node(name, text = "") {
  const value = document.createElement(name);
  value.textContent = text;
  return value;
}

const busyPhases = new Set([
  "reading", "analyzing", "formatting", "renaming", "removing-edge", "compiling", "rendering", "publishing",
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
    const edges = node("div");
    edges.dataset.role = "edges";
    const edgeControls = node("div");
    edgeControls.dataset.role = "edge-removal";
    const removeEdge = node("button", "Remove selected edge");
    removeEdge.type = "button";
    removeEdge.dataset.action = "remove-edge";
    const edgeStatus = node("p", "Select a compiled edge to remove it.");
    edgeStatus.dataset.role = "edge-status";
    edgeControls.append(removeEdge, edgeStatus);
    const rendering = node("pre");
    rendering.dataset.role = "rendering";
    section.append(controls, identity, nodes, renameControls, edges, edgeControls, rendering);

    let selectedFingerprint = "";
    let selectedNode = "";
    let selectedEdgeFingerprint = "";
    let selectedEdge = "";
    let canvasBusy = false;
    let nodeEvents = [];
    let edgeEvents = [];
    const clearNodeEvents = () => {
      for (const [button, listener] of nodeEvents) button.removeEventListener("click", listener);
      nodeEvents = [];
    };
    const clearEdgeEvents = () => {
      for (const [button, listener] of edgeEvents) button.removeEventListener("click", listener);
      edgeEvents = [];
    };
    const markSelection = () => {
      for (const [button] of nodeEvents) {
        button.dataset.selected = String(button.dataset.node === selectedNode &&
          button.dataset.fingerprint === selectedFingerprint);
      }
      rename.disabled = !selectedNode || canvasBusy;
      replacement.disabled = !selectedNode || canvasBusy;
    };
    const markEdgeSelection = () => {
      for (const [button] of edgeEvents) {
        button.dataset.selected = String(button.dataset.edge === selectedEdge &&
          button.dataset.fingerprint === selectedEdgeFingerprint);
      }
      removeEdge.disabled = !selectedEdge || canvasBusy;
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
      if (!graph || graph.fingerprint !== selectedEdgeFingerprint ||
          !(graph.edges ?? []).some((entry) => entry.id === selectedEdge)) {
        selectedEdgeFingerprint = "";
        selectedEdge = "";
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
      clearEdgeEvents();
      edges.replaceChildren();
      for (const graphEdge of [...(graph?.edges ?? [])].sort((left, right) => left.id.localeCompare(right.id))) {
        const from = `${graphEdge.from?.node ?? "?"}.${graphEdge.from?.port ?? "?"}`;
        const to = `${graphEdge.to?.node ?? "?"}.${graphEdge.to?.port ?? "?"}`;
        const button = node("button", `${graphEdge.id} · ${from} → ${to}`);
        button.type = "button";
        button.dataset.action = "select-edge";
        button.dataset.edge = graphEdge.id;
        button.dataset.fingerprint = graph.fingerprint;
        const select = () => {
          selectedEdgeFingerprint = graph.fingerprint;
          selectedEdge = graphEdge.id;
          edgeStatus.textContent = `Selected ${graphEdge.id}.`;
          markEdgeSelection();
        };
        button.addEventListener("click", select);
        edgeEvents.push([button, select]);
        edges.append(button);
      }
      markSelection();
      markEdgeSelection();
      if (!graph && snapshot.phase === "renamed") {
        renameStatus.textContent = "Node renamed. Compile the updated graph to continue.";
      } else if (!graph && snapshot.phase !== "error") {
        renameStatus.textContent = "Select a compiled node to rename it.";
      }
      if (!graph && snapshot.phase === "edge-removed") {
        edgeStatus.textContent = "Edge removed. Compile the updated graph to continue.";
      } else if (!graph && snapshot.phase !== "error") {
        edgeStatus.textContent = "Select a compiled edge to remove it.";
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
    const removeEdgeClick = async () => {
      const fingerprint = selectedEdgeFingerprint;
      const current = selectedEdge;
      try {
        edgeStatus.textContent = `Removing ${current}…`;
        await workspace.removeEdge(fingerprint, current);
        edgeStatus.textContent = `Removed ${current}. Compile the updated graph to continue.`;
      } catch (error) {
        edgeStatus.textContent = error?.message ?? String(error);
      }
    };
    model.addEventListener("click", modelClick);
    mermaid.addEventListener("click", mermaidClick);
    dot.addEventListener("click", dotClick);
    rename.addEventListener("click", renameClick);
    removeEdge.addEventListener("click", removeEdgeClick);
    const unregister = slots.register("authoring.canvas", section, 40);
    context.lifecycle.defer("authoring-canvas-events", () => {
      model.removeEventListener("click", modelClick);
      mermaid.removeEventListener("click", mermaidClick);
      dot.removeEventListener("click", dotClick);
      rename.removeEventListener("click", renameClick);
      removeEdge.removeEventListener("click", removeEdgeClick);
      clearNodeEvents();
      clearEdgeEvents();
      replacement.value = "";
      rendering.textContent = "";
    });
    context.lifecycle.defer("authoring-canvas-state", changed);
    context.lifecycle.defer("authoring-canvas-slot", unregister);
  },
};
