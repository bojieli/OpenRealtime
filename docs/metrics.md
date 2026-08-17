# Metrics and measurement rules

OpenRealtime reports distributions and Pareto frontiers. It does not optimize
one headline latency while hiding wrong starts, repairs, quality, or cost.

## Primary timing metrics

- Capture-to-revision latency: `perception.revision.monotonic_ns` minus the
  latest contributing frame’s `capture_ns`.
- Stable-prefix latency: time from the end of the corresponding spoken token
  to the first revision declaring it stable.
- End-of-user-speech to first semantic audio: first played output that advances
  the task, excluding neutral fillers, minus reference user endpoint.
- Turn-gap error: system semantic onset minus annotated target onset. Report
  signed error and absolute error.
- Stop latency: first confirmed interruption evidence to the end of the last
  played system sample.
- Deadline miss rate: deadline-bounded operations completed after their named
  deadline divided by all such operations.

For every metric report P50, P90, P95, P99, sample count, bootstrap confidence
intervals where appropriate, and the unaggregated values. Never substitute
provider request timestamps for observed audio playback timestamps.

## Interaction and content guardrails

Timing comparisons must accompany premature takeover, missed turn, false stop,
failure-to-stop, audible false-start duration, repair count and duration,
repetition or contradiction, and task success. Content measures depend on the
workload: WER or semantic error for perception, factual or reasoning score for
answers, tool correctness, and translation quality for translation.

“First audio” and “first semantic audio” are separate. A benchmark annotation
must state why an output is substantive or a backchannel. Exploratory automatic
classification must be checked against blinded human labels before supporting
a confirmatory claim.

## Efficiency

Record CPU or accelerator model, utilization, memory, audio and text tokens,
network bytes, discarded speculative work, provider price snapshot, cost per
successful interaction, and cost per conversation minute. Cost comparisons
must name currency and retrieval date.
