#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name("report-tau-endpoint-preparation.py")
SPEC = importlib.util.spec_from_file_location(
    "report_tau_endpoint_preparation", SCRIPT
)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
REPORT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(REPORT)


def matrix(policy: str) -> dict:
    return {
        "benchmark": {"tasks": 1},
        "transport": {"adapter": "openai"},
        "system": {
            "asr": "qwen",
            "fast": "fast",
            "slow": "slow",
            "tool_resumption": "exact",
            "agent_speech": "fish",
            "caller_speech": "fish",
        },
        "runtime_requirements": {
            "preparation_policy": policy,
            "slow_context": "canonical",
            "requires_local_fast": True,
            "speech": {"name": "fish", "version": "1"},
            "asr": {"model": "qwen"},
            "gateway_profiles": {"fast": "qwen", "slow": "gemini"},
        },
        "cells": [
            {
                "id": policy,
                "speech_complexity": "control",
                "voice_registry": "voices.json",
                "status": "preregistered",
            }
        ],
    }


class DifferenceTest(unittest.TestCase):
    def test_subtracts_only_common_finite_numeric_leaves(self) -> None:
        self.assertEqual(
            REPORT.numeric_difference(
                {"reward": 0.75, "counts": {"calls": 5}, "label": "left"},
                {"reward": 0.5, "counts": {"calls": 3}, "label": "right"},
            ),
            {"counts": {"calls": 2}, "reward": 0.25},
        )

    def test_scales_nested_runtime_counters(self) -> None:
        self.assertEqual(
            REPORT.scale_numeric({"calls": 4, "timing": {"ns": 20}}, 2),
            {"calls": 2.0, "timing": {"ns": 10.0}},
        )

    def test_endpoint_manipulation_check_rejects_private_work(self) -> None:
        continuous = {
            "fast_preparation": {"invocations": 4},
            "slow_preparation": {"invocations": 3},
        }
        endpoint = {
            "fast_preparation": {"invocations": 0},
            "slow_preparation": {"invocations": 0},
        }
        self.assertTrue(
            REPORT.preparation_manipulation_check(continuous, endpoint)["fast"][
                "continuous_opened_private_work"
            ]
        )
        endpoint["fast_preparation"]["invocations"] = 1
        with self.assertRaisesRegex(REPORT.PairedReportError, "endpoint-only"):
            REPORT.preparation_manipulation_check(continuous, endpoint)


class InvariantTest(unittest.TestCase):
    def test_accepts_preparation_as_the_only_policy_change(self) -> None:
        REPORT.validate_pair_invariants(matrix("continuous"), matrix("endpoint-only"))

    def test_rejects_provider_change(self) -> None:
        continuous = matrix("continuous")
        endpoint = matrix("endpoint-only")
        endpoint["system"]["fast"] = "different"
        with self.assertRaisesRegex(REPORT.PairedReportError, "system.fast"):
            REPORT.validate_pair_invariants(continuous, endpoint)


class HealthTest(unittest.TestCase):
    def test_requires_exact_preparation_and_speech_profiles(self) -> None:
        endpoint = matrix("endpoint-only")
        endpoint["runtime_requirements"]["asr"] = {
            "model": "qwen",
            "provider_chunk_ms": 200,
            "provider_max_chunk_ms": 200,
            "strategy": "fixed",
        }
        endpoint["runtime_requirements"]["gateway_profiles"] = {
            phase: {
                "provider": phase,
                "model": phase,
                "effort": "minimal" if phase == "fast" else "high",
                "tool_authority": "propose" if phase == "fast" else "execute",
            }
            for phase in ("fast", "slow")
        }
        health = {
            "preparation_policy": "endpoint-only",
            "slow_context": "canonical",
            "asr": endpoint["runtime_requirements"]["asr"],
            "fast": endpoint["runtime_requirements"]["gateway_profiles"]["fast"],
            "slow": endpoint["runtime_requirements"]["gateway_profiles"]["slow"],
            "speech": {"name": "fish", "version": "1"},
        }
        REPORT.validate_health(health, endpoint, "endpoint")
        health["speech"]["name"] = "other"
        with self.assertRaisesRegex(REPORT.PairedReportError, "speech name"):
            REPORT.validate_health(health, endpoint, "endpoint")


if __name__ == "__main__":
    unittest.main()
