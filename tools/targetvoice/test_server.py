"""Enrollment and stream invariants; no model download required."""

import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import patch
import numpy as np
import torch
from tools.targetvoice.server import Stream, Resampler


class Engine:
    def __init__(self, speech=True):
        self.audio_worker = ThreadPoolExecutor(max_workers=1)
        self.reference_worker = ThreadPoolExecutor(max_workers=1)
        self.model = self
        self.speech = speech
        self.entered = threading.Event()
        self.release = threading.Event()
        self.reference = None
        self.calls = 0

    def take_vad(self):
        return lambda *_: torch.tensor(float(self.speech))

    def return_vad(self, _):
        pass

    def enroll(self, reference):
        self.calls += 1
        self.reference = reference.copy()
        self.entered.set()
        self.release.wait(5)
        return "fixed-reference"

    def close(self):
        self.release.set()
        self.audio_worker.shutdown()
        self.reference_worker.shutdown()


class FakeExtractor:
    def __init__(self, _, cue):
        assert cue == "fixed-reference"

    def process(self, block):
        return block * 0.25


class Tests(unittest.TestCase):
    def test_reference_is_three_seconds_once_and_does_not_block_audio(self):
        engine = Engine()
        stream = Stream(engine, 16000)
        reference = np.arange(48000, dtype=np.int16)
        try:
            # Capture packet crossing the reference boundary; its extra samples
            # must not enter the reference. PCM forwarding proceeds while the
            # encoder is deliberately blocked.
            data = np.concatenate([reference, np.ones(512, dtype=np.int16)])
            for start in range(0, len(data), 503):
                body = data[start : start + 503].astype("<i2").tobytes()
                self.assertEqual(len(stream.process(body)), len(body))
            self.assertTrue(engine.entered.wait(1))
            self.assertEqual(stream.phase, "preparing-reference")
            np.testing.assert_array_equal(
                engine.reference, reference.astype(np.float32) / 32768
            )
            self.assertEqual(len(stream.process(bytes(1024))), 1024)
            engine.release.set()
            stream.reference_future.result(timeout=1)
            with patch("tools.targetvoice.server.Extractor", FakeExtractor):
                stream.process(bytes(1024))
                self.assertEqual(stream.phase, "extracting")
                for _ in range(110):
                    stream.process(bytes(1024))
            self.assertEqual(engine.calls, 1)
            np.testing.assert_array_equal(
                engine.reference, reference.astype(np.float32) / 32768
            )
        finally:
            stream.close()
            engine.close()

    def test_silence_does_not_enroll_and_fifo_is_packet_independent(self):
        data = np.arange(8000, dtype=np.int16).tobytes()
        results = []
        for size in [2, 74, 640, 3200]:
            engine = Engine(speech=False)
            stream = Stream(engine, 16000)
            try:
                results.append(
                    b"".join(
                        stream.process(data[i : i + size])
                        for i in range(0, len(data), size)
                    )
                )
                self.assertEqual(engine.calls, 0)
                self.assertEqual(stream.phase, "waiting-for-speech")
            finally:
                stream.close()
                engine.close()
        for result in results:
            self.assertEqual(result, bytes(2048) + data[:-2048])

    def test_resampling_state_survives_packets(self):
        x = np.random.default_rng(1).normal(size=12345).astype(np.float32)
        for source, target in [
            (24000, 16000),
            (16000, 24000),
            (48000, 16000),
            (16000, 48000),
        ]:
            whole = Resampler(source, target).process(x)
            split = Resampler(source, target)
            parts = np.concatenate(
                [split.process(x[i : i + 137]) for i in range(0, len(x), 137)]
            )
            np.testing.assert_array_equal(whole, parts)


if __name__ == "__main__":
    unittest.main()
