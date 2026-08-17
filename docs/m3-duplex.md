# M3 incremental speech commitment and duplex behavior

M3 defines three audio horizons:

1. **Prepared** PCM exists but has not entered playback and can be discarded.
2. **Queued** PCM is inside the bounded playback horizon but has not been
   observed as played; it can be cleared or truncated.
3. **Played** PCM is irreversible interaction history. Cancellation never
   reduces this counter or removes its candidate/text association.

The reference speech adapter streams five contiguous 20 ms PCM16 chunks. The
commit controller caps prepared lookahead at 100 ms and queued lookahead at
40 ms, validates chunk identity/format/order, and is safe for concurrent
producer and playback callbacks.

Cancellation has two meanings. `yield` stops for a directed user interruption
without declaring the heard content wrong. `invalidate` means new evidence
made the candidate wrong; if any samples were played, the plan cannot close
until an explicit repair is recorded. Both modes report queued and prepared
audio discarded beyond the played horizon.

## OpenAI wire mapping

Representative cancellation traces use the current OpenAI Realtime events in
this order:

```text
response.cancel
output_audio_buffer.clear
conversation.item.truncate(audio_end_ms = observed playback)
output_audio_buffer.cleared
conversation.item.truncated
response.done(status = cancelled)
```

The output buffer events are WebRTC/SIP-specific, while truncation synchronizes
conversation history with audio actually heard. Every generated event is
validated against its exact profile/direction schema. Directed interruption,
listener-backchannel, and side-speech traces also contain
`input_audio_buffer.append` while output audio is active, demonstrating that
input processing does not pause during system speech.

## Reference scenarios

Thirty deterministic trials per scenario produced:

| Scenario | Result |
| --- | --- |
| Directed interruption | Stop P50 17 ms, P95 26 ms, range 12–28 ms; 0 failure-to-stop |
| Listener backchannel | 0 false stops; output completes |
| Side speech | 0 false stops; output completes |
| Candidate invalidation | 30/30 explicit repairs after 40 ms of played audio |

All 120 trials preserve played history and pass OpenAI schema plus causal-trace
validation. The stop delay is injected, the overlap labels are known inputs,
and the audio is a nonsemantic signal. These results validate orchestration
semantics, not interruption-classifier accuracy, speech naturalness, or live
device latency.

The public Full-Duplex-Bench taxonomy informed the scenario names, but no
third-party corpus is redistributed because a repository-compatible data
license has not been established. A future external-data adapter must keep its
data outside this repository unless provenance review permits redistribution.

Run `./scripts/reproduce_m3.sh` to regression-check M0–M2, regenerate all 120
M3 traces, compare the report and four representative traces/timelines
byte-for-byte, and validate every trace.
