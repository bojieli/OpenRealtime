# Room voice diagnostics

These tests exercise the deployed scenario conversation pipeline through real
microphone PCM input. They are live diagnostics, not tau-bench certification.
Run cases sequentially so overlapping test sessions do not distort latency.

The room requires a model output ceiling that includes both thinking and spoken
output. The room retains a 1,024-token ceiling and now requests a 128-token thinking
budget with Gemini 3.7 Flash; see the [latency comparison](room-latency-20260909.md).
The old 128-token total ceiling reproduced empty or clipped answers with
`MAX_TOKENS`. Token exhaustion is now an error rather than successful completion.

The bilingual recognizer does not lock an utterance to Chinese based on a short
greeting or loanword such as 哈喽. Early Chinese selection requires at least four
Han characters and stronger confidence than an available primary reading. Short
Chinese utterances remain eligible at finalization.

The activation guard retains the most recent assistant turn to interpret substantive
caller replies, while excluding earlier caller observations from current-condition
evidence. A reply or concern can continue the conversation without being phrased
as a question.

Speaker attribution estimates voice identity; it does not identify the addressee.
A final utterance that stops existing speech is reconsidered once after a newer
idle output state arrives. Cancellation must not consume the new question.
The room permits direct requests and contextual follow-ups from any participant,
while preserving silence for explicit side conversations. Pure acknowledgements
are silent when the agent is idle. The scenario graph allows 1,800 ms for
unclassified overlap; a classified directed interruption cancels immediately.
This bound gives short acknowledgements time to reach recognition.

## Repeatable audio tests

Export `OPENREALTIME_ROOM_TEST_ENDPOINT` and `OPENREALTIME_TOKEN` for the target
room. Set `OPENREALTIME_ROOM_REVIEW_DIR` to a new absolute directory for each run.
The directory must not already exist. Each test retains a stereo WAV (caller on
left, agent on right), scenario manifest, transcript, and timing evidence.

```sh
go test ./cmd/openrealtime -run '^TestLiveRoomRepeatedAudioQuestions$' -v -count=1
go test ./cmd/openrealtime -run '^TestLiveRoomInterruptAndFollowup$' -v -count=1
go test ./cmd/openrealtime -run '^TestLiveRoomQuestionDuringSpeech$' -v -count=1
go test ./cmd/openrealtime -run '^TestLiveRoomObservedBackchannels$' -v -count=1
go test ./cmd/openrealtime -run '^TestLiveRoomSubstantiveReply$' -v -count=1
```

Change the review directory between commands. The interruption is released only
after captured audio proves the agent is speaking. Failure to obtain that
opportunity fails the test. A later fixed question verifies that cancellation
releases the room for another response. The interruption log reports acoustic
cessation relative to input onset and utterance end using 20 ms RMS windows at
PCM16 threshold 128. The backchannel case requires speech to continue through
listener acknowledgements.

## Adaptive simulated caller and Gemini review

`scripts/room-selftest.py` uses Gemini to play a mildly impatient, nontechnical
internet-support caller. The deployed room plays a patient customer service
agent. The caller's next utterance depends on the actual agent transcript, and
is synthesized and submitted as real audio. The driver waits for all queued
agent output to retire before starting another turn. Use Python 3.10–3.12 with
`requests` and `websockets` 15 or later.

Export `GEMINI_API_KEY` and `OPENREALTIME_TOKEN`; configure any required provider
proxy in the environment. Local services should be in `NO_PROXY`.

```sh
python3 scripts/room-selftest.py \
  --endpoint ws://127.0.0.1:8765/v1/realtime \
  --output /absolute/new/diagnostic-directory
```

The default local speech service is Fish Speech on port 8123. `--tts` and
`--tts-model` can select another OpenAI-compatible WAV speech endpoint. The
output directory includes `conversation.wav`, `result.json`, `events.json`,
`gemini-review.md`, and the raw reviewer response. Gemini reviews relevance,
correctness, language, clipping, repetition, and conversational behavior from
the recording. It is explicitly instructed not to invent millisecond timings.
Its qualitative assessment is separate from the deterministic timing checks.

The same reviewer can review a Go diagnostic's retained stereo recording:

```sh
python3 scripts/room-selftest.py --review-only --output /absolute/diagnostic-directory
```

The adaptive test and interruption test are separate: a successful ordinary
conversation does not establish interruption handling. The eight-second answer
limit is a liveness failure bound, not a claim that eight seconds is acceptable
interactive latency. Inspect every reported latency and the recording.

## Inspecting model context

For an isolated diagnostic server, `OPENREALTIME_CONTEXT_TRACE` selects a
directory containing the exact compiled model request bodies.
`OPENREALTIME_DUMP_POLICY_REQUESTS` selects a JSONL file containing exact policy
requests and responses, including measured decision duration. These traces can
contain conversation content; keep them private and avoid enabling them for
unrelated users.

The opt-in `graph` debug stream now includes typed semantic decisions, admission
outcomes, overlap state, and segmentation outcomes when `include_payloads` is
explicitly requested. Without that option the gateway redacts the payload.
Audio bytes are not copied into these debug records.

PCM measurements describe serialized received audio and injected input, not
physical speaker or microphone latency. Test the user's actual WebRTC network
and device separately. SwiftUI compilation and device permission checks require
a Mac; Linux validates native composition, resource identities, and source
contracts.
