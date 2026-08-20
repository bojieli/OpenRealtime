#!/usr/bin/env python3
"""Publish provenance-bound post-hoc analysis of the complete voice study.

The terminal full-study reporter answers whether every frozen population and
native scorer artifact is complete. This tool starts only from that terminal
artifact. It then binds the expanded tau simulation evidence back to the
hashes in each accepted matrix report and publishes:

* native task, interaction, tool-action, and priced-cost outcomes;
* response-latency tails and failure-mechanism classifications;
* matrix-pair provider-work and GPU telemetry summaries; and
* Pareto sets for every preregistered tau ablation and speech population.

No cross-benchmark score is created. Missing semantic-audio labels, monetary
provider invoices, repair annotations, or trajectory-specific labels remain
named measurement limits rather than being inferred from transcript text.
"""

from __future__ import annotations

import argparse
import csv
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import sys
from typing import Any


SCHEMA_VERSION = "1.0.0"
DEFAULT_DEFINITIONS = {
    "benchmarks/tau-voice/asr-ablation-v1.json": "9668023fa902341d4d479cb7cb47a66dfa4843b524fa0eb5f7f65cae034d85b5",
    "benchmarks/tau-voice/cadence-ablation-v1.json": "ef870c08e529654557c8b8d69df61bb11fd4c28ad27d815e06d5876fd051d8f4",
    "benchmarks/tau-voice/context-projection-ablation-v1.json": "f8fbc99fc02f54c3ea34fa5a366bd17f8a57c4ce0061abfa2761825b3551fd28",
    "benchmarks/tau-voice/endpoint-preparation-ablation-v1.json": "cad7226b3ea4e6112f879f383c83a3e624541761e2dc82bf5f7d4359f68876fb",
    "benchmarks/tau-voice/event-adaptive-ablation-v1.json": "bf3d4bd42e216956622885c1f21e9941fbefbc91d71eb96dbd38af08815c8070",
    "benchmarks/tau-voice/fast-ablation-v1.json": "df5ebeb3a226b9b2c4c21c4b4add1765f7df95693a034c42e0382ff0413c76b6",
    "benchmarks/tau-voice/slow-effort-ablation-v1.json": "65338c23751b2c1538f306f7c5861e1d147c09cad8dc265aca2b4652db9fb6fb",
}
INTERACTION_DIRECTIONS = {
    "response_latency_mean": "minimize",
    "yield_latency_mean": "minimize",
    "response_rate": "maximize",
    "yield_rate": "maximize",
    "agent_interruption_rate": "minimize",
    "selectivity_backchannel": "maximize",
    "selectivity_vocal_tic": "maximize",
    "selectivity_non_directed": "maximize",
}
AXIS_DIRECTIONS = {
    "pass_at_1": "maximize",
    "action_correctness_rate": "maximize",
    "inconsistent_behavior_errors_per_reviewed_simulation": "minimize",
    "tau_priced_agent_cost_proxy": "minimize",
    "matrix_continuation_total_tokens": "minimize",
    "matrix_provider_boundary_elapsed_ns": "minimize",
    "matrix_continuation_invocations": "minimize",
    "matrix_asr_provider_advances": "minimize",
    **INTERACTION_DIRECTIONS,
}
FRONTIER_PANELS = {
    "task_response": (
        "pass_at_1",
        "response_latency_mean",
        "response_rate",
    ),
    "task_yield": (
        "pass_at_1",
        "yield_latency_mean",
        "yield_rate",
    ),
    "task_selectivity": (
        "pass_at_1",
        "selectivity_backchannel",
        "selectivity_vocal_tic",
        "selectivity_non_directed",
    ),
    "task_tool_correctness": ("pass_at_1", "action_correctness_rate"),
    "task_trajectory_consistency": (
        "pass_at_1",
        "inconsistent_behavior_errors_per_reviewed_simulation",
    ),
    "task_compute": (
        "pass_at_1",
        "matrix_continuation_total_tokens",
        "matrix_provider_boundary_elapsed_ns",
    ),
    "task_cost": ("pass_at_1", "tau_priced_agent_cost_proxy"),
    "observed_joint": (
        "pass_at_1",
        "response_latency_mean",
        "response_rate",
        "action_correctness_rate",
        "matrix_continuation_total_tokens",
        "tau_priced_agent_cost_proxy",
    ),
}
CONTINUATION_CLASSES = ("fast", "slow", "fast_preparation", "slow_preparation")
GPU_COLUMNS = (
    "timestamp",
    "index",
    "name",
    "memory_used_mib",
    "utilization_gpu_percent",
    "power_draw_watts",
    "loadavg",
)
TAU_UNDEFINED_INTERACTION_REASON = (
    "the native tau interaction scorer reported no defined value for this population"
)


class FullStudyAnalysisError(RuntimeError):
    """Raised when complete, provenance-bound evidence cannot be established."""


def load_peer_script(filename: str, name: str) -> Any:
    path = Path(__file__).with_name(filename)
    specification = importlib.util.spec_from_file_location(name, path)
    if specification is None or specification.loader is None:
        raise RuntimeError(f"cannot import peer analysis script {path}")
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


TAILS = load_peer_script("analyze-tau-voice-tails.py", "tau_voice_tail_analysis")
FAILURES = load_peer_script(
    "classify-tau-voice-failures.py", "tau_voice_failure_classification"
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", type=Path, default=Path.cwd())
    parser.add_argument(
        "--full-study-report",
        type=Path,
        default=Path(".runtime/benchmark-runs/full-study-v1/report.json"),
    )
    parser.add_argument(
        "--definition",
        action="append",
        type=Path,
        help="ablation definition; repeatable (defaults cover all seven tau analyses)",
    )
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise FullStudyAnalysisError(message)


def require_equal(actual: Any, expected: Any, label: str) -> None:
    if actual != expected:
        raise FullStudyAnalysisError(
            f"{label}: expected {expected!r}, found {actual!r}"
        )


def read_json(path: Path, label: str) -> dict[str, Any]:
    require(path.is_file(), f"{label} is missing: {path}")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise FullStudyAnalysisError(f"cannot read {label} {path}: {error}") from error
    require(isinstance(value, dict), f"{label} must be a JSON object: {path}")
    return value


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def resolve(root: Path, path: Path | str) -> Path:
    value = Path(path)
    return value.resolve() if value.is_absolute() else (root / value).resolve()


def display_path(root: Path, path: Path) -> str:
    try:
        return str(path.resolve().relative_to(root.resolve()))
    except ValueError:
        return str(path.resolve())


def artifact(root: Path, path: Path) -> dict[str, Any]:
    require(path.is_file(), f"artifact is missing: {path}")
    return {
        "path": display_path(root, path),
        "sha256": sha256_file(path),
        "bytes": path.stat().st_size,
    }


def verify_artifact(root: Path, declaration: Any, label: str) -> Path:
    require(isinstance(declaration, dict), f"{label} declaration is absent")
    path_value = declaration.get("path")
    digest = declaration.get("sha256")
    size = declaration.get("bytes")
    require(isinstance(path_value, str) and path_value, f"{label} path is absent")
    require(isinstance(digest, str) and len(digest) == 64, f"{label} hash is invalid")
    require(isinstance(size, int) and size > 0, f"{label} byte count is invalid")
    path = resolve(root, path_value)
    require(path.is_file(), f"{label} is missing: {path}")
    require_equal(path.stat().st_size, size, f"{label} byte count")
    require_equal(sha256_file(path), digest, f"{label} SHA-256")
    return path


def finite_number(value: Any, label: str) -> float:
    require(
        type(value) in (int, float) and math.isfinite(value),
        f"{label} must be a finite number",
    )
    return float(value)


def nonnegative_number(value: Any, label: str) -> float:
    numeric = finite_number(value, label)
    require(numeric >= 0, f"{label} must be non-negative")
    return numeric


def nearest_rank(ordered: list[float], fraction: float) -> float:
    rank = max(1, math.ceil(fraction * len(ordered)))
    return ordered[min(rank, len(ordered)) - 1]


def distribution(values: list[float]) -> dict[str, Any]:
    require(bool(values), "cannot summarize an empty distribution")
    ordered = sorted(values)
    mean = sum(ordered) / len(ordered)
    variance = sum((value - mean) ** 2 for value in ordered) / len(ordered)
    return {
        "observations": len(ordered),
        "mean": mean,
        "standard_deviation": math.sqrt(variance),
        "min": ordered[0],
        "p50": nearest_rank(ordered, 0.50),
        "p90": nearest_rank(ordered, 0.90),
        "p95": nearest_rank(ordered, 0.95),
        "p99": nearest_rank(ordered, 0.99),
        "max": ordered[-1],
    }


def population_evidence(directory: Path) -> dict[str, Any]:
    files = [directory / "results.json"] + sorted(
        (directory / "simulations").glob("*.json")
    )
    require(files[0].is_file(), f"population has no results.json: {directory}")
    digest = hashlib.sha256()
    total_bytes = 0
    for path in files:
        require(path.is_file(), f"population evidence is missing: {path}")
        relative = path.relative_to(directory).as_posix()
        size = path.stat().st_size
        file_digest = sha256_file(path)
        total_bytes += size
        digest.update(relative.encode())
        digest.update(b"\0")
        digest.update(str(size).encode())
        digest.update(b"\0")
        digest.update(file_digest.encode())
        digest.update(b"\n")
    return {
        "algorithm": "sha256(path\\0size\\0file_sha256\\n)",
        "digest": digest.hexdigest(),
        "files": len(files),
        "bytes": total_bytes,
        "metadata_sha256": sha256_file(directory / "results.json"),
    }


def review_coverage(directory: Path, label: str) -> dict[str, int]:
    """Count typed review records without treating ``none`` as a review.

    tau2's aggregate ``sims_by_max_agent_severity`` map uses ``none`` both for
    an explicitly reviewed simulation with no agent errors and for a
    user-only/unreviewed simulation.  The raw archive is therefore the only
    truthful source for review coverage.
    """
    results = read_json(directory / "results.json", f"{label} results")
    index = results.get("simulation_index")
    require(isinstance(index, list) and index, f"{label} has no simulation index")
    counts = {
        "agent_reviewed_simulations": 0,
        "user_only_reviewed_simulations": 0,
        "unreviewed_simulations": 0,
    }
    seen: set[str] = set()
    for entry in index:
        identifier = entry.get("id")
        require(
            isinstance(identifier, str) and identifier and identifier not in seen,
            f"{label} has an invalid or duplicate simulation ID",
        )
        seen.add(identifier)
        path = directory / "simulations" / f"{identifier}.json"
        simulation = read_json(path, f"{label}/{identifier} simulation")
        if simulation.get("review") is not None:
            counts["agent_reviewed_simulations"] += 1
        elif simulation.get("user_only_review") is not None:
            counts["user_only_reviewed_simulations"] += 1
        else:
            counts["unreviewed_simulations"] += 1
    require_equal(
        sum(counts.values()),
        len(index),
        f"{label} review coverage reconciliation",
    )
    return counts


def summarize_gpu_telemetry(root: Path, declaration: Any, label: str) -> dict[str, Any]:
    path = verify_artifact(root, declaration, label)
    by_gpu: dict[str, dict[str, Any]] = {}
    with path.open("r", encoding="utf-8", newline="") as stream:
        reader = csv.DictReader(stream)
        require_equal(tuple(reader.fieldnames or ()), GPU_COLUMNS, f"{label} columns")
        for row_number, row in enumerate(reader, start=2):
            require(None not in row, f"{label} row {row_number} has extra columns")
            index = str(row.get("index", "")).strip()
            name = str(row.get("name", "")).strip()
            timestamp = str(row.get("timestamp", "")).strip()
            require(
                index and name and timestamp, f"{label} row {row_number} is incomplete"
            )
            try:
                datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
            except ValueError as error:
                raise FullStudyAnalysisError(
                    f"{label} row {row_number} timestamp is invalid"
                ) from error
            numeric = {}
            for field in (
                "memory_used_mib",
                "utilization_gpu_percent",
                "power_draw_watts",
            ):
                try:
                    numeric[field] = float(str(row[field]).strip())
                except (TypeError, ValueError) as error:
                    raise FullStudyAnalysisError(
                        f"{label} row {row_number} {field} is invalid"
                    ) from error
                require(
                    math.isfinite(numeric[field]) and numeric[field] >= 0,
                    f"{label} row {row_number} {field} is invalid",
                )
            require(
                numeric["utilization_gpu_percent"] <= 100,
                f"{label} row {row_number} utilization exceeds 100 percent",
            )
            load_values = str(row.get("loadavg", "")).split("/")
            require(
                len(load_values) == 3, f"{label} row {row_number} loadavg is invalid"
            )
            try:
                load = [float(value) for value in load_values]
            except ValueError as error:
                raise FullStudyAnalysisError(
                    f"{label} row {row_number} loadavg is invalid"
                ) from error
            require(
                all(math.isfinite(value) and value >= 0 for value in load),
                f"{label} row {row_number} loadavg is invalid",
            )
            entry = by_gpu.setdefault(
                index,
                {
                    "name": name,
                    "memory_used_mib": [],
                    "utilization_gpu_percent": [],
                    "power_draw_watts": [],
                    "loadavg_1m": [],
                    "loadavg_5m": [],
                    "loadavg_15m": [],
                },
            )
            require_equal(entry["name"], name, f"{label} GPU {index} name")
            for field, value in numeric.items():
                entry[field].append(value)
            entry["loadavg_1m"].append(load[0])
            entry["loadavg_5m"].append(load[1])
            entry["loadavg_15m"].append(load[2])
    require(bool(by_gpu), f"{label} contains no telemetry samples")
    return {
        "artifact": artifact(root, path),
        "sampling_interval_seconds": 5,
        "gpus": {
            index: {
                "name": values["name"],
                **{
                    field: distribution(sample)
                    for field, sample in values.items()
                    if field != "name"
                },
            }
            for index, values in sorted(by_gpu.items())
        },
    }


def runtime_summary(runtime: Any, label: str) -> dict[str, Any]:
    require(isinstance(runtime, dict), f"{label} runtime delta is absent")
    phases = {}
    for phase in CONTINUATION_CLASSES:
        value = runtime.get(phase)
        require(isinstance(value, dict), f"{label} runtime has no {phase} class")
        required = (
            "invocations",
            "completed",
            "failed",
            "cancelled",
            "events",
            "cumulative_first_event_ns",
            "cumulative_elapsed_ns",
            "input_tokens",
            "output_tokens",
            "reasoning_tokens",
            "total_tokens",
        )
        phases[phase] = {
            field: nonnegative_number(value.get(field), f"{label} {phase}.{field}")
            for field in required
        }
        if "cached_input_tokens" in value or "cached_input_reports" in value:
            require(
                "cached_input_tokens" in value and "cached_input_reports" in value,
                f"{label} {phase} cache accounting is only partially declared",
            )
            phases[phase]["cached_input_tokens"] = nonnegative_number(
                value["cached_input_tokens"], f"{label} {phase}.cached_input_tokens"
            )
            phases[phase]["cached_input_reports"] = nonnegative_number(
                value["cached_input_reports"], f"{label} {phase}.cached_input_reports"
            )

    speech = runtime.get("speech")
    require(isinstance(speech, dict), f"{label} runtime has no speech class")
    speech_fields = (
        "invocations",
        "completed",
        "failed",
        "cancelled",
        "chunks",
        "samples",
        "cumulative_first_chunk_ns",
        "cumulative_elapsed_ns",
    )
    speech_summary = {
        field: nonnegative_number(speech.get(field), f"{label} speech.{field}")
        for field in speech_fields
    }
    asr_advances = nonnegative_number(
        runtime.get("asr_provider_advances"), f"{label} asr_provider_advances"
    )
    asr_elapsed = nonnegative_number(
        runtime.get("asr_provider_elapsed_ns"), f"{label} asr_provider_elapsed_ns"
    )
    continuation_totals = {
        field: sum(phases[phase][field] for phase in CONTINUATION_CLASSES)
        for field in (
            "invocations",
            "completed",
            "failed",
            "cancelled",
            "events",
            "cumulative_first_event_ns",
            "cumulative_elapsed_ns",
            "input_tokens",
            "output_tokens",
            "reasoning_tokens",
            "total_tokens",
        )
    }
    cache_available = all(
        "cached_input_tokens" in phases[phase] for phase in CONTINUATION_CLASSES
    )
    cache = (
        {
            "cached_input_tokens": sum(
                phases[phase]["cached_input_tokens"] for phase in CONTINUATION_CLASSES
            ),
            "cached_input_reports": sum(
                phases[phase]["cached_input_reports"] for phase in CONTINUATION_CLASSES
            ),
        }
        if cache_available
        else {
            "status": "not_measured",
            "reason": "the frozen gateway predates provider-reported cached-input counters",
        }
    )
    return {
        "measurement_scope": (
            "one all-cell matrix invocation covering both speech populations; "
            "provider-boundary elapsed time is not GPU kernel time"
        ),
        "asr": {
            "provider_advances": asr_advances,
            "provider_elapsed_ns": asr_elapsed,
        },
        "continuation_classes": phases,
        "continuation_totals": continuation_totals,
        "speech": speech_summary,
        "provider_boundary_elapsed_ns": (
            asr_elapsed
            + continuation_totals["cumulative_elapsed_ns"]
            + speech_summary["cumulative_elapsed_ns"]
        ),
        "cache_reuse": cache,
    }


def cell_metrics(
    overall: Any,
    population: int,
    label: str,
    *,
    review_counts: dict[str, int],
) -> dict[str, Any]:
    require(isinstance(overall, dict), f"{label} has no overall metrics")
    agent = overall.get("agent_metrics")
    interaction = overall.get("interaction_metrics")
    require(isinstance(agent, dict), f"{label} has no agent metrics")
    require(isinstance(interaction, dict), f"{label} has no interaction metrics")
    pass_values = agent.get("pass_hat_ks")
    require(isinstance(pass_values, dict), f"{label} has no pass^k metrics")
    pass_at_1 = pass_values.get("1", pass_values.get(1))
    quality = {
        "avg_reward": finite_number(agent.get("avg_reward"), f"{label} avg_reward"),
        "pass_at_1": finite_number(pass_at_1, f"{label} pass^1"),
    }
    cost = finite_number(agent.get("avg_agent_cost"), f"{label} avg_agent_cost")

    available_interaction = {}
    not_measured = {}
    for metric in INTERACTION_DIRECTIONS:
        value = interaction.get(metric)
        if type(value) in (int, float) and math.isfinite(value):
            available_interaction[metric] = float(value)
        else:
            not_measured[metric] = TAU_UNDEFINED_INTERACTION_REASON

    action_fields = (
        "total_read_actions",
        "correct_read_actions",
        "total_write_actions",
        "correct_write_actions",
    )
    action_counts = {}
    for field in action_fields:
        value = agent.get(field)
        require(
            isinstance(value, int) and not isinstance(value, bool) and value >= 0,
            f"{label} {field} is not a non-negative integer",
        )
        action_counts[field] = value
    total_actions = (
        action_counts["total_read_actions"] + action_counts["total_write_actions"]
    )
    correct_actions = (
        action_counts["correct_read_actions"] + action_counts["correct_write_actions"]
    )
    require(
        correct_actions <= total_actions,
        f"{label} correct actions exceed total actions",
    )
    tools: dict[str, Any] = {
        **action_counts,
        "total_actions": total_actions,
        "correct_actions": correct_actions,
    }
    if total_actions:
        tools["action_correctness_rate"] = correct_actions / total_actions
    else:
        not_measured["action_correctness_rate"] = (
            "the native task scorer recorded no checked read or write action"
        )

    severity = agent.get("sims_by_max_agent_severity")
    tags = agent.get("agent_error_tags_by_severity")
    require(isinstance(severity, dict), f"{label} has no agent-review coverage map")
    require(isinstance(tags, dict), f"{label} has no agent error-tag map")
    reviewed = 0
    for name, count in severity.items():
        require(
            isinstance(name, str)
            and isinstance(count, int)
            and not isinstance(count, bool)
            and count >= 0,
            f"{label} has invalid review coverage",
        )
    # The aggregate map is retained as native evidence below, but its ``none``
    # bucket is not used for coverage because it also includes simulations with
    # no agent review record.
    require_equal(
        sum(review_counts.values()), population, f"{label} review population"
    )
    reviewed = review_counts["agent_reviewed_simulations"]
    require(reviewed <= population, f"{label} review coverage exceeds its population")
    inconsistent = 0
    if "inconsistent_behavior" in tags:
        value = tags["inconsistent_behavior"]
        require(isinstance(value, dict), f"{label} inconsistent tag has no severities")
        for count in value.values():
            require(
                isinstance(count, int) and not isinstance(count, bool) and count >= 0,
                f"{label} inconsistent tag count is invalid",
            )
            inconsistent += count
    consistency: dict[str, Any] = {
        "reviewed_simulations": reviewed,
        "population": population,
        "review_coverage_rate": reviewed / population,
        "inconsistent_behavior_errors": inconsistent,
        "user_only_reviewed_simulations": review_counts[
            "user_only_reviewed_simulations"
        ],
        "unreviewed_simulations": review_counts["unreviewed_simulations"],
        "coverage_source": "raw simulation review records",
    }
    if reviewed:
        consistency["inconsistent_behavior_errors_per_reviewed_simulation"] = (
            inconsistent / reviewed
        )
    else:
        not_measured["inconsistent_behavior_errors_per_reviewed_simulation"] = (
            "the native tau report contains no agent-review coverage"
        )

    axes = {
        "pass_at_1": quality["pass_at_1"],
        "tau_priced_agent_cost_proxy": cost,
        **available_interaction,
    }
    if "action_correctness_rate" in tools:
        axes["action_correctness_rate"] = tools["action_correctness_rate"]
    consistency_axis = consistency.get(
        "inconsistent_behavior_errors_per_reviewed_simulation"
    )
    if consistency_axis is not None:
        axes["inconsistent_behavior_errors_per_reviewed_simulation"] = consistency_axis
    return {
        "quality": quality,
        "interaction": available_interaction,
        "tool_action_checks": tools,
        "trajectory_consistency": consistency,
        "cost": {
            "tau_priced_agent_cost_proxy": cost,
            "scope": (
                "tau compatibility-model pricing ledger; not local Qwen, Gemini, "
                "Fish, electricity, or provider-billed spend"
            ),
        },
        "not_measured": not_measured,
        "axes": axes,
    }


def add_compute_axes(axes: dict[str, float], compute: dict[str, Any]) -> None:
    totals = compute["continuation_totals"]
    axes["matrix_continuation_total_tokens"] = totals["total_tokens"]
    axes["matrix_provider_boundary_elapsed_ns"] = compute[
        "provider_boundary_elapsed_ns"
    ]
    axes["matrix_continuation_invocations"] = totals["invocations"]
    axes["matrix_asr_provider_advances"] = compute["asr"]["provider_advances"]


def published_tau_overall(overall: Any, label: str) -> dict[str, Any]:
    """Reproduce the terminal panel's lossless representation of native nulls."""
    require(isinstance(overall, dict), f"{label} has no overall metrics")
    interaction = overall.get("interaction_metrics")
    require(isinstance(interaction, dict), f"{label} has no interaction metrics")
    return {
        **overall,
        "interaction_metrics": {
            name: value for name, value in interaction.items() if value is not None
        },
        "interaction_metrics_not_measured": {
            name: TAU_UNDEFINED_INTERACTION_REASON
            for name, value in interaction.items()
            if value is None
        },
    }


def analyze_matrix(root: Path, panel: dict[str, Any]) -> dict[str, Any]:
    matrix_id = panel.get("matrix_id")
    require(isinstance(matrix_id, str) and matrix_id, "tau matrix panel has no ID")
    require_equal(
        panel.get("infrastructure_errors"), 0, f"{matrix_id} infrastructure errors"
    )
    matrix_path = verify_artifact(root, panel.get("matrix"), f"{matrix_id} matrix")
    report_path = verify_artifact(root, panel.get("report"), f"{matrix_id} report")
    matrix = read_json(matrix_path, f"{matrix_id} matrix")
    report = read_json(report_path, f"{matrix_id} report")
    require_equal(matrix.get("matrix_id"), matrix_id, f"{matrix_id} matrix ID")
    require_equal(report.get("status"), "complete", f"{matrix_id} report status")
    require_equal(
        report.get("matrix", {}).get("id"), matrix_id, f"{matrix_id} report ID"
    )
    require_equal(
        report.get("matrix", {}).get("sha256"),
        sha256_file(matrix_path),
        f"{matrix_id} report matrix hash",
    )
    cells = matrix.get("cells")
    domains = matrix.get("benchmark", {}).get("domains")
    trials = matrix.get("benchmark", {}).get("num_trials")
    seed = matrix.get("benchmark", {}).get("seed")
    require(isinstance(cells, list) and cells, f"{matrix_id} has no cells")
    require(isinstance(domains, list) and domains, f"{matrix_id} has no domains")
    require(isinstance(trials, int) and trials > 0, f"{matrix_id} has no trial count")
    require(isinstance(seed, int), f"{matrix_id} has no seed")
    require_equal(
        report.get("matrix", {}).get("selected_cells"),
        [cell["id"] for cell in cells],
        f"{matrix_id} selected cells",
    )
    require_equal(
        set(report.get("cells", {})),
        {cell["id"] for cell in cells},
        f"{matrix_id} report cells",
    )
    require_equal(
        set(panel.get("cells", {})),
        {cell["id"] for cell in cells},
        f"{matrix_id} terminal cells",
    )

    complete_runs = [
        item
        for item in report.get("execution_evidence", [])
        if item.get("status") == "complete" and item.get("selected_cell") is None
    ]
    require_equal(len(complete_runs), 1, f"{matrix_id} complete all-cell run")
    execution = complete_runs[0]
    compute = runtime_summary(execution.get("gateway_runtime_delta"), matrix_id)
    gpu = summarize_gpu_telemetry(
        root, execution.get("gpu_telemetry"), f"{matrix_id} GPU telemetry"
    )

    population_summaries = []
    cell_points = {}
    seen_speech = set()
    for cell in cells:
        cell_id = cell.get("id")
        speech = cell.get("speech_complexity")
        require(
            isinstance(cell_id, str) and cell_id, f"{matrix_id} has an invalid cell"
        )
        require(
            isinstance(speech, str) and speech,
            f"{matrix_id}/{cell_id} has no speech class",
        )
        require(speech not in seen_speech, f"{matrix_id} repeats speech class {speech}")
        seen_speech.add(speech)
        report_cell = report["cells"][cell_id]
        terminal_cell = panel["cells"][cell_id]
        require_equal(
            terminal_cell.get("overall"),
            published_tau_overall(report_cell.get("overall"), f"{matrix_id}/{cell_id}"),
            f"{matrix_id}/{cell_id} terminal metrics",
        )
        require_equal(
            terminal_cell.get("infrastructure_errors"),
            0,
            f"{matrix_id}/{cell_id} terminal infrastructure errors",
        )
        cell_population = 0
        review_counts = {
            "agent_reviewed_simulations": 0,
            "user_only_reviewed_simulations": 0,
            "unreviewed_simulations": 0,
        }
        for domain in domains:
            domain_name = domain.get("name")
            tasks = domain.get("tasks")
            require(
                isinstance(domain_name, str) and domain_name,
                f"{matrix_id} has an invalid domain",
            )
            require(
                isinstance(tasks, int) and tasks > 0,
                f"{matrix_id}/{domain_name} has no tasks",
            )
            expected = tasks * trials
            directory = (
                root
                / ".runtime/tau2-bench/data/simulations"
                / f"{matrix_id}-{cell_id}-{domain_name}-seed{seed}"
            )
            domain_report = report_cell.get("domains", {}).get(domain_name)
            require(
                isinstance(domain_report, dict),
                f"{matrix_id}/{cell_id}/{domain_name} report is absent",
            )
            require_equal(
                population_evidence(directory),
                domain_report.get("evidence"),
                f"{matrix_id}/{cell_id}/{domain_name} population evidence",
            )
            try:
                tails = TAILS.summarize(directory)
                failures = FAILURES.summarize(directory)
            except (TAILS.TailAnalysisError, FAILURES.ClassificationError) as error:
                raise FullStudyAnalysisError(str(error)) from error
            require_equal(
                tails.get("scope", {}).get("complete"),
                True,
                f"{matrix_id}/{cell_id}/{domain_name} tail scope",
            )
            require_equal(
                failures.get("scope", {}).get("complete"),
                True,
                f"{matrix_id}/{cell_id}/{domain_name} failure scope",
            )
            require_equal(
                tails.get("simulations_declared"),
                expected,
                f"{matrix_id}/{cell_id}/{domain_name} tail population",
            )
            require_equal(
                failures.get("simulations"),
                expected,
                f"{matrix_id}/{cell_id}/{domain_name} failure population",
            )
            require_equal(
                failures.get("termination_reasons", {}).get("infrastructure_error", 0),
                0,
                f"{matrix_id}/{cell_id}/{domain_name} terminal infrastructure failures",
            )
            domain_review_counts = review_coverage(
                directory, f"{matrix_id}/{cell_id}/{domain_name}"
            )
            for key, value in domain_review_counts.items():
                review_counts[key] += value
            population_summaries.append(
                {
                    "cell": cell_id,
                    "speech_complexity": speech,
                    "domain": domain_name,
                    "population": expected,
                    "path": display_path(root, directory),
                    "tails": tails,
                    "failure_mechanisms": failures,
                }
            )
            cell_population += expected
        require_equal(
            terminal_cell.get("simulations"),
            cell_population,
            f"{matrix_id}/{cell_id} population",
        )
        metrics = cell_metrics(
            report_cell.get("overall"),
            cell_population,
            f"{matrix_id}/{cell_id}",
            review_counts=review_counts,
        )
        add_compute_axes(metrics["axes"], compute)
        cell_points[speech] = {
            "matrix_id": matrix_id,
            "matrix": artifact(root, matrix_path),
            "report": artifact(root, report_path),
            "cell_id": cell_id,
            "speech_complexity": speech,
            "population": cell_population,
            "metrics": {key: value for key, value in metrics.items() if key != "axes"},
            "axes": metrics["axes"],
        }
    require_equal(
        panel.get("population"),
        sum(point["population"] for point in cell_points.values()),
        f"{matrix_id} terminal population",
    )
    return {
        "matrix_id": matrix_id,
        "matrix_path": display_path(root, matrix_path),
        "matrix": artifact(root, matrix_path),
        "report": artifact(root, report_path),
        "population": panel["population"],
        "runtime": compute,
        "gpu_telemetry": gpu,
        "cells_by_speech_complexity": cell_points,
        "populations": population_summaries,
    }


def definition_conditions(
    definition: dict[str, Any], label: str
) -> list[dict[str, Any]]:
    conditions = definition.get("conditions")
    if isinstance(conditions, list) and conditions:
        result = conditions
    else:
        result = []
        for condition_id in ("baseline", "candidate"):
            condition = definition.get(condition_id)
            require(isinstance(condition, dict), f"{label} has no {condition_id}")
            result.append({"id": condition_id, "role": condition_id, **condition})
    normalized = []
    seen = set()
    for index, condition in enumerate(result):
        condition_id = condition.get("id")
        matrix = condition.get("matrix")
        require(
            isinstance(condition_id, str) and condition_id,
            f"{label} condition {index} has no ID",
        )
        require(condition_id not in seen, f"{label} repeats condition {condition_id}")
        require(
            isinstance(matrix, str) and matrix, f"{label}/{condition_id} has no matrix"
        )
        seen.add(condition_id)
        normalized.append(
            {
                "id": condition_id,
                "role": condition.get("role", condition_id),
                "matrix": matrix,
            }
        )
    require(len(normalized) >= 2, f"{label} has fewer than two conditions")
    return normalized


def dominates(
    left: dict[str, float], right: dict[str, float], axes: tuple[str, ...]
) -> bool:
    strictly_better = False
    for axis in axes:
        direction = AXIS_DIRECTIONS[axis]
        if direction == "maximize":
            if left[axis] < right[axis]:
                return False
            strictly_better = strictly_better or left[axis] > right[axis]
        else:
            if left[axis] > right[axis]:
                return False
            strictly_better = strictly_better or left[axis] < right[axis]
    return strictly_better


def frontier(points: list[dict[str, Any]], axes: tuple[str, ...]) -> dict[str, Any]:
    axis_declaration = [
        {"name": axis, "direction": AXIS_DIRECTIONS[axis]} for axis in axes
    ]
    missing = {
        point["point_id"]: [axis for axis in axes if axis not in point["axes"]]
        for point in points
    }
    missing = {point: axes for point, axes in missing.items() if axes}
    if missing:
        return {
            "status": "not_measured",
            "axes": axis_declaration,
            "reason": "at least one complete condition lacks a required native metric",
            "missing_by_point": missing,
        }
    frontier_ids = []
    dominated_by = {}
    for point in points:
        dominators = [
            other["point_id"]
            for other in points
            if other is not point and dominates(other["axes"], point["axes"], axes)
        ]
        if dominators:
            dominated_by[point["point_id"]] = sorted(dominators)
        else:
            frontier_ids.append(point["point_id"])
    return {
        "status": "complete",
        "axes": axis_declaration,
        "frontier": sorted(frontier_ids),
        "dominated_by": dict(sorted(dominated_by.items())),
        "interpretation": (
            "descriptive nondominance over the exact observed point estimates; "
            "no scalar weights, uncertainty dominance, or repeated-trial claim"
        ),
    }


def analyze_definition(
    root: Path,
    path: Path,
    matrices_by_path: dict[str, dict[str, Any]],
) -> tuple[dict[str, Any], set[str]]:
    definition = read_json(path, "tau ablation definition")
    ablation_id = definition.get("ablation_id")
    require(isinstance(ablation_id, str) and ablation_id, f"{path} has no ablation_id")
    conditions = definition_conditions(definition, ablation_id)
    referenced = set()
    points_by_speech: dict[str, list[dict[str, Any]]] = {}
    for condition in conditions:
        matrix_path = display_path(root, resolve(root, condition["matrix"]))
        referenced.add(matrix_path)
        matrix = matrices_by_path.get(matrix_path)
        require(
            matrix is not None,
            f"{ablation_id}/{condition['id']} is absent from the terminal study",
        )
        for speech, source_point in matrix["cells_by_speech_complexity"].items():
            point = {
                **source_point,
                "point_id": f"{condition['id']}:{speech}",
                "condition_id": condition["id"],
                "role": condition["role"],
            }
            points_by_speech.setdefault(speech, []).append(point)
    require(bool(points_by_speech), f"{ablation_id} contains no analyzed points")
    for speech, points in points_by_speech.items():
        require_equal(
            len(points), len(conditions), f"{ablation_id}/{speech} condition coverage"
        )
    return (
        {
            "definition": artifact(root, path),
            "conditions": conditions,
            "inference_scope": (
                "descriptive complete-population point estimates; one fixed-order trial "
                "per task does not bound run-to-run variation"
            ),
            "speech_populations": {
                speech: {
                    "points": points,
                    "frontiers": {
                        panel: frontier(points, axes)
                        for panel, axes in FRONTIER_PANELS.items()
                    },
                }
                for speech, points in sorted(points_by_speech.items())
            },
        },
        referenced,
    )


def build_analysis(
    root: Path,
    full_report_path: Path,
    definition_paths: list[Path],
    *,
    exhaustive: bool,
    expected_definition_hashes: dict[str, str] | None = None,
) -> dict[str, Any]:
    full_report = read_json(full_report_path, "terminal full-study report")
    require_equal(full_report.get("schema_version"), "1.0.0", "terminal report schema")
    require_equal(full_report.get("status"), "complete", "terminal report status")
    study = full_report.get("study")
    require(isinstance(study, dict), "terminal report has no study declaration")
    policy = study.get("publication_policy")
    require(isinstance(policy, dict), "terminal report has no publication policy")
    require_equal(policy.get("partial_results"), "forbidden", "partial-results policy")
    require_equal(
        policy.get("cross_benchmark_composite"), "forbidden", "composite policy"
    )
    require_equal(
        study.get("source_worktree_clean"), True, "terminal reporter clean source"
    )
    reporter = study.get("terminal_reporter")
    require(isinstance(reporter, dict), "terminal report has no reporter provenance")
    require_equal(
        reporter.get("source_worktree_clean"), True, "reporter-code clean source"
    )
    reporter_script = verify_artifact(
        root, reporter.get("script"), "terminal reporter script"
    )
    reporter_mode = reporter.get("mode")
    require(
        reporter_mode in ("frozen-orchestration", "postfreeze-reporting-correction"),
        "terminal reporter mode is invalid",
    )
    if reporter_mode == "frozen-orchestration":
        require_equal(
            reporter.get("source_revision"),
            study.get("orchestration_revision"),
            "frozen terminal reporter revision",
        )
        require_equal(
            reporter.get("correction_scope"), "none", "reporter correction scope"
        )
    else:
        scope = reporter.get("correction_scope")
        require(
            isinstance(scope, dict), "post-freeze reporter correction scope is absent"
        )
        require_equal(
            scope.get("benchmark_execution_changed"),
            False,
            "correction benchmark execution",
        )
        require_equal(
            scope.get("native_scores_changed"), False, "correction native scores"
        )
    tau = full_report.get("evidence_panel", {}).get("tau_voice")
    require(isinstance(tau, dict), "terminal report has no tau-Voice panel")
    matrix_panels = tau.get("matrices")
    require(
        isinstance(matrix_panels, list) and matrix_panels,
        "terminal report has no tau matrices",
    )
    matrices = [analyze_matrix(root, panel) for panel in matrix_panels]
    matrices_by_path = {matrix["matrix_path"]: matrix for matrix in matrices}
    require_equal(len(matrices_by_path), len(matrices), "unique terminal tau matrices")

    ablations = {}
    referenced = set()
    for path in definition_paths:
        if expected_definition_hashes is not None:
            relative = display_path(root, path)
            require(
                relative in expected_definition_hashes,
                f"unregistered default definition {relative}",
            )
            require_equal(
                sha256_file(path),
                expected_definition_hashes[relative],
                f"frozen ablation definition {relative} SHA-256",
            )
        analysis, paths = analyze_definition(root, path, matrices_by_path)
        ablation_id = read_json(path, "tau ablation definition")["ablation_id"]
        require(ablation_id not in ablations, f"duplicate ablation {ablation_id}")
        ablations[ablation_id] = analysis
        referenced.update(paths)
    if exhaustive:
        require_equal(
            referenced, set(matrices_by_path), "default analysis matrix coverage"
        )

    total_population = sum(matrix["population"] for matrix in matrices)
    return {
        "schema_version": SCHEMA_VERSION,
        "status": "complete",
        "generated_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "source": {
            "terminal_full_study_report": artifact(root, full_report_path),
            "study_id": study.get("id"),
            "orchestration_revision": study.get("orchestration_revision"),
            "terminal_reporter": {
                **reporter,
                "script": artifact(root, reporter_script),
            },
            "population": total_population,
        },
        "interpretation": {
            "aggregation_across_benchmark_families": "none",
            "partial_populations": "none admitted",
            "frontier": "nondominated native point estimates; no private composite",
            "cost": "tau priced compatibility proxy only; no provider-billed or electricity cost",
            "compute": (
                "provider-reported tokens and matrix-pair provider-boundary work; "
                "not device-kernel compute and not attributable to one speech cell"
            ),
            "measurement_limits": {
                "first_semantic_audio": (
                    "not measured: frozen trajectories have first-response audio but no "
                    "human semantic-progress labels; transcript heuristics are not substituted"
                ),
                "repairs": (
                    "not measured: typed audible repair was implemented after the frozen executable"
                ),
                "repetition_and_capability_denial": (
                    "not measured as dedicated labels; native task/review metrics are retained"
                ),
                "native_system_comparators": (
                    "not measured: GPT-Live and TML Interaction Models had no authorized public executable endpoint"
                ),
                "run_to_run_uncertainty": (
                    "not bounded: each frozen matrix has one fixed-order trial per task"
                ),
            },
        },
        "tau_voice": {
            "population": total_population,
            "axis_directions": AXIS_DIRECTIONS,
            "matrices": {matrix["matrix_id"]: matrix for matrix in matrices},
            "ablations": dict(sorted(ablations.items())),
        },
    }


def atomic_write(path: Path, payload: dict[str, Any]) -> None:
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
    root = arguments.repository_root.resolve()
    full_report_path = resolve(root, arguments.full_study_report)
    raw_definitions = arguments.definition or [
        Path(value) for value in DEFAULT_DEFINITIONS
    ]
    definition_paths = [resolve(root, path) for path in raw_definitions]
    output = arguments.output
    if output is None:
        output = root / ".runtime/benchmark-runs/full-study-v1/analysis.json"
    else:
        output = resolve(root, output)
    analysis = build_analysis(
        root,
        full_report_path,
        definition_paths,
        exhaustive=arguments.definition is None,
        expected_definition_hashes=(
            DEFAULT_DEFINITIONS if arguments.definition is None else None
        ),
    )
    atomic_write(output, analysis)
    print(f"full voice study analysis complete: {output}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except FullStudyAnalysisError as error:
        print(f"full study analysis refused: {error}", file=sys.stderr)
        raise SystemExit(1)
