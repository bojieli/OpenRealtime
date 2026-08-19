#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("report-full-study-progress.py")
SPEC = importlib.util.spec_from_file_location("report_full_study_progress", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
REPORT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(REPORT)


def write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload) + "\n", encoding="utf-8")


class ProgressTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.previous_queues = REPORT.QUEUE_SPECS
        REPORT.QUEUE_SPECS = ()
        self.addCleanup(setattr, REPORT, "QUEUE_SPECS", self.previous_queues)

        matrix = self.root / "benchmarks/matrix.json"
        write_json(
            matrix,
            {
                "matrix_id": "mini",
                "benchmark": {
                    "seed": 7,
                    "num_trials": 1,
                    "domains": [{"name": "airline", "tasks": 2}],
                },
                "cells": [{"id": "control"}],
            },
        )
        pilot = self.root / "benchmarks/tau-voice/matrix-v1.json"
        write_json(
            pilot,
            {
                "matrix_id": "pilot",
                "benchmark": {
                    "seed": 7,
                    "domains": [{"name": "airline", "tasks": 1}],
                },
            },
        )
        simulation = (
            self.root
            / ".runtime/tau2-bench/data/simulations/mini-control-airline-seed7/simulations/one.json"
        )
        write_json(simulation, {})
        pilot_simulation = (
            self.root
            / ".runtime/tau2-bench/data/simulations/pilot-i1-qg-control-airline-seed7/simulations/one.json"
        )
        write_json(pilot_simulation, {})

        external = {}
        for name, population in (("fdb15", 2), ("fdbv3", 3), ("fd", 4)):
            run = self.root / f".runtime/{name}/run.json"
            context = self.root / f".runtime/{name}/context.json"
            write_json(run, {"completed": list(range(population)), "failures": []})
            write_json(context, {"status": "complete"})
            external[name] = {
                "population": population,
                "run_manifest": str(run.relative_to(self.root)),
                "run_context": str(context.relative_to(self.root)),
            }
        self.manifest = self.root / "benchmarks/study.json"
        write_json(
            self.manifest,
            {
                "study_id": "study",
                "tau_voice": {"matrices": [{"path": "benchmarks/matrix.json"}]},
                "full_duplex_bench_v1_5": external["fdb15"],
                "full_duplex_bench_v3": external["fdbv3"],
                "fd_bench": external["fd"],
            },
        )

    def report(self) -> dict:
        return REPORT.build_progress(self.root, self.manifest)

    def test_reports_exact_declared_population_without_scoring_it(self) -> None:
        report = self.report()
        self.assertEqual(report["status"], "running")
        self.assertEqual(report["pilot"]["observed"], 1)
        self.assertEqual(report["pilot"]["expected"], 1)
        self.assertEqual(report["tau_voice"]["observed"], 1)
        self.assertEqual(report["tau_voice"]["expected"], 2)
        self.assertEqual(report["external"]["full_duplex_bench_v1_5"]["observed"], 2)
        self.assertFalse(report["publication_complete"])

    def test_surfaces_terminal_failures_without_changing_counts(self) -> None:
        path = self.root / ".runtime/fdbv3/run.json"
        run = json.loads(path.read_text(encoding="utf-8"))
        run["failures"] = [{"id": "failed"}]
        write_json(path, run)
        report = self.report()
        self.assertEqual(report["status"], "attention")
        self.assertIn("terminal failures", report["warnings"][0])
        self.assertEqual(report["external"]["full_duplex_bench_v3"]["observed"], 3)

    def test_malformed_run_manifest_is_not_reported_as_no_progress(self) -> None:
        path = self.root / ".runtime/fdbv3/run.json"
        run = json.loads(path.read_text(encoding="utf-8"))
        run["completed"] = {"one": True}
        write_json(path, run)
        report = self.report()
        self.assertEqual(report["status"], "attention")
        self.assertEqual(report["external"]["full_duplex_bench_v3"]["observed"], 0)
        self.assertTrue(
            any("records no completed list" in warning for warning in report["warnings"]),
            report["warnings"],
        )

    def test_malformed_failure_ledger_is_not_reported_as_no_failures(self) -> None:
        path = self.root / ".runtime/fd/run.json"
        run = json.loads(path.read_text(encoding="utf-8"))
        run["failures"] = "none"
        write_json(path, run)
        report = self.report()
        self.assertEqual(report["status"], "attention")
        self.assertTrue(
            any("records no failures list" in warning for warning in report["warnings"]),
            report["warnings"],
        )

    def test_a_run_that_has_not_started_raises_no_malformed_warning(self) -> None:
        (self.root / ".runtime/fdb15/run.json").unlink()
        report = self.report()
        self.assertEqual(report["external"]["full_duplex_bench_v1_5"]["observed"], 0)
        self.assertEqual(report["warnings"], [])

    def queue_report(self, state: str | None, complete: bool = False) -> dict:
        """Build a report for one queue whose supervisor is in the given state."""
        directory = self.root / ".runtime/benchmark-runs/queue-v1"
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "queue.pid").write_text("4242\n", encoding="utf-8")
        (directory / "queue.log").write_text(
            "done\n" if complete else "working\n", encoding="utf-8"
        )
        REPORT.QUEUE_SPECS = (("primary", "queue-v1", "queue.sh", "done"),)
        original_alive = REPORT.process_alive
        original_state = REPORT.process_state
        original_matches = REPORT.process_matches
        REPORT.process_alive = lambda pid: state is not None
        REPORT.process_state = lambda pid: state
        REPORT.process_matches = lambda pid, script: state is not None
        self.addCleanup(setattr, REPORT, "process_alive", original_alive)
        self.addCleanup(setattr, REPORT, "process_state", original_state)
        self.addCleanup(setattr, REPORT, "process_matches", original_matches)
        return self.report()

    def test_stopped_supervisor_is_not_reported_as_progressing(self) -> None:
        report = self.queue_report("T")
        queue = report["queues"][0]
        self.assertTrue(queue["alive"])
        self.assertTrue(queue["identity_matches"])
        self.assertFalse(queue["progressing"])
        self.assertEqual(report["status"], "attention")
        self.assertIn("is stopped", report["warnings"][0])
        self.assertIn("4242", report["warnings"][0])

    def test_zombie_supervisor_is_not_reported_as_progressing(self) -> None:
        report = self.queue_report("Z")
        self.assertFalse(report["queues"][0]["progressing"])
        self.assertIn("unreaped zombie", report["warnings"][0])

    def test_sleeping_supervisor_raises_no_warning(self) -> None:
        report = self.queue_report("S")
        self.assertTrue(report["queues"][0]["progressing"])
        self.assertEqual(report["status"], "running")
        self.assertEqual(report["warnings"], [])

    def test_completed_queue_needs_no_live_supervisor(self) -> None:
        report = self.queue_report(None, complete=True)
        queue = report["queues"][0]
        self.assertTrue(queue["complete"])
        self.assertFalse(queue["progressing"])
        self.assertEqual(report["warnings"], [])

    def test_only_authoritative_publication_report_completes_monitor(self) -> None:
        write_json(
            self.root / ".runtime/benchmark-runs/full-study-v1/report.json",
            {"status": "complete"},
        )
        report = self.report()
        self.assertEqual(report["status"], "complete")
        self.assertTrue(report["publication_complete"])


if __name__ == "__main__":
    unittest.main()
