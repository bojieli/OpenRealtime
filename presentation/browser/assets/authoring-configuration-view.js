function node(name, text = "") {
  const value = document.createElement(name);
  value.textContent = text;
  return value;
}

export default {
  name: "openrealtime.presentation.client.authoring-configuration-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const workspace = context.services.get("presentation.client.authoring_workspace");
    const catalog = context.services.get("presentation.client.management_static");
    if (!slots || !workspace || !catalog) {
      throw new Error("authoring configuration view dependencies are unavailable");
    }
    const section = node("section");
    section.dataset.view = "authoring-configuration";
    section.append(node("h2", "Configuration contracts"));
    const staticControls = node("div");
    const fingerprint = node("input");
    fingerprint.name = "management-graph-fingerprint";
    fingerprint.placeholder = "sha256:…";
    fingerprint.maxLength = 71;
    const load = node("button", "Load exact catalog");
    load.type = "button";
    load.dataset.action = "load-static";
    staticControls.append(fingerprint, load);
    const catalogStatus = node("p", "No static graph loaded.");
    catalogStatus.dataset.role = "catalog-status";
    const catalogMetadata = node("pre");
    catalogMetadata.dataset.role = "catalog-result";
    const status = node("p", "Analyze a graph to inspect configuration metadata.");
    const list = node("div");
    list.dataset.role = "metadata";
    section.append(staticControls, catalogStatus, catalogMetadata, status, list);
    let staticGeneration = 0;
    const loadStatic = async () => {
      const generation = ++staticGeneration;
      load.disabled = true;
      catalogStatus.textContent = "loading";
      catalogMetadata.textContent = "";
      try {
        const graph = await catalog.graph(fingerprint.value);
        const descriptor = await catalog.element(graph.nodes[0].element);
        const values = await catalog.valuesSchema(graph.fingerprint);
        if (generation !== staticGeneration) return;
        catalogStatus.textContent = "loaded";
        catalogMetadata.textContent = JSON.stringify({
          graph: { id: graph.id, revision: graph.revision, fingerprint: graph.fingerprint },
          element: { name: descriptor.name, revision: descriptor.revision,
            digest: graph.nodes[0].element.digest },
          values_schema: { digest: values.digest, complete: values.complete,
            nodes: values.nodes.length, unresolved: values.unresolved.length },
        }, null, 2);
      } catch (error) {
        if (generation !== staticGeneration) return;
        catalogStatus.textContent = "unavailable";
        catalogMetadata.textContent = error?.message ?? String(error);
      } finally {
        if (generation === staticGeneration) load.disabled = false;
      }
    };
    load.addEventListener("click", loadStatic);
    const changed = workspace.subscribe((snapshot) => {
      list.replaceChildren();
      const report = snapshot.analysis?.catalog;
      if (!report) {
        status.textContent = "Analyze a graph to inspect configuration metadata.";
        return;
      }
      status.textContent = `${report.elements.length} of ${report.total} element contracts`;
      for (const metadata of report.elements) {
        const article = node("article");
        article.dataset.element = metadata.identity.name;
        article.append(node("h3", `${metadata.identity.name} · r${metadata.identity.revision}`));
        const config = metadata.config;
        article.append(node("p", `${config.schema_status} · ${config.artifact}`));
        if (config.schema_reference) article.append(node("code", config.schema_reference));
        if (config.schema_id) article.append(node("code", config.schema_id));
        if (config.schema_digest) article.append(node("code", config.schema_digest));
        const properties = node("ul");
        for (const property of config.properties) {
          const types = Array.isArray(property.types) ? property.types.join(" | ") : "unknown";
          const item = node("li", `${property.name} · ${types}${property.required ? " · required" : ""}`);
          if (property.description) item.append(node("p", property.description));
          properties.append(item);
        }
        article.append(properties);
        list.append(article);
      }
    });
    const unregister = slots.register("authoring.configuration", section, 30);
    context.lifecycle.defer("authoring-configuration-events", () => {
      staticGeneration++;
      load.removeEventListener("click", loadStatic);
      fingerprint.value = "";
      catalogMetadata.textContent = "";
    });
    context.lifecycle.defer("authoring-configuration-state", changed);
    context.lifecycle.defer("authoring-configuration-slot", unregister);
  },
};
