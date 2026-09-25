# Lychee idle-fill recheck (2026-09-24): FDB invalid, probes informative

This reruns the fixtures of `../20260923-idle-fill/` with the same sidecar
(`sidecar_sha256` in `provenance.json`) at revision `62114cba`. `run.sh` is exact.
Run 2026-09-24 18:50 to 18:57 UTC.

## FDB: invalid, a harness defect

All 8 `user_interruption` tasks failed with
`decode sidecar header: json: unknown field "turn…`. The script pinned
binary `openrealtime-68c15b3e` (2026-09-22). That binary predates the
sidecar's `turn_status` field, so its strict decoder rejected the first turn
boundary. `fdb exit 0` in `run.log` hides this, because the runner exits 0 when
tasks fail. This measures nothing about Lychee. The 2026-09-25 queue reruns it
with `openrealtime-b7b68848`.

## Probes: the fill works; the remaining endings are unsignalled answer ends

All four probes (FDB recordings 1 and 4, continuous and finite input) ended
with an `audio_stalled` error. They are not the input-starvation stalls that
the fill was built for:

- The fill ran: the finite probes filled 27.7 s of silence after input stopped.
- Rounds never stopped. Stage timing shows 112 to 113 rounds per probe through
  the end, with a median round cost of 0.77 to 1.05 times real time.
- Each stall followed a complete second answer. The texts end on a full
  sentence ("…What feels most important to you right now?"). Then no PCM
  arrived for 8 s while the model stayed in its speaking state, and the adapter's
  8 s timer ended the turn.

So Lychee finishes the answer but never signals the end of speaking. The
adapter currently reports that as an error. Treating 8 s of silence in
speaking state as a completion would be an adapter policy change, and it is
not applied here. In the 2026-09-23 fill run, rounds ran 2 to 5 times slower
than real time and first audio arrived only after about 25 s, so its probes
most likely ended before any answer finished. That would explain why this ending
did not show there; it is an inference.

The host was not quiet: load average was 48 to 50 on 32 cores during the probes.
