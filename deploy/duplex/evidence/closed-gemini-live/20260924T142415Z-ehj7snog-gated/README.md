# Gemini Live reference, gated full campaign (invalid for comparison)

Plan cell R0, closed reference. Profile `closed-gemini-live` at `b67930e1`:
`gemini-2.5-flash-native-audio-latest` as one integrated element, with a
`gemini-3.5-flash` background reasoner. Gated replay, all four FDB v1.5
categories and the complete `cosyvoice2-single-round-combine-med` FD-Bench
condition. Run 2026-09-24 14:24 to 2026-09-25 02:39 UTC. **Exit 1:** 67 FDB
task errors and 4 incomplete FD-Bench conversations.

| Category | Applicable passed | N/A | Task errors |
| --- | --- | --- | --- |
| user_interruption | 83/111 | 89 | 7 |
| user_backchannel | 54/54 | 44 | 41 |
| background_speech | 23/23 | 77 | 7 |
| talking_to_other | 20/21 | 79 | 12 |

FD-Bench: 4/293 conversations passed. 477/1,363 turns were answered, with
886 missed.

## Why these numbers do not measure Gemini Live

- **51 task errors were our configuration.** The background reasoner ran
  with the default `-slow-max-tokens 2048`. `gemini-3.5-flash` spends its
  thinking from that budget, and a MAX_TOKENS stop is reported as a provider
  error that ends the whole voice session. The profile now sets 8192.
- **18 task errors were provider quota:** the Live stream closed with
  "Resource has been exhausted (e.g. check quota)".
- **2 recordings timed out.**

The missed-turn count in FD-Bench is likely the same failure mode, but that
is not separated here. A rerun with the corrected profile replaces this as
the comparison point. `server.log` and `timeline.log` are not retained, for size.
