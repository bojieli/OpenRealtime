const FORMAT_VERSION = 1;
const MAX_JSON_DEPTH = 256;
export const MAX_CORPUS_BYTES = 1 << 20;

export const DEFAULT_LIMITS = Object.freeze({
  max_vectors: 64,
  max_steps_per_vector: 512,
  max_event_bytes: 64 << 10,
  max_string_bytes: 16 << 10,
  max_conversation_items: 1024,
  max_tool_calls: 256,
  max_outbound_events: 2048,
  max_protocol_log_entries: 4096,
  max_errors_per_vector: 128,
  max_reconnect_attempts: 3,
  max_virtual_time_ms: 86_400_000,
});

const OPERATION_FIELDS = Object.freeze({
  connect: ["transport"], connected: [], transport_lost: ["reason"], retry: [], disconnect: ["reason"],
  inbound: ["event"], session_update: ["session"], typed_text: ["item_id", "text"], end_turn: [],
  playout: ["speaking", "item_id", "played_ms"], cancel_response: [],
  tool_result: ["call_id", "status", "output", "error"],
});

const encoder = new TextEncoder();
const byteLength = (value) => encoder.encode(value).length;
const own = (object, key) => Object.prototype.hasOwnProperty.call(object, key);

function fail(message) {
  throw new Error(message);
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function requireObject(value, name) {
  if (!isObject(value)) fail(`${name} must be a JSON object`);
  return value;
}

function requireString(value, name, maximum, allowEmpty = false) {
  if (typeof value !== "string") fail(`${name} must be a string`);
  if (!allowEmpty && value.length === 0) fail(`${name} must not be empty`);
  if (byteLength(value) > maximum) fail(`${name} exceeds ${maximum} bytes`);
  return value;
}

function requireInteger(value, name, minimum = 0) {
  if (!Number.isSafeInteger(value)) fail(`${name} must be an integer`);
  if (value < minimum) fail(`${name} must be at least ${minimum}`);
  return value;
}

function onlyKeys(object, allowed, name) {
  requireObject(object, name);
  const permitted = new Set(allowed);
  for (const key of Object.keys(object)) {
    if (!permitted.has(key)) fail(`${name} has unknown field ${JSON.stringify(key)}`);
  }
}

// JSON.parse intentionally accepts duplicate object keys. This small recursive
// parser rejects duplicates after escape decoding, rejects non-finite numbers,
// and rejects trailing values before any typed corpus interpretation occurs.
export function parseStrictJSON(source) {
  if (typeof source !== "string") fail("JSON source must be a string");
  let offset = 0;

  function whitespace() {
    while (offset < source.length && /[ \t\r\n]/.test(source[offset])) offset++;
  }

  function string() {
    const start = offset;
    if (source[offset++] !== '"') fail(`expected JSON string at offset ${start}`);
    while (offset < source.length) {
      const character = source.charCodeAt(offset++);
      if (character === 0x22) {
        const token = source.slice(start, offset);
        try {
          return JSON.parse(token);
        } catch (error) {
          fail(`invalid JSON string at offset ${start}: ${error.message}`);
        }
      }
      if (character < 0x20) fail(`control character in JSON string at offset ${offset - 1}`);
      if (character === 0x5c) {
        if (offset >= source.length) fail(`unterminated JSON escape at offset ${offset}`);
        if (source[offset] === "u") {
          const digits = source.slice(offset + 1, offset + 5);
          if (!/^[0-9a-fA-F]{4}$/.test(digits)) fail(`invalid Unicode escape at offset ${offset - 1}`);
          offset += 5;
        } else {
          if (!/["\\/bfnrt]/.test(source[offset])) fail(`invalid JSON escape at offset ${offset - 1}`);
          offset++;
        }
      }
    }
    fail(`unterminated JSON string at offset ${start}`);
  }

  function value(depth = 0) {
    whitespace();
    if (offset >= source.length) fail(`expected JSON value at offset ${offset}`);
    const character = source[offset];
    if (character === "{") {
      if (depth >= MAX_JSON_DEPTH) fail(`JSON nesting exceeds ${MAX_JSON_DEPTH}`);
      return object(depth);
    }
    if (character === "[") {
      if (depth >= MAX_JSON_DEPTH) fail(`JSON nesting exceeds ${MAX_JSON_DEPTH}`);
      return array(depth);
    }
    if (character === '"') return string();
    for (const [literal, decoded] of [["true", true], ["false", false], ["null", null]]) {
      if (source.startsWith(literal, offset)) {
        offset += literal.length;
        return decoded;
      }
    }
    const match = source.slice(offset).match(/^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/);
    if (!match) fail(`invalid JSON value at offset ${offset}`);
    offset += match[0].length;
    const decoded = Number(match[0]);
    if (!Number.isFinite(decoded)) fail(`non-finite JSON number at offset ${offset - match[0].length}`);
    return decoded;
  }

  function object(depth) {
    offset++;
    whitespace();
    const result = Object.create(null);
    const keys = new Set();
    if (source[offset] === "}") {
      offset++;
      return result;
    }
    while (true) {
      whitespace();
      if (source[offset] !== '"') fail(`expected object key at offset ${offset}`);
      const key = string();
      if (keys.has(key)) fail(`duplicate JSON key ${JSON.stringify(key)}`);
      keys.add(key);
      whitespace();
      if (source[offset++] !== ":") fail(`expected ':' after object key at offset ${offset - 1}`);
      result[key] = value(depth + 1);
      whitespace();
      const separator = source[offset++];
      if (separator === "}") return result;
      if (separator !== ",") fail(`expected ',' or '}' at offset ${offset - 1}`);
    }
  }

  function array(depth) {
    offset++;
    whitespace();
    const result = [];
    if (source[offset] === "]") {
      offset++;
      return result;
    }
    while (true) {
      result.push(value(depth + 1));
      whitespace();
      const separator = source[offset++];
      if (separator === "]") return result;
      if (separator !== ",") fail(`expected ',' or ']' at offset ${offset - 1}`);
    }
  }

  const result = value();
  whitespace();
  if (offset !== source.length) fail(`trailing JSON token at offset ${offset}`);
  return result;
}

export function stableJSON(value) {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(",")}]`;
  if (isObject(value)) {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stableJSON(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function validateSnapshot(snapshot, limits, name) {
  onlyKeys(snapshot, ["now_ms", "connection", "session", "response", "conversation", "playout", "tools", "last_truncation", "last_error", "protocol_log"], `${name}.snapshot`);
  onlyKeys(snapshot.connection, ["phase", "transport", "reason", "attempt", "next_retry_ms"], `${name}.snapshot.connection`);
  onlyKeys(snapshot.session, ["id", "manual_turns", "openrealtime"], `${name}.snapshot.session`);
  onlyKeys(snapshot.session.openrealtime, ["present", "version", "enabled", "observers", "available_observers", "debug_enabled", "video"], `${name}.snapshot.session.openrealtime`);
  onlyKeys(snapshot.session.openrealtime.video, ["format", "fps_cap", "max_dimension", "max_frame_bytes"], `${name}.snapshot.session.openrealtime.video`);
  onlyKeys(snapshot.response, ["id", "open", "status", "reason"], `${name}.snapshot.response`);
  onlyKeys(snapshot.playout, ["speaking", "item_id", "played_ms"], `${name}.snapshot.playout`);
  onlyKeys(snapshot.last_truncation, ["item_id", "audio_end_ms"], `${name}.snapshot.last_truncation`);
  if (!Array.isArray(snapshot.conversation) || snapshot.conversation.length > limits.max_conversation_items) fail(`${name}.snapshot conversation exceeds limit`);
  snapshot.conversation.forEach((item, index) => onlyKeys(item, ["item_id", "role", "channel", "text", "audio_deltas", "audio_done", "truncated_ms"], `${name}.snapshot.conversation[${index}]`));
  if (!Array.isArray(snapshot.tools) || snapshot.tools.length > limits.max_tool_calls) fail(`${name}.snapshot tools exceeds limit`);
  snapshot.tools.forEach((tool, index) => onlyKeys(tool, ["call_id", "name", "arguments", "status", "output", "error"], `${name}.snapshot.tools[${index}]`));
  if (!Array.isArray(snapshot.protocol_log) || snapshot.protocol_log.length > limits.max_protocol_log_entries) fail(`${name}.snapshot protocol log exceeds limit`);
  snapshot.protocol_log.forEach((entry, index) => onlyKeys(entry, ["at_ms", "direction", "type"], `${name}.snapshot.protocol_log[${index}]`));
}

export function loadCorpusText(source) {
  if (byteLength(source) > MAX_CORPUS_BYTES) fail(`reducer corpus exceeds ${MAX_CORPUS_BYTES} bytes`);
  const corpus = parseStrictJSON(source);
  onlyKeys(corpus, ["format_version", "limits", "vectors"], "corpus");
  if (corpus.format_version !== FORMAT_VERSION) fail(`unsupported reducer corpus format_version ${corpus.format_version}`);
  onlyKeys(corpus.limits, Object.keys(DEFAULT_LIMITS), "corpus.limits");
  if (stableJSON(corpus.limits) !== stableJSON(DEFAULT_LIMITS)) fail(`reducer corpus limits do not match format ${FORMAT_VERSION}`);
  if (!Array.isArray(corpus.vectors) || corpus.vectors.length === 0 || corpus.vectors.length > corpus.limits.max_vectors) fail("reducer corpus vector count is outside limits");
  const names = new Set();
  corpus.vectors.forEach((vector, vectorIndex) => {
    const label = `vectors[${vectorIndex}]`;
    onlyKeys(vector, ["name", "description", "steps", "expect"], label);
    requireString(vector.name, `${label}.name`, corpus.limits.max_string_bytes);
    requireString(vector.description, `${label}.description`, corpus.limits.max_string_bytes);
    if (names.has(vector.name)) fail(`duplicate reducer vector name ${JSON.stringify(vector.name)}`);
    names.add(vector.name);
    if (!Array.isArray(vector.steps) || vector.steps.length === 0 || vector.steps.length > corpus.limits.max_steps_per_vector) fail(`${label}.steps count is outside limits`);
    let previous = 0;
    vector.steps.forEach((step, stepIndex) => {
      onlyKeys(step, ["at_ms", "operation"], `${label}.steps[${stepIndex}]`);
      requireInteger(step.at_ms, `${label}.steps[${stepIndex}].at_ms`);
      if (step.at_ms < previous || step.at_ms > corpus.limits.max_virtual_time_ms) fail(`${label}.steps[${stepIndex}] has invalid virtual time`);
      previous = step.at_ms;
      const operationName = `${label}.steps[${stepIndex}].operation`;
      requireObject(step.operation, operationName);
      const kind = requireString(step.operation.kind, `${operationName}.kind`, corpus.limits.max_string_bytes);
      if (!own(OPERATION_FIELDS, kind)) fail(`unknown operation kind ${JSON.stringify(kind)}`);
      onlyKeys(step.operation, ["kind", ...OPERATION_FIELDS[kind]], operationName);
    });
    onlyKeys(vector.expect, ["snapshot", "outbound", "errors"], `${label}.expect`);
    validateSnapshot(vector.expect.snapshot, corpus.limits, `${label}.expect`);
    if (!Array.isArray(vector.expect.outbound) || vector.expect.outbound.length > corpus.limits.max_outbound_events) fail(`${label}.expect.outbound exceeds limit`);
    vector.expect.outbound.forEach((event, index) => requireObject(event, `${label}.expect.outbound[${index}]`));
    if (!Array.isArray(vector.expect.errors) || vector.expect.errors.length > corpus.limits.max_errors_per_vector) fail(`${label}.expect.errors exceeds limit`);
    vector.expect.errors.forEach((message, index) => requireString(message, `${label}.expect.errors[${index}]`, corpus.limits.max_string_bytes));
  });
  return corpus;
}

function emptyNegotiation() {
  return {present: false, version: 0, enabled: [], observers: [], available_observers: [], debug_enabled: false, video: {format: "", fps_cap: 0, max_dimension: 0, max_frame_bytes: 0}};
}

function emptySession() {
  return {id: "", manual_turns: false, openrealtime: emptyNegotiation()};
}

function initialState() {
  return {
    now_ms: 0,
    connection: {phase: "disconnected", transport: "", reason: "", attempt: 0, next_retry_ms: 0},
    session: emptySession(),
    response: {id: "", open: false, status: "", reason: ""},
    conversation: [],
    playout: {speaking: false, item_id: "", played_ms: 0},
    tools: [],
    last_truncation: {item_id: "", audio_end_ms: 0},
    last_error: "",
    protocol_log: [],
  };
}

export class ClientReducer {
  constructor(limits = DEFAULT_LIMITS) {
    if (stableJSON(limits) !== stableJSON(DEFAULT_LIMITS)) fail("unsupported reducer limits");
    this.limits = structuredClone(limits);
    this.state = initialState();
    this.commands = [];
  }

  snapshot() {
    return structuredClone(this.state);
  }

  outbound() {
    return structuredClone(this.commands);
  }

  apply(atMS, operation) {
    requireInteger(atMS, "virtual time");
    if (atMS < this.state.now_ms) fail(`virtual time moved backwards from ${this.state.now_ms} to ${atMS}`);
    if (atMS > this.limits.max_virtual_time_ms) fail(`virtual time ${atMS} exceeds limit ${this.limits.max_virtual_time_ms}`);
    this.validateOperation(operation);
    const checkpoint = {state: structuredClone(this.state), commands: structuredClone(this.commands)};
    this.state.now_ms = atMS;
    try {
      this.applyOperation(operation);
    } catch (error) {
      this.state = checkpoint.state;
      this.commands = checkpoint.commands;
      throw error;
    }
  }

  validateOperation(operation) {
    requireObject(operation, "operation");
    requireString(operation.kind, "operation kind", this.limits.max_string_bytes);
    for (const key of ["transport", "reason", "item_id", "text", "call_id", "status", "output", "error"]) {
      if (own(operation, key)) requireString(operation[key], key, this.limits.max_string_bytes, true);
    }
    for (const key of ["event", "session"]) {
      if (own(operation, key) && byteLength(JSON.stringify(operation[key])) > this.limits.max_event_bytes) fail("operation JSON exceeds " + this.limits.max_event_bytes + " bytes");
    }
    if (own(operation, "played_ms")) requireInteger(operation.played_ms, "played_ms");
    if (!own(OPERATION_FIELDS, operation.kind)) fail(`unknown operation kind ${JSON.stringify(operation.kind)}`);
    const permitted = new Set(["kind", ...OPERATION_FIELDS[operation.kind]]);
    for (const key of Object.keys(operation)) if (!permitted.has(key)) fail(`operation ${JSON.stringify(operation.kind)} does not allow field ${JSON.stringify(key)}`);
  }

  applyOperation(operation) {
    switch (operation.kind) {
      case "connect": return this.connect(operation.transport);
      case "connected": return this.connected();
      case "transport_lost": return this.transportLost(operation.reason);
      case "retry": return this.retry();
      case "disconnect": return this.disconnect(operation.reason ?? "");
      case "inbound": return this.inbound(operation.event);
      case "session_update": return this.sessionUpdate(operation.session);
      case "typed_text": return this.typedText(operation.item_id, operation.text);
      case "end_turn": return this.endTurn();
      case "playout": this.state.playout = {speaking: operation.speaking ?? false, item_id: operation.item_id ?? "", played_ms: operation.played_ms ?? 0}; return;
      case "cancel_response": return this.cancelResponse();
      case "tool_result": return this.toolResult(operation);
    }
  }

  requireConnected() {
    if (this.state.connection.phase !== "connected") fail(`operation requires connected state, got ${this.state.connection.phase}`);
  }

  connect(transport) {
    if (!["disconnected", "failed"].includes(this.state.connection.phase)) fail(`cannot connect while connection is ${this.state.connection.phase}`);
    if (!["websocket", "webrtc"].includes(transport)) fail(`unsupported transport ${JSON.stringify(transport ?? "")}`);
    this.state.connection = {phase: "connecting", transport, reason: "", attempt: 0, next_retry_ms: 0};
    this.state.last_error = "";
  }

  connected() {
    if (this.state.connection.phase !== "connecting") fail(`cannot become connected while connection is ${this.state.connection.phase}`);
    Object.assign(this.state.connection, {phase: "connected", reason: "", attempt: 0, next_retry_ms: 0});
    this.state.last_error = "";
  }

  cleanupSession(reason) {
    if (this.state.response.open) Object.assign(this.state.response, {open: false, status: "abandoned", reason});
    this.state.playout = {speaking: false, item_id: "", played_ms: 0};
    for (const tool of this.state.tools) if (tool.status === "pending") Object.assign(tool, {status: "failed", error: reason});
    this.state.session = emptySession();
  }

  transportLost(reason) {
    const phase = this.state.connection.phase;
    if (!["connected", "connecting"].includes(phase)) fail(`transport cannot be lost while connection is ${phase}`);
    if (!reason || reason.trim() === "") fail("transport loss requires a reason");
    this.cleanupSession(reason);
    let attempt = this.state.connection.attempt;
    if (attempt === 0) attempt = 1;
    else if (phase === "connecting") {
      if (attempt >= this.limits.max_reconnect_attempts) {
        Object.assign(this.state.connection, {phase: "failed", reason, next_retry_ms: 0});
        this.state.last_error = reason;
        return;
      }
      attempt++;
    }
    const delays = [250, 1000, 4000];
    Object.assign(this.state.connection, {phase: "reconnecting", reason, attempt, next_retry_ms: this.state.now_ms + delays[Math.min(attempt - 1, delays.length - 1)]});
  }

  retry() {
    if (this.state.connection.phase !== "reconnecting") fail(`cannot retry while connection is ${this.state.connection.phase}`);
    if (this.state.now_ms < this.state.connection.next_retry_ms) fail(`retry at ${this.state.now_ms} precedes deadline ${this.state.connection.next_retry_ms}`);
    Object.assign(this.state.connection, {phase: "connecting", next_retry_ms: 0});
  }

  disconnect(reason) {
    reason ||= "disconnected";
    this.cleanupSession(reason);
    this.state.connection = {phase: "disconnected", transport: "", reason, attempt: 0, next_retry_ms: 0};
  }

  emit(...events) {
    if (this.commands.length + events.length > this.limits.max_outbound_events) fail("outbound event limit reached");
    if (this.state.protocol_log.length + events.length > this.limits.max_protocol_log_entries) fail("protocol log limit reached");
    for (const event of events) {
      requireObject(event, "outbound event");
      requireString(event.type, "type", this.limits.max_string_bytes);
      if (byteLength(JSON.stringify(event)) > this.limits.max_event_bytes) fail(`outbound event exceeds ${this.limits.max_event_bytes} bytes`);
    }
    for (const event of events) {
      this.commands.push(structuredClone(event));
      this.state.protocol_log.push({at_ms: this.state.now_ms, direction: "out", type: event.type});
    }
  }

  sessionUpdate(session) {
    this.requireConnected();
    requireObject(session, "session update");
    if (own(session, "type") && session.type !== "realtime") fail('session.type must be "realtime"');
    this.emit({type: "session.update", session: structuredClone(session)});
  }

  typedText(itemID, text) {
    this.requireConnected();
    itemID = (itemID ?? "").trim();
    text = (text ?? "").trim();
    if (!itemID || !text) fail("typed_text requires non-empty item_id and text");
    if (this.findConversation(itemID, "input_text") >= 0) fail(`conversation item ${JSON.stringify(itemID)} already exists`);
    if (this.state.conversation.length >= this.limits.max_conversation_items) fail("conversation item limit reached");
    this.emit(
      {type: "conversation.item.create", item: {type: "message", role: "user", content: [{type: "input_text", text}]}},
      {type: "response.create"},
    );
    this.state.conversation.push({item_id: itemID, role: "user", channel: "input_text", text, audio_deltas: 0, audio_done: false, truncated_ms: 0});
  }

  endTurn() {
    this.requireConnected();
    if (!this.state.session.manual_turns) fail("end_turn requires negotiated manual turn detection");
    this.emit({type: "input_audio_buffer.commit"}, {type: "response.create"});
  }

  cancelResponse() {
    this.requireConnected();
    if (!this.state.response.open) fail("no response is open");
    if (this.state.response.status === "cancelling") fail("response cancellation is already pending");
    this.emit(this.responseCancel());
    this.state.response.status = "cancelling";
  }

  responseCancel() {
    const event = {type: "response.cancel"};
    if (this.state.response.id) event.response_id = this.state.response.id;
    return event;
  }

  inbound(event) {
    this.requireConnected();
    requireObject(event, "inbound event");
    const type = requireString(event.type, "type", this.limits.max_string_bytes);
    if (byteLength(JSON.stringify(event)) > this.limits.max_event_bytes) fail(`inbound event exceeds ${this.limits.max_event_bytes} bytes`);
    if (this.state.protocol_log.length >= this.limits.max_protocol_log_entries) fail("protocol log limit reached");
    this.state.protocol_log.push({at_ms: this.state.now_ms, direction: "in", type});
    switch (type) {
      case "session.created": {
        const session = requireObject(event.session, "session");
        if (!own(session, "id")) fail("session.created: missing id");
        this.state.session.id = requireString(session.id, "id", this.limits.max_string_bytes);
        return;
      }
      case "session.updated": return this.sessionUpdated(event);
      case "input_audio_buffer.speech_started": return this.speechStarted();
      case "input_audio_buffer.speech_stopped": return;
      case "conversation.item.input_audio_transcription.completed": return this.setConversation(requireString(event.item_id, "item_id", this.limits.max_string_bytes), "user", "input_audio_transcript", requireString(event.transcript, "transcript", this.limits.max_string_bytes), false);
      case "response.created": return this.responseCreated(event);
      case "response.output_audio_transcript.delta": return this.textDelta(event, "output_audio_transcript");
      case "response.output_text.delta": return this.textDelta(event, "output_text");
      case "response.output_audio.delta": return this.audioDelta(event);
      case "response.output_audio.done": return this.audioDone(event);
      case "response.function_call_arguments.done": return this.functionCall(event);
      case "response.done": return this.responseDone(event);
      case "conversation.item.truncated": return this.itemTruncated(event);
      case "openrealtime.observation.added": return this.observation(event);
      case "openrealtime.debug.event": return;
      case "error": {
        const error = requireObject(event.error, "error");
        this.state.last_error = requireString(error.message, "message", this.limits.max_string_bytes);
        return;
      }
      default: return;
    }
  }

  sessionUpdated(event) {
    const session = requireObject(event.session, "session");
    if (own(session, "id")) this.state.session.id = requireString(session.id, "session.id", this.limits.max_string_bytes, true);
    this.state.session.manual_turns = session.audio?.input?.turn_detection === null;
    const extension = session.openrealtime;
    if (extension === undefined || extension === null) {
      this.state.session.openrealtime = emptyNegotiation();
      return;
    }
    requireObject(extension, "session.openrealtime");
    const list = (key) => {
      if (!own(extension, key) || extension[key] === null) return [];
      if (!Array.isArray(extension[key]) || extension[key].length > this.limits.max_tool_calls) fail(`${key} must be an array of strings`);
      return extension[key].map((value) => requireString(value, `${key} value`, this.limits.max_string_bytes));
    };
    const projection = emptyNegotiation();
    projection.present = true;
    if (own(extension, "version")) projection.version = requireInteger(extension.version, "session.openrealtime.version");
    projection.enabled = list("enabled");
    projection.observers = list("observers");
    projection.available_observers = list("available_observers");
    if (extension.debug !== undefined && extension.debug !== null) {
      requireObject(extension.debug, "session.openrealtime.debug");
      if (own(extension.debug, "enabled") && typeof extension.debug.enabled !== "boolean") fail("session.openrealtime.debug.enabled must be a boolean");
      projection.debug_enabled = extension.debug.enabled === true;
    }
    if (extension.video !== undefined && extension.video !== null) {
      const video = requireObject(extension.video, "session.openrealtime.video");
      if (own(video, "format")) projection.video.format = requireString(video.format, "session.openrealtime.video.format", this.limits.max_string_bytes, true);
      for (const key of ["fps_cap", "max_dimension", "max_frame_bytes"]) if (own(video, key)) projection.video[key] = requireInteger(video[key], `session.openrealtime.video.${key}`);
    }
    this.state.session.openrealtime = projection;
  }

  speechStarted() {
    if (!this.state.playout.speaking) return;
    if (!this.state.playout.item_id) fail("active playout has no item_id");
    const commands = [];
    if (this.state.response.open && this.state.response.status !== "cancelling") commands.push(this.responseCancel());
    if (this.state.response.open && this.state.connection.transport === "webrtc") commands.push({type: "output_audio_buffer.clear"});
    commands.push({type: "conversation.item.truncate", item_id: this.state.playout.item_id, content_index: 0, audio_end_ms: this.state.playout.played_ms});
    this.emit(...commands);
    if (this.state.response.open) this.state.response.status = "cancelling";
    this.state.playout = {speaking: false, item_id: "", played_ms: 0};
  }

  responseCreated(event) {
    if (this.state.response.open) fail(`response ${JSON.stringify(this.state.response.id)} is already open`);
    const response = requireObject(event.response, "response");
    this.state.response = {
      id: requireString(response.id, "id", this.limits.max_string_bytes),
      open: true,
      status: own(response, "status") ? requireString(response.status, "response.status", this.limits.max_string_bytes) : "in_progress",
      reason: "",
    };
  }

  textDelta(event, channel) {
    if (!this.state.response.open) fail("response delta arrived without an open response");
    this.appendConversation(requireString(event.item_id, "item_id", this.limits.max_string_bytes), "assistant", channel, requireString(event.delta, "delta", this.limits.max_string_bytes, true));
  }

  audioDelta(event) {
    if (!this.state.response.open) fail("audio delta arrived without an open response");
    const itemID = requireString(event.item_id, "item_id", this.limits.max_string_bytes);
    const delta = requireString(event.delta, "delta", this.limits.max_string_bytes);
    if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(delta)) fail("response audio delta is not base64");
    let index = this.findConversation(itemID, "output_audio_transcript");
    if (index < 0) {
      if (this.state.conversation.length >= this.limits.max_conversation_items) fail("conversation item limit reached");
      this.state.conversation.push({item_id: itemID, role: "assistant", channel: "output_audio_transcript", text: "", audio_deltas: 1, audio_done: false, truncated_ms: 0});
    } else this.state.conversation[index].audio_deltas++;
  }

  audioDone(event) {
    const itemID = requireString(event.item_id, "item_id", this.limits.max_string_bytes);
    let index = this.findConversation(itemID, "output_audio_transcript");
    if (index < 0) {
      if (this.state.conversation.length >= this.limits.max_conversation_items) fail("conversation item limit reached");
      this.state.conversation.push({item_id: itemID, role: "assistant", channel: "output_audio_transcript", text: "", audio_deltas: 0, audio_done: true, truncated_ms: 0});
    } else this.state.conversation[index].audio_done = true;
  }

  functionCall(event) {
    const callID = requireString(event.call_id, "call_id", this.limits.max_string_bytes);
    const name = requireString(event.name, "name", this.limits.max_string_bytes);
    const args = requireString(event.arguments, "arguments", this.limits.max_string_bytes, true);
    if (this.findTool(callID) >= 0) fail(`tool call ${JSON.stringify(callID)} already exists`);
    if (this.state.tools.length >= this.limits.max_tool_calls) fail("tool call limit reached");
    this.state.tools.push({call_id: callID, name, arguments: args, status: "pending", output: "", error: ""});
  }

  toolResult(operation) {
    this.requireConnected();
    const index = this.findTool(operation.call_id ?? "");
    if (index < 0) fail(`unknown tool call ${JSON.stringify(operation.call_id ?? "")}`);
    const tool = this.state.tools[index];
    if (tool.status !== "pending") fail(`tool call ${JSON.stringify(tool.call_id)} is already ${tool.status}`);
    let wireOutput = operation.output ?? "";
    if (operation.status === "done") {
      if (operation.error) fail("a completed tool result cannot contain error");
    } else if (["failed", "declined"].includes(operation.status)) {
      if (!operation.error) fail(`${operation.status} tool result requires error`);
      wireOutput = JSON.stringify({error: operation.error});
    } else fail(`unknown tool result status ${JSON.stringify(operation.status ?? "")}`);
    this.emit(
      {type: "conversation.item.create", item: {type: "function_call_output", call_id: tool.call_id, output: wireOutput}},
      {type: "response.create"},
    );
    Object.assign(tool, {status: operation.status, output: operation.output ?? "", error: operation.error ?? ""});
  }

  responseDone(event) {
    const response = requireObject(event.response, "response");
    const id = requireString(response.id, "id", this.limits.max_string_bytes);
    const status = requireString(response.status, "status", this.limits.max_string_bytes);
    if (this.state.response.open && this.state.response.id && this.state.response.id !== id) fail(`response.done for ${JSON.stringify(id)} while ${JSON.stringify(this.state.response.id)} is open`);
    let reason = "";
    if (response.status_details !== undefined && response.status_details !== null) {
      const details = requireObject(response.status_details, "response.status_details");
      if (details.reason !== undefined && details.reason !== null) reason = requireString(details.reason, "response.status_details.reason", this.limits.max_string_bytes, true);
    }
    this.state.response = {id, open: false, status, reason};
  }

  itemTruncated(event) {
    const itemID = requireString(event.item_id, "item_id", this.limits.max_string_bytes);
    const audioEndMS = requireInteger(event.audio_end_ms, "audio_end_ms");
    this.state.last_truncation = {item_id: itemID, audio_end_ms: audioEndMS};
    for (const item of this.state.conversation) if (item.item_id === itemID) item.truncated_ms = audioEndMS;
  }

  observation(event) {
    const itemID = requireString(event.observation_id, "observation_id", this.limits.max_string_bytes);
    const observer = requireString(event.observer, "observer", this.limits.max_string_bytes);
    const text = requireString(event.text, "text", this.limits.max_string_bytes, true);
    let channel = `observation.${observer}`;
    if (event.source !== undefined && event.source !== null) {
      const source = requireString(event.source, "source", this.limits.max_string_bytes, true);
      if (source) channel = `observation.${source}`;
    }
    this.setConversation(itemID, "observation", channel, text, true);
  }

  findConversation(itemID, channel) {
    return this.state.conversation.findIndex((item) => item.item_id === itemID && item.channel === channel);
  }

  findTool(callID) {
    return this.state.tools.findIndex((tool) => tool.call_id === callID);
  }

  setConversation(itemID, role, channel, text, replace) {
    const index = this.findConversation(itemID, channel);
    if (index >= 0) {
      if (!replace) fail(`conversation item ${JSON.stringify(itemID)} channel ${JSON.stringify(channel)} already exists`);
      this.state.conversation[index].text = text;
      return;
    }
    if (this.state.conversation.length >= this.limits.max_conversation_items) fail("conversation item limit reached");
    this.state.conversation.push({item_id: itemID, role, channel, text, audio_deltas: 0, audio_done: false, truncated_ms: 0});
  }

  appendConversation(itemID, role, channel, delta) {
    const index = this.findConversation(itemID, channel);
    if (index < 0) {
      if (this.state.conversation.length >= this.limits.max_conversation_items) fail("conversation item limit reached");
      this.state.conversation.push({item_id: itemID, role, channel, text: delta, audio_deltas: 0, audio_done: false, truncated_ms: 0});
      return;
    }
    if (byteLength(this.state.conversation[index].text + delta) > this.limits.max_string_bytes) fail(`conversation text exceeds ${this.limits.max_string_bytes} bytes`);
    this.state.conversation[index].text += delta;
  }
}

export function runVector(vector, limits) {
  const reducer = new ClientReducer(limits);
  const errors = [];
  vector.steps.forEach((step, index) => {
    try {
      reducer.apply(step.at_ms, step.operation);
    } catch (error) {
      if (errors.length >= limits.max_errors_per_vector) fail(`vector ${JSON.stringify(vector.name)} exceeded error limit`);
      errors.push(`step ${index}: ${error.message}`);
    }
  });
  return {snapshot: reducer.snapshot(), outbound: reducer.outbound(), errors};
}
