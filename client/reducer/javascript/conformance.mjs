#!/usr/bin/env node
import {open} from "node:fs/promises";
import {resolve} from "node:path";
import {fileURLToPath} from "node:url";

import {loadCorpusText, MAX_CORPUS_BYTES, runVector, stableJSON} from "./reducer.mjs";

async function readBounded(path) {
  const file = await open(path, "r");
  try {
    const buffer = Buffer.alloc(MAX_CORPUS_BYTES + 1);
    const {bytesRead} = await file.read(buffer, 0, buffer.length, 0);
    if (bytesRead > MAX_CORPUS_BYTES) throw new Error(`reducer corpus exceeds ${MAX_CORPUS_BYTES} bytes`);
    return new TextDecoder("utf-8", {fatal: true}).decode(buffer.subarray(0, bytesRead));
  } finally {
    await file.close();
  }
}

export async function runConformance(path) {
  const corpus = loadCorpusText(await readBounded(path));
  const failures = [];
  for (const vector of corpus.vectors) {
    const actual = runVector(vector, corpus.limits);
    if (stableJSON(actual.snapshot) !== stableJSON(vector.expect.snapshot)) {
      failures.push(`${vector.name}: snapshot mismatch\nexpected ${stableJSON(vector.expect.snapshot)}\nactual   ${stableJSON(actual.snapshot)}`);
    }
    if (stableJSON(actual.outbound) !== stableJSON(vector.expect.outbound)) {
      failures.push(`${vector.name}: outbound mismatch\nexpected ${stableJSON(vector.expect.outbound)}\nactual   ${stableJSON(actual.outbound)}`);
    }
    if (stableJSON(actual.errors) !== stableJSON(vector.expect.errors)) {
      failures.push(`${vector.name}: errors mismatch\nexpected ${stableJSON(vector.expect.errors)}\nactual   ${stableJSON(actual.errors)}`);
    }
  }
  if (failures.length) throw new Error(failures.join("\n"));
  return corpus.vectors.length;
}

const invoked = process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url));
if (invoked) {
  const corpusPath = process.argv[2];
  if (!corpusPath) {
    console.error("usage: node client/reducer/javascript/conformance.mjs <corpus.json>");
    process.exitCode = 2;
  } else {
    try {
      const count = await runConformance(corpusPath);
      console.log(`javascript conformance: ${count} vectors passed`);
    } catch (error) {
      console.error(`javascript conformance failed: ${error.message}`);
      process.exitCode = 1;
    }
  }
}
