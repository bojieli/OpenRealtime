# DuplexCascade: faithful vs bounded playout backlog

This was a user-approved experiment. The bounded cell is **not** the released
C1 protocol. Both cells use Kyutai STT, the released DuplexCascade checkpoint
and Kyutai TTS through the same sidecar at `b9302dd8`, on the same binary
`openrealtime-b7b68848`. Both ran the gated e2e smoke scope: 10 recordings per
FDB category and 12 FD-Bench conversations. The cells ran back to back on
2026-09-25. `run.sh` is exact.

- `native-duplexcascade`, the faithful reproduction: generation runs every
  500 ms tick, as upstream `server.py` does.
- `native-duplexcascade-bounded`: silent ticks are skipped while more than 2 s
  of synthesized audio is still unplayed. Ticks that carry new user words always run.

`ab-report.json` is the output of `tools/duplexmodels/duplexcascade_ab.py`.
The two trace archives hold each cell's 52 session traces.

| | Faithful | Bounded |
| --- | --- | --- |
| Interruption (applicable) | 1/4, 3 timeouts | 1/5, 3 timeouts |
| Backchannel (applicable) | 1/1, 8 timeouts | 1/1, 9 timeouts |
| Background speech | 4/4, 1 timeout | 8/8, 2 timeouts |
| Talking to other | 7/7 | 9/9 |
| FD-Bench conversations | 0/12 | 0/12 |
| FD-Bench turns answered | 16/55 (39 missed) | 18/55 (37 missed) |
| Ticks generated / skipped | 8,490 / 0 | 2,855 / 7,223 |
| Unplayed audio, median / max | 30.0 s / 30.0 s | 3.2 s / 30.0 s |
| Ticks with a full 30 s queue | 53% | 0.1% |
| Ticks over 500 ms | 23% | 56% |
| Load average, start to end | 77 to 338 | 342 to 431 |
| Speech generated per session, median | 295 words, 87 s | 286 words, 103 s |

## Reading

The bound does what it says: unplayed audio stays near 3 s instead of
filling the 30 s queue. **It does not fix C1.** The failures are unchanged.
About a third of FDB recordings hit the three-minute deadline in both cells,
and no FD-Bench conversation passes. Both cells generate the same amount of speech
per session: a median of about 290 words and up to about 600. The released
model's answers are long, and it keeps answering through silence. Pacing that
speech to playback cannot shorten it. The timeouts come from answer length,
not from the backlog.

**Confound:** another project's OpenROAD jobs loaded the host throughout.
Load was 77 to 431 on 32 cores, and higher for the bounded cell, whose ticks
missed the 500 ms deadline 56% of the time against 23%. That affects reaction
timing, such as yield latency and turn starts. It cannot explain the finding
above, because a slower clock does not make the model say less. The small
differences in applicable counts are within that noise and are not claimed.
