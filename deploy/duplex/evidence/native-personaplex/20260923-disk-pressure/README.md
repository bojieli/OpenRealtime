# PersonaPlex disk-pressure evidence

During campaign 20260923T012919Z-td0w3l3o, the shared filesystem again
reported zero available bytes around 04:55 UTC on 2026-09-23, during the
selected FD-Bench condition. The runner and backend remained live. The
resumed resource sampler was confirmed live after cleanup and had a complete
sample at 04:56:00.900069 UTC. No matching server errors were found, but this
does not prove that every write succeeded while the filesystem was full.
Do not claim uninterrupted logging or resource stability from process liveness.

Cleanup preserved checkpoints, installed environments, executables and results.
The records describe pip HTTP cache removal, Go compilation cache cleanup,
39 task-owned compiled object intermediates, and uv CI cache pruning without
--force. uv reported 49.2 GiB of cache entries removed; actual available space
rose to roughly 11 GiB because installed hardlinks retained many data blocks.
The earlier sampler failure is separately retained in 20260923-resource-gap.
