const encoder = new TextEncoder();

export const SCHEMA_IDENTITY = "openrealtime/presentation/client-performance-evidence/v1";
export const CONFIGURATION_SCHEMA_IDENTITY = "openrealtime/presentation/client-performance-evidence-config/v1";
export const SNAPSHOT_FORMAT_VERSION = 1;
export const CONFIGURATION_VERSION = 1;

export const MAX_SOURCES = 128;
export const MAX_SERIES = 256;
export const MAX_DIMENSIONS_PER_SERIES = 4;
export const MAX_PENDING_SPANS = 256;
export const MAX_OBSERVATIONS_PER_SERIES = 4096;
export const MAX_SNAPSHOT_BYTES = 256 << 10;
export const MAX_DURATION_NS = 86_400_000_000_000n;

const MAX_U64 = (1n << 64n) - 1n;
const MAX_I64 = (1n << 63n) - 1n;
const MIN_I64 = -(1n << 63n);

export const DEFAULT_LIMITS = Object.freeze({
  max_sources: MAX_SOURCES,
  max_series: MAX_SERIES,
  max_dimensions_per_series: MAX_DIMENSIONS_PER_SERIES,
  max_pending_spans: MAX_PENDING_SPANS,
  max_observations_per_series: MAX_OBSERVATIONS_PER_SERIES,
  max_artifact_bytes: MAX_SNAPSHOT_BYTES,
});

export const OBSERVABILITY_MODES = Object.freeze({
  disabled: "disabled",
  sampled: "sampled",
  release: "release",
});

export const EVIDENCE_CLASSES = Object.freeze({
  deterministicFixture: "deterministic_fixture",
  browserRuntime: "browser_runtime",
  macOSNative: "macos_native",
});

export const PLATFORMS = Object.freeze({headless: "headless", browser: "browser", macOS: "macos"});

export const CLOCK_DOMAINS = Object.freeze({
  virtualInteger: "virtual_integer_ns",
  processMonotonic: "process_monotonic_ns",
  browserMonotonic: "browser_monotonic_ns",
  machContinuous: "mach_continuous_ns",
});

export const CLOCK_PROVENANCE = Object.freeze({
  fixture: "deterministic_fixture",
  goMonotonic: "go_monotonic",
  browserAPI: "browser_performance_api",
  machClock: "mach_continuous_time",
  synchronized: "bounded_cross_host_sync",
});

export const FAMILIES = Object.freeze({duration: "duration_ns", counter: "counter", gauge: "gauge"});

export const METRICS = Object.freeze({
  endpointToFirstRenderedAudio: "endpoint_to_first_rendered_audio_ns",
  serverOutputToRenderedAudio: "server_output_to_rendered_audio_ns",
  audioScheduled: "audio_scheduled_ns",
  audioCompleted: "audio_completed_ns",
  videoCaptureToAdmissionAck: "video_capture_to_admission_ack_ns",
  reconnectDetection: "reconnect_detection_ns",
  reconnectRecovery: "reconnect_recovery_ns",
  reducerToViewCommit: "reducer_publish_to_view_commit_ns",
  longTask: "long_task_ns",
  mainLoopStall: "main_loop_stall_ns",
  liveSnapshotRender: "live_snapshot_render_ns",
  deltaRender: "delta_render_ns",
  traceRender: "trace_render_ns",
  audioGap: "audio_gap_count",
  audioUnderflow: "audio_underflow_count",
  audioOverrun: "audio_overrun_count",
  audioCaptureDrop: "audio_capture_drop_count",
  videoCaptureDrop: "video_capture_drop_count",
  reconnectLostEvent: "reconnect_lost_event_count",
  reconnectDuplicate: "reconnect_duplicate_event_count",
  clientMessageQueue: "client_message_queue_depth",
  clientByteQueue: "client_byte_queue_bytes",
  memoryBaseline: "memory_baseline_bytes",
  memoryCurrent: "memory_current_bytes",
  memoryHighWater: "memory_high_water_bytes",
  memoryGrowth: "memory_growth_bytes",
});

const METRIC_SPECIFICATIONS = new Map([
  [METRICS.endpointToFirstRenderedAudio, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.serverOutputToRenderedAudio, {family: FAMILIES.duration, crossHost: true}],
  [METRICS.audioScheduled, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.audioCompleted, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.videoCaptureToAdmissionAck, {family: FAMILIES.duration, crossHost: true}],
  [METRICS.reconnectDetection, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.reconnectRecovery, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.reducerToViewCommit, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.longTask, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.mainLoopStall, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.liveSnapshotRender, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.deltaRender, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.traceRender, {family: FAMILIES.duration, crossHost: false}],
  [METRICS.audioGap, {family: FAMILIES.counter, crossHost: false}],
  [METRICS.audioUnderflow, {family: FAMILIES.counter, crossHost: false}],
  [METRICS.audioOverrun, {family: FAMILIES.counter, crossHost: false}],
  [METRICS.audioCaptureDrop, {family: FAMILIES.counter, crossHost: false}],
  [METRICS.videoCaptureDrop, {family: FAMILIES.counter, crossHost: false}],
  [METRICS.reconnectLostEvent, {family: FAMILIES.counter, crossHost: false}],
  [METRICS.reconnectDuplicate, {family: FAMILIES.counter, crossHost: false}],
  [METRICS.clientMessageQueue, {family: FAMILIES.gauge, crossHost: false}],
  [METRICS.clientByteQueue, {family: FAMILIES.gauge, crossHost: false}],
  [METRICS.memoryBaseline, {family: FAMILIES.gauge, crossHost: false}],
  [METRICS.memoryCurrent, {family: FAMILIES.gauge, crossHost: false}],
  [METRICS.memoryHighWater, {family: FAMILIES.gauge, crossHost: false}],
  [METRICS.memoryGrowth, {family: FAMILIES.gauge, crossHost: false}],
]);

export const DIMENSIONS = Object.freeze({transport: "transport", media: "media", queue: "queue", render: "render"});

export const DIMENSION_VALUES = Object.freeze({
  websocket: "websocket",
  webrtc: "webrtc",
  audio: "audio",
  video: "video",
  data: "data",
  outboundMessages: "outbound_messages",
  outboundBytes: "outbound_bytes",
  audioPlayout: "audio_playout",
  videoCapture: "video_capture",
  conversation: "conversation",
  liveSnapshot: "live_snapshot",
  delta: "delta",
  trace: "trace",
});

const DIMENSION_PAIRS = new Map([
  [DIMENSIONS.transport, new Set([DIMENSION_VALUES.websocket, DIMENSION_VALUES.webrtc])],
  [DIMENSIONS.media, new Set([DIMENSION_VALUES.audio, DIMENSION_VALUES.video, DIMENSION_VALUES.data])],
  [DIMENSIONS.queue, new Set([
    DIMENSION_VALUES.outboundMessages,
    DIMENSION_VALUES.outboundBytes,
    DIMENSION_VALUES.audioPlayout,
    DIMENSION_VALUES.videoCapture,
  ])],
  [DIMENSIONS.render, new Set([
    DIMENSION_VALUES.conversation,
    DIMENSION_VALUES.liveSnapshot,
    DIMENSION_VALUES.delta,
    DIMENSION_VALUES.trace,
  ])],
]);

export const COVERAGE = Object.freeze({measured: "measured", simulated: "simulated", unsupported: "unsupported"});

export const COVERAGE_REASONS = Object.freeze({
  none: "none",
  platformUnavailable: "platform_unavailable",
  capabilityUnavailable: "capability_unavailable",
  sensorUnavailable: "sensor_unavailable",
  permissionUnavailable: "permission_unavailable",
  clockUnsynchronized: "clock_unsynchronized",
});

export const REJECTION_REASONS = Object.freeze({
  unknownEnum: "unknown_enum",
  wrongMetricFamily: "wrong_metric_family",
  backwardTime: "backward_time",
  timingOverflow: "timing_overflow",
  unmatchedSpan: "unmatched_span",
  duplicateSpan: "duplicate_span",
  sourceBound: "source_bound",
  seriesBound: "series_bound",
  dimensionBound: "dimension_bound",
  pendingSpanBound: "pending_span_bound",
  observationBound: "observation_bound",
  counterOverflow: "counter_overflow",
  coverageConflict: "coverage_conflict",
  identityMismatch: "identity_mismatch",
  artifactBound: "artifact_bound",
  providerUnavailable: "provider_unavailable",
});

const REJECTION_ORDER = Object.freeze([
  REJECTION_REASONS.unknownEnum,
  REJECTION_REASONS.wrongMetricFamily,
  REJECTION_REASONS.backwardTime,
  REJECTION_REASONS.timingOverflow,
  REJECTION_REASONS.unmatchedSpan,
  REJECTION_REASONS.duplicateSpan,
  REJECTION_REASONS.sourceBound,
  REJECTION_REASONS.seriesBound,
  REJECTION_REASONS.dimensionBound,
  REJECTION_REASONS.pendingSpanBound,
  REJECTION_REASONS.observationBound,
  REJECTION_REASONS.counterOverflow,
  REJECTION_REASONS.coverageConflict,
  REJECTION_REASONS.identityMismatch,
  REJECTION_REASONS.artifactBound,
  REJECTION_REASONS.providerUnavailable,
]);

export const DROP_REASONS = Object.freeze({deterministicSampling: "deterministic_sampling"});

export const DURATION_BUCKETS_NS = Object.freeze([
  100_000n, 250_000n, 500_000n,
  1_000_000n, 2_000_000n, 5_000_000n, 10_000_000n, 20_000_000n, 50_000_000n,
  100_000_000n, 250_000_000n, 500_000_000n,
  1_000_000_000n, 2_000_000_000n, 5_000_000_000n, 10_000_000_000n,
  30_000_000_000n, 60_000_000_000n, 300_000_000_000n, 3_600_000_000_000n,
  MAX_DURATION_NS,
]);

const own = (object, key) => Object.prototype.hasOwnProperty.call(object, key);

function fail(message) {
  throw new Error(message);
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function onlyKeys(value, allowed, name) {
  if (!isObject(value)) fail(`${name} must be an object`);
  const keys = new Set(allowed);
  for (const key of Object.keys(value)) if (!keys.has(key)) fail(`${name} has unknown field ${JSON.stringify(key)}`);
}

function exactInteger(value, name, minimum, maximum) {
  let result;
  if (typeof value === "bigint") {
    result = value;
  } else if (typeof value === "number" && Number.isSafeInteger(value)) {
    result = BigInt(value);
  } else {
    fail(`${name} must be an exact integer (use BigInt outside JavaScript's safe integer range)`);
  }
  if (result < minimum || result > maximum) fail(`${name} is outside ${minimum}..${maximum}`);
  return result;
}

function u64(value, name) {
  return exactInteger(value, name, 0n, MAX_U64);
}

function i64(value, name) {
  return exactInteger(value, name, MIN_I64, MAX_I64);
}

function boundedNumber(value, name, minimum, maximum) {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    fail(`${name} must be an integer in ${minimum}..${maximum}`);
  }
  return value;
}

function clone(value) {
  if (Array.isArray(value)) return value.map(clone);
  if (isObject(value)) {
    const result = {};
    for (const key of Object.keys(value)) result[key] = clone(value[key]);
    return result;
  }
  return value;
}

function freeze(value) {
  if (value && typeof value === "object" && !Object.isFrozen(value)) {
    for (const item of Object.values(value)) freeze(item);
    Object.freeze(value);
  }
  return value;
}

// This encoder deliberately preserves insertion order. Every contract object is
// rebuilt below in the same field order as the Go structs. BigInt values are
// emitted as unquoted JSON integers, avoiding IEEE-754 truncation.
export function canonicalJSON(value) {
  if (value === null) return "null";
  if (typeof value === "string" || typeof value === "boolean") return JSON.stringify(value);
  if (typeof value === "bigint") return value.toString(10);
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value)) fail("canonical JSON accepts only safe integers or BigInt");
    return Object.is(value, -0) ? "0" : String(value);
  }
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (isObject(value)) {
    return `{${Object.keys(value).map((key) => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`).join(",")}}`;
  }
  fail(`cannot encode ${typeof value} in canonical JSON`);
}

const SHA256_INITIAL = Object.freeze([
  0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
  0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
]);

const SHA256_ROUND = Object.freeze([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

const rotateRight = (value, shift) => (value >>> shift) | (value << (32 - shift));

// Synchronous and dependency-free so it works in browsers and on the hot
// deterministic-sampling path. Input lengths here are hard bounded.
export function sha256(payload) {
  const input = payload instanceof Uint8Array ? payload : encoder.encode(payload);
  const bitLength = BigInt(input.length) * 8n;
  const paddedLength = Math.ceil((input.length + 9) / 64) * 64;
  const padded = new Uint8Array(paddedLength);
  padded.set(input);
  padded[input.length] = 0x80;
  for (let index = 0; index < 8; index++) {
    padded[padded.length - 1 - index] = Number((bitLength >> BigInt(index * 8)) & 0xffn);
  }
  const state = SHA256_INITIAL.slice();
  const words = new Uint32Array(64);
  for (let block = 0; block < padded.length; block += 64) {
    for (let index = 0; index < 16; index++) {
      const offset = block + index * 4;
      words[index] = ((padded[offset] << 24) | (padded[offset + 1] << 16) |
        (padded[offset + 2] << 8) | padded[offset + 3]) >>> 0;
    }
    for (let index = 16; index < 64; index++) {
      const left = words[index - 15];
      const right = words[index - 2];
      const small0 = rotateRight(left, 7) ^ rotateRight(left, 18) ^ (left >>> 3);
      const small1 = rotateRight(right, 17) ^ rotateRight(right, 19) ^ (right >>> 10);
      words[index] = (words[index - 16] + small0 + words[index - 7] + small1) >>> 0;
    }
    let [a, b, c, d, e, f, g, h] = state;
    for (let index = 0; index < 64; index++) {
      const big1 = rotateRight(e, 6) ^ rotateRight(e, 11) ^ rotateRight(e, 25);
      const choose = (e & f) ^ (~e & g);
      const temporary1 = (h + big1 + choose + SHA256_ROUND[index] + words[index]) >>> 0;
      const big0 = rotateRight(a, 2) ^ rotateRight(a, 13) ^ rotateRight(a, 22);
      const majority = (a & b) ^ (a & c) ^ (b & c);
      const temporary2 = (big0 + majority) >>> 0;
      h = g;
      g = f;
      f = e;
      e = (d + temporary1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (temporary1 + temporary2) >>> 0;
    }
    state[0] = (state[0] + a) >>> 0;
    state[1] = (state[1] + b) >>> 0;
    state[2] = (state[2] + c) >>> 0;
    state[3] = (state[3] + d) >>> 0;
    state[4] = (state[4] + e) >>> 0;
    state[5] = (state[5] + f) >>> 0;
    state[6] = (state[6] + g) >>> 0;
    state[7] = (state[7] + h) >>> 0;
  }
  const result = new Uint8Array(32);
  for (let index = 0; index < state.length; index++) {
    result[index * 4] = state[index] >>> 24;
    result[index * 4 + 1] = state[index] >>> 16;
    result[index * 4 + 2] = state[index] >>> 8;
    result[index * 4 + 3] = state[index];
  }
  return result;
}

function hexadecimal(bytes) {
  return Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("");
}

export function digest(payload) {
  return `sha256:${hexadecimal(sha256(payload))}`;
}

const SERVICE_DEFINITION = `${SCHEMA_IDENTITY}:scoped-runtime-bound-payload-free-integer-recorder-immutable-canonical-aggregate-snapshot`;
const CONFIGURATION_DEFINITION = `${CONFIGURATION_SCHEMA_IDENTITY}:mode-deterministic-sampling-hard-bounded-limits-fingerprinted`;

export const SERVICE_CONTRACT = freeze({
  name: "presentation.client.performance_evidence",
  revision: 1n,
  digest: digest(SERVICE_DEFINITION),
});

export const CONFIGURATION_CONTRACT = freeze({
  name: "presentation.client.performance_evidence.config",
  revision: 1n,
  digest: digest(CONFIGURATION_DEFINITION),
});

const COLLECTOR_DESCRIPTOR = {
  format_version: 1n,
  name: "openrealtime.client.performance_evidence.collector",
  revision: 1n,
  realm: "client",
  platforms: ["portable"],
  provides: [SERVICE_CONTRACT],
  config_schema: CONFIGURATION_CONTRACT,
  lifecycle: {quiesce_timeout_ms: 500n, dispose_timeout_ms: 500n},
};

export const COLLECTOR_IDENTITY = freeze({
  name: COLLECTOR_DESCRIPTOR.name,
  revision: COLLECTOR_DESCRIPTOR.revision,
  digest: digest(canonicalJSON(COLLECTOR_DESCRIPTOR)),
});

function normalizeLimits(source) {
  onlyKeys(source, Object.keys(DEFAULT_LIMITS), "performance limits");
  const maxima = DEFAULT_LIMITS;
  const result = {};
  for (const key of Object.keys(DEFAULT_LIMITS)) {
    result[key] = boundedNumber(source[key], `performance limit ${key}`, 1, maxima[key]);
  }
  return result;
}

function normalizeSampling(source) {
  onlyKeys(source, ["numerator", "denominator", "seed"], "performance sampling");
  const denominator = boundedNumber(source.denominator, "sampling denominator", 1, 1 << 20);
  const numerator = boundedNumber(source.numerator, "sampling numerator", 1, denominator);
  return {numerator, denominator, seed: u64(source.seed, "sampling seed")};
}

function buildConfiguration(source, fingerprint) {
  onlyKeys(source, ["format_version", "mode", "sampling", "limits", "fingerprint"], "performance configuration");
  if (source.format_version !== CONFIGURATION_VERSION && source.format_version !== BigInt(CONFIGURATION_VERSION)) {
    fail(`unsupported performance configuration format ${source.format_version}`);
  }
  if (source.mode === OBSERVABILITY_MODES.disabled) fail("disabled observability must omit the collector and probes");
  if (source.mode !== OBSERVABILITY_MODES.sampled && source.mode !== OBSERVABILITY_MODES.release) {
    fail(`unknown observability mode ${JSON.stringify(source.mode)}`);
  }
  return {
    format_version: CONFIGURATION_VERSION,
    mode: source.mode,
    sampling: normalizeSampling(source.sampling),
    limits: normalizeLimits(source.limits),
    fingerprint,
  };
}

export function freezeConfiguration(source) {
  const result = buildConfiguration(source, "");
  result.fingerprint = digest(canonicalJSON(result));
  return freeze(result);
}

export function defaultConfiguration(mode) {
  return freezeConfiguration({
    format_version: CONFIGURATION_VERSION,
    mode,
    sampling: {numerator: 1, denominator: 1, seed: 0n},
    limits: DEFAULT_LIMITS,
  });
}

function validateConfiguration(source) {
  const want = freezeConfiguration(source);
  if (source.fingerprint !== want.fingerprint) {
    fail(`performance configuration fingerprint is ${JSON.stringify(source.fingerprint)}, want ${JSON.stringify(want.fingerprint)}`);
  }
  return want;
}

const DIGEST_PATTERN = /^sha256:[0-9a-f]{64}$/;
const PLUGIN_NAME_PATTERN = /^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$/;

function checkedDigest(value, name) {
  if (typeof value !== "string" || !DIGEST_PATTERN.test(value)) fail(`${name} has invalid SHA-256 digest`);
  return value;
}

function normalizePluginIdentity(source, name) {
  onlyKeys(source, ["name", "revision", "digest"], name);
  if (typeof source.name !== "string" || !PLUGIN_NAME_PATTERN.test(source.name)) fail(`${name} has invalid plugin name`);
  const revision = u64(source.revision, `${name} revision`);
  if (revision === 0n) fail(`${name} revision must be positive`);
  return {name: source.name, revision, digest: checkedDigest(source.digest, `${name} digest`)};
}

function normalizeClock(source, evidenceClass) {
  onlyKeys(source, ["domain", "resolution_ns", "provenance", "cross_host_synchronized", "cross_host_error_bound_ns"], "clock identity");
  if (!Object.values(CLOCK_DOMAINS).includes(source.domain)) fail(`unknown clock domain ${JSON.stringify(source.domain)}`);
  if (!Object.values(CLOCK_PROVENANCE).includes(source.provenance)) fail(`unknown clock provenance ${JSON.stringify(source.provenance)}`);
  const resolution = u64(source.resolution_ns, "clock resolution");
  if (resolution === 0n || resolution > MAX_DURATION_NS) fail("clock resolution is outside the supported range");
  if (typeof source.cross_host_synchronized !== "boolean") fail("clock cross_host_synchronized must be boolean");
  const errorBound = own(source, "cross_host_error_bound_ns") ? u64(source.cross_host_error_bound_ns, "cross-host clock error bound") : 0n;
  if (errorBound > MAX_DURATION_NS) fail("cross-host clock error bound exceeds the supported range");
  if (!source.cross_host_synchronized && errorBound !== 0n) fail("cross-host clock error bound requires synchronized clocks");
  if (source.provenance === CLOCK_PROVENANCE.synchronized && !source.cross_host_synchronized) {
    fail("synchronized clock provenance requires synchronized clocks");
  }
  if (source.cross_host_synchronized && source.provenance !== CLOCK_PROVENANCE.synchronized && source.provenance !== CLOCK_PROVENANCE.fixture) {
    fail("cross-host synchronization requires synchronized or fixture provenance");
  }
  if (evidenceClass === EVIDENCE_CLASSES.deterministicFixture &&
      (source.domain !== CLOCK_DOMAINS.virtualInteger || source.provenance !== CLOCK_PROVENANCE.fixture)) {
    fail("deterministic fixture evidence requires the virtual fixture clock");
  }
  const result = {
    domain: source.domain,
    resolution_ns: resolution,
    provenance: source.provenance,
    cross_host_synchronized: source.cross_host_synchronized,
  };
  if (errorBound !== 0n) result.cross_host_error_bound_ns = errorBound;
  return result;
}

function normalizeSource(source, index) {
  const label = `runtime source ${index}`;
  onlyKeys(source, ["entry_fingerprint", "descriptor", "implementation_digest", "configuration_digest"], label);
  return {
    entry_fingerprint: checkedDigest(source.entry_fingerprint, `${label} entry fingerprint`),
    descriptor: normalizePluginIdentity(source.descriptor, `${label} descriptor`),
    implementation_digest: checkedDigest(source.implementation_digest, `${label} implementation digest`),
    configuration_digest: checkedDigest(source.configuration_digest, `${label} configuration digest`),
  };
}

function sourceSortKey(source) {
  return `${source.entry_fingerprint}\0${source.descriptor.name}\0${source.descriptor.digest}\0${source.implementation_digest}\0${source.configuration_digest}`;
}

function normalizeBinding(source, configuration) {
  onlyKeys(source, [
    "evidence_class", "platform", "clock", "profile_fingerprint", "lock_fingerprint",
    "plan_fingerprint", "manifest_fingerprint", "collector", "implementation_digest",
    "configuration_digest", "fixture_digest", "sources",
  ], "runtime binding");
  if (!Object.values(EVIDENCE_CLASSES).includes(source.evidence_class)) fail(`unknown evidence class ${JSON.stringify(source.evidence_class)}`);
  if (!Object.values(PLATFORMS).includes(source.platform)) fail(`unknown client platform ${JSON.stringify(source.platform)}`);
  if (source.evidence_class === EVIDENCE_CLASSES.browserRuntime) {
    if (source.platform !== PLATFORMS.browser) fail("browser runtime evidence requires the browser platform");
    if (own(source, "fixture_digest") && source.fixture_digest !== "") fail("browser runtime evidence cannot claim a fixture identity");
  }
  if (source.evidence_class === EVIDENCE_CLASSES.macOSNative) {
    if (source.platform !== PLATFORMS.macOS) fail("macos native evidence requires the macos platform");
    if (own(source, "fixture_digest") && source.fixture_digest !== "") fail("macos native evidence cannot claim a fixture identity");
  }
  if (source.evidence_class === EVIDENCE_CLASSES.deterministicFixture && (!own(source, "fixture_digest") || source.fixture_digest === "")) {
    fail("deterministic fixture evidence requires a fixture digest");
  }
  if (!Array.isArray(source.sources) || source.sources.length === 0 || source.sources.length > configuration.limits.max_sources) {
    fail(`runtime binding has invalid source count`);
  }
  const sources = source.sources.map(normalizeSource);
  sources.sort((left, right) => sourceSortKey(left).localeCompare(sourceSortKey(right), "en", {usage: "sort"}));
  for (let index = 1; index < sources.length; index++) {
    if (sourceSortKey(sources[index - 1]) >= sourceSortKey(sources[index])) fail("runtime sources are duplicate");
  }
  const result = {
    evidence_class: source.evidence_class,
    platform: source.platform,
    clock: normalizeClock(source.clock, source.evidence_class),
    profile_fingerprint: checkedDigest(source.profile_fingerprint, "profile fingerprint"),
    lock_fingerprint: checkedDigest(source.lock_fingerprint, "lock fingerprint"),
    plan_fingerprint: checkedDigest(source.plan_fingerprint, "plan fingerprint"),
    manifest_fingerprint: checkedDigest(source.manifest_fingerprint, "manifest fingerprint"),
    collector: normalizePluginIdentity(source.collector, "collector identity"),
    implementation_digest: checkedDigest(source.implementation_digest, "implementation digest"),
    configuration_digest: checkedDigest(source.configuration_digest, "configuration digest"),
  };
  if (own(source, "fixture_digest") && source.fixture_digest !== "") {
    result.fixture_digest = checkedDigest(source.fixture_digest, "fixture digest");
  }
  result.sources = sources;
  if (result.configuration_digest !== configuration.fingerprint) fail("runtime configuration identity does not match selected configuration");
  return result;
}

function runtimeIdentity(binding, configuration, generation) {
  const result = {
    evidence_class: binding.evidence_class,
    platform: binding.platform,
    clock: clone(binding.clock),
    profile_fingerprint: binding.profile_fingerprint,
    lock_fingerprint: binding.lock_fingerprint,
    plan_fingerprint: binding.plan_fingerprint,
    manifest_fingerprint: binding.manifest_fingerprint,
    collector: clone(binding.collector),
    implementation_digest: binding.implementation_digest,
    configuration_digest: binding.configuration_digest,
  };
  if (own(binding, "fixture_digest")) result.fixture_digest = binding.fixture_digest;
  result.observability_mode = configuration.mode;
  result.generation = generation;
  result.sources = clone(binding.sources);
  return result;
}

function normalizeDimensions(source, limit) {
  if (source === null) return null;
  if (!Array.isArray(source)) fail("dimensions must be an array");
  if (source.length > limit) throw new RejectedError(REJECTION_REASONS.dimensionBound);
  const result = source.map((dimension) => {
    if (!isObject(dimension) || Object.keys(dimension).length !== 2 || !own(dimension, "name") || !own(dimension, "value")) {
      throw new RejectedError(REJECTION_REASONS.unknownEnum);
    }
    const values = DIMENSION_PAIRS.get(dimension.name);
    if (!values || !values.has(dimension.value)) throw new RejectedError(REJECTION_REASONS.unknownEnum);
    return {name: dimension.name, value: dimension.value};
  });
  result.sort((left, right) => left.name < right.name ? -1 : left.name > right.name ? 1 : 0);
  for (let index = 1; index < result.length; index++) {
    if (result[index - 1].name === result[index].name) throw new RejectedError(REJECTION_REASONS.dimensionBound);
  }
  return result;
}

function dimensionsKey(dimensions) {
  if (dimensions === null) return "";
  return dimensions.map((dimension) => `${dimension.name}=${dimension.value}\0`).join("");
}

function seriesKey(source, metric, dimensions) {
  return `${source}\0${metric}\0${dimensionsKey(dimensions)}`;
}

function validCoverage(coverage, reason, evidenceClass) {
  if (!Object.values(COVERAGE_REASONS).includes(reason)) return false;
  if (coverage === COVERAGE.measured) return reason === COVERAGE_REASONS.none && evidenceClass !== EVIDENCE_CLASSES.deterministicFixture;
  if (coverage === COVERAGE.simulated) return reason === COVERAGE_REASONS.none;
  if (coverage === COVERAGE.unsupported) return reason !== COVERAGE_REASONS.none;
  return false;
}

function bigEndian(value) {
  const result = new Uint8Array(8);
  for (let index = 7; index >= 0; index--) {
    result[index] = Number(value & 0xffn);
    value >>= 8n;
  }
  return result;
}

function concatenate(...parts) {
  const length = parts.reduce((total, part) => total + part.length, 0);
  const result = new Uint8Array(length);
  let offset = 0;
  for (const part of parts) {
    result.set(part, offset);
    offset += part.length;
  }
  return result;
}

function samplingPrefix(provider, source, metric, dimensions) {
  const parts = [
    encoder.encode(SCHEMA_IDENTITY),
    bigEndian(provider.configuration.sampling.seed),
    bigEndian(provider.identity.generation),
    bigEndian(BigInt(source)),
    encoder.encode(metric),
  ];
  if (dimensions !== null) {
    for (const dimension of dimensions) {
      parts.push(Uint8Array.of(0), encoder.encode(dimension.name), Uint8Array.of(0x3d), encoder.encode(dimension.value));
    }
  }
  return concatenate(...parts);
}

function sample(provider, state, ordinal) {
  const sampling = provider.configuration.sampling;
  if (sampling.numerator === sampling.denominator) return true;
  const sum = sha256(concatenate(state.samplingPrefix, bigEndian(ordinal)));
  let value = 0n;
  for (let index = 0; index < 8; index++) value = (value << 8n) | BigInt(sum[index]);
  return value % BigInt(sampling.denominator) < BigInt(sampling.numerator);
}

function saturatingAdd(value, delta) {
  return MAX_U64 - value < delta ? MAX_U64 : value + delta;
}

export class RejectedError extends Error {
  constructor(reason) {
    super(`performance observation rejected: ${reason}`);
    this.name = "RejectedError";
    this.reason = reason;
  }
}

export class ProviderUnavailableError extends Error {
  constructor() {
    super("performance evidence provider unavailable");
    this.name = "ProviderUnavailableError";
    this.reason = REJECTION_REASONS.providerUnavailable;
  }
}

const scopeStates = new WeakMap();
const providerStates = new WeakMap();
const recorderStates = new WeakMap();
const spanStates = new WeakMap();
const registeredStates = new WeakMap();
const INTERNAL = Symbol("performance evidence internal construction");

function rejection(provider, reason) {
  provider.rejections.set(reason, saturatingAdd(provider.rejections.get(reason) ?? 0n, 1n));
  return new RejectedError(reason);
}

function activeRecorder(recorder) {
  const state = recorderStates.get(recorder);
  if (!state) throw new ProviderUnavailableError();
  const provider = providerStates.get(state.provider);
  if (!provider || !provider.active || state.generation !== provider.identity.generation || state.source >= provider.identity.sources.length) {
    throw new ProviderUnavailableError();
  }
  return {recorder: state, provider};
}

function metricFor(provider, metric, family) {
  const specification = METRIC_SPECIFICATIONS.get(metric);
  if (!specification) throw rejection(provider, REJECTION_REASONS.unknownEnum);
  if (specification.family !== family) throw rejection(provider, REJECTION_REASONS.wrongMetricFamily);
  return specification;
}

function checkedDimensions(provider, source) {
  try {
    return normalizeDimensions(source, provider.configuration.limits.max_dimensions_per_series);
  } catch (error) {
    if (error instanceof RejectedError) throw rejection(provider, error.reason);
    throw error;
  }
}

function stateObservations(state) {
  if (state.family === FAMILIES.duration) return state.duration.count;
  if (state.family === FAMILIES.counter) return state.counter.observations;
  if (state.family === FAMILIES.gauge) return state.gauge.observations;
  return 0n;
}

function makeSeriesState(provider, source, metric, family, dimensions) {
  const state = {
    source,
    metric,
    family,
    dimensions: clone(dimensions),
    coverage: provider.identity.evidence_class === EVIDENCE_CLASSES.deterministicFixture ? COVERAGE.simulated : COVERAGE.measured,
    coverageReason: COVERAGE_REASONS.none,
    coverageSet: false,
    attempts: 0n,
    samplingDropped: 0n,
    duration: null,
    counter: null,
    gauge: null,
    samplingPrefix: null,
  };
  if (family === FAMILIES.duration) state.duration = {count: 0n, sum_ns: 0n, min_ns: 0n, max_ns: 0n, bucket_counts: DURATION_BUCKETS_NS.map(() => 0n)};
  if (family === FAMILIES.counter) state.counter = {value: 0n, observations: 0n};
  if (family === FAMILIES.gauge) state.gauge = {value: 0n, min: 0n, max: 0n, observations: 0n};
  state.samplingPrefix = samplingPrefix(provider, source, metric, dimensions);
  return state;
}

function ensureSeries(provider, source, metric, family, dimensions) {
  const key = seriesKey(source, metric, dimensions);
  const existing = provider.series.get(key);
  if (existing) return {key, state: existing, created: false};
  if (provider.series.size >= provider.configuration.limits.max_series) throw rejection(provider, REJECTION_REASONS.seriesBound);
  const state = makeSeriesState(provider, source, metric, family, dimensions);
  provider.series.set(key, state);
  try {
    enforceArtifactBound(provider);
  } catch (error) {
    provider.series.delete(key);
    throw rejection(provider, REJECTION_REASONS.artifactBound);
  }
  return {key, state, created: true};
}

function attemptSample(provider, state) {
  if (state.coverage === COVERAGE.unsupported) throw rejection(provider, REJECTION_REASONS.coverageConflict);
  if (state.attempts === MAX_U64) throw rejection(provider, REJECTION_REASONS.observationBound);
  if (!sample(provider, state, state.attempts)) {
    state.attempts++;
    state.samplingDropped = saturatingAdd(state.samplingDropped, 1n);
    provider.drops.set(DROP_REASONS.deterministicSampling,
      saturatingAdd(provider.drops.get(DROP_REASONS.deterministicSampling) ?? 0n, 1n));
    return false;
  }
  if (stateObservations(state) >= BigInt(provider.configuration.limits.max_observations_per_series)) {
    throw rejection(provider, REJECTION_REASONS.observationBound);
  }
  return true;
}

function observeUnsigned(provider, state, value) {
  if (!attemptSample(provider, state)) return;
  if (state.family === FAMILIES.duration) {
    if (MAX_U64 - state.duration.sum_ns < value) throw rejection(provider, REJECTION_REASONS.timingOverflow);
    let bucket = 0;
    while (bucket < DURATION_BUCKETS_NS.length && DURATION_BUCKETS_NS[bucket] < value) bucket++;
    if (bucket === DURATION_BUCKETS_NS.length) throw rejection(provider, REJECTION_REASONS.timingOverflow);
    if (state.duration.count === 0n) {
      state.duration.min_ns = value;
      state.duration.max_ns = value;
    } else {
      if (value < state.duration.min_ns) state.duration.min_ns = value;
      if (value > state.duration.max_ns) state.duration.max_ns = value;
    }
    state.duration.count++;
    state.duration.sum_ns += value;
    state.duration.bucket_counts[bucket]++;
  } else if (state.family === FAMILIES.counter) {
    if (MAX_U64 - state.counter.value < value) throw rejection(provider, REJECTION_REASONS.counterOverflow);
    state.counter.value += value;
    state.counter.observations++;
  }
  state.attempts++;
}

function observeGauge(provider, state, value) {
  if (!attemptSample(provider, state)) return;
  if (state.gauge.observations === 0n) {
    state.gauge.min = value;
    state.gauge.max = value;
  } else {
    if (value < state.gauge.min) state.gauge.min = value;
    if (value > state.gauge.max) state.gauge.max = value;
  }
  state.gauge.value = value;
  state.gauge.observations++;
  state.attempts++;
}

function register(recorderObject, metric, family, dimensions) {
  const {recorder, provider} = activeRecorder(recorderObject);
  const specification = metricFor(provider, metric, family);
  if (specification.crossHost && !provider.identity.clock.cross_host_synchronized) {
    throw rejection(provider, REJECTION_REASONS.identityMismatch);
  }
  const normalized = checkedDimensions(provider, dimensions);
  const ensured = ensureSeries(provider, recorder.source, metric, family, normalized);
  return {providerObject: recorder.provider, recorder, provider, key: ensured.key, series: ensured.state};
}

function beginRegistered(registration, start) {
  const current = providerStates.get(registration.providerObject);
  if (!current || !current.active || current !== registration.provider || registration.recorder.generation !== current.identity.generation) {
    throw new ProviderUnavailableError();
  }
  const startNS = u64(start, "duration start");
  if (current.pending.size >= current.configuration.limits.max_pending_spans) {
    throw rejection(current, REJECTION_REASONS.pendingSpanBound);
  }
  if (current.nextSpan === MAX_U64) throw rejection(current, REJECTION_REASONS.timingOverflow);
  current.nextSpan++;
  const internal = {
    providerObject: registration.providerObject,
    generation: registration.recorder.generation,
    source: registration.recorder.source,
    key: registration.key,
    startNS,
    token: current.nextSpan,
    completed: false,
  };
  current.pending.set(internal.token, internal);
  const handle = freeze(Object.create(null));
  spanStates.set(handle, internal);
  return handle;
}

function setRegisteredCoverage(registration, coverage, reason) {
  const current = providerStates.get(registration.providerObject);
  if (!current || !current.active || current !== registration.provider || registration.recorder.generation !== current.identity.generation) {
    throw new ProviderUnavailableError();
  }
  if (!validCoverage(coverage, reason, current.identity.evidence_class)) {
    throw rejection(current, REJECTION_REASONS.unknownEnum);
  }
  const state = registration.series;
  if ((state.coverageSet || stateObservations(state) > 0n) && (coverage !== state.coverage || reason !== state.coverageReason)) {
    throw rejection(current, REJECTION_REASONS.coverageConflict);
  }
  state.coverage = coverage;
  state.coverageReason = reason;
  state.coverageSet = true;
}

function registeredActive(handle) {
  const registration = registeredStates.get(handle);
  if (!registration) throw new ProviderUnavailableError();
  const current = providerStates.get(registration.providerObject);
  if (!current || !current.active || current !== registration.provider || registration.recorder.generation !== current.identity.generation) {
    throw new ProviderUnavailableError();
  }
  return registration;
}

class RegisteredDuration {
  constructor(token, registration) {
    if (token !== INTERNAL) fail("registered duration probes are runtime-created");
    registeredStates.set(this, registration);
    Object.freeze(this);
  }

  record(value) {
    const registration = registeredActive(this);
    const duration = u64(value, "duration");
    if (duration > MAX_DURATION_NS) throw rejection(registration.provider, REJECTION_REASONS.timingOverflow);
    observeUnsigned(registration.provider, registration.series, duration);
  }

  begin(startNS) {
    return beginRegistered(registeredActive(this), startNS);
  }

  setCoverage(coverage, reason) {
    setRegisteredCoverage(registeredActive(this), coverage, reason);
  }
}

class RegisteredCounter {
  constructor(token, registration) {
    if (token !== INTERNAL) fail("registered counter probes are runtime-created");
    registeredStates.set(this, registration);
    Object.freeze(this);
  }

  add(delta) {
    const registration = registeredActive(this);
    observeUnsigned(registration.provider, registration.series, u64(delta, "counter delta"));
  }

  setCoverage(coverage, reason) {
    setRegisteredCoverage(registeredActive(this), coverage, reason);
  }
}

class RegisteredGauge {
  constructor(token, registration) {
    if (token !== INTERNAL) fail("registered gauge probes are runtime-created");
    registeredStates.set(this, registration);
    Object.freeze(this);
  }

  set(value) {
    const registration = registeredActive(this);
    observeGauge(registration.provider, registration.series, i64(value, "gauge value"));
  }

  setCoverage(coverage, reason) {
    setRegisteredCoverage(registeredActive(this), coverage, reason);
  }
}

export class Recorder {
  constructor(token, provider, source, generation) {
    if (token !== INTERNAL) fail("performance recorders are runtime-created");
    recorderStates.set(this, {provider, source, generation});
    Object.freeze(this);
  }

  registerDuration(metric, dimensions = null) {
    return new RegisteredDuration(INTERNAL, register(this, metric, FAMILIES.duration, dimensions));
  }

  registerCounter(metric, dimensions = null) {
    return new RegisteredCounter(INTERNAL, register(this, metric, FAMILIES.counter, dimensions));
  }

  registerGauge(metric, dimensions = null) {
    return new RegisteredGauge(INTERNAL, register(this, metric, FAMILIES.gauge, dimensions));
  }

  recordDuration(metric, nanoseconds, ...dimensions) {
    const normalized = arguments.length > 2 ? dimensions : null;
    this.registerDuration(metric, normalized).record(nanoseconds);
  }

  addCounter(metric, delta, ...dimensions) {
    const normalized = arguments.length > 2 ? dimensions : null;
    this.registerCounter(metric, normalized).add(delta);
  }

  setGauge(metric, value, ...dimensions) {
    const normalized = arguments.length > 2 ? dimensions : null;
    this.registerGauge(metric, normalized).set(value);
  }

  setCoverage(metric, coverage, reason, ...dimensions) {
    const {recorder, provider} = activeRecorder(this);
    const specification = METRIC_SPECIFICATIONS.get(metric);
    if (!specification) throw rejection(provider, REJECTION_REASONS.unknownEnum);
    const normalized = checkedDimensions(provider, arguments.length > 3 ? dimensions : null);
    const ensured = ensureSeries(provider, recorder.source, metric, specification.family, normalized);
    setRegisteredCoverage({providerObject: recorder.provider, recorder, provider, key: ensured.key, series: ensured.state}, coverage, reason);
  }

  beginDuration(metric, startNS, ...dimensions) {
    const normalized = arguments.length > 2 ? dimensions : null;
    return this.registerDuration(metric, normalized).begin(startNS);
  }

  completeDuration(span, endNS) {
    const {recorder, provider} = activeRecorder(this);
    const internal = spanStates.get(span);
    if (!internal) throw rejection(provider, REJECTION_REASONS.unmatchedSpan);
    if (internal.providerObject !== recorder.provider || internal.generation !== recorder.generation || internal.source !== recorder.source) {
      throw rejection(provider, REJECTION_REASONS.identityMismatch);
    }
    if (internal.completed) throw rejection(provider, REJECTION_REASONS.duplicateSpan);
    if (provider.pending.get(internal.token) !== internal) throw rejection(provider, REJECTION_REASONS.unmatchedSpan);
    provider.pending.delete(internal.token);
    internal.completed = true;
    const end = u64(endNS, "duration end");
    if (end < internal.startNS) throw rejection(provider, REJECTION_REASONS.backwardTime);
    const duration = end - internal.startNS;
    if (duration > MAX_DURATION_NS) throw rejection(provider, REJECTION_REASONS.timingOverflow);
    const state = provider.series.get(internal.key);
    if (!state) throw rejection(provider, REJECTION_REASONS.unmatchedSpan);
    observeUnsigned(provider, state, duration);
  }
}

function aggregateSeries(state) {
  const result = {
    source: state.source,
    metric: state.metric,
    family: state.family,
    dimensions: clone(state.dimensions),
    coverage: state.coverage,
    coverage_reason: state.coverageReason,
    attempts: state.attempts,
    sampling_dropped: state.samplingDropped,
  };
  if (state.coverage !== COVERAGE.unsupported) {
    if (state.duration) result.duration = clone(state.duration);
    if (state.counter) result.counter = clone(state.counter);
    if (state.gauge) result.gauge = clone(state.gauge);
  }
  return result;
}

function compareSeries(left, right) {
  if (left.source !== right.source) return left.source - right.source;
  if (left.metric < right.metric) return -1;
  if (left.metric > right.metric) return 1;
  const leftKey = dimensionsKey(left.dimensions);
  const rightKey = dimensionsKey(right.dimensions);
  return leftKey < rightKey ? -1 : leftKey > rightKey ? 1 : 0;
}

function buildSnapshot(provider, withFingerprint) {
  const result = {
    format_version: SNAPSHOT_FORMAT_VERSION,
    schema: SCHEMA_IDENTITY,
    contract: clone(SERVICE_CONTRACT),
    identity: clone(provider.identity),
    configuration: clone(provider.configuration),
    duration_buckets_ns: DURATION_BUCKETS_NS.slice(),
    series: Array.from(provider.series.values(), aggregateSeries).sort(compareSeries),
    rejections: REJECTION_ORDER.map((reason) => ({reason, count: provider.rejections.get(reason) ?? 0n})),
    drops: [{reason: DROP_REASONS.deterministicSampling, count: provider.drops.get(DROP_REASONS.deterministicSampling) ?? 0n}],
    fingerprint: "",
  };
  if (withFingerprint) result.fingerprint = digest(canonicalJSON(result));
  return result;
}

function enforceArtifactBound(provider) {
  const snapshot = buildSnapshot(provider, false);
  for (const series of snapshot.series) {
    series.coverage = COVERAGE.unsupported;
    series.coverage_reason = COVERAGE_REASONS.capabilityUnavailable;
    series.attempts = MAX_U64;
    series.sampling_dropped = MAX_U64;
    if (series.duration) {
      series.duration.count = MAX_U64;
      series.duration.sum_ns = MAX_U64;
      series.duration.min_ns = MAX_U64;
      series.duration.max_ns = MAX_U64;
      series.duration.bucket_counts.fill(MAX_U64);
    }
    if (series.counter) {
      series.counter.value = MAX_U64;
      series.counter.observations = MAX_U64;
    }
    if (series.gauge) {
      series.gauge.value = MIN_I64;
      series.gauge.min = MIN_I64;
      series.gauge.max = MAX_I64;
      series.gauge.observations = MAX_U64;
    }
  }
  for (const counter of snapshot.rejections) counter.count = MAX_U64;
  for (const counter of snapshot.drops) counter.count = MAX_U64;
  snapshot.fingerprint = `sha256:${"f".repeat(64)}`;
  if (encoder.encode(canonicalJSON(snapshot)).length + 1 > provider.configuration.limits.max_artifact_bytes) {
    fail(`performance snapshot would exceed ${provider.configuration.limits.max_artifact_bytes} bytes`);
  }
}

export function marshalSnapshot(snapshot) {
  if (!isObject(snapshot) || typeof snapshot.fingerprint !== "string") fail("performance snapshot is invalid");
  const candidate = clone(snapshot);
  candidate.fingerprint = "";
  const wanted = digest(canonicalJSON(candidate));
  if (snapshot.fingerprint !== wanted) fail(`performance snapshot fingerprint is ${JSON.stringify(snapshot.fingerprint)}, want ${JSON.stringify(wanted)}`);
  const payload = `${canonicalJSON(snapshot)}\n`;
  const maximum = snapshot.configuration?.limits?.max_artifact_bytes;
  if (!Number.isSafeInteger(maximum) || encoder.encode(payload).length > maximum || encoder.encode(payload).length > MAX_SNAPSHOT_BYTES) {
    fail("performance snapshot exceeds its artifact bound");
  }
  return payload;
}

export class Provider {
  constructor(token, identity, configuration) {
    if (token !== INTERNAL) fail("performance providers are mount-scope-created");
    providerStates.set(this, {
      active: true,
      identity,
      configuration,
      series: new Map(),
      pending: new Map(),
      nextSpan: 0n,
      rejections: new Map(),
      drops: new Map(),
    });
    Object.freeze(this);
  }

  identity() {
    const state = providerStates.get(this);
    if (!state) throw new ProviderUnavailableError();
    return freeze(clone(state.identity));
  }

  recorder(source) {
    const state = providerStates.get(this);
    if (!state || !state.active) throw new ProviderUnavailableError();
    const index = boundedNumber(source, "source index", 0, 0xffff);
    if (index >= state.identity.sources.length) throw rejection(state, REJECTION_REASONS.sourceBound);
    return new Recorder(INTERNAL, this, index, state.identity.generation);
  }

  snapshot() {
    const state = providerStates.get(this);
    if (!state) throw new ProviderUnavailableError();
    const snapshot = buildSnapshot(state, true);
    const payload = canonicalJSON(snapshot);
    if (encoder.encode(payload).length + 1 > state.configuration.limits.max_artifact_bytes ||
        encoder.encode(payload).length + 1 > MAX_SNAPSHOT_BYTES) {
      fail("performance snapshot exceeds its artifact bound");
    }
    return freeze(snapshot);
  }
}

export class MountScope {
  constructor() {
    scopeStates.set(this, {generation: 0n, active: null});
    Object.freeze(this);
  }

  mount(bindingSource, configurationSource) {
    const scope = scopeStates.get(this);
    if (!scope) fail("performance mount scope is unavailable");
    const configuration = validateConfiguration(configurationSource);
    const binding = normalizeBinding(bindingSource, configuration);
    if (scope.active) {
      const active = providerStates.get(scope.active);
      if (active?.active) fail("performance collector is already mounted in this scope");
    }
    if (scope.generation === MAX_U64) fail("performance collector generation overflow");
    scope.generation++;
    const identity = runtimeIdentity(binding, configuration, scope.generation);
    const provider = new Provider(INTERNAL, identity, configuration);
    enforceArtifactBound(providerStates.get(provider));
    provider.snapshot();
    scope.active = provider;
    return provider;
  }

  lose(provider) {
    const scope = scopeStates.get(this);
    if (!scope) fail("performance mount scope is unavailable");
    const state = providerStates.get(provider);
    if (!state) fail("performance provider is invalid");
    if (!state.active) return;
    if (!scope.active) fail("performance provider does not belong to this mount scope");
    if (scope.active !== provider) fail("performance provider does not match the active generation");
    state.active = false;
    for (const span of state.pending.values()) span.completed = true;
    state.pending.clear();
    scope.active = null;
  }
}

export function familyOf(metric) {
  return METRIC_SPECIFICATIONS.get(metric)?.family;
}
