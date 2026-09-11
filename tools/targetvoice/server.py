"""Pre-ASR competing-voice suppression with one initial three-second reference."""

import argparse
import sys
import threading
import time
from collections import deque
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import numpy as np
import torch
from silero_vad import load_silero_vad

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from tools.noisefilter.server import Sessions, ThreadingHTTPServer, handler_for
from tools.targetvoice.model import Model, Extractor


class Resampler:
    """Causal 16/24/48k conversion through a 48k intermediate clock.

    Linear interpolation and block averaging have no packet-local padding or
    state resets. Native 16k avoids this conversion and preserves full fidelity.
    The maximum extra resampling delay is below 1ms, included in the advertised
    65ms upper bound (the PCM FIFO itself is 64ms).
    """

    def __init__(self, source, target):
        self.source, self.target = source, target
        self.previous = 0.0
        self.pending = []

    def process(self, samples):
        if self.source == self.target:
            return np.asarray(samples, dtype=np.float32)
        up, down = 48000 // self.source, 48000 // self.target
        out = []
        for sample in samples:
            for step in range(1, up + 1):
                self.pending.append(
                    self.previous + (float(sample) - self.previous) * step / up
                )
                if len(self.pending) == down:
                    out.append(sum(self.pending) / down)
                    self.pending.clear()
            self.previous = float(sample)
        return np.asarray(out, dtype=np.float32)


class Engine:
    def __init__(self, runtime, device, limit):
        self.model = Model(runtime, device)
        self.audio_worker = ThreadPoolExecutor(
            max_workers=1, thread_name_prefix="target-audio"
        )
        self.reference_worker = ThreadPoolExecutor(
            max_workers=1, thread_name_prefix="target-reference"
        )
        self.vads = [load_silero_vad(onnx=True) for _ in range(limit)]
        self.pool_lock = threading.Lock()
        self.audio_worker.submit(self.warm).result()

    def warm(self):
        for size in (512, 1024, 1536):
            extractor = Extractor(self.model, torch.ones(1, 1, 128, 1))
            for _ in range(10):
                extractor.process(np.zeros(size, dtype=np.float32))
        for vad in self.vads:
            vad(torch.zeros(512), 16000)
            vad.reset_states()
        self.model.enroll(np.ones(48000, dtype=np.float32) * 0.001)

    def take_vad(self):
        with self.pool_lock:
            if not self.vads:
                raise ValueError("target voice session limit reached")
            return self.vads.pop()

    def return_vad(self, vad):
        vad.reset_states()
        with self.pool_lock:
            self.vads.append(vad)


class Stream:
    def __init__(self, engine, rate):
        self.model, self.rate = engine, rate
        self.sequence, self.retired = 0, False
        self.last_used = time.monotonic()
        self.lock = threading.Lock()
        self.vad = engine.take_vad()
        self.phase = "waiting-for-speech"
        self.reference = []
        self.reference_samples = 0
        self.reference_future = None
        self.extractor = None
        self.down = Resampler(rate, 16000)
        self.up = Resampler(16000, rate)
        self.pending = np.empty(0, dtype=np.float32)
        self.output = deque([0.0] * (rate * 64 // 1000))
        self.closed = False

    def process(self, pcm):
        try:
            return self.model.audio_worker.submit(self._process, pcm).result()
        except Exception as error:
            self.phase = "failed"
            raise ValueError("target audio processing failed") from error

    def _process(self, pcm):
        samples = np.frombuffer(pcm, dtype="<i2").astype(np.float32) / 32768
        self.pending = np.concatenate([self.pending, self.down.process(samples)])
        while len(self.pending) >= 512:
            if self.phase == "extracting":
                count = len(self.pending) // 512 * 512
                result = self.extractor.process(self.pending[:count])
                self.pending = self.pending[count:]
                self.output.extend(self.up.process(result))
                continue
            block, self.pending = self.pending[:512], self.pending[512:]
            if self.phase == "waiting-for-speech":
                probability = self.vad(torch.from_numpy(block.copy()), 16000).item()
                if probability >= 0.5:
                    self.phase = "collecting-reference"
            if self.phase == "collecting-reference":
                remaining = 48000 - self.reference_samples
                part = block[:remaining].copy()
                self.reference.append(part)
                self.reference_samples += len(part)
                if self.reference_samples == 48000:
                    reference = np.concatenate(self.reference)
                    self.reference.clear()
                    self.reference_future = self.model.reference_worker.submit(
                        self.model.model.enroll, reference
                    )
                    self.phase = "preparing-reference"
            if self.phase == "preparing-reference" and self.reference_future.done():
                try:
                    cue = self.reference_future.result()
                except Exception as error:
                    self.phase = "failed"
                    raise ValueError("target reference encoding failed") from error
                self.reference_future = None
                self.extractor = Extractor(self.model.model, cue)
                self.phase = "extracting"
            # Capture and encoding never block ASR. Once extraction activates,
            # an error terminates the session; there is no raw-audio fallback.
            result = self.extractor.process(block) if self.extractor else block
            self.output.extend(self.up.process(result))
        if len(self.output) < len(samples):
            raise ValueError("target extractor violated its streaming delay budget")
        result = np.fromiter((self.output.popleft() for _ in samples), dtype=np.float32)
        if not np.isfinite(result).all():
            raise ValueError("non-finite target audio")
        return np.clip(np.rint(result * 32768), -32768, 32767).astype("<i2").tobytes()

    def close(self):
        if self.closed:
            return
        self.closed = True
        if self.reference_future:
            self.reference_future.cancel()
        self.reference.clear()
        self.pending = np.empty(0, dtype=np.float32)
        self.output.clear()
        self.extractor = None
        self.model.return_vad(self.vad)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime", default=".runtime/targetvoice")
    parser.add_argument("--device", choices=["cpu", "cuda"], default="cuda")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8126)
    parser.add_argument("--max-sessions", type=int, default=4)
    args = parser.parse_args()
    if not 1 <= args.max_sessions <= 32:
        parser.error("max-sessions must be 1..32")
    engine = Engine(args.runtime, args.device, args.max_sessions)
    sessions = Sessions(engine, limit=args.max_sessions, stream_factory=Stream)
    server = ThreadingHTTPServer(
        (args.host, args.port),
        handler_for(
            sessions,
            model_name="real-tse",
            audio_delay_ms=65,
            frame_ms=32,
            target_speaker=True,
        ),
    )
    print(f"target voice ready at {args.host}:{args.port}", flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        engine.audio_worker.shutdown()
        engine.reference_worker.shutdown()


if __name__ == "__main__":
    main()
