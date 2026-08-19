#!/usr/bin/env python3

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
from typing import Any


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


SNAPSHOT_FIELDS = (
    "host_boot_id",
    "expected_components",
    "component_roots",
    "gpu_uuids",
    "processes",
)
PROCESS_FIELDS = (
    "gpu_uuid",
    "component",
    "component_pid",
    "pid",
    "proc_start_time_ticks",
)


def process_identity(snapshot: dict[str, Any]) -> list[dict[str, Any]]:
    return [
        {key: process[key] for key in PROCESS_FIELDS}
        for process in snapshot["processes"]
    ]


def require_snapshot(snapshot: Any, label: str) -> dict[str, Any]:
    """Return `snapshot` once it records everything this summary reads.

    The checks in main() compare each snapshot against the first one, and two
    snapshots that both omit a field agree vacuously. Without this, a guard log
    that never recorded which host, device, or components it watched would pass
    every "did not change" check and be summarised as proof of exclusive GPU
    ownership. The requirements below are exactly what
    capture-local-gpu-ownership.sh already refuses to emit without, so they
    cannot reject an honest capture.
    """
    require(isinstance(snapshot, dict), f"{label} is not a record")
    for name in SNAPSHOT_FIELDS:
        require(name in snapshot, f"{label} recorded no {name}")
    processes = snapshot["processes"]
    require(
        isinstance(processes, list) and bool(processes),
        f"{label} claims exclusive ownership over no GPU processes",
    )
    for index, process in enumerate(processes):
        require(isinstance(process, dict), f"{label} process {index} is not a record")
        for name in PROCESS_FIELDS:
            require(name in process, f"{label} process {index} recorded no {name}")
    components = snapshot["expected_components"]
    require(
        isinstance(components, list) and bool(components),
        f"{label} expects no components",
    )
    require(
        sorted({process["component"] for process in processes}) == sorted(components),
        f"{label} does not occupy every expected component",
    )
    require(
        isinstance(snapshot["gpu_uuids"], list) and bool(snapshot["gpu_uuids"]),
        f"{label} recorded no GPU device",
    )
    return snapshot


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--log", required=True, type=Path)
    parser.add_argument("--repository-root", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    arguments = parser.parse_args()
    log = arguments.log.resolve()
    root = arguments.repository_root.resolve()
    output = arguments.output.resolve()
    require(log.is_file(), f"GPU ownership log is missing: {log}")
    require(not Path(str(log) + ".active").exists(), "GPU ownership guard is active")
    records = [
        json.loads(line) for line in log.read_text(encoding="utf-8").splitlines()
    ]
    require(len(records) >= 3, "GPU ownership evidence is incomplete")
    require(
        all(isinstance(record, dict) for record in records),
        "GPU ownership evidence contains a record that is not an object",
    )
    require(records[0].get("type") == "guard.started", "GPU guard start is absent")
    for name in ("interval_seconds", "recorded_at"):
        require(name in records[0], f"GPU guard start recorded no {name}")
    require("recorded_at" in records[-1], "GPU guard completion recorded no recorded_at")
    require(
        records[-1].get("type") == "guard.completed"
        and records[-1].get("status") == "complete"
        and records[-1].get("exit_status") == 0,
        "GPU guard did not complete cleanly",
    )
    checks = records[1:-1]
    require(bool(checks), "GPU ownership evidence has no checks")
    require(
        all(
            item.get("type") == "guard.check" and item.get("status") == "ok"
            for item in checks
        ),
        "GPU ownership evidence contains a violation",
    )
    snapshots = [
        require_snapshot(item.get("ownership"), f"GPU check {index} ownership")
        for index, item in enumerate(checks)
    ]
    capture_timeout_seconds = records[0].get("capture_timeout_seconds")
    require(
        isinstance(capture_timeout_seconds, int)
        and not isinstance(capture_timeout_seconds, bool)
        and capture_timeout_seconds > 0,
        "GPU guard capture timeout is invalid",
    )
    first = snapshots[0]
    for index, snapshot in enumerate(snapshots):
        require(snapshot.get("schema_version") == "1.0.0", f"GPU check {index} schema")
        require(
            snapshot.get("exclusive") is True, f"GPU check {index} is not exclusive"
        )
        require(
            snapshot.get("host_boot_id") == first.get("host_boot_id"),
            f"GPU check {index} boot changed",
        )
        require(
            snapshot.get("expected_components") == first.get("expected_components"),
            f"GPU check {index} component set changed",
        )
        require(
            snapshot.get("component_roots") == first.get("component_roots"),
            f"GPU check {index} component roots changed",
        )
        require(
            snapshot.get("gpu_uuids") == first.get("gpu_uuids"),
            f"GPU check {index} device changed",
        )
        require(
            process_identity(snapshot) == process_identity(first),
            f"GPU check {index} process identity changed",
        )

    try:
        relative_log = log.relative_to(root).as_posix()
    except ValueError as error:
        raise RuntimeError(
            "GPU ownership log is outside the repository root"
        ) from error
    payload = {
        "schema_version": "1.0.0",
        "status": "complete",
        "interval_seconds": records[0]["interval_seconds"],
        "capture_timeout_seconds": capture_timeout_seconds,
        "started_at": records[0]["recorded_at"],
        "completed_at": records[-1]["recorded_at"],
        "checks": len(checks),
        "host_boot_id": first["host_boot_id"],
        "expected_components": first["expected_components"],
        "component_roots": first["component_roots"],
        "gpu_uuids": first["gpu_uuids"],
        "processes": process_identity(first),
        "log": {
            "path": relative_log,
            "sha256": sha256_file(log),
            "bytes": log.stat().st_size,
        },
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = output.with_name(f".{output.name}.{os.getpid()}.tmp")
    temporary.write_text(
        json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    os.replace(temporary, output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
