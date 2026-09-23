# PersonaPlex: gated vs ungated benchmark replay (matched A/B)

Lifecycle sidecar `f6f0a3a4` (per-session drop diagnostics), server and bench
binary `openrealtime-9f6dd353`, which adds opt-in `-wait-configured`. The only
difference between arms is that flag. The fixtures are the same: FDB
`user_interruption` 1–20 and the first 10 conversations of
`cosyvoice2-single-round-combine-med`. The order was ABBA: FDB ungated, FDB
gated, FD-Bench gated, FD-Bench ungated. Input frame counts match pairwise, so
the stats attribution is exact. Run 2026-09-23 09:35–10:10 UTC. The GPU was
contended: frame p95 was ~79 ms in all four arms, and median configuration
took ~4.0 s in all four arms.

| | FDB ungated | FDB gated | FD-Bench gated | FD-Bench ungated |
| --- | --- | --- | --- | --- |
| Sessions with dropped input | 20/20 (11–47 frames) | 0/20 | 0/10 | 10/10 |
| Max input queue | 26 (cap) | 4 | 4 | 26 (cap) |
| FDB applicable / passed / N/A | 7 / 5 / 13 | 19 / 19 / 1 | | |
| FDB yield latency p50 (max) | 351 ms (855) | 532 ms (863) | | |
| FD-Bench answered / missed of 46 | | | 46 / 0 | 40 / 6 |
| Premature / overrun turns | | | 3 / 42 | 7 / 30 |
| Conversations passed | | | 7/10 | 3/10 |

Ungated replay loses the opening of every recording while the sidecar
configures. In FDB the model then usually is not answering when the
interruption arrives, so 13 of 20 recordings become not applicable; gated,
19 of 20 are applicable and all pass. In FD-Bench, ungated replay misses 6 of
46 turns and starts more of them early; gated replay answers every turn and
also overruns more of them. Small samples (20 + 10 per arm). Yield latencies
compare different applicable sets.

Consequence for earlier native campaigns: every native FDB/FD-Bench result so
far ran ungated, so its applicability and answer counts depend on sidecar
configuration time under the host's load at the time. The PersonaPlex campaign
averaged about 4 drops per session, far fewer than the 11–47 here, but its
configuration time was not recorded. Its scores are valid for its own
conditions, not as a model property. Gated replay excludes setup time from
the episode clock. That time is reported per transcript, not subtracted.
