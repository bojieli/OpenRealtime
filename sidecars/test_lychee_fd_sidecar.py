"""A stalled backend must not appear as a successful turn boundary."""
import threading
import types
import unittest
from lychee_fd_sidecar import LycheeSidecar


class TurnTests(unittest.TestCase):
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
