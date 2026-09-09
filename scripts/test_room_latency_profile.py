import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location(
    "room_latency", Path(__file__).with_name("room-latency-profile.py")
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class LatencyProfileTest(unittest.TestCase):
    def fixture(self, root, omit=None):
        rows = [
            (1100, {"type": "conversation.item.input_audio_transcription.completed"}),
            (
                1110,
                {
                    "name": "semantic_admission_outcome",
                    "payload": {"value": {"kind": "admitted"}},
                },
            ),
            (1610, {"name": "prepared_text", "attributes": {"run_id": "answer"}}),
            (
                1615,
                {
                    "name": "tts_status",
                    "attributes": {"run_id": "unrelated"},
                    "correlation_id": "wrong:synthesis:generating",
                },
            ),
            (
                1630,
                {
                    "name": "tts_status",
                    "attributes": {"run_id": "answer"},
                    "correlation_id": "right:synthesis:generating",
                },
            ),
            (
                1830,
                {"name": "gateway_speech_audio", "attributes": {"run_id": "answer"}},
            ),
        ]
        events = [{"at_ms": at, "event": e} for at, e in rows if omit is None or e.get("name") != omit]
        (root / "events.json").write_text(json.dumps(events))
        (root / "result.json").write_text(
            json.dumps(
                {
                    "turns": [
                        {"turn": 1, "input_end_ms": 1000, "response_latency_ms": 900},
                        {"turn": 2, "input_end_ms": 5000, "response_latency_ms": None},
                    ]
                }
            )
        )

    def test_stages_sum_to_audio_latency_and_match_generation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.fixture(root)
            report = module.profile(root)
        self.assertEqual(report["attempted_turns"], 2)
        self.assertEqual(report["profiled_turns"], 1)
        row = report["rows"][0]
        self.assertEqual([row[k] for k in module.STAGES], [100, 10, 500, 20, 200, 70])
        self.assertEqual(sum(row[k] for k in module.STAGES), row["total_ms"])
        self.assertEqual(report["rows"][1]["excluded"], "no audible response")

    def test_missing_boundary_is_excluded_instead_of_zero_latency(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.fixture(root, omit="prepared_text")
            report = module.profile(root)
        self.assertEqual(report["profiled_turns"], 0)
        self.assertEqual(report["summary"], {})
        self.assertIn("missing", report["rows"][0]["excluded"])


if __name__ == "__main__":
    unittest.main()
