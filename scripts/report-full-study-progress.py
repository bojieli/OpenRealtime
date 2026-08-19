#!/usr/bin/env python3
"""Report live, non-authoritative progress for the preregistered voice study.

This monitor never scores a partial population. The only publication authority
is report-full-study.py; this script reports counts and operational warnings so
long-running queues can be supervised without reading model or task content.
"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import shutil
from typing import Any


# Kernel process states in which a supervisor still holds its pid but can never
# advance. The study is one serial dependency chain, so a stopped or unreaped
# supervisor silently blocks every queue behind it while an existence check
# alone still reports it as live.
STALLED_PROCESS_STATES = {
    "T": "stopped",
    "t": "in tracing stop",
    "Z": "an unreaped zombie",
    "X": "dead",
    "x": "dead",
}


QUEUE_SPECS = (
    (
        "primary",
        "voice-benchmark-queue-v1",
        "run-voice-benchmark-queue.sh",
        "voice benchmark queue complete",
    ),
    (
        "asr",
        "voice-optimization-queue-v1",
        "run-voice-optimization-queue.sh",
        "voice optimization queue complete",
    ),
    (
        "fd_bench",
        "fd-bench-queue-v1",
        "run-fdbench-queue.sh",
        "FD-Bench queue complete",
    ),
    (
        "fast",
        "fast-provider-queue-v1",
        "run-fast-provider-queue.sh",
        "fast-provider queue complete",
    ),
    (
        "tau_reports",
        "tau-report-queue-v1",
        "run-tau-report-queue.sh",
        "tau reporting queue complete",
    ),
    (
        "cadence_and_effort",
        "tau-extended-ablation-queue-v1",
        "run-tau-extended-ablation-queue.sh",
        "tau extended ablation queue complete",
    ),
    (
        "context",
        "tau-cognitive-control-queue-v1",
        "run-tau-cognitive-control-queue.sh",
        "tau cognitive-control queue complete",
    ),
    (
        "event_adaptive",
        "tau-event-adaptive-queue-v1",
        "run-tau-event-adaptive-queue.sh",
        "tau event-adaptive queue complete",
    ),
    (
        "endpoint_preparation",
        "tau-endpoint-preparation-queue-v1",
        "run-tau-endpoint-preparation-queue.sh",
        "tau endpoint-preparation queue complete",
    ),
    (
        "publication",
        "full-study-report-queue-v1",
        "run-full-study-report-queue.sh",
        "full study report queue complete",
    ),
)


def arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", type=Path, default=Path.cwd())
    parser.add_argument(
        "--manifest", type=Path, default=Path("benchmarks/full-study-v1.json")
    )
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def read_json(path: Path) -> dict[str, Any] | None:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    return value if isinstance(value, dict) else None


def resolve(root: Path, path: Path) -> Path:
    return path.resolve() if path.is_absolute() else (root / path).resolve()


def count_json_files(path: Path) -> int:
    if not path.is_dir():
        return 0
    return sum(
        1 for item in path.iterdir() if item.is_file() and item.suffix == ".json"
    )


def tau_matrix_progress(root: Path, matrix_path: Path) -> dict[str, Any]:
    matrix = read_json(matrix_path)
    if matrix is None:
        return {"matrix": str(matrix_path), "status": "invalid_or_missing"}
    matrix_id = matrix.get("matrix_id")
    seed = matrix.get("benchmark", {}).get("seed")
    trials = matrix.get("benchmark", {}).get("num_trials")
    cells = matrix.get("cells", [])
    domains = matrix.get("benchmark", {}).get("domains", [])
    expected = 0
    observed = 0
    archive_expected = 0
    archives = 0
    populations: list[dict[str, Any]] = []
    for cell in cells:
        cell_id = cell.get("id")
        for domain in domains:
            name = domain.get("name")
            tasks = domain.get("tasks")
            declared = (
                tasks * trials
                if isinstance(tasks, int) and isinstance(trials, int)
                else 0
            )
            experiment = (
                root
                / ".runtime/tau2-bench/data/simulations"
                / f"{matrix_id}-{cell_id}-{name}-seed{seed}"
            )
            count = count_json_files(experiment / "simulations")
            archive_present = (
                experiment / "raw-artifacts-archive.json"
            ).is_file() and (experiment / "raw-artifacts.tar.zst").is_file()
            expected += declared
            observed += count
            archive_expected += 1
            archives += int(archive_present)
            populations.append(
                {
                    "cell": cell_id,
                    "domain": name,
                    "observed": count,
                    "expected": declared,
                    "archive_present": archive_present,
                }
            )
    report_path = (
        root / ".runtime/benchmark-runs/tau-voice" / str(matrix_id) / "report.json"
    )
    report = read_json(report_path)
    return {
        "matrix_id": matrix_id,
        "observed": observed,
        "expected": expected,
        "archives": archives,
        "archives_expected": archive_expected,
        "report_complete": bool(report and report.get("status") == "complete"),
        "populations": populations,
    }


def external_progress(root: Path, specification: dict[str, Any]) -> dict[str, Any]:
    run_path = resolve(root, Path(specification["run_manifest"]))
    context_path = resolve(root, Path(specification["run_context"]))
    run = read_json(run_path)
    context = read_json(context_path)

    # A manifest that exists but records no list is malformed, not a run that
    # has finished nothing. Counting it as 0 makes a corrupt manifest read
    # exactly like a queue that has only just started, so the monitor names the
    # defect instead -- it stays resilient and terminal, and the caller raises
    # the report to "attention".
    malformed: list[str] = []

    def population(name: str) -> int:
        value = run.get(name, []) if run else []
        if isinstance(value, list):
            return len(value)
        malformed.append(f"run manifest {run_path} records no {name} list")
        return 0

    return {
        "observed": population("completed"),
        "expected": specification["population"],
        "terminal_failures": population("failures"),
        "run_context_status": context.get("status") if context else "not_started",
        "malformed": malformed,
    }


def process_alive(pid: int | None) -> bool:
    """Report whether pid still exists.

    Existence is weaker than progress: a stopped or unreaped supervisor keeps
    its pid and answers this check, so callers that supervise a queue must also
    consult process_state.
    """
    if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except OSError:
        return False
    return True


def process_state(pid: int | None) -> str | None:
    """Return the single-character kernel state for pid, or None if unreadable."""
    if not process_alive(pid):
        return None
    try:
        status = (Path("/proc") / str(pid) / "status").read_text(encoding="utf-8")
    except OSError:
        return None
    for line in status.splitlines():
        if line.startswith("State:"):
            fields = line.split()
            return fields[1] if len(fields) >= 2 else None
    return None


def process_matches(pid: int | None, expected_script: str) -> bool:
    if not process_alive(pid):
        return False
    try:
        argv = (Path("/proc") / str(pid) / "cmdline").read_bytes().split(b"\0")
    except OSError:
        return False
    return any(
        Path(value.decode(errors="replace")).name == expected_script
        for value in argv
        if value
    )


def queue_progress(root: Path) -> list[dict[str, Any]]:
    queue_root = root / ".runtime/benchmark-runs"
    result = []
    for queue_id, directory, expected_script, completion_marker in QUEUE_SPECS:
        path = queue_root / directory
        try:
            pid = int((path / "queue.pid").read_text(encoding="utf-8").strip())
        except (OSError, ValueError):
            pid = None
        try:
            log = (path / "queue.log").read_text(encoding="utf-8", errors="replace")
        except OSError:
            log = ""
        complete = completion_marker in log.splitlines()
        alive = process_alive(pid)
        state = process_state(pid)
        result.append(
            {
                "id": queue_id,
                "pid": pid,
                "alive": alive,
                "state": state,
                "progressing": alive and state not in STALLED_PROCESS_STATES,
                "identity_matches": alive and process_matches(pid, expected_script),
                "complete": complete,
            }
        )
    return result


def pilot_progress(root: Path) -> dict[str, Any]:
    matrix_path = root / "benchmarks/tau-voice/matrix-v1.json"
    matrix = read_json(matrix_path)
    if matrix is None:
        return {"status": "missing"}
    matrix_id = matrix["matrix_id"]
    seed = matrix["benchmark"]["seed"]
    observed = 0
    expected = 0
    populations = []
    for domain in matrix["benchmark"]["domains"]:
        experiment = (
            root
            / ".runtime/tau2-bench/data/simulations"
            / f"{matrix_id}-i1-qg-control-{domain['name']}-seed{seed}"
        )
        count = count_json_files(experiment / "simulations")
        observed += count
        expected += domain["tasks"]
        populations.append(
            {
                "domain": domain["name"],
                "observed": count,
                "expected": domain["tasks"],
            }
        )
    return {
        "classification": "pre-freeze non-causal pilot",
        "observed": observed,
        "expected": expected,
        "populations": populations,
    }


def build_progress(root: Path, manifest_path: Path) -> dict[str, Any]:
    manifest = read_json(manifest_path)
    if manifest is None:
        raise RuntimeError(f"study manifest is invalid or missing: {manifest_path}")
    tau_matrices = [
        tau_matrix_progress(root, resolve(root, Path(item["path"])))
        for item in manifest["tau_voice"]["matrices"]
    ]
    tau_observed = sum(item.get("observed", 0) for item in tau_matrices)
    tau_expected = sum(item.get("expected", 0) for item in tau_matrices)
    archives = sum(item.get("archives", 0) for item in tau_matrices)
    archives_expected = sum(item.get("archives_expected", 0) for item in tau_matrices)
    external = {
        "full_duplex_bench_v1_5": external_progress(
            root, manifest["full_duplex_bench_v1_5"]
        ),
        "full_duplex_bench_v3": external_progress(
            root, manifest["full_duplex_bench_v3"]
        ),
        "fd_bench": external_progress(root, manifest["fd_bench"]),
    }
    queues = queue_progress(root)
    disk = shutil.disk_usage(root)
    publication_path = root / ".runtime/benchmark-runs/full-study-v1/report.json"
    publication = read_json(publication_path)
    warnings = []
    for queue in queues:
        if queue["complete"]:
            continue
        if not queue["alive"] or not queue["identity_matches"]:
            warnings.append(
                f"queue {queue['id']} has no matching live process and is not complete"
            )
        elif not queue["progressing"]:
            warnings.append(
                f"queue {queue['id']} supervisor {queue['pid']} is "
                f"{STALLED_PROCESS_STATES[queue['state']]} and cannot advance the study"
            )
    for benchmark, progress in external.items():
        for defect in progress["malformed"]:
            warnings.append(f"{benchmark} {defect}")
        if progress["terminal_failures"]:
            warnings.append(
                f"{benchmark} has {progress['terminal_failures']} terminal failures"
            )
    if disk.free < 100 * 1024**3:
        warnings.append(f"filesystem free space is below 100 GiB: {disk.free} bytes")
    publication_complete = bool(publication and publication.get("status") == "complete")
    status = (
        "complete" if publication_complete else ("attention" if warnings else "running")
    )
    return {
        "schema_version": "1.0.0",
        "generated_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "study_id": manifest["study_id"],
        "status": status,
        "publication_complete": publication_complete,
        "warnings": warnings,
        "disk": {
            "total_bytes": disk.total,
            "used_bytes": disk.used,
            "free_bytes": disk.free,
        },
        "pilot": pilot_progress(root),
        "tau_voice": {
            "observed": tau_observed,
            "expected": tau_expected,
            "archives": archives,
            "archives_expected": archives_expected,
            "matrices_complete": sum(
                item.get("report_complete", False) for item in tau_matrices
            ),
            "matrices_expected": len(tau_matrices),
            "matrices": tau_matrices,
        },
        "external": external,
        "queues": queues,
    }


def atomic_write(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    temporary.write_text(
        json.dumps(payload, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    os.replace(temporary, path)


def main() -> int:
    args = arguments()
    root = args.repository_root.resolve()
    manifest_path = resolve(root, args.manifest)
    payload = build_progress(root, manifest_path)
    if args.output is None:
        print(json.dumps(payload, indent=2, sort_keys=True, allow_nan=False))
    else:
        atomic_write(resolve(root, args.output), payload)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
