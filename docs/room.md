# Conversation room

Build with Go 1.25 or later, then start the shared browser/macOS room:

```sh
go build -o openrealtime ./cmd/openrealtime
./openrealtime companion
```

Open `http://127.0.0.1:8767`. Use `-client none` to start without opening a
browser, or `-client both` on a Mac with the application installed.

The room itself requires Linux. It freezes its selection into a launch profile,
and reading a profile file back uses a hardened open implemented for Linux only,
so on macOS `serve` stops with `secure launch-profile file opening is
unsupported on this platform` before it listens. That bound covers every
launch-profile path below, including an `-launch-profile` you author. The macOS
application still connects to a room served from Linux, and on a Mac alone it
runs against an explicitly composed cascade instead - see
[Use local models](guides/local-stack.md).

The default room uses OpenRealtime's graph-native twelve-scenario pipeline:
Deepgram Nova-3 streaming recognition, local Qwen interaction decisions,
Gemini 3.5 Flash cognition, local Fish Speech synthesis, speaker embeddings,
and word timing. Set `DEEPGRAM_API_KEY` and `GEMINI_API_KEY` in the launching
environment. The local services must already be running:

| Component | Default endpoint |
| --- | --- |
| Qwen policy, with vision | `http://127.0.0.1:8000/v1` |
| Fish Speech | `http://127.0.0.1:8123/v1/tts` |
| Speaker identification | `http://127.0.0.1:8124/embed` |
| Word timing | `http://127.0.0.1:8127/v1/audio/transcriptions` |

Word timing is `deploy/wordtimings`, not the speech recogniser. It must return
a `words` array; a server that answers this route with text alone leaves every
interruption boundary proportional, which the runtime now reports as
`speech.word_timing_failed` rather than absorbing.

The room freezes the selected graph into a temporary launch profile, verifies
it using the same registry as the evaluation, and removes it on shutdown.
Missing credentials or services produce errors; there is no fallback to a
hosted Realtime endpoint. `companion -- -binding upstream` is rejected.
A separately authored `-launch-profile` can select another OpenRealtime graph.
For example, `profile scenario` authors the all-local SenseVoice/Qwen/Fish
variant; pass its output with `companion -- -launch-profile /absolute/path.yaml`.
The model and credential environment then come from that frozen profile.

## Media and recording

Join the room, then use the controls independently:

- **Microphone:** mute or unmute the outgoing audio track.
- **Agent audio:** mute playback without muting the microphone or cancelling the agent.
- **Camera:** start or stop physical-camera capture and its preview.
- **Share screen:** choose a screen/window/tab. Stopping it leaves the camera running.
- **Agent tile:** hide or show the agent's visual representation. The present
  pipeline produces audio, so the tile does not pretend to be a video feed.
- **Add image:** send a JPEG, PNG, or WebP image through the same image-message
  path used by the visual benchmark case. Images are resized before sending.
- **Record room:** start/stop a local recording and download it. Browser
  recordings compose the participant tiles and shared screen, mixing unmuted
  microphone and agent audio. They stop on leaving the room and at 256 MB.

Browser microphone, camera, and screen capture require localhost or HTTPS.
Screen selection always uses the browser's permission picker. Support varies
by browser/device, especially on mobile; capture errors appear in the room.

The native room uses SwiftUI with the same catalog, reducer semantics, protocol,
layout, and independent controls. Camera and screen previews come from the
existing native capture providers. Native recording requires macOS 15+ and
uses ScreenCaptureKit to record the room window, application audio, and the
enabled microphone to MP4. Other room features retain the macOS 14 minimum.
On Linux, Go validates native composition and resources; build/install and
permission testing must run on macOS with Xcode 16 or later.

## Practice the twelve scenarios

Expand **Practice an interaction scenario**, select a case, and apply it while
connected. Both clients use instructions, scripts, and tool declarations from
`bench/scenario.Suite()`. Read the script aloud at your own pace. Use a fresh
session for each recorded case so preceding instructions do not affect it.
The recorded-menu case handles `press_key` as an explicitly simulated local
keypad; it does not place a telephone call.

These practice sessions are not scored benchmark runs. Use the existing
[scenario evaluation workflow](../bench/scenario/graphnative/README.md) for
behavioral measurements and retained evidence. A single diagnostic pass does
not establish the suite's repeated-run acceptance criteria.

The same twelve scenarios also run in-process against the real policy and
voice models, with the script delivered as a scripted transcript at a
person's pace, pictures shown at their moment, a phone menu that answers
the keys the agent presses, and the graph's own real-time playback:

```sh
OPENREALTIME_SCENARIO_BENCH=1 go test ./graphs -run TestTwelveScenariosAgainstLiveModels -v -count=1 -timeout 40m
```

Each run is scored by the scenario package's own checks and then reviewed
by Gemini as a judge, which reads the whole exchange and says whether the
agent acted at the right moment, once, with no erroneous or duplicate
action; both verdicts and the run's timeline are written under
`.runtime/scenario-bench/`. `OPENREALTIME_SCENARIO_BENCH` can name a
comma-separated subset and `OPENREALTIME_SCENARIO_JUDGE=0` skips the judge.

## One step at a time

The interaction policy runs in lockstep with the voice. Every transcript
event - each Deepgram partial and each final - is a step: the policy is
asked, with the full option set, what to do; if it invokes the voice, nothing
else is decided until the model has answered; then the next step is decided
against the latest transcript and everything the model just said. Revisions
that arrive while a step is running collapse to the newest one, so the policy
sees a sequence of events, never a backlog, and the model is never asked twice
at once. Each step is shown the recent steps - what was heard, what was
chosen, what the agent then said or did (a key it pressed counts as an
answer, and appears in the conversation as something the agent did) - the
words new since the previous step, and what was already answered in this
utterance.

The policy model is asked about a step in narrow yes/no questions, and the
runtime composes the choice from the answers: while the agent is speaking, is
the person cutting in (`stop`)? Do the new words hold a new occurrence of a
standing instruction (`speak`)? On a partial with no standing instruction
in force, is what they are saying so far urgent enough to act on before
they finish (`speak`)? On a final with nothing due, is there a
request to answer (`speak`)? If so, was it put to the agent at all, rather
than to somebody else in the room (everything arrives down one microphone,
and the evidence says when the previous line was a question the agent chose
not to answer)? When the event is the clock - fifteen seconds of quiet after
the last commit - has a standing instruction that waits on silence come due
(`speak`)? A fast instruct model answers those reliably
where it could not apply a page of rules to one constrained token: on the same
recorded decisions, 13 of 37 right as a single choice against 33 to 34 asked
this way, and 39 of 40 in the live benchmark. The answers are recorded on
every decision (`questions`) and shown on the timeline's policy lane.

The stop answer is the only thing that takes the floor from the voice. The
overlap element still tracks who is speaking over whom, but it has no
classifier of its own (`overlap_barge_in.decider` is empty) and its fallback
keeps speaking: with a classifier it asked the same decider an older
question - "is this directed speech?" - beside the policy's, and acted on
that answer alone. In the live room it cancelled a key press the policy had
just chosen because the recorded menu read on, cancelled the acknowledgement
of a translation rule because the person kept talking, and cut a count the
policy had decided to keep, fifteen milliseconds before the policy said
keep. One question, one decider.

A decision that waited for the voice to finish is taken against everything
the voice said meanwhile, and the generation it admits runs on that context.
A tool call from such a generation - a key pressed at a menu while the
recording read on - keeps its authority: the commit names the context's
tail (`tail_item_id`), the candidate is marked as an extended context, and
admission asks that the observation precede the tail rather than have
produced it. A newer revision that only adds words after the ones the call
answered does not take its basis away either; only a rewrite does. When
that newer revision lands while the voice is still generating, the model's
commit is retried onto the newer trajectory with its proposals kept, so a
key press made while the recording read on still reaches admission. One
guard sits after the decision: a call proposed on a partial revision that
repeats, name and arguments, a call already made since the last settled
observation is refused as `duplicate_call` - the same words decided on
again are not a new occasion, and the voice, shown the key it had pressed,
pressed it again four runs out of four. A call proposed on settled evidence
(a finished turn, a frame) is never refused this way.

A voice request that the provider accepts and then leaves silent is sent
again after six seconds (`gemini.Config.FirstEventTimeout`, the fast phase's
default; the reasoner has none). Every measured generation in the harness had
its first clause playing within 4.6 s; the one that stalled had delivered
nothing after 6 s and, bounded only by the 30 s request timeout, it cost the
turn it answered. A stream that has sent no event has said nothing the
conversation could hear twice, so the second attempt is safe; one that has
sent anything is never replayed.

The voice says nothing by answering with exactly `<wait>`. The segmenter
reassembles the token from the pieces a streaming provider hands over, never
speaks it, and silences everything after it; words before it are still said,
because "Two.<wait>" is a count with the closing token in the same breath and
losing the count is worse than an extra sentence.

Standing instructions come from two places. The person sets them out loud -
"count the animals as I mention them", "if I go quiet for fifteen seconds,
ask whether I'm still there" - and the extraction pass pins them from each
final. The session instruction sets them too: "when a recorded menu offers
the option the user wants, press that key", "if they say a date that
contradicts the third, correct them immediately". Those are read once, from
the instruction text, on the first decision that sees it, and pinned as the
operator's: in force for the whole conversation and not the room's to lift.
Without them the policy listened through every partial of a menu reading out
the right option, because its question is about the instructions in force
and the rule lived in a paragraph addressed to the voice.

The counting benchmark exercises exactly this against the real policy and
voice models with a scripted transcript, no audio and nobody speaking:

```sh
OPENREALTIME_COUNTING_BENCH=1 go test ./graphs -run TestCountingBenchmark -v -count=1
```

It needs the vLLM policy (`OPENREALTIME_POLICY_URL`, default
`http://127.0.0.1:8000/v1`) and `GEMINI_API_KEY`. Six stories run as
subtests - animals at a zoo, language models tried in a week, fruit with a
question in the middle, a twelve-sentence safari, programming languages with
animals as distractors, and cities until the person says to stop counting -
and `OPENREALTIME_COUNTING_BENCH` can name a comma-separated subset. Each
prints a scorecard - every item counted once, in order, on the partial that
named it; partials of sentences with nothing to count listened to; the
question answered on its final; nothing counted after the rule is lifted;
nothing but the numbers and the answers said; no overlapping generations -
and writes its timeline under `.runtime/counting-bench/`.

## Reading a turn back

Every developer profile draws a **Turn timeline** above the session details:
five lanes across time - ASR (speech activity and each Deepgram revision),
Policy (the interaction model's choice on every partial and final, with the
evidence it was shown), LLM (request, text, outcome, and every stage a tool
call passes through - a key press the voice proposed and admission refused
shows there with the reason), TTS (synthesis,
speaking, playback), and Background (every question the standing-instruction
pass asked and what it answered). It moves with the conversation; **Paused**
freezes it, the slider scrubs back through the session, and **Replay from
here** plays it forward again at real speed. Hovering a mark shows the whole
event; the list underneath is the same events as text, one line each.

The page receives these as the `timeline` debug category with payloads (see
`docs/protocol/openrealtime-1.md`). The same lines can be kept on disk: start
the server with `-timeline-log <file>` (or pass it after `--` to `companion`)
and it appends one line per event for every session - `time session LANE
kind detail "words" [span]`. The file carries what people said and what the
models answered; keep it private. `tools/tracelog/render.py` renders the
older, complete JSON debug log the same way when the server ran with
`-log-level debug -log-format json`.

## Verification and generated assets

```sh
go test ./presentation/browser ./cmd/openrealtime ./macos \
  ./graphs ./graph/binding/scenarioconversation ./bench/scenario/graphnative
node presentation/browser/testdata/room.mjs http://127.0.0.1:8767
```

The browser probe uses headless Chromium with simulated microphone/camera/screen
sources and tests a real model answer, media independence, image submission,
recording playback, and leave/disposal. Set `ROOM_SCREENSHOT` to save a PNG.
`CHROMIUM` and `CDP_PORT` override the browser executable and debugging port.

After native source or scenario-catalog changes, regenerate the source-bound
native manifests and shared scenario resource:

```sh
go run ./tools/room-assets
```

For a single live diagnostic attempt of every canonical scenario against a
running room (including recorded input synthesis and output transcription):

```sh
OPENREALTIME_ROOM_TEST_ENDPOINT=ws://127.0.0.1:8765/v1/realtime \
  go test ./cmd/openrealtime -run '^TestLiveRoomTwelveScenarios$' \
  -count=1 -v -timeout=45m
```

This opt-in test verifies the reported runtime binding and fails on any
behavioral check. It uses the local Fish speech and word-timing endpoints,
retains structured results in the test log, and does not certify the repeated
benchmark. Provider routing is inherited from the process environment.
