"""Single-loop PCM sink with generation epochs and sample-clock receipts."""
import asyncio
import time
import wave


class PacedRecorder:
    def __init__(self, rate=24000):
        if rate <= 0:
            raise ValueError("positive sample rate required")
        self.rate = rate
        self.origin = time.monotonic()
        self.epoch = 0
        self.queue = asyncio.Queue()
        self.pcm = bytearray()
        self.marks = []
        self.worker = None
        self.closed = False

    def now(self):
        return time.monotonic() - self.origin

    def callback(self):
        epoch = self.epoch

        def receive(pcm):
            if self.closed or epoch != self.epoch:
                return
            if pcm is None:
                self.marks.append({"kind": "synthesis-eof", "epoch": epoch, "at_s": self.now()})
                return
            if len(pcm) % 2:
                raise ValueError("unaligned PCM16")
            self.marks.append({"kind": "generated", "epoch": epoch,
                               "at_s": self.now(), "samples": len(pcm) // 2})
            self.queue.put_nowait((epoch, bytes(pcm)))
        return receive

    def start(self):
        if self.worker is not None:
            raise RuntimeError("recorder already started")
        self.worker = asyncio.create_task(self.run())

    def cancel(self, reason):
        self.marks.append({"kind": "cancel", "epoch": self.epoch,
                           "at_s": self.now(), "reason": reason})
        self.epoch += 1
        while not self.queue.empty():
            self.queue.get_nowait()
            self.queue.task_done()

    def commit(self, epoch, pcm, began, ended):
        if epoch != self.epoch:
            return
        start = max(round(began * self.rate), len(self.pcm) // 2)
        offset = start * 2
        self.pcm.extend(b"\0" * max(0, offset + len(pcm) - len(self.pcm)))
        self.pcm[offset:offset + len(pcm)] = pcm
        self.marks.append({"kind": "played", "epoch": epoch,
                           "start_sample": start, "samples": len(pcm) // 2,
                           "acknowledged_at_s": ended, "sink": "paced-recorder"})

    async def run(self):
        while True:
            epoch, pcm = await self.queue.get()
            try:
                # Bound cancel uncertainty to one 20 ms frame. A canceled
                # in-flight frame is conservatively omitted, never promoted.
                frame_bytes = max(2, self.rate // 50 * 2)
                for offset in range(0, len(pcm), frame_bytes):
                    if epoch != self.epoch:
                        break
                    frame = pcm[offset:offset + frame_bytes]
                    began = self.now()
                    await asyncio.sleep(len(frame) / 2 / self.rate)
                    self.commit(epoch, frame, began, self.now())
            finally:
                self.queue.task_done()

    async def close(self):
        self.closed = True
        self.cancel("trial-end")
        if self.worker is not None:
            self.worker.cancel()
            await asyncio.gather(self.worker, return_exceptions=True)

    def write(self, path):
        with wave.open(str(path), "wb") as output:
            output.setparams((1, 2, self.rate, 0, "NONE", "not compressed"))
            output.writeframes(self.pcm)
