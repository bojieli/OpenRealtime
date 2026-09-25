# MiniCPM-o FD-Bench, gated, complete selected condition

The FD-Bench half of the MiniCPM-o campaign in
`../20260924T080217Z-ujuru9z0-gated/`. That runner stopped before FD-Bench
because sidecar sources changed mid-run. This step ran the same condition
(`cosyvoice2-single-round-combine-med`, all 293 conversations) through the
same service and profile, gated (`-wait-configured`). It used binary
`openrealtime-b7b68848`, where the FDB categories used `d0225ace`. `run.sh`
is the exact queue step. Run 2026-09-24 13:47 to 18:27 UTC, exit 0,
reportable, and no task errors.

| Measure | Value |
| --- | --- |
| Conversations passed | 85/293 |
| Turns answered | 1,010/1,382 (372 missed) |
| Premature starts | 72 (7 after speech ended) |
| Overrun turns | 983 |
| Response latency | p50 1,148 ms, p90 1,568 ms (289 conversations) |
| Overlap per conversation | p50 11,080 ms, p90 16,360 ms |

MiniCPM-o answers most turns but rarely stops in time. It overruns 983 of
1,382 turns, with about 11 s of overlap per conversation at the median. That
matches its FDB interruption result, which yielded to 14 of 184.
