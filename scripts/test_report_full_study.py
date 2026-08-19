#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


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
                "execution_evidence": [
                    {
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
                ],
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
        write_json(fdb15_output.parent / "result_overlap.json", {"result": "raw"})
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
                "completed": [
                    {
                        "trial_id": "openrealtime/background/1/overlap/r000",
                        "sample": {"scenario": "background", "id": "1"},
                        "condition": "overlap",
                        "attempt": 1,
                        "output_wav": self.relative(fdb15_output),
                        "output_sha256": REPORT.sha256_file(fdb15_output),
                    }
                ],
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
                "trace": {"samples": 1, "sha256": REPORT.sha256_file(trace)},
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
                "counts": {},
                "categories": {},
                "not_evaluated": {
                    "WER": "not emitted",
                    "CPPL": "not emitted",
                    "subjective_GPT_score": "not emitted",
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
                    "paired_reports": [],
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
            "matrix": matrix,
            "tau_archive": tau_archive,
            "tau_experiment": tau_experiment,
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

    def report(self) -> dict:
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


if __name__ == "__main__":
    unittest.main()
