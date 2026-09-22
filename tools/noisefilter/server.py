"""Session-local noise suppression before ASR. No transcript or speaker enrollment input.

The wire contract is PCM16LE, same number of samples in and out. Two models sit
behind it; both run 480-sample (10 ms) frames at 48 kHz with recurrent state
carried across packets, plus a fixed 10 ms FIFO that handles arbitrary packet
boundaries without padding each packet or resetting state:

- ``rnnoise`` (default): 10 ms algorithmic delay, so every sample is delayed by
  20 ms, including across utterance/commit boundaries.
- ``deepfilternet``: DeepFilterNet3 through libDF's C API (the upstream Rust/tract
  real-time path, the one its LADSPA plugin uses). Its 20 ms STFT window plus a
  2-frame (20 ms) lookahead is 30 ms of waveform delay at 48 kHz (window minus
  hop, plus lookahead), so with the FIFO every sample is delayed by 40 ms.
  Build the library and fetch the model with ``tools/noisefilter/build_deepfilter.sh``.
"""

import argparse
import array
import ctypes
import json
import math
import re
import socket
import sys
import threading
import time
from collections import deque
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class RNNoise:
    def __init__(self, library):
        self.lib = ctypes.CDLL(library)
        self.lib.rnnoise_create.argtypes = [ctypes.c_void_p]
        self.lib.rnnoise_create.restype = ctypes.c_void_p
        self.lib.rnnoise_destroy.argtypes = [ctypes.c_void_p]
        self.lib.rnnoise_process_frame.argtypes = [
            ctypes.c_void_p,
            ctypes.POINTER(ctypes.c_float),
            ctypes.POINTER(ctypes.c_float),
        ]
        self.lib.rnnoise_process_frame.restype = ctypes.c_float
        self.lib.rnnoise_get_frame_size.restype = ctypes.c_int
        if self.lib.rnnoise_get_frame_size() != 480:
            raise ValueError("RNNoise must use 480-sample / 10ms frames")

    def state(self):
        state = self.lib.rnnoise_create(None)
        if not state:
            raise RuntimeError("RNNoise allocation failed")
        return state


class Stream:
    # Sample scale the model's frame function expects (RNNoise: 16-bit range).
    scale = 1.0

    def __init__(self, model, rate):
        if rate not in (16000, 24000, 48000):
            raise ValueError("rate must be 16000, 24000, or 48000")
        self.model, self.rate = model, rate
        self.state = model.state()
        self.sequence = 0
        self.retired = False
        self.last_used = time.monotonic()
        self.pending = []
        self.output = deque([0] * (rate // 100))
        self.previous = 0.0
        self.buffer = (ctypes.c_float * 480)()
        self.lock = threading.Lock()

    def process(self, pcm):
        samples = array.array("h")
        samples.frombytes(pcm)
        if sys.byteorder != "little":
            samples.byteswap()
        # Causal linear interpolation to 48kHz; matched block-average decimation
        # back to the original rate. No packet-dependent resampler resets.
        factor = 48000 // self.rate
        for sample in samples:
            for step in range(1, factor + 1):
                self.pending.append(
                    self.previous + (sample - self.previous) * step / factor
                )
            self.previous = float(sample)
            if len(self.pending) == 480:
                if self.scale == 1.0:
                    self.buffer[:] = self.pending
                else:
                    self.buffer[:] = [value * self.scale for value in self.pending]
                self.process_frame()
                for index in range(0, 480, factor):
                    value = sum(self.buffer[index : index + factor]) / factor / self.scale
                    if not math.isfinite(value):
                        raise ValueError("non-finite filtered audio")
                    self.output.append(max(-32768, min(32767, round(value))))
                self.pending.clear()
        result = array.array("h", (self.output.popleft() for _ in samples))
        if sys.byteorder != "little":
            result.byteswap()
        return result.tobytes()

    def process_frame(self):
        self.model.lib.rnnoise_process_frame(self.state, self.buffer, self.buffer)

    def close(self):
        if self.state:
            self.model.lib.rnnoise_destroy(self.state)
            self.state = None


class DeepFilterNet:
    """DeepFilterNet3 via libDF's C API (``df_create``/``df_process_frame``).

    ``df_create`` parses the ONNX archive and plans the tract graphs, which takes
    far longer than one packet's budget, so fresh states are created ahead of
    time on a background thread and a new session takes one from the pool. A
    state is never reused across sessions (it carries recurrent history).
    """

    audio_delay_ms = 40
    frame_ms = 10

    def __init__(self, library, model_path, atten_lim_db=100.0, pool=2):
        self.lib = ctypes.CDLL(library)
        self.lib.df_create.argtypes = [ctypes.c_char_p, ctypes.c_float]
        self.lib.df_create.restype = ctypes.c_void_p
        self.lib.df_get_frame_length.argtypes = [ctypes.c_void_p]
        self.lib.df_get_frame_length.restype = ctypes.c_size_t
        self.lib.df_process_frame.argtypes = [
            ctypes.c_void_p,
            ctypes.POINTER(ctypes.c_float),
            ctypes.POINTER(ctypes.c_float),
        ]
        self.lib.df_process_frame.restype = ctypes.c_float
        self.lib.df_free.argtypes = [ctypes.c_void_p]
        self.model_path = str(model_path).encode()
        self.atten_lim_db = float(atten_lim_db)
        probe = self.create()
        try:
            if self.lib.df_get_frame_length(probe) != 480:
                raise ValueError("DeepFilterNet must use 480-sample / 10ms frames")
        finally:
            self.lib.df_free(probe)
        self.pool_size = max(0, pool)
        self.ready = deque()
        self.pool_lock = threading.Condition()
        self.closed = False
        if self.pool_size:
            threading.Thread(target=self._refill, daemon=True).start()

    def create(self):
        state = self.lib.df_create(self.model_path, self.atten_lim_db)
        if not state:
            raise RuntimeError("DeepFilterNet allocation failed")
        return state

    def _refill(self):
        while True:
            with self.pool_lock:
                while not self.closed and len(self.ready) >= self.pool_size:
                    self.pool_lock.wait()
                if self.closed:
                    return
            state = self.create()
            with self.pool_lock:
                self.ready.append(state)
                self.pool_lock.notify_all()

    def state(self):
        with self.pool_lock:
            if self.ready:
                state = self.ready.popleft()
                self.pool_lock.notify_all()
                return state
        # Pool exhausted: a session start pays the model build; later packets do not.
        return self.create()

    def wait_ready(self, timeout=60):
        deadline = time.monotonic() + timeout
        with self.pool_lock:
            while len(self.ready) < self.pool_size:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not self.pool_lock.wait(remaining):
                    return len(self.ready) >= self.pool_size
        return True


class DeepFilterStream(Stream):
    # libDF takes float audio in [-1, 1].
    scale = 1.0 / 32768.0

    def process_frame(self):
        self.model.lib.df_process_frame(self.state, self.buffer, self.buffer)

    def close(self):
        if self.state:
            self.model.lib.df_free(self.state)
            self.state = None


class Sessions:
    def __init__(self, model, limit=32, ttl=120, stream_factory=Stream):
        self.model, self.limit, self.ttl = model, limit, ttl
        self.stream_factory = stream_factory
        self.streams = {}
        self.lock = threading.Lock()

    def acquire(self, identity, rate, sequence):
        with self.lock:
            now = time.monotonic()
            for key, stream in list(self.streams.items()):
                if now - stream.last_used > self.ttl and stream.lock.acquire(
                    blocking=False
                ):
                    try:
                        stream.close()
                        del self.streams[key]
                    finally:
                        stream.lock.release()
            stream = self.streams.get(identity)
            if stream is None:
                if sequence != 0:
                    raise ValueError(
                        "expired or missing stream; cannot resume recurrent state"
                    )
                if len(self.streams) >= self.limit:
                    raise ValueError("session limit reached")
                stream = self.stream_factory(self.model, rate)
                self.streams[identity] = stream
            if stream.retired:
                raise ValueError("stream is retiring")
            if not stream.lock.acquire(blocking=False):
                raise ValueError("concurrent request for one stream")
            if stream.rate != rate or stream.sequence != sequence:
                stream.lock.release()
                raise ValueError("sample rate or sequence mismatch")
            stream.last_used = now
            return stream

    def delete(self, identity):
        with self.lock:
            stream = self.streams.get(identity)
            if stream:
                stream.retired = True
            if stream and stream.lock.acquire(blocking=False):
                try:
                    stream.close()
                    del self.streams[identity]
                finally:
                    stream.lock.release()

    def release(self, identity, stream):
        with self.lock:
            stream.lock.release()
            if stream.retired and self.streams.get(identity) is stream:
                stream.close()
                del self.streams[identity]


def handler_for(
    sessions, model_name="rnnoise", audio_delay_ms=20, frame_ms=10, target_speaker=False
):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def setup(self):
            super().setup()
            self.connection.settimeout(5)
            self.connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)

        def log_message(self, *_):
            pass

        def identity(self):
            match = re.fullmatch(r"/v1/filter/([a-f0-9]{32})", self.path)
            if not match:
                raise ValueError("invalid session path")
            return match.group(1)

        def do_GET(self):
            if self.path != "/health":
                self.send_error(404)
                return
            payload = json.dumps(
                {
                    "model": model_name,
                    "audio_delay_ms": audio_delay_ms,
                    "frame_ms": frame_ms,
                    "target_speaker": target_speaker,
                }
            ).encode()
            self.reply(200, payload, "application/json")

        def do_DELETE(self):
            try:
                sessions.delete(self.identity())
                self.reply(200, b"")
            except ValueError as error:
                self.send_error(400, str(error))

        def do_POST(self):
            started = time.perf_counter()
            stream = None
            identity = None
            try:
                identity = self.identity()
                rate = int(self.headers.get("X-Sample-Rate", "0"))
                sequence = int(self.headers.get("X-Sequence", "-1"))
                length = int(self.headers.get("Content-Length", "0"))
                if rate not in (16000, 24000, 48000) or sequence < 0:
                    raise ValueError("invalid sample rate or sequence")
                if not 0 < length <= rate // 10 * 2 or length % 2:
                    raise ValueError(
                        "request must contain at most 100ms complete PCM16 samples"
                    )
                if self.headers.get("Transfer-Encoding"):
                    raise ValueError("chunked requests are unsupported")
                pcm = self.rfile.read(length)
                if len(pcm) != length:
                    raise ValueError("truncated PCM")
                stream = sessions.acquire(identity, rate, sequence)
                result = stream.process(pcm)
                stream.sequence += 1
                elapsed = (time.perf_counter() - started) * 1000
                self.send_response(200)
                self.send_header("Content-Type", "application/octet-stream")
                self.send_header("Content-Length", str(len(result)))
                self.send_header("X-Sequence", str(sequence))
                self.send_header("X-Filter-Model", model_name)
                self.send_header("X-Audio-Delay-MS", str(audio_delay_ms))
                if target_speaker:
                    self.send_header("X-Target-Voice-State", stream.phase)
                self.send_header("X-Processing-MS", f"{elapsed:.3f}")
                self.end_headers()
                self.wfile.write(result)
            except (ValueError, OSError) as error:
                if stream:
                    sessions.delete(identity)
                self.close_connection = True
                try:
                    self.send_error(400, str(error))
                except OSError:
                    pass
            finally:
                if stream:
                    sessions.release(identity, stream)

        def reply(self, status, body, content_type="application/octet-stream"):
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    return Handler


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", choices=["rnnoise", "deepfilternet"], default="rnnoise")
    parser.add_argument(
        "--library", required=True, help="librnnoise.so, or libdf.so for deepfilternet"
    )
    parser.add_argument(
        "--deepfilter-model",
        help="DeepFilterNet3 ONNX archive (DeepFilterNet3_onnx.tar.gz) for --model deepfilternet",
    )
    parser.add_argument(
        "--atten-lim-db",
        type=float,
        default=100.0,
        help="deepfilternet attenuation limit in dB (100 = upstream default, unlimited)",
    )
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8125)
    parser.add_argument("--max-sessions", type=int, default=32)
    args = parser.parse_args()
    if args.max_sessions < 1:
        parser.error("max-sessions must be positive")
    if args.model == "deepfilternet":
        if not args.deepfilter_model:
            parser.error("--model deepfilternet requires --deepfilter-model")
        model = DeepFilterNet(args.library, args.deepfilter_model, args.atten_lim_db)
        stream_factory, delay, frame = DeepFilterStream, model.audio_delay_ms, model.frame_ms
    else:
        model = RNNoise(args.library)
        stream_factory, delay, frame = Stream, 20, 10
    warm = stream_factory(model, 24000)
    for _ in range(10):
        warm.process(bytes(4800))
    warm.close()
    if args.model == "deepfilternet":
        model.wait_ready()
    server = ThreadingHTTPServer(
        (args.host, args.port),
        handler_for(
            Sessions(model, args.max_sessions, stream_factory=stream_factory),
            model_name=args.model,
            audio_delay_ms=delay,
            frame_ms=frame,
        ),
    )
    print(
        json.dumps(
            {
                "ready": True,
                "model": args.model,
                "audio_delay_ms": delay,
                "address": server.server_address,
            }
        ),
        flush=True,
    )
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
