# Run benchmarks and investigate failures

The benchmark harness drives a running OpenRealtime server with timed audio,
protocol events, visual input, and tool results. It records the resulting
conversation and evaluates behavior such as interruption, endpointing, and
successful tool use.

Use a focused run to reproduce a failure. Use repeated, controlled runs when
making a comparison. Full campaigns and external model reviews are optional;
ordinary fixes still need the checks appropriate to the behavior they change.

<a id="suites"></a>

## Choose a suite

| Command | Use it to investigate | Required setup |
| --- | --- | --- |
| `scenario` | The project's 12 interaction scenarios | Matching scenario profile and model services |
| `bench fdb` | Yielding to interruptions and continuing through acknowledgements | FDB v1.5 data and a running voice server |
| `bench fdbv3` | Tool use with disfluent speech and spelled identifiers | FDB v3 data and its matching tool profile |
| `bench fdbench` | Endpointing and response timing | FD-Bench data and a running voice server |
| `bench tau-voice` | Voice tool use against an interactive environment | Prepared τ-Voice environment |
| `bench realtime-cu` | Realtime audio, video, and computer use | Matching graph, observer, model, and tool environment |
| `bench meeting` | Concurrent conversation and meeting work | [Meeting deployment](guides/meeting-assistant.md) |
| `bench dynacu` | Optional external dynamic computer-use validation | Prepared DynaCU-Bench environment |

For exact dataset paths, flags, metrics, and completeness requirements, consult
[the scoring reference](benchmark-reference.md). These commands do not install
model services or datasets automatically.

## Inspect the interaction scenarios

After building the CLI, list the authored cases without contacting providers:

```bash
./openrealtime scenario -list
```

The [demo gallery](demos.md) explains each case in everyday language. To run
one, follow the [scenario launch guide](../bench/scenario/graphnative/README.md)
for the matching profile and model configuration. Pass the same repeated
`-case` selections to profile creation and execution; mismatches are rejected.
Omitting a selection uses all 12 cases.

A subset is diagnostic, even if every attempt passes. It does not establish
full-suite performance. See [scenario scoring and replay](benchmark-reference.md#what-the-suites-judge)
for the retained artifacts and acceptance rules.

## Run a focused external suite

Start a configured voice server using the [quickstart](quickstart.md), and
prepare the chosen dataset using the commands in the scoring reference. Then,
in a separate terminal, a limited FDB run uses:

```bash
./openrealtime bench fdb --limit 4
```

A successful process exit alone is not a performance result. Read the task
outcomes, errors, and completeness fields, then listen to the affected
recordings. Keep the source revision and provider configuration with the run.

<a id="reading-a-result"></a>

## Read a result

| Field or artifact | What to check |
| --- | --- |
| Provenance | Source revision, executable, deployment, and machine |
| Expected tasks and completed tasks | Whether the intended population actually ran |
| Per-task outcomes | Failures, unsupported cases, timeouts, and scorer errors |
| Latency distributions | Sample count, median, tail, and missing responses |
| Recordings and transcript | Whether the scored event matches what is audible |
| Graph inspection and receipts | Which configuration ran and whether retained artifacts verify |

The [result reference](benchmark-reference.md#reading-a-result) shows the JSON
shape. Do not rank models from a single successful recording or an incomplete
subset. Missing or invalid results should remain visible.

<a id="what-the-suites-judge"></a>

## Reproduce a failure

1. Identify one failed task and retain its configuration and input.
2. Inspect the recording, events, and provider request relevant to the failure.
3. Determine whether the problem is in the runtime, provider, environment, or scorer.
4. Make the smallest repair and run a regression check that exercises it.
5. Repeat the affected live case when the repair depends on provider behavior.

For recorded scenarios with replay inputs, the
[replay command](benchmark-reference.md#what-the-suites-judge) can verify the
retained source and rerun deterministic scoring without provider credentials.
Replay verifies decisions against retained observations; it does not rerun the
model or independently establish model quality.

## Go deeper

<!-- Retain the original guide's section anchors for shared research links. -->

| Detailed contract | Reference |
| --- | --- |
| <a id="cells-and-pairing"></a>Paired comparisons | [Cells and pairing](benchmark-reference.md#cells-and-pairing) |
| <a id="openrealtime-realtime-cu-v1"></a>Computer-use suite | [Realtime-CU](benchmark-reference.md#openrealtime-realtime-cu-v1) |
| <a id="openrealtime-meeting-assistant-v1"></a>Meeting suite | [Meeting Assistant](benchmark-reference.md#openrealtime-meeting-assistant-v1) |
| <a id="dynacu-bench-optional-external-validation"></a>External computer-use evaluation | [DynaCU](benchmark-reference.md#dynacu-bench-optional-external-validation) |
| <a id="when-a-conversation-is-over"></a>Completion and timeouts | [Conversation completion](benchmark-reference.md#when-a-conversation-is-over) |
| <a id="adding-a-suite"></a>New suite implementations | [Adding a suite](benchmark-reference.md#adding-a-suite) |

- [Scoring, attestation, and suite contracts](benchmark-reference.md)
- [Adding a suite](benchmark-reference.md#adding-a-suite)
- [Release checks and optional diagnostics](release-validation.md)
- [Research overview](research.md) and [historical measurement log](measurement.md)
