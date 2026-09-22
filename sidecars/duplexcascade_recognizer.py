"""Admit append-only decoder fragments without waiting for a next-word marker.

Only services explicitly declaring decoder-append-only qualify. The normal
cascade's completed-word policy remains separate. Each emitted fragment
preserves its original whitespace, so a word split across packets stays whole.
"""
import asyncio
import time
import numpy as np
from microturn_sidecar import Word


def append_only_delta(previous, result):
    if result.get('stability') != 'decoder-append-only':
        raise ValueError('native fragment admission requires decoder-append-only evidence')
    text = result['text']
    if not text.startswith(previous):
        raise ValueError('append-only recognizer revised admitted text')
    return text[len(previous):]


class AppendOnlyRecognizer:
    def __init__(self, url, words, trace=None):
        self.url, self.words, self.trace = url.rstrip('/'), words, trace
        self.audio = asyncio.Queue()

    def push(self, pcm16):
        self.audio.put_nowait(pcm16)

    async def run(self):
        import httpx
        async with httpx.AsyncClient(timeout=30) as client:
            response = await client.post(self.url+'/api/start')
            response.raise_for_status()
            sid = response.json()['session_id']
            previous = ''
            try:
                while True:
                    pcm = await self.audio.get()
                    response = await client.post(self.url+'/api/chunk', params={'session_id':sid},
                        content=(np.frombuffer(pcm,dtype='<i2').astype('<f4')/32768).tobytes())
                    response.raise_for_status()
                    result = response.json()
                    delta = append_only_delta(previous, result)
                    text = result['text']
                    previous = text
                    if self.trace:
                        self.trace({'t':time.monotonic(),'text':text,'stable_text':result.get('stable_text'), 'delta':delta})
                    if delta:
                        self.words.put_nowait(Word(delta,time.monotonic()))
            finally:
                # Return the model slot even when the audio session is cancelled.
                response = await client.post(self.url+'/api/finish',params={'session_id':sid})
                response.raise_for_status()
