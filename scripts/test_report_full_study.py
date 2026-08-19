#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("report-full-study.py")
SPEC = importlib.util.spec_from_file_location("report_full_study", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
REPORT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(REPORT)


def write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def descriptor(profile: str) -> dict:
    return {
        "provider": "openrealtime",
        "model": "openrealtime-local",
        "transport": "websocket-openai-realtime",
        "architecture": "canonical-local-asr-fast-slow-tts",
        "profile": profile,
        "input_sample_rate_hz": 24000,
        "output_sample_rate_hz": 24000,
    }


def runtime_identity() -> dict:
    component = {
        "proc_start_time_ticks": "100",
        "executable": "/runtime/component",
        "executable_sha256": "a" * 64,
        "command_sha256": "b" * 64,
        "argv": ["/runtime/component"],
    }
    components = {
        name: {**component, "pid": pid}
        for name, pid in (("gateway", 10), ("asr", 20), ("fish", 30))
    }
    components["qwen"] = {
        **component,
        "pid": 40,
        "argv": [
            "/runtime/python",
            "-m",
            "vllm.entrypoints.openai.api_server",
            "--model",
            "Qwen/Qwen3-30B-A3B-FP8",
            "--revision",
            "d206ba732169f29bb77fbf80fc2c4b81d4d30782",
            "--served-model-name",
            "qwen-fast",
            "--gpu-memory-utilization",
            "0.38",
            "--max-model-len",
            "40960",
            "--enable-auto-tool-choice",
            "--tool-call-parser",
            "hermes",
        ],
        "service": {
            "implementation": "vllm",
            "version": "0.19.0",
            "served_model": "qwen-fast",
            "model": "Qwen/Qwen3-30B-A3B-FP8",
            "max_model_len": 40960,
        },
    }
    gpu_processes = [
        {
            "gpu_uuid": "GPU-test",
            "component": name,
            "component_pid": components[name]["pid"],
            "pid": components[name]["pid"] + 1,
            "proc_start_time_ticks": str(200 + index),
            "used_memory_mib": 1000 + index,
        }
        for index, name in enumerate(("asr", "fish", "qwen"))
    ]
    return {
        "schema_version": "1.0.0",
        "host_boot_id": "boot",
        "components": components,
        "gpu_ownership": {
            "schema_version": "1.0.0",
            "captured_at": "2026-01-01T00:00:00Z",
            "host_boot_id": "boot",
            "exclusive": True,
            "expected_components": ["asr", "fish", "qwen"],
            "component_roots": {
                name: {
                    "pid": components[name]["pid"],
                    "proc_start_time_ticks": components[name]["proc_start_time_ticks"],
                }
                for name in ("asr", "fish", "qwen")
            },
            "gpu_uuids": ["GPU-test"],
            "processes": gpu_processes,
        },
    }


def run_context(benchmark: str, guard: dict) -> dict:
    identity = runtime_identity()
    return {
        "schema_version": "1.0.0",
        "benchmark": benchmark,
        "status": "complete",
        "invocations": [
            {
                "status": "complete",
                "source_worktree_clean_start": True,
                "source_worktree_clean_at_completion": True,
                "study_gateway_sha256": "a" * 64,
                "openrealtime_revision_start": "c" * 40,
                "openrealtime_revision_at_completion": "c" * 40,
                "gateway_health_start": {"status": "ok"},
                "gateway_health_final": {"status": "ok"},
                "runtime_identity_start": identity,
                "runtime_identity_final": identity,
                "gpu_ownership_guard": guard,
            }
        ],
    }


class Fixture:
    def __init__(self, root: Path) -> None:
        self.root = root
        self.paths: dict[str, Path] = {}
        self._build()

    def relative(self, path: Path) -> str:
        return str(path.relative_to(self.root))

    def pin(self, path: Path) -> dict:
        return {"path": self.relative(path), "sha256": REPORT.sha256_file(path)}

    def gpu_guard(self, summary_path: Path) -> dict:
        identity = runtime_identity()
        snapshot = identity["gpu_ownership"]
        if not summary_path.name.endswith(".summary.json"):
            raise RuntimeError("GPU guard fixture summary must end in .summary.json")
        log_path = summary_path.with_name(
            summary_path.name.removesuffix(".summary.json") + ".jsonl"
        )
        records = [
            {
                "type": "guard.started",
                "status": "running",
                "recorded_at": "2026-01-01T00:00:00Z",
                "interval_seconds": 5,
                "capture_timeout_seconds": 15,
            },
            {
                "type": "guard.check",
                "status": "ok",
                "recorded_at": "2026-01-01T00:00:01Z",
                "ownership": snapshot,
            },
            {
                "type": "guard.completed",
                "status": "complete",
                "recorded_at": "2026-01-01T00:00:02Z",
                "exit_status": 0,
            },
        ]
        log_path.parent.mkdir(parents=True, exist_ok=True)
        log_path.write_text(
            "".join(
                json.dumps(record, separators=(",", ":")) + "\n" for record in records
            ),
            encoding="utf-8",
        )
        process_fields = (
            "gpu_uuid",
            "component",
            "component_pid",
            "pid",
            "proc_start_time_ticks",
        )
        summary = {
            "schema_version": "1.0.0",
            "status": "complete",
            "interval_seconds": 5,
            "capture_timeout_seconds": 15,
            "started_at": records[0]["recorded_at"],
            "completed_at": records[-1]["recorded_at"],
            "checks": 1,
            "host_boot_id": snapshot["host_boot_id"],
            "expected_components": snapshot["expected_components"],
            "component_roots": snapshot["component_roots"],
            "gpu_uuids": snapshot["gpu_uuids"],
            "processes": [
                {key: process[key] for key in process_fields}
                for process in snapshot["processes"]
            ],
            "log": {
                "path": self.relative(log_path),
                "sha256": REPORT.sha256_file(log_path),
                "bytes": log_path.stat().st_size,
            },
        }
        write_json(summary_path, summary)
        return {
            "path": self.relative(summary_path),
            "sha256": REPORT.sha256_file(summary_path),
            "bytes": summary_path.stat().st_size,
            "evidence": summary,
        }

    def _build(self) -> None:
        tau_source = self.root / "benchmarks/external/tau.json"
        write_json(tau_source, {"benchmark": {"name": "tau-Voice"}})
        runtime_manifest = self.root / "benchmarks/runtime/canonical-gateway-v1.json"
        write_json(
            runtime_manifest,
            {
                "schema_version": "1.0.0",
                "runtime_id": "openrealtime-canonical-gateway-v1",
                "source_revision": "c" * 40,
                "binary_sha256": "a" * 64,
                "runtime_path": ".runtime/study-runtime/canonical-gateway-v1/realtimegateway",
                "local_fast": dict(REPORT.LOCAL_FAST_CONTRACT),
                "gpu_ownership": dict(REPORT.GPU_OWNERSHIP_CONTRACT),
                "build": {
                    "go": "/usr/local/go/bin/go",
                    "command": "go build -trimpath -buildvcs=false ./cmd/realtimegateway",
                },
            },
        )
        matrix = self.root / "benchmarks/tau/matrix.json"
        matrix_payload = {
            "matrix_id": "tau-mini",
            "benchmark": {
                "repository": "example",
                "revision": "tau-revision",
                "seed": 300,
                "domains": [{"name": "airline", "tasks": 1}],
                "total_tasks_per_cell": 1,
                "num_trials": 1,
                "infrastructure_retries": 3,
                "infrastructure_retry_delay_seconds": 1,
            },
            "cells": [{"id": "control", "speech_complexity": "control"}],
            "transport": {
                "ping_interval_seconds": 20,
                "ping_timeout_seconds": 0,
            },
            "reporting": {
                "infrastructure_retry_policy": {
                    "maximum_retries": 3,
                    "maximum_attempts": 4,
                    "retry_delay_seconds": 1,
                    "seed_reused": True,
                    "scope": "exceptions_only",
                    "semantic_outcomes_retried": False,
                    "attempt_artifacts": "preserved",
                }
            },
            "runtime_requirements": {
                "gateway": {
                    "source_revision": "c" * 40,
                    "executable_sha256": "a" * 64,
                }
            },
        }
        write_json(matrix, matrix_payload)
        tau_experiment = (
            self.root
            / ".runtime/tau2-bench/data/simulations"
            / "tau-mini-control-airline-seed300"
        )
        tau_archive = tau_experiment / "raw-artifacts.tar.zst"
        tau_archive.parent.mkdir(parents=True, exist_ok=True)
        tau_archive.write_bytes(b"deterministic archive")
        write_json(
            tau_experiment / "raw-artifacts-archive.json",
            {
                "schema_version": "1.0.0",
                "matrix": {
                    "path": self.relative(matrix),
                    "id": "tau-mini",
                    "sha256": REPORT.sha256_file(matrix),
                },
                "population": {"cell": "control", "domain": "airline"},
                "source": {"path": "artifacts", "files": 2, "bytes": 22},
                "attempts": {
                    "tasks": 1,
                    "total": 1,
                    "successful": 1,
                    "failed_infrastructure": 0,
                    "retried_tasks": 0,
                    "maximum_observed": 1,
                    "maximum_allowed": 4,
                    "retry_delay_seconds": 1,
                    "seed_reused": True,
                    "retry_scope": "exceptions_only",
                    "semantic_outcomes_retried": False,
                },
                "archive": {
                    "path": "raw-artifacts.tar.zst",
                    "format": "deterministic-pax-tar+zstd",
                    "sha256": REPORT.sha256_file(tau_archive),
                    "bytes": tau_archive.stat().st_size,
                },
            },
        )
        tau_report = (
            self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        )
        tau_run = (
            self.root
            / ".runtime/benchmark-runs/tau-voice/tau-mini/invocations/all-cells/attempt/run.json"
        )
        tau_guard = self.gpu_guard(
            tau_run.parent / "control/airline.gpu-ownership.summary.json"
        )
        tau_execution = {
            "path": self.relative(tau_run),
            "status": "complete",
            "selected_cell": None,
            "openrealtime_revision": "c" * 40,
            "openrealtime_revision_final": "c" * 40,
            "source_worktree_clean_start": True,
            "source_worktree_clean_final": True,
            "runtime_identity": runtime_identity(),
            "runtime_identity_final": runtime_identity(),
            "gpu_ownership_guards": [tau_guard],
        }
        write_json(
            tau_run,
            {
                "matrix_sha256": REPORT.sha256_file(matrix),
                **{
                    key: value
                    for key, value in tau_execution.items()
                    if key not in {"path", "sha256"}
                },
            },
        )
        tau_execution["sha256"] = REPORT.sha256_file(tau_run)
        write_json(
            tau_report,
            {
                "schema_version": "1.0.0",
                "status": "complete",
                "matrix": {
                    "id": "tau-mini",
                    "path": self.relative(matrix),
                    "sha256": REPORT.sha256_file(matrix),
                    "selected_cells": ["control"],
                },
                "benchmark": {"revision": "tau-revision"},
                "execution_policy": {
                    "transport": {
                        "ping_interval_seconds": 20,
                        "ping_timeout_seconds": 0,
                    },
                    "infrastructure_retries": matrix_payload["reporting"][
                        "infrastructure_retry_policy"
                    ],
                },
                "execution_evidence": [tau_execution],
                "cells": {
                    "control": {
                        "domains": {
                            "airline": {
                                "population": {
                                    "status": "complete",
                                    "tasks": 1,
                                    "trials_per_task": 1,
                                    "simulations": 1,
                                    "termination_reasons": {"user_stop": 1},
                                    "infrastructure_errors": 0,
                                }
                            }
                        },
                        "overall": {"agent_metrics": {"avg_reward": 1.0}},
                    }
                },
            },
        )

        fdb15_source = self.root / "benchmarks/external/fdb15.json"
        write_json(
            fdb15_source,
            {
                "benchmark": "Full-Duplex-Bench v1.5",
                "upstream_revision": "fdb-revision",
                "archives": [
                    {"scenario": "background", "observed_complete_samples": 1}
                ],
            },
        )
        fdb15_run = self.root / ".runtime/fdb15/run.json"
        fdb15_context = self.root / ".runtime/fdb15/context.json"
        fdb15_guard = self.gpu_guard(
            self.root / ".runtime/fdb15/gpu-ownership-invocation-0.summary.json"
        )
        write_json(
            fdb15_context,
            run_context("full-duplex-bench-v1.5", fdb15_guard),
        )
        fdb15_descriptor = descriptor("fdb-v1.5-openai-realtime-adapter-i1-qg-v1")
        fdb15_output = self.root / ".runtime/fdb15/trial/output.wav"
        fdb15_output.parent.mkdir(parents=True, exist_ok=True)
        fdb15_output.write_bytes(b"fdb15-audio")
        fdb15_trial = {
            "trial_id": "openrealtime/background/1/overlap/r000",
            "sample": {"scenario": "background", "id": "1"},
            "condition": "overlap",
            "attempt": 1,
            "input_sha256": "d" * 64,
            "output_wav": self.relative(fdb15_output),
            "output_sha256": REPORT.sha256_file(fdb15_output),
        }
        write_json(fdb15_output.parent / "result_overlap.json", dict(fdb15_trial))
        write_json(
            fdb15_run,
            {
                "schema_version": "1.2.0",
                "benchmark": "full-duplex-bench-v1.5",
                "revision": "fdb-revision",
                "descriptor": fdb15_descriptor,
                "conditions": ["overlap"],
                "replicates": 1,
                "trial_attempts": 3,
                "samples": [{"scenario": "background", "id": "1"}],
                "completed": [dict(fdb15_trial)],
                "failures": [],
                "attempts": [
                    {
                        "sample_id": "1",
                        "scenario": "background",
                        "condition": "overlap",
                        "replicate": 0,
                        "attempt": 1,
                        "succeeded": True,
                    }
                ],
            },
        )
        fdb15_summary = self.root / ".runtime/fdb15/summary.json"
        write_json(
            fdb15_summary,
            {
                "schema_version": "1.2.0",
                "benchmark": "full-duplex-bench-v1.5",
                "revision": "fdb-revision",
                "conditions": [
                    {
                        "descriptor": fdb15_descriptor,
                        "scenario": "background",
                        "condition": "overlap",
                        "completed": 1,
                        "failures": 0,
                    }
                ],
            },
        )

        fdbv3_source = self.root / "benchmarks/external/fdbv3.json"
        write_json(
            fdbv3_source,
            {
                "benchmark": "Full-Duplex-Bench v3",
                "upstream_revision": "fdb-revision",
                "released_artifact": {"audio_examples": 1},
                "official_harness": {"evaluator_sha256": "5320"},
            },
        )
        fdbv3_profile = self.root / "benchmarks/fdbv3/profile.json"
        write_json(
            fdbv3_profile,
            {"profile": "fdbv3-profile", "source": {"revision": "fdb-revision"}},
        )
        fdbv3_run = self.root / ".runtime/fdbv3/run.json"
        fdbv3_context = self.root / ".runtime/fdbv3/context.json"
        fdbv3_guard = self.gpu_guard(
            self.root / ".runtime/fdbv3/gpu-ownership-invocation-0.summary.json"
        )
        write_json(
            fdbv3_context,
            run_context("full-duplex-bench-v3", fdbv3_guard),
        )
        write_json(
            fdbv3_run,
            {
                "schema_version": "1.0.0",
                "benchmark": "Full-Duplex-Bench v3",
                "revision": "fdb-revision",
                "profile": "fdbv3-profile",
                "profile_sha256": REPORT.sha256_file(fdbv3_profile),
                "descriptor": descriptor("fdbv3-profile"),
                "trial_attempts": 3,
                "samples": [
                    {
                        "example_id": "example",
                        "pid": "pid",
                        "directory": self.relative(self.root / ".runtime/fdbv3/sample"),
                        "input_sha256": "d" * 64,
                        "metadata_sha256": "e" * 64,
                    }
                ],
                "completed": ["example_pid"],
                "failures": [],
                "attempts": [{"sample": "example_pid", "number": 1, "succeeded": True}],
            },
        )
        fdbv3_output = self.root / ".runtime/fdbv3/sample/output_openrealtime.wav"
        fdbv3_output.parent.mkdir(parents=True, exist_ok=True)
        fdbv3_output.write_bytes(b"fdbv3-audio")
        write_json(
            fdbv3_output.parent / "result_openrealtime.json",
            {
                "openrealtime_schema_version": "1.0.0",
                "status": "completed",
                "pid": "pid",
                "example_id": "example",
                "provider": "openrealtime",
                "openrealtime": {
                    "benchmark_revision": "fdb-revision",
                    "profile_sha256": REPORT.sha256_file(fdbv3_profile),
                    "input_sha256": "d" * 64,
                    "metadata_sha256": "e" * 64,
                    "output_sha256": REPORT.sha256_file(fdbv3_output),
                },
            },
        )
        fdbv3_exact = self.root / ".runtime/fdbv3/exact.json"
        fdbv3_judge = self.root / ".runtime/fdbv3/judge.json"
        write_json(fdbv3_exact, self.evaluation(None))
        write_json(fdbv3_judge, self.evaluation(1.0))
        fdbv3_judge_evidence = self.root / ".runtime/fdbv3/judge-evidence.json"
        write_json(
            fdbv3_judge_evidence,
            {
                "schema_version": "1.1.0",
                "status": "complete",
                "scenarios": 1,
                "api_origin": "https://api.openai.com/v1",
                "expected_calls": {"argument": 1, "response": 1, "total": 2},
                "successful_valid_calls": 2,
                "evaluator": {"sha256": "5320"},
                "evaluation": {"sha256": REPORT.sha256_file(fdbv3_judge)},
                "calls": [
                    {
                        "sequence": 0,
                        "requested_model": "gpt-4o",
                        "response_id": "response-0",
                        "response_model": "gpt-4o-2024-08-06",
                        "request_sha256": "1" * 64,
                        "response_sha256": "2" * 64,
                        "parsed_response_sha256": "3" * 64,
                        "usage": {"total_tokens": 10},
                    },
                    {
                        "sequence": 1,
                        "requested_model": "gpt-4o",
                        "response_id": "response-1",
                        "response_model": "gpt-4o-2024-08-06",
                        "request_sha256": "4" * 64,
                        "response_sha256": "5" * 64,
                        "parsed_response_sha256": "6" * 64,
                        "usage": {"total_tokens": 11},
                    },
                ],
            },
        )

        fd_source = self.root / "benchmarks/external/fd.json"
        write_json(
            fd_source,
            {
                "upstream_revision": "fd-revision",
                "dataset_revision": "dataset-revision",
                "expected_released_conversations": 1,
                "expected_cell_count": 1,
                "expected_cell_populations": {"cell": 1},
                "evaluation_contract": {
                    "output_vad": "silero-vad",
                    "output_vad_package_version": "6.2.1",
                    "output_vad_threshold": 0.5,
                    "output_min_silence_duration_ms": 1500,
                    "input_timestamp_rate_hz": 16000,
                },
            },
        )
        fd_run = self.root / ".runtime/fd/run.json"
        fd_context = self.root / ".runtime/fd/context.json"
        fd_guard = self.gpu_guard(
            self.root / ".runtime/fd/gpu-ownership-invocation-0.summary.json"
        )
        write_json(fd_context, run_context("fd-bench", fd_guard))
        write_json(
            fd_run,
            {
                "schema_version": "1.0.0",
                "benchmark": "FD-Bench",
                "revision": "fd-revision",
                "dataset_revision": "dataset-revision",
                "descriptor": descriptor("fd-bench-standard-realtime-v1"),
                "trial_attempts": 3,
                "samples": [{"cell": "cell", "id": "conversation_1"}],
                "completed": ["cell/conversation_1"],
                "failures": [],
                "attempts": [
                    {
                        "sample": "cell/conversation_1",
                        "number": 1,
                        "succeeded": True,
                    }
                ],
            },
        )
        trace = self.root / ".runtime/fd/output/cell/openrealtime.txt"
        trace.parent.mkdir(parents=True, exist_ok=True)
        trace.write_text(
            "conversation_1.wav || [] || [] || [] || []\n", encoding="utf-8"
        )
        fd_finalization = self.root / ".runtime/fd/finalization.json"
        write_json(
            fd_finalization,
            {
                "benchmark": "FD-Bench",
                "revision": "fd-revision",
                "vad": {
                    "name": "silero-vad",
                    "package_version": "6.2.1",
                    "threshold": 0.5,
                    "min_silence_duration_ms": 1500,
                    "timestamp_rate_hz": 16000,
                },
                "results": 1,
                "result_evidence": {
                    "algorithm": "sha256(path\\0size\\0file_sha256\\n)",
                    "digest": "a" * 64,
                    "files": 1,
                    "bytes": 1,
                },
                "audio_evidence": {
                    "algorithm": "sha256(path\\0size\\0file_sha256\\n)",
                    "digest": "b" * 64,
                    "files": 1,
                    "bytes": 1,
                },
                "traces": {
                    "cell": {
                        "path": self.relative(trace),
                        "samples": 1,
                        "sha256": REPORT.sha256_file(trace),
                    }
                },
            },
        )
        fd_metric = self.root / ".runtime/fd/metrics/cell.json"
        write_json(
            fd_metric,
            {
                "schema_version": "1.0.0",
                "benchmark": "FD-Bench",
                "revision": "fd-revision",
                "trace": {
                    "path": self.relative(trace),
                    "samples": 1,
                    "sha256": REPORT.sha256_file(trace),
                },
                "metrics": {
                    "SRR_pct": 1,
                    "SIR_pct": 2,
                    "EIR_pct": 3,
                    "NIR_pct": 4,
                    "SRIR_pct": 5,
                    "FSED_ms": 6,
                    "ERT_ms": 7,
                    "EIT_ms": 8,
                    "IRD_ms": 9,
                },
                "not_measured": {},
                "counts": {
                    "rounds": 4,
                    "interruptions": 2,
                    "gaps": 1,
                    "success_responses": 4,
                    "success_responses_to_interruption": 2,
                    "success_interruptions": 2,
                    "early_interruptions": 0,
                    "noise_interruptions": 0,
                },
                "categories": {},
                "not_evaluated": {
                    "WER": "not emitted",
                    "CPPL": "not emitted",
                    "subjective_GPT_score": "not emitted",
                },
            },
        )

        paired_definition = self.root / "benchmarks/tau-voice/pair-v1.json"
        write_json(paired_definition, {"ablation_id": "pair-v1"})
        continuous_evidence = self.root / ".runtime/paired/continuous.json"
        write_json(continuous_evidence, {"status": "complete"})
        endpoint_evidence = self.root / ".runtime/paired/endpoint.json"
        write_json(endpoint_evidence, {"status": "complete"})
        paired_report = self.root / ".runtime/paired/report.json"
        write_json(
            paired_report,
            {
                "status": "complete",
                "ablation": {
                    "id": "pair-v1",
                    "path": self.relative(paired_definition),
                    "sha256": REPORT.sha256_file(paired_definition),
                },
                "comparison": {
                    "provider_work": {
                        "continuous": {
                            "fast_preparation": {"invocations": 4},
                            "slow_preparation": {"invocations": 3},
                        },
                        "endpoint_only": {
                            "fast_preparation": {"invocations": 0},
                            "slow_preparation": {"invocations": 0},
                        },
                    }
                },
                "evidence": {
                    "continuous_report": self.pin(continuous_evidence),
                    "endpoint_report": self.pin(endpoint_evidence),
                },
            },
        )

        study = self.root / "benchmarks/full-study.json"
        write_json(
            study,
            {
                "schema_version": "1.0.0",
                "study_id": "study",
                "status": "preregistered",
                "publication_policy": {
                    "partial_results": "forbidden",
                    "cross_benchmark_composite": "forbidden",
                    "presentation": "benchmark-specific evidence panel",
                },
                "runtime": self.pin(runtime_manifest),
                "tau_voice": {
                    "artifact_retention": {
                        "scoring_inputs": "results.json and simulations/*.json remain expanded",
                        "raw_artifacts": "each complete cell/domain artifacts directory is preserved losslessly as deterministic-pax-tar+zstd",
                        "deletion_gate": "remove expanded duplicates only after archive readability, SHA-256, byte count, matrix identity, exact task population, and the bounded exception-only attempt ledger are recorded",
                        "publication_gate": "the terminal reporter rehashes every archive and rejects missing evidence, invalid retry provenance, or remaining expanded duplicates",
                    },
                    "source_manifest": self.pin(tau_source),
                    "matrices": [self.pin(matrix)],
                    "paired_reports": [
                        {
                            "id": "pair-v1",
                            "definition": self.pin(paired_definition),
                            "report": self.relative(paired_report),
                        }
                    ],
                },
                "full_duplex_bench_v1_5": {
                    "source_manifest": self.pin(fdb15_source),
                    "upstream_revision": "fdb-revision",
                    "population": 1,
                    "conditions": ["overlap"],
                    "replicates": 1,
                    "trial_attempts": 3,
                    "run_context": self.relative(fdb15_context),
                    "run_manifest": self.relative(fdb15_run),
                    "summary": self.relative(fdb15_summary),
                },
                "full_duplex_bench_v3": {
                    "source_manifest": self.pin(fdbv3_source),
                    "profile": self.pin(fdbv3_profile),
                    "upstream_revision": "fdb-revision",
                    "population": 1,
                    "trial_attempts": 3,
                    "run_context": self.relative(fdbv3_context),
                    "run_manifest": self.relative(fdbv3_run),
                    "evaluations": {
                        "exact": self.relative(fdbv3_exact),
                        "gpt4o": self.relative(fdbv3_judge),
                        "gpt4o_evidence": self.relative(fdbv3_judge_evidence),
                    },
                },
                "fd_bench": {
                    "source_manifest": self.pin(fd_source),
                    "upstream_revision": "fd-revision",
                    "dataset_revision": "dataset-revision",
                    "population": 1,
                    "cells": 1,
                    "trial_attempts": 3,
                    "run_context": self.relative(fd_context),
                    "run_manifest": self.relative(fd_run),
                    "finalization": self.relative(fd_finalization),
                    "metrics_directory": self.relative(fd_metric.parent),
                    "explicit_exclusions": [
                        "WER",
                        "CPPL",
                        "subjective_GPT_score",
                    ],
                },
            },
        )
        self.paths = {
            "study": study,
            "fdb15_run": fdb15_run,
            "fdbv3_run": fdbv3_run,
            "fd_run": fd_run,
            "fdbv3_judge": fdbv3_judge,
            "fdbv3_judge_evidence": fdbv3_judge_evidence,
            "fd_metric": fd_metric,
            "fd_context": fd_context,
            "fdb15_output": fdb15_output,
            "fdb15_result": fdb15_output.parent / "result_overlap.json",
            "matrix": matrix,
            "tau_archive": tau_archive,
            "tau_experiment": tau_experiment,
            "tau_run": tau_run,
            "paired_report": paired_report,
        }

    @staticmethod
    def evaluation(response_score: float | None) -> dict:
        return {
            "total_scenarios": 1,
            "turn_taking": {"total": 1, "turn_taken": 1},
            "by_metric": {"response_qual": response_score},
            "latency": {"total_samples": 1},
            "scenario_results": [
                {
                    "scenario_id": "example",
                    "metrics": {"response_qual": {"score": response_score}},
                }
            ],
        }


class FullStudyTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.fixture = Fixture(self.root)

    def report(self, source_state: tuple[str, bool] = ("c" * 40, True)) -> dict:
        with mock.patch.object(REPORT, "git_source_state", return_value=source_state):
            return REPORT.build_report(self.root, self.fixture.paths["study"])

    def test_accepts_only_the_complete_exact_population(self) -> None:
        report = self.report()
        self.assertEqual(report["status"], "complete")
        self.assertEqual(
            report["evidence_panel"]["tau_voice"][
                "population_across_preregistered_conditions"
            ],
            1,
        )
        self.assertEqual(
            len(
                report["evidence_panel"]["tau_voice"]["matrices"][0][
                    "raw_artifact_archives"
                ]
            ),
            1,
        )
        self.assertEqual(
            report["interpretation"]["aggregation"], "none across benchmark families"
        )

    def test_rejects_terminal_failures_even_with_a_completed_row(self) -> None:
        path = self.fixture.paths["fdb15_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        run["failures"] = [{"sample_id": "1"}]
        write_json(path, run)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "terminal failures"):
            self.report()

    def test_rejects_a_run_manifest_without_a_terminal_failure_ledger(self) -> None:
        for key in ("fdb15_run", "fdbv3_run", "fd_run"):
            with self.subTest(run=key):
                self.setUp()
                path = self.fixture.paths[key]
                run = json.loads(path.read_text(encoding="utf-8"))
                del run["failures"]
                write_json(path, run)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, "terminal failure ledger"
                ):
                    self.report()

    def test_rejects_a_terminal_failure_ledger_that_is_not_a_list(self) -> None:
        for key in ("fdb15_run", "fdbv3_run", "fd_run"):
            with self.subTest(run=key):
                self.setUp()
                path = self.fixture.paths[key]
                run = json.loads(path.read_text(encoding="utf-8"))
                run["failures"] = 0
                write_json(path, run)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, "terminal failure ledger"
                ):
                    self.report()

    def test_rejects_fdb15_audio_without_a_declared_hash(self) -> None:
        path = self.fixture.paths["fdb15_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        for item in run["completed"]:
            del item["output_sha256"]
            (self.root / item["output_wav"]).write_bytes(b"tampered audio")
        write_json(path, run)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "did not record a valid output_sha256"
        ):
            self.report()

    def test_rejects_a_tau_run_that_never_recorded_its_cell(self) -> None:
        """A recorded null claims whole-matrix coverage; absence must not."""
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        document = json.loads(path.read_text(encoding="utf-8"))
        del document["execution_evidence"][0]["selected_cell"]
        write_json(path, document)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "did not record which cell it ran"
        ):
            self.report()

    def test_rejects_a_tau_cell_with_no_headline_result(self) -> None:
        cases = (
            (("overall",), "recorded no overall result"),
            (("overall", "agent_metrics"), "recorded no agent metrics"),
            (("overall", "agent_metrics", "avg_reward"), "recorded no average reward"),
        )
        for path_keys, message in cases:
            with self.subTest(field=".".join(path_keys)):
                self.setUp()
                path = (
                    self.root
                    / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
                )
                document = json.loads(path.read_text(encoding="utf-8"))
                container = document["cells"]["control"]
                for key in path_keys[:-1]:
                    container = container[key]
                del container[path_keys[-1]]
                write_json(path, document)
                with self.assertRaisesRegex(REPORT.StudyIncompleteError, message):
                    self.report()

    def test_rejects_a_study_that_declares_no_presentation_policy(self) -> None:
        path = self.fixture.paths["study"]
        document = json.loads(path.read_text(encoding="utf-8"))
        del document["publication_policy"]["presentation"]
        write_json(path, document)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "declares no presentation policy"
        ):
            self.report()

    def test_rejects_a_paired_report_that_omits_a_declaration(self) -> None:
        """An omitted key must refuse by name, not die with a KeyError."""
        path = self.fixture.paths["study"]
        pristine = path.read_text(encoding="utf-8")
        for key in ("id", "definition", "report"):
            with self.subTest(key=key):
                document = json.loads(pristine)
                del document["tau_voice"]["paired_reports"][0][key]
                write_json(path, document)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, f"tau paired report 0 declares no {key}"
                ):
                    self.report()
        path.write_text(pristine, encoding="utf-8")

    def test_rejects_an_emptied_population_that_would_verify_nothing(self) -> None:
        """An empty list is a list: the type check passes and every loop below it
        iterates zero times, so a study that validated nothing publishes as if it
        had validated everything."""
        path = self.fixture.paths["study"]
        pristine = path.read_text(encoding="utf-8")
        for pointer, message in (
            (("tau_voice", "paired_reports"), "declares no tau-Voice paired reports"),
        ):
            with self.subTest(pointer="/".join(pointer)):
                document = json.loads(pristine)
                node = document
                for key in pointer[:-1]:
                    node = node[key]
                node[pointer[-1]] = []
                write_json(path, document)
                with self.assertRaisesRegex(REPORT.StudyIncompleteError, message):
                    self.report()
        path.write_text(pristine, encoding="utf-8")

    def test_rejects_a_paired_report_whose_continuous_arm_did_nothing(self) -> None:
        """Both arms silent means the two conditions are one condition."""
        for phase in ("fast_preparation", "slow_preparation"):
            with self.subTest(phase=phase):
                path = self.fixture.paths["paired_report"]
                document = json.loads(path.read_text(encoding="utf-8"))
                work = document["comparison"]["provider_work"]
                work["continuous"][phase]["invocations"] = 0
                write_json(path, document)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, "manipulated nothing"
                ):
                    self.report()
                work["continuous"][phase]["invocations"] = 4
                write_json(path, document)

    def test_rejects_a_paired_report_with_no_continuous_provenance(self) -> None:
        """An absent continuous block is not a continuous block of zero."""
        path = self.fixture.paths["paired_report"]
        document = json.loads(path.read_text(encoding="utf-8"))
        del document["comparison"]["provider_work"]["continuous"]
        write_json(path, document)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "manipulated nothing"
        ):
            self.report()

    def test_rejects_a_paired_report_whose_endpoint_arm_worked(self) -> None:
        path = self.fixture.paths["paired_report"]
        document = json.loads(path.read_text(encoding="utf-8"))
        document["comparison"]["provider_work"]["endpoint_only"][
            "fast_preparation"
        ]["invocations"] = 1
        write_json(path, document)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "endpoint-only fast_preparation"
        ):
            self.report()

    def test_rejects_a_study_that_declares_no_paired_reports(self) -> None:
        """Absent declaration makes the paired-report loop verify nothing."""
        path = self.fixture.paths["study"]
        document = json.loads(path.read_text(encoding="utf-8"))
        del document["tau_voice"]["paired_reports"]
        write_json(path, document)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "declares no tau-Voice paired reports"
        ):
            self.report()

    def test_rejects_an_fdbv3_evaluation_missing_a_population_block(self) -> None:
        """turn-taking and latency figures need the populations behind them."""
        cases = (
            (("turn_taking",), "recorded no turn-taking counts"),
            (("turn_taking", "total"), "turn-taking total is not a count"),
            (("turn_taking", "turn_taken"), "turn-taking turn_taken is not a count"),
            (("latency",), "recorded no latency population"),
            (("latency", "total_samples"), "latency sample population is not a count"),
            (("by_metric",), "recorded no aggregate metrics"),
        )
        for path_keys, message in cases:
            with self.subTest(field=".".join(path_keys)):
                self.setUp()
                path = self.root / ".runtime/fdbv3/exact.json"
                document = json.loads(path.read_text(encoding="utf-8"))
                container = document
                for key in path_keys[:-1]:
                    container = container[key]
                del container[path_keys[-1]]
                write_json(path, document)
                with self.assertRaisesRegex(REPORT.StudyIncompleteError, message):
                    self.report()

    def test_rejects_an_fdbv3_scenario_that_never_recorded_a_score(self) -> None:
        """An absent metrics block must not read as the exact evaluation's null."""
        cases = (
            (("metrics",), "recorded no metrics"),
            (("metrics", "response_qual"), "recorded no response_qual metric"),
            (("metrics", "response_qual", "score"), "recorded no score"),
        )
        for path_keys, message in cases:
            with self.subTest(field=".".join(path_keys)):
                self.setUp()
                path = self.root / ".runtime/fdbv3/exact.json"
                document = json.loads(path.read_text(encoding="utf-8"))
                container = document["scenario_results"][0]
                for key in path_keys[:-1]:
                    container = container[key]
                del container[path_keys[-1]]
                write_json(path, document)
                with self.assertRaisesRegex(REPORT.StudyIncompleteError, message):
                    self.report()

    def test_rejects_fdbv3_audio_without_a_declared_hash(self) -> None:
        for result in self.root.rglob("result_openrealtime.json"):
            document = json.loads(result.read_text(encoding="utf-8"))
            del document["openrealtime"]["output_sha256"]
            write_json(result, document)
            (result.parent / "output_openrealtime.wav").write_bytes(b"tampered audio")
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "did not record a valid output_sha256"
        ):
            self.report()

    def test_rejects_an_archive_without_its_failed_attempt_count(self) -> None:
        for archive in self.root.rglob("raw-artifacts-archive.json"):
            document = json.loads(archive.read_text(encoding="utf-8"))
            del document["attempts"]["failed_infrastructure"]
            write_json(archive, document)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "failed infrastructure attempts"
        ):
            self.report()

    def test_rejects_a_panel_field_that_carries_no_value(self) -> None:
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        del metric["categories"]
        write_json(path, metric)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "evidence panel field has no value"
        ):
            self.report()

    def test_rejects_a_cell_that_scored_no_rounds(self) -> None:
        """A full set of metric keys computed over nothing is not a result.

        The trace population check counts lines the runner wrote; it passes
        whether or not the decision core scored a single round.
        """
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        metric["counts"]["rounds"] = 0
        write_json(path, metric)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "scored no rounds"
        ):
            self.report()

    def test_rejects_a_cell_that_never_recorded_its_scored_population(self) -> None:
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        metric.pop("counts")
        write_json(path, metric)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "scored round population"
        ):
            self.report()

    def test_rejects_a_cell_missing_a_single_scored_population(self) -> None:
        """A rate whose denominator vanished is published over nothing.

        The panel copies counts through wholesale, so a dropped counter is an
        absent key rather than a null and the no-unexplained-nulls rule cannot
        see it.
        """
        for counter in ("rounds", "interruptions", "gaps"):
            with self.subTest(counter=counter):
                self.setUp()
                path = self.fixture.paths["fd_metric"]
                metric = json.loads(path.read_text(encoding="utf-8"))
                metric["counts"].pop(counter)
                write_json(path, metric)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, "scored populations"
                ):
                    self.report()

    def test_rejects_a_scored_population_that_is_not_a_count(self) -> None:
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        metric["counts"]["interruptions"] = -1
        write_json(path, metric)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "non-negative integers"
        ):
            self.report()

    def test_rejects_an_fd_sample_that_never_recorded_its_identity(self) -> None:
        """Sample identity is what the planned population is counted against."""
        for field in ("cell", "id"):
            with self.subTest(field=field):
                self.setUp()
                path = self.fixture.paths["fd_run"]
                run = json.loads(path.read_text(encoding="utf-8"))
                run["samples"][0].pop(field)
                write_json(path, run)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, f"recorded no {field}"
                ):
                    self.report()

    def test_rejects_an_fd_metric_that_never_recorded_its_scored_trace(self) -> None:
        """The metric must say which trace it scored, not have it assumed.

        This check previously defaulted the metric's trace path to the
        finalization's own path, so it compared that path against itself and
        passed for any metric file that stayed silent.
        """
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        metric["trace"].pop("path")
        write_json(path, metric)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "recorded no path"):
            self.report()

    def test_rejects_an_fd_metric_that_scored_a_different_trace(self) -> None:
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        metric["trace"]["path"] = ".runtime/fd/some-other-trace.jsonl"
        write_json(path, metric)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "metric trace path"
        ):
            self.report()

    def test_rejects_an_fd_trace_that_never_recorded_its_path(self) -> None:
        path = self.fixture.root / ".runtime/fd/finalization.json"
        finalization = json.loads(path.read_text(encoding="utf-8"))
        finalization["traces"]["cell"].pop("path")
        write_json(path, finalization)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "recorded no path"):
            self.report()

    def test_rejects_an_fdb15_trial_that_never_recorded_its_sample(self) -> None:
        """A completed trial that names no sample is checked against nothing."""
        path = self.fixture.paths["fdb15_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        run["completed"][0].pop("sample")
        write_json(path, run)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "is not a record"):
            self.report()

    def test_rejects_an_fdb15_trial_sample_missing_its_identity(self) -> None:
        for field in ("scenario", "id"):
            with self.subTest(field=field):
                self.setUp()
                path = self.fixture.paths["fdb15_run"]
                run = json.loads(path.read_text(encoding="utf-8"))
                run["completed"][0]["sample"].pop(field)
                write_json(path, run)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, f"recorded no {field}"
                ):
                    self.report()

    def test_rejects_an_fdb15_summary_condition_missing_its_population(self) -> None:
        path = self.fixture.root / ".runtime/fdb15/summary.json"
        summary = json.loads(path.read_text(encoding="utf-8"))
        summary["conditions"][0].pop("completed")
        write_json(path, summary)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "recorded no completed count"
        ):
            self.report()

    def test_rejects_an_fdbv3_sample_that_never_recorded_its_identity(self) -> None:
        for field in ("example_id", "pid", "directory", "input_sha256"):
            with self.subTest(field=field):
                self.setUp()
                path = self.fixture.paths["fdbv3_run"]
                run = json.loads(path.read_text(encoding="utf-8"))
                run["samples"][0].pop(field)
                write_json(path, run)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, f"recorded no {field}"
                ):
                    self.report()

    def test_rejects_a_manifest_missing_a_structural_field(self) -> None:
        """A malformed preregistration must refuse, not raise a KeyError."""
        for pointer in (
            ("study_id",),
            ("runtime", "sha256"),
            ("fd_bench",),
            ("fd_bench", "cells"),
            ("full_duplex_bench_v1_5", "replicates"),
            ("full_duplex_bench_v3", "evaluations", "gpt4o_evidence"),
            ("tau_voice", "matrices"),
        ):
            with self.subTest(pointer=".".join(pointer)):
                self.setUp()
                path = self.fixture.paths["study"]
                manifest = json.loads(path.read_text(encoding="utf-8"))
                container = manifest
                for step in pointer[:-1]:
                    container = container[step]
                container.pop(pointer[-1])
                write_json(path, manifest)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, f"declares no {pointer[-1]}"
                ):
                    self.report()

    def test_rejects_a_tau_matrix_entry_missing_its_hash(self) -> None:
        path = self.fixture.paths["study"]
        manifest = json.loads(path.read_text(encoding="utf-8"))
        manifest["tau_voice"]["matrices"][0].pop("sha256")
        write_json(path, manifest)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, r"matrices\[0\] declares no sha256"
        ):
            self.report()

    def test_rejects_an_undefined_metric_with_no_explanation(self) -> None:
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        metric["metrics"]["SIR_pct"] = None
        write_json(path, metric)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "undefined metrics and their explanations"
        ):
            self.report()

    def test_rejects_an_explanation_with_no_undefined_metric(self) -> None:
        """A stale explanation is a claim about evidence that is not there."""
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        metric["not_measured"]["SIR_pct"] = "no observed interruptions"
        write_json(path, metric)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "undefined metrics and their explanations"
        ):
            self.report()

    def test_publishes_an_undefined_metric_by_name_not_as_a_null(self) -> None:
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        metric["metrics"]["SIR_pct"] = None
        metric["not_measured"]["SIR_pct"] = "no observed interruptions"
        write_json(path, metric)
        panel = self.report()["evidence_panel"]["fd_bench"]["metrics"]["cell"]
        self.assertNotIn("SIR_pct", panel["metrics"])
        self.assertIn("SIR_pct", panel["not_measured"])

    def test_permits_only_the_declared_empty_panel_field(self) -> None:
        report = self.report()
        nulls = {path for path, _ in REPORT.null_panel_fields(report)}
        self.assertEqual(nulls, set(REPORT.PERMITTED_NULL_PANEL_FIELDS))

    def test_rejects_a_study_that_declares_no_tau_matrix(self) -> None:
        path = self.fixture.paths["study"]
        study = json.loads(path.read_text(encoding="utf-8"))
        study["tau_voice"]["matrices"] = []
        write_json(path, study)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "declares no tau-Voice matrix"
        ):
            self.report()

    def test_rejects_an_external_suite_declared_as_an_empty_population(self) -> None:
        for key, suite in (
            ("full_duplex_bench_v1_5", "FDB1.5"),
            ("full_duplex_bench_v3", "FDBv3"),
            ("fd_bench", "FD-Bench"),
        ):
            with self.subTest(suite=suite):
                self.setUp()
                path = self.fixture.paths["study"]
                study = json.loads(path.read_text(encoding="utf-8"))
                study[key]["population"] = 0
                write_json(path, study)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, "declares no preregistered population"
                ):
                    self.report()

    def test_rejects_an_external_trial_over_its_lifetime_attempt_budget(self) -> None:
        path = self.fixture.paths["fdb15_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        run["attempts"] = [
            {
                "sample_id": "1",
                "scenario": "background",
                "condition": "overlap",
                "replicate": 0,
                "attempt": number,
                "succeeded": number == 4,
                **({} if number == 4 else {"error": "infrastructure failure"}),
            }
            for number in range(1, 5)
        ]
        write_json(path, run)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "attempt count"):
            self.report()

    def test_rejects_duplicate_external_attempt_numbers(self) -> None:
        path = self.fixture.paths["fdbv3_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        run["attempts"] = [
            {
                "sample": "example_pid",
                "number": 1,
                "succeeded": False,
                "error": "infrastructure failure",
            },
            {"sample": "example_pid", "number": 1, "succeeded": True},
        ]
        write_json(path, run)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "attempt numbering"):
            self.report()

    def test_rejects_external_attempt_ledger_without_success(self) -> None:
        path = self.fixture.paths["fd_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        run["attempts"] = [
            {
                "sample": "cell/conversation_1",
                "number": 1,
                "succeeded": False,
                "error": "infrastructure failure",
            }
        ]
        write_json(path, run)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "successful terminal attempt"
        ):
            self.report()

    def test_rejects_an_incomplete_tau_termination_distribution(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        report["cells"]["control"]["domains"]["airline"]["population"][
            "termination_reasons"
        ] = {}
        write_json(path, report)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "termination population"
        ):
            self.report()

    def test_rejects_missing_runtime_process_identity(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        del report["execution_evidence"][0]["runtime_identity"]["components"]["gateway"]
        write_json(path, report)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "runtime components"):
            self.report()

    def test_rejects_unregistered_gpu_process_at_runtime_capture(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        report["execution_evidence"][0]["runtime_identity"]["gpu_ownership"][
            "processes"
        ].append(
            {
                "gpu_uuid": "GPU-test",
                "component": "foreign",
                "component_pid": 99,
                "pid": 100,
                "proc_start_time_ticks": "300",
                "used_memory_mib": 1,
            }
        )
        write_json(path, report)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "unknown component"):
            self.report()

    def test_rejects_missing_tau_gpu_guard_coverage(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        report["execution_evidence"][0]["gpu_ownership_guards"] = []
        write_json(path, report)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "GPU ownership guard coverage"
        ):
            self.report()

    def test_rejects_multiple_tau_process_invocations(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        report["execution_evidence"].append(report["execution_evidence"][0])
        write_json(path, report)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "source-stable invocation count"
        ):
            self.report()

    def test_rejects_tau_run_artifact_drift(self) -> None:
        path = self.fixture.paths["tau_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        run["started_at"] = "2026-01-02T00:00:00Z"
        write_json(path, run)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "run SHA-256"):
            self.report()

    def test_rejects_transient_gpu_ownership_violation(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        guard = report["execution_evidence"][0]["gpu_ownership_guards"][0]
        summary_path = self.root / guard["path"]
        summary = json.loads(summary_path.read_text(encoding="utf-8"))
        log_path = self.root / summary["log"]["path"]
        records = [
            json.loads(line)
            for line in log_path.read_text(encoding="utf-8").splitlines()
        ]
        records[1] = {
            "type": "guard.check",
            "status": "violation",
            "recorded_at": "2026-01-01T00:00:01Z",
            "error": "foreign GPU process",
        }
        log_path.write_text(
            "".join(
                json.dumps(record, separators=(",", ":")) + "\n" for record in records
            ),
            encoding="utf-8",
        )
        summary["log"]["sha256"] = REPORT.sha256_file(log_path)
        summary["log"]["bytes"] = log_path.stat().st_size
        write_json(summary_path, summary)
        guard["sha256"] = REPORT.sha256_file(summary_path)
        guard["bytes"] = summary_path.stat().st_size
        guard["evidence"] = summary
        write_json(path, report)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "check 0 is not successful"
        ):
            self.report()

    def test_rejects_an_unsampled_gpu_ownership_interval(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        guard = report["execution_evidence"][0]["gpu_ownership_guards"][0]
        summary_path = self.root / guard["path"]
        summary = json.loads(summary_path.read_text(encoding="utf-8"))
        log_path = self.root / summary["log"]["path"]
        records = [
            json.loads(line)
            for line in log_path.read_text(encoding="utf-8").splitlines()
        ]
        records[-1]["recorded_at"] = "2026-01-01T01:00:00Z"
        log_path.write_text(
            "".join(
                json.dumps(record, separators=(",", ":")) + "\n" for record in records
            ),
            encoding="utf-8",
        )
        summary["completed_at"] = records[-1]["recorded_at"]
        summary["log"]["sha256"] = REPORT.sha256_file(log_path)
        summary["log"]["bytes"] = log_path.stat().st_size
        write_json(summary_path, summary)
        guard["sha256"] = REPORT.sha256_file(summary_path)
        guard["bytes"] = summary_path.stat().st_size
        guard["evidence"] = summary
        write_json(path, report)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "unsampled interval"):
            self.report()

    def test_rejects_tau_execution_on_a_different_gateway_binary(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        report["execution_evidence"][0]["runtime_identity"]["components"]["gateway"][
            "executable_sha256"
        ] = "d" * 64
        write_json(path, report)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "frozen executable"):
            self.report()

    def test_rejects_a_reduced_local_fast_context_window(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        report["execution_evidence"][0]["runtime_identity"]["components"]["qwen"][
            "service"
        ]["max_model_len"] = 16384
        write_json(path, report)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "qwen service contract"
        ):
            self.report()

    def test_accepts_remote_fast_runtime_without_qwen(self) -> None:
        identity = runtime_identity()
        del identity["components"]["qwen"]
        identity["gpu_ownership"]["expected_components"] = ["asr", "fish"]
        del identity["gpu_ownership"]["component_roots"]["qwen"]
        identity["gpu_ownership"]["processes"] = [
            process
            for process in identity["gpu_ownership"]["processes"]
            if process["component"] != "qwen"
        ]
        REPORT.validate_runtime_identity(
            identity,
            requires_local_fast=False,
            expected_gateway_sha256="a" * 64,
            label="remote-fast",
        )

    def test_rejects_tau_source_revision_change(self) -> None:
        path = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        report = json.loads(path.read_text(encoding="utf-8"))
        report["execution_evidence"][0]["openrealtime_revision_final"] = "d" * 40
        write_json(path, report)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "source revision at completion"
        ):
            self.report()

    def test_rejects_tau_raw_artifact_archive_drift(self) -> None:
        path = self.fixture.paths["tau_archive"]
        payload = bytearray(path.read_bytes())
        payload[0] ^= 1
        path.write_bytes(payload)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "SHA-256"):
            self.report()

    def test_rejects_unreclaimed_expanded_tau_artifacts(self) -> None:
        artifacts = self.fixture.paths["tau_experiment"] / "artifacts"
        artifacts.mkdir()
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "expanded duplicate artifacts remain"
        ):
            self.report()

    def test_rejects_external_runtime_replacement(self) -> None:
        path = self.fixture.paths["fd_context"]
        context = json.loads(path.read_text(encoding="utf-8"))
        context["invocations"][0]["runtime_identity_final"]["components"]["gateway"][
            "proc_start_time_ticks"
        ] = "101"
        write_json(path, context)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "runtime processes"):
            self.report()

    def test_rejects_multiple_external_process_invocations(self) -> None:
        path = self.fixture.paths["fd_context"]
        context = json.loads(path.read_text(encoding="utf-8"))
        context["invocations"].append(context["invocations"][0])
        write_json(path, context)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "source-stable invocation count"
        ):
            self.report()

    def test_rejects_cross_benchmark_orchestration_revision_drift(self) -> None:
        path = self.fixture.paths["fd_context"]
        context = json.loads(path.read_text(encoding="utf-8"))
        context["invocations"][0]["openrealtime_revision_start"] = "d" * 40
        context["invocations"][0]["openrealtime_revision_at_completion"] = "d" * 40
        write_json(path, context)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError,
            "source-stable orchestration revision count",
        ):
            self.report()

    def test_rejects_a_different_terminal_reporter_revision(self) -> None:
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError,
            "terminal reporter orchestration revision",
        ):
            self.report(("d" * 40, True))

    def test_rejects_a_dirty_terminal_reporter_worktree(self) -> None:
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError,
            "terminal reporter clean source",
        ):
            self.report(("c" * 40, False))

    def test_rejects_external_run_without_the_frozen_gateway_declaration(self) -> None:
        path = self.fixture.paths["fd_context"]
        context = json.loads(path.read_text(encoding="utf-8"))
        context["invocations"][0]["study_gateway_sha256"] = None
        write_json(path, context)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "frozen gateway declaration"
        ):
            self.report()

    def test_rejects_external_source_revision_change(self) -> None:
        path = self.fixture.paths["fd_context"]
        context = json.loads(path.read_text(encoding="utf-8"))
        context["invocations"][0]["openrealtime_revision_at_completion"] = "d" * 40
        write_json(path, context)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "source revision at completion"
        ):
            self.report()

    def test_rejects_a_missing_official_llm_judge_score(self) -> None:
        path = self.fixture.paths["fdbv3_judge"]
        write_json(path, Fixture.evaluation(None))
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "LLM response score"):
            self.report()

    def test_rejects_silent_official_judge_fallback(self) -> None:
        path = self.fixture.paths["fdbv3_judge_evidence"]
        evidence = json.loads(path.read_text(encoding="utf-8"))
        evidence["successful_valid_calls"] = 1
        write_json(path, evidence)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "successful judge"):
            self.report()

    def test_rejects_an_incomplete_official_judge_receipt(self) -> None:
        path = self.fixture.paths["fdbv3_judge_evidence"]
        evidence = json.loads(path.read_text(encoding="utf-8"))
        evidence["calls"][0]["response_sha256"] = None
        write_json(path, evidence)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "response_sha256"):
            self.report()

    def test_rejects_an_unreported_fdbench_exclusion(self) -> None:
        path = self.fixture.paths["fd_metric"]
        metric = json.loads(path.read_text(encoding="utf-8"))
        del metric["not_evaluated"]["WER"]
        write_json(path, metric)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "exclusions"):
            self.report()

    def test_rejects_frozen_definition_drift(self) -> None:
        path = self.fixture.paths["matrix"]
        matrix = json.loads(path.read_text(encoding="utf-8"))
        matrix["benchmark"]["num_trials"] = 2
        write_json(path, matrix)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "SHA-256"):
            self.report()

    def test_rejects_raw_output_drift(self) -> None:
        self.fixture.paths["fdb15_output"].write_bytes(b"changed-audio")
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "output.wav SHA-256"):
            self.report()

    def test_rejects_an_fdb15_raw_result_that_contradicts_its_trial(self) -> None:
        """The runner records no hash for this file, so only agreement binds it."""
        for field, expected in (
            ("trial_id", "raw result trial_id"),
            ("condition", "raw result condition"),
            ("attempt", "raw result attempt"),
            ("input_sha256", "raw result input_sha256"),
            ("output_sha256", "raw result output_sha256"),
            ("output_wav", "raw result output path"),
            ("output_wav", "raw result records no output path"),
        ):
            with self.subTest(field=field, expected=expected):
                self.setUp()
                path = self.fixture.paths["fdb15_result"]
                raw = json.loads(path.read_text(encoding="utf-8"))
                raw[field] = 9 if "records no" in expected else (
                    "e" * 64 if field.endswith("sha256") else "elsewhere/other.wav"
                )
                write_json(path, raw)
                with self.assertRaisesRegex(REPORT.StudyIncompleteError, expected):
                    self.report()

    def test_rejects_an_fdb15_raw_result_missing_a_bound_field(self) -> None:
        """Absence must not read as agreement the way a shared null would."""
        for field, expected in (
            ("trial_id", "raw result trial_id"),
            ("condition", "raw result condition"),
            ("attempt", "raw result attempt"),
            ("input_sha256", "did not record a valid input_sha256"),
            ("output_sha256", "did not record a valid output_sha256"),
            ("output_wav", "raw result records no output path"),
            ("sample", "raw result records no sample"),
        ):
            with self.subTest(field=field):
                self.setUp()
                path = self.fixture.paths["fdb15_result"]
                raw = json.loads(path.read_text(encoding="utf-8"))
                del raw[field]
                write_json(path, raw)
                with self.assertRaisesRegex(REPORT.StudyIncompleteError, expected):
                    self.report()

    def test_rejects_an_fdb15_raw_result_from_another_sample(self) -> None:
        for field in ("scenario", "id"):
            with self.subTest(field=field):
                self.setUp()
                path = self.fixture.paths["fdb15_result"]
                raw = json.loads(path.read_text(encoding="utf-8"))
                raw["sample"][field] = "other"
                write_json(path, raw)
                with self.assertRaisesRegex(
                    REPORT.StudyIncompleteError, f"raw result sample {field}"
                ):
                    self.report()

    def test_rejects_a_completed_trial_without_a_declared_input_hash(self) -> None:
        path = self.fixture.paths["fdb15_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        for item in run["completed"]:
            del item["input_sha256"]
        write_json(path, run)
        with self.assertRaisesRegex(
            REPORT.StudyIncompleteError, "did not record a valid input_sha256"
        ):
            self.report()


if __name__ == "__main__":
    unittest.main()
