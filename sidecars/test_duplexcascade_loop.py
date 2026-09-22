import asyncio
import threading
import unittest
from types import SimpleNamespace
from duplexcascade_loop import DuplexCascadeLoop


class LoopTests(unittest.IsolatedAsyncioTestCase):
    async def test_words_arriving_during_inference_belong_to_next_tick(self):
        started, release = threading.Event(), threading.Event()
        chunks, traces, applied = [], [], []
        class Session:
            def step(self, text):
                chunks.append(text)
                if len(chunks) == 1:
                    started.set()
                    if not release.wait(2): raise RuntimeError('test worker timeout')
                return [text]
            def events(self, tokens): return [('text', tokens[0])]
        class Speech:
            async def apply(self, events):
                applied.append(events)
                if len(applied) == 3: loop.closed = True
            async def close(self): self.closed = True
        queue = asyncio.Queue()
        await queue.put(SimpleNamespace(text='first', arrived=0))
        speech = Speech()
        loop = DuplexCascadeLoop(Session(), speech, queue, tick_seconds=.01, trace=traces.append)
        task = asyncio.create_task(loop.run())
        try:
            while not started.is_set(): await asyncio.sleep(.001)
            # The event loop remains responsive while generation is blocked.
            await queue.put(SimpleNamespace(text='late', arrived=0))
            release.set()
            await asyncio.wait_for(task, 2)
        finally:
            release.set()
        self.assertEqual(chunks, ['first', 'late', ''])
        self.assertEqual(len(traces), 3)
        self.assertTrue(speech.closed)


if __name__ == '__main__': unittest.main()
