# M0 reproducibility

M0 proves that a contributor can recreate a timing trace from a publicly
redistributable audio fixture before any engine optimization.

## Prerequisites

- A POSIX shell
- Go 1.25 or newer
- `curl` for the pinned specification provenance check

## One-command reproduction

From the repository root:

```bash
./scripts/reproduce_m0.sh
```

The script downloads Go modules, builds a trimmed production binary, verifies
the pinned official OpenAI schema extraction, regenerates the one-second WAV
into ignored `artifacts/`, replays it in 20 ms frames, validates every OpenAI
event plus whole-trace causality, compares all artifacts byte-for-byte with the
CC0 golden files, and runs race, test, vet, and formatting gates.

The canonical replay contains 50 OpenAI `input_audio_buffer.append` client
events followed by one `input_audio_buffer.commit` event. It starts at
monotonic time zero and commits at exactly 1,000,000,000 ns. The input is mono
PCM16 at the OpenAI-required 24 kHz rate, and each wire event carries standard
base64 audio.

## Manual commands

```bash
go build -trimpath -o artifacts/openrealtime ./cmd/openrealtime
artifacts/openrealtime fixture generate artifacts/m0-tone.wav
artifacts/openrealtime replay artifacts/m0-tone.wav \
  --events artifacts/m0-openai-events.jsonl \
  --trace artifacts/m0-trace.jsonl --session-id m0-replay --frame-ms 20
artifacts/openrealtime protocol validate \
  --profile realtime --direction client artifacts/m0-openai-events.jsonl
artifacts/openrealtime trace validate artifacts/m0-trace.jsonl
artifacts/openrealtime trace summarize artifacts/m0-trace.jsonl
```

The fixture is procedurally generated square-wave audio. It contains no speech,
personal data, imported recording, or synthesized voice. Its provenance and
license are recorded in [fixtures.md](fixtures.md).

## M1 endpointed reference condition

Run:

```bash
./scripts/reproduce_m1.sh
```

This command builds the production binary, rechecks the pinned official
protocol source, runs 30 seeded endpointed trials, validates every generated
trace, and compares the report, trial-0000 trace, and HTML timeline with the
checked reference artifacts. It then runs the same race, vet, and formatting
gates as M0. See [m1-baseline.md](m1-baseline.md) for the stage equation,
reported distribution, and limitations.

## M2 cadence ablation

Run:

```bash
./scripts/reproduce_m2.sh
```

The command regression-checks all M1 reference traces, then regenerates the six
paired M2 scheduling conditions, complete action ledger, and HTML ablation
view. It compares the report and visualization byte-for-byte and runs the full
race, test, vet, and formatting gates. See [m2-engine.md](m2-engine.md) for the
attribution equation and interpretation limits.

## M3 duplex and repair scenarios

Run:

```bash
./scripts/reproduce_m3.sh
```

The command first executes the complete M2 regression gate. It then regenerates
120 M3 scenario traces, validates every OpenAI event and causal envelope, and
compares the raw report plus four representative traces and timelines
byte-for-byte. See [m3-duplex.md](m3-duplex.md) for horizon semantics and result
limits.

## M4 fast/slow cognition

Run:

```bash
./scripts/reproduce_m4.sh
```

The command executes the complete M3 regression gate, including OpenAI protocol
validation and race tests, then regenerates the 270-trial fast/slow report and
HTML frontier byte-for-byte. See [m4-fast-slow.md](m4-fast-slow.md) for lifecycle
semantics and interpretation limits.

## M5 translation and rapid interaction

Run:

```bash
./scripts/reproduce_m5.sh
```

The command executes the complete M4 regression hierarchy, then regenerates 90
OpenAI Translation-profile traces and 60 GA Realtime game traces. It validates
all 150 causal traces, compares the report, dashboard, representative traces,
and timelines byte-for-byte, then runs race, vet, and formatting gates. No API
key, provider SDK, browser client, or proprietary fixture is required. See
[m5-demonstrations.md](m5-demonstrations.md) for metric semantics and limits.

## M6 comparative reference release

Run:

```bash
./scripts/reproduce_m6.sh
```

After the complete M5 hierarchy, this command rebuilds the machine-readable
comparative study, compares it byte-for-byte, rebuilds the 41-file release
manifest, verifies every listed size and SHA-256 digest, and repeats the full Go
quality gates. The native and human-study rows are deliberately `not_run`; see
the technical report and prospective human-study protocol in `docs/research/`.

## M7 stable v1.0.0 release

Run:

```bash
./scripts/reproduce_m7.sh
```

The final gate executes M0–M6, compares the combined protocol/provider
conformance result and direct-consumer example byte-for-byte, compiles the
maintained example, and repeats race, vet, and formatting checks. It validates
133 OpenAI event definitions and all five stable provider roles. No Python,
provider account, microphone, browser, or proprietary client is required.
