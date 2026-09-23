# PersonaPlex complete background-speech category

Campaign 20260923T012919Z-td0w3l3o, pinned checkout 1d221b9b.
Finished 2026-09-23 at 04:09:11 UTC. All 100 recordings completed with zero
task errors: 54/57 applicable recordings passed (94.7%), with 43 not applicable.
Behavioral failures: background_speech/29, background_speech/35,
background_speech/48. The 43 not-applicable recordings do not establish hold
behavior because the assistant was not speaking at the event.

This is the full category population. It measures benchmark-received audio,
not rendered playback. Output boundaries use adapter RMS/hangover, not native
EOS. Input drops and possible startup buffering remain under investigation;
the later lifecycle fixes and packet-arrival diagnostics are not loaded here.
Generic cell levels in the result JSON do not describe the native architecture;
profile.yaml and external-runtime.json identify the actual configuration.

Talking-to-other and the selected 293-conversation FD-Bench condition remain
pending at retention time. This is not full campaign acceptance. The earlier
resource-sampling ENOSPC gap is recorded in the sibling
20260923-resource-gap directory; no stability claim spans that gap.
