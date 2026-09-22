#!/usr/bin/env python3
"""Paced audio component probe: streaming ASR -> native micro-turns -> TTS.

Output timestamps are synthesis packet arrival, not rendered playback.
This verifies the composed components, not the public Realtime binding.
"""
import argparse
import asyncio
import json
from pathlib import Path
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'sidecars'))
from duplexcascade_model import DuplexCascadeModel
from duplexcascade_loop import DuplexCascadeLoop
from duplexcascade_speech import DuplexCascadeSpeech
from microturn_sidecar import SpeechContext
from duplexcascade_recognizer import AppendOnlyRecognizer


async def probe(args, backend):
    import numpy as np
    import soundfile as sf
    samples, rate = sf.read(args.wav, dtype='float32')
    if samples.ndim != 1 or rate != 16000:
        raise ValueError('probe requires mono 16 kHz WAV')
    words = asyncio.Queue()
    began = time.monotonic()
    report = {'model': backend.metadata, 'input': str(args.wav), 'asr': args.asr,
              'tts': args.tts, 'timing_basis': 'packet arrival; no rendered playback receipts',
              'ticks': [], 'audio': [], 'text': [], 'cancellations': [], 'asr_trace': []}
    asr = AppendOnlyRecognizer(args.asr, words, report['asr_trace'].append)
    def audio(pcm):
        report['audio'].append({'t': time.monotonic()-began, 'bytes':len(pcm) if pcm else 0})
    speech = DuplexCascadeSpeech(lambda:SpeechContext(args.tts, 'default'), audio,
                                 report['text'].append,
                                 lambda:report['cancellations'].append(time.monotonic()-began))
    session = backend.session()
    loop = DuplexCascadeLoop(session, speech, words, trace=report['ticks'].append, separator='')
    # Warm-up is explicit and uses a disposable history.
    backend.session().step('Hello')
    asr_task = asyncio.create_task(asr.run())
    loop_task = asyncio.create_task(loop.run())
    began = time.monotonic()
    try:
        silence = np.zeros(int(rate*args.duration), dtype=np.float32)
        end = min(len(samples), len(silence)-rate)
        silence[rate:rate+end] = samples[:end]
        for offset in range(0,len(silence),1600):
            if asr_task.done(): asr_task.result()
            if loop_task.done(): loop_task.result()
            await asyncio.sleep(max(0,began+offset/rate-time.monotonic()))
            asr.push((np.clip(silence[offset:offset+1600],-1,1)*32767).astype('<i2').tobytes())
        await asyncio.sleep(.1)
    finally:
        loop.closed = True
        loop_task.cancel()
        asr_task.cancel()
        results = await asyncio.gather(loop_task, asr_task, return_exceptions=True)
        report['errors'] = [repr(r) for r in results if isinstance(r,Exception)]
        args.out.parent.mkdir(parents=True,exist_ok=True)
        args.out.write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps({'ticks':len(report['ticks']), 'audio_packets':len(report['audio']),
                      'text':report['text'], 'errors':report['errors']}))
    if report['errors'] or not report['ticks'] or not report['audio']:
        raise RuntimeError('composed audio probe failed')


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for name in ('source','snapshot','base','wav','out'):p.add_argument('--'+name,type=Path,required=True)
    p.add_argument('--asr',default='http://127.0.0.1:9112')
    p.add_argument('--tts',default='ws://127.0.0.1:9125/v1/tts/stream')
    p.add_argument('--duration',type=float,default=20)
    a=p.parse_args()
    backend=DuplexCascadeModel(a.source,a.snapshot,a.base,slow_tokenizer=True)
    asyncio.run(probe(a,backend))


if __name__=='__main__':main()
