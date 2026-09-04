# Architecture experiments

> [!NOTE]
> **Evidence record.** This page preserves the controlled experiments behind
> the architecture catalog. For the released runtime and deployment model, read
> [Architecture](architecture.md). For commands and result interpretation,
> read [the benchmark harness](benchmarks.md).

F52 asks three narrow questions: where should interaction policy live, what
evidence should it see, and—when more than one selector is composed—which
arbitration rule gives the system one authoritative act? It does not define
four model species.

The project-level answer lives in the repository-owned architecture catalog,
not in a launch script or a benchmark label:

```sh
openrealtime architectures list
openrealtime architectures show omni.external-policy@4
openrealtime architectures validate
```

Every reference is exact. There is no mutable `latest`: changing a definition
creates a later revision, and forking one records `derived_from` lineage. The
definition fingerprint makes editing a revision in place detectable. Catalog
stage (`experimental`, `candidate`, `stable`, or `retired`) describes
operational maturity and is not a benchmark ranking.

| Level | Interaction selection | Evidence |
| --- | --- | --- |
| P, `predicates` | engine predicates | acoustic activity + silence clock |
| T, `text-policy` | external enumerated-act policy | transcript + independently selected timing/state/identity/vision channels |
| C, `composed-policy` | predicates + external policy under explicit arbitration | the policy's selected vector; predicate facts remain independently attested |
| N, `native` | foreground interaction head | native audio/latent model state |

The foreground may be a component stack, a speech-to-speech model, or a model with
concurrent I/O. Those are properties recorded elsewhere in the cell. The same
foreground may support both `native_interaction` and `interaction_acts`; T and
N then select different owners over the same available capability vector. C is
not inferred because two mechanisms happen to be installed: its controller
vector and arbitration rule are exact selected state.

## Why F52 is not one string

The generic benchmark report carries `F52=predicates`, `text-policy`,
`composed-policy`, or `native` so it can reuse completeness, provenance,
distributions, and pairing.
That level is not sufficient architecture evidence. Each cell embeds one
immutable catalog definition and adds the deployment/experiment identity in
`bench/architecture`:

- immutable foreground, perception, speech, interaction-policy, recognizer,
  speaker-identity, visual-narrator, and slow-model revisions, with adapter
  revisions recorded separately;
- the six-column ownership vector and complete stack capability vector;
- the full named policy report, observer selection, profile, and tool
  authority;
- policy instruction revision and decision timeout;
- broad evidence representation plus the exact selected evidence-capability
  vector, sidecar/in-process transport, protocol version, and
  direct/typed/translated act handoff;
- the exact selected-controller vector and a single-writer arbitration rule;
- the explicit suppression contract when an engine policy is selected over a
  foreground that retains native interaction;
- suite fixture revision and ordinary executable, source, and hardware
  provenance.

Every cell also declares `availability` as `runnable` or `unavailable`. An
unavailable desired cell remains in the experiment matrix with a machine-
readable reason. For example, Moshi-native and Qwen-external may both run while
no one foreground exposes both selections; the absent same-foreground T/N cell
must not silently disappear.

## Validation gates

The definition, authored cell, and live result are independently validated.

- Model-owned floor requires `native_floor`.
- Model-owned interaction requires `native_interaction` and native multimodal
  evidence.
- A text policy requires engine interaction ownership, an immutable policy and
  recognizer identity, an instruction revision, and a positive deadline.
- Every controller-attested definition selects predicates, text policy, native,
  and remote mechanisms independently. One selector uses `single` arbitration.
  The current composed controller selects exactly predicates plus text policy
  under `predicate-floor`: predicates retain endpoint and overlap decisions;
  the text policy owns semantic, visual, quiet, and silent-tool acts.
- Every current definition selects transcript, acoustic activity, silence
  clock, conversation state, tool state, speaker identity, addressing, visual
  description, direct visual input, and native model state independently. The
  live vector must be exactly equal; extra evidence is not harmless because it
  changes the treatment.
- Speaker-identity evidence requires an immutable embedder model pin and a
  live adapter identity. Speaker identity does not imply addressing: knowing
  whose voice it is does not establish who the utterance was directed to.
- An in-process text policy hands acts directly. A sidecar text policy requires
  protocol v2, typed acts, and `interaction_acts`; a translated `respond` or
  `interrupt` signal does not count as the same treatment.
- Selecting engine interaction over a native-interaction foreground additionally
  requires an explicit native-suppression contract. The live status must report
  the same contract and typed boundary.

After every `session.update`, the negotiated session debug stream emits
`binding.Status()`. The scenario driver retains that status per task. A result
is refused if any task lacks it, if its architecture ID/revision/fingerprint
differs, or if the live ownership, capabilities,
policies, evidence vector, controller vector, arbitration, recognizer or
speaker-identity revision, timeout, protocol, act handoff, foreground, slow
provider, or observer set differs from the intended cell.
Preset names such as `omni+text-policy` remain useful adapter evidence labels
and have no power to satisfy these checks by themselves.

## Running a cell

Launch an exact architecture while supplying the deployment-specific models
and endpoints. Structural flags are derived from the definition; explicitly
contradicting one is refused.

```sh
openrealtime serve \
  -architecture omni.external-policy@4 \
  -sidecar-address tcp:127.0.0.1:9000 \
  -policy-model interaction-policy-3b
```

Inspect it through a normal protocol session. Inspection validates the live
handshake against the same catalog and writes the status later retained for
every scenario task:

```sh
openrealtime bench architecture inspect \
  -endpoint ws://127.0.0.1:8765/v1/realtime \
  -out results/live-status.json
```

Inspection is an authoring aid, not a benchmark result. Immutable model and
instruction revisions still come from deployment pins; live status supplies
the resolved owners, capabilities, policy names, recognizer adapter revision,
and act boundary without guessing.

Author a cell from the three independent authorities. The definition supplies
structure, the inspected status supplies negotiated reality, and the pins file
supplies immutable model/adapter/instruction identities the protocol cannot
discover. None is allowed to stand in for another.

```sh
openrealtime bench architecture cell \
  -name qwen-T \
  -definition omni.external-policy@4 \
  -status results/qwen-T-status.json \
  -pins experiments/qwen-T-pins.json \
  -out experiments/qwen-T-cell.json
```

Assemble reviewed runnable and unavailable cells into one experiment. The
fixture revision is the exact suite data/code identity, not a prose label.

```sh
openrealtime bench architecture manifest \
  -name f52-qwen \
  -fixture-revision git-blob:012345... \
  -cell experiments/qwen-P-cell.json \
  -cell experiments/qwen-T-cell.json \
  -cell experiments/qwen-C-cell.json \
  -cell experiments/qwen-N-cell.json \
  -out experiments/f52-qwen.json
```

Select one runnable cell against its corresponding server and write the result:

```sh
openrealtime scenario \
  -architecture-manifest experiments/f52.json \
  -architecture-cell qwen-T \
  -repeat 5 \
  -record results/qwen-T.json
```

The artifact contains the generic `bench.Result`, raw scenario records, the
intended definition and deployment identity, and one observed live status per task. A filtered or
failed suite, a modified worktree, a missing runtime status, or a declaration/
observation mismatch remains inspectable but is not reportable.

Run another cell from the same manifest and compare the artifacts:

```sh
openrealtime bench architecture \
  -baseline results/qwen-T.json \
  -variant results/qwen-N.json \
  -out results/qwen-T-v-N.json
```

The comparison has two valid labels:

- `architecture-only`: an adjacent P/T, T/C, or T/N pair with the same manifest and
  fixture, foreground revision, capability vector, floor owner, perception,
  speech, speaker-identity component, slow provider, policies outside the
  replaced interaction-policy rows, observers, and tool authority;
- `system`: useful measured cells whose non-treatment identity differs, or a
  non-adjacent comparison such as P/N. The artifact lists the confounders.

Thus Qwen-T versus Moshi-N may be a reportable system comparison. It can never
be relabeled as proof that external or native interaction is better. A genuine
T/N architecture result needs one foreground that honestly supports both
native interaction selection and external typed-act control.

## What the present evidence supports

The repository's existing results and the architecture boundaries support
several different conclusions, not a single winner.

1. Cascade plus external interaction is a valuable baseline and fallback. It
   is modular and debuggable, but every transcript/component seam discards
   timing, speaker, prosodic, and acoustic information.
2. A speech-to-speech foreground plus a narrow external interaction policy is
   the strongest near-term *default hypothesis* for tool-oriented assistants:
   raw audio and speech stay on the foreground data plane while policy remains
   cheap, replaceable, typed, and auditable. It is not yet a measured universal
   winner.
3. An interaction-native Omni foreground, preferably a shared multimodal trunk
   with an explicit interaction head, has the highest ceiling for timing,
   prosody, overlap, and addressing because it can retain evidence a policy ASR
   removed. It trades away some modularity and independent policy iteration.
   This is still a hypothesis until a same-foreground T/N pair passes the gates
   above.
4. The component cascade remains useful where local replaceability,
   debuggability, and deterministic authority matter more than preserving
   acoustic latent state. It should not be collapsed into a supposedly lesser
   model species.
5. The present P/T diagnostic has complementary failures, which makes C the
   next controlled hypothesis: keep grounded endpoint/overlap mechanisms while
   retaining the learned policy for durable instructions and non-acoustic acts.
   C is not assumed superior; only a same-components T/C pair can establish the
   arbitration effect.

“Full model” should therefore mean a foreground with native perception,
generation, concurrency, floor, and interaction capabilities—not a process
that also absorbs asynchronous slow cognition, trajectory, tool authorization,
audit, and lifecycle. Those external boundaries supply independent work and
authority that a single foreground model cannot provide merely by being
larger.

## The current catalog's controlled path

The catalog carries two different progressions deliberately:

- `cascade.controlled@3` → `cascade.text-policy@3` isolates one predicate
  controller versus one full external controller, including endpoint and
  overlap selection, on the componentized foreground available locally today;
- `cascade.composed-policy@1` forks both and selects predicates plus the same
  text policy under exact `predicate-floor` arbitration. The enriched
  `cascade.text-policy-visual-speaker@2` and
  `cascade.composed-policy-visual-speaker@1` pair holds narrated vision and
  speaker identity fixed for the current T/C experiment;
- `cascade.text-policy-visual@1`,
  `cascade.text-policy-direct-visual@1`, and
  `cascade.text-policy-speaker@1` compose independently attested evidence
  channels without adding runtime species;
- `cascade.text-policy-visual-speaker@1` is their voice-and-vision composition:
  narrated state plus speaker identity, still without undeclared direct pixels
  or fabricated addressing;
- `cascade.text-policy-addressing@1` retains the desired addressing-aware
  branch explicitly, but startup refuses it until a real addressing producer
  exists. It must not acquire a fabricated runnable result merely to complete
  the matrix;
- `omni.external-predicates@4`, `omni.external-policy@4`, and
  `omni.native-policy@3` require the same hybrid foreground capability vector, permitting
  a genuine same-foreground P/T/N experiment when a sidecar exposes all three
  selections.

`omni.native-full@3` is a further floor-ownership revision rather than another
model species. It moves the floor into the foreground while leaving slow
cognition, trajectory, authorization, execution, audit, and lifecycle outside.

The earlier complete 11-scenario P/T diagnostic is deliberately not
architecture evidence. Its T process was launched with direct visual input
while the cell claimed only `transcript`; the old single source string could
not expose that confound. Current revisions make that launch a different
definition and refuse the mismatch during startup, cell authoring, and every
observed task. The diagnostic remains useful for finding failure modes and
supports no P-versus-T ranking.

The subsequent exact-evidence diagnostic completed the same eleven scenarios
once per cell: P passed 6/11 and T passed 8/11, with the same foreground,
recognizer, speech, slow cognition, speaker embedder, narrator, observers,
tools, authority, and fixture, and with twenty-two matching live attestations.
It remains non-reportable because the worktree was modified and each scenario
has one sample. Its failure shape is nevertheless instructive: T alone handled
durable counting, correction, requested silence, and narrated visual state; P
alone handled the recorded menu and waiter; both missed third-party speech.
Narration was sufficient without policy access to pixels, speaker identity was
insufficient without addressing, and the general policy introduced latency
and content errors where a narrow grounded mechanism was stronger. The next
clean run should repeat both exact cells and keep the addressing branch
unavailable until a real producer can attest it.

That diagnostic was exact about evidence but not yet exact about controller
composition. Its T status named an act-model floor and immediate predicate
barge-in, while the definition could not say whether that mix or a model-owned
overlap policy was selected. Current version-4 cells require a controller
vector and arbitration, so the historical result remains readable but cannot
be promoted into current architecture evidence.

The T/C experiment was authored through the production path. The live
`cascade.text-policy-visual-speaker@2` cell attests one text controller and uses
the act model for both floor and barge-in. The live
`cascade.composed-policy-visual-speaker@1` cell attests predicates plus the same
text controller under `predicate-floor`, retaining the shipped predicate floor
and immediate barge-in. Both statuses match one version-4 manifest and otherwise
share the exact models, evidence vector, producer identities, observers, tools,
authority, and fixture. Scoring was delayed until an unrelated five-repeat
audit released the shared model services; unrecorded compute contention was not
made part of the treatment.

The complete one-repeat diagnostic produced T 7/11 and C 6/11. It is not
reportable because the worktree was modified and one conversation per scenario
does not estimate a population. The useful result is the disagreement shape:

- C recovered the recorded-menu and waiter-ordering cases which T missed;
- T retained requested no-interruption, semantic correction-overlap, and the
  narrated visual completion case which C missed in this sample;
- both handled simultaneous translation, the silence-clock trigger,
  acknowledgement overlap, and the ordinary question;
- both failed durable counting and nearby third-party conversation.

This does not establish either controller arrangement as the default. It shows
that `predicate-floor` is a real treatment with complementary effects, and that
composition needs finer selected jurisdiction or an explicit veto contract
rather than the slogan “predicates for timing, model for semantics.” In
particular, a durable quiet policy may need to veto a predicate-created response
opportunity, while a grounded deadline-sensitive act may need a fast route that
does not await a general model decision. Addressing remains a separate missing
evidence capability; no arbitration rule can infer it from speaker identity.

The next catalog evolution should express these routes as selected capability
data and attest them live. Adding another supported combination must not require
an architecture-ID switch or another cascade/Omni/duplex binding species.
