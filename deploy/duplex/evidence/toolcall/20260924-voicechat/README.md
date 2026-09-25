# VoiceChat tool correctness, default profile: 10/10 timeouts

`openrealtime bench toolcall` against `native-voicechat` (binary
`openrealtime-b7b68848`), 2026-09-24 18:40 to 18:50 UTC. All ten tasks
failed with "the conversation did not finish before the timeout" (60 s),
and none produced a tool call.

`timeline.log.gz` shows the cause. From session start, VoiceChat emitted
back-to-back speech segments of about 5 s each, and none closed a response. The
default profile has no output segmentation, so the benchmark never saw a
response finish, and no ASR final was recorded. This is a profile/boundary failure,
not a measurement of VoiceChat's tool decisions. The rerun uses
`native-voicechat-output-segmented.yaml` (800 ms decoded silence closes a turn).
