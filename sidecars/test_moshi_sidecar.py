"""Cancellation arriving while a native model step is in flight."""
import io
import unittest
import threading
from types import SimpleNamespace
import numpy as np
from moshi_sidecar import MoshiSidecar, FRAME_SAMPLES


class CancellationTests(unittest.TestCase):
    def test_failed_reset_releases_session_lock(self):
        def fail():
            raise ValueError('reset failed')
        model = SimpleNamespace(lock=threading.Lock(), reset=fail)
        s = MoshiSidecar(io.BytesIO(), io.BytesIO(), repository='test', mock=False,
                        device='cpu', shared=model)
        s.input_rate, s.instructions = 24000, ''
        with self.assertRaisesRegex(ValueError, 'reset failed'):
            s.configure(None)
        self.assertFalse(model.lock.locked())
        self.assertFalse(s._holds_model)
        self.assertIsNone(s._model)

    def test_shutdown_keeps_model_owned_until_worker_exits(self):
        class SlowWorker(threading.Thread):
            def join(self, timeout=None):
                return super().join(None if timeout is None else .01)

        s = MoshiSidecar(io.BytesIO(), io.BytesIO(), repository='test', mock=False, device='cpu')
        release, entered, closed = (threading.Event() for _ in range(3))
        lock = threading.Lock()
        lock.acquire()
        model = SimpleNamespace(lock=lock)
        s._model, s._holds_model = model, True
        s._report_stats = lambda: None
        def work():
            entered.set()
            release.wait(2)
        s._stream_thread = SlowWorker(target=work)
        s._stream_thread.start()
        def close():
            s.on_close()
            closed.set()
        closer = threading.Thread(target=close)
        try:
            self.assertTrue(entered.wait(1))
            closer.start()
            self.assertFalse(closed.wait(.05))
            self.assertTrue(lock.locked())
            self.assertIs(s._model, model)
        finally:
            release.set()
            closer.join(2)
            s._stream_thread.join(2)
        self.assertTrue(closed.is_set())
        self.assertFalse(lock.locked())
        self.assertIsNone(s._model)

    def test_interrupt_during_inference_mutes_inflight_output(self):
        s = MoshiSidecar(io.BytesIO(), io.BytesIO(), repository='test', mock=False, device='cpu')
        s._speaking = True
        s._turn_text = ['already spoken']
        s._frames.put(np.zeros(FRAME_SAMPLES, dtype=np.float32))
        events = []
        class Model:
            def step(self, samples):
                s._interrupted.set()
                s._stop.set()  # one controlled iteration
                return 'stale', np.ones(FRAME_SAMPLES, dtype=np.float32) * .2
        s._model = Model()
        s.text_delta = lambda text: events.append(('delta', text))
        s.audio = lambda pcm: events.append(('audio', len(pcm)))
        s.text_done = lambda text: events.append(('text_done', text))
        s.turn_done = lambda: events.append(('end', None))
        s._stream()
        self.assertEqual(events, [('text_done', 'already spoken'), ('end', None)])
        self.assertTrue(s._muted)
        self.assertEqual(s._stats['interrupts'], 1)


if __name__ == '__main__':
    unittest.main()
