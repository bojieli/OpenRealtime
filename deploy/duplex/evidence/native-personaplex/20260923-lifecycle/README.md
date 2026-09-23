# PersonaPlex lifecycle, reconnect and readiness (new diagnostics)

Clean checkout `f6f0a3a4` (`/home/ubuntu/OpenRealtime-duplex-lifecycle-validation`),
same checkpoint, voice and upstream revision as the campaign. Server binary is
the campaign's `openrealtime-68c15b3e`, so only the sidecar and the client's
gating differ. `run.sh` is the exact script; `provenance.json` has hashes.
Run 2026-09-23 08:51–08:58 UTC on the shared GPU.

## Reconnect (real GPU)

Three `native_reconnect_probe.py` runs with the VoiceChat reconnect question,
each opening two fresh sessions and disconnecting abruptly: **3/3 passed**.
Every session handshook, produced at least 100 ms of consecutive audio at or above
-40 dBFS, and showed no errors. The shutdown path that now joins workers
before releasing shared model state was exercised six times, and the service kept
admitting sessions. The final SIGTERM stopped the sidecar within the 30 s wait.
This is energy-qualified received audio, not intelligibility or rendered playback.

## Readiness: waiting for `session.updated` removes the input drops

Twelve 20 s `realtime_probe.py` replays through `openrealtime serve`,
alternating gated (`--wait-configured`) and ungated, each with the same 2.02 s
question (Freeze-Omni `assets/question.wav`), 1 s lead and duration.

| | gated (6) | ungated (6) |
| --- | --- | --- |
| Sessions with dropped input | 0 | 6 |
| Dropped frames per session | 0 | 3, 3, 11, 16, 23, 23 |
| Max queued input frames | 1–3 | 26 (the cap) in every session |
| Client wait before replay | 2.1–4.7 s (reported, not subtracted) | 0.07 s |
| Transcript mentions Paris/France | 6/6 | 4/6 |

Sidecar configuration (voice/text prompt) took 2.1–4.7 s in both modes. The
drops rise with it: roughly one 80 ms frame for each 80 ms of configuration
beyond the ~2.08 s that the 26-frame queue can hold. An ungated client's
early audio queues during configuration, and the overflow is dropped. In
ungated-6, configuration (3.75 s) outlasted the whole lead plus question
(3.02 s); the model answered "Hey, let me know if you have any questions."
The transcript check is a keyword match, not a correctness score.

This establishes the startup-buffering mechanism that the handoff called
plausible but unproven. It very likely explains the steady ~4 drops per
session in the PersonaPlex campaign: benchmark WebSocket replay starts
without waiting for the acknowledgement, and campaign configuration times
were not recorded. A controlled rerun is needed to prove that link.
Reconnect sessions (raw sidecar protocol, input sent after the hello reply)
dropped nothing. Frame p95 rose from ~35 ms to ~80 ms from session 10
onward in both modes, so GPU contention from another process was present
for part of the comparison.

Each `graph mount closed` WARN in `server.log` coincides with the probe
closing its socket at the end of the replay while output continued. These
are client-close events, not model failures.

Not changed here: the benchmark WebSocket replay still starts ungated.
Gating it, or buffering pre-configuration input without dropping, would
change the measurement conditions of every earlier campaign. That is a
decision to make explicitly, with a matched rerun.
