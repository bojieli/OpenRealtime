import asyncio
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("cleanup", Path(__file__).with_name("native_cleanup.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class CleanupTest(unittest.IsolatedAsyncioTestCase):
    async def test_failed_inference_and_speech_still_close_recorder(self):
        calls = []

        class Speech:
            async def close(self):
                calls.append("speech")
                raise RuntimeError("socket failure")

        class Recorder:
            async def close(self):
                calls.append("recorder")

        async def failed_inference():
            raise RuntimeError("model failure")

        errors = await module.finish(asyncio.create_task(failed_inference()), Speech(), Recorder())
        self.assertEqual(calls, ["speech", "recorder"])
        self.assertEqual(len(errors), 2)
        self.assertIn("model failure", errors[0])
        self.assertIn("socket failure", errors[1])
