#!/usr/bin/env python3
"""Summarise a micro-turn cascade trace (sidecars/microturn_sidecar.py --trace).

The clock's own evidence, which no benchmark can see from outside: how long
words waited between being committed by the recogniser and being admitted at a
tick, how long each control decision took, how often a decision missed the
tick it belonged to, what the controller decided, and which of the three
triggers produced each decision (the clock, a word arriving while speaking, or
the user going quiet).

    python tools/duplexmodels/microturn_trace.py .runtime/duplex-plan/traces/*.jsonl
"""

import argparse
import json
import statistics
from collections import Counter
from pathlib import Path


def percentile(values, fraction):
    if not values:
        return None
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, int(fraction * len(ordered)))]


def summarise(path: Path, tick_ms: float) -> dict:
    ticks, waits, decisions, triggers, spent = 0, [], Counter(), Counter(), []
    sessions, previous = 0, None
    for line in path.read_text().splitlines():
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if "tick" not in row:
            continue
        if previous is not None and row["tick"] < previous:
            sessions += 1
        previous = row["tick"]
        ticks += 1
        decisions[row["decision"]] += 1
        triggers[row.get("trigger", "clock")] += 1
        waits.extend(row.get("evidence_wait_ms") or [])
        if row.get("decision_ms"):
            spent.append(row["decision_ms"])
    return {
        "trace": path.name, "sessions": sessions + 1, "ticks": ticks,
        "decisions": dict(decisions.most_common()), "triggers": dict(triggers.most_common()),
        "evidence_wait_ms": {"p50": percentile(waits, 0.5), "p90": percentile(waits, 0.9),
                             "max": max(waits, default=None), "count": len(waits)},
        "decision_ms": {"p50": percentile(spent, 0.5), "p90": percentile(spent, 0.9),
                        "max": max(spent, default=None), "count": len(spent)},
        # A decision that outlasts its tick has delayed the next one.
        "decisions_over_tick": sum(1 for value in spent if value > tick_ms),
        "mean_decision_ms": round(statistics.mean(spent), 1) if spent else None,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("traces", nargs="+")
    parser.add_argument("--tick-ms", type=float, default=500)
    parser.add_argument("--json", default="")
    args = parser.parse_args()
    rows = [summarise(Path(name), args.tick_ms) for name in args.traces]
    print("| trace | sessions | ticks | evidence wait p50/p90 ms | decision p50/p90 ms | over tick | decisions | triggers |")
    print("|" + " --- |" * 8)
    for row in rows:
        wait, spent = row["evidence_wait_ms"], row["decision_ms"]
        show = lambda d: "/".join("-" if d[k] is None else f"{d[k]:.0f}" for k in ("p50", "p90"))  # noqa: E731
        print(f"| {row['trace']} | {row['sessions']} | {row['ticks']} | {show(wait)} | {show(spent)} "
              f"| {row['decisions_over_tick']} | {row['decisions']} | {row['triggers']} |")
    if args.json:
        Path(args.json).write_text(json.dumps(rows, indent=2) + "\n")


if __name__ == "__main__":
    main()
