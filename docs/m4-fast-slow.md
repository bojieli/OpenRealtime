# M4 fast/slow cognition lifecycle baseline

> **Historical boundary:** M4 is a completed, reproducible orchestration
> experiment. Its foreground-decision/background-update interface is retained
> as an independent fast/slow control; it is not the target continuous-thinking
> abstraction in plan version 0.2.

M4 separates deadline-bounded foreground behavior from asynchronous
deliberation. Both paths carry a goal ID and perception revision. Slow updates
also carry a monotonic sequence, terminal status, compute units, and an optional
symbolic quality score.

The coordinator accepts updates only for the current running goal revision.
Replacing a goal first cancels its old state; a late callback from a provider
that ignores cancellation is recorded as stale and cannot mutate the new goal.
The asynchronous runner supports provider completion, failure, explicit
cancellation, parent-context cancellation, and non-cancellable providers.

Foreground decisions make progress claims explicit:

- `none` does not claim work has started or finished.
- `working` is valid only while the slow coordinator is running.
- `completed` is valid only after a terminal successful update.

Thus an acknowledgement such as “Let me work through that carefully” can be
truthful immediately, while “I have completed the external check” is rejected
until completion. This is structural validation, not a general natural-language
truth detector.

## Reference comparison

The project-authored symbolic workload contains three difficult tasks and fixed
quality/cost annotations. Thirty seeded trials per task and condition produced:

| Condition | Truthful progress P50 | Final answer P50 | Quality P50 | Compute P50 | Success |
| --- | ---: | ---: | ---: | ---: | ---: |
| Single blocking | 386.242 ms | 386.242 ms | 94 | 88 | 90 / 90 |
| Fast only | 34.909 ms | 34.909 ms | 38 | 8 | 0 / 90 |
| Fast + slow | 34.909 ms | 386.242 ms | 94 | 97 | 90 / 90 |

The 450 ms symbolic deadline is missed in 18/90 slow-path trials in both
blocking and fast/slow conditions. Fast/slow does not make the final answer
finish sooner in this reference model; it provides earlier truthful progress at
additional foreground cost while retaining final quality.

All scores, costs, questions, answers, and delays are symbolic instrumentation.
They do not measure a language model, factual correctness, token billing, or
human preference. The result demonstrates orchestration and accounting only.

## Relationship to the target architecture

The target design uses one canonical trajectory rather than a foreground model
receiving advice from a separate slow coordinator. A fast model appends optional
reasoning and assistant content; a slow model then consumes that exact prefix
and appends further reasoning, tool calls/results, and assistant content. Model
identity changes at a safe trajectory boundary, but agent identity and working
memory do not.

M4 remains useful for three reasons:

- Its cancellation, deadline, and stale-callback invariants are still required
  by the continuation runner.
- Its independent update stream is the split-brain comparison condition for
  testing whether canonical trajectory inheritance reduces contradiction and
  repeated work.
- Its checked symbolic frontier must remain reproducible and must not be
  retrospectively described as a live-model result.

The experimental continuation interface is now implemented outside stable
`api/v1`. Fast call-shaped output is proposal-only working state; only the slow
continuation can append executable calls and results. A future `api/v2` may
replace `FastDecisionProvider` and `DeliberationProvider` only after the
trajectory schema, provider adapters, interruption behavior, and comparative
controls have been validated. See [live-cascade.md](live-cascade.md) for the
first heterogeneous live slice.

Run `./scripts/reproduce_m4.sh` to regression-check M0–M3, run race-tested
asynchronous lifecycle cases, and compare the complete report and frontier
visualization byte-for-byte.
