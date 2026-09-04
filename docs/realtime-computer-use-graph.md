# Graph-native Realtime Computer Use

The production Realtime-CU checkpoint is the locked
`realtime_computer_use` graph in
`graphs/components/realtime-computer-use`. It is exposed to the generic server
through `graphs.RealtimeComputerUseApplicationRegistration` and a checked
graph-launch profile. The graph does not contain a model, ASR implementation,
vision implementation, browser driver, WebSocket server, or UI.

That separation is intentional. A host supplies one exact proposal-only,
silent continuation plugin, one exact post-effect semantic disposition policy,
one exact audiovisual observer plugin, and one screen-only
`computeruse.Target`. `graph/launch` then seals those identities with the
topology, target-bound values, descriptor lock, deployment artifact, adapter
profile, and mount-scoped service artifacts before a session can start.
Application-profile resolution builds and checks the plan but does not open
the model, policy, or observer factories. Those remain per-session resources
and are first acquired after `SessionProvider.Start` mounts the selected graph.

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
  -> intent settlement gate
  -> durable activation
  -> proposal + canonical provenance
  -> declaration -> confirmation -> target fence
  -> canonical call -> irreversibility ledger
  -> client call -> client result -> canonical result
  -> forced screen consequence observation
  -> disposition producer -> continue or terminal settlement
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

An addressed Realtime cancellation also crosses one typed `session_cancel`
boundary. The adapter first waits for every earlier accepted observation to
reach its exact canonical commit, then waits for the coordinator's terminal
outcome before returning. The coordinator resolves the newest exact durable
intent and requires both settlement-gate acknowledgement and actual
disposition-provider quiescence before it cancels activation. It then follows
the exact activation generation through the model, canonical model-result
batch, and every configured action stage. An `already_canceled` control response
is not model quiescence, an already-published model commit is retained for a
later cancellation, and a partial or forged commit batch cannot authorize
action cancellation. Activation retains the exact durable-intent tombstone, so
an in-flight admission cannot reorder behind cancellation and revive old work.
A later timestamped user observation may establish a new task normally.

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
    SettlementPolicy: realtimecu.ApplicationPolicySelection{
        Reference: settlementPolicyReference,
        Artifact: settlementPolicyArtifact,
        Descriptor: settlementPolicyDescriptor,
        Configuration: settlementPolicyConfiguration,
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
        Policies: []realtimecu.PolicyFactoryRegistration{{
            ApplicationPolicySelection: realtimecu.ApplicationPolicySelection{
                Reference: applicationConfig.SettlementPolicy.Reference,
                Artifact: applicationConfig.SettlementPolicy.Artifact,
            },
            DescribeConfiguration: settlementPolicyDescriptorForConfiguration,
            FactoryConfiguration: settlementPolicyFactoryForConfiguration,
            ReadinessConfiguration: settlementPolicyReadinessForConfiguration,
        }},
        Observers: []realtimecu.ObserverFactoryRegistration{{
            ApplicationObserverSelection: applicationConfig.Observer,
            ResourceFactory: observerFactory,
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

The three parameterized policy callbacks accept the selected raw configuration;
the descriptor callback must reproduce the exact descriptor pinned by
`applicationConfig`, and the readiness and factory callbacks remain unopened
until their documented lifecycle points. `checkedProfile.Application.Configuration`
is the canonical JSON encoding of `applicationConfig`; the same profile pins the application, session-provider,
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
        SettlementPolicy: realtimecu.PolicyPlugin{
            Reference: settlementPolicyReference,
            Artifact: settlementPolicyArtifact,
            Descriptor: settlementPolicyDescriptor,
            Factory: settlementPolicyFactory,
        },
        Observer: realtimecu.ObserverPlugin{
            Reference: observerReference,
            Name: "audiovisual-observer",
            Artifact: observerArtifact,
            Sources: []string{"camera", "microphone", "screen"},
            ResourceFactory: observerFactory,
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

The 2026-09-04 no-bypass production-artifact checkpoint pins, for the checked
integration fixture in
`TestRealtimeComputerUseGraphLaunchesResourceFreeAndCommitsClientEffectFeedback`
(`benchmark-browser`, one `screen` source at 1280x720, and the test
model/policy/observer selections), graph fingerprint
`sha256:1b36fed81dbda59ed8298cfe2d632a094352314c4c497b2c78656de4af9a31f1`
and plan fingerprint
`sha256:9797a77a00cc7579c52e72d0edbc370bf8cc3b58994f833ecc0d10b82e63aea3`.
Its source, lock, and values digests are respectively
`sha256:18898fcd8ea58d85ef239fbdbcab0b858f71a356b714b11c47a044052c6ee3fa`,
`sha256:5a6539e8d7a835c1bf2c3e42303326f05f9b067afef72cb8996765964c8bf66c`,
and
`sha256:17725f341669604324ccbbedfa754041926f9b9fabda8cd4a5b3a5a0a92d9a93`.
The lock selects activation revision 12
(`sha256:c88c12b977dfc0c44bcda8317002fabe72f2d32a38c61418a81a092d053a9dce`),
disposition producer revision 2
(`sha256:6924671570fc86be0b90d5711cc93dd8f9fc311bbdf03e8e3b1a9b2622db0c3e`),
and cancellation coordinator revision 3
(`sha256:fc227d6bac1d353f099dc7555ff52ae87ad099360875f421b5a8a9f9eb9eb648`).
It also selects `action.ToolResultCommit` revision 4
(`sha256:9fc6057e1e47de87b14ae0ff7ec43df59d81240a4ae9ac93e8e9eca89094de95`).
The graph template passes canonical formatting and warning-free strict
`computer-use` validation. These identities and checks describe implementation
and configuration, not live model quality or benchmark non-regression.

The local `httptest` WebSocket integration launches the locked production
graph through the strict application registration, generic profile registry,
and `server.NewProfileGraphBundle`. It then sends microphone evidence, observes a
graph-authorized ordinary function call, returns the client result, waits for
its canonical commit, and sends forced screen feedback. The same original user
task authorizes a second call only after that screen is proven to descend from
both the user item and first result. Both results produce distinct canonical
visual-consequence checkpoints through the unchanged endpoint.

The same local endpoint now exercises three cancellation phases with scripted
model, observer, and semantic-decider implementations. First it holds the
production disposition element's cancellable client in flight after a
successful effect, cancels it, and proves that a new intent can activate only
after its own complete fresh screen/camera cohort. Second it cancels a model
call before any prepared result and
again proves replacement-intent recovery. Third it cancels after a client tool
call crossed the external boundary: the public request receives the honest
`incomplete/action_already_crossed` result, the mandatory cancellation result
still becomes canonical and requests visual consequence evidence, the old
intent remains quiescent, and another new intent remains usable.

That crossed-action sequence exposed a shared-trajectory ordering defect. A
canonical tool-call append for the same run is broadcast to every commit-aware
element; `ModelResultCommit` truthfully labels the unrelated reply
`ignored/unknown_commit_reply`. Coordinator revision 2 could mistake that
ambient diagnostic for failure of its awaited model-result transaction and
return `incomplete/model_result_not_committed`. Revision 3 recognizes only the
exact empty unknown-reply diagnostic as ambient, while any outcome that claims
a request, store boundary, item identity, or other result still goes through
the fail-closed canonical verifier.

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

Two retained live checkpoints answer different historical questions and must
not be conflated. Candidate-05's current settlement-aware artifact reports
8/16. The later clean `b535b15` campaign executed and was independently
reviewed 16/16, and its then-current evaluator scored 14/16. Both camera cases
acted before fresh hazard evidence; moving-target and transient tasks continued
after the page effect; some cases repeated invalid actions until authority was
exhausted; and several sessions ran through the evaluation horizon. The later
four-case observer-repair artifact is incomplete (4/16 attempted) and still
records large post-success invalid-action loops. No live case has validated the
settlement composition described below.

The locked production topology now classifies the durable user intent as
`continue`, `succeeded`, `failed`, or `indeterminate`. It does not infer that
fact from a planner returning no proposal, one repeated effect, elapsed quiet
time, or the evaluator's private `PageResult`: all of those remain unsound
generic settlement oracles.

The repair architecture keeps two graph-visible, replaceable elements:

```text
successful result-linked post-effect evidence
  -> disposition producer
  -> typed IntentDisposition
  -> deterministic IntentSettlement gate
  -> activation continuation or same-intent quiescence
```

The profile-bound `policy.IntentDispositionProducer` classifies a closed
`continue | succeeded | failed | indeterminate` disposition and binds it to the
exact session, durable intent, canonical prefix, successful result and
invocation, result-linked observation, producer identity/configuration, and
measured decision time. An
application-authoritative producer is preferred when available. The reference
generic implementation uses a separately owned narrow semantic/vision decision
over the exact retained post-effect frame, checks media and evidence bounds,
and fails only to explicit retryable `indeterminate`. It may share a deployment
with continuation, but the application selects its descriptor, artifact, and
non-secret configuration independently. Each session opens a fresh policy
client and gives the producer the observer's exact retained-media resolver;
configuration or descriptor drift fails before provider work begins.

The producer, producer-neutral `policy.IntentSettlement` gate, and activation's
revision-12 settlement consumer/acknowledgement boundary are now connected as
independently replaceable nodes. The gate independently revalidates the typed
admission and exact canonical intent→call→successful-result→result-linked
observation chain, holds a bounded candidate consequence until the matching
disposition arrives, releases it for `continue`, and retains terminal state
until downstream activation acknowledges exact cleanup. Ordinary and
failed-effect visual evidence remains reactive. Invalid, stale, conflicting,
or indeterminate evidence fails closed without being mislabeled as success.
Activation independently verifies a terminal decision, clears only the exact
effect without a new cognition turn, retains valid cross-lane reorderings, and
retries one immutable acknowledgement. The graph has exactly one activation
admission source—`settlement.admitted`—so there is no timing-policy bypass.

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
revoked. The stateful coordinator now performs that explicit translation and
preserves the full durable-intent identity when it addresses activation.

Action-stage acknowledgement is quiescence-sensitive. In particular,
`action.ToolResultCommit` immediately acknowledges cancellation only when its
ledger proves that no result is pending and the external boundary has not
crossed. If an authenticated result is pending—or the ledger already proves a
crossing while its result is still in transit—the element retains the first
exact cancellation without eviction, finishes the mandatory canonical result
safe point, and then reports a direct-parent terminal outcome with `Crossed`
set. Conflicting cancellation authority cannot rebind the retained result.

Session shutdown has a separate lifecycle barrier. `Audio` and `Video` protect
only active observer calls with the media lifecycle lock; observation
publication is atomically ordered against `Close`, and waiting for canonical
commit holds no lifecycle lock. `Close` can therefore wait for observer safety,
drain a registered commit waiter, and reject later media without deadlocking.

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
| Typed probe, disposition, exact reset/cancel, terminal decision, acknowledgement, state, and outcome contracts | Implemented, locked, and locally verified | Live quality remains unmeasured |
| Bounded deterministic state transition for a recorded actor order | Implemented and locally verified | No claim of priority between concurrent independent ports |
| Reference semantic/vision disposition producer | Profile-bound and locally verified | Exercise quality on focused live cases |
| Protocol/session cancellation translation | Implemented and adversarially verified through provider, model, idle-action, and crossed-action phases | Complete the remaining forged/reordered/failed-effect mounted cases |
| Activation settlement input and acknowledgement output | Connected; focus→type→submit, terminal cadence, and new-intent recovery are production-mounted and race-tested | Confirm behavior in the focused live cases |
| Indeterminate settlement retry | Open: the gate retains retryable evidence, but the shipped graph submits each probe only once | Add explicit bounded graph policy and exercise it through the production mount |
| Realtime-CU graph, values, descriptors, lock, profile, and fingerprints | Implementation artifacts are pinned with no admission bypass; strict check passes | They are not yet a frozen benchmark candidate and change if later behavioral repair changes code or configuration |
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
are implementation guarantees, not evidence that the wired Realtime-CU policy
behaves correctly against a live model.

Terminal suppression requires an explicit lossless handshake with activation;
dropping only the post-effect observation would leave activation's active
generation/result state live. The production reference topology contains:

```ortg
temporal_evidence_admission.admitted -> settlement.evidence;
settlement.probe -> settlement_producer.probe;
settlement_producer.disposition -> settlement.disposition;
settlement.admitted -> activation.admitted;
settlement.terminal -> activation.settlement;
activation.settlement_ack -> settlement.ack;

input session_cancel = cancellation_coordinator.request;
cancellation_coordinator.settlement_cancel -> settlement_cancel_copy.in;
settlement_cancel_copy.out -> settlement.cancel;
settlement_cancel_copy.out -> settlement_producer.cancel;
settlement_producer.outcome -> settlement_producer_outcome_copy.in;
settlement_producer_outcome_copy.out -> cancellation_coordinator.settlement_producer_outcome;
cancellation_coordinator.activation_cancel -> activation.cancel;
cancellation_coordinator.model_cancel -> model.cancel;
cancellation_coordinator.action_cancel -> action_cancel_copy.in;
```

This topology currently has no retry edge after an `indeterminate` producer
outcome. That absence is an open production behavior, not an implicit promise
that the producer retries internally. A retry design belongs in the graph as a
reusable policy element (or equivalent explicit composed control path) so its
trigger, delay/backoff, attempt/deadline bounds, cancellation, and terminal
outcome are visible to validation and inspection. It must replay the exact
immutable probe and lineage, never invoke the semantic client concurrently,
and stop on a verified terminal decision or exact cancellation/reset. Adding a
hidden timer to either `IntentSettlement` or `IntentDispositionProducer` would
erase a developer-visible interaction-policy choice and is therefore not the
reference design.

There is no admission bypass around the settlement gate. On `continue`, the
gate releases the original verified evidence through `admitted`. On a verified
terminal disposition, it
suppresses that evidence and retains the exact terminal latch until revision-12
activation independently reopens the canonical prefix, clears the matching
generation/effect atomically, and returns the exact acknowledgement. This is
not a cognition cancellation: the cognition run may already have ended before
the result and post-effect observation exist, and a broad generation
cancellation does not prove which completed effect was settled.

The remaining mounted matrix is now concentrated on forged cross-node
evidence, duplicate/reordered terminal decisions, explicit indeterminate retry,
and failed effects. After those cases, machine-enforceable Realtime-CU
acceptance targets must be registered. Then both camera, both moving-target,
and both transient-alert variants run from one repaired immutable candidate.
Each observed failure is retained and repaired before all 16 cases are rerun
from a newly frozen candidate. No live benchmark was run for this settlement/
cancellation checkpoint, and nothing in its implementation checks advances
the project-wide 0/7,486 final-candidate attempt ledger.

Here, the existing cadence regression means five sequential changing screen
frames after terminal settlement; it does not claim sustained live-stream
coverage. Likewise, existing mounted forgery checks cover temporal-admission
source/mode trust. Forged cross-node settlement evidence remains one of the
four open full-composition cases above.
