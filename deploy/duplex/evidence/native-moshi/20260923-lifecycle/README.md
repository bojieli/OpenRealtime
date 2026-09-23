# Moshi lifecycle, reconnect and readiness (new diagnostics)

Same design as `../../native-personaplex/20260923-lifecycle/`: lifecycle checkout
`f6f0a3a4`, campaign server binary, three reconnect probes, then 6 gated and
6 ungated alternating 20 s replays of the 2.02 s "capital of France" question.
Run 2026-09-23 10:10–10:15 UTC on a contended GPU.

- Reconnect: 3/3 passed (six fresh sessions, handshakes 0.01–0.21 s, no errors),
  and the sidecar stopped on SIGTERM.
- Configuration took 5–212 ms (Moshi has no voice or text prompt). No session
  in either mode dropped input, and the queue never exceeded 3 frames. The
  pre-configuration drop mechanism found for PersonaPlex does not apply to Moshi.
- Answer relevance is poor in both modes. Only 5 of 12 transcripts address
  the question: gated 1/6, ungated 4/6. The rest answer questions nobody asked
  ("capital of Japan", "subtracting 7 from 9"). This matches the premature,
  self-invented answers seen in the earlier smoke run. Keyword match only.
- During the reconnect sessions frame p95 was 123–204 ms, above the 80 ms
  frame budget, because of GPU contention. Replays measured 25–59 ms.
