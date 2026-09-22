import asyncio
import time
import unittest
from duplexcascade_playout import PacedAudio


class PacingTests(unittest.IsolatedAsyncioTestCase):
    async def test_cancel_removes_queued_audio_during_pacing_wait(self):
        sent = []
        player = PacedAudio(lambda pcm:sent.append((time.monotonic(),pcm)), rate=1000,frame_ms=40)
        player.append(b'\x01\x00'*200)
        task = asyncio.create_task(player.run())
        try:
            await asyncio.sleep(.01)
            self.assertEqual(len(sent),1)
            player.cancel()
            player.append(b'\x02\x00'*20)
            await asyncio.sleep(.06)
            self.assertEqual([pcm for _,pcm in sent],[b'\x01\x00'*40,b'\x02\x00'*20])
            self.assertEqual(player.discarded_samples,160)
        finally:
            player.close()
            await task

    async def test_frames_are_paced_and_shutdown_wakes_idle_loop(self):
        times=[]
        player=PacedAudio(lambda pcm:times.append(time.monotonic()),rate=1000,frame_ms=30)
        player.append(b'\x00\x00'*90)
        task=asyncio.create_task(player.run())
        try:
            await asyncio.sleep(.11)
            self.assertEqual(len(times),3)
            self.assertGreaterEqual(times[-1]-times[0],.055)
        finally:
            player.close()
            await asyncio.wait_for(task,.2)

    def test_buffer_overflow_and_partial_sample_fail_visibly(self):
        player=PacedAudio(lambda pcm:None,rate=1000,max_buffer_seconds=.1)
        with self.assertRaises(ValueError):player.append(b'x')
        with self.assertRaises(BufferError):player.append(b'xx'*101)

    async def test_backpressure_wait_is_cancelled_without_stale_suffix(self):
        player=PacedAudio(lambda pcm:None,rate=1000,max_buffer_seconds=.1)
        task=asyncio.create_task(player.put(b'xx'*300))
        await asyncio.sleep(.01)
        self.assertEqual(len(player.buffer),200)
        self.assertFalse(task.done())
        player.cancel()
        await asyncio.wait_for(task,.2)
        self.assertEqual(len(player.buffer),0)


if __name__=='__main__':unittest.main()
