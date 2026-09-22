"""Response cancellation bookkeeping without loading VoiceChat weights."""
import threading
import unittest

from voicechat_sidecar import VoiceChatSidecar


class CancellationTests(unittest.TestCase):
    def make_sidecar(self, gate):
        sidecar = VoiceChatSidecar.__new__(VoiceChatSidecar)
        sidecar._state_lock = threading.Lock()
        sidecar._turn_finished = threading.Condition(sidecar._state_lock)
        sidecar._gated_response = gate
        sidecar._response_text = []
        sidecar._response_active = False
        sidecar._current_response = None
        sidecar._turns_done = 0
        sidecar.turn_done = lambda: self.fail('cancelled turn emitted a duplicate boundary')
        return sidecar

    def test_unknown_response_id_mutes_later_identified_packets(self):
        sidecar = self.make_sidecar('*')
        self.assertTrue(sidecar._gated('resp-later'))
        self.assertTrue(sidecar._gated(''))
        sidecar._finish_turn('resp-later', {})
        self.assertIsNone(sidecar._gated_response)
        self.assertFalse(sidecar._gated('resp-next'))

    def test_named_gate_does_not_mute_another_response(self):
        sidecar = self.make_sidecar('resp-old')
        self.assertTrue(sidecar._gated('resp-old'))
        self.assertFalse(sidecar._gated('resp-next'))
        sidecar._finish_turn('resp-old', {})
        self.assertFalse(sidecar._gated('resp-next'))

class SegmentationTests(unittest.TestCase):
    def test_idle_silence_and_short_pause_do_not_end_speech(self):
        import numpy as np
        from voicechat_sidecar import MODEL_OUTPUT_RATE
        s = CancellationTests().make_sidecar(None)
        s.output_quiet_ms = 800
        s._output_audible = False
        s._output_quiet_samples = 0
        s._response_text = ['hello']
        events = []
        s.audio = lambda p: events.append('audio')
        s.send = lambda *a, **k: None
        s.text_done = lambda t: events.append(t)
        s.turn_done = lambda: events.append('end')
        quiet = np.zeros(int(MODEL_OUTPUT_RATE * .08), dtype='<i2').tobytes()
        loud = (np.ones(int(MODEL_OUTPUT_RATE * .08)) * 4000).astype('<i2').tobytes()
        s._segmented_audio(quiet)
        self.assertEqual(events, [])
        s._segmented_audio(loud)
        for _ in range(7): s._segmented_audio(quiet)
        self.assertNotIn('end', events)
        s._segmented_audio(loud)  # short pause is reset by more speech
        for _ in range(10): s._segmented_audio(quiet)
        self.assertEqual(events[-2:], ['hello', 'end'])
        count = len(events)
        s._segmented_audio(quiet)
        self.assertEqual(len(events), count)

    def test_upstream_boundary_resets_silence_tracking(self):
        s = CancellationTests().make_sidecar(None)
        s._output_audible = True
        s._output_quiet_samples = 100
        s._finish_turn('response', {})
        self.assertFalse(s._output_audible)
        self.assertEqual(s._output_quiet_samples, 0)


if __name__ == '__main__':
    unittest.main()
