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

## Continuing-inference follow-up

`round-activity.json` counts non-null timing fields among rounds completed
in each stall's preceding eight seconds, parsed from the retained compressed
logs. All 19 continuing-inference stalls have at least one round reporting
`first_pcm_out_sec`. The short session has 12 such rounds; each of the 18
long-session stalls has one. These are upstream production observations,
not evidence of PCM delivered to the adapter or rendered by a client.

Upstream `app.py` marks `pcm_out` when consuming a Token2Wav message, while
`_emit_pcm_event` separately suppresses content hashes already seen. Therefore
production timing alone does not prove delivery. The next reproduction must
correlate production, hash suppression, SSE emission/receipt, and adapter
mute/turn state. No root cause or repair is established by this audit.
