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

The retained twelve-case Deepgram/Qwen/Gemini checkpoint and the focused FDB
sub-turn comparison are documented in [Sub-turn interaction benchmark study](subturn-benchmark-study.md).
That note separates deterministic scores from advisory Gemini observations and
records why the policy is not applied wholesale to every suite.

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
  "tasks": [ { "id": "user_interruption/1", "completed": true, "passed": true, "applicability": "applicable",
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

For focused scenario diagnosis, `openrealtime scenario -list` prints canonical
case names without opening a profile or contacting services. Pass the same
repeated `-case` flags to `openrealtime profile scenario` and
`openrealtime scenario`, for example
`-case 'an acknowledgement is not an interruption'`. Profile creation freezes
that exact case contract; execution rejects a different selection before
reading credentials or creating review files. Omitting `-case` selects all
twelve cases. Subsets retain per-attempt audio, results, submitted visual media,
and source receipts, and can use the ordinary independent review commands.
They remain diagnostic: their checklist has `full_suite: false` and cannot
become reportable even with fifteen passing repetitions. See the
[scenario launch contract](../bench/scenario/graphnative/README.md) for the
profile and evidence workflow.

**Interaction scenarios** score timed speech, silence, tool outcomes, and
content against the authored script. New deterministic results record
`scorer_version: 4`. Content checks match whole words and numbers, ignoring
case and repeated whitespace: `none` cannot satisfy `one`, `undone` cannot
satisfy `done`, and `30` cannot satisfy `3`. Empty or unknown checks, missing
timeline anchors or menu evidence, and invalid time windows fail explicitly.
The translation case separately requires the greeting and introduction, then
the meeting, day, time, afternoon, and office. Mentioning one appointment
keyword no longer passes the whole translation.

The acknowledgement scenario now requires acoustic continuation across both
“Mhm” and “Right, yeah.” Each `held-across` check uses the captured agent
channel on the serialized playout clock, with one second before and after
the actual synthesized line. It requires more than 120 ms of activity before
and after, and more than the smaller of 120 ms or half the line's duration
during the acknowledgement. Interior pauses may not exceed 500 ms. Activity
uses 20 ms frames with RMS at least 128 on PCM16 (about -48 dBFS), so emitted
silence and low-level dither do not earn speech credit. Missing or malformed
capture is explicitly unverified. Arrival timestamps, text, and the room
channel cannot substitute for the recorded agent audio.

Version 4 supplies a fictional five-step refund policy as scenario-owned
session instructions. The answer must mention the order number, return label,
and original payment method as well as meet both acoustic holds. This avoids
asking for a long explanation of facts the model was never given. Content
checks establish only those explicit details, not complete semantic fidelity
to every policy clause. The script's spoken words and cue times are unchanged.

New transcript moments preserve `response_id`, `playout_at_ms` on audio deltas,
and `response_status`/`response_status_reason` on terminal events. Each hold
joins responses by their recorded audio playout window, then retains their
terminal status and event time in `responses`. A prefetched response can finish
on the wire before the listener hears its audio end. A status of `completed`
does not establish that the explanation was complete; `cancelled` does not
prove that this backchannel caused the stop. Missing, unknown, or duplicated
terminal evidence is explicit and never borrowed from another response.
Response diagnostics do not change the acoustic thresholds or turn missing
continuation into a pass.

Results retain each check's windows, activity durations, and longest pause in
`holds`; the media-linked review renders the same measurements. `Play` supplies
the capture automatically, and `ScoreWithAudio` can examine retained PCM.
Transcript-only `Score` cannot verify this check. This tests acoustic
continuation, not whether arbitrary audible content continues the same
explanation; the content checks and independent media review still matter.

These are deterministic content requirements, not a general semantic judge.
Negation, contradictory statements, invented dialogue, and audible quality
still require the separately retained media review and further scorer work.
The twelve-case wire contract and 180-attempt release population are unchanged.
Historical unversioned, version-2, and version-3 results retain their original labels and
receipts; a passing historical recording does not establish a pass under
version 4. In particular, the retained v28 acknowledgement recording has no
agent activity after its second backchannel and does not meet the new check;
see the [separately attributed waveform audit](subturn-benchmark-study.md#acknowledgement-waveform-audit).

**FDB v1.5** has four categories: yield to an interruption, hold through a backchannel, background speech,
and speech addressed to somebody else. A system that scores well by always
yielding is not a system that handles overlap, which is why the report breaks
the four out rather than averaging them.

A recording where the agent was not speaking when the event arrived is reported
as **not applicable** rather than as a pass or a failure. It says something
about latency and nothing about overlap, and folding it in either direction
would corrupt both readings.

New FDB outcomes declare `applicability: applicable` or `not_applicable`.
Inapplicable recordings still count as completed and retain their latency
measurements and media reviews, but never count as passes. The shared summary
reports `not_applicable` separately and divides passes by
`completed - not_applicable`. A zero denominator has no pass rate; the stored
numeric zero is a placeholder, and the CLI renders the rate as unavailable.
Comparisons refuse zero denominators and different applicable case populations,
even when their sizes happen to match.

Historical artifacts have no typed applicability field. Their JSON summaries,
review labels, and sealed receipts keep their original interpretation so they
can still be verified. The category breakdown can read their older
`notes.applicable` field, but a historical nominal score cannot serve as a
current quality comparison or final candidate. The retained complete
`fdb-candidate-full498-20260831-04-full-reviewed.json` contains 498 completions,
68 inapplicable recordings, and **287/430 applicable passes**. Its original
355/498 nominal passes included those 68 recordings; interruption is 15/156
applicable passes, versus the original nominal 59/200. This is a correction to
the interpretation of retained evidence, not a new benchmark run.

The source verifier also recognizes the historical runtime-status encoding
that retained empty observer and capability fields. The change to sparse live
status had otherwise made those older canonical completions unreadable.
Compatibility is confined to source completions and review contexts, after
strict typed decoding; the verifier still requires the original file hashes
and an exact current or historical encoding. The 498 source recordings and
498 advisory evaluations were reopened on 2026-09-05 with the repaired
verifier, against aggregate receipt
`sha256:a375c4a48ee694ea11e77c6874f6e2f42397b024a6949b71a9fcf34d9bc77d3b`.

The checked acceptance registry translates that exact history to a 287-pass,
430-applicable floor. Every one of the 430 previously applicable cases must
remain applicable, including all 143 failures, and every applicable historical
pass must keep passing. This prevents improving the pass rate by staying
silent on difficult cases. The 68 historically inapplicable cases must still
complete and retain evidence. A fresh full candidate with explicit
applicability is required for acceptance.

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
The episode boundary discards ordinary setup chatter but preserves every
already-collected `MomentError`, rebased to zero on the
episode clock. This keeps the structured `MomentError` evidence consistent with
the retained failure string even when the error races session setup; it does
not turn the unavailable session into a scored attempt.

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

As of 2026-09-05, the release registry carries registered Realtime-CU
targets in all five domains, derived from the retained complete candidate-05
campaign and cited to it by artifact, revision, and executable digest: an
aggregate floor of 8 of 16, a per-case table in which each case that passed
must keep passing, zero-tolerance premature-action and grounding-error rules
on every case, deadline-miss and session-timeout ceilings at the campaign's
counts, and median/tail bounds on cue-to-action, cue-to-observation, and
frame-to-observation latency with the remaining timing metrics excluded by
reason. These are non-regression floors recording where the runtime is, not
where it should be; the owner may tighten them and may not lower them. The
runner never converts a result into a threshold: the registry is hand-written
from a historical run, and the candidate under test is only ever compared
against it.

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
frame. Revision-13 activation clears only that failed old effect and uses
its exact linked frame to open one recovery turn. The mounted case proves that
the error never reaches success-disposition policy, the recovery effect can
settle successfully, both result/consequence links remain canonical, and later
changing frames remain quiescent.

A subsequent connected-boundary regression then reproduced the adjacent
cancellation leak: the settlement tombstone swallowed an error consequence
before activation could retire its `canceledEffects` record, so repetition could
consume `CancelMemory`. Final audit found the same retention window when a
successful result was committed before cancellation but its direct consequence
arrived afterward. Settlement revision 2 now independently authenticates the
exact canceled intent→call→result→direct-consequence chain and emits a typed
cleanup control for either result status on a dedicated lossless
`settlement.cleanup → activation.effect_cleanup` lane. Activation revision 15
can use that control only for exact local bookkeeping: it never routes through
ordinary admission, disposition, or cognition. The result consequence is
verified against its immutable historical trajectory prefix, while the
cancellation's user authority is independently verified against the full
current snapshot; graph-edge authority supplies the cleanup control's source
provenance. Activation additionally requires its mounted session, local durable-
intent tombstone, and retained generation to match. It retains an overtaking
cleanup in one bounded slot until the separately ordered activation
cancellation returns the exact generation ID, then retires only that generation
and emits `canceled_effect_failed` or `canceled_effect_succeeded`. The canceled
intent remains protected through retirement: no tombstone is removed while a
retained effect depends on it. Under cancellation-memory pressure, activation
may reclaim only a complete effect/tombstone pair that newer final user
authority has already superseded, and it never evicts an effect that still owes
a terminal settlement acknowledgement.

The connected outcome copy remains deliberately broader than the cancellation
transaction itself: every settlement outcome is observable, but only `cancel`
and `ack` outcomes can advance a cancellation transaction. Coordinator revision
4 recognizes a valid `cleanup/evidence` outcome as a closed, nontransactional
observation and emits no cancellation state, progress, or refusal for it; a
cleanup kind on any other operation is rejected. A mounted processing-barrier
regression proves the coordinator consumed the valid cleanup without producing
the former spurious `invalid_settlement_outcome` refusal, rather than inferring
absence from a timeout or letting an activation-only debug helper discard it.

The same ordering audit found that fresh work could already have selected a
newer durable intent and reached activation while the old generation was still
waiting for its delayed cancellation. Activation retained that fresh visual
admission, but previously cleared the old generation without replaying it, so
the new task needed an unrelated later frame to make progress. Revision 15 now
detaches an already-admitted different-intent value, publishes the old
cancellation and any overtaking cleanup first, and then re-enters the complete
admission path. The retained value is therefore revalidated against current
state and opens exactly one new generation without another observation; a
same-intent deferred value remains canceled.

The locked profile covers cancel-before-failed-consequence,
cancel-after-success-result-before-consequence, recovery-before-cancel, fresh
replacement intent, and cadence quiescence. A deterministic production-mounted
race also holds the coordinator's activation-cancel output, lets cleanup reach
activation first, and proves the same behavior for both result statuses before
releasing cancellation. Its serial processing barrier also proves that valid
cleanup is nontransactional at the connected coordinator boundary and cannot
emit a cancellation refusal. Element-level adversarial tests add malformed and
cross-session control refusal, exact identity/lineage/status checks,
duplicate/conflicting/reordered delivery, terminal-versus-cleanup ordering,
ordinary and cleanup-first replay of an already-admitted newer intent, and
one-slot capacity reuse without evicting an effect that still owes a terminal
acknowledgement. These are implementation regressions, not benchmark rows;
scorer interpretation and the authored live failed-effect case remain open. No
live or paid Realtime-CU benchmark ran at this checkpoint.

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
deadline, and latency floors are now registered from candidate-05, but no
repaired candidate has passed them; the focused
two-camera/two-moving-target/two-transient campaign and repaired exact-sixteen
campaign remain open; and the final-candidate ledger remains **0/7,501**.

The next benchmark run is intentionally gated on behavior, not on producing a
new headline number. First complete the three remaining shipped-profile
subgates: forged cross-node evidence, duplicate/reordered terminal decisions,
and scorer/live acceptance for canonical failed-result lineage. Ordinary
failed-effect recovery and the canceled-result cleanup orderings, including
bounded cleanup, are already production-mounted; the broader gate remains open.
Then freeze the exact candidate against the registered Realtime-CU
acceptance domains. Run the two camera, two
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
| Interaction scenarios | 180 | Twelve cases × 15 since the 2026-09-04 promotion; the retained 12×1 checkpoint passed 12/12, and the earlier sealed 11-case diagnostic passed 8/11 | 0/180 |
| Meeting Assistant | 4 | Historical graph-native campaign passed 4/4 and was independently reopened | 0/4 |
| Realtime-CU | 16 | Candidate-05 reports 8/16; later clean `b535b15` was scored 14/16 by its then-current evaluator | 0/16 |
| FDB v1.5 | 498 | Historical diagnostic completed 498; 287/430 applicable passes, 68 not applicable (original nominal score 355/498); interruption 15/156 applicable | 0/498 |
| FDB v3 | 100 | Historical 9/100 is acceptance-invalid because the scorer admitted extra effects | 0/100 |
| FD-Bench | 6,147 | 1,546 completions and one interrupted attempt are retained; the population is incomplete | 0/6,147 |
| tau-Voice control | 278 | Older nonreportable diagnostic passed 160/278 | 0/278 |
| tau-Voice regular | 278 | No graph-native campaign has started | 0/278 |
| **Total** | **7,501** | Earlier checkpoints remain diagnostic only | **0/7,501** |

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
recordings and a changing browser surface; `bench/meeting/testdata/fixtures.json` records the
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
7,501-attempt behavioral acceptance matrix. The Omni cell is independent,
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
