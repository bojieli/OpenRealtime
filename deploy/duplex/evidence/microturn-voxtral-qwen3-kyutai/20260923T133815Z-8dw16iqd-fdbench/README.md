# Micro-turn cascade: selected FD-Bench condition and campaign completion

Campaign `20260923T133815Z-8dw16iqd`, profile `microturn-voxtral-qwen3-kyutai`, gated replay,
clean tree `accaa58c`, binary `openrealtime-4536c718`. Started 13:38 UTC and
finished 21:25 UTC on 2026-09-23. The runner's `finished.json` reports exit 0,
no errors, and exit 0 for all five commands. The FDB categories are retained in
`../20260923T133815Z-8dw16iqd-interruption/` and `../20260923T133815Z-8dw16iqd-fdb/`.

Condition `cosyvoice2-single-round-combine-med`: 293/293 conversations, zero
task errors, `complete: true`.

| Measure | Micro-turn cascade | PersonaPlex (ungated) | VoiceChat |
| --- | ---: | ---: | ---: |
| Conversation passes | **242/293** | 77/293 | 84/293 |
| Turns answered | 1,356/1,382 | 1,358/1,382 | 1,089/1,382 |
| Missed | 26 | 24 | 293 |
| Premature | **37** | 249 | 66 |
| Overrun | 927 | 955 | 852 |
| Median response latency (p50 of per-conversation medians) | 854 ms | 274.5 ms | — |

This is a system comparison, not an isolated factor. The cascade uses
different components, the native runs were ungated, and each system ran on a
different day. The cascade waits for evidence the user has finished, so it
starts early far less often, at the cost of about 0.6 s more response
latency than PersonaPlex. Overruns stay high for every system. Timing is
received audio, not device playback.
