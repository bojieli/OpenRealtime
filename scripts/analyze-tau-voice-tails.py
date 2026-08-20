#!/usr/bin/env python3
"""Report tau-Voice response-latency tail distributions per population.

A mean latency hides the behaviour that matters in a spoken interaction: the
occasional multi-second gap a listener actually notices. This reports the
distribution -- median through maximum -- so a condition that improves the
median while worsening the tail cannot be read as a straight improvement.

Latency is measured in simulation ticks, which advance on a fixed clock
(``tick_duration_seconds``). The tick clock, not wall time, is the harness's
own timebase, so a busy host cannot inflate a measurement. A population whose
ticks do not share one constant tick duration is refused rather than averaged
across two different clocks.

One observation is the gap from the last tick of a user utterance to the first
tick carrying agent speech. Turns where the agent never responds contribute no
observation and are counted separately, because scoring an absent response as
latency zero would reward silence.

Read-only: this reads archived populations and never participates in scoring.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from pathlib import Path
from typing import Any

SCHEMA_VERSION = "1.0.0"


class TailAnalysisError(RuntimeError):
    """Raised when a population cannot be read as complete timing evidence."""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--population",
        required=True,
        action="append",
        type=Path,
        help="simulation population directory; repeatable",
    )
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def reconcile_index(index: list[dict[str, Any]], directory: Path) -> None:
    """Reconcile an index against the files it names, in both directions.

    The index is an assertion inside ``results.json``; the files under
    ``simulations/`` are the population these tools actually read. An entry
    naming an absent file is broken bookkeeping, and a file no entry names is
    evidence nothing would ever score -- either way the counts stop agreeing,
    so neither can be reported as a whole cell. Checking here rather than at
    each read means an entry whose file is never opened cannot slip past.
    """
    named = set()
    for entry in index:
        identifier = entry.get("id")
        if not isinstance(identifier, str) or not identifier:
            raise TailAnalysisError(f"{directory}: an index entry declares no id")
        if identifier in named:
            raise TailAnalysisError(
                f"{directory}: duplicate simulation id {identifier!r}"
            )
        named.add(identifier)
    present = {path.stem for path in (directory / "simulations").glob("*.json")}
    for identifier in sorted(named - present):
        raise TailAnalysisError(f"{directory}: missing simulation {identifier}.json")
    for identifier in sorted(present - named):
        raise TailAnalysisError(f"{directory}: unindexed simulation {identifier}.json")


def declared_scope(payload: dict[str, Any], directory: Path) -> dict[str, Any]:
    """What the population says it set out to run, versus what it actually holds.

    ``results.json`` records the task list and trial count the run was launched
    with, so a population that stopped early states its own shortfall. Reporting
    a partial cell without that fact would describe 12% of a cell in the same
    shape as a complete one.

    Three counts have to agree, not two. The declared scope and the simulation
    index are both assertions inside ``results.json``; only the files under
    ``simulations/`` are the population these tools actually read. An index that
    lists fifty entries over six files on disk would otherwise report a complete
    cell, so completeness is the agreement of all three.
    """
    tasks = payload.get("tasks")
    info = payload.get("info") or {}
    trials = info.get("num_trials")
    indexed = len(payload.get("simulation_index") or [])
    on_disk = len(list((directory / "simulations").glob("*.json")))
    scope: dict[str, Any] = {
        "simulations_indexed": indexed,
        "simulations_on_disk": on_disk,
    }
    if not isinstance(tasks, list) or not tasks or not isinstance(trials, int):
        # Without a declared scope there is nothing to reconcile the counts
        # against; say so rather than implying completeness never asserted.
        scope["declared"] = False
        return scope
    expected = len(tasks) * trials
    scope.update(
        declared=True,
        declared_tasks=len(tasks),
        declared_trials_per_task=trials,
        declared_simulations=expected,
        complete=expected == indexed == on_disk,
    )
    return scope


def quantile(ordered: list[float], fraction: float) -> float:
    """Nearest-rank quantile of an already sorted, non-empty sample."""
    if not ordered:
        raise TailAnalysisError("quantile of an empty sample")
    rank = max(1, math.ceil(fraction * len(ordered)))
    return ordered[min(rank, len(ordered)) - 1]


def distribution(sample: list[float]) -> dict[str, Any]:
    """Describe a sample by its tail, refusing to invent one that is absent."""
    if not sample:
        return {"observations": 0}
    ordered = sorted(sample)
    total = sum(ordered)
    mean = total / len(ordered)
    variance = sum((value - mean) ** 2 for value in ordered) / len(ordered)
    return {
        "observations": len(ordered),
        "mean": mean,
        "standard_deviation": math.sqrt(variance),
        "min": ordered[0],
        "p50": quantile(ordered, 0.50),
        "p90": quantile(ordered, 0.90),
        "p95": quantile(ordered, 0.95),
        "p99": quantile(ordered, 0.99),
        "max": ordered[-1],
    }


def has_speech(chunk: Any) -> bool:
    return isinstance(chunk, dict) and bool(chunk.get("content"))


def tick_duration(ticks: list[dict[str, Any]], label: str) -> float:
    """The one tick clock this population ran on."""
    durations = {
        tick.get("tick_duration_seconds")
        for tick in ticks
        if tick.get("tick_duration_seconds") is not None
    }
    if not durations:
        raise TailAnalysisError(f"{label}: ticks declare no tick duration")
    if len(durations) > 1:
        raise TailAnalysisError(
            f"{label}: ticks mix {len(durations)} tick durations {sorted(durations)!r}"
        )
    duration = durations.pop()
    if not isinstance(duration, (int, float)) or duration <= 0:
        raise TailAnalysisError(f"{label}: tick duration {duration!r} is not positive")
    return float(duration)


def response_latencies(
    simulation: dict[str, Any], label: str
) -> tuple[list[float], int]:
    """Seconds from the end of each user utterance to the agent's first speech."""
    ticks = simulation.get("ticks")
    if not isinstance(ticks, list) or not ticks:
        raise TailAnalysisError(f"{label}: simulation records no ticks")
    seconds_per_tick = tick_duration(ticks, label)

    latencies: list[float] = []
    unanswered = 0
    position = 0
    count = len(ticks)
    while position < count:
        if not has_speech(ticks[position].get("user_chunk")):
            position += 1
            continue
        last_user = position
        while last_user + 1 < count and has_speech(
            ticks[last_user + 1].get("user_chunk")
        ):
            last_user += 1
        search = last_user + 1
        while search < count and not has_speech(ticks[search].get("agent_chunk")):
            search += 1
        if search < count:
            latencies.append((search - last_user) * seconds_per_tick)
            position = search
        else:
            unanswered += 1
            position = last_user + 1
    return latencies, unanswered


def summarize(directory: Path) -> dict[str, Any]:
    results = directory / "results.json"
    if not results.is_file():
        raise TailAnalysisError(f"{directory}: no results.json")
    if results.stat().st_size == 0:
        raise TailAnalysisError(f"{directory}: results.json is empty")
    payload = json.loads(results.read_text())
    index = payload.get("simulation_index")
    if not isinstance(index, list) or not index:
        raise TailAnalysisError(f"{directory}: results.json declares no simulations")
    if not (directory / "simulations").is_dir():
        raise TailAnalysisError(f"{directory}: no simulations directory")
    reconcile_index(index, directory)

    every_latency: list[float] = []
    per_simulation_p90: list[float] = []
    durations: list[float] = []
    unanswered_total = 0
    simulations_read = 0

    for entry in index:
        identifier = entry.get("id")
        path = directory / "simulations" / f"{identifier}.json"
        if not path.is_file():
            raise TailAnalysisError(f"{directory}: missing simulation {path.name}")
        simulation = json.loads(path.read_text())
        label = f"{directory.name}/{identifier}"
        if entry.get("termination_reason") == "infrastructure_error":
            # Typed by the harness as never having run; it has no timing to read.
            continue
        latencies, unanswered = response_latencies(simulation, label)
        simulations_read += 1
        unanswered_total += unanswered
        every_latency.extend(latencies)
        if latencies:
            per_simulation_p90.append(quantile(sorted(latencies), 0.90))
        duration = simulation.get("duration")
        if isinstance(duration, (int, float)):
            durations.append(float(duration))

    if not every_latency:
        raise TailAnalysisError(
            f"{directory}: no response latency was observed in any simulation"
        )

    return {
        "population": directory.name,
        "simulations_declared": len(index),
        "scope": declared_scope(payload, directory),
        "simulations_measured": simulations_read,
        "unanswered_user_turns": unanswered_total,
        "response_latency_seconds": distribution(every_latency),
        "per_simulation_p90_seconds": distribution(per_simulation_p90),
        "simulation_duration_seconds": distribution(durations),
    }


def main() -> int:
    arguments = parse_args()
    populations = []
    for directory in arguments.population:
        try:
            populations.append(summarize(directory))
        except TailAnalysisError as error:
            print(f"tail analysis refused: {error}", file=sys.stderr)
            return 1

    report = {"schema_version": SCHEMA_VERSION, "populations": populations}
    text = json.dumps(report, indent=2)
    if arguments.output:
        arguments.output.parent.mkdir(parents=True, exist_ok=True)
        arguments.output.write_text(text + "\n")
    else:
        print(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
