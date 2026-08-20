#!/usr/bin/env python3
"""Tests for the tau-Voice response-latency tail analyzer."""

from __future__ import annotations

import json
import subprocess
import sys
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

SCRIPT = Path(__file__).resolve().parent / "analyze-tau-voice-tails.py"


def speech_tick(user=None, agent=None, duration=0.2):
    return {
        "tick_duration_seconds": duration,
        "user_chunk": {"content": user} if user else None,
        "agent_chunk": {"content": agent} if agent else None,
        "agent_tool_calls": [],
        "agent_tool_results": [],
    }


def conversation(gaps: list[int], duration=0.2) -> list[dict]:
    """One tick of user speech, then `gap` silent ticks, then agent speech."""
    ticks = []
    for gap in gaps:
        ticks.append(speech_tick(user="hi", duration=duration))
        ticks.extend(speech_tick(duration=duration) for _ in range(gap - 1))
        ticks.append(speech_tick(agent="hello", duration=duration))
    return ticks


def write_population(
    root: Path,
    name: str,
    simulations: list[dict],
    tasks: int | None = None,
    trials: int | None = None,
) -> Path:
    directory = root / name
    (directory / "simulations").mkdir(parents=True)
    index = []
    for position, simulation in enumerate(simulations):
        identifier = simulation.setdefault("id", f"sim{position}")
        index.append(
            {
                "id": identifier,
                "task_id": str(position),
                "trial": 0,
                "reward": 1.0,
                "termination_reason": simulation.pop("termination_reason", "user_stop"),
            }
        )
        (directory / "simulations" / f"{identifier}.json").write_text(
            json.dumps(simulation)
        )
    results: dict = {"simulation_index": index}
    if tasks is not None:
        results["tasks"] = [{"id": str(number)} for number in range(tasks)]
    if trials is not None:
        results["info"] = {"num_trials": trials}
    (directory / "results.json").write_text(json.dumps(results))
    return directory


def run(*populations: Path) -> tuple[int, dict, str]:
    arguments = [sys.executable, str(SCRIPT)]
    for population in populations:
        arguments += ["--population", str(population)]
    finished = subprocess.run(arguments, capture_output=True, text=True)
    payload = json.loads(finished.stdout) if finished.returncode == 0 else {}
    return finished.returncode, payload, finished.stderr


class TailAnalysisTest(unittest.TestCase):
    def test_latency_is_measured_on_the_tick_clock(self):
        with TemporaryDirectory() as temporary:
            # Gaps of 1, 2 and 5 ticks at 0.2s => 0.2s, 0.4s and 1.0s.
            population = write_population(
                Path(temporary),
                "clock",
                [{"ticks": conversation([1, 2, 5])}],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            latency = report["populations"][0]["response_latency_seconds"]
            self.assertEqual(latency["observations"], 3)
            self.assertAlmostEqual(latency["min"], 0.2)
            self.assertAlmostEqual(latency["max"], 1.0)
            self.assertAlmostEqual(latency["p50"], 0.4)

    def test_tail_is_reported_not_averaged_away(self):
        with TemporaryDirectory() as temporary:
            # Ninety-nine fast turns and one very slow one: the mean stays low
            # while p99 and max must expose the outlier.
            population = write_population(
                Path(temporary),
                "tail",
                [{"ticks": conversation([1] * 99 + [100])}],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            latency = report["populations"][0]["response_latency_seconds"]
            self.assertAlmostEqual(latency["p50"], 0.2)
            self.assertAlmostEqual(latency["max"], 20.0)
            self.assertLess(latency["mean"], 0.5)
            self.assertGreater(latency["max"], 10 * latency["mean"])

    def test_unanswered_turn_is_counted_not_scored_as_zero_latency(self):
        with TemporaryDirectory() as temporary:
            ticks = conversation([2]) + [speech_tick(user="anyone there?")]
            population = write_population(Path(temporary), "silent", [{"ticks": ticks}])
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            summary = report["populations"][0]
            self.assertEqual(summary["unanswered_user_turns"], 1)
            self.assertEqual(summary["response_latency_seconds"]["observations"], 1)

    def test_mixed_tick_clocks_are_refused(self):
        with TemporaryDirectory() as temporary:
            ticks = conversation([2], duration=0.2) + conversation([2], duration=0.5)
            population = write_population(Path(temporary), "mixed", [{"ticks": ticks}])
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("mix", stderr)

    def test_infrastructure_error_contributes_no_timing(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "infra",
                [
                    {"ticks": conversation([3])},
                    {"ticks": [], "termination_reason": "infrastructure_error"},
                ],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            summary = report["populations"][0]
            self.assertEqual(summary["simulations_declared"], 2)
            self.assertEqual(summary["simulations_measured"], 1)

    def test_population_without_any_latency_is_refused(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "quiet",
                [{"ticks": [speech_tick(user="hello")]}],
            )
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("no response latency", stderr)

    def test_simulation_without_ticks_is_refused(self):
        with TemporaryDirectory() as temporary:
            population = write_population(Path(temporary), "empty", [{"ticks": []}])
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("records no ticks", stderr)

    def test_missing_simulation_file_is_refused(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary), "gap", [{"ticks": conversation([2])}]
            )
            for path in (population / "simulations").glob("*.json"):
                path.unlink()
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("missing simulation", stderr)

    def test_empty_results_file_is_refused(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary), "broken", [{"ticks": conversation([2])}]
            )
            (population / "results.json").write_text("")
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("empty", stderr)

    def test_quantiles_are_monotonic(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "monotone",
                [{"ticks": conversation(list(range(1, 51)))}],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            latency = report["populations"][0]["response_latency_seconds"]
            ordered = [
                latency["min"],
                latency["p50"],
                latency["p90"],
                latency["p95"],
                latency["p99"],
                latency["max"],
            ]
            self.assertEqual(ordered, sorted(ordered))

    def test_partial_population_reports_its_shortfall(self):
        """A latency tail measured over 12% of a cell must say so."""
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "aborted",
                [{"ticks": conversation([2]), "duration": 1.0} for _ in range(6)],
                tasks=50,
                trials=1,
            )
            code, payload, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            scope = payload["populations"][0]["scope"]
            self.assertTrue(scope["declared"])
            self.assertEqual(scope["declared_simulations"], 50)
            self.assertEqual(scope["simulations_on_disk"], 6)
            self.assertFalse(scope["complete"])

    def test_unindexed_simulation_file_is_refused(self):
        """A file no entry names would never be read; the counts must not agree."""
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "extra",
                [{"ticks": conversation([2]), "duration": 1.0}],
                tasks=1,
                trials=1,
            )
            (population / "simulations" / "orphan.json").write_text(
                json.dumps({"ticks": conversation([2]), "duration": 1.0})
            )
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("unindexed simulation orphan.json", stderr)


if __name__ == "__main__":
    unittest.main()
