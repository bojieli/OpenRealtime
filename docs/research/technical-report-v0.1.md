# OpenRealtime reference study v0.1

Status: deterministic reference release, not a provider or human-subject study.

## Abstract

OpenRealtime tests whether revision-aware scheduling can improve interactive
audio timing without hiding correctness, interruption, repair, or compute
costs. This release implements a complete schema/codec boundary for the pinned
OpenAI Realtime, transcription, translation, and legacy beta event profiles,
then evaluates an endpointed reference, fixed/event-driven microturn policies,
duplex commitment, fast/slow cognition, simultaneous translation, and a rapid
audio game. Every result uses project-authored nonsemantic audio and injected
deterministic delays. No OpenAI-hosted model, native provider, speech corpus,
participant, or natural-language quality evaluator was used.

## Protocol and methods

The conformance layer contains 133 direction/profile definitions representing
67 unique wire event names from a cryptographically pinned official OpenAI
OpenAPI revision. Exact wire JSON is placed in a separate versioned causal trace
envelope; validation uses the event's profile and direction.

All conditions use seed 20260817 and publish raw per-trial records plus P50,
P90, P95, P99, minima, and maxima. M1/M2 trials are paired on fixture and delay
draw. Their primary effect is `observed microturn latency - endpointed latency`;
negative values favor microturn scheduling. The release adds a deterministic
10,000-resample percentile interval for the paired median. This interval only
describes the seeded fixture runs; it is not population inference.

Workloads are kept in explicit comparability groups. Fast/slow progress,
translation lag, duplex stop latency, and game reaction latency are not merged
into a leaderboard with response onset.

## Results

On the paired symbolic response-onset workload, endpointed B0 has a 201.526 ms
P50 and 222.335 ms P95. Fixed 50 ms scheduling has a 90.436 ms P50; the
revision-triggered policy has a 71.583 ms P50. Both beat endpointed latency in
all 30 seeded pairs. Fixed 50 ms has a paired median difference of -110.501 ms
with a seeded bootstrap interval of [-113.459, -106.907] ms; revision-triggered
scheduling has -128.333 ms with [-133.819, -125.673] ms. Coarser
100/200/400/800 ms cadence conditions tie one another at 140.436 ms P50 on this
cue layout, a counterexample to assuming that every cadence change produces a
distinct outcome.

Directed-interruption stop latency is 17 ms P50 and 26 ms P95 across 30 injected
M3 trials. No directed failure-to-stop, listener-backchannel false stop,
side-speech false stop, or played-history violation occurs. All 30 explicit
invalidation scenarios record repair. These labels are injected, so this does
not measure acoustic interruption classification.

On the symbolic difficult-question workload, fast/slow orchestration advances
truthful progress from 386.242 ms to 34.909 ms P50 while retaining a 386.242 ms
final-answer P50 and authored quality 94. It consumes 97 compute units versus
88 for the blocking path. Thus the reference supports an earlier-progress
claim but directly contradicts a stronger claim that fast/slow makes the final
answer faster.

Stable incremental translation reduces mean segment lag from 586.616 ms to
79.399 ms P50 at authored exact quality 100, while compute rises from 90 to 110
units. The aggressive condition reaches 29.549 ms but quality falls to 60 with
60 explicit segment failures. Signal Match microturn scheduling reduces
reaction P50 from 152.303 ms to 63.651 ms and deadline failures from 102 to zero
over 120 rounds, while compute rises from 48 to 80 units per trial.

## Claims and counterexamples

- The data support lower injected response-onset latency for the tested 50 ms
  and revision-triggered microturn policies relative to paired endpointed B0.
  They do not support a deployment-speed or native-model claim.
- The duplex state machine preserves played semantic history in the authored
  scenarios. It does not establish classifier accuracy on human speech.
- Fast/slow separation provides earlier truthful foreground state. It neither
  improves final latency here nor comes free.
- Stable translation improves the authored latency/quality tradeoff. Aggressive
  output shows why latency must not be reported without error counts.
- Signal Match demonstrates deadline accounting under the injected timing
  model. It is not evidence about human reaction or end-to-end hardware jitter.

## Missing evidence

Native realtime comparison is marked `not_run`: no provider credential or
redistributable provider output was placed in scope. The human study is also
`not_run`; only its prospective protocol and ethics requirements are published.
No ASR WER, BLEU, factual model evaluation, human naturalness, prosody, trust,
CPU/GPU utilization, network bandwidth, or monetary provider cost is claimed.

The audio fixture is a one-second synthetic square-wave signal. Quality and
compute scores are authored state-machine expectations. Schema-valid traces
show wire compatibility but do not reproduce all server-side timing, transport,
authentication, congestion, or model behavior of an OpenAI-hosted service.

## Reproduction

`./scripts/reproduce_m6.sh` rebuilds M0–M5, regenerates the comparative study
and release manifest, verifies every listed SHA-256 digest, and runs race, vet,
and formatting gates. `benchmarks/releases/v0.1.0/study.json` is the canonical
machine-readable analysis.
