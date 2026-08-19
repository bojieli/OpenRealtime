#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("run-fdbv3-strict-judge.py")
SPEC = importlib.util.spec_from_file_location("strict_fdbv3_judge", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
JUDGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(JUDGE)


class FakeCompletions:
    def __init__(self, content: str) -> None:
        self.content = content

    def create(self, **_kwargs):
        return SimpleNamespace(
            id="response",
            model="gpt-4o-2024-08-06",
            choices=[
                SimpleNamespace(message=SimpleNamespace(content=self.content))
            ],
            usage={"total_tokens": 10},
        )


class StrictCompletionTest(unittest.TestCase):
    def test_records_only_a_valid_json_judge_response(self) -> None:
        records = []
        completion = JUDGE.StrictCompletions(
            FakeCompletions('{"correct":true,"explanation":"ok"}'), records
        )
        completion.create(model="gpt-4o", messages=[{"role": "user", "content": "x"}])
        self.assertEqual(len(records), 1)
        self.assertEqual(records[0]["requested_model"], "gpt-4o")
        self.assertEqual(len(records[0]["response_sha256"]), 64)
        self.assertEqual(len(records[0]["parsed_response_sha256"]), 64)
        self.assertEqual(records[0]["usage"]["total_tokens"], 10)

    def test_rejects_invalid_json_without_recording_success(self) -> None:
        records = []
        completion = JUDGE.StrictCompletions(FakeCompletions("not json"), records)
        with self.assertRaises(JUDGE.StrictJudgeError):
            completion.create(model="gpt-4o", messages=[])
        self.assertEqual(records, [])

    def test_rejects_a_response_missing_the_explanation(self) -> None:
        records = []
        completion = JUDGE.StrictCompletions(
            FakeCompletions('{"correct":true}'), records
        )
        with self.assertRaises(JUDGE.StrictJudgeError):
            completion.create(model="gpt-4o", messages=[])
        self.assertEqual(records, [])

    def test_rejects_an_empty_explanation(self) -> None:
        records = []
        completion = JUDGE.StrictCompletions(
            FakeCompletions('{"correct":true,"explanation":""}'), records
        )
        with self.assertRaises(JUDGE.StrictJudgeError):
            completion.create(model="gpt-4o", messages=[])
        self.assertEqual(records, [])


class ExpectedCallTest(unittest.TestCase):
    def test_counts_only_matched_arguments_and_nonempty_responses(self) -> None:
        benchmark = {
            "scenarios": [
                {
                    "id": "one",
                    "expected_tool_calls": [
                        {"function": "search"},
                        {"function": "book"},
                    ],
                }
            ]
        }
        with tempfile.TemporaryDirectory() as directory:
            result = Path(directory) / "result_openrealtime.json"
            result.write_text(
                json.dumps(
                    {
                        "example_id": "one",
                        "actual_tool_calls": [{"function": "search"}],
                        "transcript": "done",
                    }
                ),
                encoding="utf-8",
            )
            entries, counts = JUDGE.build_entries(
                benchmark, Path(directory), "openrealtime"
            )
        self.assertEqual(len(entries), 1)
        self.assertEqual(counts, {"argument": 1, "response": 1, "total": 2})

    def build_one(self, result: dict) -> tuple[list, dict]:
        benchmark = {
            "scenarios": [
                {"id": "one", "expected_tool_calls": [{"function": "search"}]}
            ]
        }
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / "result_openrealtime.json").write_text(
                json.dumps(result), encoding="utf-8"
            )
            return JUDGE.build_entries(benchmark, Path(directory), "openrealtime")

    def test_refuses_a_result_that_recorded_no_tool_calls(self) -> None:
        with self.assertRaises(JUDGE.StrictJudgeError) as caught:
            self.build_one({"example_id": "one", "transcript": "done"})
        self.assertIn("recorded no tool calls", str(caught.exception))

    def test_refuses_a_result_whose_tool_calls_are_not_records(self) -> None:
        with self.assertRaises(JUDGE.StrictJudgeError):
            self.build_one(
                {
                    "example_id": "one",
                    "actual_tool_calls": ["search"],
                    "transcript": "done",
                }
            )

    def test_refuses_a_result_that_recorded_no_transcript(self) -> None:
        with self.assertRaises(JUDGE.StrictJudgeError) as caught:
            self.build_one(
                {"example_id": "one", "actual_tool_calls": [{"function": "search"}]}
            )
        self.assertIn("recorded no transcript", str(caught.exception))

    def test_refuses_a_result_whose_transcript_is_null(self) -> None:
        with self.assertRaises(JUDGE.StrictJudgeError):
            self.build_one(
                {
                    "example_id": "one",
                    "actual_tool_calls": [{"function": "search"}],
                    "transcript": None,
                }
            )

    def test_still_counts_a_sample_that_legitimately_called_nothing(self) -> None:
        entries, counts = self.build_one(
            {"example_id": "one", "actual_tool_calls": [], "transcript": "done"}
        )
        self.assertEqual(len(entries), 1)
        self.assertEqual(counts, {"argument": 0, "response": 1, "total": 1})


if __name__ == "__main__":
    unittest.main()
