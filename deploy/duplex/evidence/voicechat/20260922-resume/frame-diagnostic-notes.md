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
