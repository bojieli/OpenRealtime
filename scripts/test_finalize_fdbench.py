#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import importlib.util
from pathlib import Path
import shutil
import tempfile
import unittest
import wave


SCRIPT = Path(__file__).with_name("finalize-fdbench.py")
SPEC = importlib.util.spec_from_file_location("finalize_fdbench", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
FINALIZE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FINALIZE)


class EvidenceTreeTest(unittest.TestCase):
    def test_is_deterministic_and_commits_to_path_size_and_hash(self) -> None:
        entries = [("b.wav", 2, "b" * 64), ("a.json", 1, "a" * 64)]
        forward = FINALIZE.evidence_tree(entries)
        reverse = FINALIZE.evidence_tree(list(reversed(entries)))
        self.assertEqual(forward, reverse)
        self.assertEqual(forward["files"], 2)
        self.assertEqual(forward["bytes"], 3)
        self.assertEqual(len(forward["digest"]), 64)
        changed = FINALIZE.evidence_tree(
            [("b.wav", 3, "b" * 64), ("a.json", 1, "a" * 64)]
        )
        self.assertNotEqual(forward["digest"], changed["digest"])

    def test_atomic_json_is_included_after_publication(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "result.json"
            FINALIZE.atomic_json(path, {"status": "completed"})
            evidence = FINALIZE.evidence_tree(
                [(path.name, path.stat().st_size, FINALIZE.sha256(path))]
            )
            self.assertEqual(evidence["files"], 1)
            self.assertGreater(evidence["bytes"], 0)


if __name__ == "__main__":
    unittest.main()


class ValidateResultTest(unittest.TestCase):
    """Drive validate_result over a complete result, then remove one fact."""

    def setUp(self) -> None:
        # Mirror the layout the runner writes -- <root>/<cell>/results/<name>.json
        # beside <root>/<cell>/audio/ -- because validate_result now reconciles a
        # result's declared identity against the path it was discovered at.
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.directory, ignore_errors=True)
        self.output_root = self.directory / "output"
        self.cell = "cell-a"
        self.results_root = self.output_root / self.cell / "results"
        self.results_root.mkdir(parents=True)
        audio_root = self.output_root / self.cell / "audio"
        audio_root.mkdir(parents=True)
        self.audio = audio_root / "conversation_1.wav"
        with wave.open(str(self.audio), "wb") as sink:
            sink.setnchannels(1)
            sink.setsampwidth(2)
            sink.setframerate(16000)
            sink.writeframes(b"\x00\x01" * 160)
        self.result_path = self.results_root / "conversation_1.json"

    def result(self, **overrides) -> dict:
        record = {
            "schema_version": "1.0.0",
            "benchmark": "FD-Bench",
            "revision": FINALIZE.BENCHMARK_REVISION,
            "dataset_revision": FINALIZE.DATASET_REVISION,
            "status": "completed",
            "output_wav": str(self.audio),
            "output_sha256": hashlib.sha256(self.audio.read_bytes()).hexdigest(),
            "sample": {
                "cell": "cell-a",
                "conversation": 1,
                "input_segments": [{"start": 0, "end": 10}],
            },
        }
        record.update(overrides)
        return record

    def validate(self, record: dict, result_path: Path | None = None) -> Path:
        return FINALIZE.validate_result(
            result_path or self.result_path, record, self.output_root
        )

    def test_accepts_a_complete_result(self) -> None:
        self.assertEqual(self.validate(self.result()), self.audio)

    def refuses(
        self, record: dict, message: str, result_path: Path | None = None
    ) -> None:
        with self.assertRaises(ValueError) as caught:
            self.validate(record, result_path)
        self.assertIn(message, str(caught.exception))

    def test_refuses_a_result_that_recorded_no_output_path(self) -> None:
        record = self.result()
        del record["output_wav"]
        self.refuses(record, "recorded no output audio path")

    def test_refuses_a_result_that_recorded_no_sample(self) -> None:
        record = self.result()
        del record["sample"]
        self.refuses(record, "recorded no sample")

    def test_refuses_a_sample_that_recorded_no_cell(self) -> None:
        record = self.result()
        del record["sample"]["cell"]
        self.refuses(record, "sample recorded no cell")

    def test_refuses_a_sample_that_recorded_no_input_segments(self) -> None:
        record = self.result()
        del record["sample"]["input_segments"]
        self.refuses(record, "sample recorded no input segments")

    def test_refuses_a_sample_that_recorded_no_conversation_number(self) -> None:
        record = self.result()
        del record["sample"]["conversation"]
        self.refuses(record, "sample recorded no conversation number")

    def test_refuses_a_sample_whose_conversation_number_is_not_a_number(self) -> None:
        record = self.result()
        record["sample"]["conversation"] = "first"
        self.refuses(record, "sample recorded no conversation number")

    def test_refuses_a_result_that_recorded_no_output_hash(self) -> None:
        record = self.result()
        del record["output_sha256"]
        self.refuses(record, "output audio identity does not match")

    def test_refuses_an_incomplete_result_before_reading_its_sample(self) -> None:
        record = self.result(status="running")
        del record["sample"]
        self.refuses(record, "is not a completed pinned FD-Bench result")

    def test_refuses_a_result_filed_under_a_cell_it_does_not_declare(self) -> None:
        # The finalizer groups by `sample.cell` and the runner files by
        # directory. When the two disagree, the result joins another
        # condition's trace and is scored as that condition's evidence. Two
        # such results crossing leaves every per-cell population intact, so no
        # count anywhere records that the conditions traded audio.
        other = self.output_root / "cell-b" / "results"
        other.mkdir(parents=True)
        self.refuses(
            self.result(),
            "declares cell cell-a but was written under cell-b",
            other / "conversation_1.json",
        )

    def test_refuses_a_result_whose_conversation_number_moved(self) -> None:
        # The trace line names conversation_<declared>.wav. If the declared
        # number is not the one the file was written as, the line points at
        # audio belonging to a different sample -- and upstream scores this
        # audio against that sample's ground truth.
        record = self.result()
        record["sample"]["conversation"] = 7
        self.refuses(record, "declares conversation 7 but was written as conversation_1")

    def test_refuses_a_result_that_does_not_sit_below_the_output_root(self) -> None:
        nested = self.output_root / "extra" / self.cell / "results"
        nested.mkdir(parents=True)
        self.refuses(
            self.result(),
            "does not sit directly below the output root",
            nested / "conversation_1.json",
        )
