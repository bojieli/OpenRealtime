#!/usr/bin/env python3
"""Tests for the complete-study Pareto and evidence analysis."""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("report-full-study-analysis.py")
SPEC = importlib.util.spec_from_file_location("report_full_study_analysis", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
ANALYSIS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ANALYSIS)


def write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


class Fixture:
    def __init__(self, root: Path) -> None:
        self.root = root
        self.full_report = root / ".runtime/full-study/report.json"
        self.definition = root / "benchmarks/tau/ablation.json"
        self.reporter_script = root / "scripts/report-full-study.py"
        self.reporter_script.parent.mkdir(parents=True, exist_ok=True)
        self.reporter_script.write_text("# frozen reporter fixture\n", encoding="utf-8")
        self.matrix_panels = [
            self.matrix("baseline", reward=0.5, latency=0.6, cost=2.0, tokens=200),
            self.matrix("candidate", reward=0.75, latency=0.4, cost=1.5, tokens=150),
        ]
        write_json(
            self.definition,
            {
                "ablation_id": "paired-test",
                "baseline": {"matrix": "benchmarks/tau/matrix-baseline.json"},
                "candidate": {"matrix": "benchmarks/tau/matrix-candidate.json"},
            },
        )
        write_json(
            self.full_report,
            {
                "schema_version": "1.0.0",
                "status": "complete",
                "study": {
                    "id": "test-study",
                    "orchestration_revision": "c" * 40,
                    "source_worktree_clean": True,
                    "terminal_reporter": {
                        "source_revision": "c" * 40,
                        "source_worktree_clean": True,
                        "script": self.pin(self.reporter_script),
                        "mode": "frozen-orchestration",
                        "correction_scope": "none",
                    },
                    "publication_policy": {
                        "partial_results": "forbidden",
                        "cross_benchmark_composite": "forbidden",
                    },
                },
                "evidence_panel": {"tau_voice": {"matrices": self.matrix_panels}},
            },
        )

    def pin(self, path: Path) -> dict:
        return {
            "path": str(path.relative_to(self.root)),
            "sha256": ANALYSIS.sha256_file(path),
            "bytes": path.stat().st_size,
        }

    @staticmethod
    def runtime(tokens: int) -> dict:
        def continuation(invocations: int, total: int) -> dict:
            return {
                "invocations": invocations,
                "completed": invocations,
                "failed": 0,
                "cancelled": 0,
                "events": invocations,
                "first_event_count": invocations,
                "cumulative_first_event_ns": invocations * 10,
                "cumulative_elapsed_ns": invocations * 20,
                "input_tokens": total - 10 if invocations else 0,
                "output_tokens": 10 if invocations else 0,
                "reasoning_tokens": 0,
                "total_tokens": total if invocations else 0,
            }

        return {
            "sessions_started": 2,
            "sessions_completed": 2,
            "asr_input_frames": 10,
            "asr_provider_advances": 8,
            "asr_provider_failures": 0,
            "asr_provider_elapsed_ns": 80,
            "asr_finalize_attempts": 2,
            "asr_finalize_failures": 0,
            "asr_finalize_elapsed_ns": 20,
            "asr_finalizations": 2,
            "fast": continuation(2, tokens // 2),
            "slow": continuation(2, tokens - tokens // 2),
            "fast_preparation": continuation(0, 0),
            "slow_preparation": continuation(0, 0),
            "speech": {
                "invocations": 2,
                "completed": 2,
                "failed": 0,
                "cancelled": 0,
                "chunks": 4,
                "samples": 9600,
                "first_chunk_count": 2,
                "cumulative_first_chunk_ns": 40,
                "cumulative_elapsed_ns": 100,
            },
        }

    @staticmethod
    def zero_runtime(value):
        if isinstance(value, dict):
            return {key: Fixture.zero_runtime(item) for key, item in value.items()}
        if type(value) in (int, float):
            return 0
        raise TypeError(f"cannot zero runtime value {value!r}")

    @staticmethod
    def runtime_identity() -> dict:
        return {
            "host_boot_id": "fixture-boot",
            "components": {
                "gateway": {
                    "pid": 10,
                    "proc_start_time_ticks": "100",
                    "executable_sha256": "a" * 64,
                    "command_sha256": "b" * 64,
                },
                "asr": {
                    "pid": 20,
                    "proc_start_time_ticks": "200",
                    "executable_sha256": "c" * 64,
                    "command_sha256": "d" * 64,
                },
            },
            "gpu_ownership": {"gpu_uuids": ["GPU-fixture"]},
        }

    def matrix(
        self, name: str, *, reward: float, latency: float, cost: float, tokens: int
    ) -> dict:
        matrix_id = f"matrix-{name}"
        matrix_path = self.root / f"benchmarks/tau/{matrix_id}.json"
        cells = [
            {"id": f"{name}-control", "speech_complexity": "control"},
            {"id": f"{name}-regular", "speech_complexity": "regular"},
        ]
        write_json(
            matrix_path,
            {
                "matrix_id": matrix_id,
                "benchmark": {
                    "seed": 300,
                    "num_trials": 1,
                    "domains": [{"name": "airline", "tasks": 1}],
                },
                "cells": cells,
            },
        )
        report_path = self.root / f".runtime/tau/{matrix_id}/report.json"
        overall = self.overall(reward=reward, latency=latency, cost=cost)
        report_cells = {}
        terminal_cells = {}
        for cell in cells:
            cell_id = cell["id"]
            directory = (
                self.root
                / ".runtime/tau2-bench/data/simulations"
                / f"{matrix_id}-{cell_id}-airline-seed300"
            )
            simulation = {
                "duration": 1.0,
                "review": {"errors": []},
                "ticks": [
                    self.tick(user="hello"),
                    self.tick(),
                    self.tick(agent="hi"),
                ],
            }
            write_json(directory / "simulations/sim.json", simulation)
            write_json(
                directory / "results.json",
                {
                    "tasks": [{"id": "1"}],
                    "info": {"num_trials": 1},
                    "simulation_index": [
                        {
                            "id": "sim",
                            "task_id": "1",
                            "trial": 0,
                            "reward": reward,
                            "termination_reason": "user_stop",
                        }
                    ],
                },
            )
            report_cells[cell_id] = {
                "domains": {
                    "airline": {
                        "population": {
                            "status": "complete",
                            "tasks": 1,
                            "trials_per_task": 1,
                            "simulations": 1,
                            "termination_reasons": {"user_stop": 1},
                            "infrastructure_errors": 0,
                        },
                        "evidence": ANALYSIS.population_evidence(directory),
                    }
                },
                "overall": overall,
            }
            terminal_cells[cell_id] = {
                "simulations": 1,
                "infrastructure_errors": 0,
                "termination_reasons": {"user_stop": 1},
                "overall": ANALYSIS.published_tau_overall(overall, cell_id),
            }

        gpu_path = report_path.parent / "gpu.csv"
        gpu_path.parent.mkdir(parents=True, exist_ok=True)
        gpu_path.write_text(
            "timestamp,index,name,memory_used_mib,utilization_gpu_percent,power_draw_watts,loadavg\n"
            "2026-01-01T00:00:00Z,0,Test GPU,1000,50,100,1/2/3\n",
            encoding="utf-8",
        )
        runtime = self.runtime(tokens)
        runtime_start = self.zero_runtime(runtime)
        identity = self.runtime_identity()
        run_path = report_path.parent / "invocations/all-cells/attempt/run.json"
        write_json(
            run_path,
            {
                "status": "complete",
                "selected_cell": None,
                "started_at": "2026-01-01T00:00:00Z",
                "completed_at": "2026-01-01T00:00:02Z",
                "openrealtime_revision": "c" * 40,
                "openrealtime_revision_final": "c" * 40,
                "source_worktree_clean_start": True,
                "source_worktree_clean_final": True,
                "runtime_identity": identity,
                "runtime_identity_final": identity,
                "gateway_health": {"runtime": runtime_start},
                "gateway_health_final": {"runtime": runtime},
                "gpu_ownership_guards": [],
            },
        )
        execution = {
            "path": str(run_path.relative_to(self.root)),
            "sha256": ANALYSIS.sha256_file(run_path),
            "status": "complete",
            "selected_cell": None,
            "started_at": "2026-01-01T00:00:00Z",
            "completed_at": "2026-01-01T00:00:02Z",
            "openrealtime_revision": "c" * 40,
            "openrealtime_revision_final": "c" * 40,
            "source_worktree_clean_start": True,
            "source_worktree_clean_final": True,
            "runtime_identity": identity,
            "runtime_identity_final": identity,
            "gateway_health_start": {"runtime": runtime_start},
            "gateway_health_final": {"runtime": runtime},
            "gateway_runtime_delta": runtime,
            "gpu_ownership_guards": [],
            "gpu_telemetry": self.pin(gpu_path),
        }
        execution_history = {
            "invocations": 1,
            "resumed": False,
            "interrupted_invocations": 0,
            "completed_invocations": 1,
            "source_revision": "c" * 40,
            "host_boot_id": "fixture-boot",
            "gpu_uuids": ["GPU-fixture"],
            "unregistered_gpu_process_violations": 0,
            "segments": [
                {
                    "run": self.pin(run_path),
                    "recorded_status": "complete",
                    "interpreted_status": "complete",
                    "started_at": "2026-01-01T00:00:00Z",
                    "completed_at": "2026-01-01T00:00:02Z",
                    "completed_guard_summaries": 0,
                    "unrecorded_terminal_guard_logs": [],
                    "gpu_telemetry": self.pin(gpu_path),
                }
            ],
        }
        write_json(
            report_path,
            {
                "status": "complete",
                "matrix": {
                    "id": matrix_id,
                    "sha256": ANALYSIS.sha256_file(matrix_path),
                    "selected_cells": [cell["id"] for cell in cells],
                },
                "execution_evidence": [execution],
                "cells": report_cells,
            },
        )
        return {
            "matrix_id": matrix_id,
            "matrix": self.pin(matrix_path),
            "report": self.pin(report_path),
            "population": 2,
            "infrastructure_errors": 0,
            "execution_history": execution_history,
            "cells": terminal_cells,
        }

    @staticmethod
    def tick(user: str | None = None, agent: str | None = None) -> dict:
        return {
            "tick_duration_seconds": 0.2,
            "user_chunk": {"content": user} if user else None,
            "agent_chunk": {"content": agent} if agent else None,
            "agent_tool_calls": [],
            "agent_tool_results": [],
        }

    @staticmethod
    def overall(*, reward: float, latency: float, cost: float) -> dict:
        return {
            "agent_metrics": {
                "avg_reward": reward,
                "pass_hat_ks": {"1": reward},
                "avg_agent_cost": cost,
                "total_read_actions": 2,
                "correct_read_actions": 2,
                "total_write_actions": 0,
                "correct_write_actions": 0,
                "sims_by_max_agent_severity": {"none": 1},
                "agent_error_tags_by_severity": {},
            },
            "interaction_metrics": {
                "response_latency_mean": latency,
                "yield_latency_mean": 0.2,
                "response_rate": 1.0,
                "yield_rate": 1.0,
                "agent_interruption_rate": 0.0,
                "selectivity_backchannel": 1.0,
                "selectivity_vocal_tic": 1.0,
                "selectivity_non_directed": 1.0,
                "counts": {},
            },
        }


class FullStudyAnalysisTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.fixture = Fixture(self.root)

    def build(self) -> dict:
        return ANALYSIS.build_analysis(
            self.root,
            self.fixture.full_report,
            [self.fixture.definition],
            exhaustive=True,
        )

    def resume_first_matrix(self) -> tuple[Path, Path]:
        full = json.loads(self.fixture.full_report.read_text(encoding="utf-8"))
        panel = full["evidence_panel"]["tau_voice"]["matrices"][0]
        report_path = self.root / panel["report"]["path"]
        report = json.loads(report_path.read_text(encoding="utf-8"))
        final_entry = report["execution_evidence"][0]
        final_entry["gateway_runtime_delta"] = self.fixture.runtime(100)
        earlier_run = (
            report_path.parent / "invocations/all-cells/earlier/run.json"
        )
        earlier_gpu = earlier_run.with_name("gpu.csv")
        earlier_gpu.parent.mkdir(parents=True, exist_ok=True)
        earlier_gpu.write_text(
            "timestamp,index,name,memory_used_mib,utilization_gpu_percent,"
            "power_draw_watts,loadavg\n"
            "2025-12-31T23:59:00Z,0,Test GPU,900,40,90,1/2/3\n",
            encoding="utf-8",
        )
        identity = self.fixture.runtime_identity()
        runtime_start = self.fixture.zero_runtime(self.fixture.runtime(200))
        earlier_payload = {
            "status": "running",
            "selected_cell": None,
            "started_at": "2025-12-31T23:59:00Z",
            "stopped_at": "2025-12-31T23:59:59Z",
            "openrealtime_revision": "c" * 40,
            "source_worktree_clean_start": True,
            "runtime_identity": identity,
            "gateway_health": {"runtime": runtime_start},
            "gpu_ownership_guards": [],
        }
        write_json(earlier_run, earlier_payload)
        earlier_gpu_pin = self.fixture.pin(earlier_gpu)
        earlier_entry = {
            "path": str(earlier_run.relative_to(self.root)),
            "sha256": ANALYSIS.sha256_file(earlier_run),
            "status": "running",
            "selected_cell": None,
            "started_at": earlier_payload["started_at"],
            "completed_at": None,
            "openrealtime_revision": "c" * 40,
            "openrealtime_revision_final": None,
            "source_worktree_clean_start": True,
            "source_worktree_clean_final": None,
            "runtime_identity": identity,
            "runtime_identity_final": None,
            "gateway_health_start": earlier_payload["gateway_health"],
            "gateway_health_final": None,
            "gpu_ownership_guards": [],
            "gpu_telemetry": earlier_gpu_pin,
        }
        report["execution_evidence"] = [earlier_entry, final_entry]
        write_json(report_path, report)
        history = panel["execution_history"]
        history["invocations"] = 2
        history["resumed"] = True
        history["interrupted_invocations"] = 1
        history["unregistered_gpu_process_violations"] = 1
        history["segments"].insert(
            0,
            {
                "run": self.fixture.pin(earlier_run),
                "recorded_status": "running",
                "interpreted_status": "stopped_before_resume_with_unfinalized_status",
                "started_at": earlier_payload["started_at"],
                "stopped_at": earlier_payload["stopped_at"],
                "completed_guard_summaries": 0,
                "unrecorded_terminal_guard_logs": [
                    {
                        "violations": [
                            {
                                "recorded_at": earlier_payload["stopped_at"],
                                "error": "foreign GPU process",
                            }
                        ]
                    }
                ],
                "gpu_telemetry": earlier_gpu_pin,
            },
        )
        panel["report"] = self.fixture.pin(report_path)
        write_json(self.fixture.full_report, full)
        return report_path, earlier_run

    def test_publishes_complete_native_frontiers_and_limits(self) -> None:
        report = self.build()
        self.assertEqual(report["status"], "complete")
        self.assertEqual(report["tau_voice"]["population"], 4)
        speech = report["tau_voice"]["ablations"]["paired-test"]["speech_populations"][
            "control"
        ]
        self.assertEqual(
            speech["frontiers"]["observed_joint"]["frontier"],
            ["candidate:control"],
        )
        self.assertIn(
            "first_semantic_audio",
            report["interpretation"]["measurement_limits"],
        )
        baseline = speech["points"][0]
        self.assertEqual(
            baseline["metrics"]["cost"]["scope"].startswith("tau compatibility"),
            True,
        )
        self.assertEqual(
            baseline["metrics"]["trajectory_consistency"]["review_coverage_rate"],
            1.0,
        )

    def test_aggregates_resumed_counter_and_gpu_segments(self) -> None:
        self.resume_first_matrix()
        report = self.build()
        matrix = report["tau_voice"]["matrices"]["matrix-baseline"]
        self.assertEqual(
            matrix["runtime"]["continuation_totals"]["total_tokens"], 200
        )
        self.assertTrue(matrix["runtime"]["execution_history"]["resumed"])
        self.assertEqual(len(matrix["gpu_telemetry"]["segments"]), 2)
        self.assertEqual(
            matrix["gpu_telemetry"]["execution_history"][
                "unregistered_gpu_process_violations"
            ],
            1,
        )

    def test_refuses_resumed_execution_source_drift(self) -> None:
        report_path, earlier_run = self.resume_first_matrix()
        run = json.loads(earlier_run.read_text(encoding="utf-8"))
        run["openrealtime_revision"] = "d" * 40
        write_json(earlier_run, run)
        report = json.loads(report_path.read_text(encoding="utf-8"))
        report["execution_evidence"][0]["openrealtime_revision"] = "d" * 40
        report["execution_evidence"][0]["sha256"] = ANALYSIS.sha256_file(
            earlier_run
        )
        write_json(report_path, report)
        full = json.loads(self.fixture.full_report.read_text(encoding="utf-8"))
        panel = full["evidence_panel"]["tau_voice"]["matrices"][0]
        panel["execution_history"]["segments"][0]["run"] = self.fixture.pin(
            earlier_run
        )
        panel["report"] = self.fixture.pin(report_path)
        write_json(self.fixture.full_report, full)
        with self.assertRaisesRegex(
            ANALYSIS.FullStudyAnalysisError, "source revision count"
        ):
            self.build()

    def test_refuses_a_partial_terminal_report(self) -> None:
        payload = json.loads(self.fixture.full_report.read_text(encoding="utf-8"))
        payload["status"] = "running"
        write_json(self.fixture.full_report, payload)
        with self.assertRaisesRegex(
            ANALYSIS.FullStudyAnalysisError, "terminal report status"
        ):
            self.build()

    def test_accepts_a_disclosed_reporting_only_correction(self) -> None:
        payload = json.loads(self.fixture.full_report.read_text(encoding="utf-8"))
        reporter = payload["study"]["terminal_reporter"]
        reporter["source_revision"] = "d" * 40
        reporter["mode"] = "postfreeze-reporting-correction"
        reporter["correction_scope"] = {
            "benchmark_execution_changed": False,
            "native_scores_changed": False,
            "changes": ["represent undefined native metrics by name"],
        }
        write_json(self.fixture.full_report, payload)
        report = self.build()
        self.assertEqual(
            report["source"]["terminal_reporter"]["mode"],
            "postfreeze-reporting-correction",
        )

    def test_refuses_expanded_population_drift(self) -> None:
        path = next(
            self.root.glob(
                ".runtime/tau2-bench/data/simulations/matrix-baseline-*/simulations/sim.json"
            )
        )
        value = json.loads(path.read_text(encoding="utf-8"))
        value["duration"] = 2.0
        write_json(path, value)
        with self.assertRaisesRegex(
            ANALYSIS.FullStudyAnalysisError, "population evidence"
        ):
            self.build()

    def test_does_not_treat_native_none_bucket_as_review_coverage(self) -> None:
        path = next(
            self.root.glob(
                ".runtime/tau2-bench/data/simulations/matrix-baseline-baseline-control-*/simulations/sim.json"
            )
        )
        simulation = json.loads(path.read_text(encoding="utf-8"))
        simulation["review"] = None
        write_json(path, simulation)
        directory = path.parent.parent
        self.assertEqual(
            ANALYSIS.review_coverage(directory, "fixture"),
            {
                "agent_reviewed_simulations": 0,
                "user_only_reviewed_simulations": 0,
                "unreviewed_simulations": 1,
            },
        )

    def test_refuses_terminal_tau_infrastructure_errors(self) -> None:
        payload = json.loads(self.fixture.full_report.read_text(encoding="utf-8"))
        payload["evidence_panel"]["tau_voice"]["matrices"][0][
            "infrastructure_errors"
        ] = 1
        write_json(self.fixture.full_report, payload)
        with self.assertRaisesRegex(
            ANALYSIS.FullStudyAnalysisError, "infrastructure errors"
        ):
            self.build()

    def test_refuses_a_changed_registered_ablation_definition(self) -> None:
        relative = str(self.fixture.definition.relative_to(self.root))
        with self.assertRaisesRegex(
            ANALYSIS.FullStudyAnalysisError, "frozen ablation definition"
        ):
            ANALYSIS.build_analysis(
                self.root,
                self.fixture.full_report,
                [self.fixture.definition],
                exhaustive=True,
                expected_definition_hashes={relative: "0" * 64},
            )

    def test_frontier_refuses_an_axis_missing_from_one_point(self) -> None:
        points = [
            {"point_id": "a", "axes": {"pass_at_1": 1.0}},
            {"point_id": "b", "axes": {}},
        ]
        report = ANALYSIS.frontier(points, ("pass_at_1",))
        self.assertEqual(report["status"], "not_measured")
        self.assertEqual(report["missing_by_point"], {"b": ["pass_at_1"]})

    def test_gpu_summary_refuses_an_empty_sample(self) -> None:
        path = self.root / "empty.csv"
        path.write_text(
            "timestamp,index,name,memory_used_mib,utilization_gpu_percent,power_draw_watts,loadavg\n",
            encoding="utf-8",
        )
        with self.assertRaisesRegex(
            ANALYSIS.FullStudyAnalysisError, "contains no telemetry samples"
        ):
            ANALYSIS.summarize_gpu_telemetry(
                self.root, self.fixture.pin(path), "empty GPU"
            )


if __name__ == "__main__":
    unittest.main()
