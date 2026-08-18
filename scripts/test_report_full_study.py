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
        "pid": 1,
        "proc_start_time_ticks": "100",
        "executable": "/runtime/component",
        "executable_sha256": "a" * 64,
        "command_sha256": "b" * 64,
        "argv": ["/runtime/component"],
    }
    return {
        "schema_version": "1.0.0",
        "host_boot_id": "boot",
        "components": {
            name: dict(component) for name in ("gateway", "asr", "fish", "qwen")
        },
    }


def run_context(benchmark: str) -> dict:
    identity = runtime_identity()
    return {
        "schema_version": "1.0.0",
        "benchmark": benchmark,
        "status": "complete",
        "invocations": [
            {
                "status": "complete",
                "source_worktree_clean_start": True,
                "openrealtime_revision_start": "c" * 40,
                "gateway_health_start": {"status": "ok"},
                "gateway_health_final": {"status": "ok"},
                "runtime_identity_start": identity,
                "runtime_identity_final": identity,
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

    def _build(self) -> None:
        tau_source = self.root / "benchmarks/external/tau.json"
        write_json(tau_source, {"benchmark": {"name": "tau-Voice"}})
        matrix = self.root / "benchmarks/tau/matrix.json"
        matrix_payload = {
            "matrix_id": "tau-mini",
            "benchmark": {
                "repository": "example",
                "revision": "tau-revision",
                "domains": [{"name": "airline", "tasks": 1}],
                "total_tasks_per_cell": 1,
                "num_trials": 1,
            },
            "cells": [{"id": "control", "speech_complexity": "control"}],
        }
        write_json(matrix, matrix_payload)
        tau_report = self.root / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
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
                "execution_evidence": [
                    {
                        "status": "complete",
                        "openrealtime_revision": "openrealtime-revision",
                        "runtime_identity": runtime_identity(),
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
        write_json(fdb15_context, run_context("full-duplex-bench-v1.5"))
        fdb15_descriptor = descriptor(
            "fdb-v1.5-openai-realtime-adapter-i1-qg-v1"
        )
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
                "samples": [{"scenario": "background", "id": "1"}],
                "completed": [
                    {
                        "trial_id": "openrealtime/background/1/overlap/r000",
                        "sample": {"scenario": "background", "id": "1"},
                        "condition": "overlap",
                        "output_wav": self.relative(fdb15_output),
                        "output_sha256": REPORT.sha256_file(fdb15_output),
                    }
                ],
                "failures": [],
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
            },
        )
        fdbv3_profile = self.root / "benchmarks/fdbv3/profile.json"
        write_json(
            fdbv3_profile,
            {"profile": "fdbv3-profile", "source": {"revision": "fdb-revision"}},
        )
        fdbv3_run = self.root / ".runtime/fdbv3/run.json"
        fdbv3_context = self.root / ".runtime/fdbv3/context.json"
        write_json(fdbv3_context, run_context("full-duplex-bench-v3"))
        write_json(
            fdbv3_run,
            {
                "schema_version": "1.0.0",
                "benchmark": "Full-Duplex-Bench v3",
                "revision": "fdb-revision",
                "profile": "fdbv3-profile",
                "profile_sha256": REPORT.sha256_file(fdbv3_profile),
                "descriptor": descriptor("fdbv3-profile"),
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
        write_json(fd_context, run_context("fd-bench"))
        write_json(
            fd_run,
            {
                "schema_version": "1.0.0",
                "benchmark": "FD-Bench",
                "revision": "fd-revision",
                "dataset_revision": "dataset-revision",
                "descriptor": descriptor("fd-bench-standard-realtime-v1"),
                "samples": [{"cell": "cell", "id": "conversation_1"}],
                "completed": ["cell/conversation_1"],
                "failures": [],
            },
        )
        trace = self.root / ".runtime/fd/output/cell/openrealtime.txt"
        trace.parent.mkdir(parents=True, exist_ok=True)
        trace.write_text("conversation_1.wav || [] || [] || [] || []\n", encoding="utf-8")
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
                "tau_voice": {
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
                    "run_context": self.relative(fdb15_context),
                    "run_manifest": self.relative(fdb15_run),
                    "summary": self.relative(fdb15_summary),
                },
                "full_duplex_bench_v3": {
                    "source_manifest": self.pin(fdbv3_source),
                    "profile": self.pin(fdbv3_profile),
                    "upstream_revision": "fdb-revision",
                    "population": 1,
                    "run_context": self.relative(fdbv3_context),
                    "run_manifest": self.relative(fdbv3_run),
                    "evaluations": {
                        "exact": self.relative(fdbv3_exact),
                        "gpt4o": self.relative(fdbv3_judge),
                    },
                },
                "fd_bench": {
                    "source_manifest": self.pin(fd_source),
                    "upstream_revision": "fd-revision",
                    "dataset_revision": "dataset-revision",
                    "population": 1,
                    "cells": 1,
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
            "fdbv3_judge": fdbv3_judge,
            "fd_metric": fd_metric,
            "fd_context": fd_context,
            "fdb15_output": fdb15_output,
            "matrix": matrix,
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
        self.assertEqual(report["interpretation"]["aggregation"], "none across benchmark families")

    def test_rejects_terminal_failures_even_with_a_completed_row(self) -> None:
        path = self.fixture.paths["fdb15_run"]
        run = json.loads(path.read_text(encoding="utf-8"))
        run["failures"] = [{"sample_id": "1"}]
        write_json(path, run)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "terminal failures"):
            self.report()

    def test_rejects_an_incomplete_tau_termination_distribution(self) -> None:
        path = (
            self.root
            / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        )
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
        path = (
            self.root
            / ".runtime/benchmark-runs/tau-voice/tau-mini/report.json"
        )
        report = json.loads(path.read_text(encoding="utf-8"))
        del report["execution_evidence"][0]["runtime_identity"]["components"][
            "gateway"
        ]
        write_json(path, report)
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "runtime components"):
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

    def test_rejects_a_missing_official_llm_judge_score(self) -> None:
        path = self.fixture.paths["fdbv3_judge"]
        write_json(path, Fixture.evaluation(None))
        with self.assertRaisesRegex(REPORT.StudyIncompleteError, "LLM response score"):
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
