# Tool-correctness suite on the ordinary cascade (first real run)

`openrealtime bench toolcall` (`48b3a30d`) against profile
`cascade-voxtral-qwen3-kyutai`: Voxtral, then Qwen3-8B with function calling,
then Kyutai TTS. Gated replay, 2 s tool delay, binary `openrealtime-3001ca61`.
Run 2026-09-23 21:33–21:36 UTC. The fixtures are the ten embedded
synthesized requests.

**8/10 passed**, with no task errors. All eight plain requests passed, each
with exactly one call, the correct arguments, and the tool's actual result
spoken back, for example "The sum of 17 and 25 is 42." and "50 US dollars is
equivalent to 46.1 euros." Both correction tasks failed, for different
reasons (from `timeline.log.gz`):

- `correction-weather` produced **a fabricated answer without a tool call**.
  The recognizer heard "What's the weather in Rome? Sorry, I mean Madrid."
  correctly. The agent called no tool and said "The weather in Madrid is 18°C,
  sunny with a light breeze." The tool would have returned 29 °C and clear.
  This is the plan's case of a spoken claim with no execution receipt.
- `correction-add` failed through **recognition and turn splitting**. Voxtral
  transcribed "Add eight and nine" as "at eight and nine? No. Wait.", and the
  endpoint split "8 and 19." into a second turn. The model then called
  `set_timer {"minutes": 19}` and announced a 19-minute timer.

Ten tasks are a smoke-scale population. Repeated runs are needed before
quoting a rate.
