import io
import unittest
from types import SimpleNamespace
from duplexcascade_sidecar import NativeCascade
from duplexcascade_playout import PacedAudio


class ProtocolTests(unittest.TestCase):
    def sidecar(self):
        result = NativeCascade(io.BytesIO(),io.BytesIO(),args=None,backend=None)
        result.messages=[]
        result.send=lambda kind,payload=b'',**fields:result.messages.append((kind,payload,fields))
        result.player=PacedAudio(result.audio)
        result.speech=SimpleNamespace(context=SimpleNamespace(sample_rate=24000))
        return result

    def test_model_cancel_discards_queued_audio_and_ends_once(self):
        sidecar=self.sidecar()
        sidecar._text('Paris')
        sidecar._audio(b'\x01\x00'*2400)
        sidecar._cancel()
        sidecar._cancel()
        self.assertEqual(len(sidecar.player.buffer),0)
        self.assertEqual(sum(kind=='turn_done' for kind,_,_ in sidecar.messages),1)
        self.assertEqual(sidecar.player.discarded_samples,2400)
        self.assertFalse(sidecar.turn_audio)

    def test_shutdown_does_not_emit_spurious_turn(self):
        sidecar=self.sidecar()
        sidecar._text('unfinished')
        sidecar.closing.set()
        sidecar._cancel()
        self.assertFalse(any(kind=='turn_done' for kind,_,_ in sidecar.messages))

    def test_unexpected_synthesis_rate_is_rejected(self):
        sidecar=self.sidecar()
        sidecar.speech.context.sample_rate=48000
        with self.assertRaises(ValueError):sidecar._audio(b'\x00\x00')


if __name__=='__main__':unittest.main()
