# Lychee FDB recheck, 2026-09-25: no measurement (host starved)

This reruns `../20260924-fill-recheck/` with a binary that knows the sidecar
`turn_status` field (`openrealtime-b7b68848`). `run.sh` is exact. Run
2026-09-25 18:36 to 18:45 UTC.

The decoder failure is gone, but this still measures nothing about Lychee.
Load average was 360 to 400 on 32 cores (another project's OpenROAD and
simulation jobs). All four probes produced **no audio at all**:
`output_seconds` 0 and no first audio in 45 s. On FDB, 7 of 8 recordings were
not applicable because the model was not speaking at the event. Recording 1
ended with `audio_stalled`. Lychee's input-clocked rounds cannot keep up on a
host this loaded. The FDB comparison still needs a quiet host.
