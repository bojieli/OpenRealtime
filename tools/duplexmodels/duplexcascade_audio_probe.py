#!/usr/bin/env python3
"""Paced audio component probe: streaming ASR -> native micro-turns -> TTS.

Output timestamps are synthesis packet arrival, not rendered playback.
This verifies the composed components, not the public Realtime binding.
"""
import argparse
import asyncio
import json
import hashlib
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
    interruption = None
    if args.interruption:
        interruption, interrupt_rate = sf.read(args.interruption, dtype='float32')
        if interruption.ndim != 1 or interrupt_rate != rate:
            raise ValueError('interruption requires mono 16 kHz WAV')
    if args.duration <= 1 + len(samples)/rate:
        raise ValueError('duration must contain the complete first question')
    words = asyncio.Queue()
    began = time.monotonic()
    report = {'model': backend.metadata, 'input': str(args.wav), 'asr': args.asr,
              'tts': args.tts, 'timing_basis': 'packet arrival; no rendered playback receipts',
              'ticks': [], 'audio': [], 'text': [], 'cancellations': [], 'asr_trace': [], 'interruption_started_s': None,
              'input_sha256': hashlib.sha256(args.wav.read_bytes()).hexdigest(),
              'interruption': str(args.interruption) if args.interruption else None,
              'interruption_sha256': hashlib.sha256(args.interruption.read_bytes()).hexdigest() if args.interruption else None}
    chunks = []
    first_audio = None
    sample_rate = None
    shutting_down = False
    def context():
        value = SpeechContext(args.tts, 'default')
        contexts.append(value)
        return value
    contexts = []
    asr = AppendOnlyRecognizer(args.asr, words, report['asr_trace'].append)
    def audio(pcm):
        nonlocal first_audio, sample_rate
        if pcm:
            if first_audio is None: first_audio = time.monotonic()-began
            rate_now = contexts[-1].sample_rate
            if sample_rate is not None and sample_rate != rate_now: raise ValueError('TTS sample rate changed')
            sample_rate = rate_now
            chunks.append(pcm)
        report['audio'].append({'t': time.monotonic()-began, 'bytes':len(pcm) if pcm else 0})
    speech = DuplexCascadeSpeech(context, audio,
                                 report['text'].append,
                                 lambda:report['cancellations'].append({'t':time.monotonic()-began, 'reason':'shutdown' if shutting_down else 'model'}))
    session = backend.session()
    loop = DuplexCascadeLoop(session, speech, words, trace=report['ticks'].append, separator='')
    # Warm-up is explicit and uses a disposable history.
    backend.session().step('Hello')
    asr_task = asyncio.create_task(asr.run())
    loop_task = asyncio.create_task(loop.run())
    began = time.monotonic()
    failure = None
    try:
        silence = np.zeros(int(rate*args.duration), dtype=np.float32)
        end = min(len(samples), len(silence)-rate)
        silence[rate:rate+end] = samples[:end]
        for offset in range(0,len(silence),1600):
            if asr_task.done(): asr_task.result()
            if loop_task.done(): loop_task.result()
            await asyncio.sleep(max(0,began+offset/rate-time.monotonic()))
            if (interruption is not None and first_audio is not None
                    and report['interruption_started_s'] is None
                    and time.monotonic()-began >= first_audio+args.interrupt_after):
                if offset+len(interruption) > len(silence):
                    raise ValueError('duration too short for complete interruption')
                silence[offset:offset+len(interruption)] = interruption
                report['interruption_started_s'] = offset/rate
            asr.push((np.clip(silence[offset:offset+1600],-1,1)*32767).astype('<i2').tobytes())
        await asyncio.sleep(.1)
    except Exception as error:
        failure = repr(error)
        raise
    finally:
        shutting_down = True
        loop.closed = True
        loop_task.cancel()
        asr_task.cancel()
        results = await asyncio.gather(loop_task, asr_task, return_exceptions=True)
        report['errors'] = ([failure] if failure else []) + [repr(r) for r in results if isinstance(r,Exception)]
        args.out.parent.mkdir(parents=True,exist_ok=True)
        if chunks:
            output = np.frombuffer(b''.join(chunks), dtype='<i2')
            wav = args.out.with_suffix('.wav')
            sf.write(wav, output, sample_rate, subtype='PCM_16')
            report['output'] = {'path':str(wav), 'sha256':hashlib.sha256(wav.read_bytes()).hexdigest(),
                                'sample_rate':sample_rate, 'seconds':len(output)/sample_rate,
                                'layout':'concatenated synthesis packets; not rendered timeline'}
        args.out.write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps({'ticks':len(report['ticks']), 'audio_packets':len(report['audio']),
                      'text':report['text'], 'errors':report['errors']}))
    if report['errors'] or not report['ticks'] or not chunks:
        raise RuntimeError('composed audio probe failed')


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for name in ('source','snapshot','base','wav','out'):p.add_argument('--'+name,type=Path,required=True)
    p.add_argument('--asr',default='http://127.0.0.1:9112')
    p.add_argument('--tts',default='ws://127.0.0.1:9125/v1/tts/stream')
    p.add_argument('--duration',type=float,default=20)
    p.add_argument('--interruption', type=Path)
    p.add_argument('--interrupt-after', type=float, default=.8)
    a=p.parse_args()
    backend=DuplexCascadeModel(a.source,a.snapshot,a.base,slow_tokenizer=True)
    asyncio.run(probe(a,backend))


if __name__=='__main__':main()
