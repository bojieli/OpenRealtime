# Micro-turn cascade: complete interruption category (gated)

Campaign `20260923T133815Z-8dw16iqd` for profile `microturn-voxtral-qwen3-kyutai`: Voxtral Mini 4B
Realtime (480 ms delay), then a Qwen3-8B controller consulted every 500 ms,
then a streamed Qwen3-8B answer, then Kyutai TTS 1.6B. Clean tree at `accaa58c`,
binary `openrealtime-4536c718`, and replay gated on `session.updated`
(`wait_configured: true` in run.json). The category finished 2026-09-23 at
15:03 UTC. The rest of the campaign was still running when this was retained.

All 200 recordings completed with zero task errors. **188/188 applicable
passed**; 12 were not applicable. Yield latency by arrival: p50 40 ms,
p90 86 ms. On the playout clock, for a client that never flushes its buffer
(`playout_yield_latency_no_flush_ms`): p50 148 ms, p90 201 ms. About
100 ms of already-sent audio would still play after the stop. Timing is
received and simulated playout, not device playback. This is one category of
the campaign, not campaign acceptance.
