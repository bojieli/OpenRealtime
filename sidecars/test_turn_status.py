"""Interrupted turns must be reported as interrupted, and only those turns."""
import io
import json
import threading
import types
import unittest

from moshi_sidecar import MoshiSidecar
from lychee_fd_sidecar import LycheeSidecar
from openrealtime_sidecar import Sidecar


def turn_statuses(stream):
    frames = [json.loads(line) for line in stream.getvalue().splitlines() if line.startswith(b"{")]
    return [frame.get("turn_status") for frame in frames if frame["type"] == "turn_done"]


class TurnStatusTests(unittest.TestCase):
    def test_mark_applies_to_the_next_turn_only(self):
        out = io.BytesIO()
        s = Sidecar(io.BytesIO(), out)
        s.turn_done()
        s.mark_turn_interrupted()
        s.turn_done()
        s.turn_done()
        s.turn_done(interrupted=True)
        self.assertEqual(turn_statuses(out), [None, "interrupted", None, "interrupted"])

    def test_moshi_interrupt_ends_the_turn_interrupted(self):
        for mute, expected in ((False, None), (True, "interrupted")):
            out = io.BytesIO()
            s = MoshiSidecar(io.BytesIO(), out, repository='test', mock=False, device='cpu')
            s._speaking, s._turn_text = True, ["Hello"]
            s._end_turn(mute=mute)
            self.assertEqual(turn_statuses(out), [expected], f"mute={mute}")

    def test_lychee_interrupt_reasons_end_the_turn_interrupted(self):
        for reason, expected in (("backend_done", None), ("yielded", None),
                                 ("model_interrupt", "interrupted"), ("engine_interrupt", "interrupted")):
            out = io.BytesIO()
            s = LycheeSidecar.__new__(LycheeSidecar)
            Sidecar.__init__(s, io.BytesIO(), out)
            s._lock = threading.RLock()
            s._turn_open, s._turn_text, s._turn_audio_samples = True, "", 0
            s._turn_started_ms, s._respond_waiter = 0, None
            s.session_label = 'test'
            s.control = types.SimpleNamespace(event=lambda *a, **k: None)
            s._finish_turn(reason)
            self.assertEqual(turn_statuses(out), [expected], reason)


if __name__ == '__main__':
    unittest.main()
