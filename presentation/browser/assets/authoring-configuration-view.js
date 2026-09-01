function node(name, text = "") {
  const value = document.createElement(name);
  value.textContent = text;
  return value;
}

function fact(list, name, value) {
  const item = node("li");
  item.dataset.field = name;
  item.append(node("strong", `${name}: `), node("code", String(value)));
  list.append(item);
}

function json(value) {
  const encoded = JSON.stringify(value);
  return encoded === undefined ? "absent" : encoded;
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
        const contract = node("ul");
        contract.dataset.role = "contract";
        fact(contract, "artifact", config.artifact);
        fact(contract, "schema status", config.schema_status);
        fact(contract, "descriptor resolved", config.resolved);
        fact(contract, "inline topology values", config.inline_topology_values);
        fact(contract, "empty object only", config.empty_object_only);
        fact(contract, "properties complete", config.properties_complete);
        if (config.schema_reference) fact(contract, "schema reference", config.schema_reference);
        if (config.schema_id) fact(contract, "schema identity", config.schema_id);
        if (config.schema_digest) fact(contract, "schema digest", config.schema_digest);
        if (Object.hasOwn(config, "additional_properties")) {
          fact(contract, "additional properties", json(config.additional_properties));
        }
        article.append(contract);
        const properties = node("ul");
        properties.dataset.role = "properties";
        for (const property of config.properties) {
          const types = Array.isArray(property.types) ? property.types.join(" | ") : "unknown";
          const item = node("li");
          item.dataset.property = property.name;
          item.append(node("h4", `${property.name} · ${types || "untyped"}${property.required ? " · required" : ""}`));
          const details = node("ul");
          fact(details, "pointer", property.pointer);
          if (property.title) fact(details, "title", property.title);
          if (property.description) item.append(node("p", property.description));
          if (property.format) fact(details, "format", property.format);
          if (Object.hasOwn(property, "default")) fact(details, "default", json(property.default));
          if (Object.hasOwn(property, "enum")) fact(details, "enum", json(property.enum));
          fact(details, "schema", json(property.schema));
          item.append(details);
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
