# External benchmark adapters

External corpora are not committed to this repository. A manifest pins each
upstream revision, download identity, byte length, digest, observed sample
count, and license statement. Downloading or using an archive remains an
explicit user action subject to its upstream terms.

## τ-Voice

`tau-voice.manifest.json` pins Sierra Research's `tau2-bench` revision
`c3398666e6559e3a063da3fc04b5acf7f941464e` and the τ-Voice paper. The
benchmark combines 278 grounded airline/retail/telecom tasks with full-duplex
speech, real environment tools, multi-turn policy following, control/regular
speech conditions, and tick-level interaction metrics. Its default tick is
200 ms.

This is the primary joint intelligence/interaction benchmark for OpenRealtime.
A checked patch preserves the
standard upstream OpenAI adapter while adding an explicit local endpoint and a
separately attributed Fish Audio caller synthesizer; 86 affected upstream tests
pass from a fresh pinned checkout. The persistent gateway passes 12/12 selected
official provider tests, and the seven-persona Fish voice registry records its
generated-source provenance. A two-cell full run is in progress/queued. A
patch test, mock-tool, text-only, exploratory task, or incomplete cell must not
be reported as a τ-Voice score. See
the [integration and evaluation plan](../tau-voice/README.md).

## Full-Duplex-Bench v1.5

The Go `livebench` runner consumes the original paired `input.wav`,
`clean_input.wav`, and `metadata.json` files without modifying them. It streams
each waveform at wall-clock speed and writes aligned provider audio, a
credential-free protocol trace, provider/model identity, input/output hashes,
usage, and neutral timing observations.

The pinned official archives contain 498 complete paired cases:

| Scenario | Complete pairs |
| --- | ---: |
| Background speech | 100 |
| Talking to another person | 100 |
| Listener backchannel | 98 |
| User interruption | 200 |

The upstream README lists 99 listener-backchannel cases, but the valid official
archive contains 98 directories. The manifest preserves that discrepancy.

Inspect a downloaded corpus without making provider calls:

```sh
go run ./cmd/livebench inspect --dataset-root /path/to/fdb-v1.5
```

Create a credential-free, ten-sample paired run plan:

```sh
go run ./cmd/livebench run \
  --dataset-root /path/to/fdb-v1.5 \
  --provider gemini \
  --scenario user_interruption \
  --limit 10 \
  --selection even \
  --conditions overlap,clean \
  --dry-run
```

Live calls read only the provider-specific environment variable. Result JSON
never stores an API key or base64 audio payload.

Run a complete scenario with bounded retries and resumable provenance:

```sh
go run ./cmd/livebench run \
  --dataset-root /path/to/fdb-v1.5 \
  --output-root /path/to/results/user_interruption \
  --provider gemini \
  --model gemini-3.1-flash-live-preview \
  --scenario user_interruption \
  --conditions overlap \
  --trial-attempts 3 \
  --retry-delay 1s \
  --resume=true
```

Each process invocation makes at most `--trial-attempts` calls for an
unfinished trial, using exponential backoff capped at 30 seconds. The atomic
manifest keeps every attempt's timestamps, duration, outcome, and error. Resume
first verifies that the provider, exact model, conditions, replicates, and
ordered sample plan match; it then preserves the attempt ledger and verifies
input/output hashes before reusing a completed trial.

Recompute local timing observations after a scorer update without repeating
provider calls, then create a deterministic aggregate:

```sh
go run ./cmd/livebench rescore --manifest /path/to/run-provider-model.json
go run ./cmd/livebench summarize \
  --manifest /path/to/run-provider-model.json \
  --output /path/to/summary.json
```

### Provider profiles

The Gemini profile follows the pinned upstream Gemini 3.1 inference policy:
1,024-sample 16 kHz frames, `AUDIO` output, minimal thinking, no added system
instruction or transcription service, and a fresh Live session after a
provider interruption/turn-completion event. OpenAI uses the GA Realtime
session shape and exact requested model snapshot. Groq is identified as a
cascade—not a native live model—with energy VAD, `whisper-large-v3-turbo`,
`openai/gpt-oss-120b`, and `canopylabs/orpheus-v1-english` (`autumn`).

The local scorer preserves the interval/de-duplication definitions from the
upstream `get_timing.py`, but uses the named deterministic Go energy VAD rather
than claiming equivalence to Silero. This distinction makes local timing useful
for paired comparisons while preventing false equivalence with paper values.

### Published reference data

`full-duplex-bench-v1.5-published-gpt4o.json` transcribes Table 2's historical
GPT-4o Realtime result from arXiv v4. It is external reference data, not a local
run: the paper used `gpt-4o-realtime-preview-2024-12-17`, the `alloy` voice,
Silero-VAD, and a GPT-4o behavior judge. Reports must keep it separate from
locally observed results and confidence intervals.

## Full-Duplex-Bench v3 tool-use benchmark

`full-duplex-bench-v3-published-results.json` transcribes Tables 2–6 and the
paper-reported pre-emptive tool-call rates from arXiv `2604.04847v1`. The source
PDF digest and benchmark repository revision are pinned. It contains published
results for `gpt-realtime-1.5`, Gemini Live 2.5 and 3.1, xAI Grok, Ultravox
v0.7, and a Whisper→GPT-4o→OpenAI TTS cascade.

The published artifact is not a local run. It explicitly preserves that
GPT-Realtime is not GPT-4o Realtime, xAI Grok is not Groq, and the cascade's
GPT-4o component is not a GPT-4o Realtime result. Its argument and response
metrics also depend on GPT-4o judges.

## Tool-use benchmark execution

The v3 manifest now pins the exact 736,136,419-byte released bundle and its 100
recordings, 79 unique released scenario IDs, 150 expected calls, 17 rollback
examples, official 12-tool catalog, mock APIs, runner, and evaluator. The
direct Go runner streams every recording through the standard OpenAI Realtime
adapter pointed at the local endpoint. Authoritative function calls execute the
upstream mock semantics; all results are sent as standard
`function_call_output` items, followed by one `response.create`. The adapter
then waits for a completed terminal response rather than silently truncating a
high-thinking continuation at a fixed receive tail.

Prepare and run the exact released population:

```sh
scripts/prepare-fdb-v3.sh
OPENREALTIME_API_KEY="$OPENREALTIME_GATEWAY_TOKEN" \
  scripts/run-fdb-v3-openrealtime.sh
```

The run writes upstream-compatible `output_openrealtime.wav` and
`result_openrealtime.json` files, a resumable immutable manifest, exact
evaluation, and—when enabled—the official GPT-4o argument/response judgment.
It is queued but not yet a completed local score. The wire transcript and VAD
end event are retained as local evidence; paper-identical Parakeet alignment
has not been claimed.

The v3 release card calls the audio “100 examples, 79 unique scenarios,” while
the pinned `benchmark_data_v2.json` contains 100 unique definition IDs. The
manifest records both observations and uses `definition_entries` rather than
silently treating examples, recordings, and scenario designs as identical.

## FD-Bench long-form interaction matrix

`fd-bench.manifest.json` pins Peng et al.'s separate FD-Bench repository,
Hugging Face dataset revision, 13 archives, 8,310,251,185 bytes, and source
digests. This is not Full-Duplex-Bench v1.5 or v3. The archives expand to 21
cells that vary ChatTTS, CosyVoice2, and F5-TTS inputs across difficulty, noise
placement, and three SNR levels. Full inspection finds 6,147 playable
conversations and 77.2184 hours of input. The paper and ground-truth file
describe 293 conversations per cell. Only the three ChatTTS cells omit IDs 60
and 120 and therefore contain 291; all other cells contain 293. The adapter
never synthesizes replacements.

Prepare and inspect the complete external release:

```sh
scripts/prepare-fdbench.sh
go run ./cmd/fdbench inspect --dataset-root .runtime/fd-bench/dataset
```

Run all conditions after the primary and ASR-ablation queues:

```sh
OPENREALTIME_API_KEY="$OPENREALTIME_GATEWAY_TOKEN" \
  scripts/run-fdbench-openrealtime.sh
```

The Go runner streams 20 ms PCM frames at wall-clock speed through the same
standard OpenAI Realtime adapter and records aligned playback, hashes,
credential-free wire evidence, bounded retries, and an immutable resume
manifest. It retains the upstream clients' fixed 10-second post-input window.
Since 188/291 files in the inspected clean ChatTTS cell leave less than 500 ms
after their last annotated speech, a declared 600 ms zero-PCM finalizer is
streamed inside—not in addition to—that window so standard server VAD can close
the last utterance.
Finalization uses Silero-VAD 6.2.1 with threshold 0.5, 1,500 ms minimum silence,
and the benchmark's 16 kHz timestamp clock, then emits the original five-field
trace format. The evaluator invokes the pinned upstream
`analyze_VAD_interruption_new2` decision core and faithfully aggregates SRR,
SIR, EIR, NIR, SRIR, FSED, ERT, EIT, and IRD.

The released non-Moshi entry points unconditionally read a WER file they never
create and a separately precomputed Llama-3 CPPL file. Those and the separate
OpenAI subjective batch judge are explicitly marked unevaluated; no dummy WER,
CPPL, or judge result is inserted. At 77.2184 hours of source audio before the
fixed collection tails, this matrix is intentionally queued rather than
sharing GPU service with a frozen cell.

TOBench was also evaluated for fit. It is a 100-task omni-modal MCP benchmark,
not a realtime duplex voice-protocol benchmark, and its official harness is a
Python 3.12 plus mixed MCP/Node environment. It remains an external general
agent benchmark rather than a dependency or result of this no-Python runner.
