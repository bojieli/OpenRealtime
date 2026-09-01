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
    const format = node("button", "Format");
    const compile = node("button", "Compile");
    analyze.dataset.action = "analyze";
    format.dataset.action = "format";
    compile.dataset.action = "compile";
    const status = node("p", "idle");
    status.dataset.role = "status";
    const diagnosticStatus = node("p", "Analyze a graph to inspect diagnostics.");
    diagnosticStatus.dataset.role = "diagnostic-status";
    const diagnostics = node("ol");
    diagnostics.dataset.role = "diagnostics";
    controls.append(analyze, format, compile);
    const publication = node("fieldset");
    publication.dataset.role = "source-publication";
    publication.append(node("legend", "Mediated source access"));
    const rootLabel = node("label", "Root identity ");
    const rootIdentity = node("input");
    rootIdentity.name = "authoring-root-identity";
    rootIdentity.maxLength = 71;
    rootIdentity.placeholder = "sha256:…";
    rootLabel.append(rootIdentity);
    const predecessorLabel = node("label", " Expected predecessor digest ");
    const predecessor = node("input");
    predecessor.name = "authoring-expected-digest";
    predecessor.maxLength = 71;
    predecessor.placeholder = "sha256:… (update only)";
    predecessorLabel.append(predecessor);
    const load = node("button", "Load");
    load.type = "button";
    load.dataset.action = "load";
    const create = node("button", "Create");
    create.type = "button";
    create.dataset.action = "publish-create";
    const update = node("button", "Update");
    update.type = "button";
    update.dataset.action = "publish-update";
    const receipt = node("pre");
    receipt.dataset.role = "publication-receipt";
    const readResult = node("pre");
    readResult.dataset.role = "source-read-result";
    publication.append(rootLabel, predecessorLabel, load, create, update, readResult, receipt);
    section.append(path, source, controls, status, diagnosticStatus, diagnostics, publication);
    let renderedEpoch = -1;
    const changed = workspace.subscribe((snapshot) => {
      if (snapshot.epoch !== renderedEpoch) {
        renderedEpoch = snapshot.epoch;
        path.value = snapshot.document.path;
        source.value = snapshot.document.source;
      }
      status.textContent = snapshot.error ? `${snapshot.phase}: ${snapshot.error}` : snapshot.phase;
      diagnostics.replaceChildren();
      const report = snapshot.analysis?.diagnostics;
      if (!report) {
        diagnosticStatus.textContent = "Analyze a graph to inspect diagnostics.";
      } else {
        diagnosticStatus.textContent = `${report.items.length} of ${report.total} diagnostics${
          report.incomplete ? " (truncated)" : ""}`;
        for (const diagnostic of report.items) {
          const item = node("li");
          item.dataset.code = diagnostic.code;
          item.dataset.severity = diagnostic.severity;
          const start = diagnostic.span.start;
          const end = diagnostic.span.end;
          const diagnosticPath = diagnostic.path || snapshot.document.path;
          item.append(node("strong", `[${diagnostic.code}] ${diagnostic.severity}`),
            node("span", ` · ${diagnosticPath}:${start.line}:${start.column}-${end.line}:${end.column}`),
            node("p", diagnostic.message));
          if ((diagnostic.notes ?? []).length !== 0) {
            const notes = node("ul");
            for (const note of diagnostic.notes) notes.append(node("li", note));
            item.append(notes);
          }
          diagnostics.append(item);
        }
      }
      const busy = new Set(["reading", "analyzing", "formatting", "renaming", "removing-edge", "compiling", "rendering", "publishing"])
        .has(snapshot.phase);
      analyze.disabled = compile.disabled = busy;
      format.disabled = busy || !snapshot.analysis?.parsed || snapshot.analysis.recovered ||
        snapshot.analysis.canonical || !snapshot.analysis.formatting ||
        snapshot.analysis.formatting.edits.length === 0;
      load.disabled = busy || !workspace.canRead();
      create.disabled = update.disabled = busy || !workspace.canPublish();
      if (snapshot.sourceRead) {
        predecessor.value = snapshot.sourceRead.source_digest;
        readResult.textContent = JSON.stringify(snapshot.sourceRead, null, 2);
      } else if (snapshot.phase === "idle") {
        readResult.textContent = "";
      }
      if (snapshot.publication) {
        predecessor.value = snapshot.publication.source_digest;
        receipt.textContent = JSON.stringify(snapshot.publication, null, 2);
      } else if (snapshot.phase === "idle") {
        receipt.textContent = "";
      }
    });
    const commit = () => workspace.setDocument(path.value, source.value, workspace.snapshot().document.revision);
    const run = (operation) => async () => {
      try {
        commit();
        await operation();
      } catch (error) {
        status.textContent = error?.message ?? String(error);
      }
    };
    const analyzeClick = run(() => workspace.analyze());
    const formatClick = async () => {
      try {
        const current = workspace.snapshot().document;
        if (path.value !== current.path || source.value !== current.source) {
          throw new Error("analyze the current source before formatting");
        }
        await workspace.format();
      } catch (error) {
        status.textContent = error?.message ?? String(error);
      }
    };
    const compileClick = run(() => workspace.compile());
    const loadClick = async () => {
      try {
        await workspace.load(rootIdentity.value, path.value);
      } catch (error) {
        status.textContent = error?.message ?? String(error);
      }
    };
    const createClick = run(() => workspace.publish("create", rootIdentity.value));
    const updateClick = run(() => workspace.publish("update", rootIdentity.value, predecessor.value));
    analyze.addEventListener("click", analyzeClick);
    format.addEventListener("click", formatClick);
    compile.addEventListener("click", compileClick);
    load.addEventListener("click", loadClick);
    create.addEventListener("click", createClick);
    update.addEventListener("click", updateClick);
    const unregister = slots.register("authoring.editor", section, 20);
    context.lifecycle.defer("authoring-editor-events", () => {
      analyze.removeEventListener("click", analyzeClick);
      format.removeEventListener("click", formatClick);
      compile.removeEventListener("click", compileClick);
      load.removeEventListener("click", loadClick);
      create.removeEventListener("click", createClick);
      update.removeEventListener("click", updateClick);
      source.value = "";
      rootIdentity.value = "";
      predecessor.value = "";
      readResult.textContent = "";
      receipt.textContent = "";
      diagnostics.replaceChildren();
    });
    context.lifecycle.defer("authoring-editor-state", changed);
    context.lifecycle.defer("authoring-editor-slot", unregister);
  },
};
