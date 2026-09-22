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
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2] / ".runtime/duplex-plan/results/e2e"
CATEGORIES = ["user_interruption", "user_backchannel", "background_speech", "talking_to_other"]


def fdb(directory: Path) -> dict:
    result = {}
    for category in CATEGORIES:
        path = directory / f"fdb-{category}.json"
        if not path.exists():
            continue
        data = json.loads(path.read_text())
        tasks = data.get("tasks", [])
        applicable = [t for t in tasks if t.get("notes", {}).get("applicable") == "true"]
        passed = [t for t in applicable if t.get("passed")]
        failed = [t for t in tasks if t.get("error")]
        latencies = [t["metrics"]["yield_latency_ms"] for t in applicable
                     if "yield_latency_ms" in t.get("metrics", {})]
        result[category] = {
            "tasks": len(tasks), "applicable": len(applicable), "passed": len(passed),
            "not_applicable": len(tasks) - len(applicable) - len(failed), "errors": len(failed),
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


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("profiles", nargs="*")
    parser.add_argument("--json", default="")
    args = parser.parse_args()
    directories = [ROOT / name for name in args.profiles] if args.profiles else sorted(p for p in ROOT.iterdir() if p.is_dir())
    rows = {}
    for directory in directories:
        run = json.loads((directory / "run.json").read_text()) if (directory / "run.json").exists() else {}
        rows[directory.name] = {"run": run, "fdb": fdb(directory), "fdbench": fdbench(directory)}
    columns = ["profile", "interrupt yield", "backchannel hold", "background hold", "other-talk hold",
               "yield p50 ms", "FD-Bench answered/turns", "premature", "resp p50 ms", "load"]
    print("| " + " | ".join(columns) + " |")
    print("|" + " --- |" * len(columns))
    for name, row in rows.items():
        cells = [name]
        for category in CATEGORIES:
            entry = row["fdb"].get(category)
            cells.append(f"{entry['passed']}/{entry['applicable']} (+{entry['not_applicable']} n/a)" if entry else "-")
        yields = row["fdb"].get("user_interruption", {}).get("yield_latency_ms_p50")
        cells.append(f"{yields:.0f}" if yields is not None else "-")
        bench = row["fdbench"]
        cells.append(f"{bench['answered']}/{bench['turns']}" if bench else "-")
        cells.append(str(bench.get("premature", "-")))
        latency = bench.get("response_latency_ms_p50")
        cells.append(f"{latency:.0f}" if latency is not None else "-")
        cells.append(str(row["run"].get("load_average", "?")))
        print("| " + " | ".join(cells) + " |")
    if args.json:
        Path(args.json).write_text(json.dumps(rows, indent=2))


if __name__ == "__main__":
    main()
