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

This is the primary planned joint intelligence/interaction benchmark for
OpenRealtime, but it has not been run locally. The persistent OpenRealtime
`DiscreteTimeAdapter` bridge remains to be implemented, and the current
environment lacks the ElevenLabs credential and externally configured persona
voice IDs required by the upstream user simulator. A mock-tool, text-only, or
partial smoke result must not be reported as a τ-Voice score. See the
[integration and evaluation plan](../tau-voice/README.md).

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

## Full-Duplex-Bench v3 published tool-use reference

`full-duplex-bench-v3-published-results.json` transcribes Tables 2–6 and the
paper-reported pre-emptive tool-call rates from arXiv `2604.04847v1`. The source
PDF digest and benchmark repository revision are pinned. It contains published
results for `gpt-realtime-1.5`, Gemini Live 2.5 and 3.1, xAI Grok, Ultravox
v0.7, and a Whisper→GPT-4o→OpenAI TTS cascade.

The artifact is not a local run. It explicitly preserves that GPT-Realtime is
not GPT-4o Realtime, xAI Grok is not Groq, and the cascade's GPT-4o component is
not a GPT-4o Realtime result. Its argument and response metrics also depend on
GPT-4o judges.

## Tool-use benchmark scope

The v3 manifest pins the upstream 100-scenario disfluent tool-use definition
and its 12 mock APIs, but marks it `inventoried_not_run`. A valid FDB-v3 run
also needs the separate audio bundle, LiveKit-equivalent orchestration, actual
tool responses, ASR, and a declared argument/response judge. Reporting a small
JSON-only tool-call test as an FDB-v3 score would be misleading.

The v3 release card calls the audio “100 examples, 79 unique scenarios,” while
the pinned `benchmark_data_v2.json` contains 100 unique definition IDs. The
manifest records both observations and uses `definition_entries` rather than
silently treating examples, recordings, and scenario designs as identical.

## LiveKit eot-bench published component reference

`livekit-eot-bench-published-2026-08.json` pins LiveKit revision
`7f2acca997211908c6ee962ace8bcc8d6a66fbac` and transcribes its committed
English operating-point table. The benchmark evaluates causal end-of-turn
decisions over 400 turns, jointly sweeping score threshold, minimum action
delay, and timeout.

This is component evidence only. Its latency is endpointing dead air rather
than inference or speech-response latency. Its OpenAI row is
`gpt-realtime-2` semantic VAD, not GPT-4o, and the endpoint-event adapter
produces binary rather than calibrated probability scores. The upstream
harness uses Python, but this repository only stores a JSON transcription and
does not add or execute a Python dependency.

TOBench was also evaluated for fit. It is a 100-task omni-modal MCP benchmark,
not a realtime duplex voice-protocol benchmark, and its official harness is a
Python 3.12 plus mixed MCP/Node environment. It remains an external general
agent benchmark rather than a dependency or result of this no-Python runner.
