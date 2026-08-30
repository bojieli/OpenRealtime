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
committed user observation
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

After `action.ToolResultCommit` publishes a canonical result, the adapter
notifies the observer through `Observer.Consequence`. The next screen
observation causally names both the durable canonical user task and that exact
canonical result item. The newest screen remains the context tail, while the
user item remains the proposal-authority basis independently rechecked by the
shared `authority.ProposalAdmission` element. This makes an unchanged screen
meaningful feedback too: an observer can force one post-effect sample and then
return to its normal sparse cadence. Camera observations inherit the user task
but never the screen-effect result parent. Ordinary camera/screen cadence is
still committed as context, but it cannot independently reactivate cognition;
only a new canonical user task or the first screen carrying a new canonical
tool result is an activation boundary.

Canonical results awaiting visual evidence are retained in a bounded FIFO;
they are never stored in a replaceable "latest result" slot. Each nonempty
screen observation consumes exactly the oldest result, while camera frames and
screen frames for which the observer emits no observation consume none. If a
client completes more effects than the bounded observer path can retain, the
session fails closed before another consequence notification instead of
discarding or reparenting causal evidence.

An addressed Realtime cancellation also crosses the graph. The adapter waits
for the durable-activation cancellation outcome before returning, so a frame
already in flight cannot reorder behind the cancellation and silently revive
the old intent. A later user observation may establish a new task normally.

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
        Height: 720,
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
            Height: 720,
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
drift, and incorrect post-effect causality. Each profile negative asserts that
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
