# Micro-turn cascade, sampled FD-Bench coverage (20 conditions)

Profile `microturn-voxtral-qwen3-kyutai`: Voxtral streaming ASR, Qwen3-8B
micro-turn controller, Kyutai TTS. Gated replay. It covers the 20 FD-Bench
conditions besides `cosyvoice2-single-round-combine-med`, which ran in full
earlier (242/293). The sample is 50 conversations per condition, drawn by
`bench fdbench -sample-per-condition 50 -sample-seed 20260924` (`52270ffc`),
1,000 in all. Binary `openrealtime-b7b68848`. `run.sh` is the exact queue
step. Finished 2026-09-25 12:03 UTC, exit 0, no task errors.

Overall: 408/1000 conversations passed, 3,562/4,700 turns answered, 1,138
missed, and 257 premature.

| Condition | Passed | Answered/turns | Missed | Premature | Overrun | Latency p50 ms |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| chattts easy | 29/50 | 220/236 | 16 | 13 | 117 | 892 |
| chattts med | 26/50 | 212/237 | 25 | 9 | 154 | 761 |
| chattts hard | 34/50 | 219/235 | 16 | 5 | 163 | 807 |
| cosyvoice2 easy | 42/50 | 232/234 | 2 | 9 | 115 | 979 |
| cosyvoice2 hard | 38/50 | 231/240 | 9 | 4 | 175 | 1,309 |
| cosyvoice2 easy, noisy bg 20 dB | 34/50 | 229/238 | 9 | 16 | 105 | 1,043 |
| cosyvoice2 easy, noisy bg 10 dB | 11/50 | 212/239 | 27 | 50 | 87 | 1,017 |
| cosyvoice2 easy, noisy bg 0 dB | 1/50 | 115/239 | 124 | 37 | 43 | 949 |
| cosyvoice2 easy, noisy gap 20 dB | 12/50 | 178/236 | 58 | 9 | 89 | 1,559 |
| cosyvoice2 easy, noisy gap 10 dB | 9/50 | 158/237 | 79 | 17 | 80 | 1,206 |
| cosyvoice2 easy, noisy gap 0 dB | 0/50 | 104/233 | 129 | 18 | 60 | 1,169 |
| f5tts easy | 41/50 | 223/233 | 10 | 1 | 124 | 1,453 |
| f5tts med | 47/50 | 235/236 | 1 | 2 | 151 | 965 |
| f5tts hard | 47/50 | 224/227 | 3 | 0 | 146 | 1,174 |
| f5tts easy, noisy bg 20 dB | 20/50 | 190/234 | 44 | 9 | 77 | 1,601 |
| f5tts easy, noisy bg 10 dB | 2/50 | 132/234 | 102 | 18 | 47 | 1,191 |
| f5tts easy, noisy bg 0 dB | 0/50 | 62/232 | 170 | 13 | 9 | 906 |
| f5tts easy, noisy gap 20 dB | 11/50 | 174/236 | 62 | 6 | 62 | 1,162 |
| f5tts easy, noisy gap 10 dB | 4/50 | 127/232 | 105 | 9 | 56 | 1,308 |
| f5tts easy, noisy gap 0 dB | 0/50 | 85/232 | 147 | 12 | 33 | 1,506 |

## Reading

The cascade holds up on clean speech from all three synthesizers: 26 to 47
of 50. Noise is what breaks it, and it breaks by silence rather than by
talking over the user. At 0 dB the cascade misses most turns (124 to 170
of about 235) and passes 0 or 1 conversation. Noise between utterances ("gap")
hurts as much as noise under them ("bg"), and more at 20 dB. This points at
recognition and endpointing under noise, not the controller. That is an
inference; this run does not separate the ASR's output from the controller's
decisions. Premature starts peak at 10 dB background (50), where noise is
loud enough to look like speech but not to hide it.
