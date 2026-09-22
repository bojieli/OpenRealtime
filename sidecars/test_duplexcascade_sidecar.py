import io
import asyncio
import unittest
import threading
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
        asyncio.run(sidecar._audio(b'\x01\x00'*2400))
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
        with self.assertRaises(ValueError):asyncio.run(sidecar._audio(b'\x00\x00'))

    def test_peer_eof_unblocks_pending_response_before_worker_join(self):
        sidecar=self.sidecar()
        worker=threading.Thread(target=sidecar.on_respond,daemon=True)
        worker.start()
        try:
            sidecar._read_loop()  # empty input is peer EOF
            worker.join(.5)
            self.assertFalse(worker.is_alive())
            self.assertTrue(sidecar.closing.is_set())
        finally:
            sidecar.closing.set()
            worker.join(1)

    def test_disconnect_releases_native_workers_before_next_handshake(self):
        import socket
        import time
        from unittest.mock import patch
        from openrealtime_sidecar.protocol import read_message, write_message

        class Recognizer:
            def __init__(self, *args): pass
            async def run(self): await asyncio.Event().wait()

        backend=SimpleNamespace(session_lock=threading.Lock(),session=lambda:None)
        args=SimpleNamespace(trace_dir=None,asr='unused',tts='unused',voice='default')
        with patch('duplexcascade_sidecar.AppendOnlyRecognizer',Recognizer):
            for _ in range(2):
                client,server=socket.socketpair()
                client.settimeout(2)
                reader,writer=server.makefile('rb'),server.makefile('wb')
                sidecar=NativeCascade(reader,writer,args=args,backend=backend)
                thread=threading.Thread(target=sidecar.run,daemon=True)
                thread.start()
                remote_reader,remote_writer=client.makefile('rb'),client.makefile('wb')
                try:
                    write_message(remote_writer,'hello',version=1,sample_rate=16000)
                    self.assertEqual(read_message(remote_reader).type,'ready')
                    write_message(remote_writer,'respond')
                    time.sleep(.03)  # response worker waits for model-owned output
                    write_message(remote_writer,'bye')
                    thread.join(1)
                    self.assertFalse(thread.is_alive())
                    self.assertTrue(sidecar.finished.is_set())
                    self.assertFalse(backend.session_lock.locked())
                finally:
                    sidecar.closing.set()
                    client.shutdown(socket.SHUT_RDWR)
                    for handle in (remote_reader,remote_writer,client,reader,writer,server):
                        handle.close()

    def test_failed_session_releases_model_for_next_connection(self):
        class Failed(NativeCascade):
            async def _main(self): raise RuntimeError('failed initialization')
        backend=SimpleNamespace(session_lock=threading.Lock())
        sidecar=Failed(io.BytesIO(),io.BytesIO(),args=SimpleNamespace(trace_dir=None),backend=backend)
        sidecar.error=lambda *args,**kwargs:None
        sidecar.owns_session=True
        backend.session_lock.acquire()
        sidecar._thread()
        self.assertTrue(sidecar.finished.is_set())
        self.assertFalse(backend.session_lock.locked())
        self.assertFalse(sidecar.owns_session)
        self.assertIsInstance(sidecar.failure,RuntimeError)


if __name__=='__main__':unittest.main()
