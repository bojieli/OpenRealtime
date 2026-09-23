import asyncio
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("paced", Path(__file__).with_name("paced_recorder.py"))
paced = importlib.util.module_from_spec(spec)
spec.loader.exec_module(paced)


class RecorderTest(unittest.IsolatedAsyncioTestCase):
    async def test_cancel_discards_queue_and_late_old_callbacks(self):
        recorder = paced.PacedRecorder(1000)
        old = recorder.callback()
        old(b"\1\0" * 100)
        recorder.cancel("policy-interruption")
        old(b"\2\0" * 100)
        self.assertTrue(recorder.queue.empty())
        recorder.commit(0, b"\3\0" * 10, 0, .01)
        self.assertEqual(recorder.pcm, b"")
        recorder.start()
        recorder.callback()(b"\4\0" * 20)
        await asyncio.wait_for(recorder.queue.join(), 2)
        played = [m for m in recorder.marks if m["kind"] == "played"]
        self.assertEqual(sum(m["samples"] for m in played), 20)
        self.assertEqual({m["epoch"] for m in played}, {1})
        self.assertGreaterEqual(played[0]["acknowledged_at_s"], .02)
        await recorder.close()

    async def test_generation_is_not_playback(self):
        recorder = paced.PacedRecorder()
        recorder.callback()(b"\1\0" * 480)
        recorder.callback()(None)
        self.assertEqual(recorder.pcm, b"")
        self.assertFalse(any(m["kind"] == "played" for m in recorder.marks))
        await recorder.close()
