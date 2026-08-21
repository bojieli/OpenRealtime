# Two agents, talking

Every suite in [benchmarks.md](benchmarks.md) plays a recording at the system
and scores the reply. That measures the system against a person who is not
there, and it can only ever measure the half of the conversation the recording
is not doing. The recording never hesitates, never interrupts at the wrong
moment, never mishears the reply, and never decides the call is over.

The motivating scenarios for this project are not recordings. A support call, a
technical screen, an argument over a specification — each is two parties taking
turns, and none of it is observable from one side.

So `openrealtime simulate` connects two sessions ear to mouth.

```sh
openrealtime serve &
openrealtime simulate -endpoint ws://127.0.0.1:8765/v1/realtime
```

What one agent says is synthesised, carried as audio, recognised by the other,
and answered. There is no text shortcut anywhere in the loop. A turn only
becomes a turn if the speech survived synthesis, the link, and recognition —
which is the path a deployment has, and the path a text harness cannot test.

## The link

The wire ticks every 20 ms and always carries something in both directions.

That is the whole trick. A real microphone does not stop producing frames when
nobody is talking, so an endpoint detector on the far side only fires if
silence arrives as reliably as speech does. A relay that forwarded audio only
when there was audio would produce a system that never finished a turn, and the
failure would look like the model's fault.

Audio is queued at the listener's ear and paced out in real time, because
synthesis is faster than speech: a model produces a ten-second answer in two,
and forwarding it as it arrives would deliver ten seconds of speech in two —
gabble to the recogniser, and an endpoint placed in the wrong second. When a
speaker is cut off, what is still queued is discarded: it is speech that was
never uttered, and delivering it anyway is the one thing barge-in must not do.

## What is recorded

A turn is what the **listener heard**, not what the speaker said. A turn the
speaker generated and the listener never received does not appear in the
record, which is correct — it did not happen.

Both are kept, though. `Said` is what each side's own model produced, and
comparing the two separates a recognition failure from a reasoning one. A
scenario that fails because a number was mangled in transit is a different
problem from one that fails because neither model knew the number, and a report
that could not tell them apart would send you to the wrong place.

Speech and overlap are counted in frames the link actually carried, so
"overlap" means two voices arriving at once rather than two models generating
at once. Some overlap is barge-in working. A lot of it is two systems talking
past each other with nobody holding the floor, which is why the check is a
bound rather than zero.

## The scenarios

| Scenario | What it asks |
| --- | --- |
| `support` | can an identifier spoken by one agent reach the other's tool call intact? |
| `interview` | do two agents alternate cleanly over many short turns? |
| `debate` | can two agents argue from a large shared document at conversational latency? |

**support** is the one that cannot be faked. The customer's order number is
synthesised, carried as audio, and recognised by the agent, and the check looks
for it inside the agent's tool call. Arriving there unmangled is the only proof
it survived, and a paraphrase would hide the failure. This is the same
spelled-identifier failure FDB v3 measures, but with both halves live.

**interview** is turn-taking. A system that answers well but talks over its
counterpart, or waits four seconds before every reply, fails a conversation
while passing every single-turn benchmark there is. The check counts
consecutive turns by the same speaker: one is a clarification, several mean the
floor was lost.

**debate** hands both sides the same long specification — around 8,000 tokens —
and asks them to argue from it. A figure appears exactly once, in the middle,
phrased like the measurements around it and marked in no way. The check looks
for that figure in speech. Retrieving it while a real-time conversation is in
flight is a different problem from retrieving it in a single-shot answer: the
context has to stay usable at conversational latency, turn after turn.

## Writing one

A scenario is two roles and what has to be true afterwards:

```go
simulation.Scenario{
    Name:  "handover",
    Left:  simulation.Role{Name: "caller", Instruction: …, Opens: true, Cue: …},
    Right: simulation.Role{Name: "desk", Instruction: …, Tools: …, Tool: …},
    Checks: []simulation.Check{ … },
}
```

Two things are worth knowing before writing one.

**Say what the role is not.** A session instruction is composed ahead of the
phase instruction, which frames the model as the voice of a helpful agent. Left
alone, both sides drift into being the assistant — the *customer* opens with
"how can I help you?" — and the conversation measures nothing.

**Give the opener its first line.** A bare `response.create` asks the model to
continue an empty conversation, and it has nothing to say. The cue is delivered
to the opening side alone, never reaches the other, and never becomes a turn.

**Check substance before anything else.** Turn counts are not evidence that a
conversation happened. An interview here once reported four passes while one
side had contributed half a second of noise: its counterpart's every utterance
was answered with a grunt, so turns alternated perfectly and overlap was
negligible. Both of those checks were true and neither meant anything. Every
scenario now requires each side to have said something before the rest of its
checks are worth reading.

Every check states why it matters, and a failing run prints that reason. A
scenario that fails should say what the system failed to do, not that assertion
four returned false.

## Running it locally

Two agents means two sessions, and two sessions means two concurrent
recognition streams. That is worth knowing before pointing this at a
development recogniser: the Qwen3-ASR demo server drives one vLLM engine
client from its request handler with no locking, and two interleaved sessions
crash its engine core with

```
ValueError: b'\x00\x00' is not a valid EngineCoreRequestType
```

after which every request hangs rather than failing. A serialising proxy in
front of it is enough — each chunk is tens of milliseconds against a 200 ms
cadence, so serialising two streams costs latency and not correctness. A
production recogniser will not need one.

The scenarios are also demanding of the model playing a part. A session
instruction is composed ahead of the phase instruction, which frames the model
as the voice of a helpful agent, and a small local model does not always hold
a role against it — a customer that drifts into offering to look up its own
complaint is the characteristic failure. That is a finding about the
configuration rather than a fault in the harness, and it is visible in the
transcript rather than hidden behind a pass.
