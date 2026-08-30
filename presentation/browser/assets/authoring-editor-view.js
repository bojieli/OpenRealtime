function node(name, text = "") {
  const value = document.createElement(name);
  value.textContent = text;
  return value;
}

export default {
  name: "openrealtime.presentation.client.authoring-editor-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const workspace = context.services.get("presentation.client.authoring_workspace");
    if (!slots || !workspace) throw new Error("authoring editor view dependencies are unavailable");
    const section = node("section");
    section.dataset.view = "authoring-editor";
    section.append(node("h2", "Graph source"));
    const path = node("input");
    path.name = "authoring-path";
    path.maxLength = 4096;
    const source = node("textarea");
    source.name = "authoring-source";
    source.spellcheck = false;
    const controls = node("div");
    const analyze = node("button", "Analyze");
    const compile = node("button", "Compile");
    analyze.dataset.action = "analyze";
    compile.dataset.action = "compile";
    const status = node("p", "idle");
    status.dataset.role = "status";
    controls.append(analyze, compile);
    section.append(path, source, controls, status);
    let renderedEpoch = -1;
    const changed = workspace.subscribe((snapshot) => {
      if (snapshot.epoch !== renderedEpoch) {
        renderedEpoch = snapshot.epoch;
        path.value = snapshot.document.path;
        source.value = snapshot.document.source;
      }
      status.textContent = snapshot.error ? `${snapshot.phase}: ${snapshot.error}` : snapshot.phase;
    });
    const commit = () => workspace.setDocument(path.value, source.value, workspace.snapshot().document.revision);
    const run = (operation) => async () => {
      analyze.disabled = compile.disabled = true;
      try {
        commit();
        await operation();
      } catch (error) {
        status.textContent = error?.message ?? String(error);
      } finally {
        analyze.disabled = compile.disabled = false;
      }
    };
    const analyzeClick = run(() => workspace.analyze());
    const compileClick = run(() => workspace.compile());
    analyze.addEventListener("click", analyzeClick);
    compile.addEventListener("click", compileClick);
    const unregister = slots.register("authoring.editor", section, 20);
    context.lifecycle.defer("authoring-editor-events", () => {
      analyze.removeEventListener("click", analyzeClick);
      compile.removeEventListener("click", compileClick);
      source.value = "";
    });
    context.lifecycle.defer("authoring-editor-state", changed);
    context.lifecycle.defer("authoring-editor-slot", unregister);
  },
};
