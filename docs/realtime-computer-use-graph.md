# Graph-native Realtime Computer Use

The production Realtime-CU checkpoint is the locked
`realtime_computer_use` graph in
`graphs/components/realtime-computer-use`. It is exposed to the generic server
through `graphs.RealtimeComputerUseApplicationRegistration` and a checked
graph-launch profile. The graph does not contain a model, ASR implementation,
vision implementation, browser driver, WebSocket server, or UI.

That separation is intentional. A host supplies one exact proposal-only,
silent continuation plugin, one exact audiovisual observer plugin, and one
screen-only `computeruse.Target`. `graph/launch` then seals those identities
with the topology, target-bound values, descriptor lock, deployment artifact,
adapter profile, and mount-scoped service artifacts before a session can
start. Application-profile resolution builds and checks the plan but does not
open the model or observer factories. Those remain per-session resources and
are first acquired by `SessionProvider.Start`.

## Stable protocol surface

Clients continue to use the OpenAI Realtime-compatible WebSocket endpoint:

```text
ws://127.0.0.1:8765/v1/realtime
```

The adapter accepts microphone PCM, independent `screen` and `camera` video
sources, user text, ordinary function-call outputs, response creation, and
addressed cancellation. Authorized `computer.*` calls are rendered as normal
Realtime function calls. A client returns the result as a normal
`function_call_output`; no computer-use-only transport or legacy
implementation selector is added.

Screen and camera have deliberately different authority. The observer must
label microphone transcripts as `user`, and both screen and camera text as
`observer`. The action target owns only `screen`. An observer that changes a
source or authority label, a client that changes a target-bound tool schema,
or a result that does not match an already emitted graph-authorized call is
rejected before it can cross the corresponding boundary.

## Effect and visual-feedback cycle

The external-effect path is:

```text
committed user observation + pre-intent observer/source cohort
  -> fresh post-intent causal observations
  -> temporal-evidence admission
  -> durable activation
  -> proposal + canonical provenance
  -> declaration -> confirmation -> target fence
  -> canonical call -> irreversibility ledger
  -> client call -> client result -> canonical result
  -> forced screen consequence observation
```

Only `action.Dispatch` has an external effect, and its dispatcher is the
per-session client rendezvous supplied by the adapter plugin. The adapter does
not render a call until `dispatch.committed` proves the graph has crossed that
boundary. It does not accept a client result before that emission.

The reference values configure `policy.TemporalEvidenceAdmission` as
`after_intent` with `observed_before_intent`. The policy freezes the bounded
observer/source cohort that was actually present before the newest final user
intent, then admits activation only when each exact pair has a newer causal
observation. A fresh screen observation therefore cannot stand in for a stale
camera observation. The typed admission carries the exact prefix, trigger,
durable intent, and qualifying observation identities; activation independently
revalidates those identities against the canonical trajectory. Developers can
replace this policy in another graph, including with its `immediate` mode,
without changing the model or action path; they must also set activation's
`expected_admission` to the same contract. This independent pin prevents a
typed-but-forged admission from downgrading the timing mode or substituting a
smaller explicit observer/source requirement.

Final user intent also has explicit source time. Audio/video observations keep
their capture timestamp; `binding.TextInput.OccurredNS` carries typed-input
time, and the protocol gateway stamps it in Unix nanoseconds at dispatch. A
direct binding caller that omits the optional field receives a commit-clock
fallback rather than creating untimed durable intent. Temporal admission can
therefore compare every qualifying observer sample with one positive intent
time without inferring timing from modality or arrival order.

After `action.ToolResultCommit` publishes a canonical result, the adapter
notifies the observer through `Observer.Consequence`. The next screen
observation causally names both the durable canonical user task and that exact
canonical result item. The newest screen remains the context tail, while the
user item remains the proposal-authority basis independently rechecked by the
shared `authority.ProposalAdmission` element. This makes an unchanged screen
meaningful feedback too: an observer can force one post-effect sample and then
return to its normal sparse cadence. Camera observations inherit the user task
but never the screen-effect result parent. Ordinary camera/screen cadence is
still committed as context. Once temporal admission has established a durable
intent, later changed observer evidence may reactivate cognition so a waiting
condition can be detected; the activation policy still serializes one unsettled
generation/effect and consumes each exact post-effect screen consequence.

Canonical results awaiting visual evidence are retained in a bounded FIFO;
they are never stored in a replaceable "latest result" slot. Each nonempty
screen observation consumes exactly the oldest result, while camera frames and
screen frames for which the observer emits no observation consume none. If a
client completes more effects than the bounded observer path can retain, the
session fails closed before another consequence notification instead of
discarding or reparenting causal evidence.

An addressed Realtime cancellation also crosses the graph. The adapter waits
for the durable-activation cancellation outcome before returning. Activation
records both the runtime sequence and canonical store-version revocation floors
in every admission mode, so an admission already in flight cannot reorder
behind cancellation and revive the old intent even when its runtime sequence is
absent or newer. A later timestamped user observation may establish a new task
normally.

## Application profile and plugin registry

The serializable application-owned part of the launch profile contains only
exact selections and the target:

```go
applicationConfig := realtimecu.ApplicationConfig{
    FormatVersion: realtimecu.ApplicationFormatVersion,
    Model: realtimecu.ApplicationModelSelection{
        Reference: modelReference,
        Artifact: modelArtifact,
        Descriptor: proposalOnlySilentDescriptor,
    },
    Observer: realtimecu.ApplicationObserverSelection{
        Reference: observerReference,
        Name: "audiovisual-observer",
        Artifact: observerArtifact,
        Sources: []string{"camera", "microphone", "screen"},
    },
    Target: computeruse.Target{
        Name: "benchmark-browser",
        Sources: []string{"screen"},
        Width: 1280,
        Height: 577,
    },
}
```

The host installs the matching opaque references and factories separately:

```go
registration, err := graphs.RealtimeComputerUseApplicationRegistration(
    realtimecu.ApplicationRegistrationConfig{
        ApplicationArtifact: applicationArtifact,
        ProviderArtifact: providerArtifact,
        RuntimeArtifact: runtimeArtifact,
        Models: []realtimecu.ModelFactoryRegistration{{
            ApplicationModelSelection: applicationConfig.Model,
            Factory: modelFactory,
        }},
        Observers: []realtimecu.ObserverFactoryRegistration{{
            ApplicationObserverSelection: applicationConfig.Observer,
            Factory: observerFactory,
        }},
    },
)
registry, err := launchprofile.NewRegistry([]launchprofile.Registration{
    registration,
})
bundle, err := server.NewProfileGraphBundle(ctx, server.ProfileGraphBundleConfig{
    Profile: checkedProfile,
    Applications: registry,
    GatewayArtifact: installedGatewayArtifact,
})
```

`checkedProfile.Application.Configuration` is the canonical JSON encoding of
`applicationConfig`; the same profile pins the application, session-provider,
adapter, graph plan, and server gateway artifacts. The host registry supplies
factories only after exact reference/artifact/descriptor/source matching. It
does not put credentials, device handles, or executable digests into the
application configuration, and profile resolution does not read the process
environment.

`graphs.RealtimeComputerUseLaunchConfig` remains the lower-level,
resource-free constructor used to author/check a plan identity:

```go
launchConfig, err := graphs.RealtimeComputerUseLaunchConfig(
    realtimecu.PluginConfig{
        RuntimeArtifact: runtimeArtifact,
        Model: realtimecu.ModelPlugin{
            Reference: modelReference,
            Artifact: modelArtifact,
            Descriptor: proposalOnlySilentDescriptor,
            Factory: modelFactory,
        },
        Observer: realtimecu.ObserverPlugin{
            Reference: observerReference,
            Name: "audiovisual-observer",
            Artifact: observerArtifact,
            Sources: []string{"camera", "microphone", "screen"},
            Factory: observerFactory,
        },
        Target: computeruse.Target{
            Name: "benchmark-browser",
            Sources: []string{"screen"},
            Width: 1280,
            Height: 577,
        },
    },
)
if err != nil {
    // Refuse startup; there is no legacy fallback.
}
```

The target width and height must match the client-visible browser viewport.
`RealtimeComputerUseArtifacts` injects all eleven standard `computer.*`
definitions with that exact screen enum and coordinate space into the values
artifact. A Realtime session may select a subset of those exact definitions,
as the pixel and set-of-mark harness conditions do, but it may not widen or
rewrite them.

A browser console, benchmark driver, or macOS app remains a composable client
of the same endpoint. Presentation code owns capture, display, confirmation
affordances, and client-side effect execution; none of those concerns are
built into the graph or the server adapter.

## Validation and the sixteen-case suite

Resource-free integration and fail-closed tests run without credentials:

```sh
go test ./graph/binding/realtimecu ./graphs -count=1
go test -race ./graph/binding/realtimecu ./graphs -count=1
go vet ./graph/binding/realtimecu ./graphs
openrealtime graph fmt -check graphs/components/realtime-computer-use/agent.ortg
openrealtime graph check \
  -descriptor graphs/components/realtime-computer-use/elements.json \
  -values graphs/components/realtime-computer-use/agent.values.yaml \
  -lock graphs/components/realtime-computer-use/openrealtime.lock \
  -deployment graphs/components/realtime-computer-use/agent.deployment.yaml \
  -secrets graphs/components/realtime-computer-use/agent.secrets.yaml \
  -profile computer-use -warnings-as-errors \
  graphs/components/realtime-computer-use/agent.ortg
```

The 2026-09-03 production checkpoint pins graph fingerprint
`sha256:7f7a2d7d93231236350b9eec8f774c6c1c74451a0be5eb447225e491bea6eb1c`
and plan fingerprint
`sha256:0384b009fb10d90742f31e7585de2a30bfb5706fc10f1e078f34dbe16704490d`.
Its activation descriptor/runtime/implementation are revision 9 with descriptor
digest
`sha256:3f110646c31efba255c5a3b6d089a95bb648f0584e1acffb269d119e4631baf0`;
the temporal-admission descriptor is revision 1 with digest
`sha256:91e7c9bdd498945efc09eb34f0e29679c3de752bbb98455d33fa672a437a8ee8`.
The dedicated activation values schema requires the independent admission
contract without widening the generic generation schema. Focused
normal/race/vet, strict graph, explicit-source and mode-forgery refusal,
all-mode cancellation/recovery, multi-source freshness, and stable WebSocket
endpoint gates pass, followed by repository-wide test and vet from a clean
detached worktree. These are implementation checks, not live model quality
evidence.

The WebSocket integration test launches the real locked graph through the
strict application registration, generic profile registry, and
`server.NewProfileGraphBundle`. It then sends microphone evidence, observes a
graph-authorized ordinary function call, returns the client result, waits for
its canonical commit, and sends forced screen feedback. The same original user
task authorizes a second call only after that screen is proven to descend from
both the user item and first result. Both results produce distinct canonical
visual-consequence checkpoints through the unchanged endpoint.

Separate negative tests cover unknown configuration fields, application/model/
observer artifact or descriptor drift, source and target widening, schema
drift, unknown tools, premature or mismatched results, camera/user-authority
drift, stale or wrong-source temporal evidence, cancellation/revocation, and
incorrect post-effect causality. Each profile negative asserts that
neither the model nor observer factory was acquired.

All eight authored task families under pixel and set-of-mark grounding select
the same suite-wide endpoint, for sixteen cases total:

```sh
openrealtime bench realtime-cu \
  -endpoint ws://127.0.0.1:8765/v1/realtime \
  -grounding pixel,set_of_mark -fps 3 \
  -out results/realtime-cu.json
```

`-categories`, `-grounding`, and `-limit` remain diagnostic filters. A
filtered run is incomplete and cannot establish suite performance. The
credential-free integration tests establish wiring and safety invariants, not
model quality or benchmark non-regression; publish performance only after the
full sixteen live cases finish against the exact candidate endpoint and their
result artifact passes the repository's release validation.

The latest retained live diagnostic predates this production wiring: it ran all
16 cases and scored 14/16, with both camera cases acting before fresh hazard
evidence and moving-target/transient cases exposing post-success continuation.
No live case has yet validated this temporal repair.

Production also does not yet have an authoritative typed fact that the durable
user intent itself has succeeded. A planner returning no proposal can mean
"wait for later evidence," and repetition admission proves only that one exact
effect already succeeded. Neither fact, elapsed quiet time, nor the evaluator's
private `PageResult` is a sound generic settlement oracle. In particular,
moving-target retries can use different coordinates and evade exact repetition
identity, while treating no-proposal as success would break asynchronous and
multi-step tasks.

The repair architecture keeps two graph-visible, replaceable elements:

```text
successful result-linked post-effect evidence
  -> disposition producer
  -> typed IntentDisposition
  -> deterministic IntentSettlement gate
  -> activation continuation or same-intent quiescence
```

The producer classifies a closed `continue | succeeded | failed |
indeterminate` disposition and binds it to the exact session, durable intent,
canonical prefix, successful result and invocation, result-linked observation,
producer identity/configuration, and measured decision time. An
application-authoritative producer is preferred when available. The reference
generic producer will instead use a separately locked narrow semantic/vision
decision over the exact retained post-effect frame. It may share the Qwen/VLM
deployment with continuation, but it must own a distinct semantic-decider
client, selection, digest, and lifecycle so its policy and latency remain
independently inspectable.

The producer-neutral half of that architecture now exists as the registered
`policy.IntentSettlement` element. It independently revalidates the typed
admission and exact canonical intent→call→successful-result→result-linked
observation chain, holds a bounded candidate consequence until the matching
disposition arrives, releases it for `continue`, and retains terminal state
until downstream activation acknowledges exact cleanup. Ordinary and
failed-effect visual evidence remains reactive. Invalid, stale, conflicting,
or indeterminate evidence fails closed without being mislabeled as success.

The terminal decision is not just a string label. Succeeded and failed
decisions embed the exact disposition; canceled decisions embed an exact
canonical durable-intent cancellation; reset decisions embed the exact reset;
and superseded decisions embed the newer canonical user-authority identity.
The independent verifier rejects a terminal kind without its one mutually
exclusive witness. The mounted trajectory service now also carries a trusted
session identity, so the first foreign evidence or cancellation cannot select
the session or poison cancellation memory.

Cancellation intentionally has the distinct type
`Interrupt<policy.IntentSettlementCancellation>`. Existing
`GenerationCancel` values address a generation or media stream and are
compile-time incompatible: copying an observation stream ID or a session ID
into this boundary would not prove which durable-intent epoch is being
revoked. The production reference therefore still needs a stateful
cancellation coordinator that resolves protocol/session authority to the
exact canonical intent and routes the appropriate downstream activation
cancellation as an explicit graph choice.

Independent ports do not acquire a hidden scheduler priority merely because a
descriptor classifies one as an interrupt. Settlement actor receipt is the
linearization point. If cancel is received first, a later continuation is
refused and exact cleanup awaits acknowledgement. If continuation is received
first, its one admitted envelope cannot be retracted; the later cancellation
tombstones subsequent same-intent evidence, and the production coordinator
must cancel the already released downstream activation. Concurrent messages
with no happens-before relationship may linearize either way. Lossless output
publication applies normal graph backpressure and is canceled by graph context,
not preempted by a later control waiting on another input port.

Current implementation ledger:

| Slice | Status | Remaining boundary |
| --- | --- | --- |
| Typed probe, disposition, exact reset/cancel, terminal decision, acknowledgement, state, and outcome contracts | Implemented and locally verified | Bind a real producer artifact/configuration in the production lock |
| Bounded deterministic state transition for a recorded actor order | Implemented and locally verified | No claim of priority between concurrent independent ports |
| Reference semantic/vision disposition producer | Open | Implement its separate client, lifecycle, exact-media resolution, and measured latency |
| Protocol/session cancellation translation | Open | Resolve exact canonical intent and explicitly coordinate already released activation |
| Activation settlement input and acknowledgement output | Open | Independently verify the decision, atomically clear the matching effect, and return exact terminal lineage |
| Realtime-CU graph, values, descriptors, lock, profile, and fingerprints | Open | Wire and freeze the complete reference subgraph without an admission bypass |
| Live behavioral validation | Open | Register thresholds, run the focused six variants, repair failures, then rerun all sixteen |

The implementation checkpoint is also hardened at its untrusted typed-input
boundary. It checks nested evidence, disposition, and acknowledgement shape
before cloning or canonical hashing; a failed clock/ID construction cannot
leave an empty intent record or a rejected replacement queued for later
execution. Refusal output projects only bounded canonical metadata, and state
plus refusal envelopes are pinned to the mounted trajectory session; a nested
foreign-session probe cannot be relabeled as mounted-session evidence. Probe
issuer and sequence are bound into the request identity and agree with the
published envelope. All generated lineages are deduplicated, capped, and
stripped of a pre-seeded self-parent. A continued admission receives a fresh
content-addressed identity over its immutable envelope and directly names the
held evidence, disposition, and probe while preserving the upstream source and
sequence used by activation cancellation floors. That identity is verified,
not nominally forbidden, on another settlement gate's evidence input, so
type-correct gate chaining remains executable; cross-type namespace reuse and
same-ID/changed-metadata reuse fail closed. Duplicate terminal dispositions do
not publish a second envelope under the existing terminal ID. These properties
are local implementation guarantees, not evidence that the still-unwired
Realtime-CU policy behaves correctly against a live model.

Terminal suppression requires an explicit lossless handshake with activation;
dropping only the post-effect observation would leave activation's active
generation/result state live. The target reference topology is therefore:

```ortg
temporal_evidence_admission.admitted -> intent_settlement.evidence;
intent_settlement.probe              -> intent_disposition.probe;
intent_disposition.disposition       -> intent_settlement.disposition;
intent_settlement.admitted           -> activation.admitted;
intent_settlement.terminal           -> activation.settlement;
activation.settlement_ack            -> intent_settlement.ack;
```

There is no admission bypass around the settlement gate. On `continue`, the
gate releases the original verified evidence through `admitted`. On a verified
terminal disposition, it suppresses that evidence and retains the exact
terminal latch until activation independently reopens the canonical prefix,
clears the matching generation/effect atomically, and returns the exact
acknowledgement. This is not a cognition cancellation: the cognition run may
already have ended before the result and post-effect observation exist, and a
broad generation cancellation does not prove which completed effect was
settled.

The remaining behavioral sequence is to implement the producer, translator,
and activation handshake; adversarially test their mounted composition; wire
and lock the reference subgraph; register machine-enforceable Realtime-CU
acceptance targets; and then run both camera, both moving-target, and both
transient-alert variants from one repaired immutable candidate. Any failure is
retained and repaired before all 16 cases are rerun from a newly frozen
candidate. No live benchmark was run for this generic checkpoint, and nothing
in its implementation checks advances the project-wide 0/7,486 final-candidate
attempt ledger.
