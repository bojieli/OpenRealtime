#!/usr/bin/env python3
"""Test synthesis audio credits, bounded model PCM, and cancel on a live service.

Requires Kyutai's health counters. Keeps credits exhausted long enough to check
that generation pauses, then grants one more window and verifies it resumes.
Reports transport behavior, not rendered playback or speech quality.
"""
import argparse
import asyncio
import base64
import json
import time
from pathlib import Path

import httpx
import websockets


async def probe(base, seconds):
    window = 48000
    async with httpx.AsyncClient(timeout=5) as http:
        async def health():
            response = await http.get(base+'/health')
            response.raise_for_status()
            return response.json()
        before = await health()
        if before.get('rows_busy') != 0:
            raise RuntimeError('probe requires idle synthesis service for step attribution')
        if not before.get('output_buffer_frames_per_row'):
            raise RuntimeError('service does not report bounded output queues')
        uri = base.replace('http://','ws://').replace('https://','wss://')+'/v1/tts/stream'
        async with websockets.connect(uri) as ws:
            async def send(kind, **fields):
                await ws.send(json.dumps({'type':kind,'context_id':'backpressure-probe',**fields}))
            async def receive_window():
                count = 0
                while count < window:
                    message = json.loads(await asyncio.wait_for(ws.recv(), 15))
                    if message['type'] == 'error':
                        raise RuntimeError(message)
                    if message['type'] == 'audio':
                        count += len(base64.b64decode(message['pcm16']))
                if count != window:
                    raise AssertionError(f'audio exceeded credit: {count}')
                return count
            await send('context.open',audio_window_bytes=window)
            ready = json.loads(await ws.recv())
            assert ready.get('audio_window_bytes') == window
            assert ready.get('sample_rate') == 24000
            await send('text.append',text=('A slow audio consumer must not prevent cancellation or make the model buffer grow without limit. '*20))
            await send('text.end')
            first = await receive_window()
            await asyncio.sleep(seconds)
            paused = await health()
            await asyncio.sleep(1)
            stable = await health()
            assert paused['steps'] == stable['steps'], 'generation did not pause'
            queued = stable['output_queued_frames']
            assert max(queued) == stable['output_buffer_frames_per_row'], 'queue limit not exercised'
            pong = await ws.ping()
            await asyncio.wait_for(pong,1)
            await send('audio.credit',bytes=window)
            second = await receive_window()
            await asyncio.sleep(.5)
            resumed = await health()
            assert resumed['steps'] > stable['steps'], 'generation did not resume'
            began = time.monotonic()
            await send('context.cancel')
            message = json.loads(await asyncio.wait_for(ws.recv(),2))
            assert message['type'] == 'context.cancelled', message
            latency = (time.monotonic()-began)*1000
        await asyncio.sleep(.2)
        after = await health()
        assert after['rows_busy'] == 0, 'cancel did not release model row'
        return {'passed':True,'window_bytes':window,'delivered_bytes':[first,second],
                'cancel_ack_ms':latency,'before':before,'paused':paused,'stable':stable,
                'resumed':resumed,'after':after}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url',default='http://127.0.0.1:9125')
    parser.add_argument('--settle-seconds',type=float,default=4)
    parser.add_argument('--out',type=Path,required=True)
    args=parser.parse_args()
    try:
        result=asyncio.run(probe(args.url,args.settle_seconds))
    except Exception as error:
        result={'passed':False,'error':f'{type(error).__name__}: {error}'}
    args.out.parent.mkdir(parents=True,exist_ok=True)
    args.out.write_text(json.dumps(result,indent=2)+'\n')
    print(json.dumps(result,indent=2))
    raise SystemExit(0 if result['passed'] else 1)


if __name__=='__main__':main()
