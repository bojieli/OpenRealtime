import importlib.util
import json
from pathlib import Path
import struct
import tempfile
import unittest
import wave

spec = importlib.util.spec_from_file_location("study_summary", Path(__file__).with_name("summarize.py"))
summary = importlib.util.module_from_spec(spec)
spec.loader.exec_module(summary)


class OverlapTest(unittest.TestCase):
    def test_failed_trial_with_no_decisions_is_not_complete(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "fixtures.json").write_text("[]")
            branch = root / "failed"
            branch.mkdir()
            (branch / "actions.jsonl").write_text("")
            (branch / "result.json").write_text(json.dumps({
                "status": "failed", "trial_error": "context canceled", "session_id": "run/trial"
            }))
            report = summary.summarize(root)
            self.assertEqual(report["incomplete_branches"], 1)
            self.assertEqual(report["branches"][0]["status"], "failed")
            self.assertEqual(report["branches"][0]["trial_error"], "context canceled")

    def test_overlap_uses_both_channels_and_bounded_window(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "stereo.wav"
            with wave.open(str(path), "wb") as wav:
                wav.setparams((2, 2, 1000, 0, "NONE", "not compressed"))
                wav.writeframes(
                    struct.pack("<hh", 4000, 0) * 100
                    + struct.pack("<hh", 4000, 4000) * 100
                    + struct.pack("<hh", 0, 4000) * 100
                )
            result = summary.acoustic_overlap(path, 0, 300_000_000)
            self.assertAlmostEqual(result["simultaneous_energy_seconds"], 0.1)
            self.assertAlmostEqual(result["checked_seconds"], 0.3)
            result = summary.acoustic_overlap(path, 0, 100_000_000)
            self.assertEqual(result["simultaneous_energy_seconds"], 0)


class LatencyTest(unittest.TestCase):
    def test_empty_and_stratified_requests(self):
        self.assertEqual(summary.latency_summary([])["all"]["count"], 0)
        rows = [{"decision": {"duration_ns": n * 1_000_000,
                             "action": {"text": text}}}
                for n, text in [(100, ""), (300, ""), (900, "hello")]]
        result = summary.latency_summary(rows)
        self.assertEqual(result["all"]["median"], 300)
        self.assertEqual(result["all"]["p95_nearest_rank"], 900)
        self.assertEqual(result["without_text"]["median"], 200)
        self.assertEqual(result["with_text"]["count"], 1)


class ExecutionTest(unittest.TestCase):
    def test_rejected_revisions_and_trial_cut_are_distinct(self):
        rows = [{"model_admission_ns": n, "execution_status": status,
                 "decision": {"action": {"act": "revise", "text": "same words",
                                          "replaces_pending": "segment-1"}}}
                for n, status in [(10, "synthesis-started"), (20, "invalid-replacement")]]
        result = {"ledger": {"segments": [{"id": "segment-1", "text": "same words"}]}}
        marks = [{"mark": "cancel", "reason": "policy-revise"},
                 {"mark": "cancel", "reason": "trial-horizon"}]
        report = summary.execution_diagnostics(rows, result, marks, 15)
        self.assertEqual(report["executed_identical_text_revisions"], 1)
        self.assertEqual(report["post_feedback_execution_outcomes"], {"invalid-replacement": 1})
        self.assertEqual(report["cancellation_requests_by_reason"]["trial-horizon"], 1)
        self.assertEqual(report["synthesized_segment_words_max"], 2)



class PairedDiscriminationTest(unittest.TestCase):
    def pair(self):
        expect = {"after": "fb-w0", "within": 10, "require_any_of": [["feed", "feeding"]],
                  "forbid": ["oven"], "min_words": 3}
        return {"id": "p", "prefix": [], "variants": [{"id": "deepen", "expect": expect,
                "events": [{"id": "fb-w0", "available_at": 100}]}]}

    def write(self, root, variant, repeat, segments, passed):
        name = f"A2-p-{variant}-{repeat}"
        (root / name).mkdir()
        (root / name / "result.json").write_text(json.dumps({"ledger": {"segments": segments}}))
        return {"directory": name, "pair_id": "p", "variant_id": variant, "cell_id": "A2",
                "lexical_screen": {"passed": passed}}

    def run_pair(self, feedback_segments, control_segments, feedback_pass):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            branches = [self.write(root, "deepen", 0, feedback_segments, feedback_pass),
                        self.write(root, "deepen-nofeedback", 0, control_segments, False)]
            return summary.paired_discrimination(root, self.pair(), branches)[0]

    def test_repeats_pair_with_their_own_control(self):
        # Repeat 0's control reaches the content; repeat 1's does not. Keying by
        # variant alone would let one control stand in for both.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fed = [self.segment("you feed it every day", 101, 105)]
            branches = [self.write(root, "deepen", 0, fed, True),
                        self.write(root, "deepen-nofeedback", 0, [self.segment("then feed it daily", 102, 106)], False),
                        self.write(root, "deepen", 1, fed, True),
                        self.write(root, "deepen-nofeedback", 1, [], False)]
            rows = summary.paired_discrimination(root, self.pair(), branches)
        self.assertEqual([(r["repeat"], r["verdict"]) for r in rows],
                         [("0", "non-discriminating"), ("1", "feedback-only-pass")])

    def segment(self, text, first, last):
        return {"text": text, "first_played_at": first, "last_played_at": last}

    def test_control_reaching_required_content_is_non_discriminating(self):
        # The control's segment is unfinished and starts inside the window:
        # completed-only screening misses it, the upper bound does not.
        row = self.run_pair([self.segment("you feed it every day", 101, 105)],
                            [self.segment("then feed the starter daily", 108, 130)], True)
        self.assertEqual(row["verdict"], "non-discriminating")
        self.assertFalse(row["control_completed_pass"])

    def test_feedback_only_and_outside_window(self):
        row = self.run_pair([self.segment("you feed it every day", 101, 105)],
                            [self.segment("then feed the starter daily", 111, 130),
                             self.segment("mix flour and water", 101, 109)], True)
        self.assertEqual(row["verdict"], "feedback-only-pass")
        self.assertEqual(self.run_pair([], [], False)["verdict"], "no-feedback-pass")

    def test_forbidden_and_word_boundaries_match_go_screen(self):
        self.assertFalse(summary.lexical_match(self.pair()["variants"][0]["expect"], "feed it in the oven"))
        self.assertFalse(summary.lexical_match(self.pair()["variants"][0]["expect"], "a feeder is not it"))
        self.assertTrue(summary.lexical_match(self.pair()["variants"][0]["expect"], "Feeding: twice, daily!"))

    def test_missing_control_is_incomplete(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            branches = [self.write(root, "deepen", 0, [], False)]
            rows = summary.paired_discrimination(root, self.pair(), branches)
        self.assertEqual(rows[0]["verdict"], "incomplete-pair")


if __name__ == "__main__":
    unittest.main()
