#!/usr/bin/env python3
"""Tests for the tau-Voice failure-mechanism classifier."""

from __future__ import annotations

import json
import subprocess
import sys
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

SCRIPT = Path(__file__).resolve().parent / "classify-tau-voice-failures.py"


def tick(agent_calls=None, agent_results=None, agent_text=None, user_text=None):
    return {
        "tick_id": 0,
        "agent_tool_calls": agent_calls or [],
        "agent_tool_results": agent_results or [],
        "agent_chunk": {"content": agent_text} if agent_text else None,
        "user_chunk": {"content": user_text} if user_text else None,
    }


def write_population(root: Path, name: str, simulations: list[dict]) -> Path:
    directory = root / name
    (directory / "simulations").mkdir(parents=True)
    index = []
    for position, simulation in enumerate(simulations):
        identifier = simulation.get("id", f"sim{position}")
        simulation["id"] = identifier
        index.append(
            {
                "id": identifier,
                "task_id": simulation.get("task_id", str(position)),
                "trial": 0,
                "reward": simulation.get("reward", 0.0),
                "termination_reason": simulation["termination_reason"],
                "duration": 1.0,
            }
        )
        (directory / "simulations" / f"{identifier}.json").write_text(
            json.dumps(simulation)
        )
    (directory / "results.json").write_text(json.dumps({"simulation_index": index}))
    return directory


def run(*populations: Path) -> tuple[int, dict, str]:
    arguments = [sys.executable, str(SCRIPT)]
    for population in populations:
        arguments += ["--population", str(population)]
    finished = subprocess.run(arguments, capture_output=True, text=True)
    payload = {}
    if finished.returncode == 0:
        payload = json.loads(finished.stdout)
    return finished.returncode, payload, finished.stderr


class ClassifierTest(unittest.TestCase):
    def test_spelled_token_is_named_not_lumped(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "spelled",
                [
                    {
                        "termination_reason": "too_many_errors",
                        "ticks": [
                            tick(
                                agent_calls=[
                                    {
                                        "name": "find_user",
                                        "arguments": {"last_name": "K O V A C S"},
                                    }
                                ],
                                agent_results=[
                                    {"error": True, "content": "Error: User not found"}
                                ],
                                agent_text="please spell it out letter by letter",
                            )
                        ]
                        * 10,
                    }
                ],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            mechanisms = report["populations"][0]["mechanisms"]
            self.assertEqual(mechanisms, {"spelled_token_not_reassembled": 1})
            detail = report["populations"][0]["details"][0]
            self.assertTrue(detail["repair_requested"])
            self.assertIn("K O V A C S", detail["unjoined_spelling_examples"])

    def test_variant_search_is_distinguished_from_spelling(self):
        with TemporaryDirectory() as temporary:
            calls = [
                tick(
                    agent_calls=[
                        {"name": "get_user", "arguments": {"user_id": f"raj_sanchez_{n}"}}
                    ],
                    agent_results=[{"error": True, "content": "Error: User not found"}],
                )
                for n in (740, 74, 7, 40, 7340)
            ]
            population = write_population(
                Path(temporary),
                "variants",
                [{"termination_reason": "too_many_errors", "ticks": calls}],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            self.assertEqual(
                report["populations"][0]["mechanisms"],
                {"identifier_variant_search": 1},
            )

    def test_non_lookup_errors_are_not_called_identifier_failures(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "other",
                [
                    {
                        "termination_reason": "too_many_errors",
                        "ticks": [
                            tick(
                                agent_calls=[
                                    {"name": "book", "arguments": {"id": "abc"}}
                                ],
                                agent_results=[
                                    {
                                        "error": True,
                                        "content": "Error: policy violation, cannot book",
                                    }
                                ],
                            )
                        ]
                        * 4,
                    }
                ],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            self.assertEqual(
                report["populations"][0]["mechanisms"], {"other_tool_errors": 1}
            )

    def test_evidence_free_simulation_is_unclassified_not_guessed(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "empty",
                [{"termination_reason": "too_many_errors", "ticks": []}],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            self.assertEqual(report["populations"][0]["mechanisms"], {"unclassified": 1})

    def test_successful_runs_are_not_classified_as_failures(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "good",
                [
                    {"termination_reason": "user_stop", "reward": 1.0, "ticks": []},
                    {"termination_reason": "agent_stop", "reward": 1.0, "ticks": []},
                ],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            self.assertEqual(report["populations"][0]["mechanisms"], {})
            self.assertEqual(report["populations"][0]["error_terminated"], 0)
            self.assertEqual(report["populations"][0]["mean_reward_scored"], 1.0)

    def test_null_reward_is_excluded_from_the_mean_not_read_as_zero(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "mixed",
                [
                    {"termination_reason": "user_stop", "reward": 1.0, "ticks": []},
                    {
                        "termination_reason": "infrastructure_error",
                        "reward": None,
                        "ticks": [],
                    },
                ],
            )
            code, report, stderr = run(population)
            self.assertEqual(code, 0, stderr)
            summary = report["populations"][0]
            self.assertEqual(summary["mean_reward_scored"], 1.0)
            self.assertEqual(summary["scored_simulations"], 1)
            self.assertEqual(summary["simulations"], 2)

    def test_empty_results_file_is_refused(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary), "broken", [{"termination_reason": "user_stop"}]
            )
            (population / "results.json").write_text("")
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("empty", stderr)

    def test_missing_simulation_file_is_refused(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary),
                "gap",
                [{"termination_reason": "too_many_errors", "ticks": []}],
            )
            for path in (population / "simulations").glob("*.json"):
                path.unlink()
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("missing simulation", stderr)

    def test_untyped_termination_reason_is_refused(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary), "untyped", [{"termination_reason": "user_stop"}]
            )
            results = json.loads((population / "results.json").read_text())
            results["simulation_index"][0]["termination_reason"] = ""
            (population / "results.json").write_text(json.dumps(results))
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("no termination reason", stderr)

    def test_population_declaring_no_simulations_is_refused(self):
        with TemporaryDirectory() as temporary:
            population = write_population(
                Path(temporary), "vacuous", [{"termination_reason": "user_stop"}]
            )
            (population / "results.json").write_text(
                json.dumps({"simulation_index": []})
            )
            code, _, stderr = run(population)
            self.assertEqual(code, 1)
            self.assertIn("declares no simulations", stderr)


if __name__ == "__main__":
    unittest.main()
