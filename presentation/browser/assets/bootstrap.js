const root = document.getElementById("openrealtime-root");
const SHA256 = /^sha256:[0-9a-f]{64}$/;

function fail(message) {
  root.textContent = `Client failed to start: ${message}`;
  root.dataset.state = "failed";
  throw new Error(message);
}

function reject(message) {
  throw new Error(message);
}

function only(value, fields, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      Object.keys(value).some((field) => !fields.includes(field))) {
    reject(`${label} has unknown or invalid fields`);
  }
}

function hex(bytes) {
  return [...new Uint8Array(bytes)].map((value) => value.toString(16).padStart(2, "0")).join("");
}

async function digest(bytes) {
  return `sha256:${hex(await crypto.subtle.digest("SHA-256", bytes))}`;
}

function permissionSet(manifest, entry) {
  const grants = manifest.grants?.find((row) => row.entry === entry)?.permissions ?? [];
  return Object.freeze({
    allows(kind, resource, operation) {
      return grants.some((grant) => grant.kind === kind && grant.resource === resource &&
        grant.operations.includes(operation));
    },
    snapshot() { return structuredClone(grants); },
  });
}

function validateManifest(manifest) {
  if (!manifest || typeof manifest !== "object" || manifest.format_version !== 1 ||
      manifest.platform !== "browser" || manifest.plan?.realm !== "client" ||
      !Array.isArray(manifest.plan.entries) || !Array.isArray(manifest.implementations) ||
      !Array.isArray(manifest.assets ?? []) || !Array.isArray(manifest.endpoints ?? []) ||
      !Array.isArray(manifest.grants ?? [])) {
    reject("unsupported or non-browser manifest");
  }
  only(manifest, ["format_version", "platform", "fingerprint", "plan", "implementations",
    "assets", "endpoints", "grants"], "client manifest");
  const entries = new Map(manifest.plan.entries.map((entry) => [entry.entry.id, entry]));
  if (entries.size !== manifest.plan.entries.length) reject("duplicate plan entry");
  const implementations = new Map();
  for (const implementation of manifest.implementations) {
    only(implementation, ["entry", "implementation", "artifact", "entrypoint"],
      "client implementation");
    only(implementation.artifact, ["id", "revision", "digest"], "client implementation artifact");
    if (!entries.has(implementation.entry) || implementations.has(implementation.entry) ||
        typeof implementation.implementation !== "string" || !implementation.implementation ||
        implementation.implementation.trim() !== implementation.implementation ||
        typeof implementation.entrypoint !== "string" || !implementation.entrypoint ||
        typeof implementation.artifact?.id !== "string" || !implementation.artifact.id ||
        implementation.artifact.id.trim() !== implementation.artifact.id ||
        (implementation.artifact.revision !== undefined &&
          (typeof implementation.artifact.revision !== "string" ||
            implementation.artifact.revision.trim() !== implementation.artifact.revision)) ||
        !SHA256.test(implementation.artifact.digest)) reject("invalid implementation map");
    implementations.set(implementation.entry, implementation);
  }
  if (implementations.size !== entries.size) reject("incomplete implementation map");
  const assets = new Map();
  for (const asset of manifest.assets ?? []) {
    only(asset, ["entry", "name", "media_type", "digest", "path"], "client asset");
    const key = `${asset.entry}\0${asset.name}`;
    if (assets.has(key) || !entries.has(asset.entry) ||
        typeof asset.name !== "string" || !asset.name ||
        typeof asset.media_type !== "string" || !asset.media_type ||
        !SHA256.test(asset.digest) ||
        asset.path !== `/client/v1/modules/${asset.digest.replace("sha256:", "")}`) {
      reject("invalid content-addressed asset map");
    }
    const declared = entries.get(asset.entry).descriptor.assets?.find((candidate) => candidate.name === asset.name);
    if (!declared || declared.digest !== asset.digest || declared.media_type !== asset.media_type) {
      reject("asset differs from its descriptor");
    }
    assets.set(key, asset);
  }
  for (const [entry, planned] of entries) {
    for (const declared of planned.descriptor.assets ?? []) {
      if (!assets.has(`${entry}\0${declared.name}`)) reject("manifest omits a descriptor asset");
    }
  }
  for (const [entry, implementation] of implementations) {
    const asset = assets.get(`${entry}\0${implementation.entrypoint}`);
    if (!asset || implementation.artifact.digest !== asset.digest ||
        !["text/javascript", "application/javascript"].includes(asset.media_type)) {
      reject("entrypoint is not the exact implementation asset");
    }
  }
  for (const row of manifest.grants ?? []) {
    const planned = entries.get(row.entry);
    if (!planned || !Array.isArray(row.permissions)) reject("permission grant names an absent entry");
    for (const grant of row.permissions) {
      const ceiling = planned.descriptor.permissions?.find((candidate) =>
        candidate.kind === grant.kind && candidate.resource === grant.resource);
      if (!ceiling || ceiling.authority !== grant.authority || !grant.operations?.length ||
          grant.operations.some((operation) => !ceiling.operations.includes(operation))) {
        reject("permission grant exceeds its descriptor ceiling");
      }
    }
  }
  return { entries, implementations, assets };
}

async function verifiedManifest(value) {
  const manifest = structuredClone(value);
  const claimedFingerprint = manifest.fingerprint;
  const fingerprintInput = structuredClone(manifest);
  fingerprintInput.fingerprint = "";
  const actualFingerprint = await digest(new TextEncoder().encode(JSON.stringify(fingerprintInput)));
  if (actualFingerprint !== claimedFingerprint) reject("manifest fingerprint verification failed");
  return { manifest, ...validateManifest(manifest) };
}

async function loadModule(asset) {
  const response = await fetch(asset.path, { cache: "no-store", credentials: "same-origin" });
  if (!response.ok) reject(`module ${asset.name} returned ${response.status}`);
  const bytes = await response.arrayBuffer();
  if (await digest(bytes) !== asset.digest) reject(`module ${asset.name} failed digest verification`);
  const moduleURL = URL.createObjectURL(new Blob([bytes], { type: asset.media_type }));
  try {
    return await import(moduleURL);
  } finally {
    URL.revokeObjectURL(moduleURL);
  }
}

class Scope {
  #disposed = false;
  #effects = [];
  get effectCount() { return this.#effects.length; }
  defer(name, dispose) {
    if (this.#disposed || !name || typeof dispose !== "function") throw new Error("invalid scoped effect");
    if (this.#effects.some((effect) => effect.name === name)) throw new Error(`duplicate effect ${name}`);
    this.#effects.push({ name, dispose });
  }
  async dispose() {
    if (this.#disposed) return;
    this.#disposed = true;
    const failures = [];
    for (const effect of this.#effects.reverse()) {
      try { await effect.dispose(); } catch (error) { failures.push(error); }
    }
    this.#effects = [];
    if (failures.length) throw new AggregateError(failures, "client plugin disposal failed");
  }
}

async function boot() {
  const response = await fetch("/client/v1/manifest", { cache: "no-store", credentials: "same-origin" });
  if (!response.ok) reject(`manifest returned ${response.status}`);
  const verified = await verifiedManifest(await response.json());
  let manifest = verified.manifest;
  const entries = verified.entries;
  let implementations = verified.implementations;
  const assets = verified.assets;
  const services = new Map();
  const modules = new Map();
  const entriesByID = new Map(manifest.plan.entries.map((planned) => [planned.entry.id, planned]));
  const states = new Map(manifest.plan.entries.map((planned) => [planned.entry.id, {
    state: "pending", desired: true, error: "",
  }]));
  const desired = new Set(entriesByID.keys());
  const mounted = new Map();

  // Resolve and authenticate every implementation before any module is
  // allowed to start an effect. A missing or substituted later module cannot
  // leave half a client mounted.
  for (const planned of manifest.plan.entries) {
    const id = planned.entry.id;
    const implementation = implementations.get(id);
    const asset = assets.get(`${id}\0${implementation.entrypoint}`);
    const loaded = await loadModule(asset);
    const plugin = loaded.default;
    if (!plugin || plugin.name !== planned.identity.name || plugin.revision !== planned.identity.revision ||
        typeof plugin.mount !== "function") reject(`module identity mismatch for ${id}`);
    modules.set(id, plugin);
  }

  const requiredReady = (planned) => (planned.dependencies ?? []).every((binding) =>
    binding.optional || services.has(`${binding.provider}\0${binding.service.name}`));

  const mountOne = async (id) => {
    if (mounted.has(id)) return;
    const planned = entriesByID.get(id);
    if (!planned || !desired.has(id) || !requiredReady(planned)) return;
    const plugin = modules.get(id);
    const dependencies = new Map((planned.dependencies ?? []).map((binding) =>
      [binding.service.name, binding]));
    const scope = new Scope();
    const published = new Set();
    states.set(id, { state: "mounting", desired: true, error: "" });
    const context = Object.freeze({
      entry: id,
      descriptor: structuredClone(planned.descriptor),
      root,
      manifest: structuredClone(manifest),
      services: Object.freeze({
        get(name) {
          const binding = dependencies.get(name);
          if (!binding) return undefined;
          const record = services.get(`${binding.provider}\0${binding.service.name}`);
          if (record && JSON.stringify(record.contract) !== JSON.stringify(binding.service)) {
            throw new Error(`service contract mismatch for ${name}`);
          }
          return record?.value;
        },
        has(name) {
          const binding = dependencies.get(name);
          return Boolean(binding && services.has(`${binding.provider}\0${binding.service.name}`));
        },
      }),
      permissions: permissionSet(manifest, id),
      publish(name, value) {
        const contract = planned.descriptor.provides?.find((candidate) => candidate.name === name);
        if (!contract || value == null || published.has(name)) throw new Error(`invalid service publication ${name}`);
        const key = `${id}\0${name}`;
        if (services.has(key)) throw new Error(`service ${name} is already published by ${id}`);
        const record = { contract, value, scope };
        services.set(key, record);
        published.add(name);
        scope.defer(`service:${name}`, () => {
          if (services.get(key) === record) services.delete(key);
        });
      },
      lifecycle: Object.freeze({ defer: (name, dispose) => scope.defer(name, dispose) }),
    });
    try {
      await plugin.mount(context);
      for (const contract of planned.descriptor.provides ?? []) {
        if (!published.has(contract.name)) throw new Error(`plugin ${id} omitted service ${contract.name}`);
      }
    } catch (error) {
      await scope.dispose().catch(() => {});
      states.set(id, { state: "failed", desired: desired.has(id), error: error?.message ?? String(error) });
      throw error;
    }
    mounted.set(id, { scope, published });
    states.set(id, { state: "active", desired: true, error: "" });
  };

  const stopOne = async (id, cause = "deactivated") => {
    const instance = mounted.get(id);
    if (!instance) {
      const row = states.get(id);
      states.set(id, { state: desired.has(id) ? "pending" : "inactive", desired: desired.has(id), error: row?.error ?? "" });
      return;
    }
    mounted.delete(id);
    states.set(id, { state: "stopping", desired: desired.has(id), error: "" });
    try {
      await instance.scope.dispose();
      states.set(id, { state: desired.has(id) ? "pending" : "inactive", desired: desired.has(id), error: "" });
    } catch (error) {
      states.set(id, { state: "failed", desired: desired.has(id), error: `${cause}: ${error?.message ?? String(error)}` });
      throw error;
    }
  };

  const mountDesired = async () => {
    const newlyMounted = [];
    try {
      for (const planned of manifest.plan.entries) {
        const id = planned.entry.id;
        if (!desired.has(id) || mounted.has(id) || !requiredReady(planned)) continue;
        await mountOne(id);
        newlyMounted.push(id);
      }
    } catch (error) {
      for (const id of newlyMounted.reverse()) await stopOne(id, "activation rollback").catch(() => {});
      throw error;
    }
    return newlyMounted;
  };

  await mountDesired();
  if (mounted.size !== manifest.plan.entries.length) {
    for (const planned of [...manifest.plan.entries].reverse()) await stopOne(planned.entry.id, "boot rollback").catch(() => {});
    reject("client plan has unresolved required services");
  }
  root.dataset.state = "ready";
  root.dataset.clientFingerprint = manifest.plan.fingerprint;
  root.dataset.clientManifestFingerprint = manifest.fingerprint;
  let disposed = false;
  let sequence = 1;
  let lifecycle = Promise.resolve();
  const serialized = (operation) => {
    const next = lifecycle.then(operation, operation);
    lifecycle = next.catch(() => {});
    return next;
  };
  const dependentClosure = (id) => {
    const affected = new Set([id]);
    let changed = true;
    while (changed) {
      changed = false;
      for (const planned of manifest.plan.entries) {
        if (affected.has(planned.entry.id) || !(planned.dependencies ?? []).some(
          (binding) => affected.has(binding.provider))) continue;
        affected.add(planned.entry.id);
        changed = true;
      }
    }
    return affected;
  };
  const stopAffected = async (affected, cause) => {
    const failures = [];
    for (const planned of [...manifest.plan.entries].reverse()) {
      if (!affected.has(planned.entry.id)) continue;
      try { await stopOne(planned.entry.id, cause); }
      catch (error) { failures.push(error); }
    }
    if (failures.length) throw new AggregateError(failures, cause);
  };
  const deactivate = (id) => serialized(async () => {
    if (disposed) throw new Error("client composition is disposed");
    if (!entriesByID.has(id)) throw new Error(`unknown client plugin ${id}`);
    desired.delete(id);
    await stopAffected(dependentClosure(id), `client plugin ${id} deactivation failed`);
    sequence++;
    return live();
  });
  const activate = (id) => serialized(async () => {
    if (disposed) throw new Error("client composition is disposed");
    if (!entriesByID.has(id)) throw new Error(`unknown client plugin ${id}`);
    desired.add(id);
    await mountDesired();
    if (!mounted.has(id)) throw new Error(`client plugin ${id} has unavailable required services`);
    sequence++;
    return live();
  });
  const replace = (id, value) => serialized(async () => {
    if (disposed) throw new Error("client composition is disposed");
    const planned = entriesByID.get(id);
    if (!planned) throw new Error(`unknown client plugin ${id}`);
    if (!desired.has(id) || !mounted.has(id)) {
      throw new Error(`client plugin ${id} is not active and desired`);
    }
    if (planned.descriptor.state_schema) {
      throw new Error(`client plugin ${id} requires explicit state migration`);
    }

    const candidate = await verifiedManifest(value);
    for (const field of ["format_version", "platform", "plan", "assets", "endpoints", "grants"]) {
      if (JSON.stringify(candidate.manifest[field] ?? null) !== JSON.stringify(manifest[field] ?? null)) {
        throw new Error(`client replacement changes immutable manifest field ${field}`);
      }
    }
    const changed = [];
    for (const entryID of entriesByID.keys()) {
      const before = implementations.get(entryID);
      const after = candidate.implementations.get(entryID);
      if (JSON.stringify(before) !== JSON.stringify(after)) changed.push(entryID);
    }
    if (changed.length !== 1 || changed[0] !== id || candidate.manifest.fingerprint === manifest.fingerprint) {
      throw new Error(`client replacement must change exactly implementation ${id}`);
    }
    const nextImplementation = candidate.implementations.get(id);
    const nextAsset = candidate.assets.get(`${id}\0${nextImplementation.entrypoint}`);
    const loaded = await loadModule(nextAsset);
    const nextPlugin = loaded.default;
    if (!nextPlugin || nextPlugin.name !== planned.identity.name ||
        nextPlugin.revision !== planned.identity.revision || typeof nextPlugin.mount !== "function") {
      throw new Error(`replacement module identity mismatch for ${id}`);
    }

    const affected = dependentClosure(id);
    const beforeManifest = manifest;
    const beforeImplementations = implementations;
    const beforePlugin = modules.get(id);
    const beforeImplementation = structuredClone(beforeImplementations.get(id));
    const beforeSequence = sequence;
    try {
      await stopAffected(affected, `client plugin ${id} replacement quiescence failed`);
    } catch (error) {
      try { await mountDesired(); }
      catch (restoreError) {
        throw new AggregateError([error, restoreError], `client plugin ${id} replacement could not restore`);
      }
      throw error;
    }

    manifest = candidate.manifest;
    implementations = candidate.implementations;
    modules.set(id, nextPlugin);
    root.dataset.clientManifestFingerprint = manifest.fingerprint;
    try {
      await mountDesired();
      if (!mounted.has(id)) throw new Error(`replacement client plugin ${id} did not activate`);
    } catch (error) {
      const failures = [error];
      try { await stopAffected(affected, `client plugin ${id} candidate cleanup failed`); }
      catch (cleanupError) { failures.push(cleanupError); }
      manifest = beforeManifest;
      implementations = beforeImplementations;
      modules.set(id, beforePlugin);
      root.dataset.clientManifestFingerprint = manifest.fingerprint;
      try { await mountDesired(); }
      catch (restoreError) { failures.push(restoreError); }
      if (!mounted.has(id)) failures.push(new Error(`previous client plugin ${id} was not restored`));
      throw new AggregateError(failures, `client plugin ${id} replacement failed and was rolled back`);
    }
    sequence++;
    return Object.freeze({
      format_version: 1,
      plan_fingerprint: manifest.plan.fingerprint,
      before_manifest_fingerprint: beforeManifest.fingerprint,
      after_manifest_fingerprint: manifest.fingerprint,
      before_sequence: beforeSequence,
      after_sequence: sequence,
      entry: id,
      before_implementation: beforeImplementation,
      after_implementation: structuredClone(implementations.get(id)),
    });
  });
  const live = () => Object.freeze({
    format_version: 1,
    fingerprint: manifest.plan.fingerprint,
    manifest_fingerprint: manifest.fingerprint,
    sequence,
    state: disposed ? "closed" : "active",
    entries: Object.fromEntries(manifest.plan.entries.map((planned) => {
      const id = planned.entry.id;
      const row = states.get(id);
      return [id, {
        state: row.state, desired: desired.has(id), error: row.error,
        effects: mounted.get(id)?.scope.effectCount ?? 0,
        services: [...(mounted.get(id)?.published ?? [])].sort(),
        implementation: implementations.get(id).implementation,
        artifact: structuredClone(implementations.get(id).artifact),
      }];
    })),
  });
  const dispose = () => serialized(async () => {
    if (disposed) return;
    disposed = true;
    desired.clear();
    const failures = [];
    for (const planned of [...manifest.plan.entries].reverse()) {
      try { await stopOne(planned.entry.id, "client disposal"); } catch (error) { failures.push(error); }
    }
    root.dataset.state = "disposed";
    if (failures.length) throw new AggregateError(failures, "client disposal failed");
  });
  window.__openrealtime = Object.freeze({
    get manifest() { return structuredClone(manifest); },
    get mounted() { return manifest.plan.entries.map((row) => row.entry.id).filter((id) => mounted.has(id)); },
    live, activate, deactivate, replace, dispose,
  });
  addEventListener("beforeunload", () => { dispose().catch(() => {}); }, { once: true });
}

boot().catch((error) => fail(error?.message ?? String(error)));
