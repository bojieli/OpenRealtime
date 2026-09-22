"""Synthesis shutdown must not strand callbacks waiting for playback space."""
import asyncio
import base64
import json

from microturn_sidecar import SpeechContext


class Socket:
    def __init__(self, messages):
        self.messages = iter(messages)
        self.closed = False

    def __aiter__(self):
        return self

    async def __anext__(self):
        try:
            return json.dumps(next(self.messages))
        except StopIteration:
            raise StopAsyncIteration

    async def close(self):
        self.closed = True


def test_close_cancels_reader_blocked_in_audio_callback():
    async def scenario():
        entered = asyncio.Event()
        released = asyncio.Event()
        async def audio(pcm):
            entered.set()
            try:
                await asyncio.Event().wait()
            finally:
                released.set()
        context = SpeechContext('unused', 'default')
        context.socket = Socket([{'type':'audio', 'pcm16':base64.b64encode(b'\0\0').decode()}])
        context.reader = asyncio.create_task(context._read(audio))
        await asyncio.wait_for(entered.wait(), 1)
        await asyncio.wait_for(context.close(), 1)
        assert released.is_set() and context.reader.done() and context.socket.closed
        await context.close()  # repeated shutdown is safe
    asyncio.run(scenario())


def test_terminal_and_error_events_await_async_completion_callback():
    async def scenario():
        for kind in ('audio.done', 'context.cancelled', 'error'):
            calls = []
            async def audio(pcm):
                await asyncio.sleep(0)
                calls.append(pcm)
            context = SpeechContext('unused', 'default')
            context.socket = Socket([{'type':kind, 'message':'test failure'}])
            await context._read(audio)
            assert calls == [None]
            assert (context.failure is not None) == (kind == 'error')
    asyncio.run(scenario())
