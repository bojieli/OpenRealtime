#!/usr/bin/env python3

from __future__ import annotations

import importlib.util
from pathlib import Path
import tempfile
import unittest


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
