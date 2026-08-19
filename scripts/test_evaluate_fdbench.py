#!/usr/bin/env python3
"""Tests for the FD-Bench metric aggregator.

The aggregator's job is to report what the pinned upstream decision core
measured. Its failure mode is not a wrong number but a fabricated one: every
rate has a denominator and every latency a sample set, and both can legitimately
be empty. These tests hold the line that an empty population produces no value
at all rather than a zero, because for the latency metrics zero is the best
score attainable and would be published as an excellent result.
"""

from __future__ import annotations

import importlib.util
from pathlib import Path
import unittest


SCRIPT = Path(__file__).with_name("evaluate-fdbench.py")
SPEC = importlib.util.spec_from_file_location("evaluate_fdbench", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
EVALUATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(EVALUATE)


EMPTY_MARK = {
    "Number Round": 0,
    "Number Interrupt": 0,
    "Number Gaps": 0,
    "Success Response": 0,
    "Success Response to Interruptions": 0,
    "Success Interrupt": 0,
    "Wrong Interrupt": 0,
    "Noise Interrupt": 0,
    "Interrupt Delay": [],
    "Response Delay": [],
    "Response Delay to Interruption": [],
    "Lead Times": [],
    "Lead Times Interrupt": [],
    "Lead Times to Interruption": [],
}


def mark(**overrides: object) -> dict[str, object]:
    record = dict(EMPTY_MARK)
    record.update(overrides)
    return record


class Upstream:
    """The surface of upstream Benchmarking that aggregate() actually reads."""

    def __init__(self, marks: dict[str, object], categories: dict[str, int] | None = None):
        self.VAD_marks = marks
        self.interruption_cats = dict(categories or {})
        self.interruption_success_cats = dict.fromkeys(self.interruption_cats, 0)
        self.interruption_res_suc_cats = dict.fromkeys(self.interruption_cats, 0)


class UndefinedValueTest(unittest.TestCase):
    def test_a_rate_over_no_trials_has_no_value(self) -> None:
        self.assertIsNone(EVALUATE.rate(0, 0))
        self.assertIsNone(EVALUATE.percentage(0, 0))

    def test_a_rate_over_real_trials_keeps_its_value(self) -> None:
        self.assertEqual(EVALUATE.rate(1, 4), 0.25)
        self.assertEqual(EVALUATE.percentage(1, 4), 25.0)

    def test_a_genuine_zero_rate_is_not_confused_with_an_absent_one(self) -> None:
        self.assertEqual(EVALUATE.percentage(0, 4), 0.0)
        self.assertIsNone(EVALUATE.percentage(0, 0))

    def test_a_median_of_no_samples_has_no_value(self) -> None:
        self.assertIsNone(EVALUATE.median([]))
        self.assertIsNone(EVALUATE.milliseconds([]))

    def test_a_median_of_real_samples_keeps_its_value(self) -> None:
        self.assertEqual(EVALUATE.median([1, 2, 3]), 2.0)
        self.assertEqual(EVALUATE.median([1, 3]), 2.0)
        self.assertEqual(EVALUATE.milliseconds([1, 3]), 2.0)


class AggregateTest(unittest.TestCase):
    def test_refuses_an_evaluation_that_scored_nothing(self) -> None:
        with self.assertRaisesRegex(ValueError, "scored no rounds"):
            EVALUATE.aggregate(Upstream({}))

    def test_refuses_conversations_that_all_dropped_out(self) -> None:
        """Marks exist, but every one was skipped, so nothing was scored."""
        with self.assertRaisesRegex(ValueError, "scored no rounds"):
            EVALUATE.aggregate(Upstream({"a": mark(), "b": mark()}))

    def test_separates_a_measured_zero_from_an_unmeasurable_one(self) -> None:
        result = EVALUATE.aggregate(
            Upstream({"a": mark(**{"Number Round": 4, "Number Gaps": 3})})
        )
        metrics = result["metrics"]
        # Four rounds were scored and none succeeded: a real 0%.
        self.assertEqual(metrics["SRR_pct"], 0.0)
        self.assertEqual(metrics["EIR_pct"], 0.0)
        # Three gaps occurred and none drew a noise interruption: a real 0%.
        self.assertEqual(metrics["NIR_pct"], 0.0)
        # No interruption ever occurred, so its success rate is undefined.
        self.assertIsNone(metrics["SIR_pct"])
        self.assertIsNone(metrics["SRIR_pct"])
        # No timing sample was emitted, so no latency has a median.
        for name in ("FSED_ms", "ERT_ms", "EIT_ms", "IRD_ms"):
            self.assertIsNone(metrics[name], name)

    def test_every_undefined_metric_names_the_population_that_was_empty(self) -> None:
        result = EVALUATE.aggregate(
            Upstream({"a": mark(**{"Number Round": 4, "Number Gaps": 3})})
        )
        undefined = {name for name, value in result["metrics"].items() if value is None}
        self.assertEqual(set(result["not_measured"]), undefined)
        self.assertIn("observed interruptions", result["not_measured"]["SIR_pct"])
        self.assertIn("response-delay samples", result["not_measured"]["FSED_ms"])

    def test_a_fully_observed_run_leaves_nothing_unmeasured(self) -> None:
        result = EVALUATE.aggregate(
            Upstream(
                {
                    "a": mark(
                        **{
                            "Number Round": 4,
                            "Number Interrupt": 2,
                            "Number Gaps": 1,
                            "Success Response": 3,
                            "Success Response to Interruptions": 1,
                            "Success Interrupt": 2,
                            "Wrong Interrupt": 1,
                            "Noise Interrupt": 1,
                            "Interrupt Delay": [[160, 320]],
                            "Response Delay": [[320]],
                            "Response Delay to Interruption": [[480]],
                            "Lead Times": [[160]],
                            "Lead Times Interrupt": [[320]],
                            "Lead Times to Interruption": [[480]],
                        }
                    )
                }
            )
        )
        self.assertEqual(result["not_measured"], {})
        self.assertNotIn(None, result["metrics"].values())
        self.assertEqual(result["metrics"]["SRR_pct"], 75.0)
        self.assertEqual(result["counts"]["rounds"], 4)

    def test_preserves_the_upstream_double_count_in_first_speech_delay(self) -> None:
        """analyze_VAD_marks counts a post-interruption response in FSED too.

        Dropping that would silently change the published metric relative to the
        pinned upstream implementation this wrapper exists to reproduce.
        """
        result = EVALUATE.aggregate(
            Upstream(
                {
                    "a": mark(
                        **{
                            "Number Round": 2,
                            "Response Delay": [[160]],
                            "Response Delay to Interruption": [[160, 160]],
                        }
                    )
                }
            )
        )
        # 10 ms three times over: one direct sample and two counted twice would
        # both give 10, so assert the sample set size through the median of a
        # skewed set instead.
        skewed = EVALUATE.aggregate(
            Upstream(
                {
                    "a": mark(
                        **{
                            "Number Round": 2,
                            "Response Delay": [[16000]],
                            "Response Delay to Interruption": [[160, 160]],
                        }
                    )
                }
            )
        )
        self.assertEqual(result["metrics"]["FSED_ms"], 10.0)
        # Samples are [1000, 10, 10]; the median is 10 only because the
        # interruption delays are folded in.
        self.assertEqual(skewed["metrics"]["FSED_ms"], 10.0)

    def test_refuses_upstream_category_tables_that_disagree(self) -> None:
        upstream = Upstream({"a": mark(**{"Number Round": 1})}, {"Topic Shift": 3})
        upstream.interruption_success_cats = {}
        with self.assertRaisesRegex(ValueError, "missing from interruption_success"):
            EVALUATE.aggregate(upstream)

    def test_carries_every_upstream_category_through(self) -> None:
        upstream = Upstream(
            {"a": mark(**{"Number Round": 1})}, {"Topic Shift": 3, "Denial": 1}
        )
        upstream.interruption_success_cats = {"Topic Shift": 2, "Denial": 0}
        upstream.interruption_res_suc_cats = {"Topic Shift": 1, "Denial": 0}
        categories = EVALUATE.aggregate(upstream)["categories"]
        self.assertEqual(set(categories), {"Topic Shift", "Denial"})
        self.assertEqual(categories["Topic Shift"]["all"], 3)
        self.assertEqual(categories["Topic Shift"]["interruption_success"], 2)
        self.assertEqual(categories["Topic Shift"]["response_success"], 1)


if __name__ == "__main__":
    unittest.main()
