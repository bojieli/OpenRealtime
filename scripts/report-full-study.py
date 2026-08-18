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


LOCAL_FAST_CONTRACT = {
    "implementation": "vllm",
    "version": "0.19.0",
    "model": "Qwen/Qwen3-30B-A3B-FP8",
    "revision": "d206ba732169f29bb77fbf80fc2c4b81d4d30782",
    "served_model": "qwen-fast",
    "native_context_tokens": 40960,
    "configured_context_tokens": 40960,
    "kv_cache_tokens_observed_before_freeze": 44896,
    "gpu_memory_utilization": "0.38",
    "thinking": "disabled",
    "tool_authority": "propose",
}


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
        raise StudyIncompleteError(f"{label}: expected {expected!r}, found {actual!r}")


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def valid_sha256(value: Any) -> bool:
    return (
        isinstance(value, str)
        and len(value) == 64
        and all(character in "0123456789abcdef" for character in value)
    )


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


def artifact_tree(
    root: Path,
    entries: list[tuple[Path, str | None]],
    *,
    label: str,
) -> dict[str, Any]:
    digest = hashlib.sha256()
    total_bytes = 0
    seen: set[str] = set()
    for path, expected_hash in sorted(
        entries, key=lambda item: display_path(root, item[0])
    ):
        require(path.is_file(), f"{label} artifact is missing: {path}")
        logical_path = display_path(root, path)
        require(
            logical_path not in seen, f"{label} contains duplicate path {logical_path}"
        )
        seen.add(logical_path)
        size = path.stat().st_size
        file_hash = sha256_file(path)
        if expected_hash is not None:
            require_equal(file_hash, expected_hash, f"{label} {logical_path} SHA-256")
        total_bytes += size
        digest.update(logical_path.encode())
        digest.update(b"\0")
        digest.update(str(size).encode())
        digest.update(b"\0")
        digest.update(file_hash.encode())
        digest.update(b"\n")
    return {
        "algorithm": "sha256(path\\0size\\0file_sha256\\n)",
        "digest": digest.hexdigest(),
        "files": len(entries),
        "bytes": total_bytes,
    }


def validate_tree_summary(value: Any, *, files: int, label: str) -> None:
    require(isinstance(value, dict), f"{label} evidence tree is absent")
    require_equal(
        value.get("algorithm"),
        "sha256(path\\0size\\0file_sha256\\n)",
        f"{label} evidence algorithm",
    )
    require_equal(value.get("files"), files, f"{label} evidence files")
    require(
        isinstance(value.get("bytes"), int)
        and not isinstance(value["bytes"], bool)
        and value["bytes"] >= 0,
        f"{label} evidence bytes are invalid",
    )
    digest = value.get("digest")
    require(
        isinstance(digest, str)
        and len(digest) == 64
        and all(character in "0123456789abcdef" for character in digest),
        f"{label} evidence digest is invalid",
    )


def load_pin(
    root: Path, specification: dict[str, Any], label: str
) -> tuple[Path, dict[str, Any]]:
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


def validate_attempt_ledger(
    value: Any,
    *,
    expected: set[Any],
    maximum: int,
    identity: Any,
    number_field: str,
    label: str,
) -> tuple[dict[str, Any], dict[Any, int]]:
    """Validate a complete append-only provider-call ledger.

    The configured maximum is a lifetime budget for each logical trial across
    all process resumes. A terminally publishable trial has a contiguous
    prefix 1..N, exactly one success, and that success is its last attempt.
    """
    require(
        isinstance(value, list), f"{label} attempt ledger must be a JSON array"
    )
    groups: dict[Any, list[dict[str, Any]]] = {}
    for index, record in enumerate(value):
        require(
            isinstance(record, dict),
            f"{label} attempt record {index} must be a JSON object",
        )
        key = identity(record)
        try:
            known_trial = key in expected
        except TypeError as error:
            raise StudyIncompleteError(
                f"{label} attempt record {index} has an invalid trial identity"
            ) from error
        require(
            known_trial,
            f"{label} attempt ledger contains unknown trial {key!r}",
        )
        groups.setdefault(key, []).append(record)
    require_equal(set(groups), expected, f"{label} attempt-ledger trials")

    successes: dict[Any, int] = {}
    retried = 0
    maximum_observed = 0
    for key, records in groups.items():
        require(
            1 <= len(records) <= maximum,
            f"{label} trial {key!r} attempt count must be within 1..{maximum}",
        )
        numbers = [record.get(number_field) for record in records]
        require(
            all(
                isinstance(number, int) and not isinstance(number, bool)
                for number in numbers
            ),
            f"{label} trial {key!r} attempt numbers are invalid",
        )
        require_equal(
            numbers,
            list(range(1, len(records) + 1)),
            f"{label} trial {key!r} attempt numbering",
        )
        require(
            all(isinstance(record.get("succeeded"), bool) for record in records),
            f"{label} trial {key!r} attempt outcomes are invalid",
        )
        successful = [
            index
            for index, record in enumerate(records)
            if record.get("succeeded") is True
        ]
        require_equal(
            successful,
            [len(records) - 1],
            f"{label} trial {key!r} successful terminal attempt",
        )
        require(
            all(
                isinstance(record.get("error"), str) and bool(record["error"])
                for record in records[:-1]
            ),
            f"{label} trial {key!r} failed attempts lack error evidence",
        )
        require(
            not records[-1].get("error"),
            f"{label} trial {key!r} successful attempt contains an error",
        )
        successes[key] = numbers[-1]
        retried += int(len(records) > 1)
        maximum_observed = max(maximum_observed, len(records))
    return (
        {
            "maximum_attempts": maximum,
            "attempts": len(value),
            "trials": len(groups),
            "retried_trials": retried,
            "maximum_observed": maximum_observed,
        },
        successes,
    )


def validate_study_runtime(
    root: Path, specification: dict[str, Any]
) -> tuple[Path, dict[str, Any]]:
    path, runtime = load_pin(root, specification, "study runtime manifest")
    require_equal(runtime.get("schema_version"), "1.0.0", "study runtime schema")
    require_equal(
        runtime.get("runtime_id"),
        "openrealtime-canonical-gateway-v1",
        "study runtime ID",
    )
    source_revision = runtime.get("source_revision")
    require(
        isinstance(source_revision, str)
        and len(source_revision) == 40
        and all(character in "0123456789abcdef" for character in source_revision),
        "study runtime source revision is invalid",
    )
    require(valid_sha256(runtime.get("binary_sha256")), "study runtime hash is invalid")
    require_equal(
        runtime.get("runtime_path"),
        ".runtime/study-runtime/canonical-gateway-v1/realtimegateway",
        "study runtime path",
    )
    require_equal(
        runtime.get("local_fast"), LOCAL_FAST_CONTRACT, "study local-fast runtime"
    )
    build = runtime.get("build")
    require(isinstance(build, dict), "study runtime build declaration is absent")
    require_equal(build.get("go"), "/usr/local/go/bin/go", "study runtime Go path")
    require_equal(
        build.get("command"),
        "go build -trimpath -buildvcs=false ./cmd/realtimegateway",
        "study runtime build command",
    )
    return path, runtime


def require_argv_option(argv: list[str], option: str, expected: str, label: str) -> None:
    positions = [index for index, value in enumerate(argv) if value == option]
    require_equal(len(positions), 1, f"{label} {option} occurrence")
    position = positions[0]
    require(position + 1 < len(argv), f"{label} {option} has no value")
    require_equal(argv[position + 1], expected, f"{label} {option}")


def validate_runtime_identity(
    identity: Any,
    *,
    requires_local_fast: bool,
    expected_gateway_sha256: str,
    label: str,
) -> None:
    require(isinstance(identity, dict), f"{label} runtime identity is absent")
    require_equal(identity.get("schema_version"), "1.0.0", f"{label} identity schema")
    require(
        isinstance(identity.get("host_boot_id"), str)
        and bool(identity["host_boot_id"]),
        f"{label} host boot identity is absent",
    )
    components = identity.get("components")
    require(isinstance(components, dict), f"{label} runtime components are absent")
    required = {"gateway", "asr", "fish"}
    if requires_local_fast:
        required.add("qwen")
    require(required <= set(components), f"{label} is missing runtime components")
    require_equal(
        components["gateway"].get("executable_sha256"),
        expected_gateway_sha256,
        f"{label}/gateway frozen executable",
    )
    for name in sorted(required):
        component = components[name]
        require(
            isinstance(component.get("pid"), int)
            and not isinstance(component["pid"], bool)
            and component["pid"] > 0,
            f"{label}/{name} has an invalid PID",
        )
        require(
            isinstance(component.get("proc_start_time_ticks"), str)
            and component["proc_start_time_ticks"].isdigit(),
            f"{label}/{name} has an invalid process start identity",
        )
        for field in ("executable_sha256", "command_sha256"):
            value = component.get(field)
            require(
                isinstance(value, str)
                and len(value) == 64
                and all(character in "0123456789abcdef" for character in value),
                f"{label}/{name} has an invalid {field}",
            )
        require(
            isinstance(component.get("argv"), list)
            and all(isinstance(value, str) for value in component["argv"])
            and bool(component["argv"]),
            f"{label}/{name} has invalid argv evidence",
        )
    if requires_local_fast:
        qwen = components["qwen"]
        require_equal(
            qwen.get("service"),
            {
                "implementation": LOCAL_FAST_CONTRACT["implementation"],
                "version": LOCAL_FAST_CONTRACT["version"],
                "served_model": LOCAL_FAST_CONTRACT["served_model"],
                "model": LOCAL_FAST_CONTRACT["model"],
                "max_model_len": LOCAL_FAST_CONTRACT["configured_context_tokens"],
            },
            f"{label}/qwen service contract",
        )
        argv = qwen["argv"]
        require_argv_option(
            argv, "--model", LOCAL_FAST_CONTRACT["model"], f"{label}/qwen"
        )
        require_argv_option(
            argv, "--revision", LOCAL_FAST_CONTRACT["revision"], f"{label}/qwen"
        )
        require_argv_option(
            argv,
            "--served-model-name",
            LOCAL_FAST_CONTRACT["served_model"],
            f"{label}/qwen",
        )
        require_argv_option(
            argv,
            "--max-model-len",
            str(LOCAL_FAST_CONTRACT["configured_context_tokens"]),
            f"{label}/qwen",
        )
        require_argv_option(
            argv,
            "--gpu-memory-utilization",
            LOCAL_FAST_CONTRACT["gpu_memory_utilization"],
            f"{label}/qwen",
        )
        require_argv_option(argv, "--tool-call-parser", "hermes", f"{label}/qwen")
        require_equal(
            argv.count("--enable-auto-tool-choice"),
            1,
            f"{label}/qwen auto-tool-choice occurrence",
        )


def validate_run_context(
    root: Path,
    path_value: str,
    *,
    benchmark: str,
    expected_gateway_sha256: str,
    label: str,
) -> tuple[Path, dict[str, Any]]:
    path = resolve(root, path_value)
    context = read_json(path, f"{label} run context")
    require_equal(context.get("schema_version"), "1.0.0", f"{label} context schema")
    require_equal(context.get("benchmark"), benchmark, f"{label} context benchmark")
    require_equal(context.get("status"), "complete", f"{label} context status")
    invocations = context.get("invocations")
    require(
        isinstance(invocations, list) and bool(invocations),
        f"{label} context invocation ledger is absent",
    )
    require_equal(
        invocations[-1].get("status"), "complete", f"{label} final invocation"
    )
    for index, invocation in enumerate(invocations):
        invocation_label = f"{label} invocation {index}"
        require(
            invocation.get("status") in {"complete", "interrupted"},
            f"{invocation_label} has a non-terminal status",
        )
        require_equal(
            invocation.get("source_worktree_clean_start"),
            True,
            f"{invocation_label} clean source",
        )
        require_equal(
            invocation.get("study_gateway_sha256"),
            expected_gateway_sha256,
            f"{invocation_label} frozen gateway declaration",
        )
        revision = invocation.get("openrealtime_revision_start")
        require(
            isinstance(revision, str)
            and len(revision) == 40
            and all(character in "0123456789abcdef" for character in revision),
            f"{invocation_label} has an invalid source revision",
        )
        start = invocation.get("runtime_identity_start")
        validate_runtime_identity(
            start,
            requires_local_fast=True,
            expected_gateway_sha256=expected_gateway_sha256,
            label=f"{invocation_label} start",
        )
        snapshot = invocation.get("gateway_health_start")
        require(
            isinstance(snapshot, dict) and snapshot.get("status") == "ok",
            f"{invocation_label} gateway_health_start is not healthy",
        )
        if invocation["status"] == "complete":
            require_equal(
                invocation.get("source_worktree_clean_at_completion"),
                True,
                f"{invocation_label} clean source at completion",
            )
            require_equal(
                invocation.get("openrealtime_revision_at_completion"),
                revision,
                f"{invocation_label} source revision at completion",
            )
            final = invocation.get("runtime_identity_final")
            validate_runtime_identity(
                final,
                requires_local_fast=True,
                expected_gateway_sha256=expected_gateway_sha256,
                label=f"{invocation_label} final",
            )
            require_equal(
                start.get("host_boot_id"),
                final.get("host_boot_id"),
                f"{invocation_label} runtime boot",
            )
            require_equal(
                start.get("components"),
                final.get("components"),
                f"{invocation_label} runtime processes",
            )
            final_health = invocation.get("gateway_health_final")
            require(
                isinstance(final_health, dict) and final_health.get("status") == "ok",
                f"{invocation_label} gateway_health_final is not healthy",
            )
    return path, context


def validate_tau_artifact_archive(
    root: Path,
    *,
    matrix_path: Path,
    matrix: dict[str, Any],
    matrix_sha256: str,
    cell_id: str,
    domain: str,
) -> dict[str, Any]:
    experiment = (
        root
        / ".runtime/tau2-bench/data/simulations"
        / (
            f"{matrix['matrix_id']}-{cell_id}-{domain}-seed"
            f"{matrix['benchmark']['seed']}"
        )
    )
    evidence_path = experiment / "raw-artifacts-archive.json"
    evidence = read_json(
        evidence_path,
        f"{matrix['matrix_id']}/{cell_id}/{domain} raw-artifact archive",
    )
    label = f"{matrix['matrix_id']}/{cell_id}/{domain} raw-artifact archive"
    require_equal(evidence.get("schema_version"), "1.0.0", f"{label} schema")
    require_equal(
        evidence.get("matrix"),
        {
            "path": display_path(root, matrix_path),
            "id": matrix["matrix_id"],
            "sha256": matrix_sha256,
        },
        f"{label} matrix",
    )
    require_equal(
        evidence.get("population"),
        {"cell": cell_id, "domain": domain},
        f"{label} population",
    )
    source = evidence.get("source")
    require(isinstance(source, dict), f"{label} source declaration is absent")
    require_equal(source.get("path"), "artifacts", f"{label} source path")
    for field in ("files", "bytes"):
        require(
            isinstance(source.get(field), int)
            and not isinstance(source[field], bool)
            and source[field] > 0,
            f"{label} source {field} is invalid",
        )
    attempts = evidence.get("attempts")
    require(isinstance(attempts, dict), f"{label} attempt declaration is absent")
    domain_specification = next(
        item for item in matrix["benchmark"]["domains"] if item["name"] == domain
    )
    expected_tasks = domain_specification["tasks"] * matrix["benchmark"]["num_trials"]
    maximum_attempts = matrix["reporting"]["infrastructure_retry_policy"][
        "maximum_attempts"
    ]
    require_equal(attempts.get("tasks"), expected_tasks, f"{label} attempt tasks")
    require_equal(
        attempts.get("successful"), expected_tasks, f"{label} successful attempts"
    )
    require_equal(
        attempts.get("total"),
        attempts.get("successful", 0) + attempts.get("failed_infrastructure", 0),
        f"{label} total attempts",
    )
    require_equal(
        attempts.get("maximum_allowed"), maximum_attempts, f"{label} attempt bound"
    )
    require(
        isinstance(attempts.get("maximum_observed"), int)
        and 1 <= attempts["maximum_observed"] <= maximum_attempts,
        f"{label} observed attempt maximum is invalid",
    )
    require(
        isinstance(attempts.get("retried_tasks"), int)
        and 0 <= attempts["retried_tasks"] <= expected_tasks,
        f"{label} retried task count is invalid",
    )
    require_equal(
        attempts.get("retry_delay_seconds"),
        matrix["reporting"]["infrastructure_retry_policy"]["retry_delay_seconds"],
        f"{label} retry delay",
    )
    require_equal(attempts.get("seed_reused"), True, f"{label} retry seed policy")
    require_equal(
        attempts.get("retry_scope"), "exceptions_only", f"{label} retry scope"
    )
    require_equal(
        attempts.get("semantic_outcomes_retried"),
        False,
        f"{label} semantic retry policy",
    )
    archive_evidence = evidence.get("archive")
    require(
        isinstance(archive_evidence, dict), f"{label} archive declaration is absent"
    )
    require_equal(
        archive_evidence.get("path"), "raw-artifacts.tar.zst", f"{label} path"
    )
    require_equal(
        archive_evidence.get("format"),
        "deterministic-pax-tar+zstd",
        f"{label} format",
    )
    require(valid_sha256(archive_evidence.get("sha256")), f"{label} hash is invalid")
    archive_path = experiment / "raw-artifacts.tar.zst"
    require(archive_path.is_file(), f"{label} is missing: {archive_path}")
    require_equal(
        archive_path.stat().st_size, archive_evidence.get("bytes"), f"{label} bytes"
    )
    require_equal(
        sha256_file(archive_path), archive_evidence["sha256"], f"{label} SHA-256"
    )
    require(
        not (experiment / "artifacts").exists(),
        f"{label} expanded duplicate artifacts remain",
    )
    return {
        "cell": cell_id,
        "domain": domain,
        "source": source,
        "attempts": attempts,
        "archive": artifact(root, archive_path),
        "evidence": artifact(root, evidence_path),
    }


def validate_tau_matrix(
    root: Path,
    matrix_specification: dict[str, Any],
    *,
    expected_gateway_source_revision: str,
    expected_gateway_sha256: str,
) -> dict[str, Any]:
    matrix_path, matrix = load_pin(root, matrix_specification, "tau matrix")
    matrix_id = matrix.get("matrix_id")
    require(isinstance(matrix_id, str) and matrix_id, f"{matrix_path} has no matrix_id")
    gateway_requirement = matrix.get("runtime_requirements", {}).get("gateway", {})
    require_equal(
        gateway_requirement.get("source_revision"),
        expected_gateway_source_revision,
        f"{matrix_id} frozen gateway source",
    )
    require_equal(
        gateway_requirement.get("executable_sha256"),
        expected_gateway_sha256,
        f"{matrix_id} frozen gateway executable",
    )
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
    infrastructure_retries = matrix["benchmark"].get("infrastructure_retries")
    retry_policy = matrix.get("reporting", {}).get("infrastructure_retry_policy", {})
    require(
        isinstance(infrastructure_retries, int)
        and not isinstance(infrastructure_retries, bool)
        and infrastructure_retries >= 0,
        f"{matrix_id} infrastructure retry count is invalid",
    )
    require_equal(
        retry_policy.get("maximum_retries"),
        infrastructure_retries,
        f"{matrix_id} retry count",
    )
    require_equal(
        retry_policy.get("maximum_attempts"),
        infrastructure_retries + 1,
        f"{matrix_id} attempt count",
    )
    require_equal(
        retry_policy.get("retry_delay_seconds"),
        matrix["benchmark"].get("infrastructure_retry_delay_seconds"),
        f"{matrix_id} retry delay",
    )
    require_equal(
        retry_policy.get("seed_reused"), True, f"{matrix_id} retry seed policy"
    )
    require_equal(
        retry_policy.get("scope"), "exceptions_only", f"{matrix_id} retry scope"
    )
    require_equal(
        retry_policy.get("semantic_outcomes_retried"),
        False,
        f"{matrix_id} semantic retry policy",
    )
    require_equal(
        matrix.get("transport", {}).get("ping_interval_seconds"),
        20,
        f"{matrix_id} ping interval",
    )
    require_equal(
        matrix.get("transport", {}).get("ping_timeout_seconds"),
        0,
        f"{matrix_id} ping timeout",
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
        report.get("execution_policy"),
        {
            "transport": {
                "ping_interval_seconds": 20,
                "ping_timeout_seconds": 0,
            },
            "infrastructure_retries": retry_policy,
        },
        f"{matrix_id} report execution policy",
    )
    require_equal(
        matrix_evidence.get("selected_cells"), cell_ids, f"{matrix_id} selected cells"
    )
    require_equal(
        report.get("benchmark", {}).get("revision"),
        matrix["benchmark"]["revision"],
        f"{matrix_id} tau revision",
    )
    require_equal(
        set(report.get("cells", {})), set(cell_ids), f"{matrix_id} report cells"
    )

    total_simulations = 0
    infrastructure_errors = 0
    termination_reasons: Counter[str] = Counter()
    cell_panel: dict[str, Any] = {}
    raw_artifact_archives: list[dict[str, Any]] = []
    for cell_id in cell_ids:
        cell_report = report["cells"][cell_id]
        require_equal(
            set(cell_report.get("domains", {})),
            set(domains),
            f"{matrix_id}/{cell_id} domains",
        )
        cell_simulations = 0
        cell_errors = 0
        cell_termination_reasons: Counter[str] = Counter()
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
            reasons = population.get("termination_reasons")
            require(
                isinstance(reasons, dict)
                and all(
                    isinstance(reason, str)
                    and reason
                    and isinstance(count, int)
                    and not isinstance(count, bool)
                    and count >= 0
                    for reason, count in reasons.items()
                ),
                f"{matrix_id}/{cell_id}/{domain_name} has invalid termination reasons",
            )
            require_equal(
                sum(reasons.values()),
                expected_simulations,
                f"{matrix_id}/{cell_id}/{domain_name} termination population",
            )
            require_equal(
                reasons.get("infrastructure_error", 0),
                errors,
                f"{matrix_id}/{cell_id}/{domain_name} infrastructure reconciliation",
            )
            raw_artifact_archives.append(
                validate_tau_artifact_archive(
                    root,
                    matrix_path=matrix_path,
                    matrix=matrix,
                    matrix_sha256=matrix_specification["sha256"],
                    cell_id=cell_id,
                    domain=domain_name,
                )
            )
            cell_simulations += expected_simulations
            cell_errors += errors
            cell_termination_reasons.update(reasons)
        total_simulations += cell_simulations
        infrastructure_errors += cell_errors
        termination_reasons.update(cell_termination_reasons)
        cell_panel[cell_id] = {
            "simulations": cell_simulations,
            "infrastructure_errors": cell_errors,
            "termination_reasons": dict(sorted(cell_termination_reasons.items())),
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
    requires_local_fast = matrix.get("runtime_requirements", {}).get(
        "requires_local_fast", True
    )
    for index, complete_run in enumerate(complete_runs):
        execution_label = f"{matrix_id} complete execution {index}"
        revision = complete_run.get("openrealtime_revision")
        require(
            isinstance(revision, str)
            and len(revision) == 40
            and all(character in "0123456789abcdef" for character in revision),
            f"{execution_label} source revision is invalid",
        )
        require_equal(
            complete_run.get("source_worktree_clean_start"),
            True,
            f"{execution_label} clean source at start",
        )
        require_equal(
            complete_run.get("source_worktree_clean_final"),
            True,
            f"{execution_label} clean source at completion",
        )
        require_equal(
            complete_run.get("openrealtime_revision_final"),
            revision,
            f"{execution_label} source revision at completion",
        )
        validate_runtime_identity(
            complete_run.get("runtime_identity"),
            requires_local_fast=requires_local_fast,
            expected_gateway_sha256=expected_gateway_sha256,
            label=f"{execution_label} start",
        )
        validate_runtime_identity(
            complete_run.get("runtime_identity_final"),
            requires_local_fast=requires_local_fast,
            expected_gateway_sha256=expected_gateway_sha256,
            label=f"{execution_label} final",
        )
        require_equal(
            complete_run["runtime_identity"].get("host_boot_id"),
            complete_run["runtime_identity_final"].get("host_boot_id"),
            f"{execution_label} runtime boot",
        )
        require_equal(
            complete_run["runtime_identity"].get("components"),
            complete_run["runtime_identity_final"].get("components"),
            f"{execution_label} runtime processes",
        )
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
        "termination_reasons": dict(sorted(termination_reasons.items())),
        "openrealtime_revisions": revisions,
        "raw_artifact_archives": raw_artifact_archives,
        "cells": cell_panel,
    }


def validate_tau(
    root: Path,
    specification: dict[str, Any],
    *,
    expected_gateway_source_revision: str,
    expected_gateway_sha256: str,
) -> dict[str, Any]:
    artifact_retention = specification.get("artifact_retention")
    require_equal(
        artifact_retention,
        {
            "scoring_inputs": "results.json and simulations/*.json remain expanded",
            "raw_artifacts": "each complete cell/domain artifacts directory is preserved losslessly as deterministic-pax-tar+zstd",
            "deletion_gate": "remove expanded duplicates only after archive readability, SHA-256, byte count, matrix identity, and exact task population are recorded",
            "publication_gate": "the terminal reporter rehashes every archive and rejects missing evidence or remaining expanded duplicates",
        },
        "tau artifact-retention policy",
    )
    source_path, source = load_pin(
        root, specification["source_manifest"], "tau source manifest"
    )
    require_equal(
        source.get("benchmark", {}).get("name"), "tau-Voice", "tau source benchmark"
    )
    matrix_panel = [
        validate_tau_matrix(
            root,
            item,
            expected_gateway_source_revision=expected_gateway_source_revision,
            expected_gateway_sha256=expected_gateway_sha256,
        )
        for item in specification["matrices"]
    ]
    matrix_ids = [item["matrix_id"] for item in matrix_panel]
    require_equal(len(matrix_ids), len(set(matrix_ids)), "unique tau matrix IDs")

    paired_panel = []
    for pair in specification.get("paired_reports", []):
        definition_path, definition = load_pin(
            root, pair["definition"], f"tau paired definition {pair['id']}"
        )
        require_equal(
            definition.get("ablation_id"), pair["id"], "tau paired definition ID"
        )
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
            ablation.get("sha256"),
            pair["definition"]["sha256"],
            f"{pair['id']} definition hash",
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
        "artifact_retention": artifact_retention,
        "matrices": matrix_panel,
        "paired_reports": paired_panel,
        "population_across_preregistered_conditions": sum(
            item["population"] for item in matrix_panel
        ),
    }


def validate_fdb15(
    root: Path, specification: dict[str, Any], *, expected_gateway_sha256: str
) -> dict[str, Any]:
    source_path, source = load_pin(
        root, specification["source_manifest"], "FDB1.5 source manifest"
    )
    require_equal(source.get("benchmark"), "Full-Duplex-Bench v1.5", "FDB1.5 source")
    require_equal(
        source.get("upstream_revision"),
        specification["upstream_revision"],
        "FDB1.5 revision",
    )
    scenario_population = {
        item["scenario"]: item["observed_complete_samples"]
        for item in source["archives"]
    }
    require_equal(
        sum(scenario_population.values()),
        specification["population"],
        "FDB1.5 source population",
    )
    context_path, context = validate_run_context(
        root,
        specification["run_context"],
        benchmark="full-duplex-bench-v1.5",
        expected_gateway_sha256=expected_gateway_sha256,
        label="FDB1.5",
    )

    run_path = resolve(root, specification["run_manifest"])
    run = read_json(run_path, "FDB1.5 run manifest")
    require_equal(run.get("schema_version"), "1.2.0", "FDB1.5 run schema")
    require_equal(
        run.get("benchmark"), "full-duplex-bench-v1.5", "FDB1.5 run benchmark"
    )
    require_equal(
        run.get("revision"), specification["upstream_revision"], "FDB1.5 run revision"
    )
    require_equal(
        run.get("conditions"), specification["conditions"], "FDB1.5 conditions"
    )
    require_equal(
        run.get("replicates"), specification["replicates"], "FDB1.5 replicates"
    )
    require_equal(
        specification.get("trial_attempts"), 3, "FDB1.5 preregistered attempts"
    )
    require_equal(
        run.get("trial_attempts"),
        specification["trial_attempts"],
        "FDB1.5 run attempt budget",
    )
    validate_local_descriptor(
        run.get("descriptor", {}),
        profile="fdb-v1.5-openai-realtime-adapter-i1-qg-v1",
        label="FDB1.5",
    )
    samples = run.get("samples", [])
    require_equal(
        len(samples), specification["population"], "FDB1.5 planned population"
    )
    sample_rows = [(item.get("scenario"), item.get("id")) for item in samples]
    require_equal(len(sample_rows), len(set(sample_rows)), "FDB1.5 unique samples")
    require_equal(
        Counter(item[0] for item in sample_rows),
        Counter(scenario_population),
        "FDB1.5 scenarios",
    )
    completed = run.get("completed", [])
    require_equal(
        len(completed), specification["population"], "FDB1.5 completed population"
    )
    require_equal(len(run.get("failures", [])), 0, "FDB1.5 terminal failures")
    expected_trials = {
        (scenario, sample_id, condition, replicate)
        for scenario, sample_id in sample_rows
        for condition in specification["conditions"]
        for replicate in range(specification["replicates"])
    }
    attempt_summary, successful_attempts = validate_attempt_ledger(
        run.get("attempts"),
        expected=expected_trials,
        maximum=specification["trial_attempts"],
        identity=lambda item: (
            item.get("scenario"),
            item.get("sample_id"),
            item.get("condition"),
            item.get("replicate"),
        ),
        number_field="attempt",
        label="FDB1.5",
    )
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
    output_entries: list[tuple[Path, str | None]] = []
    result_entries: list[tuple[Path, str | None]] = []
    for item in completed:
        replicate_label = item.get("trial_id", "").rsplit("/", 1)[-1]
        require(
            len(replicate_label) == 4
            and replicate_label.startswith("r")
            and replicate_label[1:].isdigit(),
            f"FDB1.5 completed trial has invalid replicate identity: {item.get('trial_id')!r}",
        )
        completed_identity = (
            item["sample"]["scenario"],
            item["sample"]["id"],
            item["condition"],
            int(replicate_label[1:]),
        )
        require_equal(
            item.get("attempt"),
            successful_attempts.get(completed_identity),
            f"FDB1.5 {item['trial_id']} successful attempt",
        )
        output_path = resolve(root, item.get("output_wav", ""))
        output_entries.append((output_path, item.get("output_sha256")))
        result_entries.append(
            (output_path.parent / f"result_{item['condition']}.json", None)
        )
    output_tree = artifact_tree(root, output_entries, label="FDB1.5 output audio")
    result_tree = artifact_tree(root, result_entries, label="FDB1.5 raw results")

    summary_path = resolve(root, specification["summary"])
    summary = read_json(summary_path, "FDB1.5 summary")
    require_equal(summary.get("schema_version"), "1.2.0", "FDB1.5 summary schema")
    require_equal(
        summary.get("benchmark"), run["benchmark"], "FDB1.5 summary benchmark"
    )
    require_equal(summary.get("revision"), run["revision"], "FDB1.5 summary revision")
    conditions = summary.get("conditions", [])
    require_equal(
        len(conditions), len(scenario_population), "FDB1.5 summary conditions"
    )
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
        "run_context": artifact(root, context_path),
        "openrealtime_revisions": sorted(
            {item["openrealtime_revision_start"] for item in context["invocations"]}
        ),
        "run_manifest": artifact(root, run_path),
        "summary": artifact(root, summary_path),
        "population": len(completed),
        "terminal_failures": 0,
        "attempt_ledger": attempt_summary,
        "raw_results": result_tree,
        "output_audio": output_tree,
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
        {item.get("scenario_id") for item in scenarios},
        sample_ids,
        f"{label} scenario IDs",
    )
    for scenario in scenarios:
        score = scenario.get("metrics", {}).get("response_qual", {}).get("score")
        if llm_judge:
            require(
                isinstance(score, (int, float)) and not isinstance(score, bool),
                f"{label}/{scenario.get('scenario_id')} lacks an LLM response score",
            )
        else:
            require_equal(
                score,
                None,
                f"{label}/{scenario.get('scenario_id')} exact-only response score",
            )
    aggregate_score = report.get("by_metric", {}).get("response_qual")
    if llm_judge:
        require(
            isinstance(aggregate_score, (int, float))
            and not isinstance(aggregate_score, bool),
            f"{label} lacks the aggregate LLM response score",
        )
    else:
        require_equal(
            aggregate_score, None, f"{label} exact-only aggregate response score"
        )
    return report


def validate_fdbv3_judge_evidence(
    root: Path,
    path_value: str,
    *,
    evaluation_path: Path,
    evaluator_sha256: str,
    scenarios: int,
) -> tuple[Path, dict[str, Any]]:
    path = resolve(root, path_value)
    evidence = read_json(path, "FDBv3 GPT-4o call evidence")
    require_equal(
        evidence.get("schema_version"), "1.1.0", "FDBv3 judge evidence schema"
    )
    require_equal(evidence.get("status"), "complete", "FDBv3 judge evidence status")
    require_equal(
        evidence.get("scenarios"), scenarios, "FDBv3 judge evidence scenarios"
    )
    require_equal(
        evidence.get("api_origin"),
        "https://api.openai.com/v1",
        "FDBv3 judge API origin",
    )
    expected = evidence.get("expected_calls", {})
    require(
        all(
            isinstance(expected.get(name), int)
            and not isinstance(expected[name], bool)
            and expected[name] >= 0
            for name in ("argument", "response", "total")
        ),
        "FDBv3 judge expected-call counts are invalid",
    )
    require_equal(
        expected["argument"] + expected["response"],
        expected["total"],
        "FDBv3 judge expected-call reconciliation",
    )
    require(expected["total"] > 0, "FDBv3 judge opened no calls")
    require_equal(
        evidence.get("successful_valid_calls"),
        expected["total"],
        "FDBv3 successful judge calls",
    )
    calls = evidence.get("calls")
    require(isinstance(calls, list), "FDBv3 judge call ledger is absent")
    require_equal(len(calls), expected["total"], "FDBv3 judge call ledger length")
    require_equal(
        [call.get("sequence") for call in calls],
        list(range(len(calls))),
        "FDBv3 judge call sequence",
    )
    require(
        all(call.get("requested_model") == "gpt-4o" for call in calls),
        "FDBv3 judge used a model other than gpt-4o",
    )
    response_ids: list[str] = []
    for index, call in enumerate(calls):
        call_label = f"FDBv3 judge call {index}"
        response_id = call.get("response_id")
        require(
            isinstance(response_id, str) and bool(response_id),
            f"{call_label} has no response ID",
        )
        response_ids.append(response_id)
        response_model = call.get("response_model")
        require(
            isinstance(response_model, str)
            and (response_model == "gpt-4o" or response_model.startswith("gpt-4o-")),
            f"{call_label} has an invalid response model",
        )
        for field in (
            "request_sha256",
            "response_sha256",
            "parsed_response_sha256",
        ):
            require(
                valid_sha256(call.get(field)), f"{call_label} has an invalid {field}"
            )
        usage = call.get("usage")
        require(
            isinstance(usage, dict)
            and isinstance(usage.get("total_tokens"), int)
            and not isinstance(usage["total_tokens"], bool)
            and usage["total_tokens"] > 0,
            f"{call_label} has invalid token usage",
        )
    require_equal(
        len(set(response_ids)),
        len(response_ids),
        "FDBv3 judge unique response IDs",
    )
    require_equal(
        evidence.get("evaluator", {}).get("sha256"),
        evaluator_sha256,
        "FDBv3 judge evaluator hash",
    )
    require_equal(
        evidence.get("evaluation", {}).get("sha256"),
        sha256_file(evaluation_path),
        "FDBv3 judge evaluation hash",
    )
    return path, evidence


def validate_fdbv3(
    root: Path, specification: dict[str, Any], *, expected_gateway_sha256: str
) -> dict[str, Any]:
    source_path, source = load_pin(
        root, specification["source_manifest"], "FDBv3 source manifest"
    )
    profile_path, profile = load_pin(root, specification["profile"], "FDBv3 profile")
    require_equal(source.get("benchmark"), "Full-Duplex-Bench v3", "FDBv3 source")
    require_equal(
        source.get("upstream_revision"),
        specification["upstream_revision"],
        "FDBv3 revision",
    )
    require_equal(
        source["released_artifact"]["audio_examples"],
        specification["population"],
        "FDBv3 source population",
    )
    require_equal(
        profile.get("source", {}).get("revision"),
        specification["upstream_revision"],
        "FDBv3 profile revision",
    )
    context_path, context = validate_run_context(
        root,
        specification["run_context"],
        benchmark="full-duplex-bench-v3",
        expected_gateway_sha256=expected_gateway_sha256,
        label="FDBv3",
    )

    run_path = resolve(root, specification["run_manifest"])
    run = read_json(run_path, "FDBv3 run manifest")
    require_equal(run.get("schema_version"), "1.0.0", "FDBv3 run schema")
    require_equal(run.get("benchmark"), "Full-Duplex-Bench v3", "FDBv3 benchmark")
    require_equal(
        run.get("revision"), specification["upstream_revision"], "FDBv3 run revision"
    )
    require_equal(run.get("profile"), profile["profile"], "FDBv3 run profile")
    require_equal(
        run.get("profile_sha256"),
        specification["profile"]["sha256"],
        "FDBv3 run profile hash",
    )
    require_equal(
        specification.get("trial_attempts"), 3, "FDBv3 preregistered attempts"
    )
    require_equal(
        run.get("trial_attempts"),
        specification["trial_attempts"],
        "FDBv3 run attempt budget",
    )
    validate_local_descriptor(
        run.get("descriptor", {}), profile=profile["profile"], label="FDBv3"
    )
    samples = run.get("samples", [])
    require_equal(len(samples), specification["population"], "FDBv3 planned population")
    labels = {f"{item['example_id']}_{item['pid']}" for item in samples}
    sample_ids = {item["example_id"] for item in samples}
    require_equal(len(labels), len(samples), "FDBv3 unique sample labels")
    require_equal(len(sample_ids), len(samples), "FDBv3 unique example IDs")
    require_equal(set(run.get("completed", [])), labels, "FDBv3 completed samples")
    require_equal(len(run.get("completed", [])), len(labels), "FDBv3 completed count")
    require_equal(len(run.get("failures", [])), 0, "FDBv3 terminal failures")
    attempt_summary, _ = validate_attempt_ledger(
        run.get("attempts"),
        expected=labels,
        maximum=specification["trial_attempts"],
        identity=lambda item: item.get("sample"),
        number_field="number",
        label="FDBv3",
    )
    output_entries: list[tuple[Path, str | None]] = []
    result_entries: list[tuple[Path, str | None]] = []
    for sample in samples:
        directory = resolve(root, sample["directory"])
        output_path = directory / "output_openrealtime.wav"
        result_path = directory / "result_openrealtime.json"
        result = read_json(result_path, f"FDBv3 raw result {sample['example_id']}")
        require_equal(
            result.get("openrealtime_schema_version"),
            "1.0.0",
            f"FDBv3 {sample['example_id']} result schema",
        )
        require_equal(
            result.get("status"), "completed", f"FDBv3 {sample['example_id']} status"
        )
        require_equal(
            result.get("pid"), sample["pid"], f"FDBv3 {sample['example_id']} PID"
        )
        require_equal(
            result.get("example_id"),
            sample["example_id"],
            f"FDBv3 {sample['example_id']} ID",
        )
        require_equal(
            result.get("provider"),
            "openrealtime",
            f"FDBv3 {sample['example_id']} provider",
        )
        evidence = result.get("openrealtime", {})
        require_equal(
            evidence.get("benchmark_revision"),
            specification["upstream_revision"],
            f"FDBv3 {sample['example_id']} evidence revision",
        )
        require_equal(
            evidence.get("profile_sha256"),
            specification["profile"]["sha256"],
            f"FDBv3 {sample['example_id']} evidence profile",
        )
        require_equal(
            evidence.get("input_sha256"),
            sample["input_sha256"],
            f"FDBv3 {sample['example_id']} input hash",
        )
        require_equal(
            evidence.get("metadata_sha256"),
            sample["metadata_sha256"],
            f"FDBv3 {sample['example_id']} metadata hash",
        )
        output_entries.append((output_path, evidence.get("output_sha256")))
        result_entries.append((result_path, None))
    output_tree = artifact_tree(root, output_entries, label="FDBv3 output audio")
    result_tree = artifact_tree(root, result_entries, label="FDBv3 raw results")

    exact_path = resolve(root, specification["evaluations"]["exact"])
    judge_path = resolve(root, specification["evaluations"]["gpt4o"])
    exact = validate_fdbv3_evaluation(
        exact_path,
        sample_ids=sample_ids,
        llm_judge=False,
        label="FDBv3 exact evaluation",
    )
    judge = validate_fdbv3_evaluation(
        judge_path,
        sample_ids=sample_ids,
        llm_judge=True,
        label="FDBv3 GPT-4o evaluation",
    )
    judge_evidence_path, judge_evidence = validate_fdbv3_judge_evidence(
        root,
        specification["evaluations"]["gpt4o_evidence"],
        evaluation_path=judge_path,
        evaluator_sha256=source["official_harness"]["evaluator_sha256"],
        scenarios=len(sample_ids),
    )
    return {
        "source_manifest": artifact(root, source_path),
        "run_context": artifact(root, context_path),
        "openrealtime_revisions": sorted(
            {item["openrealtime_revision_start"] for item in context["invocations"]}
        ),
        "profile": artifact(root, profile_path),
        "run_manifest": artifact(root, run_path),
        "population": len(labels),
        "terminal_failures": 0,
        "attempt_ledger": attempt_summary,
        "raw_results": result_tree,
        "output_audio": output_tree,
        "evaluations": {
            "exact": {
                "artifact": artifact(root, exact_path),
                "turn_taking": exact.get("turn_taking"),
                "by_metric": exact.get("by_metric"),
                "latency": exact.get("latency"),
            },
            "gpt4o": {
                "artifact": artifact(root, judge_path),
                "call_evidence": artifact(root, judge_evidence_path),
                "judge_calls": {
                    "expected": judge_evidence["expected_calls"],
                    "successful_valid": judge_evidence["successful_valid_calls"],
                },
                "turn_taking": judge.get("turn_taking"),
                "by_metric": judge.get("by_metric"),
                "latency": judge.get("latency"),
            },
        },
    }


def validate_fdbench(
    root: Path, specification: dict[str, Any], *, expected_gateway_sha256: str
) -> dict[str, Any]:
    source_path, source = load_pin(
        root, specification["source_manifest"], "FD-Bench source manifest"
    )
    require_equal(
        source.get("upstream_revision"),
        specification["upstream_revision"],
        "FD-Bench revision",
    )
    require_equal(
        source.get("dataset_revision"),
        specification["dataset_revision"],
        "FD-Bench dataset revision",
    )
    require_equal(
        source.get("expected_released_conversations"),
        specification["population"],
        "FD-Bench source population",
    )
    require_equal(
        source.get("expected_cell_count"),
        specification["cells"],
        "FD-Bench source cells",
    )
    expected_cells = source["expected_cell_populations"]
    context_path, context = validate_run_context(
        root,
        specification["run_context"],
        benchmark="fd-bench",
        expected_gateway_sha256=expected_gateway_sha256,
        label="FD-Bench",
    )

    run_path = resolve(root, specification["run_manifest"])
    run = read_json(run_path, "FD-Bench run manifest")
    require_equal(run.get("schema_version"), "1.0.0", "FD-Bench run schema")
    require_equal(run.get("benchmark"), "FD-Bench", "FD-Bench run benchmark")
    require_equal(
        run.get("revision"), specification["upstream_revision"], "FD-Bench run revision"
    )
    require_equal(
        run.get("dataset_revision"),
        specification["dataset_revision"],
        "FD-Bench run dataset revision",
    )
    require_equal(
        specification.get("trial_attempts"), 3, "FD-Bench preregistered attempts"
    )
    require_equal(
        run.get("trial_attempts"),
        specification["trial_attempts"],
        "FD-Bench run attempt budget",
    )
    validate_local_descriptor(
        run.get("descriptor", {}),
        profile="fd-bench-standard-realtime-v1",
        label="FD-Bench",
    )
    samples = run.get("samples", [])
    require_equal(
        len(samples), specification["population"], "FD-Bench planned population"
    )
    labels = {f"{item['cell']}/{item['id']}" for item in samples}
    require_equal(len(labels), len(samples), "FD-Bench unique samples")
    require_equal(
        Counter(item["cell"] for item in samples),
        Counter(expected_cells),
        "FD-Bench cell populations",
    )
    require_equal(set(run.get("completed", [])), labels, "FD-Bench completed samples")
    require_equal(
        len(run.get("completed", [])), len(labels), "FD-Bench completed count"
    )
    require_equal(len(run.get("failures", [])), 0, "FD-Bench terminal failures")
    attempt_summary, _ = validate_attempt_ledger(
        run.get("attempts"),
        expected=labels,
        maximum=specification["trial_attempts"],
        identity=lambda item: item.get("sample"),
        number_field="number",
        label="FD-Bench",
    )

    finalization_path = resolve(root, specification["finalization"])
    finalization = read_json(finalization_path, "FD-Bench finalization")
    require_equal(
        finalization.get("benchmark"), "FD-Bench", "FD-Bench finalization benchmark"
    )
    require_equal(
        finalization.get("revision"),
        specification["upstream_revision"],
        "FD-Bench finalization revision",
    )
    require_equal(
        finalization.get("results"),
        specification["population"],
        "FD-Bench finalized population",
    )
    validate_tree_summary(
        finalization.get("result_evidence"),
        files=specification["population"],
        label="FD-Bench raw results",
    )
    validate_tree_summary(
        finalization.get("audio_evidence"),
        files=specification["population"],
        label="FD-Bench output audio",
    )
    vad = finalization.get("vad", {})
    contract = source["evaluation_contract"]
    require_equal(vad.get("name"), contract["output_vad"], "FD-Bench VAD")
    require_equal(
        vad.get("package_version"),
        contract["output_vad_package_version"],
        "FD-Bench VAD version",
    )
    require_equal(
        vad.get("threshold"), contract["output_vad_threshold"], "FD-Bench VAD threshold"
    )
    require_equal(
        vad.get("min_silence_duration_ms"),
        contract["output_min_silence_duration_ms"],
        "FD-Bench VAD silence",
    )
    require_equal(
        vad.get("timestamp_rate_hz"),
        contract["input_timestamp_rate_hz"],
        "FD-Bench VAD clock",
    )
    traces = finalization.get("traces", {})
    require_equal(set(traces), set(expected_cells), "FD-Bench finalized trace cells")

    expected_metric_names = {
        "SRR_pct",
        "SIR_pct",
        "EIR_pct",
        "NIR_pct",
        "SRIR_pct",
        "FSED_ms",
        "ERT_ms",
        "EIT_ms",
        "IRD_ms",
    }
    metrics_directory = resolve(root, specification["metrics_directory"])
    metric_paths = (
        sorted(metrics_directory.glob("*.json")) if metrics_directory.is_dir() else []
    )
    require_equal(
        {path.stem for path in metric_paths},
        set(expected_cells),
        "FD-Bench metric cells",
    )
    metric_panel: dict[str, Any] = {}
    for cell, expected_population in expected_cells.items():
        trace = traces[cell]
        require_equal(
            trace.get("samples"),
            expected_population,
            f"FD-Bench {cell} trace population",
        )
        trace_path = resolve(root, trace["path"])
        require(trace_path.is_file(), f"FD-Bench {cell} trace is missing: {trace_path}")
        require_equal(
            sha256_file(trace_path), trace.get("sha256"), f"FD-Bench {cell} trace hash"
        )
        metric_path = metrics_directory / f"{cell}.json"
        metric = read_json(metric_path, f"FD-Bench metric {cell}")
        require_equal(
            metric.get("schema_version"), "1.0.0", f"FD-Bench {cell} metric schema"
        )
        require_equal(
            metric.get("benchmark"), "FD-Bench", f"FD-Bench {cell} metric benchmark"
        )
        require_equal(
            metric.get("revision"),
            specification["upstream_revision"],
            f"FD-Bench {cell} metric revision",
        )
        require_equal(
            metric.get("trace", {}).get("samples"),
            expected_population,
            f"FD-Bench {cell} metric population",
        )
        require_equal(
            metric.get("trace", {}).get("sha256"),
            trace["sha256"],
            f"FD-Bench {cell} metric trace hash",
        )
        require_equal(
            resolve(root, metric.get("trace", {}).get("path", trace["path"])),
            trace_path,
            f"FD-Bench {cell} metric trace path",
        )
        require_equal(
            set(metric.get("metrics", {})),
            expected_metric_names,
            f"FD-Bench {cell} objective metrics",
        )
        require_equal(
            set(metric.get("not_evaluated", {})),
            set(specification["explicit_exclusions"]),
            f"FD-Bench {cell} exclusions",
        )
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
        "run_context": artifact(root, context_path),
        "openrealtime_revisions": sorted(
            {item["openrealtime_revision_start"] for item in context["invocations"]}
        ),
        "run_manifest": artifact(root, run_path),
        "finalization": artifact(root, finalization_path),
        "population": len(labels),
        "cells": len(metric_panel),
        "terminal_failures": 0,
        "attempt_ledger": attempt_summary,
        "raw_results": finalization["result_evidence"],
        "output_audio": finalization["audio_evidence"],
        "explicit_exclusions": specification["explicit_exclusions"],
        "metrics": metric_panel,
    }


def build_report(root: Path, manifest_path: Path) -> dict[str, Any]:
    manifest = read_json(manifest_path, "full study manifest")
    require_equal(manifest.get("schema_version"), "1.0.0", "study manifest schema")
    require_equal(manifest.get("status"), "preregistered", "study manifest status")
    policy = manifest.get("publication_policy", {})
    require_equal(policy.get("partial_results"), "forbidden", "partial-results policy")
    require_equal(
        policy.get("cross_benchmark_composite"), "forbidden", "composite policy"
    )
    runtime_path, runtime = validate_study_runtime(root, manifest["runtime"])
    gateway_source_revision = runtime["source_revision"]
    gateway_sha256 = runtime["binary_sha256"]
    return {
        "schema_version": "1.0.0",
        "status": "complete",
        "generated_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "study": {
            "id": manifest["study_id"],
            "manifest": artifact(root, manifest_path),
            "runtime_manifest": artifact(root, runtime_path),
            "gateway_source_revision": gateway_source_revision,
            "gateway_executable_sha256": gateway_sha256,
            "publication_policy": policy,
        },
        "evidence_panel": {
            "tau_voice": validate_tau(
                root,
                manifest["tau_voice"],
                expected_gateway_source_revision=gateway_source_revision,
                expected_gateway_sha256=gateway_sha256,
            ),
            "full_duplex_bench_v1_5": validate_fdb15(
                root,
                manifest["full_duplex_bench_v1_5"],
                expected_gateway_sha256=gateway_sha256,
            ),
            "full_duplex_bench_v3": validate_fdbv3(
                root,
                manifest["full_duplex_bench_v3"],
                expected_gateway_sha256=gateway_sha256,
            ),
            "fd_bench": validate_fdbench(
                root,
                manifest["fd_bench"],
                expected_gateway_sha256=gateway_sha256,
            ),
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
