# Reproduce the live meeting-assistant deployment

This is the specialized, multi-model deployment used by the repository's live
meeting evaluation. It is preserved as a reproducible guide for contributors
and benchmark operators; it is not the recommended first run. Start with the
[quickstart](../quickstart.md) if you simply want to talk to OpenRealtime.

## What the deployment compares

The setup keeps a common background reasoner and interaction controls while
varying the foreground voice path:

| Cell | Foreground path | Shared controls |
| --- | --- | --- |
| Cascade | SenseVoice streaming ASR → Qwen3-VL-8B → Fish speech | direct keyframes, Qwen interaction policy, bounded computer actions, Gemini slow cognition |
| Omni | raw audio + direct images → Qwen3-Omni native speech | the same Qwen policy, SenseVoice control transcript, bounded actions, Gemini slow cognition |

The currently documented validated meeting runs exercise the cascade cell.
They do not establish a Cascade-versus-Omni ranking unless the separate Omni
sidecar and the same complete suite are run.

## Before you start

- Prepare the SenseVoice and Fish speech services used by the cascade path.
- Set `GEMINI_API_KEY` for the default slow reasoner.
- Inspect GPU memory before starting the Omni checkpoint beside the policy
  service. The default Omni checkpoint is approximately 35 GB.
- Review the configurable `MEETING_*` variables at the top of
  `scripts/meeting-assistant.sh`.

The launcher keeps every server in the foreground and never kills an existing
GPU process. It builds a dedicated binary under `.runtime/meeting-assistant`
and writes result JSON there.

## Run the cascade cell

Use separate terminals for the long-lived processes:

```bash
export GEMINI_API_KEY="your-key"

# Interaction policy service.
./scripts/meeting-assistant.sh policy
```

```bash
# Cascade server; SenseVoice and Fish must already be running.
./scripts/meeting-assistant.sh cascade
```

```bash
# Complete cascade meeting suite.
./scripts/meeting-assistant.sh bench-cascade
```

## Run the Omni cell

Start the persistent model process, verify its sidecar contract, then launch
and benchmark the cell:

```bash
./scripts/meeting-assistant.sh omni-sidecar
```

```bash
./scripts/meeting-assistant.sh conformance-omni
./scripts/meeting-assistant.sh omni
```

```bash
./scripts/meeting-assistant.sh bench-omni
```

The cascade and Omni servers expose both WebSocket and direct WebRTC. Their
default WebRTC listeners are `127.0.0.1:28786` and `127.0.0.1:28787`,
respectively. Microphone audio uses the media track; selected screen and camera
frames travel as OpenRealtime video events on the WebRTC data channel.

## Override one controlled component

The recogniser is recorded as part of each measured cell. Select another one
without changing the meeting runtime:

```bash
export MEETING_ASR_PROVIDER=whisper
export MEETING_ASR_URL=http://127.0.0.1:8003/v1
export MEETING_ASR_MODEL=openai/whisper-large-v3-turbo
```

`MEETING_SLOW_PROVIDER`, `MEETING_SLOW_URL`, and `MEETING_SLOW_MODEL` similarly
select the asynchronous reasoner. The benchmark launcher writes the actual
recogniser, policy, foreground, and slow-model identities into the result cell,
so an override cannot remain mislabeled as the default deployment.

## How the cascade cell is divided

The shipped topology is intentionally asymmetric:

- Qwen3-VL-8B owns the conversational fast voice and interaction policy.
- A larger local Qwen3-VL model is the silent direct-pixel actor.
- Gemini is the asynchronous slow reasoner and authoritative semantic-tool
  user.
- Streaming ASR, interaction, and action opportunities run on a 200 ms clock.
- Ordinary speech uses a separate one-second audible endpoint so UI commands
  can act early without fragmenting conversation at every partial transcript.

The conversational model is proposal-only for computer use. A separate silent
visual actor is the sole owner of screen effects, preventing the spoken answer
and visual monitor from both acting on one obligation. It receives the newest
retained screen frame even when speech and the frame do not arrive in the same
200 ms batch.

A visual `WAIT` closes only the visual branch. Speech and slow work continue,
and later visual evidence resumes monitoring. Autonomous screen observations
remain admissible while agent audio is playing, so a silent alert action does
not wait for the presentation to end.

## Coordinate and action discipline

The actor may use literal target pixels with `computer.click` or the 0–1000
convention with `computer.click_normalized`. The dispatcher performs the
declared conversion; the runtime never guesses which coordinate system the
model meant.

The controller also receives unresolved tool names and the most recent
computer action/result. Identical unresolved calls are suppressed. Nearby
repeats of a completed normalized coordinate are no-ops.

Visual control uses receding-horizon action chunks:

1. the actor commits at most one bounded effect;
2. adaptive observation admits a fresh post-action frame;
3. the actor may then ground the next effect.

While a vision-model request is in flight, newer ASR or frames replace the
pending request instead of forming a stale queue. A canonical endpoint cancels
visual inference based on the superseded live prefix and takes over. The next
unfulfilled explicit UI clause is carried forward; an incomplete tail such as
“share your” cannot authorize a guessed click.

Typed authority applies on every entry path. A conservative deterministic
compiler accepts only high-confidence UI imperatives; learned policy handles
ambiguous and future-monitoring language. The pixel actor must return a private
grounded target label, which is checked against the next requested control and
stripped before execution.

Rejected calls are closed as controller placeholders so a corrected retry is
not mistaken for unresolved work. Canonical transcript updates also handle
within-word completion, such as a live prefix ending in “over” becoming the
final word “overview.”

## Composite work remains concurrent

A request may contain an immediate semantic clause and a visual condition.
Typed decomposition lets the semantic answer proceed while the visual monitor
stays armed. A slow tool result wakes the fast voice even if the slow reasoner
adds no prose; the tool result already committed to the trajectory remains the
source of truth.

The fast action path sees pixels directly rather than waiting for narration.
Adaptive observation may still retain keyframes and optional narration as
persistent context outside that reflex path.

## The Omni cell's control transcript

The current Qwen3-Omni checkpoint does not independently emit a sufficiently
reliable user transcript for this controlled deployment. Raw audio still goes
straight to Qwen3-Omni, but SenseVoice supplies control evidence and canonical
user text for the background reasoner. The cell is therefore deliberately
`omni+text-policy`, not a transcript-free Omni experiment.

## Interpret results carefully

Read the [meeting benchmark definition](../benchmark-reference.md#openrealtime-meeting-assistant-v1)
before comparing result files. In particular, do not infer any of these from a
cascade-only run:

- Cascade superiority over Omni;
- interaction-model superiority over policy controls;
- general architecture superiority from one model deployment;
- a model ranking when the controlled recogniser, prompts, or model identities
  differ.

The result cell, executable identity, source receipt, and review receipt are
the evidence boundary. See [release validation](../release-validation.md) and
the [measurement record](../measurement.md) for the reporting rules.
