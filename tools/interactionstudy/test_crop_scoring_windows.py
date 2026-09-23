import importlib.util
from pathlib import Path
import struct
import json
import tempfile
import wave
import unittest

spec = importlib.util.spec_from_file_location("crop_windows", Path(__file__).with_name("crop_scoring_windows.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class CropTest(unittest.TestCase):
    def test_crop_keeps_original_positions_and_pads_only_missing_tail(self):
        pcm = struct.pack("<hhhh", 10, 20, 30, 40)
        result = module.crop(pcm, 2, 500_000_000, 3_000_000_000)
        self.assertEqual(struct.unpack("<hhhhh", result), (20, 30, 40, 0, 0))
        self.assertEqual(module.crop(pcm, 2, 3_000_000_000, 4_000_000_000), bytes(4))

    def test_invalid_interval_is_rejected(self):
        with self.assertRaises(ValueError):
            module.crop(b"", 24000, 2, 1)

    def test_go_control_uses_original_identity_and_window(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            variant = {"id": "skip", "events": [{"id": "feedback", "available_at": 500_000_000}],
                       "expect": {"after": "feedback", "within": 1_000_000_000}}
            (root / "fixtures.json").write_text(json.dumps([{"id": "pair", "prefix": [], "variants": [variant]}]))
            trial = root / "arbitrary-directory-name"
            trial.mkdir()
            (trial / "actions.jsonl").write_text(json.dumps({"pair_id": "pair", "variant_id": "skip-nofeedback", "cell_id": "A2"}) + "\n")
            (trial / "result.json").write_text('{"status":"failed"}')
            with wave.open(str(trial / "output.wav"), "wb") as wav:
                wav.setparams((1, 2, 2, 0, "NONE", "not compressed"))
                wav.writeframes(struct.pack("<hhhh", 10, 20, 30, 40))
            out = root / "windows"
            module.package_go(root, out)
            manifest = json.loads((out / "manifest.json").read_text())
            self.assertEqual(manifest[0]['variant_id'], 'skip-nofeedback')
            self.assertEqual(manifest[0]['status'], 'failed')
            with wave.open(str(out / 'arbitrary-directory-name.wav')) as wav:
                self.assertEqual(struct.unpack('<hh', wav.readframes(2)), (20, 30))
            with self.assertRaises(FileExistsError):
                module.package_go(root, out)

    def test_silent_branch_crops_to_silence_only_when_nothing_played(self):
        for played in (0, 5):
            with self.subTest(played=played), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                variant = {"id": "quiet", "events": [{"id": "feedback", "available_at": 500_000_000}],
                           "expect": {"after": "feedback", "within": 1_000_000_000}}
                (root / "fixtures.json").write_text(json.dumps([{"id": "pair", "prefix": [], "variants": [variant]}]))
                trial = root / "trial"
                trial.mkdir()
                (trial / "actions.jsonl").write_text(json.dumps({"pair_id": "pair", "variant_id": "quiet", "cell_id": "A2"}) + "\n")
                (trial / "result.json").write_text(json.dumps({"status": "complete", "ledger": {"segments": [{"played_samples": played}]}}))
                with wave.open(str(trial / "input.wav"), "wb") as wav:
                    wav.setparams((1, 2, 4, 0, "NONE", "not compressed"))
                    wav.writeframes(b"\0\0" * 8)
                if played:
                    with self.assertRaisesRegex(ValueError, "missing although the ledger"):
                        module.package_go(root, root / "windows")
                    continue
                module.package_go(root, root / "windows")
                with wave.open(str(root / "windows" / "trial.wav")) as wav:
                    self.assertEqual((wav.getframerate(), wav.readframes(10)), (4, b"\0\0" * 4))
                manifest = json.loads((root / "windows" / "manifest.json").read_text())
                self.assertIn("no played audio", manifest[0]["source_sha256"])
