import importlib.util
from pathlib import Path
import unittest
import struct
import json
import tempfile
import wave

spec = importlib.util.spec_from_file_location("pack_native", Path(__file__).with_name("package_native_live.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ReceiptAuditTest(unittest.TestCase):
    def test_feedback_opportunity_clips_receipt_and_excludes_onset_sample(self):
        pcm = struct.pack("<6h", 0, 0, 12, -20, 99, 0)
        marks = [{"kind": "played", "start_sample": 2, "samples": 4}]
        report = module.feedback_opportunity(pcm, 1000, marks, 4_000_000)
        self.assertEqual(report["nonzero_samples_before_feedback"], 2)
        self.assertEqual(report["pre_feedback_peak_pcm"], 20)
        self.assertEqual(report["pre_feedback_receipt_samples"], 2)
        self.assertTrue(report["receipt_spans_feedback_onset"])
        self.assertIsNone(report["substantive_speech_underway"])

    def test_silent_playback_and_late_audio_do_not_prove_speech(self):
        marks = [{"kind": "played", "start_sample": 0, "samples": 4}]
        report = module.feedback_opportunity(bytes(8), 1000, marks, 4_000_000)
        self.assertEqual(report["pre_feedback_receipt_samples"], 4)
        self.assertEqual(report["nonzero_samples_before_feedback"], 0)
        self.assertFalse(report["receipt_spans_feedback_onset"])
        late = module.feedback_opportunity(struct.pack("<6h", 0, 0, 0, 0, 99, 99),
            1000, [{"kind": "played", "start_sample": 4, "samples": 2}], 4_000_000)
        self.assertEqual(late["pre_feedback_receipt_samples"], 0)
        self.assertEqual(late["nonzero_samples_before_feedback"], 0)

    def test_rejects_unheard_and_post_cancel_receipts(self):
        generated = {"kind": "generated", "epoch": 0, "samples": 20}
        played = {"kind": "played", "epoch": 0, "start_sample": 0,
                  "samples": 20, "acknowledged_at_s": .02}
        self.assertEqual(module.audit([generated, played], 20, 1000)["violations"], [])
        self.assertTrue(module.audit([played], 20, 1000)["violations"])
        self.assertTrue(module.audit([generated, {"kind": "cancel", "epoch": 0}, played], 20, 1000)["violations"])
        self.assertTrue(module.audit([generated, played], 10, 1000)["violations"])
        self.assertTrue(module.audit([generated, dict(played, acknowledged_at_s=.01)], 20, 1000)["violations"])

    def test_backlog_excludes_already_played_and_resets_by_epoch(self):
        marks = [{"kind": "generated", "epoch": 0, "samples": 100},
                 {"kind": "played", "epoch": 0, "start_sample": 0,
                  "samples": 20, "acknowledged_at_s": .02},
                 {"kind": "cancel", "epoch": 0, "at_s": .03, "reason": "interruption"},
                 {"kind": "generated", "epoch": 1, "samples": 40}]
        report = module.audit(marks, 20, 1000)
        self.assertEqual(report["violations"], [])
        self.assertEqual(report["peak_generated_audio_backlog_seconds"], .1)
        self.assertEqual(report["generated_audio_seconds"], .14)
        self.assertEqual(report["cancellation_backlogs"][0]["unplayed_generated_samples"], 80)


class PackageCompletionTest(unittest.TestCase):
    def test_retains_failed_status_and_missing_control(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "run"
            root.mkdir()
            prepared = Path(temp) / "prepared"
            prepared.mkdir()
            trial = root / "corrected"
            trial.mkdir()
            pair = {"variants": [{"id": "corrected", "feedback": "feedback",
                    "events": [{"kind": "sound", "group": "feedback",
                                "source_start": 0, "source_end": 10_000_000}]}]}
            (root / "prepared-pair.json").write_text(json.dumps(pair))
            (prepared / "pair.json").write_text(json.dumps(pair))
            for path in (prepared / "corrected.input.wav", trial / "output.wav"):
                with wave.open(str(path), "wb") as wav:
                    wav.setparams((1, 2, 1000, 0, "NONE", "not compressed"))
                    wav.writeframes(bytes(20))
            (trial / "playback.json").write_text("[]")
            (trial / "result.json").write_text(json.dumps({"status": "failed", "error": "canceled"}))
            out = Path(temp) / "listening"
            module.package(root, prepared, out)
            report = json.loads((out / "audit.json").read_text())
            self.assertFalse(report["execution_complete"])
            self.assertEqual(report["missing_branches"], ["corrected-nofeedback"])
            self.assertEqual(report["branches"][0]["status"], "failed")
            self.assertEqual(report["branches"][0]["execution_error"], "canceled")
            self.assertIsNone(report["capability_claim"])
            self.assertTrue(report["prepared_fixture_matches_run"])
            pair["variants"][0]["events"][0]["source_start"] = 1
            (prepared / "pair.json").write_text(json.dumps(pair))
            mismatched = Path(temp) / "mismatched"
            with self.assertRaisesRegex(ValueError, "differs from run-retained"):
                module.package(root, prepared, mismatched)
            self.assertFalse(mismatched.exists())
