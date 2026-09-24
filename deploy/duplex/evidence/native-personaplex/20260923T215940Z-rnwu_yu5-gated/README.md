# PersonaPlex gated rerun (complete)

Campaign `20260923T215940Z-rnwu_yu5`: the same scope and profile as the ungated
`20260923T012919Z-td0w3l3o` campaign, now gated on `session.updated` by the
runner default (`wait_configured: true`). Binary `openrealtime-d0225ace`,
sidecar on the repaired kyutai venv. Started 2026-09-23 at 21:59 UTC.

| Category | Gated applicable passed (N/A) | Ungated, same morning (N/A) |
| --- | --- | --- |
| user_interruption | 182/186 (14), yield p50 442 ms | 174/180 (20), 435 ms |
| user_backchannel | 93/93 (5) | 88/88 (10) |
| background_speech | 66/67 (33) | 54/57 (43) |
| talking_to_other | 74/75 (25) | 69/70 (30) |

Neither category has task errors. Gating adds a few applicable recordings
here. The large effect in `../20260923-gating-ab/` came from a run where
configuration took about 4 s; the morning campaign configured faster. The campaign finished 2026-09-24 at 05:36 UTC. `finished.json` reports exit 0,
no errors, and exit 0 for all five commands. **No input frame was dropped in
any of the 791 sessions** (`personaplex-stats.jsonl.gz`; median configuration
2.1 s, max queue 12). The ungated run dropped frames in every session.

Selected FD-Bench condition (293 conversations, zero task errors):

| Measure | Gated | Ungated (same scope) |
| --- | ---: | ---: |
| Conversations passed | **129/293** | 77/293 |
| Turns answered | 1,358/1,382 | 1,358/1,382 |
| Missed | 24 | 24 |
| Premature (after audible speech ended) | **179 (32)** | 249 (51) |
| Overrun | 1,086 | 955 |
| Median response latency (p50 of per-conversation medians) | 299.7 ms | 274.5 ms |

Starting replay only after setup nearly doubles PersonaPlex's conversation
pass count. The gain comes mainly from fewer premature starts; overruns
rise. The two runs were on different days under different host load, so
this is a strong indication, not an isolated-factor proof. That proof is the
matched A/B in `../20260923-gating-ab/`.
