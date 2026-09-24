# PersonaPlex gated rerun (in progress)

Campaign `20260923T215940Z-rnwu_yu5`: the same scope and profile as the ungated
`20260923T012919Z-td0w3l3o` campaign, now gated on `session.updated` by the
runner default (`wait_configured: true`). Binary `openrealtime-d0225ace`,
sidecar on the repaired kyutai venv. Started 2026-09-23 at 21:59 UTC.

| Category | Gated applicable passed (N/A) | Ungated, same morning (N/A) |
| --- | --- | --- |
| user_interruption | 182/186 (14), yield p50 442 ms | 174/180 (20), 435 ms |
| user_backchannel | 93/93 (5) | 88/88 (10) |

Neither category has task errors. Gating adds a few applicable recordings
here. The large effect in `../20260923-gating-ab/` came from a run where
configuration took about 4 s; the morning campaign configured faster. The
remaining categories and FD-Bench will be added when they finish.
