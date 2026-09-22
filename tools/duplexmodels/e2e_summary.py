#!/usr/bin/env python3
"""Summarise duplex-plan end-to-end runs (deploy/duplex/run-e2e.sh).

Reads .runtime/duplex-plan/results/e2e/<profile>/ and prints one row per
profile: FDB v1.5 pass counts per category over applicable recordings (with
the not-applicable count, which is a latency failure of a different kind),
median yield latency, and FD-Bench answered / premature / median response
latency. Subsets are smoke measurements; the table says so.

    python tools/duplexmodels/e2e_summary.py [--json out.json] [profile ...]
"""

from __future__ import annotations

import argparse
import json
import statistics
from e2e_run import digest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2] / ".runtime/duplex-plan/results/e2e"
CATEGORIES = ["user_interruption", "user_backchannel", "background_speech", "talking_to_other"]


def applicability(task: dict) -> str:
    typed = task.get("applicability")
    legacy = {"true": "applicable", "false": "not_applicable"}.get(
        task.get("notes", {}).get("applicable"))
    if typed:
        if typed not in ("applicable", "not_applicable") or (legacy and legacy != typed):
            return "unknown"
        return typed
    return legacy or "unknown"


def fdb(directory: Path) -> dict:
    result = {}
    for category in CATEGORIES:
        path = directory / f"fdb-{category}.json"
        if not path.exists():
            continue
        data = json.loads(path.read_text())
        tasks = data.get("tasks", [])
        valid = [t for t in tasks if not t.get("error")]
        applicable = [t for t in valid if applicability(t) == "applicable"]
        passed = [t for t in applicable if t.get("passed")]
        failed = [t for t in tasks if t.get("error")]
        latencies = [t["metrics"]["yield_latency_ms"] for t in applicable
                     if "yield_latency_ms" in t.get("metrics", {})]
        result[category] = {
            "tasks": len(tasks), "applicable": len(applicable), "passed": len(passed),
            "not_applicable": sum(applicability(t) == "not_applicable" for t in valid),
            "unknown_applicability": sum(applicability(t) == "unknown" for t in valid),
            "errors": len(failed),
            "yield_latency_ms_p50": statistics.median(latencies) if latencies else None,
        }
    return result


def fdbench(directory: Path) -> dict:
    path = directory / "fdbench.json"
    if not path.exists():
        return {}
    data = json.loads(path.read_text())
    tasks = [t for t in data.get("tasks", []) if not t.get("error")]
    total = lambda key: sum(t.get("metrics", {}).get(key, 0) for t in tasks)  # noqa: E731
    latencies = [t["metrics"]["response_latency_ms"] for t in tasks if "response_latency_ms" in t.get("metrics", {})]
    return {
        "conversations": len(data.get("tasks", [])), "errors": len(data.get("tasks", [])) - len(tasks),
        "turns": total("turns"), "answered": total("answered"), "premature": total("premature_turns"),
        "missed": total("missed_turns"), "response_latency_ms_p50": statistics.median(latencies) if latencies else None,
    }


def staleness(directory: Path) -> str:
    """Report result files older than the run that should have replaced them.

    A cell that failed part way leaves a previous run's files in place, and a
    table that mixed the two would present them as one measurement.
    """
    finished = directory / "finished.json"
    if not finished.exists():
        return "INCOMPLETE (no finished.json)"
    try:
        run = json.loads((directory / "run.json").read_text())
        completion = json.loads(finished.read_text())
        if run.get("schema") != 2:
            return "UNVERIFIED legacy run (no result digests or command statuses)"
        if completion.get("exit_code") != 0 or completion.get("errors"):
            return "FAILED (see finished.json)"
        expected = [f"fdb-{category}.json" for category in CATEGORIES]
        if run.get("fdbench_conversations", 0):
            expected.append("fdbench.json")
        if run.get("expected_results") != expected:
            return "INVALID result inventory"
        for name in ["run.json", "profile.yaml", *expected]:
            actual = digest(directory / name)
            if completion.get("sha256", {}).get(name) != actual:
                return f"MODIFIED or missing digest: {name}"
        for name in expected:
            command = completion.get("commands", {}).get(name, {})
            if command.get("exit_code") != 0 or command.get("error"):
                return f"FAILED or missing command: {name}"
        return ""
    except (OSError, ValueError, TypeError, AttributeError) as error:
        return f"INVALID metadata: {error}"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("profiles", nargs="*")
    parser.add_argument("--json", default="")
    args = parser.parse_args()
    directories = [ROOT / name for name in args.profiles] if args.profiles else sorted(p for p in ROOT.glob("*") if p.is_dir())
    rows = {}
    for directory in directories:
        name = directory.name
        if (directory / "latest").is_symlink():
            directory = directory / "latest"
        integrity = staleness(directory)
        try:
            run = json.loads((directory / "run.json").read_text())
        except (OSError, ValueError):
            run = {}
        rows[name] = {"run": run, "path": str(directory.resolve()),
                      "fdb": fdb(directory) if not integrity else {},
                      "fdbench": fdbench(directory) if not integrity else {},
                      "integrity": integrity}
    columns = ["profile", "interrupt yield", "backchannel hold", "background hold", "other-talk hold",
               "yield p50 ms", "FD-Bench answered/turns", "premature", "resp p50 ms", "load"]
    print("Smoke measurements; NOT REPORTABLE as a full campaign. Invalid or unverified runs are excluded.\n")
    print("| " + " | ".join(columns) + " |")
    print("|" + " --- |" * len(columns))
    for name, row in rows.items():
        cells = [name]
        for category in CATEGORIES:
            entry = row["fdb"].get(category)
            if entry:
                cell = f"{entry['passed']}/{entry['applicable']} (+{entry['not_applicable']} n/a)"
                if entry["unknown_applicability"]:
                    cell += f"; {entry['unknown_applicability']} unknown"
                cells.append(cell)
            else:
                cells.append("-")
        yields = row["fdb"].get("user_interruption", {}).get("yield_latency_ms_p50")
        cells.append(f"{yields:.0f}" if yields is not None else "-")
        bench = row["fdbench"]
        cells.append(f"{bench['answered']}/{bench['turns']}" if bench else "-")
        cells.append(str(bench.get("premature", "-")))
        latency = bench.get("response_latency_ms_p50")
        cells.append(f"{latency:.0f}" if latency is not None else "-")
        cells.append(str(row["run"].get("load_average", "?")))
        print("| " + " | ".join(cells) + " |")
        if row["integrity"]:
            print(f"| {name} | **{row['integrity']}** |" + " |" * (len(columns) - 2))
    if args.json:
        Path(args.json).write_text(json.dumps(rows, indent=2))


if __name__ == "__main__":
    main()
