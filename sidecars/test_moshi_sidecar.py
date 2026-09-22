"""Cancellation arriving while a native model step is in flight."""
import io
import unittest
import numpy as np
from moshi_sidecar import MoshiSidecar, FRAME_SAMPLES


class CancellationTests(unittest.TestCase):
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
