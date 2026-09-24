# Freeze-Omni endurance reproduction with KV diagnostics

Revision with `b934979b` diagnostics; the same 600 s input as the earlier
`freeze-longsession.json` (`build/long-input-600s.wav`, hashes in
`provenance.json`), wall-clock paced over the sidecar protocol, 605 s total.
Run 2026-09-23 09:01–09:11 UTC on the shared GPU; card use peaked at
80.8 GB, below the ~95 GB contention of the earlier OOM run. `run.sh` is exact.

The first attempt did not start because of a launcher bug. `freeze-omni.sh`
read the lease path as its memory requirement and looped forever. Commit
`2c56bb40` fixed it, and this run used the fixed launcher.

## Outcome

All input was sent. 68 VAD starts produced 68 model answers (67
`turn_done`), with 46 answer interrupts and no protocol errors. There was no
OOM. That does not make this an endurance pass: memory grows monotonically.

| At | KV sequence | live KV | snapshot KV | allocated | reserved | process VRAM |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| first answer | 116 tokens | 10 MB | 10 MB | 16.21 GiB | 16.31 GiB | 17,312 MiB |
| last answer | 10,062 tokens | 865 MB | 865 MB | 18.58 GiB (max 20.14) | 23.83 GiB | 25,118 MiB |

Mechanism, from `cache_memory` events (156 samples):

- The conversation KV cache is never truncated. Every heard and spoken token
  stays in the context, so the sequence grows about linearly with session
  time: 10,062 tokens after 10 minutes.
- The generation snapshot is a `copy.deepcopy` that shares no storage with the
  live cache (`shared_storage_count` 0 in every sample), which doubles KV memory.
- KV accounts for only 1.7 GB of the ~7.8 GiB process growth. Reserved
  memory grows faster than allocated memory (+7.5 vs +2.4 GiB at the end,
  +3.9 GiB at the allocated peak). This is consistent with transient buffers
  that scale with sequence length and with allocator caching of ever-larger
  blocks. It is an inference; the allocator is not broken down by category here.

If growth stays linear, the context would reach Qwen2-7B's 32k positions in
roughly half an hour. That extrapolation is not measured. Any fix bounds the
model's context and so changes model behavior: a history window, snapshot
sharing or copy-on-write, or `empty_cache` between turns. Each needs its own
matched quality run, so none was applied.

## Upstream behaviour (checked 2026-09-24)

The growth reproduces upstream Freeze-Omni `163a248` exactly; it is not a
sidecar defect. Upstream `bin/server.py` also keeps the whole conversation in
the KV cache and deep-copies the full state into `generate_outputs` after
every step. It limits each *answer* to 500 tokens, as the sidecar does, but
puts no limit on the history. What bounds it upstream is the demo lifecycle:
`recording-started` / `recording-stopped` call `reset()`, which restores the
system prompt and drops all history, and an idle session disconnects after
600 s. A client that keeps one session open, as this probe does, grows
without bound upstream too.
