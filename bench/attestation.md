# Benchmark execution attestation

Benchmark scores describe behavior only when the executable composition that
produced each task is also known. Source and binary provenance remain in
`bench.Provenance`; execution attestation answers the separate question of
which graph, values, runtime elements, providers, and routes that binary used.

The schema is versioned by `AttestationFormatVersion` and has two deliberately
incompatible kinds:

- `graph-native` records the frozen Graph IR identity, the separate values
  artifact identity, every node's config reference and digest, exact live
  element/runtime/capability resolutions, and any selected edge paths.
- `legacy` records a compatibility binding and a digest of its live status. It
  remains useful historical evidence but cannot satisfy a graph-native cell.

An empty `Cell.Execution` is an unattested historical cell. This preserves old
artifacts; it does not promote them to graph-native evidence.

## Authoring a graph-native cell

Build the requirement from the exact bound Graph IR, the identity of its
separate values artifact, and the resolution the deployment is expected to
produce:

```go
requirement, err := bench.RequireGraph(bound.Graph, bench.ArtifactIdentity{
    ID:       "values://meeting-agent",
    Revision: "openrealtime.ai/config/v1alpha1",
    Digest:   bound.Fingerprint,
}, expectedResolution)
cell.Execution = requirement
```

Requirements have a strict, deterministic standalone JSON representation:

```go
err = bench.WriteExecutionRequirement("agent.execution.json", requirement)
requirement, err = bench.ReadExecutionRequirement("agent.execution.json")
```

`openrealtime bench architecture cell -execution agent.execution.json ...`
attaches that reviewed contract to the authored cell. Omitting `-execution`
retains the historical unattested-cell behavior; explicitly supplying `{}` is
rejected so a misspelled or empty artifact cannot look like graph attestation.
The requirement contains expected identities and required paths. It is not
runtime evidence and cannot substitute for the per-task proof below.

The repository CLI can author the requirement without embedding graph
construction in Go:

```text
openrealtime bench execution graph \
  -graph agent.ir.json \
  -values agent.values.yaml \
  -resolution agent.expected-resolution.json \
  -out agent.execution.json
```

An explicitly legacy baseline is authored separately. The binding is always
required; the three architecture fields are supplied together only when the
legacy server was launched from a versioned architecture catalog entry:

```text
openrealtime bench execution legacy \
  -binding cascade \
  -architecture-id cascade.external-policy \
  -architecture-revision 4 \
  -architecture-fingerprint sha256:<reviewed-digest> \
  -out baseline.execution.json
```

Omitting all three architecture flags records the older binding-only launch.
Supplying a partial or mutable architecture identity is rejected. This command
authors expected legacy identity; `LegacyStatusAttestor` must still capture
the independently negotiated live status for every task.

The Graph IR must already contain every node's configuration reference and
digest. The command re-binds the supplied values and requires the resulting
Graph IR fingerprint to be identical, then derives the configuration artifact
digest from those canonical values. `-configuration-id` and
`-configuration-revision` can name the deployed artifact; their defaults are
`values://<graph-id>` and `openrealtime.ai/config/v1alpha1`.

An expected-resolution artifact has `format_version: 1`, complete `elements`,
and optional `required_paths`. Each element names its exact descriptor,
implementation, mounted runtime artifact, and selected provider/adapter
capabilities. Revisions such as `latest`, `current`, `unknown`, template
expressions, and other live placeholders are rejected. The command also
rejects missing graph nodes, extra nodes, implementation drift, unknown path
edges, discontinuous paths, and values/Graph IR drift.

`expectedResolution.Elements` must cover every graph node. Model/service
capabilities carry provider and adapter identities independently, so changing
an adapter behind an unchanged model name is still a changed treatment.
Required paths may be included in `expectedResolution.Paths`; an observed task
may retain additional paths.

The session driver must receive a `GraphAttestor`:

```go
session.RuntimeAttestor = bench.GraphAttestor{
    Graph:         bound.Graph,
    Configuration: configurationIdentity,
    Resolve: func(ctx context.Context, request bench.AttestationRequest) (bench.LiveResolution, error) {
        return inspector.Resolution(ctx, request.Scope, request.Status.Graph)
    },
}
```

The driver automatically negotiates live session status. At task completion it
first verifies that status against the Graph IR fingerprint, then calls the
resolver. Completion timing matters: a resolver backed by runtime inspection
or traces can now report the mux branch, fork, or output route actually used by
the task rather than predicting it at session startup. Session-backed suites
also pass their task ID as `AttestationScope`, allowing an inspector to select
the right session when benchmark tasks run concurrently.

`binding.Status.Graph` alone is intentionally insufficient. It proves which
immutable IR was mounted, but it cannot invent dynamic provider revisions or
route selections that the status protocol does not expose. A graph-native
result with no attestor, incomplete node resolution, a mismatched fingerprint,
or missing required path is non-reportable.

## Result behavior

Execution evidence is attached to each `TaskOutcome`, not only to the cell. A
long evaluation may cross a deployment restart between two tasks. Shared
session-based suites copy evidence with `TaskOutcome.AttachExecution`; copies
do not alias the transcript or another row.

`Result.Reportable` validates every completed task against its cell contract.
It refuses missing, corrupt, wrong-kind, wrong-graph, wrong-config,
wrong-resolution, and required-path mismatches. `Finish` still computes scores
for diagnosis, and `Write` may retain a refused artifact; neither turns a
refusal into a score claim.

External drivers such as tau-Voice cannot reconstruct evidence from their
launch flags. They accept an independently captured, already validated
`ExecutionEvidence` and copy it into their rows. If a graph-native cell does
not provide that evidence, verification fails before the expensive suite is
started. Because one preflight artifact is shared by many tau tasks, it must
have an empty scope and no task-selected paths; claiming a preflight route for
every later conversation would be false precision. Deployments with a trace
store can instead provide tau-Voice's `TaskAttestor`, which resolves evidence
after each task and may retain selected paths when its scope equals that task
ID.

Use `LegacyStatusAttestor` only for a status with no mounted graph. It refuses a
graph-bearing status and produces evidence whose kind can never match a
graph-native requirement.
