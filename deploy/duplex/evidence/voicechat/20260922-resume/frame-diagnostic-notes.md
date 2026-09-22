# Frame accounting probe

Runtime source: `9005d789033b8c3ec5876a7a68c4e2d9238f5c69` plus
`deploy/duplex/patches/voicechat-boundary-diagnostics.patch` with
`VOICECHAT_TRACE_BOUNDARIES=1`. Input: FDB user_interruption/1 context,
2 seconds leading silence, 18 seconds total, protocol v3 native probe.

EOS token 2 was observed at text frames 10 and 38, with audio at 9 and 37.
At text frames 100 and 200 the token was padding (12), audio was at 99 and
199, and the pending-EOS queue was empty. No later EOS was logged.
This probe does not support the hypothesis that a pending answer EOS was
waiting on a growing audio-frame backlog. It does not establish correct
model parity or justify treating every padding token as a turn boundary.
Reference-runtime speaking-state semantics remain to be checked.

## Upstream-prompt control

A separate 30-second probe used the upstream default system prompt, with the
same input and runtime. The answer changed to budgeting and cutting unnecessary
expenses, but its response still lacked `response.done` while audio packets
continued to the end of the probe. Prompt substitution alone did not resolve
the missing boundary. `reference-prompt.json` and
`reference-prompt-boundaries.json` retain this evidence.

The vendored TTS implementation treats EOS separately from padding and forces
silence feedback on EOS (`duplex_ear_tts.py`, `inference_force_speech_silence_on_eos`).
Therefore closing a response on every padding token would not reproduce the
reference semantics. The upstream recipe also warns that BF16 greedy thinker
decoding may diverge from FP32; a precision comparison remains outstanding.
