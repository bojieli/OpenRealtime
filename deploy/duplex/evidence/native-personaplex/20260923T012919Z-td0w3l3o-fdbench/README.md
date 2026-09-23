# PersonaPlex selected FD-Bench condition (complete)

Campaign 20260923T012919Z-td0w3l3o, pinned checkout 1d221b9b, binary
`openrealtime-68c15b3e`. Condition `cosyvoice2-single-round-combine-med`:
all 293 conversations completed at 08:49:41 UTC with **zero task errors**;
the benchmark summary reports `complete: true`. One condition of the 21 FD-Bench
conditions; not an all-condition result.

| Measure | Value |
| --- | ---: |
| Conversation passes | 77/293 (26.3%) |
| Turns answered | 1,358/1,382 |
| Missed turns | 24 |
| Premature turns | 249 (51 of them began after audible speech ended, inside annotated trailing silence) |
| Overrun turns | 955 |
| Per-conversation median response latency | p50 274.5 ms, p90 364.5 ms, max 529.9 ms |
| Overlap per conversation | p50 4,640 ms, p95 9,744 ms |

For comparison, VoiceChat on the same condition answered 1,089 turns and
missed 293, with 66 premature, 852 overrun, and 84/293 conversation passes.
PersonaPlex answers nearly every turn and answers quickly, but it starts
early and talks over the user more. Its pass rate is lower despite the
higher answer count. Timing is benchmark-received audio, not rendered
playback; output boundaries use adapter RMS/hangover, not native EOS.

## Completion record is reconstructed

The Python runner (PID 3810341) was killed between 08:05:48 and 08:05:58 UTC,
most likely together with the previous tool session. Its `finally` block never
ran, so no runner `finished.json` exists. The benchmark child (PID 2874038)
survived in its own session and completed. `finalize_orphan.py` attached at
08:08:44. It resumed resource sampling (`resources-resumed-2.jsonl`) and
wrote `finished-reconstructed.json` after the benchmark exited. That record
repeats the runner's result validation, which passed, and file digests. The benchmark exit code is not
observable, and executable/sidecar-inventory re-hashing after the run was not
performed. Resource samples are missing from 08:05:48 to 08:08:44. The server
(PID 3810353) was then stopped manually after an ownership check
(`server-stop.json`). See `runner-loss.json`.

## Input drops (old backend; new diagnostics not loaded)

`input-drop-summary.json` summarizes `personaplex-stats-campaign.jsonl`
(one record per session, all from backend PID 3808281). Every one of the 791
campaign sessions dropped input frames. FDB: 2,041 frames (2.2%), median 4 per
session, max 7. FD-Bench: 1,953 (1.3%), median 4, max 257. Every session had
at least one frame over budget. Outliers are sessions 524, 533 and 534
(05:03–05:12 UTC, up to 257 drops, one 25 s frame). They follow the second
disk-full event. From 08:11 to 08:42, frame p95 rose from about 34 ms to
57–81 ms and drops rose to a median of 15.5 per session. GPU memory was
unchanged and host load stayed near 14. The contending process was not
identified. The handoff agent ran CPU-only DeepFilterNet diagnostics from
08:09 to 08:15 only.
The cause of the steady ~4 drops/session is not established by these counters.
The lifecycle checkout's first-packet/queue-depth/first-drop diagnostics are
meant to answer it.
