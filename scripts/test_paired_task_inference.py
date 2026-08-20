#!/usr/bin/env python3
"""Tests for the paired task-level inference tool."""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

SCRIPT = Path(__file__).resolve().parent / "paired-task-inference.py"


def load_module():
    spec = importlib.util.spec_from_file_location("paired_task_inference", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def write_population(root: Path, name: str, rewards: dict[str, object]) -> Path:
    directory = root / name
    (directory / "simulations").mkdir(parents=True)
    index = [
        {
            "id": f"sim{position}",
            "task_id": task,
            "trial": 0,
            "reward": reward,
            "termination_reason": "user_stop",
        }
        for position, (task, reward) in enumerate(rewards.items())
    ]
    (directory / "results.json").write_text(json.dumps({"simulation_index": index}))
    return directory


def run(baseline: Path, treatment: Path) -> tuple[int, dict, str]:
    finished = subprocess.run(
        [
            sys.executable,
            str(SCRIPT),
            "--baseline",
            str(baseline),
            "--treatment",
            str(treatment),
        ],
        capture_output=True,
        text=True,
    )
    payload = json.loads(finished.stdout) if finished.returncode == 0 else {}
    return finished.returncode, payload, finished.stderr


class SignTestTest(unittest.TestCase):
    def test_matches_hand_computed_exact_values(self):
        module = load_module()
        for successes, trials, expected in [
            (0, 1, 1.0),
            (0, 5, 0.0625),
            (5, 5, 0.0625),
            (1, 10, 0.021484375),
            (2, 10, 0.109375),
            (5, 10, 1.0),
            (0, 10, 0.001953125),
        ]:
            with self.subTest(successes=successes, trials=trials):
                self.assertAlmostEqual(
                    module.binomial_two_sided_p(successes, trials), expected, places=12
                )

    def test_is_symmetric(self):
        module = load_module()
        for successes in range(21):
            self.assertAlmostEqual(
                module.binomial_two_sided_p(successes, 20),
                module.binomial_two_sided_p(20 - successes, 20),
                places=15,
            )

    def test_zero_pairs_is_refused(self):
        module = load_module()
        with self.assertRaises(module.PairedInferenceError):
            module.binomial_two_sided_p(0, 0)


class BootstrapTest(unittest.TestCase):
    def test_clear_effect_excludes_zero_and_null_includes_it(self):
        module = load_module()
        positive = module.paired_bootstrap([1.0] * 40 + [0.0] * 10, 4000, 7)
        self.assertGreater(positive["ci95_low"], 0.0)
        null = module.paired_bootstrap([1.0] * 25 + [-1.0] * 25, 4000, 7)
        self.assertLessEqual(null["ci95_low"], 0.0)
        self.assertGreaterEqual(null["ci95_high"], 0.0)

    def test_is_reproducible_from_the_same_seed(self):
        module = load_module()
        sample = [1.0, 0.0, 1.0, 0.5, 0.0, 1.0, 0.0]
        first = module.paired_bootstrap(sample, 500, 99)
        second = module.paired_bootstrap(sample, 500, 99)
        self.assertEqual(first, second)


class ComparisonTest(unittest.TestCase):
    def test_pairs_by_task_and_reports_unpaired_tasks(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            baseline = write_population(
                root, "base", {"a": 1.0, "b": 0.0, "c": 1.0, "extra": 1.0}
            )
            treatment = write_population(root, "treat", {"a": 1.0, "b": 1.0, "c": 1.0})
            code, report, stderr = run(baseline, treatment)
            self.assertEqual(code, 0, stderr)
            comparison = report["comparison"]
            self.assertEqual(comparison["paired_tasks"], 3)
            self.assertEqual(comparison["baseline_only_tasks"], ["extra"])
            self.assertEqual(comparison["treatment_only_tasks"], [])
            # The unpaired task must not move the paired baseline mean.
            self.assertAlmostEqual(comparison["baseline_mean"], 2 / 3)

    def test_all_tied_reports_no_p_value_rather_than_one(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            rewards = {"a": 1.0, "b": 0.0, "c": 1.0}
            code, report, stderr = run(
                write_population(root, "base", rewards),
                write_population(root, "treat", dict(rewards)),
            )
            self.assertEqual(code, 0, stderr)
            sign_test = report["comparison"]["sign_test"]
            self.assertIsNone(sign_test["p_value"])
            self.assertEqual(sign_test["discordant_pairs"], 0)
            self.assertIn("no evidence", sign_test["note"])

    def test_null_reward_is_dropped_not_scored_as_zero(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            code, report, stderr = run(
                write_population(root, "base", {"a": 1.0, "b": None}),
                write_population(root, "treat", {"a": 1.0, "b": 1.0}),
            )
            self.assertEqual(code, 0, stderr)
            comparison = report["comparison"]
            self.assertEqual(comparison["paired_tasks"], 1)
            self.assertAlmostEqual(comparison["mean_difference"], 0.0)

    def test_repeated_trials_are_refused_not_silently_dropped(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            baseline = write_population(root, "base", {"a": 1.0})
            results = json.loads((baseline / "results.json").read_text())
            results["simulation_index"].append(
                {
                    "id": "sim1",
                    "task_id": "a",
                    "trial": 1,
                    "reward": 0.0,
                    "termination_reason": "user_stop",
                }
            )
            (baseline / "results.json").write_text(json.dumps(results))
            code, _, stderr = run(
                baseline, write_population(root, "treat", {"a": 1.0})
            )
            self.assertEqual(code, 1)
            self.assertIn("repeated-trial estimator", stderr)

    def test_disjoint_task_sets_are_refused(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            code, _, stderr = run(
                write_population(root, "base", {"a": 1.0}),
                write_population(root, "treat", {"b": 1.0}),
            )
            self.assertEqual(code, 1)
            self.assertIn("share no task", stderr)

    def test_empty_results_file_is_refused(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            baseline = write_population(root, "base", {"a": 1.0})
            (baseline / "results.json").write_text("")
            code, _, stderr = run(
                baseline, write_population(root, "treat", {"a": 1.0})
            )
            self.assertEqual(code, 1)
            self.assertIn("empty", stderr)

    def test_unscored_population_is_refused(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            code, _, stderr = run(
                write_population(root, "base", {"a": None}),
                write_population(root, "treat", {"a": 1.0}),
            )
            self.assertEqual(code, 1)
            self.assertIn("no task scored", stderr)

    def test_inference_scope_is_stated_in_the_report(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            code, report, stderr = run(
                write_population(root, "base", {"a": 0.0, "b": 0.0}),
                write_population(root, "treat", {"a": 1.0, "b": 1.0}),
            )
            self.assertEqual(code, 0, stderr)
            scope = report["comparison"]["inference_scope"]
            self.assertIn("does not bound run-to-run variation", scope)


if __name__ == "__main__":
    unittest.main()
