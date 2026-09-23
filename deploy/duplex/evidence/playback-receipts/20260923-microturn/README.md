# Playback receipts on the micro-turn cascade

Twenty FDB `user_interruption` recordings on `microturn-voxtral-qwen3-kyutai`,
gated, with the benchmark player on (`-player`, `1bab9039`). The player plays
received audio on the playout clock. When the server cancels the response it
is playing, it stops, discards the rest, and sends
`conversation.item.truncate` with how much it played. Per-session transcripts
are included.

## First run: the server never said "cancelled"

`player-fdb-before-fix.json`, binary `openrealtime-3001ca61`. Every
`response.done` read `completed`, including turns the agent stopped because
the user interrupted, so the player never had a signal to follow: 0 stops,
0 truncations. The cause was that the sidecar protocol's `turn_done` carried no
reason. Fixed in `d0225ace`: `turn_done` may carry `turn_status:
"interrupted"`, the binding then ends the utterance incomplete, and the
gateway reports `cancelled` / `turn_detected`. The micro-turn sidecar marks
its stop path. This run had only 7 applicable recordings (7/7 passed). The
reason for that low applicability is not established.

## After the fix

`player-fdb-fixed.json`, binary `openrealtime-d0225ace`, run 21:37–21:45 UTC.

| | Value |
| --- | ---: |
| Recordings / task errors | 20 / 0 |
| Applicable passed | 19/19 |
| Cancelled responses reported by the server | 44 |
| Player stops with audio still queued (truncations sent) | 35 |
| Truncations confirmed (`conversation.item.truncated`) / refused | 35 / 0 |
| Audible yield (`audible_yield_latency_ms`) p50 / max | 44 / 115 ms |
| Yield by arrival (`yield_latency_ms`) p50 | 16 ms |

The client now reports where playback actually stopped, and the server
accepts it, so the server's record of what the user heard matches the
listener. The micro-turn sidecar paces its own playout and discards its queue
when it stops, so little audio was still queued at the client. Audible yield
stays within about 0.1 s of arrival. This is a simulated device on the playout
clock, not a physical speaker.
