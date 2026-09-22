#!/usr/bin/env python3
"""Experimental released DuplexCascade -> persistent Kyutai synthesis sidecar.

Output is paced PCM. A turn closes after model thinking plus 600 ms with no
new synthesis packets and a drained delivery buffer; this is an adapter policy,
not a playback receipt or a native EOS. Instructions/text injection are not
part of the released training protocol and are not advertised as supported.
"""
import argparse
import asyncio
from pathlib import Path
import threading
import subprocess
import time

import numpy as np
from openrealtime_sidecar import Sidecar, Capability, run, log
from duplexcascade_model import DuplexCascadeModel
from duplexcascade_loop import DuplexCascadeLoop
from duplexcascade_recognizer import AppendOnlyRecognizer
from duplexcascade_speech import DuplexCascadeSpeech
from duplexcascade_playout import PacedAudio
from microturn_sidecar import LinearResampler, SpeechContext
from moshi_sidecar import _listen


class NativeCascade(Sidecar):
    model_name = 'sbintuitions/DuplexCascade/native-cascade'
    output_rate = 24000
    capabilities = (Capability.FULL_DUPLEX, Capability.NATIVE_VAD,
                    Capability.NATIVE_INTERACTION, Capability.BARGE_IN)

    def __init__(self, reader, writer, *, args, backend):
        super().__init__(reader, writer)
        self.args, self.backend = args, backend
        self.closing = threading.Event()
        self.finished = threading.Event()
        self.initialized = threading.Event()
        self.failure = None
        self.loop = None
        self.text = ''
        self.last_audio = 0
        self.turn_open = False
        self.turn_audio = False
        self.resampler = None
        self.owns_session = False

    def configure(self, hello):
        if not self.backend.session_lock.acquire(timeout=10):
            raise RuntimeError('native DuplexCascade is serving another session')
        self.owns_session = True
        if self.instructions:
            log('released DuplexCascade does not consume session instructions')
        if self.input_rate != 16000:
            self.resampler = LinearResampler(self.input_rate,16000)
        self.thread = threading.Thread(target=self._thread,daemon=True)
        try:
            self.thread.start()
        except BaseException:
            self.backend.session_lock.release()
            self.owns_session = False
            raise
        if not self.initialized.wait(15):
            self.closing.set()
            raise RuntimeError('native cascade initialization timed out')
        if self.failure:
            raise self.failure

    def _thread(self):
        try:
            asyncio.run(self._main())
        except Exception as error:
            self.failure = error
            self.initialized.set()
            if not self.closing.is_set():
                self.error(str(error),code='native_cascade_failed',fatal=True)
        finally:
            if self.owns_session:
                self.owns_session = False
                self.backend.session_lock.release()
            self.finished.set()

    async def _main(self):
        self.loop = asyncio.get_running_loop()
        words = asyncio.Queue()
        self.recognizer = AppendOnlyRecognizer(self.args.asr,words)
        self.player = PacedAudio(self.audio,rate=self.output_rate)
        self.speech = DuplexCascadeSpeech(self._context,self._audio,self._text,self._cancel)
        self.clock = DuplexCascadeLoop(self.backend.session(),self.speech,words,separator='')
        tasks = [asyncio.create_task(self.recognizer.run()),asyncio.create_task(self.player.run()),
                 asyncio.create_task(self.clock.run())]
        self.initialized.set()
        try:
            while not self.closing.is_set():
                for task in tasks:
                    if task.done():
                        task.result()
                        raise RuntimeError('native component ended unexpectedly')
                if self.interrupted():
                    await self.speech.cancel()
                    self._interrupted.clear()
                if (self.turn_open and self.turn_audio and self.speech.thinking and not self.player.buffer
                        and time.monotonic()-self.last_audio > .6):
                    self._end_turn()
                await asyncio.sleep(.02)
        finally:
            self.clock.closed = True
            self.player.close()
            for task in tasks: task.cancel()
            results = await asyncio.gather(*tasks,return_exceptions=True)
            for result in results:
                if isinstance(result,Exception):
                    log(f'native component shutdown: {result}')

    def _context(self):
        return SpeechContext(self.args.tts,self.args.voice)

    def _audio(self, pcm):
        if pcm:
            if self.speech.context is not None and self.speech.context.sample_rate != self.output_rate:
                raise ValueError('native profile requires 24 kHz synthesis')
            self.last_audio = time.monotonic()
            self.turn_audio = True
            self.player.append(pcm)

    def _text(self,text):
        if text.strip():
            self.turn_open = True
            self.text += text
            self.last_audio = time.monotonic()
            self.text_delta(text)

    def _end_turn(self):
        if self.turn_open:
            self.text_done(self.text)
            self.turn_done()
            self.text, self.turn_open, self.turn_audio = '',False,False

    def _cancel(self):
        self.player.cancel()
        if not self.closing.is_set(): self._end_turn()

    def on_audio(self, pcm16):
        if self.closing.is_set() or self.loop is None: return
        samples=np.frombuffer(pcm16,dtype='<i2').astype(np.float32)/32768
        if self.resampler is not None:samples=self.resampler(samples)
        pcm=(np.clip(samples,-1,1)*32767).astype('<i2').tobytes()
        if pcm:self.loop.call_soon_threadsafe(self.recognizer.push,pcm)

    def on_respond(self):
        # This protocol request cannot force a trained listen/speak decision.
        # Wait for the model's own response rather than synthesize a substitute.
        deadline=time.monotonic()+60
        while not self.closing.is_set() and time.monotonic()<deadline:
            if self.turn_open: break
            time.sleep(.02)
        while self.turn_open and not self.closing.is_set() and time.monotonic()<deadline:
            time.sleep(.02)

    def on_close(self):
        self.closing.set()
        self.finished.wait(30)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for name in ('source','snapshot','base'):p.add_argument('--'+name,type=Path,required=True)
    p.add_argument('--asr',default='http://127.0.0.1:9112')
    p.add_argument('--tts',default='ws://127.0.0.1:9125/v1/tts/stream')
    p.add_argument('--voice',default='default')
    p.add_argument('--listen',default='')
    a=p.parse_args()
    free = int(subprocess.check_output(
        ['nvidia-smi','--query-gpu=memory.free','--format=csv,noheader,nounits'],text=True).splitlines()[0])
    if free < 20000:
        raise RuntimeError(f'native DuplexCascade needs 20000 MiB free before load; available {free}')
    backend=DuplexCascadeModel(a.source,a.snapshot,a.base,slow_tokenizer=True)
    backend.session().step('Hello')
    backend.session_lock = threading.Lock()
    if a.listen:
        _listen(a.listen,lambda reader,writer:NativeCascade(reader,writer,args=a,backend=backend))
        return
    run(NativeCascade,args=a,backend=backend)


if __name__=='__main__':main()
