# Graph-native production assembly

`openrealtime graph preflight` is the non-legacy startup gate. It reads the
locked topology, values, deployment, optional channel overlay, and optional
graph-scoped secret catalog; resolves them against the executable catalog
compiled into the binary; reduces that broad catalog to the exact immutable
plan; and emits the public sealed preparation. It does not mount factories,
resolve secret values, or route through `serve`/binding switches.

For example, this audits the shipped acoustic component. Its two standard
configuration contracts resolve and validate; the command then exits at the
first genuinely missing deployment service rather than launching a partial
graph:

```sh
openrealtime graph preflight \
  -lock graphs/components/acoustic-endpoint/openrealtime.lock \
  -values graphs/components/acoustic-endpoint/agent.values.yaml \
  -deployment graphs/components/acoustic-endpoint/agent.deployment.yaml \
  -secrets graphs/components/acoustic-endpoint/agent.secrets.yaml \
  graphs/components/acoustic-endpoint/agent.ortg
```

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

The same inventory output lists all 27 standard config-schema references
provided by this binary. Each is a strict, self-contained Draft 2020-12 object
schema. Plan creation checks every representable field shape, required field,
enum, and independent bound. Resource-free preparation then invokes the exact
selected `Factory.ConfigValidator` before dependency sealing, secret-store
binding, or mount; this preserves cross-field contracts such as one byte limit
bounding another. A selected plugin descriptor whose schema is absent remains
in `unresolved_config_schemas` and fails the `RequireResolved` plan gate. No
permissive placeholder schema or empty provider registry is fabricated.

Every shipped component has separate `agent.deployment.yaml`,
`agent.secrets.yaml`, and `agent.evidence.yaml` artifacts. The shipped secret
catalogs contain references only (currently none), never values. The evidence
manifests contain `profiles: []`, explicitly making no empirical claim until a
profile is bound to an exact plan fingerprint, element, implementation,
hardware artifact, and load artifact.

The scenario conversation adapter sends playback-state updates through
`playback_state_append` into the graph's trajectory store. The update records
what was heard, including the pending suffix after interruption. Its original
trajectory prefix supplies provenance while later observations may advance the
store. The adapter waits for the matching commit and published state before
releasing the turn. Consequently a following explicit response request can
sample played history without waiting for another observation. The policy
still decides whether that request should produce speech or remain silent.
