import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    "room_model_latency", Path(__file__).with_name("room-model-latency.py")
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class FakeResponse:
    def __init__(self, events):
        self.status_code = 200
        self.text = ""
        self._events = events

    def __enter__(self):
        return self

    def __exit__(self, *exception):
        return False

    def iter_lines(self, chunk_size=1):
        for event in self._events:
            yield b"data: " + json.dumps(event).encode()


class FakeSession:
    def __init__(self, events):
        self._events = events
        self.body = None

    def post(self, url, **kwargs):
        self.body = kwargs["json"]
        return FakeResponse(self._events)


def event(text, thought=False, usage=None, finish=None):
    part = {"text": text}
    if thought:
        part["thought"] = True
    candidate = {"content": {"parts": [part]}}
    if finish:
        candidate["finishReason"] = finish
    payload = {"candidates": [candidate]}
    if usage:
        payload["usageMetadata"] = usage
    return payload


CASE = {"name": "case", "expected": "Paris", "body": {"contents": []}}


class ThinkingConfigTest(unittest.TestCase):
    def measure(self, thinking, include_thoughts, events=None):
        events = events or [event("The capital is Paris.", finish="STOP")]
        session = FakeSession(events)
        row = module.measure(session, "model", thinking, CASE, include_thoughts)
        config = session.body["generationConfig"]["thinkingConfig"]
        return config, row

    def test_level_request_carries_no_numeric_budget(self):
        config, row = self.measure("low", True)
        self.assertEqual(config, {"thinkingLevel": "low", "includeThoughts": True})
        self.assertNotIn("thinkingBudget", config)
        self.assertEqual(row["thinking_level"], "low")
        self.assertNotIn("thinking_budget", row)

    def test_budget_request_carries_no_level(self):
        config, row = self.measure(128, False)
        self.assertEqual(config, {"thinkingBudget": 128, "includeThoughts": False})
        self.assertNotIn("thinkingLevel", config)
        self.assertEqual(row["thinking_budget"], 128)
        self.assertNotIn("thinking_level", row)

    def test_thought_parts_are_timed_and_excluded_from_the_answer(self):
        events = [
            event("Consider the question.", thought=True),
            event("The capital is Paris.", usage={"thoughtsTokenCount": 12}, finish="STOP"),
        ]
        _, row = self.measure("high", True, events)
        self.assertEqual(row["text"], "The capital is Paris.")
        self.assertEqual(row["thought_summary_chars"], len("Consider the question."))
        self.assertLessEqual(row["first_thought_ms"], row["first_text_ms"])
        self.assertEqual(row["usage"]["thoughtsTokenCount"], 12)
        self.assertTrue(row["basic_pass"])

    def test_absent_thought_summary_is_reported_as_zero_characters(self):
        _, row = self.measure("medium", True)
        self.assertEqual(row["thought_summary_chars"], 0)
        self.assertNotIn("first_thought_ms", row)
        self.assertTrue(row["include_thoughts"])


if __name__ == "__main__":
    unittest.main()
