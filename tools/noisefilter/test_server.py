import array
import collections
import math
import os
import random
import threading
import unittest
from server import DeepFilterNet, DeepFilterStream, RNNoise, Sessions, Stream


class FakeLibrary:
    def rnnoise_process_frame(self, state, output, audio):
        return 1.0

    def rnnoise_destroy(self, state):
        pass


class FakeModel:
    lib = FakeLibrary()

    def state(self):
        return 1


class FakeDeepFilterLibrary:
    def df_process_frame(self, state, output, audio):
        return 0.0

    def df_free(self, state):
        pass


class FakeDeepFilterModel:
    lib = FakeDeepFilterLibrary()

    def state(self):
        return 1


class CountingDeepFilterNet(DeepFilterNet):
    """Pool logic of DeepFilterNet without the shared library."""

    def __init__(self, pool):
        self.lib = FakeDeepFilterLibrary()
        self.created = 0
        self.pool_size = pool
        self.ready = collections.deque()
        self.pool_lock = threading.Condition()
        self.closed = False
        threading.Thread(target=self._refill, daemon=True).start()

    def create(self):
        self.created += 1  # only the refill thread and state() on an empty pool call this
        return self.created


def pcm(values):
    return array.array("h", values).tobytes()


def samples(data):
    result = array.array("h")
    result.frombytes(data)
    return result


def lag_of(reference, output, max_lag):
    best, best_lag = -1.0, 0
    for lag in range(max_lag):
        score = sum(a * b for a, b in zip(reference, output[lag:]))
        if score > best:
            best, best_lag = score, lag
    return best_lag


def voiced(rate, seconds, seed=3):
    """Speech-like test signal: a gliding harmonic voice with syllable-rate
    amplitude modulation (a stationary tone would be treated as noise)."""
    rng = random.Random(seed)
    out, phase = [], 0.0
    for n in range(int(rate * seconds)):
        t = n / rate
        f0 = 140 + 40 * math.sin(2 * math.pi * 0.7 * t)
        phase += 2 * math.pi * f0 / rate
        envelope = max(0.0, math.sin(2 * math.pi * 3.0 * t)) ** 2
        value = sum(math.sin(k * phase) / k for k in range(1, 12)) * envelope
        out.append(int(6000 * value + rng.randint(-40, 40)))
    return out


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

    def test_deepfilter_stream_is_packet_invariant_and_scales_losslessly(self):
        for rate in (16000, 24000, 48000):
            audio = pcm((i * 37) % 20000 - 10000 for i in range(rate))
            whole = DeepFilterStream(FakeDeepFilterModel(), rate)
            split = DeepFilterStream(FakeDeepFilterModel(), rate)
            expected = whole.process(audio)
            rng, pieces, offset = random.Random(9), [], 0
            while offset < len(audio):
                end = min(len(audio), offset + rng.randint(1, 500) * 2)
                pieces.append(split.process(audio[offset:end]))
                offset = end
            self.assertEqual(expected, b"".join(pieces))
            self.assertEqual(len(expected), len(audio))
            self.assertEqual(expected[: rate // 100 * 2], bytes(rate // 100 * 2))
        # At 48 kHz an identity frame function returns the input 10 ms (FIFO) later,
        # so the [-1, 1] scaling round trip is exact.
        audio = pcm((i * 37) % 20000 - 10000 for i in range(4800))
        out = DeepFilterStream(FakeDeepFilterModel(), 48000).process(audio)
        self.assertEqual(samples(out)[480:], samples(audio)[:-480])

    def test_deepfilter_pool_hands_out_fresh_states(self):
        model = CountingDeepFilterNet(pool=2)
        self.assertTrue(model.wait_ready(5))
        states = [model.state() for _ in range(5)]
        self.assertEqual(len(set(states)), 5)
        self.assertTrue(model.wait_ready(5))
        with model.pool_lock:
            model.closed = True
            model.pool_lock.notify_all()

    def test_deepfilter_sessions_use_the_deepfilter_stream(self):
        sessions = Sessions(FakeDeepFilterModel(), limit=1, stream_factory=DeepFilterStream)
        stream = sessions.acquire("a", 16000, 0)
        self.assertIsInstance(stream, DeepFilterStream)
        sessions.release("a", stream)
        sessions.delete("a")

    @unittest.skipUnless(
        os.getenv("DEEPFILTER_LIBRARY") and os.getenv("DEEPFILTER_MODEL"),
        "set DEEPFILTER_LIBRARY (libdf.so) and DEEPFILTER_MODEL (DeepFilterNet3_onnx.tar.gz) "
        "for real model tests; see tools/noisefilter/build_deepfilter.sh",
    )
    def test_real_deepfilter_invariance_attenuation_and_declared_delay(self):
        model = DeepFilterNet(
            os.environ["DEEPFILTER_LIBRARY"], os.environ["DEEPFILTER_MODEL"], pool=0
        )
        rng = random.Random(4)
        noise = pcm(rng.randint(-5000, 5000) for _ in range(24000 * 3))
        whole, split = DeepFilterStream(model, 24000), DeepFilterStream(model, 24000)
        expected = b"".join(
            whole.process(noise[i : i + 4800]) for i in range(0, len(noise), 4800)
        )
        actual = b"".join(
            split.process(noise[i : i + 146]) for i in range(0, len(noise), 146)
        )
        self.assertEqual(expected, actual)
        original, filtered = samples(noise[48000:]), samples(actual[48000:])
        self.assertLess(sum(x * x for x in filtered), sum(x * x for x in original) / 4)
        whole.close()
        split.close()
        # The declared waveform delay holds at every supported rate.
        for rate in (16000, 24000, 48000):
            voice = voiced(rate, 2.0)
            stream = DeepFilterStream(model, rate)
            out = samples(b"".join(
                stream.process(pcm(voice[i : i + rate // 10]))
                for i in range(0, len(voice), rate // 10)
            ))
            stream.close()
            start, span = rate // 2, rate // 2
            lag = lag_of(voice[start : start + span], out[start:], rate // 10)
            self.assertAlmostEqual(
                lag / rate * 1000, DeepFilterNet.audio_delay_ms, delta=1000 / rate + 0.5
            )

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
