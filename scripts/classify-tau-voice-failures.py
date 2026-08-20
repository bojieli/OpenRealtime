#!/usr/bin/env python3
"""Classify tau-Voice zero-reward simulations by the mechanism that ended them.

The matrix report already counts termination reasons. A count answers "how many
runs ended this way" and not "why", so a mean reward that a spoken-identifier
failure dragged down reads exactly like one a policy difference dragged down.
This decomposes the failures instead.

Every verdict is derived from evidence recorded in the simulation, and a
simulation whose evidence does not support a verdict is reported as
``unclassified`` rather than assigned to the nearest bucket. Silence about a
mechanism is a finding; a guess about one is a fabrication.

The classifier is read-only and never participates in scoring. It does not
change ``max_errors`` or any preregistered protocol.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from collections import Counter
from pathlib import Path
from typing import Any

SCHEMA_VERSION = "1.0.0"

# tau2 terminates a simulation once environment tool errors reach max_errors
# (full_duplex_orchestrator.py). The budget is a protocol constant, so a run
# that reaches it was truncated by the budget rather than by task completion.
TOO_MANY_ERRORS = "too_many_errors"
INFRASTRUCTURE_ERROR = "infrastructure_error"

# A run of single characters each followed by a separator is the agent
# forwarding spelled-out speech without reassembling it: it received
# "K O V A C S" and passed the letters through as-is. The run is matched
# anywhere in the value, because the agent often joins part of a token and
# leaves the rest spelled ("t_i_m_i_a_garcia_4516@example.com"). Three
# separated characters is the shortest run that is not plausibly a real
# identifier -- two would match ordinary values such as "a_b_name". The run
# may end at a word boundary, so a trailing unseparated character counts.
UNJOINED_SPELLING = re.compile(
    r"(?:(?<![A-Za-z0-9])[A-Za-z0-9][ _.-]){2,}(?<![A-Za-z0-9])[A-Za-z0-9](?![A-Za-z0-9])"
)


class ClassificationError(RuntimeError):
    """Raised when a population cannot be read as complete evidence."""


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


def load_population(directory: Path) -> tuple[dict[str, Any], Path]:
    """Read a population's results, refusing anything that is not readable."""
    results = directory / "results.json"
    if not results.is_file():
        raise ClassificationError(f"{directory}: no results.json")
    if results.stat().st_size == 0:
        raise ClassificationError(f"{directory}: results.json is empty")
    payload = json.loads(results.read_text())
    index = payload.get("simulation_index")
    if not isinstance(index, list) or not index:
        raise ClassificationError(f"{directory}: results.json declares no simulations")
    simulations = directory / "simulations"
    if not simulations.is_dir():
        raise ClassificationError(f"{directory}: no simulations directory")
    reconcile_index(index, directory)
    return payload, simulations


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
            raise ClassificationError(f"{directory}: an index entry declares no id")
        named.add(identifier)
    present = {path.stem for path in (directory / "simulations").glob("*.json")}
    for identifier in sorted(named - present):
        raise ClassificationError(f"{directory}: missing simulation {identifier}.json")
    for identifier in sorted(present - named):
        raise ClassificationError(
            f"{directory}: unindexed simulation {identifier}.json"
        )


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


def tool_calls(simulation: dict[str, Any]) -> list[dict[str, Any]]:
    calls: list[dict[str, Any]] = []
    for tick in simulation.get("ticks") or []:
        for call in tick.get("agent_tool_calls") or []:
            if isinstance(call, dict):
                calls.append(call)
    return calls


def tool_errors(simulation: dict[str, Any]) -> list[str]:
    errors: list[str] = []
    for tick in simulation.get("ticks") or []:
        for result in tick.get("agent_tool_results") or []:
            if isinstance(result, dict) and result.get("error"):
                errors.append(str(result.get("content", "")))
    return errors


def agent_speech(simulation: dict[str, Any]) -> str:
    parts = []
    for tick in simulation.get("ticks") or []:
        chunk = tick.get("agent_chunk")
        if isinstance(chunk, dict) and chunk.get("content"):
            parts.append(str(chunk["content"]))
    return " ".join(parts)


def user_speech(simulation: dict[str, Any]) -> str:
    parts = []
    for tick in simulation.get("ticks") or []:
        chunk = tick.get("user_chunk")
        if isinstance(chunk, dict) and chunk.get("content"):
            parts.append(str(chunk["content"]))
    return "".join(parts)


def identifier_arguments(calls: list[dict[str, Any]]) -> list[tuple[str, str, str]]:
    """Every (tool, field, value) the agent supplied as a lookup key."""
    supplied: list[tuple[str, str, str]] = []
    for call in calls:
        arguments = call.get("arguments")
        if not isinstance(arguments, dict):
            continue
        for field, value in arguments.items():
            if isinstance(value, str) and value.strip():
                supplied.append((str(call.get("name", "")), field, value))
    return supplied


def classify(simulation: dict[str, Any], reason: str) -> dict[str, Any]:
    """Name the mechanism that ended one simulation, or decline to."""
    if reason == INFRASTRUCTURE_ERROR:
        # Typed by the harness itself and already excluded from scoring.
        return {"mechanism": "infrastructure_error", "evidence": "typed by harness"}
    if reason != TOO_MANY_ERRORS:
        return {"mechanism": "not_error_terminated", "evidence": reason}

    errors = tool_errors(simulation)
    calls = tool_calls(simulation)
    supplied = identifier_arguments(calls)
    if not errors or not supplied:
        return {
            "mechanism": "unclassified",
            "evidence": f"{len(errors)} tool errors, {len(supplied)} identifier arguments",
        }

    # A lookup failure repeats the same tool against a shifting key. Anything
    # else -- a policy violation, a rejected write -- is a different mechanism.
    lookup_failures = sum(
        1 for text in errors if "not found" in text.lower() or "no user" in text.lower()
    )
    if lookup_failures * 2 < len(errors):
        return {
            "mechanism": "other_tool_errors",
            "evidence": f"{lookup_failures} of {len(errors)} errors are lookup misses",
            "sample_error": errors[0][:200],
        }

    values = [value for _, _, value in supplied]
    distinct = {value for value in values}
    unjoined = sorted({value for value in distinct if UNJOINED_SPELLING.search(value)})
    repair_requested = "spell" in agent_speech(simulation).lower()

    if unjoined:
        mechanism = "spelled_token_not_reassembled"
        evidence = f"agent forwarded {len(unjoined)} spelled-out argument(s) verbatim"
    elif len(distinct) > 1:
        mechanism = "identifier_variant_search"
        evidence = (
            f"{len(distinct)} distinct identifier values over {len(values)} calls"
        )
    else:
        mechanism = "identifier_not_varied"
        evidence = f"the same identifier {values[0]!r} was retried {len(values)} times"

    return {
        "mechanism": mechanism,
        "evidence": evidence,
        "lookup_errors": lookup_failures,
        "distinct_identifiers": len(distinct),
        "unjoined_spelling_examples": unjoined[:4],
        "repair_requested": repair_requested,
        "attempts": values[:10],
    }


def summarize(directory: Path) -> dict[str, Any]:
    payload, simulations = load_population(directory)
    index = payload["simulation_index"]
    reasons: Counter[str] = Counter()
    mechanisms: Counter[str] = Counter()
    repair_requested = 0
    error_terminated = 0
    details: list[dict[str, Any]] = []
    scored: list[float] = []

    for entry in index:
        reason = entry.get("termination_reason")
        if not isinstance(reason, str) or not reason:
            raise ClassificationError(
                f"{directory}: simulation {entry.get('id')!r} has no termination reason"
            )
        reasons[reason] += 1
        reward = entry.get("reward")
        if isinstance(reward, (int, float)):
            scored.append(float(reward))

        if reason not in (TOO_MANY_ERRORS, INFRASTRUCTURE_ERROR):
            continue

        path = simulations / f"{entry.get('id')}.json"
        if not path.is_file():
            raise ClassificationError(f"{directory}: missing simulation {path.name}")
        simulation = json.loads(path.read_text())
        verdict = classify(simulation, reason)
        mechanisms[verdict["mechanism"]] += 1
        if reason == TOO_MANY_ERRORS:
            error_terminated += 1
            if verdict.get("repair_requested"):
                repair_requested += 1
        details.append(
            {
                "task_id": entry.get("task_id"),
                "trial": entry.get("trial"),
                "termination_reason": reason,
                "reward": reward,
                **verdict,
            }
        )

    simulations_total = len(index)
    return {
        "population": directory.name,
        "simulations": simulations_total,
        "scope": declared_scope(payload, directory),
        "scored_simulations": len(scored),
        "mean_reward_scored": (sum(scored) / len(scored)) if scored else None,
        "termination_reasons": dict(sorted(reasons.items())),
        "error_terminated": error_terminated,
        "error_terminated_rate": error_terminated / simulations_total,
        "repair_requested_when_error_terminated": repair_requested,
        "mechanisms": dict(sorted(mechanisms.items())),
        "details": details,
    }


def main() -> int:
    arguments = parse_args()
    populations = []
    for directory in arguments.population:
        try:
            populations.append(summarize(directory))
        except ClassificationError as error:
            print(f"classification refused: {error}", file=sys.stderr)
            return 1

    combined: Counter[str] = Counter()
    total = 0
    error_terminated = 0
    for population in populations:
        combined.update(population["mechanisms"])
        total += population["simulations"]
        error_terminated += population["error_terminated"]

    report = {
        "schema_version": SCHEMA_VERSION,
        "populations": populations,
        "combined": {
            "simulations": total,
            "error_terminated": error_terminated,
            "error_terminated_rate": (error_terminated / total) if total else None,
            "mechanisms": dict(sorted(combined.items())),
        },
    }
    text = json.dumps(report, indent=2, sort_keys=False)
    if arguments.output:
        arguments.output.parent.mkdir(parents=True, exist_ok=True)
        arguments.output.write_text(text + "\n")
    else:
        print(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
