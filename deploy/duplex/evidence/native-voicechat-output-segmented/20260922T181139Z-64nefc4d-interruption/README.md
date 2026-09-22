# VoiceChat interruption category: incomplete campaign evidence

This is the first completed category command from the still-running selected
campaign `20260922T181139Z-64nefc4d`, pinned at `1a8c216f`.
It is not evidence that the full campaign completed or passed.

Of 200 recordings, 199 completed: 138 were applicable, 73 passed, and 61
were not applicable. One recording (`user_interruption/68`) failed with
`text_delta requires text`, so the category summary declares `complete: false`.
Applicable yield latency p50 was 960.938 ms and p90 was 1704.0666 ms.

The receiver rejects whitespace-only text deltas in this revision. A regression
test reproduces the same error for a standalone space token; the raw offending
upstream token was not retained, so that token identity is not independently
verified. Commit `5c9a798c` accepts nonempty whitespace deltas and still rejects
empty ones. The live campaign was not modified or restarted for that fix.
A subsequent real-model run is needed to validate the corrected receiver.

The profile uses an adapter boundary after 800 ms of decoded quiet PCM; this
is not a claim of native model EOS. Shared host load and runtime provenance
are recorded in the copied metadata. `sha256.json` binds the retained files.
