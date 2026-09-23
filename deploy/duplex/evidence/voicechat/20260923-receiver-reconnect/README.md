# VoiceChat receiver regression and reconnect validation

Measured 2026-09-23 on the shared GPU. Corrected Go binary SHA256 is
32db2bfc648c81d729e2712b6802bfa8dd5dea733c35014a0dfa629450966b58.
It includes receiver and binding whitespace fixes. External runtime and profile
are retained from the smoke run; the same live backend served the replay.

Three repeats of original FDB user_interruption/68 completed without protocol
errors. Yield latencies were 1017, 2305, and 944 ms; only one met the criterion.
The original offending token was not captured, so these repeats do not prove
the precise cause of the original failure. The subset dataset contains only
recording 68: its CLI reportable label is not full-category coverage.

The separate smoke ran two recordings per FDB category and two FD-Bench
conversations: seven of eight FDB behavioral passes; seven of eight turns
answered, one missed, no premature starts. No task errors. Runner status 1
correctly reflects incomplete dataset coverage, not a new protocol failure.

Reconnect probe revision 1d221b9b used negotiated 22050 Hz output. Each of two
fresh stdio sessions produced at least 100 ms consecutive active PCM above
-40 dBFS before abrupt transport shutdown; both child processes exited zero.
Handshake times were 1.55 and 1.22 s. This establishes release and readmission
for these two sessions, not speech intelligibility, continuity of conversation,
rendered playback cancellation, endurance, or multi-session concurrency.
The adapter still segments output using 800 ms decoded quiet, not native EOS.
