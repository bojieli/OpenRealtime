"""Transport backpressure must preserve samples and admit cancellation."""
import threading
import time

from common import AudioCredits


def test_credit_exhaustion_blocks_until_consumed_without_losing_pcm():
    credit = AudioCredits(8)
    cancel = threading.Event()
    source = bytes(range(24))
    packets = []
    first_window = threading.Event()

    def produce():
        for packet in credit.packets(source, cancel, 4):
            packets.append(packet)
            if len(packets) == 2:
                first_window.set()

    worker = threading.Thread(target=produce)
    worker.start()
    assert first_window.wait(1)
    time.sleep(.03)
    assert len(packets) == 2 and worker.is_alive()
    credit.grant(8)
    deadline = time.monotonic()+1
    while len(packets) < 4 and time.monotonic() < deadline:
        time.sleep(.005)
    assert len(packets) == 4
    credit.grant(8)
    worker.join(1)
    assert not worker.is_alive()
    assert b''.join(packets) == source


def test_cancel_releases_producer_without_further_credit():
    credit = AudioCredits(2)
    cancel = threading.Event()
    packets = credit.packets(b'abcdef', cancel, 2)
    assert next(packets) == b'ab'
    result = []
    worker = threading.Thread(target=lambda: result.extend(packets))
    worker.start()
    cancel.set()
    worker.join(1)
    assert not worker.is_alive() and not result


def test_repeated_grants_cannot_exceed_window():
    credit = AudioCredits(8)
    credit.grant(800)
    assert credit.available == 8
    for invalid in (0, -2, 3):
        try:
            credit.grant(invalid)
        except ValueError:
            pass
        else:
            raise AssertionError('invalid PCM credit accepted')


def test_websocket_control_remains_live_when_audio_credit_is_exhausted():
    import asyncio
    import json
    import socket
    import subprocess
    import sys
    from pathlib import Path
    import websockets

    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    program = '''
import numpy as np
from common import Synthesizer, serve_synthesizer
class Fake(Synthesizer):
    def synthesize_incremental(self, texts, voice, cancel):
        yield np.arange(24000, dtype=np.int16)
serve_synthesizer(Fake(), '127.0.0.1', PORT)
'''.replace('PORT', str(port))
    server = subprocess.Popen([sys.executable, '-c', program], cwd=Path(__file__).parent,
                              stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)

    async def check():
        deadline = time.monotonic()+10
        while True:
            try:
                ws = await websockets.connect(f'ws://127.0.0.1:{port}/v1/tts/stream')
                break
            except OSError:
                if time.monotonic() > deadline:
                    raise
                await asyncio.sleep(.05)
        async with ws:
            await ws.send(json.dumps({'type':'context.open','context_id':'test','audio_window_bytes':4800}))
            messages = [json.loads(await asyncio.wait_for(ws.recv(), 2)) for _ in range(2)]
            assert {m['type'] for m in messages} == {'context.ready','audio'}
            try:
                await asyncio.wait_for(ws.recv(), .1)
            except asyncio.TimeoutError:
                pass
            else:
                raise AssertionError('audio exceeded granted window')
            pong = await ws.ping()
            await asyncio.wait_for(pong, 1)
            await ws.send(json.dumps({'type':'context.cancel','context_id':'test'}))
            message = json.loads(await asyncio.wait_for(ws.recv(), 1))
            assert message['type'] == 'context.cancelled'

    try:
        asyncio.run(check())
    finally:
        server.terminate()
        try:
            server.wait(5)
        except subprocess.TimeoutExpired:
            server.kill()
            server.wait()
