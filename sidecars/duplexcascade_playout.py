"""Pace synthesis PCM at real time and invalidate unsent audio on cancellation.

All calls belong to one asyncio loop. This is delivery pacing, not proof of
rendered playback. A synchronous send callback must enqueue/write promptly.
"""
import asyncio
import time


class PacedAudio:
    def __init__(self, send, *, rate=24000, frame_ms=20, max_buffer_seconds=30):
        if rate <= 0 or frame_ms <= 0 or max_buffer_seconds <= 0:
            raise ValueError('playout settings must be positive')
        self.send = send
        self.rate = rate
        self.frame_bytes = int(rate*frame_ms/1000)*2
        if not self.frame_bytes:
            raise ValueError('frame is smaller than a sample')
        self.limit = int(rate*max_buffer_seconds)*2
        self.buffer = bytearray()
        self.changed = asyncio.Event()
        self.space = asyncio.Event()
        self.epoch = 0
        self.closed = False
        self.sent_samples = 0
        self.discarded_samples = 0

    def append(self, pcm):
        if self.closed:
            return
        if pcm is None:
            return
        if len(pcm) % 2:
            raise ValueError('PCM16 chunk has a partial sample')
        if len(self.buffer)+len(pcm) > self.limit:
            raise BufferError('synthesis exceeded bounded playout buffer')
        self.buffer.extend(pcm)
        self.changed.set()

    async def put(self, pcm):
        """Wait for bounded capacity; cancellation discards the pending suffix."""
        if pcm is None: return
        if len(pcm) % 2: raise ValueError('PCM16 chunk has a partial sample')
        epoch = self.epoch
        offset = 0
        while offset < len(pcm) and not self.closed and epoch == self.epoch:
            available = (self.limit-len(self.buffer))//2*2
            if not available:
                self.space.clear()
                await self.space.wait()
                continue
            end = min(len(pcm),offset+available)
            self.append(pcm[offset:end])
            offset = end

    def cancel(self):
        self.discarded_samples += len(self.buffer)//2
        self.buffer.clear()
        self.epoch += 1
        self.space.set()
        self.changed.set()

    def close(self):
        self.closed = True
        self.cancel()

    async def run(self):
        deadline = None
        epoch = self.epoch
        while not self.closed:
            if epoch != self.epoch:
                deadline, epoch = None, self.epoch
            if not self.buffer:
                self.changed.clear()
                await self.changed.wait()
                continue
            now = time.monotonic()
            if deadline is not None and deadline > now:
                self.changed.clear()
                try:
                    await asyncio.wait_for(self.changed.wait(), deadline-now)
                    continue
                except asyncio.TimeoutError:
                    pass
            # No await between selecting the epoch's bytes and sending them.
            # cancel() therefore cannot interleave and leak an obsolete frame.
            if self.closed or epoch != self.epoch:
                continue
            frame = bytes(self.buffer[:self.frame_bytes])
            del self.buffer[:len(frame)]
            self.space.set()
            self.send(frame)
            samples = len(frame)//2
            self.sent_samples += samples
            deadline = max(deadline or 0, time.monotonic()) + samples/self.rate
