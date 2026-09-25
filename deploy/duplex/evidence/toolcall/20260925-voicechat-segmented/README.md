# VoiceChat tool correctness with output segmentation: 0/10, refused by design

`openrealtime bench toolcall` against `native-voicechat-output-segmented`
(800 ms decoded silence closes a turn), binary `openrealtime-b7b68848` at
`e7e659b6`, 2026-09-25 18:32 to 18:35 UTC. Segmentation fixed the timeouts
of `../20260924-voicechat/`: all ten tasks completed. None passed, and the
client received no tool call.

`sidecar-tool-events.log` shows why. VoiceChat's function channel proposed
calls in 2 of the 10 sessions, both with correct arguments:
`get_weather({"city": "Paris"})` (twice) and `get_weather({"city": "London"})`
(three times). The sidecar binding refused each one with "the fast provider has
execution authority only for declared, bounded computer actions"
(`binding/sidecarbinding/mirror.go`, `modelToolCall`). That is the documented
boundary (`docs/sidecar-protocol-3.md`, "Fast-action authority";
`docs/safety.md`). A native model's own call is only a proposal. Only
declared `computer.*` actions are executable, and only with
`-fast-computer-use`. Client function tools are reachable only through the
background reasoner. The background reasoner produced no call either: no user
transcript appears in the timeline for it to act on.

In the other 8 tasks VoiceChat proposed nothing: Tokyo weather, both
additions, both timers, currency, and both corrections. Even with routing,
this run bounds the model at 2/10.

Letting a native model's function-channel proposal reach the client as a
Realtime `function_call` would change that authority boundary. It is left as
a decision for the project owner, not changed here.
