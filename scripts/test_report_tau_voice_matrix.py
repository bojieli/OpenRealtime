#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

SCRIPT = Path(__file__).with_name("report-tau-voice-matrix.py")
SPEC = importlib.util.spec_from_file_location("report_tau_voice_matrix", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
REPORT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(REPORT)


def item(**values):
    return SimpleNamespace(**values)


class PopulationValidationTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.experiment = Path(self.temporary.name)
        (self.experiment / "simulations").mkdir()
        (self.experiment / "results.json").write_text("{}\n", encoding="utf-8")
        for simulation_id in ("sim-0", "sim-1"):
            (self.experiment / "simulations" / f"{simulation_id}.json").write_text(
                "{}\n", encoding="utf-8"
            )
        self.metadata = item(
            info=item(
                environment_info=item(domain_name="airline"),
                num_trials=1,
                seed=300,
                speech_complexity="control",
                audio_native_config=item(
                    tick_duration_seconds=0.2,
                    provider="openai",
                    model="gpt-realtime-1.5",
                    base_url="ws://127.0.0.1:8765/v1/realtime",
                ),
                agent_info=item(
                    implementation="discrete_time_audio_native_agent",
                    llm="openai:gpt-realtime-1.5",
                ),
                user_info=item(
                    voice_settings=item(
                        synthesis_config=item(
                            provider="fish_audio",
                            provider_config=item(
                                voice_registry={"person": "tau-person-v1"}
                            ),
                        )
                    )
                ),
            ),
            tasks=[item(id="task-0"), item(id="task-1")],
            simulation_index=[
                item(
                    id="sim-0",
                    task_id="task-0",
                    trial=0,
                    termination_reason="user_stop",
                ),
                item(
                    id="sim-1",
                    task_id="task-1",
                    trial=0,
                    termination_reason="infrastructure_error",
                ),
            ],
        )
        self.simulations = [
            item(id="sim-0", task_id="task-0", trial=0),
            item(id="sim-1", task_id="task-1", trial=0),
        ]

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def validate(self):
        with (
            mock.patch.object(REPORT.Results, "load_metadata", return_value=self.metadata),
            mock.patch.object(
                REPORT.Results,
                "iter_simulations",
                return_value=iter(self.simulations),
            ),
        ):
            return REPORT.validate_population(
                self.experiment,
                cell_id="cell",
                speech_complexity="control",
                domain="airline",
                expected_tasks=2,
                num_trials=1,
                seed=300,
                tick_seconds=0.2,
                transport={
                    "provider": "openai",
                    "compatibility_model": "gpt-realtime-1.5",
                    "base_url": "ws://127.0.0.1:8765/v1/realtime",
                },
                voice_registry={"person": "tau-person-v1"},
            )

    def test_accepts_exact_population_and_preserves_infrastructure_failure(self) -> None:
        self.assertEqual(
            self.validate(),
            {
                "status": "complete",
                "tasks": 2,
                "trials_per_task": 1,
                "simulations": 2,
                "termination_reasons": {
                    "infrastructure_error": 1,
                    "user_stop": 1,
                },
                "infrastructure_errors": 1,
            },
        )

    def test_rejects_duplicate_task_trial_rows(self) -> None:
        self.metadata.simulation_index[1].task_id = "task-0"
        with self.assertRaisesRegex(
            REPORT.IncompleteMatrixError, "duplicate task/trial rows"
        ):
            self.validate()

    def test_rejects_index_file_identity_disagreement(self) -> None:
        self.simulations[1].task_id = "task-0"
        with self.assertRaisesRegex(REPORT.IncompleteMatrixError, "disagrees"):
            self.validate()


class AggregationTest(unittest.TestCase):
    def test_equal_domain_means_and_summed_counts(self) -> None:
        aggregate = REPORT.aggregate_agent_metrics(
            {
                "airline": {
                    "avg_reward": 0.5,
                    "pass_hat_ks": {"1": 0.5},
                    "avg_agent_cost": 1.0,
                    "total_simulations": 2,
                    "agent_errors_by_severity": {"minor": 1},
                },
                "retail": {
                    "avg_reward": 1.0,
                    "pass_hat_ks": {"1": 1.0},
                    "avg_agent_cost": None,
                    "total_simulations": 3,
                    "agent_errors_by_severity": {"critical": 2},
                },
            }
        )
        self.assertEqual(aggregate["avg_reward"], 0.75)
        self.assertEqual(aggregate["pass_hat_ks"], {"1": 0.75})
        self.assertEqual(aggregate["avg_agent_cost"], 1.0)
        self.assertEqual(aggregate["total_simulations"], 5)
        self.assertEqual(
            aggregate["agent_errors_by_severity"], {"critical": 2, "minor": 1}
        )


class RuntimeEvidenceTest(unittest.TestCase):
    def test_subtracts_additive_metrics_and_omits_maximum_gauges(self) -> None:
        delta = REPORT.cumulative_runtime_delta(
            {
                "sessions_started": 2,
                "asr_provider_maximum_elapsed_ns": 90,
                "fast": {
                    "invocations": 4,
                    "cumulative_elapsed_ns": 100,
                    "maximum_elapsed_ns": 80,
                },
            },
            {
                "sessions_started": 7,
                "asr_provider_maximum_elapsed_ns": 400,
                "fast": {
                    "invocations": 10,
                    "cumulative_elapsed_ns": 900,
                    "maximum_elapsed_ns": 300,
                },
            },
        )
        self.assertEqual(
            delta,
            {
                "sessions_started": 5,
                "fast": {"invocations": 6, "cumulative_elapsed_ns": 800},
            },
        )

    def test_rejects_counter_regression(self) -> None:
        with self.assertRaisesRegex(REPORT.IncompleteMatrixError, "decreased"):
            REPORT.cumulative_runtime_delta(
                {"slow": {"invocations": 3}},
                {"slow": {"invocations": 1}},
            )


if __name__ == "__main__":
    unittest.main()
