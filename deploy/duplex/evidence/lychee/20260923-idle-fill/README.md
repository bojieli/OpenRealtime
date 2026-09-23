# Lychee idle fill (c2bdb04f) on the rebuilt sm_120 vLLM

Same fixtures as `../20260923-stall-reproduction/`, without the 600 s leg.
Run 2026-09-23 11:18–11:27 UTC. The backend ran on the vLLM tree rebuilt by
`deploy/duplex/services/build-lychee-vllm.sh`, with xformers FA3 disabled
(`b8948731`).

**The fill works.** Both finite-input probes (`--stop-after 1.0`) completed
without an `audio_stalled` error. The sidecar filled 28.2 s and 28.1 s of
wall-clock silence after the client stopped sending. In the reproduction,
both finite probes stalled with zero upstream rounds. Continuous probes filled
nothing, as expected. Turn endings: 12 model interrupts, 1 stall (FDB recording
1); all 130 received PCM events were forwarded.

**Not a behavior measurement.** The host was saturated during the run: seven
OpenROAD routing jobs from another project, load 14–21 on 32 cores, swap
full, 2–19 GB RAM free. Lychee rounds ran at a median RTF of 2.1–5.3,
against 1.21 in the morning run. Answers were correct in text but reached
first audio only after ~25 s, in short fragments. So 7 of 8 FDB recordings
were not applicable: the model was not yet speaking at the interruption.
The FDB comparison needs a rerun on a quiet host. Whether the FA2/CUTLASS
attention path costs throughput by itself is not separated here. The
morning build also could not have used FA3 on this GPU, so it most likely
ran the same kernels.
