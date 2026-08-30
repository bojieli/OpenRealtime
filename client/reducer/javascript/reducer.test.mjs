import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {dirname, resolve} from "node:path";
import test from "node:test";
import {fileURLToPath} from "node:url";

import {
  ClientReducer,
  DEFAULT_LIMITS,
  loadCorpusText,
  MAX_CORPUS_BYTES,
  parseStrictJSON,
  runVector,
  stableJSON,
} from "./reducer.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const corpusText = readFileSync(resolve(here, "../testdata/reducer_vectors.json"), "utf8");

test("JavaScript reducer passes every canonical vector", () => {
  const corpus = loadCorpusText(corpusText);
  for (const vector of corpus.vectors) {
    const actual = runVector(vector, corpus.limits);
    assert.equal(stableJSON(actual.snapshot), stableJSON(vector.expect.snapshot), `${vector.name} snapshot`);
    assert.equal(stableJSON(actual.outbound), stableJSON(vector.expect.outbound), `${vector.name} outbound`);
    assert.deepEqual(actual.errors, vector.expect.errors, `${vector.name} errors`);
  }
});

test("strict parser rejects literal and escaped duplicate keys", () => {
  assert.throws(() => parseStrictJSON('{"a":1,"a":2}'), /duplicate JSON key/);
  assert.throws(() => parseStrictJSON('{"a":1,"\\u0061":2}'), /duplicate JSON key/);
  assert.throws(() => parseStrictJSON('{"event":{"id":1,"id":2}}'), /duplicate JSON key/);
});

test("strict parser rejects trailing values and non-finite numbers", () => {
  assert.throws(() => parseStrictJSON('{} {}'), /trailing JSON token/);
  assert.throws(() => parseStrictJSON('{"n":1e9999}'), /non-finite JSON number/);
  assert.throws(() => parseStrictJSON("[".repeat(257) + "0" + "]".repeat(257)), /nesting exceeds/);
});

test("typed corpus rejects unknown fields and oversized input", () => {
  assert.throws(
    () => loadCorpusText(corpusText.replace('"format_version": 1,', '"format_version": 1, "surprise": true,')),
    /unknown field/,
  );
  assert.throws(
    () => loadCorpusText(corpusText.replace('{"kind": "connected"}', '{"kind": "connected", "speaking": false}')),
    /unknown field/,
  );
  assert.throws(() => loadCorpusText(" ".repeat(MAX_CORPUS_BYTES + 1)), /exceeds/);
});

test("failed operations are atomic and returned state is detached", () => {
  const machine = new ClientReducer(DEFAULT_LIMITS);
  machine.apply(0, {kind: "connect", transport: "websocket"});
  machine.apply(1, {kind: "connected"});
  machine.apply(2, {kind: "inbound", event: {type: "response.created", response: {id: "r", status: "in_progress"}}});
  const before = machine.snapshot();
  assert.throws(
    () => machine.apply(3, {kind: "inbound", event: {type: "response.output_audio.delta", item_id: "a", delta: "%%%"}}),
    /not base64/,
  );
  assert.deepEqual(machine.snapshot(), before);
  const detached = machine.snapshot();
  detached.response.id = "mutated";
  assert.equal(machine.snapshot().response.id, "r");
});

test("published cross-language limits cannot be changed implicitly", () => {
  const limits = {...DEFAULT_LIMITS, max_outbound_events: 1};
  assert.throws(() => new ClientReducer(limits), /unsupported reducer limits/);
  // The published limits are immutable across languages; a corpus cannot
  // silently lower one runtime's bound and thereby change its semantics.
});
