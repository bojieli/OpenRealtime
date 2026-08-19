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
        self.pristine_matrix = self.matrix.read_text(encoding="utf-8")

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

    def load_matrix(self) -> dict:
        """Re-parse the pristine matrix so subtests never inherit each other's mutation."""
        return json.loads(self.pristine_matrix)

    def test_rejects_a_matrix_that_declares_no_population(self) -> None:
        for description, mutate in (
            ("no cells", lambda m: m.__setitem__("cells", [])),
            ("no domains", lambda m: m["benchmark"].__setitem__("domains", [])),
        ):
            with self.subTest(description):
                matrix = self.load_matrix()
                mutate(matrix)
                write_json(self.matrix, matrix)

                completed = self.run_archive()

                self.assertNotEqual(completed.returncode, 0, completed.stdout)
                self.assertIn("population of cells x domains", completed.stderr)
                self.assertTrue((self.experiment / "artifacts").is_dir())
                self.assertFalse((self.experiment / "raw-artifacts.tar.zst").exists())

    def test_rejects_a_matrix_that_leaves_its_retry_policy_undeclared(self) -> None:
        for field in ("maximum_attempts", "retry_delay_seconds"):
            with self.subTest(field):
                matrix = self.load_matrix()
                del matrix["reporting"]["infrastructure_retry_policy"][field]
                write_json(self.matrix, matrix)

                completed = self.run_archive()

                self.assertNotEqual(completed.returncode, 0, completed.stdout)
                self.assertIn(
                    f"declares no reporting.infrastructure_retry_policy.{field}",
                    completed.stderr,
                )
                self.assertFalse((self.experiment / "raw-artifacts.tar.zst").exists())

    def test_rejects_a_domain_that_declares_no_task_population(self) -> None:
        for description, tasks in (("absent", None), ("zero", 0)):
            with self.subTest(description):
                matrix = self.load_matrix()
                if tasks is None:
                    del matrix["benchmark"]["domains"][0]["tasks"]
                else:
                    matrix["benchmark"]["domains"][0]["tasks"] = tasks
                write_json(self.matrix, matrix)

                completed = self.run_archive()

                self.assertNotEqual(completed.returncode, 0, completed.stdout)
                self.assertIn("domain airline task count", completed.stderr)
                self.assertFalse((self.experiment / "raw-artifacts.tar.zst").exists())

    def test_rejects_a_matrix_that_leaves_its_identity_undeclared(self) -> None:
        for field, path in (
            ("matrix_id", ("matrix_id",)),
            ("benchmark.seed", ("benchmark", "seed")),
            ("benchmark.num_trials", ("benchmark", "num_trials")),
        ):
            with self.subTest(field):
                matrix = self.load_matrix()
                node = matrix
                for key in path[:-1]:
                    node = node[key]
                del node[path[-1]]
                write_json(self.matrix, matrix)

                completed = self.run_archive()

                self.assertNotEqual(completed.returncode, 0, completed.stdout)
                self.assertIn(f"declares no {field}", completed.stderr)
                self.assertFalse((self.experiment / "raw-artifacts.tar.zst").exists())


if __name__ == "__main__":
    unittest.main()
