# The benchmark harness

Every suite reduces to the same thing: play audio into a session and judge what
came out. So there is one driver, and the suites differ only in what they play
and what they look for.

The harness drives a **running server over the protocol**, not an in-process
session. A measurement of something other than what users get is not a
measurement of anything.

```sh
openrealtime serve &                       # the system under test
openrealtime bench fdb --limit 4           # a suite against it
```

## Suites

| Command | Suite | Measures |
| --- | --- | --- |
| `bench fdb` | FDB v1.5 | overlap: yielding to interruptions, holding through backchannels |
| `bench fdbv3` | FDB v3 | tool use under disfluent speech, including spelled identifiers |
| `bench fdbench` | FD-Bench | endpointing and response timing at scale |

τ-Voice and DynaCU-Bench run through their own environments; see
[measurement.md](measurement.md).

## Cells and pairing

A run measures one **cell**: a complete configuration, including the factors
that did not change. A cell that recorded only its differences could not be
compared against a reference that later moved.

```sh
openrealtime bench fdb -cell reference -out results/reference.json
openrealtime bench fdb -vary F2 -level fast-only -out results/fast-only.json
openrealtime compare results/reference.json results/fast-only.json
```

`compare` refuses anything that would not mean something:

- cells that differ in more than one factor, because a difference cannot be
  attributed to any of them;
- an incomplete cell, because a partially executed cell is a different
  experiment rather than a smaller one;
- a cell from a modified working tree, because it cannot be reproduced;
- a comparison across suites, because two suites measuring different things do
  not average.

Every one of those is a refusal rather than a warning. A measurement program
that warns is a measurement program that gets ignored.

## Reading a result

```jsonc
{
  "suite": "fdb-v1.5",
  "cell": { "name": "reference", "levels": { "F1": "cascade", "...": "..." } },
  "provenance": {
    "revision": "…", "modified": false, "executable_sha256": "…",
    "machine": { "cpu": "…", "gpu": "…" }
  },
  "expected_tasks": 498,
  "tasks": [ { "id": "user_interruption/1", "completed": true, "passed": true,
               "metrics": { "yield_latency_ms": 194 } } ],
  "summary": {
    "complete": true, "pass_rate": 0.87,
    "distributions": { "yield_latency_ms": { "p50": 201, "p95": 640, "max": 980 } }
  }
}
```

Latency is always a distribution. A mean without its tail describes a system
nobody is using.

## What the suites judge

**FDB v1.5** has four categories and two of them want the opposite of the other
two: yield to an interruption, hold through a backchannel, background speech,
and speech addressed to somebody else. A system that scores well by always
yielding is not a system that handles overlap, which is why the report breaks
the four out rather than averaging them.

A recording where the agent was not speaking when the event arrived is reported
as **not applicable** rather than as a pass or a failure. It says something
about latency and nothing about overlap, and folding it in either direction
would corrupt both readings.

**FDB v3** checks the call and its arguments separately. "Track order
B-O-B-1-2" has to become `track_order(order_id="BOB12")`, and reassembling a
spelled identifier from speech is a distinct failure from not knowing which
tool to call. The report says which one happened.

The agent is offered the union of every tool in the dataset, not the one the
task expects. Handing it exactly the right tool would measure whether it can
call the only tool available.

**FD-Bench** measures three things in tension: response latency, whether the
agent started speaking while the person still was, and whether it answered at
all before the next turn. Tuning for any one of them damages another, so a
single number would hide the trade.

It also separates *premature* from *overrun*: an answer that begins while the
person is mid-turn is an endpointing failure, while one that runs past the gap
and is then cut short is what barge-in is for. They have different causes.

## Adding a suite

Implement `Load` for the dataset and a function that turns a
`bench.Transcript` into a `bench.TaskOutcome`. The transcript is a timed record
of everything that happened — user speech boundaries, transcripts, agent text
and audio with durations, tool calls — so a new metric is usually a question
about that record rather than new plumbing.

Then let `bench.Result.Finish` derive the summary, and `Reportable` decide
whether it may be published. Do not compute a pass rate yourself: the point of
the harness is that no suite gets to decide it is complete.
