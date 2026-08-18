# Metrics and measurement rules

OpenRealtime reports distributions and Pareto frontiers. It does not optimize
one headline latency while hiding wrong starts, repairs, quality, or cost.

## Primary timing metrics

- Capture-to-revision latency: `perception.revision.monotonic_ns` minus the
  latest contributing frame’s `capture_ns`.
- Stable-prefix latency: time from the end of the corresponding spoken token
  to the first revision declaring it stable.
- Opportunity-to-invocation latency: provider start minus the fixed or
  event-driven microturn that enabled it. Report opportunities that caused no
  call separately rather than treating them as zero-latency calls.
- Invocation-to-first-token latency: first reasoning or assistant token minus
  provider invocation start, stratified by fast/slow phase and cache state.
- End-of-user-speech to first semantic audio: first played output that advances
  the task, excluding neutral fillers, minus reference user endpoint.
- Turn-gap error: system semantic onset minus annotated target onset. Report
  signed error and absolute error.
- Stop latency: first confirmed interruption evidence to the end of the last
  played system sample.
- Deadline miss rate: deadline-bounded operations completed after their named
  deadline divided by all such operations.
- Continued-result latency: first slow reasoning/tool/content item and final
  correct answer or tool outcome minus the causal user observation.

For every metric report P50, P90, P95, P99, sample count, bootstrap confidence
intervals where appropriate, and the unaggregated values. Never substitute
provider request timestamps for observed audio playback timestamps.

## Interaction and content guardrails

Timing comparisons must accompany premature takeover, missed turn, false stop,
failure-to-stop, audible false-start duration, repair count and duration,
repetition or contradiction, and task success. Content measures depend on the
workload: WER or semantic error for perception, factual or reasoning score for
answers, tool correctness, and translation quality for translation.

Canonical-trajectory comparisons additionally report:

- Contradictions between slow continuation and content already played.
- Explicit repairs when a played commitment changes.
- Repeated reasoning or duplicate tool calls.
- False capability denial despite an available capability manifest.
- Claims of tool completion without a causal completed result.
- Successful resumption after user, ASR, or tool interruption.

“First audio” and “first semantic audio” are separate. A benchmark annotation
must state why an output is substantive or a backchannel. Exploratory automatic
classification must be checked against blinded human labels before supporting
a confirmatory claim.

## Efficiency

Record CPU or accelerator model, utilization, memory, audio and text tokens,
network bytes, discarded speculative work, provider price snapshot, cost per
successful interaction, and cost per conversation minute. Cost comparisons
must name currency and retrieval date.

Live co-location reports component queue arrival, wait and service time, model
residency, cache reuse, preemption delay, and microturn opportunity/provider-call
counts. Raw reasoning text is not required for these metrics; phase boundaries,
timestamps, counts, and causal item references are sufficient.

The admission snapshot uses fixed-memory per-class totals and maxima for
enqueue-to-grant wait, grant-to-release service, and preemption-to-release
acknowledgement. The last interval measures cooperative cancellation latency;
capacity remains occupied throughout it. Per-class aggregates do not replace
per-request traces or quantile sampling in multi-trial studies.

The audio benchmark uses **provider-boundary service time** for the elapsed wall
time around one provider advance. It can include admission wait, local HTTP or
IPC transport, server queueing, and model service. It is not device-kernel
compute time and must not be reported as such. Version 0.3 fields are
`provider_boundary_ms`, `provider_service_ms`, `provider_service_rtf`,
`mean_provider_invocation_ms`, and `p95_provider_invocation_ms`.

Pre-endpoint preparation additionally reports observed revisions, distinct
semantic fingerprints, chains started/completed/superseded/failed/coalesced,
per-stage ready/eligible/start/first-event/end time, disposition, and whether an
exact final root was committed. A stage cancelled while temporally paced has no
fabricated invocation ID or start timestamp. Counts distinguish paced waits,
cancellations before provider launch, and exact-commit pacing bypasses. Replay
and live-fallback counts are separate. A root match is insufficient by itself:
every captured fast or slow stage must match the complete canonical request it
actually receives. Endpoint-to-fast latency is measured only for the committed
attempt; discarded speculation is never selected as the latency winner. If a
matching candidate completed before endpoint, the endpoint-relative value is
zero while the report still retains that candidate's nonzero invocation
latency.

The live gateway additionally keeps disjoint provider aggregates for canonical
fast/slow calls and private fast/slow preparation calls. Classification comes
from the typed execution path that owns the call, never transcript content.
Each class separately reports starts, completion/failure/cancellation, events,
tokens, and provider/first-event time; no subtraction of nested timers is used.

Tool metrics distinguish non-executable proposals from authoritative calls.
Proposal count is not tool-call success, and a result counts only when it is
causally attached to an executable call with matching identity.
Checked tasks may declare an exact multiset of tool name plus canonical JSON
arguments. Extra calls, missing calls, invalid argument objects, and tool errors
fail by default. A later correct answer does not erase a preceding wrong or
failed external action.
