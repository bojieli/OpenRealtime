const MAX_REFERENCES = 256;
const MAX_ARTIFACT_BYTES = 8 << 20;
const MAX_DOWNLOAD_BYTES = 32 << 20;
const encoder = new TextEncoder();
const bytes = (value) => encoder.encode(value).length;
const isObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
const idPattern = /^[A-Za-z0-9_-]{1,64}$/;
const digestPattern = /^sha256:[0-9a-f]{64}$/;

function onlyKeys(value, allowed, label) {
  if (!isObject(value)) throw new Error(`${label} must be an object`);
  for (const key of Object.keys(value)) {
    if (!allowed.includes(key)) throw new Error(`${label} contains unknown field ${key}`);
  }
}

function text(value, label, maximum, allowEmpty = false) {
  if (typeof value !== "string" || (!allowEmpty && !value) || bytes(value) > maximum || value.trim() !== value) {
    throw new Error(`${label} is not canonical bounded text`);
  }
  return value;
}

function common(value, kind, maximumBytes) {
  if (!idPattern.test(value.id ?? "") || value.path !== `/client/v1/${kind}s/${value.id}` ||
      !digestPattern.test(value.digest ?? "") || !Number.isSafeInteger(value.bytes) ||
      value.bytes < 1 || value.bytes > maximumBytes || !Number.isSafeInteger(value.version) ||
      value.version < 1 || typeof value.updated_at !== "string" || value.updated_at.length > 64 ||
      !Number.isFinite(Date.parse(value.updated_at))) {
    throw new Error(`${kind} reference is not canonical or exceeds its bound`);
  }
}

function artifact(value) {
  onlyKeys(value, ["id", "title", "path", "digest", "bytes", "version", "updated_at"], "artifact reference");
  common(value, "artifact", MAX_ARTIFACT_BYTES);
  text(value.title, "artifact title", 512);
  return Object.freeze(structuredClone(value));
}

function download(value) {
  onlyKeys(value, ["id", "filename", "media_type", "path", "digest", "bytes", "version", "updated_at"], "download reference");
  common(value, "download", MAX_DOWNLOAD_BYTES);
  const filename = text(value.filename, "download filename", 255);
  if (filename === "." || filename === ".." || /[\\/\p{Cc}\p{Cf}]/u.test(filename)) {
    throw new Error("download filename is unsafe");
  }
  const mediaType = text(value.media_type, "download media type", 255);
  if (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+\/[!#$%&'*+.^_`|~0-9A-Za-z-]+(?:\s*;.*)?$/.test(mediaType) || mediaType.includes("*")) {
    throw new Error("download media type is invalid");
  }
  return Object.freeze(structuredClone(value));
}

function revise(map, order, reference) {
  const prior = map.get(reference.id);
  if (prior && reference.version <= prior.version) {
    throw new Error(`resource ${reference.id} did not advance its version`);
  }
  if (!prior && map.size >= MAX_REFERENCES) {
    const oldest = order.shift();
    map.delete(oldest);
  }
  if (prior) order.splice(order.indexOf(reference.id), 1);
  map.set(reference.id, reference);
  order.push(reference.id);
}

export default {
  name: "openrealtime.presentation.client.artifact-references",
  revision: 1,
  async mount(context) {
    const effects = context.services.get("presentation.client.tools_effects");
    if (!effects) throw new Error("artifact references require the effect client");
    const artifacts = new Map();
    const downloads = new Map();
    const artifactOrder = [];
    const downloadOrder = [];
    const listeners = new Set();
    let diagnostic = "";
    let disposed = false;

    const snapshot = () => Object.freeze({
      artifacts: artifactOrder.map((id) => structuredClone(artifacts.get(id))),
      downloads: downloadOrder.map((id) => structuredClone(downloads.get(id))),
      diagnostic,
    });
    const publish = () => {
      const current = snapshot();
      for (const listener of listeners) {
        try { listener(current); } catch {}
      }
    };
    const offEffects = effects.subscribe((_snapshot, event) => {
      if (disposed || event?.type !== "result" || event.error) return;
      try {
        if (event.artifact !== undefined) revise(artifacts, artifactOrder, artifact(event.artifact));
        if (event.download !== undefined) revise(downloads, downloadOrder, download(event.download));
        diagnostic = "";
      } catch (error) {
        diagnostic = error?.message ?? "invalid resource reference";
      }
      publish();
    });
    const service = Object.freeze({
      snapshot,
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("artifact listener must be a function");
        listeners.add(listener);
        try { listener(snapshot()); } catch {}
        return () => listeners.delete(listener);
      },
    });
    context.publish("presentation.client.artifacts", service);
    context.lifecycle.defer("artifact-references", () => {
      disposed = true;
      offEffects();
      listeners.clear();
      artifacts.clear();
      downloads.clear();
      artifactOrder.length = 0;
      downloadOrder.length = 0;
    });
  },
};
