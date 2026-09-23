# Micro-turn cascade: remaining FDB categories (gated)

Campaign `20260923T133815Z-8dw16iqd`: the same run as `../20260923T133815Z-8dw16iqd-interruption/`, which holds the
interruption category (188/188 applicable passed). All FDB categories
completed with zero task errors. Every result reports `complete: true`.

| Category | Applicable passed | Not applicable |
| --- | ---: | ---: |
| user_backchannel (hold) | 86/94 | 4 |
| background_speech (hold) | 71/89 | 11 |
| talking_to_other (hold) | 78/93 | 7 |

The cascade yields reliably to real interruptions. It is weaker at holding
through speech that is not addressed to it: 18 background-speech and 15
talking-to-other recordings stopped when they should have continued.
Timing is received and simulated playout, not device playback. During the
FDB phase, the tool-call fixtures were synthesized once on the shared Kyutai
TTS service: 10 short requests, a few seconds of extra load. The FD-Bench
condition was still running when this was retained.
