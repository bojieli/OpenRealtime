const DIGEST = /^sha256:[0-9a-f]{64}$/;
const SYMBOL = /^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$/;
const NODE = /^[A-Za-z_][A-Za-z0-9_-]*$/;
const MAX_SOURCE_BYTES = 1 << 20;
const MAX_ITEMS = 65_536;
const MAX_RENAME_EDITS = 8_192;
const MAX_IDENTIFIER_BYTES = 256;
const MAX_TEXT_BYTES = 64 << 20;
const MAX_DIAGNOSTIC_BYTES = 64 << 10;
const MAX_DIAGNOSTIC_NOTES = 256;
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
const SCHEMA_TYPES = new Set(["array", "boolean", "integer", "null", "number", "object", "string"]);
const TOPOLOGY_WHITESPACE = new Set([" ", "\t", "\r", "\n"]);
const ENDPOINT_TAIL = new Set(["->", "=>", ";"]);

function object(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} is not an object`);
  return value;
}

function only(value, fields, label) {
  object(value, label);
  const allowed = new Set(fields);
  for (const field of Object.keys(value)) {
    if (!allowed.has(field)) throw new Error(`${label} has unknown field ${field}`);
  }
}

function digest(value, label) {
  if (typeof value !== "string" || !DIGEST.test(value)) throw new Error(`${label} is not a canonical digest`);
  return value;
}

function identity(value, label) {
  only(value, ["name", "revision", "digest"], label);
  if (typeof value.name !== "string" || !SYMBOL.test(value.name) ||
      !Number.isSafeInteger(value.revision) || value.revision < 1) throw new Error(`${label} is invalid`);
  digest(value.digest, `${label} digest`);
}

function rows(value, label) {
  if (!Array.isArray(value) || value.length > MAX_ITEMS) throw new Error(`${label} is not a bounded array`);
  return value;
}

function frozen(value) {
  const copy = structuredClone(value);
  const visit = (entry) => {
    if (!entry || typeof entry !== "object" || Object.isFrozen(entry)) return entry;
    for (const child of Object.values(entry)) visit(child);
    return Object.freeze(entry);
  };
  return visit(copy);
}

function document(input, analyze = false) {
  only(input, ["path", "source", "lock", "channel_depth", "revision"], "authoring document");
  if (typeof input.path !== "string" || input.path.length === 0 || input.path.length > 4096 ||
      input.path.trim() !== input.path || /[\0\r\n]/.test(input.path) ||
      typeof input.source !== "string" || encoder.encode(input.source).byteLength === 0 ||
      encoder.encode(input.source).byteLength > MAX_SOURCE_BYTES ||
      !/\.(?:ortg|ya?ml|json)$/i.test(input.path) || (analyze && !/\.ortg$/i.test(input.path)) ||
      (input.revision !== undefined && (!Number.isSafeInteger(input.revision) || input.revision < 0))) {
    throw new Error("authoring document is invalid");
  }
  if (input.channel_depth !== undefined) {
    object(input.channel_depth, "channel-depth overrides");
    for (const [edge, depth] of Object.entries(input.channel_depth)) {
      if (!edge || edge.trim() !== edge || !Number.isSafeInteger(depth) || depth < 1 || depth > 1 << 20) {
        throw new Error("authoring channel-depth override is invalid");
      }
    }
  }
  if (input.lock !== undefined && input.lock !== null) validateLock(input.lock);
  return frozen(input);
}

function validateLock(lock) {
  only(lock, ["format_version", "elements"], "resolution lock");
  if (lock.format_version !== 1) throw new Error("resolution lock format is invalid");
  const entries = rows(lock.elements, "resolution entries");
  const references = new Set();
  for (const entry of entries) {
    only(entry, ["reference", "identity"], "resolution entry");
    if (typeof entry.reference !== "string" || entry.reference.length === 0 || references.has(entry.reference)) {
      throw new Error("resolution lock has an invalid or duplicate reference");
    }
    references.add(entry.reference);
    identity(entry.identity, "resolved element identity");
  }
}

function validateGraph(graph, fingerprint = graph?.fingerprint) {
  object(graph, "graph IR");
  if (graph.format_version !== 1 || typeof graph.id !== "string" || graph.id.trim() === "" ||
      !Number.isSafeInteger(graph.revision) || graph.revision < 1 || graph.fingerprint !== fingerprint ||
      !DIGEST.test(graph.fingerprint) || !Array.isArray(graph.nodes) || graph.nodes.length === 0 ||
      graph.nodes.length > MAX_ITEMS) {
    throw new Error("compiled graph IR is invalid");
  }
  const seen = new Set();
  for (const node of graph.nodes) {
    object(node, "graph node");
    if (typeof node.id !== "string" || node.id.length === 0 || seen.has(node.id) ||
        !Array.isArray(node.ports) || node.ports.length === 0 || node.ports.length > MAX_ITEMS) {
      throw new Error("compiled graph node is invalid");
    }
    seen.add(node.id);
    identity(node.element, "graph element identity");
  }
}

async function sourceDigest(source) {
  const bytes = new Uint8Array(await crypto.subtle.digest("SHA-256", encoder.encode(source)));
  return `sha256:${[...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function validateReport(report, collection, label) {
  only(report, [collection, "total", "incomplete"], label);
  const values = rows(report[collection], `${label} items`);
  if (!Number.isSafeInteger(report.total) || report.total < values.length || report.total > MAX_ITEMS ||
      Boolean(report.incomplete) !== (values.length < report.total)) {
    throw new Error(`${label} bounds are invalid`);
  }
  return values;
}

function sourcePositions(source, offsets) {
  const requested = new Set(offsets);
  const result = new Map();
  if (requested.size === 0) return result;
  let offset = 0;
  let line = 1;
  let column = 1;
  if (requested.has(0)) result.set(0, Object.freeze({ line, column }));
  for (const character of source) {
    offset += encoder.encode(character).byteLength;
    if (character === "\n") {
      line++;
      column = 1;
    } else {
      column++;
    }
    if (requested.has(offset)) result.set(offset, Object.freeze({ line, column }));
    if (result.size === requested.size) break;
  }
  return result;
}

function sourcePosition(value, positions, label) {
  only(value, ["offset", "line", "column"], label);
  if (!Number.isSafeInteger(value.offset) || value.offset < 0 ||
      !Number.isSafeInteger(value.line) || value.line < 1 ||
      !Number.isSafeInteger(value.column) || value.column < 1) {
    throw new Error(`${label} is invalid`);
  }
  const actual = positions?.get(value.offset);
  if (positions && (!actual || actual.line !== value.line || actual.column !== value.column)) {
    throw new Error(`${label} does not name a source boundary`);
  }
  return value.offset;
}

function sourceSpan(value, positions, label) {
  only(value, ["start", "end"], label);
  const start = sourcePosition(value.start, positions, `${label} start`);
  const end = sourcePosition(value.end, positions, `${label} end`);
  if (end < start) throw new Error(`${label} is reversed`);
  return start;
}

function sameBytes(left, right) {
  if (left.byteLength !== right.byteLength) return false;
  for (let index = 0; index < left.byteLength; index++) {
    if (left[index] !== right[index]) return false;
  }
  return true;
}

function nodeName(value, label) {
  if (typeof value !== "string" || !NODE.test(value) ||
      encoder.encode(value).byteLength > MAX_IDENTIFIER_BYTES) {
    throw new Error(`${label} is not a canonical node identifier`);
  }
  return value;
}

// Tokenize only the syntax needed to prove node-reference coverage. Canonical
// .ortg identifiers and punctuation are ASCII; comments and import strings are
// skipped while byte offsets continue to count the original UTF-8 source.
function topologyTokens(source) {
  const tokens = [];
  let index = 0;
  let offset = 0;
  const advance = () => {
    const point = source.codePointAt(index);
    const character = String.fromCodePoint(point);
    index += character.length;
    offset += encoder.encode(character).byteLength;
    return character;
  };
  const advanceASCII = (count) => {
    index += count;
    offset += count;
  };
  const identifierStart = (character) => /[A-Za-z_]/.test(character);
  const identifierContinue = (character) => /[A-Za-z0-9_-]/.test(character);
  while (index < source.length) {
    const character = source[index];
    if (TOPOLOGY_WHITESPACE.has(character)) {
      advance();
      continue;
    }
    if (source.startsWith("//", index) || character === "#") {
      advanceASCII(character === "#" ? 1 : 2);
      while (index < source.length && source[index] !== "\r" && source[index] !== "\n") advance();
      continue;
    }
    if (source.startsWith("/*", index)) {
      advanceASCII(2);
      while (index < source.length && !source.startsWith("*/", index)) advance();
      if (!source.startsWith("*/", index)) throw new Error("authoring source has an unterminated comment");
      advanceASCII(2);
      continue;
    }
    if (character === '"') {
      advanceASCII(1);
      let escaped = false;
      let closed = false;
      while (index < source.length) {
        const current = advance();
        if (escaped) {
          escaped = false;
        } else if (current === "\\") {
          escaped = true;
        } else if (current === '"') {
          closed = true;
          break;
        } else if (current === "\r" || current === "\n") {
          throw new Error("authoring source has a multiline string");
        }
      }
      if (!closed) throw new Error("authoring source has an unterminated string");
      continue;
    }
    const compound = ["::", "->", "=>"].find((value) => source.startsWith(value, index));
    if (compound) {
      tokens.push(Object.freeze({ text: compound, start: offset, end: offset + 2 }));
      advanceASCII(2);
      continue;
    }
    if ("{};.=".includes(character)) {
      tokens.push(Object.freeze({ text: character, start: offset, end: offset + 1 }));
      advanceASCII(1);
      continue;
    }
    if (identifierStart(character)) {
      const start = offset;
      let text = advance();
      while (index < source.length && identifierContinue(source[index]) &&
          !(source[index] === "-" && source.startsWith("->", index))) {
        text += advance();
      }
      tokens.push(Object.freeze({ text, start, end: offset, identifier: true }));
      continue;
    }
    throw new Error("authoring source is not lexical .ortg");
  }
  return tokens;
}

function renameReferenceSpans(source, selected, replacement) {
  const tokens = topologyTokens(source);
  const declarations = tokens.filter((token, index) => token.identifier && tokens[index - 1]?.text === "::");
  if (declarations.filter((token) => token.text === selected).length !== 1) {
    throw new Error("authoring rename source does not declare the selected node exactly once");
  }
  if (replacement !== selected && declarations.some((token) => token.text === replacement)) {
    throw new Error("authoring rename target already exists");
  }
  const references = tokens.filter((token, index) => token.identifier && token.text === selected &&
    (tokens[index - 1]?.text === "::" || (tokens[index + 1]?.text === "." &&
      tokens[index + 2]?.identifier && ENDPOINT_TAIL.has(tokens[index + 3]?.text))));
  if (references.length === 0 || references.length > MAX_RENAME_EDITS) {
    throw new Error("authoring rename reference count is invalid");
  }
  return references.map(({ start, end }) => Object.freeze({ start, end }));
}

function checkedEditSet(value, input, expectedDigest) {
  only(value, ["path", "source_digest", "edits"], "authoring edit set");
  if (value.path !== input.path || value.source_digest !== expectedDigest) {
    throw new Error("authoring edit set names another source");
  }
  digest(value.source_digest, "authoring edit set source digest");
  const sourceBytes = encoder.encode(input.source);
  const values = rows(value.edits, "authoring edits");
  const offsets = [];
  for (const [index, edit] of values.entries()) {
    only(edit, ["span", "old_text", "new_text"], `authoring edit ${index}`);
    sourceSpan(edit.span, null, `authoring edit ${index} span`);
    offsets.push(edit.span.start.offset, edit.span.end.offset);
    if (typeof edit.old_text !== "string" || typeof edit.new_text !== "string" ||
        encoder.encode(edit.old_text).byteLength > MAX_SOURCE_BYTES ||
        encoder.encode(edit.new_text).byteLength > MAX_SOURCE_BYTES) {
      throw new Error(`authoring edit ${index} text exceeds its bound`);
    }
  }
  const positions = sourcePositions(input.source, offsets);
  const edits = values.map((edit, index) => {
    const start = sourceSpan(edit.span, positions, `authoring edit ${index} span`);
    const end = edit.span.end.offset;
    const oldBytes = encoder.encode(edit.old_text);
    const newBytes = encoder.encode(edit.new_text);
    if (!sameBytes(sourceBytes.subarray(start, end), oldBytes)) {
      throw new Error(`authoring edit ${index} has stale old text`);
    }
    return { start, end, newBytes };
  }).sort((left, right) => left.start - right.start || left.end - right.end);

  let previous = 0;
  let resultBytes = sourceBytes.byteLength;
  for (const [index, edit] of edits.entries()) {
    if (edit.start < previous) throw new Error(`authoring edit ${index} overlaps a previous edit`);
    previous = edit.end;
    resultBytes += edit.newBytes.byteLength - (edit.end - edit.start);
    if (!Number.isSafeInteger(resultBytes) || resultBytes < 1 || resultBytes > MAX_SOURCE_BYTES) {
      throw new Error("formatted authoring source exceeds its bound");
    }
  }

  const output = new Uint8Array(resultBytes);
  let sourceOffset = 0;
  let outputOffset = 0;
  for (const edit of edits) {
    const unchanged = sourceBytes.subarray(sourceOffset, edit.start);
    output.set(unchanged, outputOffset);
    outputOffset += unchanged.byteLength;
    output.set(edit.newBytes, outputOffset);
    outputOffset += edit.newBytes.byteLength;
    sourceOffset = edit.end;
  }
  output.set(sourceBytes.subarray(sourceOffset), outputOffset);
  return Object.freeze({ editSet: frozen(value), source: decoder.decode(output) });
}

function validateRename(value, input, selected, replacement, requestedDigest, evidence) {
  only(value, ["node", "new_name", "edits"], "authoring rename result");
  if (nodeName(value.node, "authoring rename result node") !== selected ||
      nodeName(value.new_name, "authoring rename result replacement") !== replacement ||
      evidence !== `authoring:rename:${requestedDigest}`) {
    throw new Error("authoring rename result changed request identity");
  }
  const references = renameReferenceSpans(input.source, selected, replacement);
  const applied = checkedEditSet(value.edits, input, requestedDigest);
  if (applied.editSet.edits.length > MAX_RENAME_EDITS) {
    throw new Error("authoring rename edit set exceeds its bound");
  }
  for (const edit of applied.editSet.edits) {
    if (edit.old_text !== selected || edit.new_text !== replacement) {
      throw new Error("authoring rename result contains another text mutation");
    }
  }
  if (selected === replacement) {
    if (applied.editSet.edits.length !== 0 || applied.source !== input.source) {
      throw new Error("authoring no-op rename returned edits");
    }
  } else {
    if (applied.editSet.edits.length !== references.length || applied.source === input.source) {
      throw new Error("authoring rename result omitted a node reference");
    }
    const expected = new Set(references.map(({ start, end }) => `${start}:${end}`));
    for (const edit of applied.editSet.edits) {
      const key = `${edit.span.start.offset}:${edit.span.end.offset}`;
      if (!expected.delete(key)) throw new Error("authoring rename result edits another source span");
    }
    if (expected.size !== 0) throw new Error("authoring rename result omitted a node reference");
  }
  return frozen(value);
}

function validateDiagnostics(report, source) {
  const diagnostics = validateReport(report, "items", "diagnostic report");
  const offsets = [];
  let previous = -1;
  for (const diagnostic of diagnostics) {
    only(diagnostic, ["code", "severity", "path", "span", "message", "notes"], "diagnostic");
    if (typeof diagnostic.code !== "string" || diagnostic.code.length === 0 ||
        diagnostic.code.trim() !== diagnostic.code ||
        encoder.encode(diagnostic.code).byteLength > 256 ||
        !new Set(["error", "warning"]).has(diagnostic.severity) ||
        typeof diagnostic.message !== "string" || diagnostic.message.length === 0 ||
        encoder.encode(diagnostic.message).byteLength > MAX_DIAGNOSTIC_BYTES) {
      throw new Error("analysis diagnostic is invalid");
    }
    const diagnosticPath = diagnostic.path ?? "";
    if (typeof diagnosticPath !== "string" || /[\0\r\n]/.test(diagnosticPath) ||
        encoder.encode(diagnosticPath).byteLength > MAX_DIAGNOSTIC_BYTES) {
      throw new Error("analysis diagnostic path is invalid");
    }
    const start = sourceSpan(diagnostic.span, null, "diagnostic span");
    if (start < previous) throw new Error("analysis diagnostics are not in source order");
    previous = start;
    offsets.push(diagnostic.span.start.offset, diagnostic.span.end.offset);
    const notes = rows(diagnostic.notes ?? [], "diagnostic notes");
    if (notes.length > MAX_DIAGNOSTIC_NOTES || notes.some((note) =>
      typeof note !== "string" || encoder.encode(note).byteLength > MAX_DIAGNOSTIC_BYTES)) {
      throw new Error("analysis diagnostic notes are invalid");
    }
  }
  const positions = sourcePositions(source, offsets);
  for (const diagnostic of diagnostics) sourceSpan(diagnostic.span, positions, "diagnostic span");
  return diagnostics;
}

function boundedString(value, label, required = false) {
  if (typeof value !== "string" || (required && value.length === 0) ||
      encoder.encode(value).byteLength > MAX_TEXT_BYTES) {
    throw new Error(`${label} is invalid`);
  }
  return value;
}

function jsonPointerProperty(name) {
  return `#/properties/${name.replaceAll("~", "~0").replaceAll("/", "~1")}`;
}

function validatePropertyMetadata(property, previous) {
  only(property, ["name", "pointer", "required", "types", "title", "description", "format",
    "default", "enum", "schema"], "configuration property metadata");
  boundedString(property.name, "configuration property name", true);
  if (property.name <= previous || property.pointer !== jsonPointerProperty(property.name) ||
      (property.required !== undefined && typeof property.required !== "boolean") ||
      !Object.hasOwn(property, "schema")) {
    throw new Error("configuration property metadata is not canonical");
  }
  const types = property.types ?? [];
  if (!Array.isArray(types) || types.length > SCHEMA_TYPES.size) {
    throw new Error("configuration property types are invalid");
  }
  let previousType = "";
  for (const type of types) {
    if (!SCHEMA_TYPES.has(type) || type <= previousType) {
      throw new Error("configuration property types are not canonical");
    }
    previousType = type;
  }
  for (const field of ["title", "description", "format"]) {
    if (property[field] !== undefined) boundedString(property[field], `configuration property ${field}`);
  }
  if (property.enum !== undefined && (!Array.isArray(property.enum) || property.enum.length > MAX_ITEMS)) {
    throw new Error("configuration property enum is invalid");
  }
  for (const value of [property.schema, ...(property.enum ?? []),
    ...(Object.hasOwn(property, "default") ? [property.default] : [])]) {
    if (encoder.encode(JSON.stringify(value)).byteLength > MAX_TEXT_BYTES) {
      throw new Error("configuration property JSON exceeds its bound");
    }
  }
  return property.name;
}

function validateConfigMetadata(config) {
  only(config, ["artifact", "resolved", "schema_reference", "inline_topology_values",
    "empty_object_only", "schema_status", "schema_id", "schema_digest", "properties_complete",
    "properties", "additional_properties"], "element configuration metadata");
  boundedString(config.artifact, "configuration artifact", true);
  if (config.resolved !== true || config.inline_topology_values !== false ||
      typeof config.empty_object_only !== "boolean" || typeof config.properties_complete !== "boolean" ||
      !Array.isArray(config.properties) || config.properties.length > MAX_ITEMS) {
    throw new Error("configuration metadata changed the topology/value boundary");
  }
  let previous = "";
  for (const property of config.properties) previous = validatePropertyMetadata(property, previous);
  switch (config.schema_status) {
    case "empty-object-only":
      if (!config.empty_object_only || !config.properties_complete || config.properties.length !== 0 ||
          config.schema_reference !== undefined || config.schema_id !== undefined ||
          config.schema_digest !== undefined || config.additional_properties !== undefined) {
        throw new Error("empty-object configuration metadata is inconsistent");
      }
      break;
    case "unresolved":
    case "invalid":
      boundedString(config.schema_reference, "configuration schema reference", true);
      if (config.empty_object_only || config.properties_complete || config.properties.length !== 0 ||
          config.schema_id !== undefined || config.schema_digest !== undefined ||
          config.additional_properties !== undefined) {
        throw new Error("unresolved configuration metadata invented fields");
      }
      break;
    case "resolved":
      boundedString(config.schema_reference, "configuration schema reference", true);
      boundedString(config.schema_id, "configuration schema identity", true);
      digest(config.schema_digest, "configuration schema digest");
      if (config.empty_object_only) throw new Error("resolved configuration metadata is inconsistent");
      if (config.additional_properties !== undefined &&
          encoder.encode(JSON.stringify(config.additional_properties)).byteLength > MAX_TEXT_BYTES) {
        throw new Error("configuration additional-properties metadata exceeds its bound");
      }
      break;
    default:
      throw new Error("configuration schema status is invalid");
  }
}

function validateAnalysis(value, input, requestedDigest, evidence) {
  only(value, ["source_digest", "parsed", "recovered", "canonical", "diagnostics", "catalog", "formatting"],
    "analysis result");
  if (value.source_digest !== requestedDigest || evidence !== `authoring:analyze:${requestedDigest}` ||
      typeof value.parsed !== "boolean" || typeof value.recovered !== "boolean" ||
      typeof value.canonical !== "boolean" || value.parsed === value.recovered ||
      (value.canonical && !value.parsed)) {
    throw new Error("analysis result changed source identity");
  }
  validateDiagnostics(value.diagnostics, input.source);
  const metadata = validateReport(value.catalog, "elements", "metadata report");
  let previous = "";
  for (const element of metadata) {
    object(element, "element metadata");
    identity(element.identity, "metadata element identity");
    if (previous && element.identity.name <= previous) throw new Error("element metadata is not canonical");
    previous = element.identity.name;
    validateConfigMetadata(element.config);
  }
  if (value.parsed) {
    if (value.formatting === undefined || value.formatting === null) {
      throw new Error("parsed analysis omitted its formatter edit set");
    }
    const formatting = checkedEditSet(value.formatting, input, requestedDigest);
    if (value.canonical && (formatting.editSet.edits.length !== 0 || formatting.source !== input.source)) {
      throw new Error("canonical analysis proposed source changes");
    }
    if (!value.canonical && (formatting.editSet.edits.length === 0 || formatting.source === input.source)) {
      throw new Error("noncanonical analysis omitted its source changes");
    }
  } else if (value.formatting !== undefined) {
    throw new Error("recovered analysis proposed formatter edits");
  }
  return frozen(value);
}

function validateCompile(value, input, evidence) {
  only(value, ["graph", "lock"], "compile result");
  validateGraph(value.graph);
  validateLock(value.lock);
  const revision = input.revision || 1;
  if (value.graph.revision !== revision || evidence !== `authoring:compile:${value.graph.fingerprint}`) {
    throw new Error("compile result changed graph identity");
  }
  return frozen(value);
}

function validateRender(value, graph, format, evidence) {
  only(value, ["fingerprint", "format", "model", "text"], "render result");
  if (value.fingerprint !== graph.fingerprint || value.format !== format ||
      evidence !== `authoring:render:${format}:${graph.fingerprint}`) {
    throw new Error("render result changed graph identity");
  }
  if (format === "model") {
    if (!value.model || value.text !== undefined) throw new Error("model rendering has an invalid shape");
  } else if (typeof value.text !== "string" || encoder.encode(value.text).byteLength > MAX_TEXT_BYTES ||
      value.model !== undefined) {
    throw new Error("text rendering has an invalid shape");
  }
  return frozen(value);
}

export default {
  name: "openrealtime.presentation.client.management-authoring",
  revision: 1,
  async mount(context) {
    const transport = context.services.get("presentation.client.management_transport");
    if (!transport) throw new Error("authoring management transport is unavailable");
    let disposed = false;
    const ready = () => {
      if (disposed) throw new Error("authoring management client is disposed");
    };
    context.publish("presentation.client.management_authoring", Object.freeze({
      async analyze(input) {
        ready();
        const exact = document(input, true);
        const fingerprint = await sourceDigest(exact.source);
        const response = await transport.authoring("analyze", exact);
        ready();
        return validateAnalysis(response.value, exact, fingerprint, response.identity);
      },
      async compile(input) {
        ready();
        const exact = document(input, false);
        const response = await transport.authoring("compile", exact);
        ready();
        return validateCompile(response.value, exact, response.identity);
      },
      async render(graph, format = "model") {
        ready();
        validateGraph(graph);
        if (!new Set(["model", "mermaid", "dot"]).has(format)) throw new Error("render format is invalid");
        const response = await transport.authoring("render", { graph, format });
        ready();
        return validateRender(response.value, graph, format, response.identity);
      },
    }));
    context.publish("presentation.client.management_editing", Object.freeze({
      async rename(input, selected, replacement) {
        ready();
        const exact = document(input, true);
        if ((exact.lock !== undefined && exact.lock !== null) ||
            (exact.channel_depth !== undefined && Object.keys(exact.channel_depth).length !== 0)) {
          throw new Error("authoring rename does not accept resolution or channel-depth planes");
        }
        const node = nodeName(selected, "authoring rename node");
        const newName = nodeName(replacement, "authoring rename replacement");
        renameReferenceSpans(exact.source, node, newName);
        const fingerprint = await sourceDigest(exact.source);
        const response = await transport.authoring("rename", {
          document: exact, node, new_name: newName,
        });
        ready();
        return validateRename(response.value, exact, node, newName, fingerprint, response.identity);
      },
      async applyEdits(input, editSet) {
        ready();
        const exact = document(input, true);
        const fingerprint = await sourceDigest(exact.source);
        ready();
        return checkedEditSet(editSet, exact, fingerprint).source;
      },
    }));
    context.lifecycle.defer("management-authoring", () => { disposed = true; });
  },
};
