# VoiceChat selected campaign final results

Campaign `20260922T181139Z-64nefc4d`, pinned to `1a8c216f`, finished
2026-09-23 at 01:19 UTC. Runner exit status: **1**, caused by the retained
interruption task protocol error. The whitespace fixes postdate this run.

FD-Bench covers only `cosyvoice2-single-round-combine-med`: all 293
conversations completed, zero task errors, 84 conversations passed.
Totals: 1089/1382 answered, 293 missed,
66 premature and 852 overrun turns.
Median of conversation-level median response latencies: 1,149 ms;
this is not a pooled turn-level median. Median conversation overlap: 3,600 ms.

The other 20 FD-Bench conditions were not run in this campaign. Complete
execution of this condition does not establish behavioral acceptance.
The profile segments output after 800 ms of decoded quiet PCM, not native
EOS. These are benchmark-observed audio metrics, not device playback receipts.
All final runner result digests were checked against source artifacts before
retention; the sibling category directories retain the four FDB results.
