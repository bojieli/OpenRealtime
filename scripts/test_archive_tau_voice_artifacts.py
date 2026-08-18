#!/usr/bin/env python3

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name("archive-tau-voice-artifacts.sh")


def write_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


class AttemptProofArchiveTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.tau2 = self.root / "tau2"
        self.matrix = self.root / "matrix.json"
        self.experiment = (
            self.tau2 / "data/simulations/tau-attempt-test-control-airline-seed300"
        )
        write_json(
            self.matrix,
            {
                "matrix_id": "tau-attempt-test",
                "benchmark": {
                    "seed": 300,
                    "num_trials": 1,
                    "domains": [{"name": "airline", "tasks": 1}],
                },
                "cells": [{"id": "control"}],
                "reporting": {
                    "infrastructure_retry_policy": {
                        "maximum_attempts": 4,
                        "retry_delay_seconds": 1,
                    }
                },
            },
        )
        write_json(
            self.experiment / "results.json",
            {"simulation_index": [{"id": "used-final"}]},
        )
        write_json(
            self.experiment / "simulations/used-final.json", {"id": "used-final"}
        )
        write_json(
            self.experiment / "artifacts/task_0/sim_failed-first/sim_status.json",
            {
                "status": "failed",
                "reason": "infrastructure_error",
                "error": "connection closed",
                "error_type": "RuntimeError",
            },
        )
        write_json(
            self.experiment / "artifacts/task_0/sim_used-final/sim_status.json",
            {"status": "used"},
        )
        (self.experiment / "artifacts/task_0/sim_failed-first/task.log").write_text(
            "preserved failure\n", encoding="utf-8"
        )

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def run_archive(self) -> subprocess.CompletedProcess[str]:
        environment = os.environ.copy()
        environment["TAU2_DIR"] = str(self.tau2)
        return subprocess.run(
            [str(SCRIPT), str(self.matrix)],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=environment,
        )

    def test_archives_successful_retry_and_records_attempt_ledger(self) -> None:
        completed = self.run_archive()

        self.assertEqual(completed.returncode, 0, completed.stderr)
        evidence = json.loads(
            (self.experiment / "raw-artifacts-archive.json").read_text(encoding="utf-8")
        )
        self.assertEqual(
            evidence["attempts"],
            {
                "tasks": 1,
                "total": 2,
                "successful": 1,
                "failed_infrastructure": 1,
                "retried_tasks": 1,
                "maximum_observed": 2,
                "maximum_allowed": 4,
                "retry_delay_seconds": 1,
                "seed_reused": True,
                "retry_scope": "exceptions_only",
                "semantic_outcomes_retried": False,
            },
        )
        self.assertFalse((self.experiment / "artifacts").exists())
        listing = subprocess.run(
            ["tar", "--zstd", "-tf", str(self.experiment / "raw-artifacts.tar.zst")],
            check=True,
            text=True,
            stdout=subprocess.PIPE,
        ).stdout
        self.assertIn("sim_failed-first/task.log", listing)

    def test_rejects_non_infrastructure_failed_attempt(self) -> None:
        write_json(
            self.experiment / "artifacts/task_0/sim_failed-first/sim_status.json",
            {
                "status": "failed",
                "reason": "semantic_failure",
                "error": "low reward",
                "error_type": "AssertionError",
            },
        )

        completed = self.run_archive()

        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("not typed infrastructure evidence", completed.stderr)
        self.assertTrue((self.experiment / "artifacts").is_dir())
        self.assertFalse((self.experiment / "raw-artifacts.tar.zst").exists())


if __name__ == "__main__":
    unittest.main()
