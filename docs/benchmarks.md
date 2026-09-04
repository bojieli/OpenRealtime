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

A graph-native release run freezes the strict scenario profile together with
its exact Graph IR, values, resource-free live-resolution probe, and reviewed
execution requirement. Pass the resulting `execution.json` and `graph.json` to
the suite so every task is checked against authenticated live inspection:

```sh
openrealtime profile scenario \
  -out /absolute/campaign/profile.yaml \
  -graph-out /absolute/campaign/graph.json \
  -values-out /absolute/campaign/values.json \
  -resolution-out /absolute/campaign/resolution.json \
  -execution-out /absolute/campaign/execution.json
openrealtime serve -launch-profile /absolute/campaign/profile.yaml &
openrealtime bench fdb \
  -execution /absolute/campaign/execution.json \
  -inspection-graph /absolute/campaign/graph.json
```

The profile is published last and acts as the marker that its create-only
companions are durable. Omitting the execution pair remains useful for local
diagnosis, but it cannot support a graph-native release claim.

FDB v3 sessions submit the union of every released support tool, so their
strict application profile must select that exact union before the server can
accept `session.update`. Freeze it from the same released dataset the runner
will load:

```sh
openrealtime profile scenario \
  -fdbv3-dataset /absolute/full-duplex-bench-v3/dataset/fdb_v3_data_released \
  -out /absolute/fdbv3-campaign/profile.yaml \
  -graph-out /absolute/fdbv3-campaign/graph.json \
  -values-out /absolute/fdbv3-campaign/values.json \
  -resolution-out /absolute/fdbv3-campaign/resolution.json \
  -execution-out /absolute/fdbv3-campaign/execution.json
```

The freeze path calls the benchmark's own loader and catalog builder, then
embeds the resulting names, descriptions, and argument schemas in the profile.
It does not add those tools to ordinary scenario profiles, whose smaller
authored action surface remains unchanged.

## Suites

| Command | Suite | Measures |
| --- | --- | --- |
| `scenario -architecture-manifest …` | OpenRealtime interaction scenarios | F52 predicate/text/composed/native cells with live architecture attestation |
| `bench architecture` | F52 comparison gate | architecture-only versus system claims |
| `bench fdb` | FDB v1.5 | overlap: yielding to interruptions, holding through backchannels |
| `bench fdbv3` | FDB v3 | tool use under disfluent speech, including spelled identifiers |
| `bench fdbench` | FD-Bench | endpointing and response timing at scale |
| `bench tau-voice` | τ-Voice | tool-use success under voice, against a live environment |
| `bench realtime-cu` | OpenRealtime Realtime-CU v1 | repository-owned realtime audio, video, camera, authorization, games, and computer use |
| `bench meeting` | OpenRealtime Meeting Assistant v1 | concurrent listening, speaking, shared-screen action, correction, and slow document work |
| `bench dynacu` | DynaCU-Bench | optional independent external validation of dynamic computer use |

For an FDB v3 repair loop, `-tasks id-a,id-b` reruns exact released directory
identities while still exposing the full released tool catalog. The resulting
partial artifact is diagnostic-only and cannot satisfy the 100-task release
population; after it passes, rerun all 100 tasks from the frozen candidate.

Architecture experiments need more identity than the generic factor table can
carry. [Architecture experiments](architecture-experiments.md) defines the
versioned manifest, live session evidence, unavailable-cell handling, and the
P/T/C/N pairing rules.

Their authoring path is part of the binary and shares production parsers with
the server; it is not a set of launch scripts:

```sh
# Start and inspect an exact immutable definition.
openrealtime serve -architecture cascade.controlled@3 &
openrealtime bench architecture inspect \
  -out results/cascade-P-status.json

# Combine structure, negotiated status, and immutable deployment pins.
openrealtime bench architecture cell \
  -name cascade-P -definition cascade.controlled@3 \
  -status results/cascade-P-status.json \
  -pins experiments/local-pins.json \
  -out experiments/cascade-P-cell.json

# Assemble all reviewed desired cells, then run one at a time.
openrealtime bench architecture manifest \
  -name f52-local -fixture-revision git-blob:012345... \
  -cell experiments/cascade-P-cell.json \
  -cell experiments/cascade-T-cell.json \
  -cell experiments/cascade-C-cell.json \
  -out experiments/f52-local.json
```

`inspect` refuses a server with no architecture identity. `cell` refuses a
definition/status/pins disagreement. `manifest` refuses an invalid or duplicate
cell. `scenario` still independently validates the live status on every task,
so a server replacement after authoring cannot inherit the authored label.

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
openrealtime bench tau-voice inventory \
  -out tau-voice-base-inventory.json      # exact direct-candidate task IDs
openrealtime bench tau-voice -condition regular -out regular.json
```

The inventory command calls tau2's own `load_tasks(domain, "base")` for all
three domains. It accepts only the pinned revision with clean task and loader
inputs, overrides `TAU2_DATA_DIR` and `PYTHONPATH` to the verified checkout,
and emits strict canonical JSON. It also checks the exact published partition
(50 airline, 114 retail, 114 telecom), not just the plausible total of 278.
Calling the loader without the explicit `base` split would currently select
2,285 telecom tasks and is therefore rejected by this boundary.

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
Android display. Its selected click contract uses explicit 0–1000 normalized
frame coordinates, which the target-bound dispatcher maps into the exact CSS
pixel space before applying its coordinate fence. This avoids treating a
vision model's normalized point as an absolute pixel while retaining a
pixel-only observation boundary. Set-of-mark grounding is the browser
condition: the evaluator renders numbered bubbles over interactive elements, and
`computer.click_element` resolves the visible label at action time. DOM access
stays inside the browser grounding/evaluator boundary; the model never receives
a CSS selector or hidden task state. The portable published vocabulary remains
dimension-free, while the deployment-bound target and dispatcher retain the
exact inclusive CSS coordinate maxima.

Launch the locked graph-native provider through the constructor and exact
profile-artifact path described in
[Graph-native Realtime Computer Use](realtime-computer-use-graph.md). It serves
the unchanged Realtime extension endpoint and has no Realtime-CU implementation
switch or fallback to the legacy `-computer-use` / `-fast-computer-use` path.
Then run the complete suite against that one endpoint:

```sh
openrealtime bench realtime-cu \
  -endpoint ws://127.0.0.1:8765/v1/realtime \
  -grounding pixel,set_of_mark -fps 3 \
  -vary F10 -level bounded-fast \
  -out results/realtime-cu.json
```

The `-vary F10` label is part of the measurement: omit it only when the server
uses the suite's `slow-only` reference authority. A server option cannot be
inferred through the benchmark WebSocket, so the runner requires the operator
to record the paired cell explicitly.

When a later experiment holds an earlier change fixed, record that shared
configuration with `-reference-levels`. For example, an F9 hosted-versus-local
vision pair that keeps the bounded fast action lane in both cells uses
`-reference-levels F10=bounded-fast -vary F9 -level local-vlm`. The baseline
uses the same reference override without `-vary`; the two artifacts then differ
only in F9.

`-categories`, `-grounding`, and `-limit` are diagnostic filters. A filtered
run always declares the full sixteen-case suite as expected and is therefore
incomplete and non-reportable. `-list` prints the owned tasks without starting
a browser.

Correctness, settlement, and timeliness remain separately inspectable.
`correct_action_rate` records that the intended browser effect occurred even
when later work remained unsettled. `task_success_rate` additionally requires
the complete settlement witness and a quiescent, error-free session, but it
does not erase an otherwise settled result solely because it was late.
`deadline_miss_count` records that independent timing failure, and the
case-level `Passed` bit is the conjunction of settled task success and no
deadline miss. Realtime aggregate acceptance therefore uses `minimum_passed`
rather than treating `task_success_rate` alone as the pass oracle.

As of 2026-09-04, the release registry deliberately marks all five
Realtime-CU behavioral domains—aggregate, exact per-case, safety, deadline,
and latency—`unavailable`. A complete result can therefore be retained,
reviewed, and independently reopened, but it cannot pass behavioral
acceptance until the owners register the accepted `minimum_passed`, all
sixteen case minima, the authoritative zero-tolerance safety metrics, deadline
bounds, and latency median/tail limits plus any reasoned exclusions. Diagnostic
or historical observations are evidence for choosing those targets; the
runner must not silently convert them into acceptance thresholds.

The settlement/cancellation implementation checkpoint did not run a live or
paid Realtime-CU benchmark. It now provides a runnable graph-native
implementation checkpoint, but not a frozen benchmark candidate:
the profile binds an independently owned reference disposition policy and
shared retained-media resolver; the locked graph routes temporal admission
through `policy.IntentSettlement` and explicit
`policy.IntentDispositionRetry` without an activation bypass; and one exact
session coordinator routes cancellation through retry quiescence before
requiring settlement-gate, producer, activation, model, canonical model-result,
and configured action-stage acknowledgements.
Focused and stable-endpoint checks establish implementation behavior only. They
produce no benchmark row and do not show whether the selected live policy
correctly distinguishes continuation from success on the authored tasks.

The subsequent production-mounted cancellation regression found and repaired
one real shared-trajectory race before any new live campaign: an unrelated
same-run canonical-call commit could cause `ModelResultCommit` to emit its
normal empty `ignored/unknown_commit_reply` diagnostic while session
cancellation was active, and coordinator revision 2 misclassified that fanout
as failure of the awaited model-result commit. Revision 3 ignores only that
exact authority-free diagnostic shape and continues to require the canonical
model-result acknowledgement. The unchanged WebSocket endpoint now exercises
in-flight disposition, active-model, and crossed-client-action cancellation,
including fresh-intent recovery after each. A separate production composition
executes focus→type→submit through
`indeterminate → automatic retry → continue → continue → succeeded`,
then verifies that the terminal latch resists changing visual cadence and
reopens only for a new intent. The retry is a typed graph node with bounded
deterministic backoff, exact probe replay, control quiescence, typed exhaustion,
and inspectable state/outcomes; it is not a hidden provider timer. These remain
local behavioral regressions, not benchmark rows.

A later locked-profile regression exposed and repaired a separate behavioral
stall before a live campaign: activation had treated the exact visual
consequence of a canonical `ToolResult.Error` as consumed terminal evidence,
leaving the still-achievable durable intent waiting for an unrelated future
frame. Revision-13 activation now clears only that failed old effect and uses
its exact linked frame to open one recovery turn. The mounted case proves that
the error never reaches success-disposition policy, the recovery effect can
settle successfully, both result/consequence links remain canonical, and later
changing frames remain quiescent. Cancellation on both sides of that failed
consequence, bounded cleanup of canceled known-call retention, and benchmark-
scorer interpretation remain open. This repair and its focused checks also
produce no benchmark row. No live or paid Realtime-CU benchmark ran at this
checkpoint.

The retained live evidence must also be read by checkpoint rather than reduced
to one headline number. Candidate-05's current settlement-aware exact-sixteen
artifact reports 8/16. The later clean `b535b15` campaign executed and was
reviewed 16/16 and was scored 14/16 by its then-current evaluator. Concrete
defects—not either headline—drive the remaining work: camera actions lacked
fresh hazard evidence; moving-target and transient cases continued after the
page effect; repeated invalid actions exhausted authority; and sessions ran to
the evaluation horizon. The observer-repair artifact attempted only 4/16 and
still contains large post-success invalid-action loops.

Focused unit, exact-media, lifecycle, bounded-state, event-reordering,
acknowledgement-retry, schema/catalog, strict-profile, race, and affected-package
checks are implementation evidence only. Aggregate, exact-per-case, safety,
deadline, and latency acceptance remain unavailable; the focused
two-camera/two-moving-target/two-transient campaign and repaired exact-sixteen
campaign remain open; and the final-candidate ledger remains **0/7,486**.

The next benchmark run is intentionally gated on behavior, not on producing a
new headline number. First complete the three remaining shipped-profile
subgates: forged cross-node evidence, duplicate/reordered terminal decisions,
and cancellation/capacity/scorer behavior around failed effects. Ordinary
failed-effect recovery is already production-mounted; the broader gate remains
open. Then register all five Realtime-CU
acceptance domains and freeze the exact candidate. Run the two camera, two
moving-target, and two transient-alert variants as a focused repair set. Every
failure must be reopened from retained evidence, attributed to code,
configuration, policy, provider, or evaluator behavior, and repaired before
the affected focused case is repeated. Only after that loop meets the
registered constraints should all sixteen cases run and be independently
reopened from one newly frozen candidate. A behavior-affecting repair
invalidates affected candidate evidence; it is never hidden by averaging more
attempts into an aggregate.

The following is a status mirror of the master implementation tracker, not a
second acceptance source of truth:

| Required cell | Required attempts | Retained diagnostic evidence | Final-candidate credit |
| --- | ---: | --- | ---: |
| Interaction scenarios | 165 | Latest sealed 11-case diagnostic passed 8/11 | 0/165 |
| Meeting Assistant | 4 | Historical graph-native campaign passed 4/4 and was independently reopened | 0/4 |
| Realtime-CU | 16 | Candidate-05 reports 8/16; later clean `b535b15` was scored 14/16 by its then-current evaluator | 0/16 |
| FDB v1.5 | 498 | Historical diagnostic passed 355/498 and exposed severe interruption-latency failure | 0/498 |
| FDB v3 | 100 | Historical 9/100 is acceptance-invalid because the scorer admitted extra effects | 0/100 |
| FD-Bench | 6,147 | 1,546 completions and one interrupted attempt are retained; the population is incomplete | 0/6,147 |
| tau-Voice control | 278 | Older nonreportable diagnostic passed 160/278 | 0/278 |
| tau-Voice regular | 278 | No graph-native campaign has started | 0/278 |
| **Total** | **7,486** | Earlier checkpoints remain diagnostic only | **0/7,486** |

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

## OpenRealtime Meeting Assistant v1

This four-case suite puts the voice, vision, action, and slow-cognition paths
on one deterministic wall clock. The inputs are checked-in 24 kHz PCM
recordings and a changing browser surface; `testdata/fixtures.json` records the
exact transcript, cue intervals, audio hashes, format, and synthesis
provenance. The model receives pixels and audio, never evaluator state or a
visual narration substituted for the screen.

| Case | Required evidence |
| --- | --- |
| `open-share-present` | open the launch review, share it, call `meeting.read_launch_review`, and speak the grounded `18.4%` value |
| `follow-up-during-analysis` | navigate to risks in response to a later spoken request while the deliberately slow analysis tool is still running |
| `visual-alert-during-presentation` | acknowledge a visual-only deployment alert within 1.4 seconds while agent audio exists before and after the action |
| `spoken-navigation-correction` | first navigate to summary, then reverse to overview within two seconds of the correction |

Tool handlers run concurrently in this suite. A long document-analysis call
must not stop collection of audio, video, protocol output, or subsequent tool
calls; quiet detection also waits for outstanding work. This is a runtime
requirement, not benchmark convenience: serial tool handling would make the
second case impossible by construction.

```sh
openrealtime bench meeting -list
openrealtime bench meeting \
  -foreground cascade \
  -endpoint ws://127.0.0.1:18786/v1/realtime \
  -fps 5 -analysis-delay 8s \
  -out results/meeting-cascade.json
```

`-foreground omni` selects the registered Omni cell. Both full runs use the
same recordings, frame cadence, deadlines, slow Gemini configuration, policy
recognizer, action boundary, and scorer. The Omni cell changes the binding and
foreground model/speech topology together, however, so the pair is an
end-to-end **system treatment**, not a one-factor proof that Omni or cascade is
intrinsically better. Report the task-level traces and latency distributions;
do not turn a partial, filtered, dirty-tree, or unavailable-provider run into a
ranking.

The cascade four-case cell is the required Meeting Assistant result in the
7,486-attempt behavioral acceptance matrix. The Omni cell is independent,
opt-in architecture/provider-quality validation; when selected it still must
run all four cases, but it does not enter behavioral acceptance or
`release_complete`.

The checked-in launcher passes `-reference-levels` derived from its active
`MEETING_ASR_*`, `MEETING_POLICY_*`, `MEETING_SLOW_*`, and foreground-model
configuration. This is part of measurement validity: a diagnostic run using
Whisper and a local slow Qwen model must not retain the default F12
SenseVoice/F6 Gemini labels. Direct CLI runs with non-default services must
provide the same explicit level overrides.

Five frames per second is an observation opportunity, not a promise of 200 ms
cue-to-effect latency. The measured interval includes frame capture and
admission, model queueing/prefill/decoding, tool dispatch, and browser effect.
The suite therefore reports end-to-end latency distributions. Adaptive
observation may collapse unchanged frames while retaining the newest pixels;
it is preserved in both treatments and no visual narration is placed on the
critical action path.

For the cascade cell, 200 ms is also a control opportunity rather than an
action-sequence length. The visual actor emits one bounded chunk, waits for a
fresh post-effect frame, and replans from the newest ASR/task state. Pending
visual work is latest-wins; the audible endpoint remains a separate one-second
commitment clock. An action before `cue.End` is therefore valid when the spoken
destination was already unambiguous, and latency remains reported relative to
the endpoint (possibly negative) rather than erasing that action.

Composite meeting requests are evaluated as concurrent obligations. A silent
visual `WAIT` or action cannot consume pending speech, and presentation audio
cannot block an autonomous visual action. Slow semantic tools remain
asynchronous and in-flight calls are exposed to the visual actor; an identical
unresolved call is returned as `already_in_flight` rather than launched again.
For grounded screen effects, `computer.click_normalized` declares x/y in the
0–1000 VLM coordinate space and the dispatcher maps them to target pixels.
The controller additionally requires a private pixel-grounded label, validates
it against the next unfulfilled explicit action rather than the whole request,
and removes it before publishing the tool call. Rejected committed calls are
closed with typed placeholders so a later corrected grounding is not suppressed
as already in flight.

`-categories` and `-limit` are smoke-test filters. As with the other owned
suites, the result still declares all four expected tasks and is incomplete.
The scorer retains page events, computer actions, knowledge-tool intervals,
and recognized user turns so a miss can be assigned to ASR, interaction,
vision/grounding, action execution, slow-work concurrency, or speech output.

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
