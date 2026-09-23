# Lychee stall reproduction, 2026-09-23

Same fixtures as the 26 stalls audited in `../20260923-stall-audit/`: FDB v1.5
`user_interruption` 1–8 through `openrealtime serve`, and the 600 s long-session
input. The run also compares finite and continuous input on recordings 1 and 4
(`native_probe.py`: continuous paced silence vs `--stop-after 1.0`). The sidecar
logs every received PCM event (`audio_delivery`); upstream stage timing is kept
per session. `run.sh` is exact; `analyze_stalls.py` produces
`stall-correlation.json` (window: 8 s before each stall).

17 of the 60 turns ended `audio_stalled`, and every stall happened in model
state S (speaking). All 883 received PCM events were forwarded, none muted or
empty, so the sidecar did not discard delivered audio. All 8 FDB recordings failed
with the stall error (`fdb.json`).

| Group | Stalls | Upstream rounds in prior 8 s | Speaking tokens / PCM produced |
| --- | ---: | --- | --- |
| Finite input: FDB replay (stops after the recording), `--stop-after` probes | 10/10 | none; no input consumed | none |
| Continuous input, ui4 and long session up to ~360 s | 4 | 12–15 rounds, 4.8–6.0 s of input | 0–2 speaking tokens, no PCM |
| Continuous input, long session after ~540 s | 3 | 4 rounds, 1.6 s of input | speaking tokens and T2W submits in all 4, no PCM out |

What this establishes:

1. **Input-clocked generation halts when input stops.** Every finite-input stall
   had zero rounds. The model was left in state S with its answer
   unfinished. The FDB harness stops sending audio after the recording; a live
   microphone does not. Lychee FDB scores taken this way measure the harness
   stopping, not the model. The fix belongs where input ends: keep
   wall-clock silence flowing, in the harness or the sidecar. Neither was changed here.
2. **Lychee runs slower than real time on this shared GPU.** In the long session,
   a 0.4 s input window took a median 0.48 s in the first minute (RTF 1.21),
   rising to 1.44 by 9 minutes and 2.7–3.3 at the end (`long-session-rtf.json`).
   Only ~341 s of the 605 s input was processed, so the model fell minutes
   behind the live audio. The configuration uses `ENFORCE_EAGER=1` and
   XFORMERS attention, and the card was shared; neither was varied here.
3. **Continuous-input stalls with running rounds and no speaking tokens.**
   These look like a missing yield rather than lost audio: the model stops
   producing speech without a state change back to L, so the turn stays open.
   The backend runs with `LYCHEEFD_VLLM_KEEP_ALIVE_SPEAKING=1`. That is a
   plausible cause, not a verified one.
4. The late long-session stalls had speaking tokens and Token2Wav submissions
   but no PCM output within 8 s. Rounds were taking ~1.5 s each at the time,
   so this matches the throughput collapse above. Whether upstream
   content-hash suppression dropped anything cannot be seen from these logs,
   because suppression happens before SSE emission.

The audit's earlier hypothesis for the zero-round stalls is confirmed. Its
"continuing inference with PCM production" group did not recur in this run
(0 rounds with `first_pcm_out_sec` in any stall window). No repair was applied.
