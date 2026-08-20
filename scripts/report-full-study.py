#!/usr/bin/env python3
"""Publish one fail-closed evidence panel for the complete voice study.

This is deliberately a completeness validator, not a new benchmark or a
cross-suite scoring function. It refuses to write an output until every frozen
population and every required upstream evaluation artifact is exact.
"""

from __future__ import annotations

import argparse
from collections import Counter
from collections.abc import Iterator
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
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

GPU_OWNERSHIP_CONTRACT = {
    "policy": "exclusive-process-ancestry",
    "scope": "entire scored provider invocation",
    "sampling_interval_seconds": 5,
    "capture_timeout_seconds": 15,
    "expected_components_local_fast": ["asr", "fish", "qwen"],
    "expected_components_remote_fast": ["asr", "fish"],
    "identity": [
        "host_boot_id",
        "component_root_pid",
        "component_root_proc_start_time_ticks",
        "gpu_process_pid",
        "gpu_process_proc_start_time_ticks",
        "gpu_uuid",
    ],
    "violation_policy": "invalidate the entire invocation",
    "evidence": "append-only ownership checks plus a content-addressed terminal summary",
}


def arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--manifest", type=Path, default=Path("benchmarks/full-study-v1.json")
    )
    parser.add_argument("--repository-root", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path)
    parser.add_argument(
        "--allow-postfreeze-reporting-correction",
        action="store_true",
        help=(
            "allow clean reporting-only code from a different revision to "
            "analyze an unchanged frozen orchestration root, with both revisions "
            "and the reporter hash disclosed in the output"
        ),
    )
    return parser.parse_args()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise StudyIncompleteError(message)


def require_equal(actual: Any, expected: Any, label: str) -> None:
    if actual != expected:
        raise StudyIncompleteError(f"{label}: expected {expected!r}, found {actual!r}")


def require_declared(
    container: Any, keys: tuple[str, ...], label: str
) -> dict[str, Any]:
    """Return `container` once it declares every named key.

    The arm validators index their preregistered declarations directly. An
    omitted key still fails closed, but as a KeyError traceback rather than as
    a statement of what the study never declared -- which reads as a broken
    tool instead of the refusal a reader has to act on.
    """
    require(isinstance(container, dict), f"{label} is not a record")
    for key in keys:
        require(key in container, f"{label} declares no {key}")
    return container


def require_fields(
    record: Any,
    label: str,
    *,
    text: tuple[str, ...] = (),
    count: tuple[str, ...] = (),
) -> dict[str, Any]:
    """Return `record` once it carries every named field.

    The callers below index these fields directly, to build the identity keys
    a population is checked against and to resolve the artifacts a hash is
    bound to. A runner that never wrote one of them would otherwise kill the
    gate with a KeyError, which reads as a broken tool rather than as the
    refusal the study's reader needs to see.
    """
    require(isinstance(record, dict), f"{label} is not a record")
    for name in text:
        value = record.get(name)
        require(
            isinstance(value, str) and value.strip() != "",
            f"{label} recorded no {name}",
        )
    for name in count:
        value = record.get(name)
        require(
            isinstance(value, int) and not isinstance(value, bool) and value >= 0,
            f"{label} recorded no {name} count",
        )
    return record


_ARTIFACT_SHAPE = {"path": None, "sha256": None}

# The preregistration manifest is hand-authored and under version control, so a
# missing field here is a malformed experiment definition rather than a runner
# fault. The validators below index these fields directly, which turned that
# mistake into a traceback; naming the absent field keeps every refusal
# readable. `None` marks a leaf whose value the relevant validator checks.
STUDY_MANIFEST_SHAPE: dict[str, Any] = {
    "study_id": None,
    "runtime": _ARTIFACT_SHAPE,
    "fd_bench": {
        "source_manifest": _ARTIFACT_SHAPE,
        "run_context": None,
        "run_manifest": None,
        "finalization": None,
        "metrics_directory": None,
        "cells": None,
        "dataset_revision": None,
        "explicit_exclusions": None,
        "upstream_revision": None,
    },
    "full_duplex_bench_v1_5": {
        "source_manifest": _ARTIFACT_SHAPE,
        "run_context": None,
        "run_manifest": None,
        "summary": None,
        "conditions": None,
        "replicates": None,
        "upstream_revision": None,
    },
    "full_duplex_bench_v3": {
        "source_manifest": _ARTIFACT_SHAPE,
        "run_context": None,
        "run_manifest": None,
        "profile": _ARTIFACT_SHAPE,
        "evaluations": {"exact": None, "gpt4o": None, "gpt4o_evidence": None},
        "upstream_revision": None,
    },
    "tau_voice": {
        "source_manifest": _ARTIFACT_SHAPE,
        "matrices": [_ARTIFACT_SHAPE],
    },
}


def require_shape(value: Any, shape: Any, label: str) -> None:
    """Require every field the validators below index without checking."""
    if shape is None:
        return
    if isinstance(shape, list):
        require(isinstance(value, list), f"{label} is not a list")
        for index, item in enumerate(value):
            require_shape(item, shape[0], f"{label}[{index}]")
        return
    require(isinstance(value, dict), f"{label} is not a record")
    for key, child in shape.items():
        require(key in value, f"{label} declares no {key}")
        require_shape(value[key], child, f"{label}.{key}")


def require_recorded_no_failures(run: dict[str, Any], label: str) -> None:
    """Require an explicitly recorded, empty terminal-failure ledger.

    A missing key is refused rather than read as zero. A runner that stopped
    before recording its failures writes a manifest indistinguishable from a
    clean run, so treating absence as success would publish exactly the
    evidence this gate exists to withhold.
    """
    failures = run.get("failures")
    require(
        isinstance(failures, list),
        f"{label} did not record a terminal failure ledger",
    )
    require_equal(len(failures), 0, f"{label} terminal failures")


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


def parse_utc_timestamp(value: Any, label: str) -> datetime:
    require(isinstance(value, str) and bool(value), f"{label} timestamp is absent")
    try:
        timestamp = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise StudyIncompleteError(f"{label} timestamp is invalid: {value}") from error
    require(timestamp.tzinfo is not None, f"{label} timestamp has no timezone")
    return timestamp.astimezone(timezone.utc)


def git_source_state(root: Path) -> tuple[str, bool]:
    try:
        revision = subprocess.run(
            ["git", "-C", str(root), "rev-parse", "HEAD"],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        ).stdout.strip()
        status = subprocess.run(
            [
                "git",
                "-C",
                str(root),
                "status",
                "--porcelain=v1",
                "--untracked-files=normal",
            ],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        ).stdout
    except (OSError, subprocess.CalledProcessError) as error:
        raise StudyIncompleteError(
            f"cannot verify terminal reporter source: {error}"
        ) from error
    require(
        len(revision) == 40
        and all(character in "0123456789abcdef" for character in revision),
        "terminal reporter source revision is invalid",
    )
    return revision, not bool(status)


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


PERMITTED_NULL_PANEL_FIELDS = frozenset(
    {
        # The official exact evaluation carries no LLM response score by design;
        # validate_fdbv3_evaluation requires that field to be empty, so the panel
        # reports the absence rather than inventing a number for it.
        "evidence_panel.full_duplex_bench_v3.evaluations.exact.by_metric.response_qual",
    }
)

TAU_UNDEFINED_INTERACTION_REASON = (
    "the native tau interaction scorer reported no defined value for this population"
)


def null_panel_fields(value: Any, path: str = "") -> Iterator[tuple[str, str]]:
    """Yield (reported path, index-free path) for every null inside value."""
    if value is None:
        yield path, re.sub(r"\[\d+\]", "[]", path)
    elif isinstance(value, dict):
        for key, child in value.items():
            yield from null_panel_fields(child, f"{path}.{key}" if path else str(key))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            yield from null_panel_fields(child, f"{path}[{index}]")


def require_no_unexplained_nulls(report: dict[str, Any]) -> None:
    """Refuse a panel that presents absent evidence as a published null.

    Panel values are copied out of upstream artifacts, so a field a runner never
    wrote arrives here as None and would be published as a result. Only fields
    this gate explicitly requires to be empty are allowed to be null.
    """
    for reported, normalized in null_panel_fields(report):
        require(
            normalized in PERMITTED_NULL_PANEL_FIELDS,
            f"evidence panel field has no value: {reported}",
        )


def publish_tau_overall(overall: dict[str, Any], label: str) -> dict[str, Any]:
    """Publish native undefined interaction metrics by name, never as zero/null."""
    interaction = overall.get("interaction_metrics")
    require(isinstance(interaction, dict), f"{label} recorded no interaction metrics")
    measured = {name: value for name, value in interaction.items() if value is not None}
    not_measured = {
        name: TAU_UNDEFINED_INTERACTION_REASON
        for name, value in interaction.items()
        if value is None
    }
    return {
        **overall,
        "interaction_metrics": measured,
        "interaction_metrics_not_measured": not_measured,
    }


def declared_population(specification: dict[str, Any], label: str) -> int:
    """Return a preregistered population, refusing a scope of nothing.

    Every downstream count is checked against this number, so a suite declared
    as zero trials would verify nothing and still publish a panel for it.
    """
    population = specification.get("population")
    require(
        isinstance(population, int)
        and not isinstance(population, bool)
        and population > 0,
        f"{label} declares no preregistered population",
    )
    return population


def declared_sha256(container: dict[str, Any], key: str, label: str) -> str:
    """Return a recorded SHA-256, refusing an absent or malformed one.

    artifact_tree compares a file against its recorded hash only when a hash was
    recorded, so an omitted hash silently unbinds that artifact from the
    evidence commitment while the tree root still looks authoritative.
    """
    value = container.get(key)
    require(valid_sha256(value), f"{label} did not record a valid {key}")
    return value


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
    require_declared(specification, ("path", "sha256"), label)
    require_fields(specification, label, text=("path", "sha256"))
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
    require(isinstance(value, list), f"{label} attempt ledger must be a JSON array")
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
    require_equal(
        runtime.get("gpu_ownership"),
        GPU_OWNERSHIP_CONTRACT,
        "study GPU ownership runtime",
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


def require_argv_option(
    argv: list[str], option: str, expected: str, label: str
) -> None:
    positions = [index for index, value in enumerate(argv) if value == option]
    require_equal(len(positions), 1, f"{label} {option} occurrence")
    position = positions[0]
    require(position + 1 < len(argv), f"{label} {option} has no value")
    require_equal(argv[position + 1], expected, f"{label} {option}")


def gpu_component_names(requires_local_fast: bool) -> list[str]:
    names = ["asr", "fish"]
    if requires_local_fast:
        names.append("qwen")
    return sorted(names)


def gpu_process_identity(processes: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return [
        {
            key: process.get(key)
            for key in (
                "gpu_uuid",
                "component",
                "component_pid",
                "pid",
                "proc_start_time_ticks",
            )
        }
        for process in processes
    ]


def validate_gpu_ownership_snapshot(
    value: Any,
    *,
    components: dict[str, Any],
    host_boot_id: str,
    requires_local_fast: bool,
    label: str,
) -> None:
    require(isinstance(value, dict), f"{label} GPU ownership snapshot is absent")
    require_equal(value.get("schema_version"), "1.0.0", f"{label} GPU schema")
    parse_utc_timestamp(value.get("captured_at"), f"{label} GPU capture")
    require_equal(value.get("exclusive"), True, f"{label} GPU exclusivity")
    require_equal(value.get("host_boot_id"), host_boot_id, f"{label} GPU host boot")
    expected = gpu_component_names(requires_local_fast)
    require_equal(
        value.get("expected_components"), expected, f"{label} GPU component set"
    )
    roots = value.get("component_roots")
    require(isinstance(roots, dict), f"{label} GPU component roots are absent")
    require_equal(sorted(roots), expected, f"{label} GPU component roots")
    for name in expected:
        root = roots[name]
        require(isinstance(root, dict), f"{label}/{name} GPU root is invalid")
        require_equal(
            root.get("pid"), components[name].get("pid"), f"{label}/{name} GPU root PID"
        )
        require_equal(
            root.get("proc_start_time_ticks"),
            components[name].get("proc_start_time_ticks"),
            f"{label}/{name} GPU root process start",
        )
    gpu_uuids = value.get("gpu_uuids")
    require(
        isinstance(gpu_uuids, list)
        and len(gpu_uuids) == 1
        and isinstance(gpu_uuids[0], str)
        and bool(gpu_uuids[0]),
        f"{label} must attest exactly one GPU",
    )
    processes = value.get("processes")
    require(
        isinstance(processes, list) and bool(processes),
        f"{label} GPU process evidence is absent",
    )
    observed: set[str] = set()
    process_ids: set[tuple[str, int]] = set()
    for index, process in enumerate(processes):
        process_label = f"{label} GPU process {index}"
        require(isinstance(process, dict), f"{process_label} is invalid")
        component = process.get("component")
        require(component in expected, f"{process_label} has an unknown component")
        observed.add(component)
        require_equal(
            process.get("component_pid"),
            roots[component].get("pid"),
            f"{process_label} component root",
        )
        pid = process.get("pid")
        require(
            isinstance(pid, int) and not isinstance(pid, bool) and pid > 0,
            f"{process_label} has an invalid PID",
        )
        require(
            isinstance(process.get("proc_start_time_ticks"), str)
            and process["proc_start_time_ticks"].isdigit(),
            f"{process_label} has an invalid process start identity",
        )
        require_equal(
            process.get("gpu_uuid"), gpu_uuids[0], f"{process_label} GPU identity"
        )
        process_key = (gpu_uuids[0], pid)
        require(process_key not in process_ids, f"{process_label} is duplicated")
        process_ids.add(process_key)
        memory = process.get("used_memory_mib")
        require(
            isinstance(memory, int) and not isinstance(memory, bool) and memory >= 0,
            f"{process_label} has invalid memory evidence",
        )
    require_equal(sorted(observed), expected, f"{label} occupied GPU components")


def validate_gpu_ownership_guard(
    root: Path,
    value: Any,
    *,
    components: dict[str, Any],
    host_boot_id: str,
    requires_local_fast: bool,
    label: str,
) -> None:
    require(isinstance(value, dict), f"{label} GPU ownership guard is absent")
    path_value = value.get("path")
    require(isinstance(path_value, str) and bool(path_value), f"{label} path is absent")
    summary_path = resolve(root, path_value)
    try:
        summary_path.relative_to(root.resolve())
    except ValueError as error:
        raise StudyIncompleteError(
            f"{label} summary is outside the repository"
        ) from error
    summary = read_json(summary_path, f"{label} summary")
    require_equal(sha256_file(summary_path), value.get("sha256"), f"{label} SHA-256")
    require_equal(summary_path.stat().st_size, value.get("bytes"), f"{label} bytes")
    require_equal(summary, value.get("evidence"), f"{label} embedded evidence")
    require_equal(summary.get("schema_version"), "1.0.0", f"{label} schema")
    require_equal(summary.get("status"), "complete", f"{label} status")
    require_equal(
        summary.get("expected_components"),
        gpu_component_names(requires_local_fast),
        f"{label} component set",
    )
    require_equal(summary.get("host_boot_id"), host_boot_id, f"{label} host boot")
    checks = summary.get("checks")
    require(
        isinstance(checks, int) and not isinstance(checks, bool) and checks > 0,
        f"{label} check count is invalid",
    )
    interval = summary.get("interval_seconds")
    require(
        isinstance(interval, int) and not isinstance(interval, bool) and interval > 0,
        f"{label} interval is invalid",
    )
    require_equal(
        interval,
        GPU_OWNERSHIP_CONTRACT["sampling_interval_seconds"],
        f"{label} frozen sampling interval",
    )
    capture_timeout = summary.get("capture_timeout_seconds")
    require(
        isinstance(capture_timeout, int)
        and not isinstance(capture_timeout, bool)
        and capture_timeout > 0,
        f"{label} capture timeout is invalid",
    )
    require_equal(
        capture_timeout,
        GPU_OWNERSHIP_CONTRACT["capture_timeout_seconds"],
        f"{label} frozen capture timeout",
    )
    log_artifact = summary.get("log")
    require(isinstance(log_artifact, dict), f"{label} log evidence is absent")
    log_path_value = log_artifact.get("path")
    require(
        isinstance(log_path_value, str) and bool(log_path_value),
        f"{label} log path is absent",
    )
    log_path = resolve(root, log_path_value)
    try:
        log_path.relative_to(root.resolve())
    except ValueError as error:
        raise StudyIncompleteError(f"{label} log is outside the repository") from error
    require(log_path.is_file(), f"{label} log is missing: {log_path}")
    require_equal(
        sha256_file(log_path), log_artifact.get("sha256"), f"{label} log SHA-256"
    )
    require_equal(
        log_path.stat().st_size, log_artifact.get("bytes"), f"{label} log bytes"
    )
    try:
        records = [
            json.loads(line)
            for line in log_path.read_text(encoding="utf-8").splitlines()
        ]
    except (OSError, json.JSONDecodeError) as error:
        raise StudyIncompleteError(f"cannot parse {label} log: {error}") from error
    require_equal(len(records), checks + 2, f"{label} log record count")
    record_times = [
        parse_utc_timestamp(record.get("recorded_at"), f"{label} record {index}")
        for index, record in enumerate(records)
        if isinstance(record, dict)
    ]
    require_equal(len(record_times), len(records), f"{label} object records")
    require(
        all(later >= earlier for earlier, later in zip(record_times, record_times[1:])),
        f"{label} record times are not monotonic",
    )
    require(
        isinstance(records[0], dict)
        and records[0].get("type") == "guard.started"
        and records[0].get("status") == "running",
        f"{label} start record is invalid",
    )
    require_equal(
        records[0].get("interval_seconds"), interval, f"{label} logged interval"
    )
    require_equal(
        records[0].get("capture_timeout_seconds"),
        capture_timeout,
        f"{label} logged capture timeout",
    )
    require(
        isinstance(records[-1], dict)
        and records[-1].get("type") == "guard.completed"
        and records[-1].get("status") == "complete"
        and records[-1].get("exit_status") == 0,
        f"{label} completion record is invalid",
    )
    require_equal(
        summary.get("started_at"), records[0].get("recorded_at"), f"{label} start time"
    )
    require_equal(
        summary.get("completed_at"),
        records[-1].get("recorded_at"),
        f"{label} completion time",
    )
    first_snapshot: dict[str, Any] | None = None
    for index, record in enumerate(records[1:-1]):
        check_label = f"{label} check {index}"
        require(
            isinstance(record, dict)
            and record.get("type") == "guard.check"
            and record.get("status") == "ok",
            f"{check_label} is not successful",
        )
        snapshot = record.get("ownership")
        validate_gpu_ownership_snapshot(
            snapshot,
            components=components,
            host_boot_id=host_boot_id,
            requires_local_fast=requires_local_fast,
            label=check_label,
        )
        captured_at = parse_utc_timestamp(
            snapshot.get("captured_at"), f"{check_label} ownership capture"
        )
        require(
            0
            <= (record_times[index + 1] - captured_at).total_seconds()
            <= capture_timeout,
            f"{check_label} capture time is inconsistent",
        )
        if first_snapshot is None:
            first_snapshot = snapshot
        else:
            for field in (
                "host_boot_id",
                "expected_components",
                "component_roots",
                "gpu_uuids",
            ):
                require_equal(
                    snapshot.get(field),
                    first_snapshot.get(field),
                    f"{check_label} stable {field}",
                )
            require_equal(
                gpu_process_identity(snapshot["processes"]),
                gpu_process_identity(first_snapshot["processes"]),
                f"{check_label} stable GPU process identity",
            )
    assert first_snapshot is not None
    maximum_gap = interval + capture_timeout + 1
    require(
        all(
            (later - earlier).total_seconds() <= maximum_gap
            for earlier, later in zip(record_times, record_times[1:])
        ),
        f"{label} contains an unsampled interval",
    )
    require_equal(
        summary.get("component_roots"),
        first_snapshot.get("component_roots"),
        f"{label} summarized component roots",
    )
    require_equal(
        summary.get("gpu_uuids"),
        first_snapshot.get("gpu_uuids"),
        f"{label} summarized GPU identity",
    )
    require_equal(
        summary.get("processes"),
        gpu_process_identity(first_snapshot["processes"]),
        f"{label} summarized process identity",
    )


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
    validate_gpu_ownership_snapshot(
        identity.get("gpu_ownership"),
        components=components,
        host_boot_id=identity["host_boot_id"],
        requires_local_fast=requires_local_fast,
        label=label,
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
    require_equal(len(invocations), 1, f"{label} source-stable invocation count")
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
            validate_gpu_ownership_guard(
                root,
                invocation.get("gpu_ownership_guard"),
                components=start["components"],
                host_boot_id=start["host_boot_id"],
                requires_local_fast=True,
                label=f"{invocation_label} GPU ownership",
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
    failed_infrastructure = attempts.get("failed_infrastructure")
    require(
        isinstance(failed_infrastructure, int)
        and not isinstance(failed_infrastructure, bool)
        and failed_infrastructure >= 0,
        f"{label} did not record its failed infrastructure attempts",
    )
    require_equal(
        attempts.get("total"),
        attempts["successful"] + failed_infrastructure,
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
            # Every declared task has one successful terminal attempt in the
            # archive ledger. A terminal infrastructure-error simulation would
            # either displace that successful result or add a second terminal
            # outcome for the same task. Tau's aggregate scorer may skip such
            # rows, but a publication gate must not call that exact population
            # complete.
            require_equal(
                errors,
                0,
                f"{matrix_id}/{cell_id}/{domain_name} terminal infrastructure errors",
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
        # The panel copies this block through wholesale, so a headline score the
        # runner never wrote arrives as an absent key rather than a null and is
        # published as a cell that simply has no reward beside it.
        overall = cell_report.get("overall")
        require(
            isinstance(overall, dict),
            f"{matrix_id}/{cell_id} recorded no overall result",
        )
        agent_metrics = overall.get("agent_metrics")
        require(
            isinstance(agent_metrics, dict),
            f"{matrix_id}/{cell_id} recorded no agent metrics",
        )
        average_reward = agent_metrics.get("avg_reward")
        require(
            isinstance(average_reward, (int, float))
            and not isinstance(average_reward, bool),
            f"{matrix_id}/{cell_id} recorded no average reward",
        )
        cell_panel[cell_id] = {
            "simulations": cell_simulations,
            "infrastructure_errors": cell_errors,
            "termination_reasons": dict(sorted(cell_termination_reasons.items())),
            "overall": publish_tau_overall(overall, f"{matrix_id}/{cell_id}"),
        }

    expected_total = (
        len(cell_ids)
        * matrix["benchmark"]["total_tasks_per_cell"]
        * matrix["benchmark"]["num_trials"]
    )
    require_equal(total_simulations, expected_total, f"{matrix_id} total population")
    execution = report.get("execution_evidence")
    require(isinstance(execution, list), f"{matrix_id} execution evidence is absent")
    require_equal(len(execution), 1, f"{matrix_id} source-stable invocation count")
    complete_runs = [item for item in execution if item.get("status") == "complete"]
    require_equal(len(complete_runs), 1, f"{matrix_id} complete execution count")
    require_equal(
        complete_runs[0].get("selected_cell"),
        None,
        f"{matrix_id} all-cell execution",
    )
    requires_local_fast = matrix.get("runtime_requirements", {}).get(
        "requires_local_fast", True
    )
    for index, complete_run in enumerate(complete_runs):
        execution_label = f"{matrix_id} complete execution {index}"
        run_path_value = complete_run.get("path")
        require(
            isinstance(run_path_value, str) and bool(run_path_value),
            f"{execution_label} run path is absent",
        )
        run_path = resolve(root, run_path_value)
        expected_run_root = (
            root / ".runtime/benchmark-runs/tau-voice" / matrix_id / "invocations"
        ).resolve()
        try:
            run_path.relative_to(expected_run_root)
        except ValueError as error:
            raise StudyIncompleteError(
                f"{execution_label} run artifact is outside its matrix invocation root"
            ) from error
        require_equal(run_path.name, "run.json", f"{execution_label} run filename")
        run_payload = read_json(run_path, f"{execution_label} run artifact")
        require_equal(
            sha256_file(run_path),
            complete_run.get("sha256"),
            f"{execution_label} run SHA-256",
        )
        require_equal(
            run_payload.get("matrix_sha256"),
            matrix_specification["sha256"],
            f"{execution_label} run matrix SHA-256",
        )
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
        start_identity = complete_run.get("runtime_identity")
        final_identity = complete_run.get("runtime_identity_final")
        validate_runtime_identity(
            start_identity,
            requires_local_fast=requires_local_fast,
            expected_gateway_sha256=expected_gateway_sha256,
            label=f"{execution_label} start",
        )
        validate_runtime_identity(
            final_identity,
            requires_local_fast=requires_local_fast,
            expected_gateway_sha256=expected_gateway_sha256,
            label=f"{execution_label} final",
        )
        require_equal(
            start_identity.get("host_boot_id"),
            final_identity.get("host_boot_id"),
            f"{execution_label} runtime boot",
        )
        require_equal(
            start_identity.get("components"),
            final_identity.get("components"),
            f"{execution_label} runtime processes",
        )
        # A recorded null means "this run covered the whole matrix", so an
        # absent key silently claims coverage the runner never asserted.
        # Require the field, then allow its null.
        require(
            "selected_cell" in complete_run,
            f"{execution_label} did not record which cell it ran",
        )
        selected_cell = complete_run["selected_cell"]
        require(
            selected_cell is None or selected_cell in cell_ids,
            f"{execution_label} selected cell is invalid",
        )
        covered_cells = [selected_cell] if selected_cell is not None else cell_ids
        run_directory = Path(run_path_value).parent
        expected_guard_paths = {
            (
                run_directory / cell_id / f"{domain}.gpu-ownership.summary.json"
            ).as_posix()
            for cell_id in covered_cells
            for domain in domains
        }
        guards = complete_run.get("gpu_ownership_guards")
        require(
            isinstance(guards, list),
            f"{execution_label} GPU ownership guards are absent",
        )
        require(
            all(
                isinstance(item, dict) and isinstance(item.get("path"), str)
                for item in guards
            ),
            f"{execution_label} GPU ownership guard paths are invalid",
        )
        require_equal(
            {item["path"] for item in guards},
            expected_guard_paths,
            f"{execution_label} GPU ownership guard coverage",
        )
        require_equal(
            len(guards),
            len(expected_guard_paths),
            f"{execution_label} unique GPU ownership guards",
        )
        for guard_index, guard in enumerate(guards):
            validate_gpu_ownership_guard(
                root,
                guard,
                components=start_identity["components"],
                host_boot_id=start_identity["host_boot_id"],
                requires_local_fast=requires_local_fast,
                label=f"{execution_label} GPU ownership guard {guard_index}",
            )
        projected_fields = {
            "status": "status",
            "selected_cell": "selected_cell",
            "started_at": "started_at",
            "completed_at": "completed_at",
            "openrealtime_revision": "openrealtime_revision",
            "openrealtime_revision_final": "openrealtime_revision_final",
            "source_worktree_clean_start": "source_worktree_clean_start",
            "source_worktree_clean_final": "source_worktree_clean_final",
            "runtime_identity": "runtime_identity",
            "runtime_identity_final": "runtime_identity_final",
            "gateway_health_start": "gateway_health",
            "gateway_health_final": "gateway_health_final",
            "gpu_ownership_guards": "gpu_ownership_guards",
        }
        for report_field, run_field in projected_fields.items():
            require_equal(
                complete_run.get(report_field),
                run_payload.get(run_field),
                f"{execution_label} projected {report_field}",
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
            "deletion_gate": "remove expanded duplicates only after archive readability, SHA-256, byte count, matrix identity, exact task population, and the bounded exception-only attempt ledger are recorded",
            "publication_gate": "the terminal reporter rehashes every archive and rejects missing evidence, invalid retry provenance, or remaining expanded duplicates",
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
    require(matrix_ids, "the study declares no tau-Voice matrix")
    require_equal(len(matrix_ids), len(set(matrix_ids)), "unique tau matrix IDs")

    # A vacuous loop verifies nothing: dropping the declaration drops every
    # paired-report check with it and still publishes a panel. Requiring a list
    # only closes half of that -- an EMPTY list is a list, and it drops exactly
    # the same checks while satisfying the type. The study declares paired
    # ablations; publishing a panel that validated none of them is the same
    # absence-read-as-success this gate exists to refuse.
    paired_specifications = specification.get("paired_reports")
    require(
        isinstance(paired_specifications, list) and bool(paired_specifications),
        "the study declares no tau-Voice paired reports",
    )
    paired_panel = []
    for index, pair in enumerate(paired_specifications):
        require_declared(
            pair, ("id", "definition", "report"), f"tau paired report {index}"
        )
        require_fields(pair, f"tau paired report {index}", text=("id", "report"))
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
        provider_work = report.get("comparison", {}).get("provider_work", {})
        endpoint_work = provider_work.get("endpoint_only", {})
        continuous_work = provider_work.get("continuous", {})
        for phase in ("fast_preparation", "slow_preparation"):
            require_equal(
                endpoint_work.get(phase, {}).get("invocations"),
                0,
                f"{pair['id']} endpoint-only {phase}",
            )
            # Proving the endpoint arm did nothing proves nothing on its own:
            # if the continuous arm also opened no private calls, the two arms
            # are the same condition and every published difference is noise.
            # The gate has to see the manipulation it is releasing evidence for.
            continuous_invocations = continuous_work.get(phase, {}).get("invocations")
            require(
                isinstance(continuous_invocations, int)
                and not isinstance(continuous_invocations, bool)
                and continuous_invocations > 0,
                f"{pair['id']} continuous {phase} records no private calls, so "
                f"the paired ablation manipulated nothing",
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
    declared_population(specification, "FDB1.5")
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
    require_recorded_no_failures(run, "FDB1.5")
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
    for item in completed:
        require_fields(
            item.get("sample"),
            f"FDB1.5 completed trial {item.get('trial_id')!r} sample",
            text=("scenario", "id"),
        )
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
        output_entries.append(
            (
                output_path,
                declared_sha256(
                    item, "output_sha256", f"FDB1.5 {item['trial_id']} output audio"
                ),
            )
        )
        result_path = output_path.parent / f"result_{item['condition']}.json"
        result_entries.append((result_path, None))
        # The runner writes this file and the manifest entry from a single
        # value, but records no hash for the file, so artifact_tree admits
        # whatever is on disk and republishes its digest as though it were
        # attested. Bind the raw result to the entry it duplicates, the way
        # FDBv3 already binds its own raw results.
        raw_result = read_json(result_path, f"FDB1.5 raw result {item['trial_id']}")
        for field in ("trial_id", "condition", "attempt"):
            require_equal(
                raw_result.get(field),
                item.get(field),
                f"FDB1.5 {item['trial_id']} raw result {field}",
            )
        for field in ("input_sha256", "output_sha256"):
            require_equal(
                declared_sha256(
                    raw_result, field, f"FDB1.5 {item['trial_id']} raw result"
                ),
                declared_sha256(
                    item, field, f"FDB1.5 {item['trial_id']} completed trial"
                ),
                f"FDB1.5 {item['trial_id']} raw result {field}",
            )
        raw_output_wav = raw_result.get("output_wav")
        require(
            isinstance(raw_output_wav, str) and raw_output_wav.strip() != "",
            f"FDB1.5 {item['trial_id']} raw result records no output path",
        )
        require_equal(
            resolve(root, raw_output_wav),
            output_path,
            f"FDB1.5 {item['trial_id']} raw result output path",
        )
        raw_sample = raw_result.get("sample")
        require(
            isinstance(raw_sample, dict),
            f"FDB1.5 {item['trial_id']} raw result records no sample",
        )
        for field in ("scenario", "id"):
            require_equal(
                raw_sample.get(field),
                item["sample"].get(field),
                f"FDB1.5 {item['trial_id']} raw result sample {field}",
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
    for index, condition in enumerate(conditions):
        require_fields(
            condition,
            f"FDB1.5 summary condition {index}",
            text=("scenario",),
            count=("completed",),
        )
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
    # The panel copies these blocks through wholesale, so a counter the runner
    # never wrote arrives as an absent key rather than a null and publishes a
    # turn-taking or latency figure with no population behind it.
    turn_taking = report.get("turn_taking")
    require(isinstance(turn_taking, dict), f"{label} recorded no turn-taking counts")
    for name in ("total", "turn_taken"):
        value = turn_taking.get(name)
        require(
            isinstance(value, int) and not isinstance(value, bool) and value >= 0,
            f"{label} turn-taking {name} is not a count",
        )
    latency = report.get("latency")
    require(isinstance(latency, dict), f"{label} recorded no latency population")
    samples = latency.get("total_samples")
    require(
        isinstance(samples, int) and not isinstance(samples, bool) and samples >= 0,
        f"{label} latency sample population is not a count",
    )
    for scenario in scenarios:
        # Chained .get() defaults cannot tell "the runner recorded no score",
        # which is what the exact evaluation is required to do, from "the runner
        # never wrote the metrics block at all", which is missing evidence.
        # Requiring the structure makes the empty score an explicit null.
        scenario_label = f"{label}/{scenario.get('scenario_id')}"
        metrics = scenario.get("metrics")
        require(isinstance(metrics, dict), f"{scenario_label} recorded no metrics")
        response_qual = metrics.get("response_qual")
        require(
            isinstance(response_qual, dict),
            f"{scenario_label} recorded no response_qual metric",
        )
        require(
            "score" in response_qual,
            f"{scenario_label} response_qual recorded no score",
        )
        score = response_qual["score"]
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
    by_metric = report.get("by_metric")
    require(isinstance(by_metric, dict), f"{label} recorded no aggregate metrics")
    require(
        "response_qual" in by_metric,
        f"{label} aggregate metrics recorded no response_qual",
    )
    aggregate_score = by_metric["response_qual"]
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
    declared_population(specification, "FDBv3")
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
    for index, item in enumerate(samples):
        require_fields(
            item,
            f"FDBv3 sample {index}",
            text=(
                "example_id",
                "pid",
                "directory",
                "input_sha256",
                "metadata_sha256",
            ),
        )
    labels = {f"{item['example_id']}_{item['pid']}" for item in samples}
    sample_ids = {item["example_id"] for item in samples}
    require_equal(len(labels), len(samples), "FDBv3 unique sample labels")
    require_equal(len(sample_ids), len(samples), "FDBv3 unique example IDs")
    require_equal(set(run.get("completed", [])), labels, "FDBv3 completed samples")
    require_equal(len(run.get("completed", [])), len(labels), "FDBv3 completed count")
    require_recorded_no_failures(run, "FDBv3")
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
        output_entries.append(
            (
                output_path,
                declared_sha256(
                    evidence,
                    "output_sha256",
                    f"FDBv3 {sample['example_id']} output audio",
                ),
            )
        )
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
    declared_population(specification, "FD-Bench")
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
    for index, item in enumerate(samples):
        require_fields(item, f"FD-Bench sample {index}", text=("cell", "id"))
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
    require_recorded_no_failures(run, "FD-Bench")
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
    expected_count_names = {
        "samples",
        "rounds",
        "interruptions",
        "gaps",
        "success_responses",
        "success_responses_to_interruption",
        "success_interruptions",
        "early_interruptions",
        "noise_interruptions",
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
        require_fields(trace, f"FD-Bench {cell} trace", text=("path",))
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
        metric_trace = metric.get("trace")
        require(
            isinstance(metric_trace, dict),
            f"FD-Bench {cell} metric recorded no scored trace",
        )
        require_equal(
            metric_trace.get("samples"),
            expected_population,
            f"FD-Bench {cell} metric population",
        )
        require_equal(
            metric_trace.get("sha256"),
            trace["sha256"],
            f"FD-Bench {cell} metric trace hash",
        )
        # Not `.get("path", trace["path"])`: defaulting to the finalization's
        # own path compares that path against itself, so a metric file that
        # never said which trace it scored would satisfy the check vacuously.
        require_fields(metric_trace, f"FD-Bench {cell} metric trace", text=("path",))
        require_equal(
            resolve(root, metric_trace["path"]),
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
        # The trace population above counts lines the runner wrote. It says
        # nothing about how many rounds the decision core actually scored, and
        # a cell that scored none still emits a full set of metric keys.
        counts = metric.get("counts")
        require(
            isinstance(counts, dict),
            f"FD-Bench {cell} metric must record the scored round population",
        )
        # Every published rate divides by one of these counters. The panel
        # copies this dict through wholesale, so a counter the runner never
        # wrote arrives as an absent key rather than a null and slips past the
        # no-unexplained-nulls rule, publishing a rate with no population
        # behind it.
        require_equal(
            set(counts),
            expected_count_names,
            f"FD-Bench {cell} scored populations",
        )
        require(
            all(
                isinstance(counts[name], int)
                and not isinstance(counts[name], bool)
                and counts[name] >= 0
                for name in expected_count_names
            ),
            f"FD-Bench {cell} scored populations must be non-negative integers",
        )
        require(
            counts["rounds"] > 0,
            f"FD-Bench {cell} scored no rounds, so its metrics measure nothing",
        )
        # The trace states how many samples were submitted; `samples` states how
        # many the evaluator actually aggregated over. If those differ, some
        # samples dropped out of every published rate while the trace still
        # reported the full population, and the rates describe a smaller run
        # than the one the study claims to have made.
        require_equal(
            counts["samples"],
            expected_population,
            f"FD-Bench {cell} aggregated sample population",
        )
        # A metric with no value must say why it has none, and an explanation
        # with no matching null is a stale claim. Requiring the two sets to be
        # equal refuses both directions.
        not_measured = metric.get("not_measured")
        require(
            isinstance(not_measured, dict),
            f"FD-Bench {cell} metric must record which values were not measured",
        )
        require_equal(
            set(not_measured),
            {name for name, value in metric["metrics"].items() if value is None},
            f"FD-Bench {cell} undefined metrics and their explanations",
        )
        # Undefined metrics are published by name in not_measured rather than
        # as nulls under metrics, which is how this panel already reports
        # not_evaluated. A reader scanning metrics sees only measurements.
        metric_panel[cell] = {
            "population": expected_population,
            "scored_rounds": counts["rounds"],
            "artifact": artifact(root, metric_path),
            "trace": artifact(root, trace_path),
            "metrics": {
                name: value
                for name, value in metric["metrics"].items()
                if value is not None
            },
            "not_measured": not_measured,
            "counts": counts,
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


def require_preregistered_declarations(manifest: dict[str, Any]) -> None:
    """Name an omitted preregistration key rather than dying on a KeyError."""
    require_fields(manifest, "the study preregistration", text=("study_id",))
    require_declared(
        manifest,
        (
            "runtime",
            "tau_voice",
            "full_duplex_bench_v1_5",
            "full_duplex_bench_v3",
            "fd_bench",
        ),
        "the study preregistration",
    )
    require_declared(
        manifest["tau_voice"],
        ("source_manifest", "matrices"),
        "the tau-Voice arm",
    )
    matrices = manifest["tau_voice"]["matrices"]
    require(isinstance(matrices, list), "the tau-Voice arm declares no matrices")
    for index, matrix in enumerate(matrices):
        require_declared(
            matrix, ("path", "sha256"), f"tau-Voice matrix {index}"
        )
    require_declared(
        manifest["full_duplex_bench_v1_5"],
        (
            "source_manifest",
            "upstream_revision",
            "conditions",
            "replicates",
            "run_context",
            "run_manifest",
            "summary",
        ),
        "the FDB1.5 arm",
    )
    require_declared(
        manifest["full_duplex_bench_v3"],
        (
            "source_manifest",
            "profile",
            "upstream_revision",
            "run_context",
            "run_manifest",
            "evaluations",
        ),
        "the FDBv3 arm",
    )
    require_declared(
        manifest["full_duplex_bench_v3"]["profile"],
        ("path", "sha256"),
        "the FDBv3 profile",
    )
    require_declared(
        manifest["full_duplex_bench_v3"]["evaluations"],
        ("exact", "gpt4o", "gpt4o_evidence"),
        "the FDBv3 evaluations",
    )
    require_declared(
        manifest["fd_bench"],
        (
            "source_manifest",
            "upstream_revision",
            "dataset_revision",
            "cells",
            "run_context",
            "run_manifest",
            "finalization",
            "metrics_directory",
            "explicit_exclusions",
        ),
        "the FD-Bench arm",
    )


def build_report(
    root: Path,
    manifest_path: Path,
    *,
    reporter_source_root: Path | None = None,
    allow_postfreeze_reporting_correction: bool = False,
) -> dict[str, Any]:
    manifest = read_json(manifest_path, "full study manifest")
    require_equal(manifest.get("schema_version"), "1.0.0", "study manifest schema")
    require_equal(manifest.get("status"), "preregistered", "study manifest status")
    policy = manifest.get("publication_policy", {})
    require_equal(policy.get("partial_results"), "forbidden", "partial-results policy")
    require_equal(
        policy.get("cross_benchmark_composite"), "forbidden", "composite policy"
    )
    # The panel republishes this policy block, so a declaration the manifest
    # never made would be presented as one the study committed to. The wording
    # is prose and may change; that it was declared at all may not.
    presentation = policy.get("presentation")
    require(
        isinstance(presentation, str) and presentation.strip() != "",
        "the study declares no presentation policy",
    )
    require_shape(manifest, STUDY_MANIFEST_SHAPE, "the study manifest")
    require_preregistered_declarations(manifest)
    runtime_path, runtime = validate_study_runtime(root, manifest["runtime"])
    gateway_source_revision = runtime["source_revision"]
    gateway_sha256 = runtime["binary_sha256"]
    tau_panel = validate_tau(
        root,
        manifest["tau_voice"],
        expected_gateway_source_revision=gateway_source_revision,
        expected_gateway_sha256=gateway_sha256,
    )
    fdb15_panel = validate_fdb15(
        root,
        manifest["full_duplex_bench_v1_5"],
        expected_gateway_sha256=gateway_sha256,
    )
    fdbv3_panel = validate_fdbv3(
        root,
        manifest["full_duplex_bench_v3"],
        expected_gateway_sha256=gateway_sha256,
    )
    fdbench_panel = validate_fdbench(
        root,
        manifest["fd_bench"],
        expected_gateway_sha256=gateway_sha256,
    )
    orchestration_revisions = {
        revision
        for matrix in tau_panel["matrices"]
        for revision in matrix["openrealtime_revisions"]
    }
    for panel in (fdb15_panel, fdbv3_panel, fdbench_panel):
        orchestration_revisions.update(panel["openrealtime_revisions"])
    require_equal(
        len(orchestration_revisions),
        1,
        "source-stable orchestration revision count",
    )
    orchestration_revision = next(iter(orchestration_revisions))
    reporter_revision, reporter_clean = git_source_state(root)
    require_equal(
        reporter_revision,
        orchestration_revision,
        "terminal reporter orchestration revision",
    )
    require_equal(reporter_clean, True, "terminal reporter clean source")
    reporter_code_root = (
        root.resolve()
        if reporter_source_root is None
        else reporter_source_root.resolve()
    )
    if reporter_code_root == root.resolve():
        reporter_code_revision, reporter_code_clean = reporter_revision, reporter_clean
    else:
        reporter_code_revision, reporter_code_clean = git_source_state(
            reporter_code_root
        )
    correction = reporter_code_revision != orchestration_revision
    if correction:
        require(
            allow_postfreeze_reporting_correction,
            "terminal reporter code revision differs from the frozen orchestration; "
            "the explicit reporting-correction flag is required",
        )
        require_equal(
            reporter_code_clean, True, "post-freeze correction reporter clean source"
        )
    else:
        require(
            not allow_postfreeze_reporting_correction,
            "post-freeze reporting correction was requested but reporter and "
            "orchestration revisions are identical",
        )
    reporter_script = Path(__file__).resolve()
    report = {
        "schema_version": "1.0.0",
        "status": "complete",
        "generated_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "study": {
            "id": manifest["study_id"],
            "manifest": artifact(root, manifest_path),
            "runtime_manifest": artifact(root, runtime_path),
            "gateway_source_revision": gateway_source_revision,
            "gateway_executable_sha256": gateway_sha256,
            "orchestration_revision": orchestration_revision,
            "source_worktree_clean": reporter_clean,
            "terminal_reporter": {
                "source_revision": reporter_code_revision,
                "source_worktree_clean": reporter_code_clean,
                "script": artifact(root, reporter_script),
                "mode": (
                    "postfreeze-reporting-correction"
                    if correction
                    else "frozen-orchestration"
                ),
                "correction_scope": (
                    {
                        "benchmark_execution_changed": False,
                        "native_scores_changed": False,
                        "changes": [
                            "represent native undefined tau interaction metrics by name instead of null",
                            "reject terminal tau infrastructure-error simulations",
                        ],
                    }
                    if correction
                    else "none"
                ),
            },
            "publication_policy": policy,
        },
        "evidence_panel": {
            "tau_voice": tau_panel,
            "full_duplex_bench_v1_5": fdb15_panel,
            "full_duplex_bench_v3": fdbv3_panel,
            "fd_bench": fdbench_panel,
        },
        "interpretation": {
            "aggregation": "none across benchmark families",
            "partial_results": "none admitted",
            "metric_authority": "each panel retains its pinned benchmark and upstream scorer semantics",
        },
    }
    require_no_unexplained_nulls(report)
    return report


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
    report = build_report(
        root,
        manifest_path,
        reporter_source_root=Path(__file__).resolve().parent.parent,
        allow_postfreeze_reporting_correction=args.allow_postfreeze_reporting_correction,
    )
    atomic_write_json(output.resolve(), report)
    print(f"full voice study report complete: {output.resolve()}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except StudyIncompleteError as error:
        print(f"incomplete full study; no report: {error}", file=os.sys.stderr)
        raise SystemExit(1)
