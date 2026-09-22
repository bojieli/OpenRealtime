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


if __name__ == '__main__':
    unittest.main()
