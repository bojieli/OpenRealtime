import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("native_probe", Path(__file__).with_name("native_pair_probe.py"))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class ScheduleTest(unittest.TestCase):
    def test_causal_words_and_control_preserve_horizon(self):
        def word(group, text, at):
            return {"kind": "word", "group": group, "text": text,
                    "source_start": at - 1, "available_at": at}
        variant = {"feedback": "change", "events": [word("change", "skip", 1_600_000_000)],
                   "expect": {"within": 1_000_000_000}}
        pair = {"prefix": [word("open", "hello", 100_000_000)], "variants": [variant]}
        original = list(probe.schedule(pair, variant))
        control = list(probe.schedule(pair, variant, True))
        self.assertEqual([t for t, _ in original], [t for t, _ in control])
        self.assertEqual([e["text"] for _, es in control for e in es], ["hello"])
        self.assertEqual([e["text"] for _, es in original for e in es], ["hello", "skip"])
        for at, events in original:
            self.assertTrue(all(e["available_at"] <= at for e in events))

    def test_chunking_delays_known_words_without_future_release(self):
        words = [{"kind": "word", "group": "open", "text": str(i),
                  "source_start": i, "available_at": 1_000_000_000} for i in range(5)]
        variant = {"feedback": "change", "events": [], "expect": {"within": 1_000_000_000}}
        pair = {"prefix": words, "variants": [variant]}
        rows = list(probe.schedule(pair, variant, words_per_tick=2))
        self.assertEqual([len(es) for _, es in rows[:3]], [2, 2, 1])
        self.assertEqual([e for _, es in rows for e in es], words)
        self.assertTrue(all(e["available_at"] <= at for at, es in rows for e in es))
