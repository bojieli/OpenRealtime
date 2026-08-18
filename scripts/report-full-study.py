#!/usr/bin/env python3
"""Publish one fail-closed evidence panel for the complete voice study.

This is deliberately a completeness validator, not a new benchmark or a
cross-suite scoring function. It refuses to write an output until every frozen
population and every required upstream evaluation artifact is exact.
"""

from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
from typing import Any


class StudyIncompleteError(RuntimeError):
    """The preregistered study is missing or contains inconsistent evidence."""


def arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--manifest", type=Path, default=Path("benchmarks/full-study-v1.json")
    )
    parser.add_argument("--repository-root", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise StudyIncompleteError(message)


def require_equal(actual: Any, expected: Any, label: str) -> None:
    if actual != expected:
        raise StudyIncompleteError(
            f"{label}: expected {expected!r}, found {actual!r}"
        )


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def read_json(path: Path, label: str) -> dict[str, Any]:
    if not path.is_file():
        raise StudyIncompleteError(f"{label} is missing: {path}")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise StudyIncompleteError(f"cannot read {label} {path}: {error}") from error
    if not isinstance(value, dict):
        raise StudyIncompleteError(f"{label} must be a JSON object: {path}")
    return value


def resolve(root: Path, value: str) -> Path:
    path = Path(value)
    if not path.is_absolute():
        path = root / path
    return path.resolve()


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


def load_pin(root: Path, specification: dict[str, Any], label: str) -> tuple[Path, dict[str, Any]]:
    path = resolve(root, specification["path"])
    payload = read_json(path, label)
    require_equal(sha256_file(path), specification["sha256"], f"{label} SHA-256")
    return path, payload


def validate_local_descriptor(
    descriptor: dict[str, Any], *, profile: str, label: str
) -> None:
    expected = {
        "provider": "openrealtime",
        "model": "openrealtime-local",
        "transport": "websocket-openai-realtime",
        "architecture": "canonical-local-asr-fast-slow-tts",
        "profile": profile,
        "input_sample_rate_hz": 24000,
        "output_sample_rate_hz": 24000,
    }
    require_equal(descriptor, expected, f"{label} descriptor")


def validate_tau_matrix(
    root: Path, matrix_specification: dict[str, Any]
) -> dict[str, Any]:
    matrix_path, matrix = load_pin(root, matrix_specification, "tau matrix")
    matrix_id = matrix.get("matrix_id")
    require(isinstance(matrix_id, str) and matrix_id, f"{matrix_path} has no matrix_id")
    report_path = root / ".runtime/benchmark-runs/tau-voice" / matrix_id / "report.json"
    report = read_json(report_path, f"tau report {matrix_id}")
    require_equal(report.get("schema_version"), "1.0.0", f"{matrix_id} report schema")
    require_equal(report.get("status"), "complete", f"{matrix_id} report status")

    cell_ids = [cell["id"] for cell in matrix["cells"]]
    domains = {domain["name"]: domain for domain in matrix["benchmark"]["domains"]}
    require_equal(len(cell_ids), len(set(cell_ids)), f"{matrix_id} unique cells")
    require_equal(
        sum(domain["tasks"] for domain in domains.values()),
        matrix["benchmark"]["total_tasks_per_cell"],
        f"{matrix_id} declared tasks",
    )
    matrix_evidence = report.get("matrix", {})
    require_equal(matrix_evidence.get("id"), matrix_id, f"{matrix_id} report matrix ID")
    require_equal(
        matrix_evidence.get("path"),
        display_path(root, matrix_path),
        f"{matrix_id} report matrix path",
    )
    require_equal(
        matrix_evidence.get("sha256"),
        matrix_specification["sha256"],
        f"{matrix_id} report matrix hash",
    )
    require_equal(
        matrix_evidence.get("selected_cells"), cell_ids, f"{matrix_id} selected cells"
    )
    require_equal(
        report.get("benchmark", {}).get("revision"),
        matrix["benchmark"]["revision"],
        f"{matrix_id} tau revision",
    )
    require_equal(set(report.get("cells", {})), set(cell_ids), f"{matrix_id} report cells")

    total_simulations = 0
    infrastructure_errors = 0
    cell_panel: dict[str, Any] = {}
    for cell_id in cell_ids:
        cell_report = report["cells"][cell_id]
        require_equal(
            set(cell_report.get("domains", {})),
            set(domains),
            f"{matrix_id}/{cell_id} domains",
        )
        cell_simulations = 0
        cell_errors = 0
        for domain_name, domain in domains.items():
            population = cell_report["domains"][domain_name].get("population", {})
            expected_simulations = domain["tasks"] * matrix["benchmark"]["num_trials"]
            require_equal(
                population.get("status"),
                "complete",
                f"{matrix_id}/{cell_id}/{domain_name} status",
            )
            require_equal(
                population.get("tasks"),
                domain["tasks"],
                f"{matrix_id}/{cell_id}/{domain_name} tasks",
            )
            require_equal(
                population.get("trials_per_task"),
                matrix["benchmark"]["num_trials"],
                f"{matrix_id}/{cell_id}/{domain_name} trials",
            )
            require_equal(
                population.get("simulations"),
                expected_simulations,
                f"{matrix_id}/{cell_id}/{domain_name} simulations",
            )
            errors = population.get("infrastructure_errors")
            require(
                isinstance(errors, int) and errors >= 0,
                f"{matrix_id}/{cell_id}/{domain_name} has invalid infrastructure count",
            )
            cell_simulations += expected_simulations
            cell_errors += errors
        total_simulations += cell_simulations
        infrastructure_errors += cell_errors
        cell_panel[cell_id] = {
            "simulations": cell_simulations,
            "infrastructure_errors": cell_errors,
            "overall": cell_report.get("overall"),
        }

    expected_total = (
        len(cell_ids)
        * matrix["benchmark"]["total_tasks_per_cell"]
        * matrix["benchmark"]["num_trials"]
    )
    require_equal(total_simulations, expected_total, f"{matrix_id} total population")
    execution = report.get("execution_evidence")
    require(isinstance(execution, list), f"{matrix_id} execution evidence is absent")
    complete_runs = [item for item in execution if item.get("status") == "complete"]
    require(complete_runs, f"{matrix_id} has no complete execution artifact")
    revisions = sorted(
        {
            item["openrealtime_revision"]
            for item in complete_runs
            if isinstance(item.get("openrealtime_revision"), str)
            and item["openrealtime_revision"]
        }
    )
    require(revisions, f"{matrix_id} complete execution has no source revision")
    return {
        "matrix_id": matrix_id,
        "matrix": artifact(root, matrix_path),
        "report": artifact(root, report_path),
        "population": total_simulations,
        "infrastructure_errors": infrastructure_errors,
        "openrealtime_revisions": revisions,
        "cells": cell_panel,
    }


def validate_tau(root: Path, specification: dict[str, Any]) -> dict[str, Any]:
    source_path, source = load_pin(root, specification["source_manifest"], "tau source manifest")
    require_equal(
        source.get("benchmark", {}).get("name"), "tau-Voice", "tau source benchmark"
    )
    matrix_panel = [validate_tau_matrix(root, item) for item in specification["matrices"]]
    matrix_ids = [item["matrix_id"] for item in matrix_panel]
    require_equal(len(matrix_ids), len(set(matrix_ids)), "unique tau matrix IDs")

    paired_panel = []
    for pair in specification.get("paired_reports", []):
        definition_path, definition = load_pin(
            root, pair["definition"], f"tau paired definition {pair['id']}"
        )
        require_equal(definition.get("ablation_id"), pair["id"], "tau paired definition ID")
        report_path = resolve(root, pair["report"])
        report = read_json(report_path, f"tau paired report {pair['id']}")
        require_equal(report.get("status"), "complete", f"{pair['id']} status")
        ablation = report.get("ablation", {})
        require_equal(ablation.get("id"), pair["id"], f"{pair['id']} report ID")
        require_equal(
            ablation.get("path"),
            display_path(root, definition_path),
            f"{pair['id']} definition path",
        )
        require_equal(
            ablation.get("sha256"), pair["definition"]["sha256"], f"{pair['id']} definition hash"
        )
        endpoint_work = (
            report.get("comparison", {})
            .get("provider_work", {})
            .get("endpoint_only", {})
        )
        for phase in ("fast_preparation", "slow_preparation"):
            require_equal(
                endpoint_work.get(phase, {}).get("invocations"),
                0,
                f"{pair['id']} endpoint-only {phase}",
            )
        for evidence_name in ("continuous_report", "endpoint_report"):
            evidence = report.get("evidence", {}).get(evidence_name, {})
            evidence_path = resolve(root, evidence.get("path", ""))
            require(
                evidence_path.is_file(),
                f"{pair['id']} {evidence_name} is missing: {evidence_path}",
            )
            require_equal(
                sha256_file(evidence_path),
                evidence.get("sha256"),
                f"{pair['id']} {evidence_name} hash",
            )
        paired_panel.append(
            {
                "id": pair["id"],
                "definition": artifact(root, definition_path),
                "report": artifact(root, report_path),
                "comparison": report.get("comparison"),
            }
        )
    return {
        "source_manifest": artifact(root, source_path),
        "matrices": matrix_panel,
        "paired_reports": paired_panel,
        "population_across_preregistered_conditions": sum(
            item["population"] for item in matrix_panel
        ),
    }


def validate_fdb15(root: Path, specification: dict[str, Any]) -> dict[str, Any]:
    source_path, source = load_pin(
        root, specification["source_manifest"], "FDB1.5 source manifest"
    )
    require_equal(source.get("benchmark"), "Full-Duplex-Bench v1.5", "FDB1.5 source")
    require_equal(
        source.get("upstream_revision"), specification["upstream_revision"], "FDB1.5 revision"
    )
    scenario_population = {
        item["scenario"]: item["observed_complete_samples"] for item in source["archives"]
    }
    require_equal(sum(scenario_population.values()), specification["population"], "FDB1.5 source population")

    run_path = resolve(root, specification["run_manifest"])
    run = read_json(run_path, "FDB1.5 run manifest")
    require_equal(run.get("schema_version"), "1.2.0", "FDB1.5 run schema")
    require_equal(run.get("benchmark"), "full-duplex-bench-v1.5", "FDB1.5 run benchmark")
    require_equal(run.get("revision"), specification["upstream_revision"], "FDB1.5 run revision")
    require_equal(run.get("conditions"), specification["conditions"], "FDB1.5 conditions")
    require_equal(run.get("replicates"), specification["replicates"], "FDB1.5 replicates")
    validate_local_descriptor(
        run.get("descriptor", {}),
        profile="fdb-v1.5-openai-realtime-adapter-i1-qg-v1",
        label="FDB1.5",
    )
    samples = run.get("samples", [])
    require_equal(len(samples), specification["population"], "FDB1.5 planned population")
    sample_rows = [(item.get("scenario"), item.get("id")) for item in samples]
    require_equal(len(sample_rows), len(set(sample_rows)), "FDB1.5 unique samples")
    require_equal(Counter(item[0] for item in sample_rows), Counter(scenario_population), "FDB1.5 scenarios")
    completed = run.get("completed", [])
    require_equal(len(completed), specification["population"], "FDB1.5 completed population")
    require_equal(len(run.get("failures", [])), 0, "FDB1.5 terminal failures")
    trial_ids = [item.get("trial_id") for item in completed]
    require_equal(len(trial_ids), len(set(trial_ids)), "FDB1.5 unique completed trials")
    require_equal(
        {(item["sample"]["scenario"], item["sample"]["id"]) for item in completed},
        set(sample_rows),
        "FDB1.5 completed samples",
    )
    require(
        all(item.get("condition") == "overlap" for item in completed),
        "FDB1.5 completed a condition outside overlap",
    )

    summary_path = resolve(root, specification["summary"])
    summary = read_json(summary_path, "FDB1.5 summary")
    require_equal(summary.get("schema_version"), "1.2.0", "FDB1.5 summary schema")
    require_equal(summary.get("benchmark"), run["benchmark"], "FDB1.5 summary benchmark")
    require_equal(summary.get("revision"), run["revision"], "FDB1.5 summary revision")
    conditions = summary.get("conditions", [])
    require_equal(len(conditions), len(scenario_population), "FDB1.5 summary conditions")
    summary_counts: dict[str, int] = {}
    for condition in conditions:
        require_equal(condition.get("condition"), "overlap", "FDB1.5 summary condition")
        require_equal(condition.get("failures"), 0, "FDB1.5 summary failures")
        validate_local_descriptor(
            condition.get("descriptor", {}),
            profile="fdb-v1.5-openai-realtime-adapter-i1-qg-v1",
            label=f"FDB1.5 summary {condition.get('scenario')}",
        )
        summary_counts[condition["scenario"]] = condition["completed"]
    require_equal(summary_counts, scenario_population, "FDB1.5 summary populations")
    return {
        "source_manifest": artifact(root, source_path),
        "run_manifest": artifact(root, run_path),
        "summary": artifact(root, summary_path),
        "population": len(completed),
        "terminal_failures": 0,
        "metrics": conditions,
    }


def validate_fdbv3_evaluation(
    path: Path, *, sample_ids: set[str], llm_judge: bool, label: str
) -> dict[str, Any]:
    report = read_json(path, label)
    require_equal(report.get("total_scenarios"), len(sample_ids), f"{label} population")
    scenarios = report.get("scenario_results", [])
    require_equal(len(scenarios), len(sample_ids), f"{label} scenario rows")
    require_equal(
        {item.get("scenario_id") for item in scenarios}, sample_ids, f"{label} scenario IDs"
    )
    for scenario in scenarios:
        score = scenario.get("metrics", {}).get("response_qual", {}).get("score")
        if llm_judge:
            require(
                isinstance(score, (int, float)) and not isinstance(score, bool),
                f"{label}/{scenario.get('scenario_id')} lacks an LLM response score",
            )
        else:
            require_equal(score, None, f"{label}/{scenario.get('scenario_id')} exact-only response score")
    aggregate_score = report.get("by_metric", {}).get("response_qual")
    if llm_judge:
        require(
            isinstance(aggregate_score, (int, float)) and not isinstance(aggregate_score, bool),
            f"{label} lacks the aggregate LLM response score",
        )
    else:
        require_equal(aggregate_score, None, f"{label} exact-only aggregate response score")
    return report


def validate_fdbv3(root: Path, specification: dict[str, Any]) -> dict[str, Any]:
    source_path, source = load_pin(
        root, specification["source_manifest"], "FDBv3 source manifest"
    )
    profile_path, profile = load_pin(root, specification["profile"], "FDBv3 profile")
    require_equal(source.get("benchmark"), "Full-Duplex-Bench v3", "FDBv3 source")
    require_equal(source.get("upstream_revision"), specification["upstream_revision"], "FDBv3 revision")
    require_equal(source["released_artifact"]["audio_examples"], specification["population"], "FDBv3 source population")
    require_equal(profile.get("source", {}).get("revision"), specification["upstream_revision"], "FDBv3 profile revision")

    run_path = resolve(root, specification["run_manifest"])
    run = read_json(run_path, "FDBv3 run manifest")
    require_equal(run.get("schema_version"), "1.0.0", "FDBv3 run schema")
    require_equal(run.get("benchmark"), "Full-Duplex-Bench v3", "FDBv3 benchmark")
    require_equal(run.get("revision"), specification["upstream_revision"], "FDBv3 run revision")
    require_equal(run.get("profile"), profile["profile"], "FDBv3 run profile")
    require_equal(run.get("profile_sha256"), specification["profile"]["sha256"], "FDBv3 run profile hash")
    validate_local_descriptor(run.get("descriptor", {}), profile=profile["profile"], label="FDBv3")
    samples = run.get("samples", [])
    require_equal(len(samples), specification["population"], "FDBv3 planned population")
    labels = {f"{item['example_id']}_{item['pid']}" for item in samples}
    sample_ids = {item["example_id"] for item in samples}
    require_equal(len(labels), len(samples), "FDBv3 unique sample labels")
    require_equal(len(sample_ids), len(samples), "FDBv3 unique example IDs")
    require_equal(set(run.get("completed", [])), labels, "FDBv3 completed samples")
    require_equal(len(run.get("completed", [])), len(labels), "FDBv3 completed count")
    require_equal(len(run.get("failures", [])), 0, "FDBv3 terminal failures")

    exact_path = resolve(root, specification["evaluations"]["exact"])
    judge_path = resolve(root, specification["evaluations"]["gpt4o"])
    exact = validate_fdbv3_evaluation(
        exact_path, sample_ids=sample_ids, llm_judge=False, label="FDBv3 exact evaluation"
    )
    judge = validate_fdbv3_evaluation(
        judge_path, sample_ids=sample_ids, llm_judge=True, label="FDBv3 GPT-4o evaluation"
    )
    return {
        "source_manifest": artifact(root, source_path),
        "profile": artifact(root, profile_path),
        "run_manifest": artifact(root, run_path),
        "population": len(labels),
        "terminal_failures": 0,
        "evaluations": {
            "exact": {
                "artifact": artifact(root, exact_path),
                "turn_taking": exact.get("turn_taking"),
                "by_metric": exact.get("by_metric"),
                "latency": exact.get("latency"),
            },
            "gpt4o": {
                "artifact": artifact(root, judge_path),
                "turn_taking": judge.get("turn_taking"),
                "by_metric": judge.get("by_metric"),
                "latency": judge.get("latency"),
            },
        },
    }


def validate_fdbench(root: Path, specification: dict[str, Any]) -> dict[str, Any]:
    source_path, source = load_pin(
        root, specification["source_manifest"], "FD-Bench source manifest"
    )
    require_equal(source.get("upstream_revision"), specification["upstream_revision"], "FD-Bench revision")
    require_equal(source.get("dataset_revision"), specification["dataset_revision"], "FD-Bench dataset revision")
    require_equal(source.get("expected_released_conversations"), specification["population"], "FD-Bench source population")
    require_equal(source.get("expected_cell_count"), specification["cells"], "FD-Bench source cells")
    expected_cells = source["expected_cell_populations"]

    run_path = resolve(root, specification["run_manifest"])
    run = read_json(run_path, "FD-Bench run manifest")
    require_equal(run.get("schema_version"), "1.0.0", "FD-Bench run schema")
    require_equal(run.get("benchmark"), "FD-Bench", "FD-Bench run benchmark")
    require_equal(run.get("revision"), specification["upstream_revision"], "FD-Bench run revision")
    require_equal(run.get("dataset_revision"), specification["dataset_revision"], "FD-Bench run dataset revision")
    validate_local_descriptor(
        run.get("descriptor", {}), profile="fd-bench-standard-realtime-v1", label="FD-Bench"
    )
    samples = run.get("samples", [])
    require_equal(len(samples), specification["population"], "FD-Bench planned population")
    labels = {f"{item['cell']}/{item['id']}" for item in samples}
    require_equal(len(labels), len(samples), "FD-Bench unique samples")
    require_equal(
        Counter(item["cell"] for item in samples), Counter(expected_cells), "FD-Bench cell populations"
    )
    require_equal(set(run.get("completed", [])), labels, "FD-Bench completed samples")
    require_equal(len(run.get("completed", [])), len(labels), "FD-Bench completed count")
    require_equal(len(run.get("failures", [])), 0, "FD-Bench terminal failures")

    finalization_path = resolve(root, specification["finalization"])
    finalization = read_json(finalization_path, "FD-Bench finalization")
    require_equal(finalization.get("benchmark"), "FD-Bench", "FD-Bench finalization benchmark")
    require_equal(finalization.get("revision"), specification["upstream_revision"], "FD-Bench finalization revision")
    require_equal(finalization.get("results"), specification["population"], "FD-Bench finalized population")
    vad = finalization.get("vad", {})
    contract = source["evaluation_contract"]
    require_equal(vad.get("name"), contract["output_vad"], "FD-Bench VAD")
    require_equal(vad.get("package_version"), contract["output_vad_package_version"], "FD-Bench VAD version")
    require_equal(vad.get("threshold"), contract["output_vad_threshold"], "FD-Bench VAD threshold")
    require_equal(vad.get("min_silence_duration_ms"), contract["output_min_silence_duration_ms"], "FD-Bench VAD silence")
    require_equal(vad.get("timestamp_rate_hz"), contract["input_timestamp_rate_hz"], "FD-Bench VAD clock")
    traces = finalization.get("traces", {})
    require_equal(set(traces), set(expected_cells), "FD-Bench finalized trace cells")

    expected_metric_names = {
        "SRR_pct", "SIR_pct", "EIR_pct", "NIR_pct", "SRIR_pct",
        "FSED_ms", "ERT_ms", "EIT_ms", "IRD_ms",
    }
    metrics_directory = resolve(root, specification["metrics_directory"])
    metric_paths = sorted(metrics_directory.glob("*.json")) if metrics_directory.is_dir() else []
    require_equal({path.stem for path in metric_paths}, set(expected_cells), "FD-Bench metric cells")
    metric_panel: dict[str, Any] = {}
    for cell, expected_population in expected_cells.items():
        trace = traces[cell]
        require_equal(trace.get("samples"), expected_population, f"FD-Bench {cell} trace population")
        trace_path = resolve(root, trace["path"])
        require(trace_path.is_file(), f"FD-Bench {cell} trace is missing: {trace_path}")
        require_equal(sha256_file(trace_path), trace.get("sha256"), f"FD-Bench {cell} trace hash")
        metric_path = metrics_directory / f"{cell}.json"
        metric = read_json(metric_path, f"FD-Bench metric {cell}")
        require_equal(metric.get("schema_version"), "1.0.0", f"FD-Bench {cell} metric schema")
        require_equal(metric.get("benchmark"), "FD-Bench", f"FD-Bench {cell} metric benchmark")
        require_equal(metric.get("revision"), specification["upstream_revision"], f"FD-Bench {cell} metric revision")
        require_equal(metric.get("trace", {}).get("samples"), expected_population, f"FD-Bench {cell} metric population")
        require_equal(metric.get("trace", {}).get("sha256"), trace["sha256"], f"FD-Bench {cell} metric trace hash")
        require_equal(
            resolve(root, metric.get("trace", {}).get("path", trace["path"])),
            trace_path,
            f"FD-Bench {cell} metric trace path",
        )
        require_equal(set(metric.get("metrics", {})), expected_metric_names, f"FD-Bench {cell} objective metrics")
        require_equal(set(metric.get("not_evaluated", {})), set(specification["explicit_exclusions"]), f"FD-Bench {cell} exclusions")
        metric_panel[cell] = {
            "population": expected_population,
            "artifact": artifact(root, metric_path),
            "trace": artifact(root, trace_path),
            "metrics": metric["metrics"],
            "counts": metric.get("counts"),
            "categories": metric.get("categories"),
            "not_evaluated": metric["not_evaluated"],
        }
    return {
        "source_manifest": artifact(root, source_path),
        "run_manifest": artifact(root, run_path),
        "finalization": artifact(root, finalization_path),
        "population": len(labels),
        "cells": len(metric_panel),
        "terminal_failures": 0,
        "explicit_exclusions": specification["explicit_exclusions"],
        "metrics": metric_panel,
    }


def build_report(root: Path, manifest_path: Path) -> dict[str, Any]:
    manifest = read_json(manifest_path, "full study manifest")
    require_equal(manifest.get("schema_version"), "1.0.0", "study manifest schema")
    require_equal(manifest.get("status"), "preregistered", "study manifest status")
    policy = manifest.get("publication_policy", {})
    require_equal(policy.get("partial_results"), "forbidden", "partial-results policy")
    require_equal(policy.get("cross_benchmark_composite"), "forbidden", "composite policy")
    return {
        "schema_version": "1.0.0",
        "status": "complete",
        "generated_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "study": {
            "id": manifest["study_id"],
            "manifest": artifact(root, manifest_path),
            "publication_policy": policy,
        },
        "evidence_panel": {
            "tau_voice": validate_tau(root, manifest["tau_voice"]),
            "full_duplex_bench_v1_5": validate_fdb15(
                root, manifest["full_duplex_bench_v1_5"]
            ),
            "full_duplex_bench_v3": validate_fdbv3(
                root, manifest["full_duplex_bench_v3"]
            ),
            "fd_bench": validate_fdbench(root, manifest["fd_bench"]),
        },
        "interpretation": {
            "aggregation": "none across benchmark families",
            "partial_results": "none admitted",
            "metric_authority": "each panel retains its pinned benchmark and upstream scorer semantics",
        },
    }


def atomic_write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temporary.open("w", encoding="utf-8") as target:
        json.dump(payload, target, indent=2, sort_keys=True, allow_nan=False)
        target.write("\n")
        target.flush()
        os.fsync(target.fileno())
    os.replace(temporary, path)


def main() -> int:
    args = arguments()
    root = args.repository_root.resolve()
    manifest_path = resolve(root, str(args.manifest))
    output = args.output
    if output is None:
        output = root / ".runtime/benchmark-runs/full-study-v1/report.json"
    elif not output.is_absolute():
        output = root / output
    report = build_report(root, manifest_path)
    atomic_write_json(output.resolve(), report)
    print(f"full voice study report complete: {output.resolve()}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except StudyIncompleteError as error:
        print(f"incomplete full study; no report: {error}", file=os.sys.stderr)
        raise SystemExit(1)
