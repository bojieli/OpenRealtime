# M2 microturn engine and cadence ablation

M2 adds two deterministic schedulers and revision-aware planning state:

- Fixed policies open microturns at 50, 100, 200, 400, or 800 ms. A final
  endpoint opportunity prevents a cadence boundary from stranding current
  evidence.
- The event-driven policy opens a microturn whenever a perception revision
  becomes available, plus an endpoint opportunity.
- The revision ledger requires increasing identities and source times, protects
  declared stable prefixes from later rewrites, and rejects updates after a
  final revision.
- The candidate ledger permits only explicit `prepared`, `superseded`, and
  `cancelled` transitions. Candidates name their source revision; slow results
  from an older revision are rejected rather than mutating current state.

The M2 runner pairs every policy and B0 with the same fixture, adapters, seed,
and sampled stage durations. It performs text-level preparation only. Speech
work begins at or after the endpoint, so M3 can test speculative synthesis and
the audio commit horizon separately.

## Attribution equation

```text
B0 observed latency
  = finalization + planning queue + cognition + downstream speech/playback

M2 observed latency
  = residual planning after endpoint + downstream speech/playback

planning overlap
  = (finalization + planning queue + cognition) - residual planning
```

For every reference trial, `M2 observed - B0 observed = -planning overlap`
with 0 ns reconciliation error. A negative observed-minus-baseline value is an
improvement.

## Reference result

The pinned 30-trial deterministic simulation produced:

| Policy | Observed P50 | Observed P95 | Paired delta P50 vs B0 | Candidate ready before endpoint |
| --- | ---: | ---: | ---: | ---: |
| Fixed 50 ms | 90.436 ms | 111.132 ms | -110.501 ms | 0 / 30 |
| Fixed 100 ms | 140.436 ms | 161.132 ms | -60.501 ms | 0 / 30 |
| Fixed 200 ms | 140.436 ms | 161.132 ms | -60.501 ms | 0 / 30 |
| Fixed 400 ms | 140.436 ms | 161.132 ms | -60.501 ms | 0 / 30 |
| Fixed 800 ms | 140.436 ms | 161.132 ms | -60.501 ms | 0 / 30 |
| Revision event | 71.583 ms | 91.853 ms | -128.333 ms | 6 / 30 |

The coarser fixed policies tie on this one-second fixture because their forced
endpoint opportunity sees the same last stable revision. That is a fixture
effect, not evidence that those cadences are generally equivalent.

These measurements use symbolic perception, fixed cognition, deterministic
signal speech, and simulated delays. They demonstrate state semantics and
latency attribution only. The output marker is not semantic speech, and the
numbers do not support provider, quality, or human-responsiveness claims.

## Reproduction

Run `./scripts/reproduce_m2.sh`. The gate first rechecks the M1 protocol traces,
then regenerates the complete M2 action report and self-contained ablation
visualization byte-for-byte. Unit and race tests include a deliberately slow
planning condition that proves stale revision results are cancelled.
