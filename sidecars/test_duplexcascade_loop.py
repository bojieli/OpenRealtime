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

    async def test_cancel_before_first_word_closes_synthesis(self):
        class Speech:
            closed = False
            async def close(self): self.closed = True
        speech = Speech()
        loop = DuplexCascadeLoop(None, speech, asyncio.Queue())
        task = asyncio.create_task(loop.run())
        await asyncio.sleep(0)
        task.cancel()
        with self.assertRaises(asyncio.CancelledError): await task
        self.assertTrue(speech.closed)

    async def test_failed_inference_still_closes_synthesis(self):
        class Session:
            def step(self, chunk): raise ValueError('inference failed')
        class Speech:
            closed = False
            async def close(self): self.closed = True
        speech = Speech()
        words = asyncio.Queue()
        await words.put(SimpleNamespace(text='hello', arrived=0))
        loop = DuplexCascadeLoop(Session(), speech, words, tick_seconds=.001)
        with self.assertRaisesRegex(ValueError, 'inference failed'): await loop.run()
        self.assertTrue(speech.closed)

    async def test_disconnect_waits_for_worker_without_emitting_answer(self):
        started, release = threading.Event(), threading.Event()
        class Session:
            def step(self, chunk):
                started.set()
                if not release.wait(2): raise RuntimeError('worker timeout')
                return [1]
            def events(self, tokens): raise AssertionError('disconnected output')
        class Speech:
            closed = False
            async def close(self): self.closed = True
        words = asyncio.Queue()
        await words.put(SimpleNamespace(text='hello', arrived=0))
        speech = Speech()
        loop = DuplexCascadeLoop(Session(), speech, words, tick_seconds=.001)
        task = asyncio.create_task(loop.run())
        try:
            while not started.is_set(): await asyncio.sleep(.001)
            task.cancel()
            await asyncio.sleep(.01)
            self.assertFalse(task.done())
            release.set()
            with self.assertRaises(asyncio.CancelledError): await task
            self.assertTrue(speech.closed)
        finally:
            release.set()

    async def test_bounded_variant_skips_only_silent_ticks_over_the_backlog(self):
        backlog = [0.0]
        chunks, traces = [], []
        class Session:
            def step(self, text):
                chunks.append(text)
                return [text]
            def events(self, tokens): return []
        class Speech:
            async def apply(self, events):
                # Each generated tick leaves 5 s of unplayed speech.
                backlog[0] = 5.0
            async def close(self): pass
        words = asyncio.Queue()
        await words.put(SimpleNamespace(text='hi', arrived=0))
        loop = DuplexCascadeLoop(Session(), Speech(), words, tick_seconds=.005, trace=traces.append,
                                 backlog=lambda: backlog[0], max_backlog_seconds=2)
        async def until(condition):
            async def poll():
                while not condition(): await asyncio.sleep(.001)
            await asyncio.wait_for(poll(), 2)
        task = asyncio.create_task(loop.run())
        await until(lambda: len(traces) >= 4)
        self.assertEqual(chunks, ['hi'])
        self.assertTrue(all(t.get('skipped') == 'backlog' for t in traces[1:]))
        await words.put(SimpleNamespace(text='wait', arrived=0))
        await until(lambda: len(chunks) >= 2)
        self.assertEqual(chunks[1], 'wait')
        backlog[0] = 1.0
        await until(lambda: len(chunks) >= 3)
        loop.closed = True
        await asyncio.wait_for(task, 2)
        self.assertEqual(chunks[2], '')

    async def test_faithful_default_generates_regardless_of_backlog(self):
        chunks = []
        class Session:
            def step(self, text):
                chunks.append(text)
                if len(chunks) == 3: loop.closed = True
                return [text]
            def events(self, tokens): return []
        class Speech:
            async def apply(self, events): pass
            async def close(self): pass
        words = asyncio.Queue()
        await words.put(SimpleNamespace(text='hi', arrived=0))
        loop = DuplexCascadeLoop(Session(), Speech(), words, tick_seconds=.001, backlog=lambda: 99.0)
        await asyncio.wait_for(loop.run(), 2)
        self.assertEqual(chunks, ['hi', '', ''])


if __name__ == '__main__': unittest.main()
