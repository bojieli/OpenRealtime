#!/usr/bin/env python3
"""Tests for the paired task-level inference tool."""

from __future__ import annotations

import importlib.util
import json
import math
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


def write_population(
    root: Path,
    name: str,
    rewards: dict[str, object],
    tasks: int | None = None,
    trials: int | None = None,
) -> Path:
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
    results: dict = {"simulation_index": index}
    if tasks is not None:
        results["tasks"] = [{"id": str(number)} for number in range(tasks)]
    if trials is not None:
        results["info"] = {"num_trials": trials}
    (directory / "results.json").write_text(json.dumps(results))
    for entry in index:
        (directory / "simulations" / f"{entry['id']}.json").write_text("{}")
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
    def test_refuses_unpaired_tasks_instead_of_selecting_the_intersection(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            baseline = write_population(
                root, "base", {"a": 1.0, "b": 0.0, "c": 1.0, "extra": 1.0}
            )
            treatment = write_population(root, "treat", {"a": 1.0, "b": 1.0, "c": 1.0})
            code, _, stderr = run(baseline, treatment)
            self.assertEqual(code, 1)
            self.assertIn("same scored task set", stderr)

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

    def test_null_reward_is_not_scored_as_zero_or_silently_unpaired(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            code, report, stderr = run(
                write_population(root, "base", {"a": 1.0, "b": None}),
                write_population(root, "treat", {"a": 1.0, "b": 1.0}),
            )
            self.assertEqual(code, 1)
            self.assertEqual(report, {})
            self.assertIn("same scored task set", stderr)

    def test_non_binary_reward_is_refused_for_exact_mcnemar_inference(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            code, _, stderr = run(
                write_population(root, "base", {"a": 0.5}),
                write_population(root, "treat", {"a": 1.0}),
            )
            self.assertEqual(code, 1)
            self.assertIn("non-binary reward", stderr)

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
            (baseline / "simulations" / "sim1.json").write_text("{}")
            code, _, stderr = run(baseline, write_population(root, "treat", {"a": 1.0}))
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
            code, _, stderr = run(baseline, write_population(root, "treat", {"a": 1.0}))
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
            comparability = report["comparison"]["comparability_scope"]
            self.assertIn("must separately establish", comparability)

    def test_each_side_reports_its_own_completeness(self):
        """A difference over a growing cell must not read as a finished one."""
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            baseline = write_population(
                root, "base", {"a": 0.0, "b": 1.0}, tasks=2, trials=1
            )
            treatment = write_population(
                root, "treat", {"a": 1.0, "b": 1.0}, tasks=50, trials=1
            )
            code, payload, stderr = run(baseline, treatment)
            self.assertEqual(code, 0, stderr)
            self.assertTrue(payload["baseline"]["scope"]["complete"])
            treated = payload["treatment"]["scope"]
            self.assertFalse(treated["complete"])
            self.assertEqual(treated["declared_simulations"], 50)
            self.assertEqual(treated["simulations_indexed"], 2)
            self.assertEqual(treated["simulations_on_disk"], 2)

    def test_population_without_declared_scope_claims_no_completeness(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            baseline = write_population(root, "base", {"a": 0.0})
            treatment = write_population(root, "treat", {"a": 1.0})
            code, payload, stderr = run(baseline, treatment)
            self.assertEqual(code, 0, stderr)
            for side in ("baseline", "treatment"):
                scope = payload[side]["scope"]
                self.assertFalse(scope["declared"])
                self.assertNotIn("complete", scope)

    def test_index_and_simulation_files_must_match_in_both_directions(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            treatment = write_population(root, "treat", {"a": 1.0})

            baseline = write_population(root, "missing", {"a": 1.0})
            (baseline / "simulations" / "sim0.json").unlink()
            code, _, stderr = run(baseline, treatment)
            self.assertEqual(code, 1)
            self.assertIn("missing simulation", stderr)

            baseline = write_population(root, "unindexed", {"a": 1.0})
            (baseline / "simulations" / "orphan.json").write_text("{}")
            code, _, stderr = run(baseline, treatment)
            self.assertEqual(code, 1)
            self.assertIn("unindexed simulation", stderr)

    def test_duplicate_simulation_id_is_refused(self):
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            baseline = write_population(root, "base", {"a": 1.0})
            payload = json.loads((baseline / "results.json").read_text())
            duplicate = dict(payload["simulation_index"][0])
            duplicate["task_id"] = "b"
            payload["simulation_index"].append(duplicate)
            (baseline / "results.json").write_text(json.dumps(payload))
            code, _, stderr = run(
                baseline, write_population(root, "treat", {"a": 1.0, "b": 1.0})
            )
            self.assertEqual(code, 1)
            self.assertIn("duplicate simulation id", stderr)

    def test_sign_test_is_exact_mcnemar_on_binary_rewards(self):
        """The discordant pairs form a 2x2 off-diagonal; the test is McNemar's.

        Checked against an independent formulation over every (b, c) up to 25
        rather than a handful of tabulated values, so an off-by-one in the tail
        cannot survive by matching at the points that were written down.
        """

        def mcnemar_exact(better: int, worse: int) -> float:
            total = better + worse
            tail = sum(math.comb(total, k) for k in range(0, min(better, worse) + 1))
            return min(1.0, 2 * tail / 2**total)

        module = load_module()
        for better in range(26):
            for worse in range(26):
                if better + worse == 0:
                    continue
                with self.subTest(better=better, worse=worse):
                    self.assertAlmostEqual(
                        module.binomial_two_sided_p(better, better + worse),
                        mcnemar_exact(better, worse),
                        places=12,
                    )


if __name__ == "__main__":
    unittest.main()
