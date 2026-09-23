"""A stalled backend must not appear as a successful turn boundary."""
import threading
import base64
import types
import unittest
from lychee_fd_sidecar import LycheeSidecar


class TurnTests(unittest.TestCase):
    def test_received_audio_records_local_disposition(self):
        for muted, payload, interrupted, expected in [
                (True, b'\0\0', False, 'muted'),
                (False, b'', False, 'empty'),
                (False, b'\0\0', True, 'engine_interrupt'),
                (False, b'\0\0', False, 'forward')]:
            with self.subTest(disposition=expected):
                s = LycheeSidecar.__new__(LycheeSidecar)
                s._muted, s._turn_open = muted, True
                s._model_state = 'S'
                s._turn_audio_samples = 1
                s._lock = threading.RLock()
                s.session_label = 'test'
                s.stats = {'audio_chunks': 0}
                records, forwarded, cutoffs = [], [], []
                s.control = types.SimpleNamespace(event=lambda *a, **kw: records.append((a, kw)))
                s.interrupted = lambda: interrupted
                s._interrupt_turn = cutoffs.append
                s._open_turn = lambda cause: None
                s.audio = forwarded.append
                s._on_backend_event({'type': 'audio_chunk_pcm', 'round_id': 4,
                                     'pcm_b64': base64.b64encode(payload).decode()}, 123)
                self.assertEqual(records[0][1]['disposition'], expected)
                self.assertEqual(records[0][1]['pcm_bytes'], len(payload))
                self.assertEqual(records[0][1]['recv_ms'], 123)
                self.assertEqual(forwarded, [payload] if expected == 'forward' else [])
                self.assertEqual(cutoffs, ['engine_interrupt'] if expected == 'engine_interrupt' else [])

    def test_stall_emits_error_and_closes_turn_once(self):
        s = LycheeSidecar.__new__(LycheeSidecar)
        s._lock = threading.RLock()
        s._turn_open = True
        s._turn_text = 'partial answer'
        s._turn_audio_samples = 2400
        s._turn_started_ms = 0
        s._respond_waiter = None
        s.session_label = 'test'
        s.control = types.SimpleNamespace(event=lambda *args, **kwargs: None)
        events = []
        s.error = lambda text, **kw: events.append(('error', kw['code']))
        s.text_done = lambda text: events.append(('text', text))
        s.turn_done = lambda: events.append(('end', None))
        s._finish_turn('audio_stalled')
        s._finish_turn('audio_stalled')
        self.assertEqual(events, [('error', 'audio_stalled'), ('text', 'partial answer'), ('end', None)])


if __name__ == '__main__':
    unittest.main()
