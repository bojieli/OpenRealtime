# Lychee stall audit, 2026-09-23

Retrospective analysis of the 26 `audio_stalled` events in the retained
2026-09-22 control log. These are observations from failed runs, not acceptance.

For each stall, `stalls.json` joins its session to the stage-timing path in
`session_start`, then counts round starts and completions in the preceding
8,000 ms. `sources.json` hashes the original and losslessly compressed logs.
All timestamps are server-host epoch milliseconds; no cross-host clock subtraction.

- 7 stalls had no round start or completion in the preceding eight seconds.
- 19 stalls had continuing inference rounds, across two sessions.

Upstream `lychee_fd/app.py:3239` waits when pending input is shorter than the
inference window. Thus a stopped input stream can halt generation even while
the state remains speaking. This is a plausible explanation for the first
group, not proof: these logs do not establish the upload queue state.
It cannot explain all 26 stalls, because the second group kept computing.

Next GPU reproduction must retain input upload timing, round state/control,
text/audio token counts, vocoder completion, and received PCM. Compare finite
input with a continuous wall-clock silence tail; preserve the original
stalled fixtures. Do not relabel silence timeouts as model completion or
extend timeouts to count the failures as passes.
