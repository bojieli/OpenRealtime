const encoder = new TextEncoder();
const MAX_SOURCE_BYTES = 1 << 20;
const DIGEST = /^sha256:[0-9a-f]{64}$/;

function frozen(value) {
  const copy = structuredClone(value);
  const visit = (entry) => {
    if (!entry || typeof entry !== "object" || Object.isFrozen(entry)) return entry;
    for (const child of Object.values(entry)) visit(child);
    return Object.freeze(entry);
  };
  return visit(copy);
}

function checkedDocument(path, source, revision = 1) {
  if (typeof path !== "string" || path.length === 0 || path.length > 4096 || path.trim() !== path ||
      /[\0\r\n]/.test(path) || !/\.(?:ortg|ya?ml|json)$/i.test(path) ||
      typeof source !== "string" || encoder.encode(source).byteLength === 0 ||
      encoder.encode(source).byteLength > MAX_SOURCE_BYTES ||
      !Number.isSafeInteger(revision) || revision < 1) {
    throw new Error("authoring workspace document is invalid");
  }
  return Object.freeze({ path, source, revision });
}

export default {
  name: "openrealtime.presentation.client.authoring-workspace",
  revision: 1,
  async mount(context) {
    const authoring = context.services.get("presentation.client.management_authoring");
    const editing = context.services.get("presentation.client.management_editing");
    const sourceReading = context.services.get("presentation.client.source_reading");
    const sourcePublication = context.services.get("presentation.client.source_publication");
    if (!authoring || !editing) throw new Error("authoring workspace services are unavailable");
    let disposed = false;
    let epoch = 0;
    let request = 0;
    let state = {
      document: checkedDocument("agent.ortg", "graph agent {\n}\n"),
      phase: "idle", error: "", sourceRead: null,
      analysis: null, compiled: null, rendering: null, publication: null,
    };
    const listeners = new Set();
    const snapshot = () => frozen({ ...state, epoch });
    const notify = () => {
      const projection = snapshot();
      for (const listener of listeners) {
        try { listener(projection); } catch {}
      }
    };
    const replace = (next) => {
      state = next;
      notify();
      return snapshot();
    };
    const ready = () => {
      if (disposed) throw new Error("authoring workspace is disposed");
    };
    const invoke = async (phase, operation, apply) => {
      ready();
      const expectedEpoch = epoch;
      const expectedRequest = ++request;
      replace({ ...state, phase, error: "" });
      try {
        const result = await operation();
        ready();
        if (epoch !== expectedEpoch || request !== expectedRequest) {
          throw new Error("authoring document changed during request");
        }
        return replace(apply(state, result));
      } catch (error) {
        if (!disposed && epoch === expectedEpoch && request === expectedRequest) {
          replace({ ...state, phase: "error", error: error?.message ?? String(error) });
        }
        throw error;
      }
    };

    context.publish("presentation.client.authoring_workspace", Object.freeze({
      snapshot,
      canRead: () => !disposed && Boolean(sourceReading),
      canPublish: () => !disposed && Boolean(sourcePublication),
      subscribe(listener) {
        ready();
        if (typeof listener !== "function") throw new Error("workspace listener is invalid");
        listeners.add(listener);
        try { listener(snapshot()); } catch {}
        return () => listeners.delete(listener);
      },
      setDocument(path, source, revision = 1) {
        ready();
        epoch++;
        request++;
        return replace({ document: checkedDocument(path, source, revision), phase: "idle", error: "",
          sourceRead: null, analysis: null, compiled: null, rendering: null, publication: null });
      },
      load(rootIdentity, path) {
        ready();
        if (!sourceReading || typeof sourceReading.read !== "function") {
          throw new Error("source reading is unavailable in this client profile");
        }
        const revision = state.document.revision;
        return invoke("reading", () => sourceReading.read({
          format_version: 1, root_identity: rootIdentity, path,
        }), (_current, result) => {
          const document = checkedDocument(result.path, result.source, revision);
          const { source: _source, ...sourceRead } = result;
          epoch++;
          return { document, phase: "loaded", error: "", sourceRead,
            analysis: null, compiled: null, rendering: null, publication: null };
        });
      },
      analyze() {
        const input = state.document;
        return invoke("analyzing", () => authoring.analyze(input), (current, analysis) => ({
          ...current, phase: "analyzed", error: "", analysis,
        }));
      },
      format() {
        ready();
        const input = state.document;
        const analysis = state.analysis;
        if (!analysis?.parsed || analysis.recovered || analysis.canonical || !analysis.formatting ||
            typeof editing.applyEdits !== "function") {
          throw new Error("authoring workspace has no applicable formatter edit");
        }
        return invoke("formatting", () => editing.applyEdits(input, analysis.formatting),
          (_current, source) => {
            const document = checkedDocument(input.path, source, input.revision);
            epoch++;
            return { document, phase: "formatted", error: "", sourceRead: null, analysis: null,
              compiled: null, rendering: null, publication: null };
          });
      },
      renameNode(expectedFingerprint, selected, replacement) {
        ready();
        const input = state.document;
        const graph = state.compiled?.graph;
        if (typeof expectedFingerprint !== "string" || !DIGEST.test(expectedFingerprint) ||
            !graph || graph.fingerprint !== expectedFingerprint || graph.revision !== input.revision ||
            typeof selected !== "string" || !graph.nodes?.some((node) => node.id === selected)) {
          throw new Error("authoring workspace node selection is stale");
        }
        if (typeof replacement !== "string" || replacement === selected) {
          throw new Error("authoring workspace node rename is not a mutation");
        }
        if (graph.nodes.some((node) => node.id === replacement)) {
          throw new Error("authoring workspace node rename target already exists");
        }
        if (input.revision >= Number.MAX_SAFE_INTEGER) {
          throw new Error("authoring workspace document revision is exhausted");
        }
        return invoke("renaming", async () => {
          const result = await editing.rename(input, selected, replacement);
          const source = await editing.applyEdits(input, result.edits);
          return Object.freeze({ result, source });
        }, (current, renamed) => {
          if (current.compiled?.graph?.fingerprint !== expectedFingerprint) {
            throw new Error("authoring workspace node selection changed during rename");
          }
          const document = checkedDocument(input.path, renamed.source, input.revision + 1);
          epoch++;
          return { document, phase: "renamed", error: "", sourceRead: null, analysis: null,
            compiled: null, rendering: null, publication: null };
        });
      },
      removeEdge(expectedFingerprint, selected) {
        ready();
        const input = state.document;
        const graph = state.compiled?.graph;
        if (typeof expectedFingerprint !== "string" || !DIGEST.test(expectedFingerprint) ||
            !graph || graph.fingerprint !== expectedFingerprint || graph.revision !== input.revision ||
            typeof selected !== "string" || !graph.edges?.some((edge) => edge.id === selected)) {
          throw new Error("authoring workspace edge selection is stale");
        }
        if (input.revision >= Number.MAX_SAFE_INTEGER) {
          throw new Error("authoring workspace document revision is exhausted");
        }
        return invoke("removing-edge", async () => {
          const result = await editing.removeEdge(input, selected);
          const source = await editing.applyEdits(input, result.edits);
          return Object.freeze({ result, source });
        }, (current, removed) => {
          if (current.compiled?.graph?.fingerprint !== expectedFingerprint ||
              !current.compiled.graph.edges?.some((edge) => edge.id === selected)) {
            throw new Error("authoring workspace edge selection changed during removal");
          }
          const document = checkedDocument(input.path, removed.source, input.revision + 1);
          epoch++;
          return { document, phase: "edge-removed", error: "", sourceRead: null, analysis: null,
            compiled: null, rendering: null, publication: null };
        });
      },
      compile() {
        const input = state.document;
        return invoke("compiling", () => authoring.compile(input), (current, compiled) => ({
          ...current, phase: "compiled", error: "", compiled, rendering: null,
        }));
      },
      render(format = "model") {
        ready();
        const graph = state.compiled?.graph;
        if (!graph) throw new Error("authoring workspace has no compiled graph");
        return invoke("rendering", () => authoring.render(graph, format), (current, rendering) => ({
          ...current, phase: "rendered", error: "", rendering,
        }));
      },
      publish(mode, rootIdentity, expectedSourceDigest = "") {
        ready();
        if (!sourcePublication || typeof sourcePublication.publish !== "function") {
          throw new Error("source publication is unavailable in this client profile");
        }
        const input = state.document;
        const publicationRequest = {
          format_version: 1, root_identity: rootIdentity, mode,
          path: input.path, source: input.source,
        };
        if (mode === "update") publicationRequest.expected_source_digest = expectedSourceDigest;
        return invoke("publishing", () => sourcePublication.publish(publicationRequest),
          (current, publication) => ({
            ...current, phase: "published", error: "", publication,
          }));
      },
    }));
    context.lifecycle.defer("authoring-workspace", () => {
      disposed = true;
      epoch++;
      request++;
      listeners.clear();
      state = { document: Object.freeze({ path: "", source: "", revision: 0 }), phase: "disposed",
        error: "", sourceRead: null, analysis: null, compiled: null, rendering: null, publication: null };
    });
  },
};
