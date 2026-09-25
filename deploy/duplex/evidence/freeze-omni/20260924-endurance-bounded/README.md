# Freeze-Omni endurance with the allocator release and context cap

Sidecar at `f636ac17`: `torch.cuda.empty_cache()` after every answer, and a
fatal `context_limit` error past 28,000 context tokens. The model's context
itself is unchanged. The probe is the same as `../20260923-endurance/`: 600 s
input, paced over the sidecar protocol, 605 s total. `run.sh` is the exact
queue step. Run 2026-09-24 18:27 to 18:37 UTC.

**Not a matched comparison.** The 600 s input was rebuilt after the
workspace was deleted (`tools/duplexmodels/long_input.py`, FDB
`user_interruption` 1 to 41). It hashes differently from the earlier input and
produces a longer context: 11,539 against 10,062 tokens.

## Outcome

All 605 s of input was sent, with no probe errors. There were 82 VAD starts,
81 answers and 80 `turn_done`, plus 59 answer interrupts and 21 playback
interrupts. The session never reached the cap: no `context_limit` event.

| | 2026-09-23 (no release) | 2026-09-24 (release after answer) |
| --- | ---: | ---: |
| KV sequence at last answer | 10,062 | 11,539 |
| Live KV at last answer | 865 MB | 993 MB |
| Process VRAM at start | 17,312 MiB | 17,312 MiB |
| Process VRAM peak | 25,118 MiB | 26,508 MiB |
| Process VRAM at end | 25,118 MiB | 21,642 MiB |
| Reserved at last answer end | 23.83 GiB | 22.66 GiB |

Reserved memory at answer end grew 0.81 GiB/min and allocated memory 0.43 GiB/min,
by a linear fit over 81 answers.

## Reading

Releasing the allocator cache after each answer lowers the resting footprint.
The session ends 4.3 GiB above its start rather than 7.8 GiB. The peak is
unchanged: it scales with the context length, because the KV cache
and its deep-copied snapshot both grow with every token (`shared_storage_count`
stays 0). Growth is still linear in session time. The 28,000-token cap now
turns what would be an out-of-memory crash, at roughly 25 minutes
extrapolated, into an explicit session error. That cap is a bound, not a fix:
removing the growth needs a history window or snapshot sharing, and either
changes what the model sees.
