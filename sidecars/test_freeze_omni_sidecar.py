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


class ShutdownTests(unittest.TestCase):
    def test_session_state_outlives_slow_model_workers(self):
        import io
        from types import SimpleNamespace
        from freeze_omni_sidecar import FreezeOmniSidecar

        # Accelerate only the old timed-join path: a timed-out worker remains
        # live just as it would after a slow GPU call exceeds 5 or 10 seconds.
        class SlowWorker(threading.Thread):
            def join(self, timeout=None):
                return super().join(None if timeout is None else .01)

        for kind in ('_listener', '_generation'):
            with self.subTest(worker=kind):
                release, entered, closed = (threading.Event() for _ in range(3))
                cleared = []
                engine = SimpleNamespace(torch=SimpleNamespace(cuda=SimpleNamespace(
                    empty_cache=lambda: cleared.append(True))))
                sidecar = FreezeOmniSidecar(io.BytesIO(), io.BytesIO(), engine=engine, pace=False)
                state = {'still_owned': True}
                sidecar.generate_outputs = state
                sidecar.system_role = state
                def work():
                    entered.set()
                    release.wait(2)
                worker = SlowWorker(target=work)
                setattr(sidecar, kind, worker)
                worker.start()
                def close():
                    sidecar.on_close()
                    closed.set()
                closer = threading.Thread(target=close)
                try:
                    self.assertTrue(entered.wait(1))
                    closer.start()
                    returned_early = closed.wait(.05)
                    retained_state = sidecar.generate_outputs is state
                    cleared_early = bool(cleared)
                finally:
                    release.set()
                    closer.join(2)
                    worker.join(2)
                self.assertFalse(returned_early, 'session released while model worker is live')
                self.assertTrue(retained_state)
                self.assertFalse(cleared_early)
                self.assertTrue(closed.is_set())
                self.assertIsNone(sidecar.generate_outputs)
                self.assertEqual(cleared, [True])


if __name__ == '__main__':
    unittest.main()
