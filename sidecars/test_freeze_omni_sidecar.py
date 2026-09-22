"""Ordering of local output cutoff and an in-flight transport write."""
import threading
import unittest
from freeze_omni_sidecar import OutputPacer


class PacerTests(unittest.TestCase):
    def test_drop_returns_only_after_admitted_write_finishes(self):
        writing, release, dropping, dropped = (threading.Event() for _ in range(4))
        events = []
        def send(kind, payload, **header):
            writing.set()
            if not release.wait(2):
                raise RuntimeError('test writer timed out')
            events.append(kind)
        pacer = OutputPacer(send, 24000, frame_ms=40, lead_ms=0)
        pacer.put('output_audio', bytes(24000 * 2), {})
        def drop():
            dropping.set()
            pacer.drop_audio()
            events.append('cutoff')
            dropped.set()
        worker = threading.Thread(target=drop)
        try:
            self.assertTrue(writing.wait(2))
            worker.start()
            self.assertTrue(dropping.wait(2))
            premature = dropped.wait(.05)
        finally:
            release.set()
            if worker.ident is not None:
                worker.join(2)
            pacer.close(drain_timeout=0)
        self.assertFalse(premature, 'cutoff returned while admitted write was pending')
        self.assertEqual(events[-1], 'cutoff')
        self.assertGreater(pacer.dropped_samples, 0)


if __name__ == '__main__':
    unittest.main()
