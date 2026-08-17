# M5 translation and rapid interaction

M5 supplies two key-free demonstrations over the CC0 one-second signal
fixture. They exercise real OpenAI wire shapes but use authored symbolic labels;
they do not claim speech recognition, natural-language translation, or model
quality.

## Simultaneous translation

The translation adapter binds five symbolic source/target deltas to the five
200 ms engine frames required by the OpenAI Realtime Translation protocol.
Every trial contains the complete translation session lifecycle:

1. server `session.created`;
2. client `session.update` and server `session.updated`;
3. 24 kHz mono PCM16 `session.input_audio_buffer.append` frames;
4. optional source `session.input_transcript.delta` events;
5. append-only target `session.output_transcript.delta` and 200 ms
   `session.output_audio.delta` events;
6. client `session.close` and server `session.closed` after pending output is
   flushed.

The endpointed and stable-incremental policies emit the exact authored target.
The aggressive policy emits two deliberately wrong early deltas. Quality is
therefore exact authored segment accuracy, not BLEU, human preference, or a
language-model score. Lag is measured from each segment's engine-frame end to
its output delta.

Thirty trials per condition produce:

| Policy | Mean lag P50 | Completion lag P50 | Exact quality P50 | Failures | Compute P50 |
| --- | ---: | ---: | ---: | ---: | ---: |
| Endpointed | 586.616 ms | 186.616 ms | 100 | 0 | 90 |
| Stable incremental | 79.399 ms | 75.108 ms | 100 | 0 | 110 |
| Aggressive incremental | 29.549 ms | 26.331 ms | 60 | 60 | 50 |

The checked report at `benchmarks/m5/reference/report.json` is the exact source
of truth. The stable policy reduces symbolic mean lag without losing authored
exact-match quality, but costs more than endpointed processing. The aggressive
policy is faster and cheaper only by accepting deliberate target errors.

## Signal Match rapid audio game

Signal Match presents four audio cues and expects a corresponding symbolic
action before a 130 ms deadline. The endpointed condition and 50 ms microturn
condition use the same authored actions and seeded reaction model. Task quality
is the percentage of correct actions delivered before the deadline. Semantic
correctness and deadline success are both retained per round, so a late correct
action is never silently counted as success.

Every game trace uses GA Realtime response, audio-delta, output-buffer, and
concurrent client input events. The fixture contains signals rather than a
voice, so the demonstration measures scheduling and protocol behavior only.

Across 30 trials (120 rounds), endpointed scheduling has a 152.303 ms reaction
P50 and 102 deadline failures; the 50 ms microturn condition has a 63.651 ms
reaction P50 and zero deadline failures. Its compute cost is 80 symbolic units
per trial versus 48. These injected timing results demonstrate accounting and
the expected policy tradeoff, not deployed system speed.

Run `./scripts/demo_translation.sh` or `./scripts/demo_signal_match.sh` for
inspectable reports, traces, and timelines. Run `./scripts/reproduce_m5.sh` to
regression-check the full M0–M5 hierarchy and compare M5 artifacts byte for
byte.
