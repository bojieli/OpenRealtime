import array
import os
import random
import unittest
from server import RNNoise, Sessions, Stream


class FakeLibrary:
    def rnnoise_process_frame(self, state, output, audio):
        return 1.0

    def rnnoise_destroy(self, state):
        pass


class FakeModel:
    lib = FakeLibrary()

    def state(self):
        return 1


class PacketTests(unittest.TestCase):
    def test_packet_boundaries_do_not_change_output(self):
        for rate in (16000, 24000, 48000):
            audio = array.array("h", (i % 1000 for i in range(rate))).tobytes()
            whole, split = Stream(FakeModel(), rate), Stream(FakeModel(), rate)
            expected = whole.process(audio)
            rng, pieces, offset = random.Random(7), [], 0
            while offset < len(audio):
                end = min(len(audio), offset + rng.randint(1, 500) * 2)
                pieces.append(split.process(audio[offset:end]))
                offset = end
            self.assertEqual(expected, b"".join(pieces))
            self.assertEqual(len(expected), len(audio))
            self.assertEqual(expected[: rate // 100 * 2], bytes(rate // 100 * 2))
            whole.close()
            split.close()

    def test_order_rate_and_concurrency_are_enforced(self):
        sessions = Sessions(FakeModel(), limit=1)
        stream = sessions.acquire("a", 24000, 0)
        with self.assertRaises(ValueError):
            sessions.acquire("a", 24000, 0)
        stream.sequence += 1
        stream.lock.release()
        for identity, rate, sequence in [
            ("a", 24000, 0),
            ("a", 16000, 1),
            ("b", 24000, 0),
        ]:
            with self.assertRaises(ValueError):
                sessions.acquire(identity, rate, sequence)
        sessions.delete("a")
        with self.assertRaises(ValueError):
            sessions.acquire("a", 24000, 1)

    def test_delete_during_processing_releases_model_after_request(self):
        sessions = Sessions(FakeModel(), limit=1)
        stream = sessions.acquire("a", 24000, 0)
        sessions.delete("a")
        self.assertIsNotNone(stream.state)
        with self.assertRaises(ValueError):
            sessions.acquire("a", 24000, 0)
        sessions.release("a", stream)
        self.assertIsNone(stream.state)
        self.assertNotIn("a", sessions.streams)

    @unittest.skipUnless(
        os.getenv("RNNOISE_LIBRARY"), "set RNNOISE_LIBRARY for real model test"
    )
    def test_real_model_packet_invariance_and_noise_attenuation(self):
        model = RNNoise(os.environ["RNNOISE_LIBRARY"])
        rng = random.Random(4)
        audio = array.array(
            "h", (rng.randint(-5000, 5000) for _ in range(24000 * 3))
        ).tobytes()
        whole, split = Stream(model, 24000), Stream(model, 24000)
        expected = b"".join(
            whole.process(audio[i : i + 4800]) for i in range(0, len(audio), 4800)
        )
        actual = b"".join(
            split.process(audio[i : i + 146]) for i in range(0, len(audio), 146)
        )
        self.assertEqual(expected, actual)
        original = array.array("h")
        original.frombytes(audio[24000:])
        filtered = array.array("h")
        filtered.frombytes(actual[24000:])
        self.assertLess(sum(x * x for x in filtered), sum(x * x for x in original) / 4)
        whole.close()
        split.close()


if __name__ == "__main__":
    unittest.main()
