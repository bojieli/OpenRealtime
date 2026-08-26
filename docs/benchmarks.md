# The benchmark harness

Every suite reduces to the same thing: drive a timed environment into a session
and judge what came out. The shared driver can pace audio, schedule protocol
events, negotiate video sources, stream changing frames while the agent acts,
answer tool calls, and retain the resulting observations and actions on one
clock. Suites differ in the environment and scorer, not in protocol plumbing.
When a session ends, the driver stops scheduling frames and joins any capture
already in flight before a suite reuses its environment; a canceled capture
cannot leak into the next case or invalidate a shared browser connection.

The harness drives a **running server over the protocol**, not an in-process
session. A measurement of something other than what users get is not a
measurement of anything.

```sh
openrealtime serve &                       # the system under test
openrealtime bench fdb --limit 4           # a suite against it
```

## Suites

| Command | Suite | Measures |
| --- | --- | --- |
| `bench fdb` | FDB v1.5 | overlap: yielding to interruptions, holding through backchannels |
| `bench fdbv3` | FDB v3 | tool use under disfluent speech, including spelled identifiers |
| `bench fdbench` | FD-Bench | endpointing and response timing at scale |
| `bench tau-voice` | τ-Voice | tool-use success under voice, against a live environment |
| `bench realtime-cu` | OpenRealtime Realtime-CU v1 | repository-owned realtime audio, video, camera, authorization, games, and computer use |
| `bench dynacu` | DynaCU-Bench | optional independent external validation of dynamic computer use |

Every suite here plays a recording at the system. For conversations where both
sides are live — a support call, an interview, an argument over a large
document — see [simulation.md](simulation.md), which connects two agents ear to
mouth and checks what happened between them.

## Cells and pairing

A run measures one **cell**: a complete configuration, including the factors
that did not change. A cell that recorded only its differences could not be
compared against a reference that later moved.

```sh
openrealtime bench fdb -cell reference -out results/reference.json
openrealtime bench fdb -vary F2 -level fast-only -out results/fast-only.json
openrealtime compare results/reference.json results/fast-only.json
```

`compare` refuses anything that would not mean something:

- cells that differ in more than one factor, because a difference cannot be
  attributed to any of them;
- an incomplete cell, because a partially executed cell is a different
  experiment rather than a smaller one;
- a cell from a modified working tree, because it cannot be reproduced;
- a comparison across suites, because two suites measuring different things do
  not average.

Every one of those is a refusal rather than a warning. A measurement program
that warns is a measurement program that gets ignored.

## Reading a result

```jsonc
{
  "suite": "fdb-v1.5",
  "cell": { "name": "reference", "levels": { "F1": "cascade", "...": "..." } },
  "provenance": {
    "revision": "…", "modified": false, "executable_sha256": "…",
    "machine": { "cpu": "…", "gpu": "…" }
  },
  "expected_tasks": 498,
  "tasks": [ { "id": "user_interruption/1", "completed": true, "passed": true,
               "metrics": { "yield_latency_ms": 194 } } ],
  "summary": {
    "complete": true, "pass_rate": 0.87,
    "distributions": { "yield_latency_ms": { "p50": 201, "p95": 640, "max": 980 } }
  }
}
```

Latency is always a distribution. A mean without its tail describes a system
nobody is using.

## What the suites judge

**FDB v1.5** has four categories and two of them want the opposite of the other
two: yield to an interruption, hold through a backchannel, background speech,
and speech addressed to somebody else. A system that scores well by always
yielding is not a system that handles overlap, which is why the report breaks
the four out rather than averaging them.

A recording where the agent was not speaking when the event arrived is reported
as **not applicable** rather than as a pass or a failure. It says something
about latency and nothing about overlap, and folding it in either direction
would corrupt both readings.

**FDB v3** checks the call and its arguments separately. "Track order
B-O-B-1-2" has to become `track_order(order_id="BOB12")`, and reassembling a
spelled identifier from speech is a distinct failure from not knowing which
tool to call. The report says which one happened.

Argument comparison normalises case and the punctuation a recogniser sprinkles
through a spelled identifier — `BOB-12` is the same order as `BOB12` — but not
whitespace. `B O B 1 2` is not `BOB12`: a real API would reject it, and a
scorer that accepted it would report the reassembly working when it was not.

The agent is offered the union of every tool in the dataset, not the one the
task expects. Handing it exactly the right tool would measure whether it can
call the only tool available.

**FD-Bench** measures three things in tension: response latency, whether the
agent started speaking while the person still was, and whether it answered at
all before the next turn. Tuning for any one of them damages another, so a
single number would hide the trade.

It also separates *premature* from *overrun*: an answer that begins while the
person is mid-turn is an endpointing failure, while one that runs past the gap
and is then cut short is what barge-in is for. They have different causes, and
both appear in the metrics — `premature_turns`, `overrun_turns`, and the total
`overlap_ms` — because a single overlap number would hide which one a
deployment has. Only premature turns fail a conversation.

**τ-Voice** is the one suite the harness does not own. tau2-bench has the
domains, the databases, the user simulator, and the reward function, and a
reimplementation would produce a benchmark that agreed with this project rather
than with the published one. So `bench tau-voice` is a runner: it pins the
environment to a revision, points it at a running server, and turns what comes
back into the same report every other suite produces.

Pointing it at OpenRealtime takes no bridge. tau2's audio-native path speaks
the OpenAI Realtime protocol over a configurable base URL, and OpenRealtime is
a strict superset of that protocol, so the endpoint is a flag and the wire is
unmodified in both directions. It is the protocol claim tested by something
that has never heard of this project.

```sh
scripts/prepare-tau-voice.sh              # pin, patch, and check the checkout
openrealtime bench tau-voice -verify      # confirm before spending hours
openrealtime bench tau-voice -condition regular -out regular.json
```

Two conditions carry the measurement. **Control** is clean synthesised speech
and **regular** carries the disfluencies, backchannels, and non-directed audio
a deployed system actually receives; between them sit ablations that hold one
group of effects constant, so a system that loses ground can be told *which*
part of realistic speech it lost to. A number from control alone describes a
recording studio.

Task success is only half of it. `-interaction-metrics` runs tau2's own
turn-taking computation — response and yield latency, response and yield rate,
the three selectivity measures — because a system can pass every task while
talking over the caller throughout, and nothing in a pass rate would say so.

Two behaviours in the runner are worth knowing about. A restricted run — one
domain, a task limit, named task IDs — is reported **incomplete** no matter how
well it scores, because it is not the declared suite. And a simulation that
never reached evaluation is recorded as incomplete rather than as a failure: an
endpoint that was down is not a benchmark result, and scoring it zero is how
infrastructure trouble becomes a published capability claim.

A connected session that continues acting beyond a suite's evaluation horizon
is different: the environment did run and its deterministic state can be
scored. Realtime-CU records that case as a completed negative result with
`session_timeout_count=1`, retaining its observations and action trace. Only a
failure to reach or read the evaluator remains an infrastructure failure. A
protocol `error` from ASR, cognition, the engine, or transport is also
incomplete: the driver returns a typed session failure with the transcript
evidence, rather than scoring an unavailable provider as an incapable agent.

## OpenRealtime Realtime-CU v1

This is the repository-owned audiovisual computer-use capability and release
suite. Its task definitions, authored audio, browser environment, live screen
and camera capture, action evaluator, and scorer ship in `bench/realtimecu`.
No external checkout or model vendor is part of its definition.

Eight diagnostic task families run under both pixel and set-of-mark grounding,
for sixteen declared cases:

| Task family | What it isolates |
| --- | --- |
| static control | ordinary audio-to-screen action control |
| transient deployment alert | a short-lived visual cue and reaction deadline |
| live temperature threshold | continuous visual-temporal monitoring |
| moving target | game-like realtime interaction |
| spoken colour choice | cross-modal audio instruction and visual grounding |
| physical-camera smoke | camera evidence with a separate screen-only action target |
| untrusted payment prompt | authorization and prompt-injection resistance |
| incident-code form | dependent click, literal typing, and submission |

Pixel grounding is the portable baseline for a desktop, virtual machine, or
Android display. Set-of-mark grounding is the browser condition: the evaluator
renders numbered bubbles over interactive elements, and
`computer.click_element` resolves the visible label at action time. DOM access
stays inside the browser grounding/evaluator boundary; the model never receives
a CSS selector or hidden task state. A deployment-bound pixel tool schema names
the target's exact inclusive coordinate maxima, while the portable published
vocabulary remains dimension-free. Realtime-CU also repeats the viewport extent
in its pixel instruction so a model cannot silently assume the source image's
encoded or training-time dimensions.

```sh
openrealtime serve \
  -computer-use -fast-computer-use \
  -fast-provider google -fast-sees \
  -observers audio+video -observer-components keyframe

openrealtime bench realtime-cu \
  -grounding pixel,set_of_mark -fps 3 \
  -vary F10 -level bounded-fast \
  -out results/realtime-cu.json
```

The `-vary F10` label is part of the measurement: omit it only when the server
uses the suite's `slow-only` reference authority. A server option cannot be
inferred through the benchmark WebSocket, so the runner requires the operator
to record the paired cell explicitly.

`-categories`, `-grounding`, and `-limit` are diagnostic filters. A filtered
run always declares the full sixteen-case suite as expected and is therefore
incomplete and non-reportable. `-list` prints the owned tasks without starting
a browser.

Correctness and timeliness are separate outputs. `correct_action_rate` records
functional success even when an action was late;
`task_success_rate`/`deadline_miss_count` express the task's realtime outcome.
The report also retains cue-to-first-tool, cue-to-effectful-action,
speech-end-to-action, cue/frame-to-observation, action execution, and total
completion latency, with sample counts and distributions. Screenshot, wait,
and pointer-move calls do not masquerade as the first effectful action. Per-case
notes retain the recognized user turns beside the action trace, which separates
speech-recognition mistakes from downstream reasoning and grounding mistakes.

The evaluator controls reset and scoring through a private browser control
plane. Model actions travel only through declared `computer.*` tools and see
the consequence in subsequent video frames. A separate camera source is
observation-only; every action must still name the declared `screen` source.

## DynaCU-Bench (optional external validation)

150 browser tasks: 100 dynamic ones across ten categories that a
screenshot-only agent cannot solve — podcasts, meetings, video, carousels,
live dashboards, transient UI, phone calls, interviews, collaborative editing,
games — and a static 50 that any agent should, which is the control saying
whether perception cost anything where there was nothing to perceive.

The AOI repository has the
task pages, the Playwright environment that serves them, the audio injected
into them, and the evaluator that decides whether a task passed; a
reimplementation would produce a benchmark that agreed with this project rather
than with the published one. So `bench dynacu` is a runner: it pins the
environment to a revision, points it at a running server, and turns what comes
back into the same report shape every other suite produces.

It is useful independent validation and remains intentionally unmodified, but
it is not an OpenRealtime dependency, capability definition, release gate, or
publication prerequisite. The owned Realtime-CU suite fills those roles.

```sh
scripts/prepare-dynacu.sh                  # clone, pin, and check the environment
openrealtime serve &
openrealtime bench dynacu -verify          # confirm before spending hours
openrealtime bench dynacu -out results/dynacu.json
```

Pointing it at OpenRealtime needs no bridge, for the same reason τ-Voice does
not. The suite's own GA Realtime baseline is already provider-agnostic — its
websocket base, credential, and image support are constructor arguments,
because OpenAI and xAI both speak that protocol — and OpenRealtime is a strict
superset of it, so it is a third value for the same argument. Nothing in the
benchmark is patched and nothing in it knows this project exists.

That is also what makes it a test of the protocol rather than of our adapter.
Running it found four things a client written against the official API needs
and this server did not have: text output, client-declared turns, images
attached to a message, and every output item naming the response that produced
it rather than each output kind opening one of its own. Each of those is now a
capability rather than a workaround, and the benchmark is unmodified.

The report breaks out the eleven categories rather than averaging them, and
counts **invalid** separately from **failed**: a task where every model call
failed says something about the endpoint and nothing about the agent, and
scoring it zero is how infrastructure trouble becomes a published capability
claim. A restricted run — one category, a task limit, named task IDs — is
reported incomplete however well it scores.

## When a conversation is over

A driver ends a recording when the session goes quiet after playback — but
quiet only means finished while the agent owes nothing. This system's reasoning
phase never speaks, so a turn that needs it produces a gap whose length is a
property of the question, and a driver that read silence as completion would be
scoring the agent on what it finished before a stopwatch rather than on what it
can do.

So an open response — created and not yet done — counts as work still owed, and
the short quiet test does not apply while one is open. `WorkingTimeout` bounds
that case separately, because a server that opens a response and never closes
it has to fail rather than hang. If a cell reports tasks that plainly should
have used a tool and did not, check this first: the question is whether the
answer never came or whether nobody was still listening.

## Adding a suite

Implement `Load` for the dataset and a function that turns a
`bench.Transcript` into a `bench.TaskOutcome`. The transcript is a timed record
of everything that happened — user speech boundaries, transcripts, agent text
and audio with durations, tool calls — so a new metric is usually a question
about that record rather than new plumbing.

Then let `bench.Result.Finish` derive the summary, and `Reportable` decide
whether it may be published. Do not compute a pass rate yourself: the point of
the harness is that no suite gets to decide it is complete.
