# OpenAI Realtime reference, gated full campaign

Plan cell R0, closed reference. Profile `closed-openai-live` at `b67930e1`:
OpenAI Realtime as one integrated element, `gpt-5.4-mini` background
reasoner. Gated replay (`wait_configured: true`), all four FDB v1.5
categories and the complete `cosyvoice2-single-round-combine-med`
FD-Bench condition. Run 2026-09-24 14:24 to 22:14 UTC, exit 0, no task errors.
Every result digest in `finished.json` was verified before retaining.

| Category | Applicable passed | N/A | Task errors | |
| --- | --- | --- | --- | --- |
| user_interruption | 12/134 | 66 | 0 | yield p50 216 ms (passes only) |
| user_backchannel | 42/42 | 56 | 0 | |
| background_speech | 43/45 | 55 | 0 | |
| talking_to_other | 58/58 | 42 | 0 | |

FD-Bench: 20/293 conversations passed. 1,155/1,382 turns were answered, with
227 missed, 331 premature (9 after speech ended), and 886 overrun.

Holding is near perfect where it applies. Interruption is the weak case:
12 of 134 applicable recordings yielded within the benchmark window.
The high N/A counts mean the assistant was not speaking at the scripted
event, so those recordings measure nothing. `server.log` and `timeline.log`
are not retained, for size.
