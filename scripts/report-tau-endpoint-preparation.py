#!/usr/bin/env python3
"""Build a strict paired report for the tau-Voice preparation-policy ablation.

The report is emitted only after both complete matrix reports exist, their
frozen runtime profiles match, and the only declared system difference is the
preparation policy and its descriptive trigger text. Differences are
descriptive: the first complete pair has one fixed-order trial per task.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


class PairedReportError(RuntimeError):
    """Raised when the two conditions are not a complete controlled pair."""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", required=True, type=Path)
    parser.add_argument("--manifest", required=True, type=Path)
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def load_json(path: Path) -> dict[str, Any]:
    if not path.is_file():
        raise PairedReportError(f"missing required artifact: {path}")
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise PairedReportError(f"artifact must contain a JSON object: {path}")
    return value


def require_equal(actual: Any, expected: Any, label: str) -> None:
    if actual != expected:
        raise PairedReportError(
            f"{label}: expected {expected!r}, found {actual!r}"
        )


def numeric_difference(left: Any, right: Any) -> Any:
    """Return left-right for common finite numeric leaves in two structures."""
    if isinstance(left, dict) and isinstance(right, dict):
        result = {
            key: numeric_difference(left[key], right[key])
            for key in sorted(left.keys() & right.keys())
        }
        return {key: value for key, value in result.items() if value is not None}
    if (
        type(left) in (int, float)
        and type(right) in (int, float)
        and math.isfinite(left)
        and math.isfinite(right)
    ):
        return left - right
    return None


def scale_numeric(value: Any, denominator: int) -> Any:
    if isinstance(value, dict):
        return {key: scale_numeric(item, denominator) for key, item in value.items()}
    if type(value) in (int, float):
        return value / denominator
    raise PairedReportError("runtime delta contains a non-numeric leaf")


def cell_signature(matrix: dict[str, Any]) -> list[dict[str, Any]]:
    return [
        {
            "speech_complexity": cell["speech_complexity"],
            "voice_registry": cell["voice_registry"],
            "status": cell["status"],
        }
        for cell in matrix["cells"]
    ]


def validate_pair_invariants(
    continuous: dict[str, Any], endpoint: dict[str, Any]
) -> None:
    for field in ("benchmark", "transport"):
        require_equal(endpoint[field], continuous[field], f"matrix {field}")
    for field in (
        "asr",
        "fast",
        "slow",
        "tool_resumption",
        "agent_speech",
        "caller_speech",
    ):
        require_equal(
            endpoint["system"][field],
            continuous["system"][field],
            f"system.{field}",
        )
    for field in (
        "slow_context",
        "requires_local_fast",
        "speech",
        "asr",
        "gateway_profiles",
    ):
        require_equal(
            endpoint["runtime_requirements"][field],
            continuous["runtime_requirements"][field],
            f"runtime_requirements.{field}",
        )
    require_equal(cell_signature(endpoint), cell_signature(continuous), "cell design")
    require_equal(
        continuous["runtime_requirements"]["preparation_policy"],
        "continuous",
        "continuous policy",
    )
    require_equal(
        endpoint["runtime_requirements"]["preparation_policy"],
        "endpoint-only",
        "endpoint policy",
    )


def validate_health(
    health: dict[str, Any], matrix: dict[str, Any], label: str
) -> None:
    requirements = matrix["runtime_requirements"]
    require_equal(
        health.get("preparation_policy"),
        requirements["preparation_policy"],
        f"{label} preparation policy",
    )
    require_equal(
        health.get("slow_context"),
        requirements["slow_context"],
        f"{label} slow context",
    )
    for field in ("model", "provider_chunk_ms", "provider_max_chunk_ms", "strategy"):
        require_equal(
            health.get("asr", {}).get(field),
            requirements["asr"][field],
            f"{label} ASR {field}",
        )
    for phase in ("fast", "slow"):
        for field in ("provider", "model", "effort", "tool_authority"):
            require_equal(
                health.get(phase, {}).get(field),
                requirements["gateway_profiles"][phase][field],
                f"{label} {phase} {field}",
            )
    for field in ("name", "version"):
        require_equal(
            health.get("speech", {}).get(field),
            requirements["speech"][field],
            f"{label} speech {field}",
        )


def validate_matrix_report(
    repository_root: Path,
    condition: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any], Path, dict[str, Any]]:
    matrix_path = repository_root / condition["matrix"]
    matrix = load_json(matrix_path)
    require_equal(
        matrix["runtime_requirements"]["preparation_policy"],
        condition["preparation_policy"],
        f"{condition['id']} manifest policy",
    )
    report_path = (
        repository_root
        / ".runtime/benchmark-runs/tau-voice"
        / matrix["matrix_id"]
        / "report.json"
    )
    report = load_json(report_path)
    require_equal(report.get("status"), "complete", f"{condition['id']} report")
    require_equal(
        report.get("matrix", {}).get("id"),
        matrix["matrix_id"],
        f"{condition['id']} matrix ID",
    )
    require_equal(
        report.get("matrix", {}).get("sha256"),
        sha256_file(matrix_path),
        f"{condition['id']} matrix hash",
    )
    expected_cells = [cell["id"] for cell in matrix["cells"]]
    require_equal(
        report.get("matrix", {}).get("selected_cells"),
        expected_cells,
        f"{condition['id']} selected cells",
    )
    require_equal(
        sorted(report.get("cells", {})),
        sorted(expected_cells),
        f"{condition['id']} reported cells",
    )
    completed_evidence = [
        item
        for item in report.get("execution_evidence", [])
        if item.get("status") == "complete" and item.get("selected_cell") is None
    ]
    require_equal(
        len(completed_evidence), 1, f"{condition['id']} complete all-cell run"
    )
    evidence = completed_evidence[0]
    if not isinstance(evidence.get("gateway_runtime_delta"), dict):
        raise PairedReportError(
            f"{condition['id']} complete run lacks provider runtime deltas"
        )
    for boundary in ("gateway_health_start", "gateway_health_final"):
        health = evidence.get(boundary)
        if not isinstance(health, dict):
            raise PairedReportError(
                f"{condition['id']} complete run lacks {boundary}"
            )
        validate_health(health, matrix, f"{condition['id']} {boundary}")
    return matrix, report, report_path, evidence


def metrics_by_speech(
    matrix: dict[str, Any], report: dict[str, Any]
) -> dict[str, dict[str, Any]]:
    return {
        cell["speech_complexity"]: report["cells"][cell["id"]]["overall"]
        for cell in matrix["cells"]
    }


def atomic_write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temporary.open("w", encoding="utf-8") as stream:
        json.dump(payload, stream, indent=2, sort_keys=True, allow_nan=False)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)


def main() -> int:
    arguments = parse_args()
    repository_root = arguments.repository_root.resolve()
    manifest_path = arguments.manifest.resolve()
    manifest = load_json(manifest_path)
    conditions = {item["id"]: item for item in manifest["conditions"]}
    require_equal(
        sorted(conditions),
        ["continuous", "endpoint-only"],
        "preparation conditions",
    )
    continuous_matrix, continuous_report, continuous_path, continuous_evidence = (
        validate_matrix_report(repository_root, conditions["continuous"])
    )
    endpoint_matrix, endpoint_report, endpoint_path, endpoint_evidence = (
        validate_matrix_report(repository_root, conditions["endpoint-only"])
    )
    validate_pair_invariants(continuous_matrix, endpoint_matrix)
    require_equal(
        endpoint_evidence.get("openrealtime_revision"),
        continuous_evidence.get("openrealtime_revision"),
        "paired OpenRealtime revision",
    )
    revision = continuous_evidence.get("openrealtime_revision")
    if not isinstance(revision, str) or not revision:
        raise PairedReportError("paired run lacks an OpenRealtime revision")

    continuous_metrics = metrics_by_speech(continuous_matrix, continuous_report)
    endpoint_metrics = metrics_by_speech(endpoint_matrix, endpoint_report)
    require_equal(
        sorted(endpoint_metrics), sorted(continuous_metrics), "speech populations"
    )
    cell_differences = {
        speech: numeric_difference(
            continuous_metrics[speech], endpoint_metrics[speech]
        )
        for speech in sorted(continuous_metrics)
    }

    benchmark = continuous_matrix["benchmark"]
    planned_simulations = (
        benchmark["total_tasks_per_cell"]
        * benchmark["num_trials"]
        * len(continuous_matrix["cells"])
    )
    continuous_runtime = continuous_evidence["gateway_runtime_delta"]
    endpoint_runtime = endpoint_evidence["gateway_runtime_delta"]
    runtime_difference = numeric_difference(continuous_runtime, endpoint_runtime)
    output = arguments.output
    if output is None:
        output = (
            repository_root
            / ".runtime/benchmark-runs/tau-voice"
            / manifest["ablation_id"]
            / "report.json"
        )
    payload = {
        "schema_version": "1.0.0",
        "status": "complete",
        "generated_at": datetime.now(timezone.utc)
        .isoformat()
        .replace("+00:00", "Z"),
        "ablation": {
            "id": manifest["ablation_id"],
            "path": str(manifest_path.relative_to(repository_root)),
            "sha256": sha256_file(manifest_path),
            "hypothesis": manifest["hypothesis"],
            "execution_order": manifest["execution_order"],
        },
        "comparison": {
            "orientation": "continuous minus endpoint-only",
            "interpretation": {
                "quality": "positive means continuous scored higher",
                "latency": "negative means continuous was faster",
                "provider_work": "negative means continuous used less measured work",
            },
            "inference": "descriptive exact-population difference; one fixed-order trial per task, so no repeated-trial confidence claim",
            "planned_simulations_per_condition": planned_simulations,
            "cell_metric_differences": cell_differences,
            "provider_work": {
                "continuous": continuous_runtime,
                "endpoint_only": endpoint_runtime,
                "continuous_minus_endpoint_only": runtime_difference,
                "continuous_per_planned_simulation": scale_numeric(
                    continuous_runtime, planned_simulations
                ),
                "endpoint_only_per_planned_simulation": scale_numeric(
                    endpoint_runtime, planned_simulations
                ),
            },
        },
        "evidence": {
            "openrealtime_revision": revision,
            "continuous_report": {
                "path": str(continuous_path.relative_to(repository_root)),
                "sha256": sha256_file(continuous_path),
            },
            "endpoint_report": {
                "path": str(endpoint_path.relative_to(repository_root)),
                "sha256": sha256_file(endpoint_path),
            },
        },
        "limitations": [
            manifest["analysis"]["order_limitation"],
            "raw metric differences retain upstream field semantics and are not combined into a private composite score",
        ],
    }
    atomic_write_json(output.resolve(), payload)
    print(f"tau endpoint-preparation report complete: {output.resolve()}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except PairedReportError as error:
        print(f"incomplete paired ablation; no comparison: {error}", file=sys.stderr)
        raise SystemExit(1)
