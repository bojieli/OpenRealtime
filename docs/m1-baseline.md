# M1 endpointed reference baseline

M1 implements the `B0_endpointed_reference` control condition. It is a
deterministic instrumentation baseline: response creation cannot begin until
the one-second input fixture has committed, every wire message passes the
pinned OpenAI Realtime schema, and every stage is represented in one causal
trace.

The key-free reference adapters deliberately isolate orchestration:

- `reference.manifest_perception` consumes ordered 24 kHz PCM16 frames and
  emits revision cues from a fixture-bound manifest.
- `reference.fixed_cognition` accepts only a final perception revision and
  emits a fixed semantic candidate.
- `reference.signal_speech` emits a deterministic 100 ms PCM signal associated
  with that candidate.

The fixture has no human speech and the speech output is not a voice. Therefore
this checkpoint is not ASR accuracy, speech quality, conversational quality, or
real provider performance evidence.

## Timing model

Each of 30 trials uses the recorded base seed plus its zero-based trial index.
The seeded simulator samples bounded durations for perception finalization,
perception queueing, cognition, cognition queueing, first speech chunk, and
playback queueing. It records both every raw trial and P50/P90/P95/P99
distributions.

The required reconciliation is exact:

```text
observed endpoint-to-output-playback marker
  = perception finalization
  + perception queue
  + cognition
  + cognition queue
  + speech first chunk
  + playback queue
```

The playback marker is tied to the response candidate but carries a signal, not
spoken semantic content. A later real-speech condition must measure true first
semantic audio separately.

For the pinned seed, the 30-trial simulated observed latency has P50
201,525,834 ns, P95 222,334,515 ns, and range 170,745,602–227,753,374 ns. The
maximum reconciliation error is 0 ns. These values verify timing accounting;
they are not latency claims about a deployed system.

## Evidence and reproduction

Run:

```bash
./scripts/reproduce_m1.sh
```

The gate regenerates 30 trials, validates all 30 traces against the OpenAI wire
schemas and trace invariants, compares the report, first trace, and self-
contained timeline byte-for-byte with the checked reference artifacts, then
runs race, test, vet, and formatting checks.

The CLI requires a new or empty output directory, preventing traces from an
older run from being mistaken for current evidence.

The reference evidence is in `benchmarks/m1/reference/`. All 30 unaggregated
trials remain in `report.json`; trial 0000 is retained as the representative
full event trace and visualization. The reproduction command regenerates every
full trace rather than committing 30 redundant copies.
