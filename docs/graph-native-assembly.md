# Prepare a graph deployment

Use graph preparation to validate a typed topology and its dependencies before
starting a session. This is an integration workflow: the included component
files do not by themselves provide a complete set of running model services.
For a first conversation, use the [quickstart](quickstart.md).

## Inputs and stages

| Input | What it selects |
| --- | --- |
| `.ortg` and lock | Topology and exact component descriptors |
| Values | Component configuration |
| Deployment | Implementation and service bindings |
| Optional channel overlay | Deployment-specific channel settings |
| Secret catalog | Secret references, not secret values |

`openrealtime graph preflight` selects the exact plan from the binary's catalog,
validates configuration and dependencies, and emits a sealed preparation. It
does not instantiate providers, open devices, or resolve secret values. Mounting
the selected application acquires runtime resources separately.

## Run a preflight check

For example, this audits the shipped acoustic component. Its two standard
configuration contracts resolve and validate; the command then exits at the
first missing deployment service rather than launching a partial
graph:

```sh
openrealtime graph preflight \
  -lock graphs/components/acoustic-endpoint/openrealtime.lock \
  -values graphs/components/acoustic-endpoint/agent.values.yaml \
  -deployment graphs/components/acoustic-endpoint/agent.deployment.yaml \
  -secrets graphs/components/acoustic-endpoint/agent.secrets.yaml \
  graphs/components/acoustic-endpoint/agent.ortg
```

## Inspect available implementations

The built-in catalog currently contributes 34 exact in-process factory
profiles and exact mount-owned identities for `runtime.clock`,
`runtime.sequence`, and `runtime.secrets`. It intentionally contributes no
credential-bearing provider, application registry, device, or remote service.
`Catalog.Select` removes unused broad-catalog entries; direct `Preflight`
rejects missing, excess, and duplicate graph-scoped contributions before any
resource acquisition.

The exact production catalog and unresolved plugin surfaces for this build are
emitted as JSON:

```sh
openrealtime graph inventory
```

## Supply required services

Required service plugins are:

- `action.confirmation.providers`
- `action.irreversibility.ledger`
- `action.ledger.registries`
- `action.target.registries`
- `action.tool.registries`
- `action.trajectory.store`
- `cognition.continuation.providers`
- `model.external.deployments`
- `model.external.payload-codec`
- `perception.asr.providers`
- `perception.visual.providers`
- `speech.playback.sinks`
- `speech.tts.providers`

Descriptor-declared optional services are `cognition.media.resolver`,
`perception.media.retainer`, `speech.playback.scheduler`, and
`state.trajectory.store`. They enter a plan only through repeatable
`-optional-dependency` selections; an unselected optional service cannot
appear later as ambient mount state.

## Configuration validation

The same inventory output lists all 27 standard config-schema references
provided by this binary. Each is a strict, self-contained Draft 2020-12 object
schema. Plan creation checks every representable field shape, required field,
enum, and independent bound. Resource-free preparation then invokes the exact
selected `Factory.ConfigValidator` before dependency sealing, secret-store
binding, or mount; this preserves cross-field contracts such as one byte limit
bounding another. A selected plugin descriptor whose schema is absent remains
in `unresolved_config_schemas` and fails the `RequireResolved` plan gate. No
permissive placeholder schema or empty provider registry is fabricated.

## Secrets and evidence

Every shipped component has separate `agent.deployment.yaml`,
`agent.secrets.yaml`, and `agent.evidence.yaml` artifacts. The shipped secret
catalogs contain references only (currently none), never values. The evidence
manifests contain `profiles: []`, explicitly making no empirical claim until a
profile is bound to an exact plan fingerprint, element, implementation,
hardware artifact, and load artifact.

## Playback history in Scenario Conversation

The scenario conversation adapter sends playback-state updates through
`playback_state_append` into the graph's trajectory store. The update records
what was heard, including the pending suffix after interruption. Its original
trajectory prefix supplies provenance while later observations may advance the
store. The adapter waits for the matching commit and published state before
releasing the turn. Consequently a following explicit response request can
sample played history without waiting for another observation. The policy
still decides whether that request should produce speech or remain silent.
