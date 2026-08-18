#!/usr/bin/env python3
"""Validate and score one frozen OpenRealtime tau-Voice matrix.

This script deliberately has no partial-report mode.  It first proves that every
requested cell contains the exact task/trial population declared by its matrix,
then delegates task and interaction scoring to the pinned upstream tau2 code.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import subprocess
import sys
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

from tau2.data_model.simulation import Results
from tau2.metrics.agent_metrics import compute_metrics
from tau2.metrics.voice_interaction_metrics import aggregate_domain_metrics
from tau2.scripts.leaderboard.compute_interaction_metrics import (
    compute_metrics_for_loaded_results,
)


class IncompleteMatrixError(RuntimeError):
    """Raised when an experiment is not the exact preregistered population."""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--matrix", required=True, type=Path)
    parser.add_argument("--tau2-dir", required=True, type=Path)
    parser.add_argument("--repository-root", required=True, type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument(
        "--cell",
        action="append",
        dest="cells",
        help="score only this complete cell; may be repeated (default: every cell)",
    )
    parser.add_argument(
        "--validate-only",
        action="store_true",
        help="prove exact population completeness without loading tick trajectories",
    )
    return parser.parse_args()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def git_output(directory: Path, *arguments: str) -> str:
    return subprocess.run(
        ["git", "-C", str(directory), *arguments],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    ).stdout.strip()


def git_worktree_evidence(directory: Path) -> dict[str, Any]:
    status = subprocess.run(
        ["git", "-C", str(directory), "status", "--porcelain=v1", "-z"],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    ).stdout
    changed_output = subprocess.run(
        [
            "git",
            "-C",
            str(directory),
            "ls-files",
            "--modified",
            "--others",
            "--exclude-standard",
            "-z",
        ],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    ).stdout
    changed_paths = sorted(
        {part.decode() for part in changed_output.split(b"\0") if part}
    )
    digest = hashlib.sha256()
    files: list[dict[str, Any]] = []
    for relative in changed_paths:
        path = directory / relative
        if path.is_file():
            size = path.stat().st_size
            file_hash = sha256_file(path)
        else:
            size = None
            file_hash = None
        files.append({"path": relative, "bytes": size, "sha256": file_hash})
        digest.update(relative.encode())
        digest.update(b"\0")
        digest.update(str(size).encode())
        digest.update(b"\0")
        digest.update((file_hash or "missing").encode())
        digest.update(b"\n")
    return {
        "status_sha256": hashlib.sha256(status).hexdigest(),
        "content_sha256": digest.hexdigest(),
        "files": files,
    }


def require_equal(actual: Any, expected: Any, label: str) -> None:
    if actual != expected:
        raise IncompleteMatrixError(f"{label}: expected {expected!r}, found {actual!r}")


def normalize_task_id(value: Any) -> str:
    return str(value)


def validate_population(
    experiment: Path,
    *,
    cell_id: str,
    speech_complexity: str,
    domain: str,
    expected_tasks: int,
    num_trials: int,
    seed: int,
    tick_seconds: float,
    transport: dict[str, Any],
    voice_registry: dict[str, str],
) -> dict[str, Any]:
    metadata_path = experiment / "results.json"
    simulations_path = experiment / "simulations"
    if not metadata_path.is_file():
        raise IncompleteMatrixError(f"{cell_id}/{domain}: missing {metadata_path}")
    if not simulations_path.is_dir():
        raise IncompleteMatrixError(f"{cell_id}/{domain}: missing {simulations_path}")

    metadata = Results.load_metadata(experiment)
    label = f"{cell_id}/{domain}"
    require_equal(metadata.info.environment_info.domain_name, domain, f"{label} domain")
    require_equal(metadata.info.num_trials, num_trials, f"{label} trial count")
    require_equal(metadata.info.seed, seed, f"{label} seed")
    require_equal(
        metadata.info.speech_complexity,
        speech_complexity,
        f"{label} speech complexity",
    )
    audio_config = metadata.info.audio_native_config
    if audio_config is None:
        raise IncompleteMatrixError(f"{label}: missing audio-native configuration")
    if not math.isclose(
        audio_config.tick_duration_seconds,
        tick_seconds,
        rel_tol=0.0,
        abs_tol=1e-9,
    ):
        raise IncompleteMatrixError(
            f"{label} tick: expected {tick_seconds}, "
            f"found {audio_config.tick_duration_seconds}"
        )
    require_equal(
        audio_config.provider, transport["provider"], f"{label} transport provider"
    )
    require_equal(
        audio_config.model,
        transport["compatibility_model"],
        f"{label} compatibility model",
    )
    require_equal(
        audio_config.base_url, transport["base_url"], f"{label} transport endpoint"
    )
    require_equal(
        metadata.info.agent_info.implementation,
        "discrete_time_audio_native_agent",
        f"{label} upstream adapter",
    )
    require_equal(
        metadata.info.agent_info.llm,
        f"{transport['provider']}:{transport['compatibility_model']}",
        f"{label} upstream adapter model",
    )
    voice_settings = metadata.info.user_info.voice_settings
    if voice_settings is None or voice_settings.synthesis_config is None:
        raise IncompleteMatrixError(f"{label}: missing caller speech configuration")
    synthesis = voice_settings.synthesis_config
    require_equal(synthesis.provider, "fish_audio", f"{label} caller TTS provider")
    require_equal(
        synthesis.provider_config.voice_registry,
        voice_registry,
        f"{label} Fish voice registry",
    )

    expected_task_ids = [normalize_task_id(task.id) for task in metadata.tasks]
    require_equal(len(expected_task_ids), expected_tasks, f"{label} metadata tasks")
    if len(set(expected_task_ids)) != len(expected_task_ids):
        raise IncompleteMatrixError(f"{label}: duplicate task IDs in metadata")
    expected_population = {
        (task_id, trial)
        for task_id in expected_task_ids
        for trial in range(num_trials)
    }

    index = metadata.simulation_index
    if index is None:
        raise IncompleteMatrixError(f"{label}: missing simulation index")
    expected_simulations = expected_tasks * num_trials
    require_equal(len(index), expected_simulations, f"{label} simulation index")
    indexed_ids = [entry.id for entry in index]
    if len(set(indexed_ids)) != len(indexed_ids):
        raise IncompleteMatrixError(f"{label}: duplicate simulation IDs in index")
    indexed_population = {
        (normalize_task_id(entry.task_id), entry.trial) for entry in index
    }
    if len(indexed_population) != len(index):
        raise IncompleteMatrixError(f"{label}: duplicate task/trial rows in index")
    if indexed_population != expected_population:
        missing = sorted(expected_population - indexed_population)
        extra = sorted(indexed_population - expected_population)
        raise IncompleteMatrixError(
            f"{label}: incomplete task/trial population; "
            f"missing={missing[:10]}, extra={extra[:10]}"
        )

    simulation_files = sorted(simulations_path.glob("*.json"))
    require_equal(len(simulation_files), expected_simulations, f"{label} files")
    file_ids = {path.stem for path in simulation_files}
    if file_ids != set(indexed_ids):
        missing = sorted(set(indexed_ids) - file_ids)
        extra = sorted(file_ids - set(indexed_ids))
        raise IncompleteMatrixError(
            f"{label}: simulation files do not match index; "
            f"missing={missing[:10]}, extra={extra[:10]}"
        )

    # Validate each file's identity and population row without retaining ticks.
    indexed_rows = {
        entry.id: (normalize_task_id(entry.task_id), entry.trial) for entry in index
    }
    actual_population: set[tuple[str, int]] = set()
    actual_ids: set[str] = set()
    for simulation in Results.iter_simulations(experiment):
        if simulation.id in actual_ids:
            raise IncompleteMatrixError(
                f"{label}: duplicate simulation ID {simulation.id!r} on disk"
            )
        actual_ids.add(simulation.id)
        if simulation.trial is None:
            raise IncompleteMatrixError(
                f"{label}: simulation {simulation.id!r} has no trial"
            )
        row = (normalize_task_id(simulation.task_id), simulation.trial)
        if indexed_rows.get(simulation.id) != row:
            raise IncompleteMatrixError(
                f"{label}: simulation {simulation.id!r} disagrees with its index row"
            )
        if row in actual_population:
            raise IncompleteMatrixError(f"{label}: duplicate task/trial row {row!r}")
        actual_population.add(row)
    if actual_population != expected_population or actual_ids != set(indexed_ids):
        raise IncompleteMatrixError(f"{label}: on-disk simulations fail population proof")

    return {
        "status": "complete",
        "tasks": expected_tasks,
        "trials_per_task": num_trials,
        "simulations": expected_simulations,
        "infrastructure_errors": sum(
            entry.termination_reason == "infrastructure_error" for entry in index
        ),
    }


def hash_evidence(experiment: Path) -> dict[str, Any]:
    files = [experiment / "results.json"] + sorted(
        (experiment / "simulations").glob("*.json")
    )
    root_digest = hashlib.sha256()
    total_bytes = 0
    for path in files:
        relative = path.relative_to(experiment).as_posix()
        size = path.stat().st_size
        digest = sha256_file(path)
        total_bytes += size
        root_digest.update(relative.encode())
        root_digest.update(b"\0")
        root_digest.update(str(size).encode())
        root_digest.update(b"\0")
        root_digest.update(digest.encode())
        root_digest.update(b"\n")
    return {
        "algorithm": "sha256(path\\0size\\0file_sha256\\n)",
        "digest": root_digest.hexdigest(),
        "files": len(files),
        "bytes": total_bytes,
        "metadata_sha256": sha256_file(experiment / "results.json"),
    }


def cumulative_runtime_delta(
    start: dict[str, Any], final: dict[str, Any], path: str = "runtime"
) -> dict[str, Any]:
    """Subtract additive, content-free counters while retaining their shape.

    Maximum latency fields are gauges rather than cumulative counters and stay
    in the raw health evidence. A decrease in any additive field means the
    gateway restarted or the evidence is inconsistent, so the run cannot be
    treated as one measurement population.
    """
    result: dict[str, Any] = {}
    for key, final_value in final.items():
        if key.startswith("maximum_") or "_maximum_" in key:
            continue
        start_value = start.get(key)
        field_path = f"{path}.{key}"
        if isinstance(final_value, dict):
            if not isinstance(start_value, dict):
                raise IncompleteMatrixError(
                    f"{field_path} is missing from the initial runtime snapshot"
                )
            result[key] = cumulative_runtime_delta(
                start_value, final_value, field_path
            )
            continue
        if type(final_value) not in (int, float) or type(start_value) not in (
            int,
            float,
        ):
            raise IncompleteMatrixError(
                f"{field_path} must be numeric in both runtime snapshots"
            )
        if final_value < start_value:
            raise IncompleteMatrixError(
                f"{field_path} decreased from {start_value} to {final_value}"
            )
        result[key] = final_value - start_value
    return result


def collect_run_evidence(
    repository_root: Path, matrix_id: str, matrix_sha256: str
) -> list[dict[str, Any]]:
    matrix_run_root = (
        repository_root / ".runtime/benchmark-runs/tau-voice" / matrix_id
    )
    if not matrix_run_root.is_dir():
        return []
    evidence: list[dict[str, Any]] = []
    for run_path in sorted(matrix_run_root.rglob("run.json")):
        payload = json.loads(run_path.read_text(encoding="utf-8"))
        require_equal(
            payload.get("matrix_sha256"),
            matrix_sha256,
            f"run artifact {run_path} matrix hash",
        )
        entry: dict[str, Any] = {
            "path": str(run_path.relative_to(repository_root)),
            "sha256": sha256_file(run_path),
            "status": payload.get("status"),
            "selected_cell": payload.get("selected_cell"),
            "started_at": payload.get("started_at"),
            "completed_at": payload.get("completed_at"),
            "openrealtime_revision": payload.get("openrealtime_revision"),
            "gateway_health_start": payload.get("gateway_health"),
            "gateway_health_final": payload.get("gateway_health_final"),
        }
        health_start = payload.get("gateway_health")
        health_final = payload.get("gateway_health_final")
        if isinstance(health_start, dict) and isinstance(health_final, dict):
            runtime_start = health_start.get("runtime")
            runtime_final = health_final.get("runtime")
            if isinstance(runtime_start, dict) and isinstance(runtime_final, dict):
                entry["gateway_runtime_delta"] = cumulative_runtime_delta(
                    runtime_start, runtime_final
                )
        telemetry = run_path.with_name("gpu.csv")
        if telemetry.is_file():
            entry["gpu_telemetry"] = {
                "path": str(telemetry.relative_to(repository_root)),
                "sha256": sha256_file(telemetry),
                "bytes": telemetry.stat().st_size,
            }
        evidence.append(entry)
    return evidence


def finite_json(value: Any) -> Any:
    if isinstance(value, float) and not math.isfinite(value):
        return None
    if isinstance(value, dict):
        return {str(key): finite_json(item) for key, item in value.items()}
    if isinstance(value, list):
        return [finite_json(item) for item in value]
    return value


def sum_nested_counts(values: Iterable[dict[str, Any]]) -> dict[str, Any]:
    totals: defaultdict[str, Any] = defaultdict(int)
    for value in values:
        for key, count in value.items():
            if isinstance(count, dict):
                current = totals.get(key)
                if not isinstance(current, dict):
                    current = {}
                totals[key] = sum_nested_counts([current, count])
            else:
                totals[key] += count
    return dict(sorted(totals.items()))


def aggregate_agent_metrics(domain_metrics: dict[str, dict[str, Any]]) -> dict[str, Any]:
    metrics = list(domain_metrics.values())

    def mean_present(key: str) -> float | None:
        values = [metric[key] for metric in metrics if metric.get(key) is not None]
        return sum(values) / len(values) if values else None

    pass_keys = sorted(
        {key for metric in metrics for key in metric.get("pass_hat_ks", {})},
        key=int,
    )
    result: dict[str, Any] = {
        "aggregation": "equal-domain arithmetic mean; count fields are sums",
        "avg_reward": mean_present("avg_reward"),
        "pass_hat_ks": {
            key: sum(
                metric["pass_hat_ks"][key]
                for metric in metrics
                if key in metric["pass_hat_ks"]
            )
            / sum(key in metric["pass_hat_ks"] for metric in metrics)
            for key in pass_keys
        },
        "avg_agent_cost": mean_present("avg_agent_cost"),
    }
    excluded = {"avg_reward", "pass_hat_ks", "avg_agent_cost"}
    for key in sorted(set().union(*(metric.keys() for metric in metrics)) - excluded):
        values = [metric.get(key, 0) for metric in metrics]
        if all(isinstance(value, dict) for value in values):
            result[key] = sum_nested_counts(values)
        elif all(isinstance(value, int) and not isinstance(value, bool) for value in values):
            result[key] = sum(values)
    return result


def atomic_write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temporary.open("w", encoding="utf-8") as stream:
        json.dump(finite_json(payload), stream, indent=2, sort_keys=True, allow_nan=False)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)


def main() -> int:
    arguments = parse_args()
    matrix_path = arguments.matrix.resolve()
    tau2_dir = arguments.tau2_dir.resolve()
    repository_root = arguments.repository_root.resolve()
    matrix = json.loads(matrix_path.read_text(encoding="utf-8"))

    cell_ids = [cell["id"] for cell in matrix["cells"]]
    domain_names = [domain["name"] for domain in matrix["benchmark"]["domains"]]
    if len(cell_ids) != len(set(cell_ids)):
        raise IncompleteMatrixError("matrix contains duplicate cell IDs")
    if len(domain_names) != len(set(domain_names)):
        raise IncompleteMatrixError("matrix contains duplicate domains")
    declared_tasks = sum(
        domain["tasks"] for domain in matrix["benchmark"]["domains"]
    )
    require_equal(
        declared_tasks,
        matrix["benchmark"]["total_tasks_per_cell"],
        "matrix total tasks per cell",
    )

    expected_tau_revision = matrix["benchmark"]["revision"]
    actual_tau_revision = git_output(tau2_dir, "rev-parse", "HEAD")
    require_equal(actual_tau_revision, expected_tau_revision, "tau2 revision")

    declared_cells = {cell["id"]: cell for cell in matrix["cells"]}
    selected_cells = arguments.cells or list(declared_cells)
    unknown_cells = sorted(set(selected_cells) - set(declared_cells))
    if unknown_cells:
        raise IncompleteMatrixError(f"unknown matrix cells: {unknown_cells}")
    if len(selected_cells) != len(set(selected_cells)):
        raise IncompleteMatrixError("a matrix cell was selected more than once")

    data_root = tau2_dir / "data" / "simulations"
    population: dict[str, dict[str, dict[str, Any]]] = {}
    experiments: dict[tuple[str, str], Path] = {}
    for cell_id in selected_cells:
        cell = declared_cells[cell_id]
        registry_path = repository_root / cell["voice_registry"]
        voice_registry = json.loads(registry_path.read_text(encoding="utf-8"))
        population[cell_id] = {}
        for domain_spec in matrix["benchmark"]["domains"]:
            domain = domain_spec["name"]
            experiment = data_root / (
                f"{matrix['matrix_id']}-{cell_id}-{domain}-seed"
                f"{matrix['benchmark']['seed']}"
            )
            experiments[(cell_id, domain)] = experiment
            population[cell_id][domain] = validate_population(
                experiment,
                cell_id=cell_id,
                speech_complexity=cell["speech_complexity"],
                domain=domain,
                expected_tasks=domain_spec["tasks"],
                num_trials=matrix["benchmark"]["num_trials"],
                seed=matrix["benchmark"]["seed"],
                tick_seconds=matrix["benchmark"]["tick_seconds"],
                transport=matrix["transport"],
                voice_registry=voice_registry,
            )

    if arguments.validate_only:
        print(
            f"complete population: {matrix['matrix_id']} "
            f"({', '.join(selected_cells)})"
        )
        return 0

    scorer_paths = {
        "agent_metrics": tau2_dir / "src/tau2/metrics/agent_metrics.py",
        "interaction_metrics": tau2_dir
        / "src/tau2/metrics/voice_interaction_metrics.py",
        "interaction_entrypoint": tau2_dir
        / "src/tau2/scripts/leaderboard/compute_interaction_metrics.py",
    }
    adapter_patch = (
        repository_root
        / "benchmarks/tau-voice/patches/0001-local-openai-fish-audio.patch"
    )
    report: dict[str, Any] = {
        "schema_version": "1.0.0",
        "status": "complete",
        "generated_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "matrix": {
            "id": matrix["matrix_id"],
            "path": str(matrix_path.relative_to(repository_root)),
            "sha256": sha256_file(matrix_path),
            "selected_cells": selected_cells,
        },
        "benchmark": {
            "repository": matrix["benchmark"]["repository"],
            "revision": actual_tau_revision,
            "adapter_patch": {
                "path": str(adapter_patch.relative_to(repository_root)),
                "sha256": sha256_file(adapter_patch),
            },
            "working_tree": git_worktree_evidence(tau2_dir),
            "scorers": {
                name: {
                    "path": str(path.relative_to(tau2_dir)),
                    "sha256": sha256_file(path),
                }
                for name, path in scorer_paths.items()
            },
        },
        "aggregation": {
            "task": "official tau2 compute_metrics per domain; equal-domain means overall",
            "interaction": "official tau2 compute_metrics_for_loaded_results and aggregate_domain_metrics",
            "partial_results": "forbidden",
        },
        "execution_evidence": collect_run_evidence(
            repository_root, matrix["matrix_id"], sha256_file(matrix_path)
        ),
        "cells": {},
    }

    for cell_id in selected_cells:
        domain_agent: dict[str, dict[str, Any]] = {}
        domain_interaction: dict[str, dict[str, Any]] = {}
        cell_report: dict[str, Any] = {
            "speech_complexity": declared_cells[cell_id]["speech_complexity"],
            "domains": {},
        }
        for domain_spec in matrix["benchmark"]["domains"]:
            domain = domain_spec["name"]
            experiment = experiments[(cell_id, domain)]
            results = Results.load(experiment)
            agent = finite_json(compute_metrics(results).model_dump(mode="json"))
            interaction = finite_json(compute_metrics_for_loaded_results(results))
            domain_agent[domain] = agent
            domain_interaction[domain] = interaction
            cell_report["domains"][domain] = {
                "population": population[cell_id][domain],
                "agent_metrics": agent,
                "interaction_metrics": interaction,
                "evidence": hash_evidence(experiment),
            }
        cell_report["overall"] = {
            "agent_metrics": aggregate_agent_metrics(domain_agent),
            "interaction_metrics": aggregate_domain_metrics(domain_interaction),
        }
        report["cells"][cell_id] = cell_report

    output = arguments.output
    if output is None:
        output = (
            repository_root
            / ".runtime/benchmark-runs/tau-voice"
            / matrix["matrix_id"]
            / "report.json"
        )
    atomic_write_json(output.resolve(), report)
    print(f"tau-Voice report complete: {output.resolve()}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except IncompleteMatrixError as error:
        print(f"incomplete matrix; not a tau-Voice score: {error}", file=sys.stderr)
        raise SystemExit(1)
