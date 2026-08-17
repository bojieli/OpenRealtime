# External benchmark adapters

External corpora are not committed to this repository. A manifest pins each
upstream revision, download identity, byte length, digest, observed sample
count, and license statement. Downloading or using an archive remains an
explicit user action subject to its upstream terms.

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

## Tool-use benchmark scope

The v3 manifest pins the upstream 100-scenario disfluent tool-use definition
and its 12 mock APIs, but marks it `inventoried_not_run`. A valid FDB-v3 run
also needs the separate audio bundle, LiveKit-equivalent orchestration, actual
tool responses, ASR, and a declared argument/response judge. Reporting a small
JSON-only tool-call test as an FDB-v3 score would be misleading.

TOBench was also evaluated for fit. It is a 100-task omni-modal MCP benchmark,
not a realtime duplex voice-protocol benchmark, and its official harness is a
Python 3.12 plus mixed MCP/Node environment. It remains an external general
agent benchmark rather than a dependency or result of this no-Python runner.
