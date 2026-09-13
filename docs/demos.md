# Realtime demos

[← Project overview](../README.md) · [Run OpenRealtime](quickstart.md)

Twelve scenarios explore when an assistant should speak, listen, act, or wait.
They are the project's acceptance test: every one is scripted in the
[interaction suite](../bench/scenario/scenarios.go), played against the real
policy and voice models, scored on what a listener would have heard, and
reviewed by Gemini as a judge. [The default pipeline](room.md#the-default-pipeline)
gives the commands to play them in the harness, and live against a running
room with synthesised speech.

**Videos: not yet published.** The descriptions below say what to watch for
when you play a scenario yourself.

## Start with these

- [Picking up where it was cut off](#12-picking-up-where-it-was-cut-off): hear
  where the assistant resumes after an interruption.
- [An acknowledgement is not an interruption](#9-an-acknowledgement-is-not-an-interruption):
  hear whether “mhm” lets the explanation continue.
- [Telling them what it saw](#10-telling-them-what-it-saw): watch a screen
  change trigger a spoken update without another question.

## 1. Count-as-they-go

Ask the assistant to count animals aloud as a story mentions them.
Watch for one count per animal and silence during unrelated speech and pauses.

**Video:** coming soon.

## 2. Asked not to be interrupted

Think aloud and explicitly ask the assistant to wait until invited to answer.
Watch for it to respect that request through pauses in the monologue.

**Video:** coming soon.

## 3. A recorded menu

Ask the assistant to find an order's status through a simulated phone menu.
Watch for it to select the matching keypad option without speaking over the
recording, and show the resulting menu destination.

**Video:** coming soon.

## 4. Cutting in on something wrong

Give the assistant the correct deadline and ask it to interrupt if a spoken
plan contradicts it. Watch for the correction after the mistake is spoken,
while the speaker is still talking.

**Video:** coming soon.

## 5. Ordering from a waiter

Ask for fish, then have a waiter list the specials. Watch for the assistant
to speak up when it hears a matching dish.

**Video:** coming soon.

## 6. Translating as they speak

Ask the assistant to interpret Mandarin into English as a colleague speaks.
Listen for the translated content and when it arrives relative to the speaker.

**Video:** coming soon.

## 7. Waiting out a silence they asked for

Ask the assistant to check in after about fifteen seconds of silence.
Watch for it to wait and then initiate the requested check-in without a new
spoken prompt.

**Video:** coming soon.

## 8. Somebody else's conversation

Let two other people talk near the microphone while the user works.
Watch for the assistant to stay out of a conversation that is not addressed
to it, even when it hears questions.

**Video:** coming soon.

## 9. An acknowledgement is not an interruption

Say “mhm” and “right, yeah” during a detailed explanation.
Listen for the assistant to keep speaking through those acknowledgements
without stopping or restarting its answer.

**Video:** coming soon.

## 10. Telling them what it saw

Ask the assistant to watch the screen and announce when a build finishes.
Watch for silence while the build is running and a spoken update after the
completion appears on screen.

**Video:** coming soon.

## 11. An ordinary question

Ask “What's the capital of France?” Listen for a brief, relevant answer.
This provides a simple baseline alongside the more unusual interaction cases.

**Video:** coming soon.

## 12. Picking up where it was cut off

Ask the assistant to count aloud, interrupt it, and then ask it to continue.
Listen for it to stop and resume from the count actually heard by the listener.

**Video:** coming soon.

<!-- Publishing checklist for each recording:
- Replace its placeholder with the actual video link or embed.
- Include captions identifying speakers and the behavior to watch for.
- Preserve timing through the relevant interaction; label cuts or speed changes.
- State whether input is live or prerecorded and identify simulated tools.
- Add the recording date, source revision, model/provider configuration, and
  reproduction command or setup link. Do not imply the default quickstart
  reproduces a scenario that requires a different profile.
- Describe the observed outcome and any visible limitation.
Keep detailed traces and scoring in the benchmark documentation.
-->

## Try it yourself

Start with the [browser quickstart](quickstart.md) for a first conversation.
Individual demo configurations and reproduction instructions will accompany
the published recordings.

For scenario definitions and evaluation details, see the
[benchmark guide](benchmarks.md) and the
[sub-turn interaction study](subturn-benchmark-study.md). The study contains
historical diagnostics and their limitations; it is separate from this planned
video gallery.
