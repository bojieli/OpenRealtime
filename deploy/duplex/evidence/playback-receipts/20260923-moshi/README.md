# Playback receipts on native Moshi: the yield is not signalled

Same player validation as `../20260923-microturn/`: 20 FDB interruption
recordings, gated, `-player`, profile `native-moshi`, binary
`openrealtime-d0225ace`. The sidecar code includes `fe51439a` (interrupted
turns reported) and runs on the repaired kyutai venv (moshi 0.2.13,
`b1424dd3`). Run 2026-09-23 21:51–21:58 UTC.

No task errors. 11 recordings were applicable and **3 passed**. Audible yield
p50 was 1,481 ms (max 5,718), against 1,362 ms by arrival. **All 61 responses
ended `completed`, and the player never stopped.** Moshi's stats show 0
engine interrupts. Moshi owns the floor, stops talking on its own, and the
adapter closes the turn after its quiet hangover. Nothing tells the sidecar
that the stop was a yield, so `fe51439a`, which covers engine and model
interrupt paths, has nothing to mark. Classifying a quiet-down during user
speech as an interruption would be a heuristic, and none was added. For
natively yielding models, the no-flush playout numbers remain the audible
measure. Interruption handling is weak here, consistent with the earlier
smoke run (1/2).
