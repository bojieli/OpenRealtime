function node(name, text = "") {
  const value = document.createElement(name);
  value.textContent = text;
  return value;
}

const busyPhases = new Set([
  "reading", "analyzing", "formatting", "renaming", "removing-edge", "creating-edge",
  "compiling", "rendering", "publishing",
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
    const endpointControls = node("div");
    endpointControls.dataset.role = "edge-endpoints";
    const outputs = node("div");
    outputs.dataset.role = "edge-outputs";
    const inputs = node("div");
    inputs.dataset.role = "edge-inputs";
    endpointControls.append(outputs, inputs);
    const createControls = node("div");
    createControls.dataset.role = "edge-creation";
    const edgeNameLabel = node("label", "New edge name ");
    const edgeName = node("input");
    edgeName.name = "authoring-edge-name";
    edgeName.maxLength = 256;
    edgeName.placeholder = "new_edge";
    edgeNameLabel.append(edgeName);
    const createEdge = node("button", "Create selected edge");
    createEdge.type = "button";
    createEdge.dataset.action = "create-edge";
    const createStatus = node("p", "Select one output and one input port.");
    createStatus.dataset.role = "edge-creation-status";
    createControls.append(edgeNameLabel, createEdge, createStatus);
    const rendering = node("pre");
    rendering.dataset.role = "rendering";
    section.append(controls, identity, nodes, renameControls, edges, edgeControls,
      endpointControls, createControls, rendering);

    let selectedFingerprint = "";
    let selectedNode = "";
    let selectedEdgeFingerprint = "";
    let selectedEdge = "";
    let creationFingerprint = "";
    let selectedFrom = "";
    let selectedTo = "";
    let canvasBusy = false;
    let nodeEvents = [];
    let edgeEvents = [];
    let endpointEvents = [];
    const clearNodeEvents = () => {
      for (const [button, listener] of nodeEvents) button.removeEventListener("click", listener);
      nodeEvents = [];
    };
    const clearEdgeEvents = () => {
      for (const [button, listener] of edgeEvents) button.removeEventListener("click", listener);
      edgeEvents = [];
    };
    const clearEndpointEvents = () => {
      for (const [button, listener] of endpointEvents) button.removeEventListener("click", listener);
      endpointEvents = [];
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
    const markCreationSelection = () => {
      for (const [button] of endpointEvents) {
        const selected = button.dataset.direction === "output"
          ? button.dataset.endpoint === selectedFrom
          : button.dataset.endpoint === selectedTo;
        button.dataset.selected = String(selected && button.dataset.fingerprint === creationFingerprint);
      }
      createEdge.disabled = !selectedFrom || !selectedTo || !edgeName.value || canvasBusy;
      edgeName.disabled = !creationFingerprint || canvasBusy;
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
      if (!graph || graph.fingerprint !== creationFingerprint) {
        creationFingerprint = "";
        selectedFrom = "";
        selectedTo = "";
        edgeName.value = "";
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
      clearEndpointEvents();
      outputs.replaceChildren();
      inputs.replaceChildren();
      const ports = [];
      for (const graphNode of graph?.nodes ?? []) {
        for (const port of graphNode.ports ?? []) {
          if (port.direction === "output" || port.direction === "input") {
            ports.push({ endpoint: `${graphNode.id}.${port.name}`, direction: port.direction });
          }
        }
      }
      ports.sort((left, right) => left.endpoint.localeCompare(right.endpoint) ||
        left.direction.localeCompare(right.direction));
      for (const port of ports) {
        const button = node("button", `${port.direction} · ${port.endpoint}`);
        button.type = "button";
        button.dataset.action = port.direction === "output" ? "select-edge-from" : "select-edge-to";
        button.dataset.endpoint = port.endpoint;
        button.dataset.direction = port.direction;
        button.dataset.fingerprint = graph.fingerprint;
        const select = () => {
          creationFingerprint = graph.fingerprint;
          if (port.direction === "output") selectedFrom = port.endpoint;
          else selectedTo = port.endpoint;
          createStatus.textContent = `Selected ${selectedFrom || "an output"} → ${selectedTo || "an input"}.`;
          markCreationSelection();
        };
        button.addEventListener("click", select);
        endpointEvents.push([button, select]);
        (port.direction === "output" ? outputs : inputs).append(button);
      }
      markSelection();
      markEdgeSelection();
      markCreationSelection();
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
      if (!graph && snapshot.phase === "edge-created") {
        createStatus.textContent = "Edge created. Compile the updated graph to continue.";
      } else if (!graph && snapshot.phase !== "error") {
        createStatus.textContent = "Select one output and one input port.";
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
    const endpoint = (value) => {
      const separator = value.indexOf(".");
      return { node: value.slice(0, separator), port: value.slice(separator + 1) };
    };
    const createEdgeClick = async () => {
      const fingerprint = creationFingerprint;
      const from = selectedFrom;
      const to = selectedTo;
      const name = edgeName.value;
      try {
        createStatus.textContent = `Creating ${name}…`;
        await workspace.createEdge(fingerprint, name, endpoint(from), endpoint(to), "lossless");
        createStatus.textContent = `Created ${name}. Compile the updated graph to continue.`;
      } catch (error) {
        createStatus.textContent = error?.message ?? String(error);
      }
    };
    const edgeNameInput = () => markCreationSelection();
    model.addEventListener("click", modelClick);
    mermaid.addEventListener("click", mermaidClick);
    dot.addEventListener("click", dotClick);
    rename.addEventListener("click", renameClick);
    removeEdge.addEventListener("click", removeEdgeClick);
    createEdge.addEventListener("click", createEdgeClick);
    edgeName.addEventListener("input", edgeNameInput);
    const unregister = slots.register("authoring.canvas", section, 40);
    context.lifecycle.defer("authoring-canvas-events", () => {
      model.removeEventListener("click", modelClick);
      mermaid.removeEventListener("click", mermaidClick);
      dot.removeEventListener("click", dotClick);
      rename.removeEventListener("click", renameClick);
      removeEdge.removeEventListener("click", removeEdgeClick);
      createEdge.removeEventListener("click", createEdgeClick);
      edgeName.removeEventListener("input", edgeNameInput);
      clearNodeEvents();
      clearEdgeEvents();
      clearEndpointEvents();
      replacement.value = "";
      edgeName.value = "";
      rendering.textContent = "";
    });
    context.lifecycle.defer("authoring-canvas-state", changed);
    context.lifecycle.defer("authoring-canvas-slot", unregister);
  },
};
