const root = document.getElementById("openrealtime-root");
const SHA256 = /^sha256:[0-9a-f]{64}$/;
const MAXIMUM_STATE_SNAPSHOT_BYTES = 1 << 20;

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

function normalizedJSON(value, ancestors = new Set(), budget = {nodes: 0, units: 0}, depth = 0) {
  if (++budget.nodes > MAXIMUM_STATE_SNAPSHOT_BYTES || depth > 128) {
    reject("client state snapshot exceeds its structural bound");
  }
  if (typeof value === "string") {
    budget.units += value.length;
    if (budget.units > MAXIMUM_STATE_SNAPSHOT_BYTES) {
      reject("client state snapshot exceeds its structural bound");
    }
    return value;
  }
  if (value === null || typeof value === "boolean") return value;
  if (typeof value === "number") {
    if (!Number.isFinite(value)) reject("client state snapshot contains a non-finite number");
    return Object.is(value, -0) ? 0 : value;
  }
  if (typeof value !== "object") reject("client state snapshot is not strict JSON");
  if (ancestors.has(value)) reject("client state snapshot contains a cycle");
  ancestors.add(value);
  try {
    if (Array.isArray(value)) {
      if (value.length > MAXIMUM_STATE_SNAPSHOT_BYTES) {
        reject("client state snapshot exceeds its structural bound");
      }
      const keys = Reflect.ownKeys(value);
      if (keys.length !== value.length + 1 || !keys.includes("length")) {
        reject("client state snapshot array has non-JSON properties");
      }
      const result = [];
      for (let index = 0; index < value.length; index++) {
        const descriptor = Object.getOwnPropertyDescriptor(value, String(index));
        if (!descriptor || !descriptor.enumerable || !("value" in descriptor)) {
          reject("client state snapshot array is sparse or accessor-backed");
        }
        result.push(normalizedJSON(descriptor.value, ancestors, budget, depth + 1));
      }
      return result;
    }
    const prototype = Object.getPrototypeOf(value);
    if (prototype !== Object.prototype && prototype !== null) {
      reject("client state snapshot contains a non-JSON object");
    }
    const keys = Reflect.ownKeys(value);
    if (keys.length > MAXIMUM_STATE_SNAPSHOT_BYTES || keys.some((key) => typeof key !== "string")) {
      reject("client state snapshot has non-JSON properties");
    }
    const result = Object.create(null);
    for (const key of keys.sort()) {
      budget.units += key.length;
      if (budget.units > MAXIMUM_STATE_SNAPSHOT_BYTES) {
        reject("client state snapshot exceeds its structural bound");
      }
      const descriptor = Object.getOwnPropertyDescriptor(value, key);
      if (!descriptor?.enumerable || !("value" in descriptor)) {
        reject("client state snapshot is accessor-backed or non-enumerable");
      }
      result[key] = normalizedJSON(descriptor.value, ancestors, budget, depth + 1);
    }
    return result;
  } finally {
    ancestors.delete(value);
  }
}

async function canonicalState(value) {
  const normalized = normalizedJSON(value);
  if (!normalized || typeof normalized !== "object" || Array.isArray(normalized)) {
    reject("client state snapshot must be a strict JSON object");
  }
  const bytes = new TextEncoder().encode(JSON.stringify(normalized));
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_STATE_SNAPSHOT_BYTES) {
    reject(`client state snapshot exceeds ${MAXIMUM_STATE_SNAPSHOT_BYTES} bytes`);
  }
  return { snapshot: JSON.parse(new TextDecoder().decode(bytes)), digest: await digest(bytes) };
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

class StateBoundary {
  #descriptor;
  #restored;
  #available;
  #consumed = false;
  #snapshot;
  #sealed = false;
  #api;
  constructor(descriptor, restored, available) {
    this.#descriptor = descriptor;
    this.#restored = available ? structuredClone(restored) : undefined;
    this.#available = available;
    this.#api = Object.freeze({
      restored: () => this.restored(),
      snapshot: (callback) => this.snapshot(callback),
    });
  }
  get api() { return this.#api; }
  restored() {
    if (!this.#descriptor.state_schema) throw new Error("client plugin has no state schema");
    if (this.#sealed) throw new Error("client plugin state lifecycle is sealed");
    if (this.#consumed) throw new Error("client restored state was already consumed");
    this.#consumed = true;
    return this.#available ? structuredClone(this.#restored) : undefined;
  }
  snapshot(callback) {
    if (!this.#descriptor.state_schema) throw new Error("client plugin has no state schema");
    if (this.#sealed) throw new Error("client plugin state lifecycle is sealed");
    if (!this.#descriptor.lifecycle?.snapshot) {
      throw new Error("client plugin does not declare state snapshot support");
    }
    if (typeof callback !== "function") throw new Error("client state snapshot requires a callback");
    if (this.#snapshot) throw new Error("client plugin registered more than one state snapshot");
    this.#snapshot = callback;
  }
  assertPublicationAllowed() {
    if (this.#available && !this.#consumed) {
      throw new Error("client plugin must consume restored state before publishing services");
    }
  }
  seal() {
    if (this.#sealed) throw new Error("client plugin state lifecycle was already sealed");
    this.#sealed = true;
    if (this.#available && !this.#descriptor.lifecycle?.restore) {
      throw new Error("client plugin does not declare state restore support");
    }
    if (this.#available && !this.#consumed) {
      throw new Error("client plugin did not consume restored state");
    }
    if (this.#descriptor.lifecycle?.snapshot && !this.#snapshot) {
      throw new Error("client plugin did not register a state snapshot");
    }
    return this.#snapshot;
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
  const suspendedStates = new Map();

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

  const mountOne = async (id, restoredStates) => {
    if (mounted.has(id)) return;
    const planned = entriesByID.get(id);
    if (!planned || !desired.has(id) || !requiredReady(planned)) return;
    const plugin = modules.get(id);
    const dependencies = new Map((planned.dependencies ?? []).map((binding) =>
      [binding.service.name, binding]));
    const scope = new Scope();
    const state = new StateBoundary(
      planned.descriptor, restoredStates?.get(id), Boolean(restoredStates?.has(id)));
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
      state: state.api,
      publish(name, value) {
        state.assertPublicationAllowed();
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
      const snapshot = state.seal();
      mounted.set(id, { scope, published, snapshot });
    } catch (error) {
      await scope.dispose().catch(() => {});
      states.set(id, { state: "failed", desired: desired.has(id), error: error?.message ?? String(error) });
      throw error;
    }
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

  const mountDesired = async (restoredStates) => {
    const stateInputs = new Map(suspendedStates);
    for (const [id, state] of restoredStates ?? []) {
      stateInputs.set(id, structuredClone(state));
    }
    const newlyMounted = [];
    try {
      for (const planned of manifest.plan.entries) {
        const id = planned.entry.id;
        if (!desired.has(id) || mounted.has(id) || !requiredReady(planned)) continue;
        await mountOne(id, stateInputs);
        newlyMounted.push(id);
      }
    } catch (error) {
      for (const id of newlyMounted.reverse()) await stopOne(id, "activation rollback").catch(() => {});
      throw error;
    }
    for (const id of mounted.keys()) suspendedStates.delete(id);
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
  const captureState = async (affected) => {
    const captures = new Map();
    for (const planned of manifest.plan.entries) {
      const id = planned.entry.id;
      if (!affected.has(id) || !planned.descriptor.state_schema) continue;
      if (!planned.descriptor.lifecycle?.snapshot || !planned.descriptor.lifecycle?.restore) {
        throw new Error(`client plugin ${id} requires declared snapshot and restore support`);
      }
      const instance = mounted.get(id);
      if (typeof instance?.snapshot !== "function") {
        throw new Error(`client plugin ${id} has no live state snapshot callback`);
      }
      let sealed;
      try {
        sealed = await canonicalState(await instance.snapshot());
      } catch (error) {
        throw new Error(`client plugin ${id} state snapshot failed: ${error?.message ?? String(error)}`);
      }
      captures.set(id, {
        entry: id,
        schema: structuredClone(planned.descriptor.state_schema),
        snapshot: sealed.snapshot,
        digest: sealed.digest,
        source_implementation: implementations.get(id).implementation,
      });
    }
    return captures;
  };
  const predecessorState = (captures) => new Map([...captures].map(([id, capture]) =>
    [id, structuredClone(capture.snapshot)]));
  const migrateState = async (captures, changed, nextPlugins, nextImplementations) => {
    const restored = new Map();
    const transfers = [];
    for (const [id, capture] of captures) {
      let after = {snapshot: structuredClone(capture.snapshot), digest: capture.digest};
      let migratorImplementation = "";
      if (changed.has(id)) {
        const plugin = nextPlugins.get(id);
        let raw;
        try {
          raw = await plugin.migrateState(Object.freeze({
            entry: id,
            schema: structuredClone(capture.schema),
            source_implementation: capture.source_implementation,
            snapshot: structuredClone(capture.snapshot),
          }));
          after = await canonicalState(raw);
        } catch (error) {
          throw new Error(`client plugin ${id} state migration failed: ${error?.message ?? String(error)}`);
        }
        migratorImplementation = nextImplementations.get(id).implementation;
      }
      restored.set(id, structuredClone(after.snapshot));
      transfers.push({
        entry: id,
        schema: structuredClone(capture.schema),
        before_state_digest: capture.digest,
        after_state_digest: after.digest,
        migrator_implementation: migratorImplementation,
      });
    }
    return {restored, transfers};
  };
  const deactivate = (id) => serialized(async () => {
    if (disposed) throw new Error("client composition is disposed");
    if (!entriesByID.has(id)) throw new Error(`unknown client plugin ${id}`);
    const affected = dependentClosure(id);
    const captures = await captureState(affected);
    for (const [stateID, capture] of captures) {
      suspendedStates.set(stateID, structuredClone(capture.snapshot));
    }
    desired.delete(id);
    await stopAffected(affected, `client plugin ${id} deactivation failed`);
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
  const replaceLocked = async (requestedIDs, value, single) => {
    if (disposed) throw new Error("client composition is disposed");
    if (!Array.isArray(requestedIDs) || requestedIDs.length === 0) {
      throw new Error("client replacement requires at least one entry");
    }
    const requested = new Set();
    for (const id of requestedIDs) {
      if (typeof id !== "string" || !id || id.trim() !== id || requested.has(id)) {
        throw new Error("client replacement entries must be distinct canonical strings");
      }
      const planned = entriesByID.get(id);
      if (!planned) throw new Error(`unknown client plugin ${id}`);
      if (!desired.has(id) || !mounted.has(id)) {
        throw new Error(`client plugin ${id} is not active and desired`);
      }
      requested.add(id);
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
    if (changed.length !== requested.size || changed.some((id) => !requested.has(id)) ||
        candidate.manifest.fingerprint === manifest.fingerprint) {
      throw new Error("client replacement must change exactly the requested implementations");
    }
    const changedSet = new Set(changed);
    const nextPlugins = new Map();
    for (const id of changed) {
      const planned = entriesByID.get(id);
      const nextImplementation = candidate.implementations.get(id);
      const nextAsset = candidate.assets.get(`${id}\0${nextImplementation.entrypoint}`);
      const loaded = await loadModule(nextAsset);
      const nextPlugin = loaded.default;
      if (!nextPlugin || nextPlugin.name !== planned.identity.name ||
          nextPlugin.revision !== planned.identity.revision || typeof nextPlugin.mount !== "function") {
        throw new Error(`replacement module identity mismatch for ${id}`);
      }
      if (planned.descriptor.state_schema && typeof nextPlugin.migrateState !== "function") {
        throw new Error(`client plugin ${id} requires explicit state migration`);
      }
      nextPlugins.set(id, nextPlugin);
    }

    const affected = new Set();
    for (const id of changed) {
      for (const affectedID of dependentClosure(id)) affected.add(affectedID);
    }
    const captures = await captureState(affected);
    const beforeState = predecessorState(captures);
    const beforeManifest = manifest;
    const beforeImplementations = implementations;
    const beforePlugins = new Map(changed.map((id) => [id, modules.get(id)]));
    const transitions = changed.map((id) => ({
      entry: id,
      before_implementation: structuredClone(beforeImplementations.get(id)),
      after_implementation: structuredClone(candidate.implementations.get(id)),
    }));
    const beforeSequence = sequence;
    try {
      await stopAffected(affected, "client replacement quiescence failed");
    } catch (error) {
      try {
        await mountDesired(beforeState);
        for (const id of affected) {
          if (!mounted.has(id)) throw new Error(`previous client plugin ${id} was not restored`);
        }
      } catch (restoreError) {
        throw new AggregateError([error, restoreError], "client replacement could not restore");
      }
      throw error;
    }

    let stateTransfers = [];
    try {
      const migrated = await migrateState(captures, changedSet, nextPlugins, candidate.implementations);
      stateTransfers = migrated.transfers;
      manifest = candidate.manifest;
      implementations = candidate.implementations;
      for (const [id, plugin] of nextPlugins) modules.set(id, plugin);
      root.dataset.clientManifestFingerprint = manifest.fingerprint;
      await mountDesired(migrated.restored);
      for (const id of affected) {
        if (!mounted.has(id)) throw new Error(`replacement client plugin ${id} did not activate`);
      }
    } catch (error) {
      const failures = [error];
      try { await stopAffected(affected, "client replacement candidate cleanup failed"); }
      catch (cleanupError) { failures.push(cleanupError); }
      manifest = beforeManifest;
      implementations = beforeImplementations;
      for (const [id, plugin] of beforePlugins) modules.set(id, plugin);
      root.dataset.clientManifestFingerprint = manifest.fingerprint;
      try { await mountDesired(beforeState); }
      catch (restoreError) { failures.push(restoreError); }
      for (const id of affected) {
        if (!mounted.has(id)) failures.push(new Error(`previous client plugin ${id} was not restored`));
      }
      throw new AggregateError(failures, "client replacement failed and was rolled back");
    }
    sequence++;
    const receipt = {
      format_version: 1,
      plan_fingerprint: manifest.plan.fingerprint,
      before_manifest_fingerprint: beforeManifest.fingerprint,
      after_manifest_fingerprint: manifest.fingerprint,
      before_sequence: beforeSequence,
      after_sequence: sequence,
    };
    if (single && stateTransfers.length === 0) {
      return Object.freeze({ ...receipt, ...transitions[0] });
    }
    if (!single && stateTransfers.length === 0) return Object.freeze({
      ...receipt,
      format_version: 2,
      transitions: Object.freeze(transitions.map((row) => Object.freeze(row))),
    });
    const stateful = single ? {...receipt, ...transitions[0]} : {
      ...receipt, transitions: Object.freeze(transitions.map((row) => Object.freeze(row))),
    };
    return Object.freeze({
      ...stateful,
      format_version: 3,
      state_transfers: Object.freeze(stateTransfers.map((row) => Object.freeze(row))),
    });
  };
  const replace = (id, value) => serialized(() => replaceLocked([id], value, true));
  const replaceMany = (ids, value) => serialized(() => replaceLocked(ids, value, false));
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
    suspendedStates.clear();
    root.dataset.state = "disposed";
    if (failures.length) throw new AggregateError(failures, "client disposal failed");
  });
  window.__openrealtime = Object.freeze({
    get manifest() { return structuredClone(manifest); },
    get mounted() { return manifest.plan.entries.map((row) => row.entry.id).filter((id) => mounted.has(id)); },
    live, activate, deactivate, replace, replaceMany, dispose,
  });
  addEventListener("beforeunload", () => { dispose().catch(() => {}); }, { once: true });
}

boot().catch((error) => fail(error?.message ?? String(error)));
