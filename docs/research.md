# Research and evaluation

OpenRealtime studies how an agent decides **when** to speak, listen, interrupt,
or act while its inputs continue to change. The repository combines a runtime,
scripted interaction scenarios, integrations with external benchmarks, and
records of measurements and debugging experiments.

The research record includes failed attempts and later corrections. A result
applies to its recorded code, configuration, data, and scoring method; an older
pass count is not a current performance guarantee.

## Questions and reading paths

The proposed [interaction capability study](interaction-capability-study.md)
separates temporal representation, acoustic evidence, policy learning, and
speech planning. Its [implementation and GPU plan](interaction-capability-plan.md)
distinguishes existing runnable smoke checks from the new experiments still to
build. It reports no new measurements.

| Question | Start here | Supporting detail |
| --- | --- | --- |
| What behavior is the project trying to demonstrate? | [12-scenario demo gallery](demos.md) | [Scenario definitions](../bench/scenario/scenarios.go) |
| How do timing decisions differ from response generation? | [Core concepts](concepts.md) | [Interaction findings](interaction-findings.md) |
| What does a listener actually wait for? | [Response latency](latency.md) | Waveform methodology and repeated measurements on that page |
| Where should an interrupted answer resume? | [The spoken boundary](spoken-boundary.md) | [Sub-turn study](subturn-benchmark-study.md) |
| How do policy placement and available evidence affect behavior? | [Architecture experiments](architecture-experiments.md) | [Architecture catalog](architecture.md#architecture-definitions-evolve-above-bindings) |
| How do I run or add an evaluation? | [Benchmark guide](benchmarks.md) | [Release validation](release-validation.md) for provisioned checks |
| How did the conclusions evolve? | [Measurement record](measurement.md) | Dated experiments, artifacts, and corrections |

## What to report

For a reproducible comparison, identify the source revision, model/provider
versions, deployment and hardware, dataset and scorer, number of attempts, and
the exact command. Report failures, missing outputs, and variance alongside
latency or success rates. Separate the time to a model decision from the time
to audible output.

A focused run can help reproduce a defect. A model ranking needs a suitable
comparison and enough repeated observations to support it. The benchmark guide
documents complete-campaign and artifact-verification tools for that purpose.

## Current implementation versus proposed design

[Architecture](architecture.md) describes the implementation and its active
launch paths. The [graph design](composable-agent-graph.md) and
[presentation design](composable-presentation.md) contain accepted design work
and implementation trackers, including unfinished work. Read their status and
unchecked items before citing a feature as implemented.

The [v1 product plan](openrealtime-v1-plan.md) and
[architecture decisions](adr/) preserve design history. They explain intent
and tradeoffs; they are not installation instructions.
