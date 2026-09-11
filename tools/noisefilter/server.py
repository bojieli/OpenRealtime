"""Session-local RNNoise before ASR. No transcript or speaker enrollment input.

The wire contract is PCM16LE, same number of samples in and out. RNNoise has
10ms algorithmic delay; an additional fixed 10ms FIFO handles arbitrary packet
boundaries without padding each packet or resetting recurrent state. Thus every
sample is delayed by 20ms, including across utterance/commit boundaries.
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
                self.buffer[:] = self.pending
                self.model.lib.rnnoise_process_frame(
                    self.state, self.buffer, self.buffer
                )
                for index in range(0, 480, factor):
                    value = sum(self.buffer[index : index + factor]) / factor
                    if not math.isfinite(value):
                        raise ValueError("non-finite filtered audio")
                    self.output.append(max(-32768, min(32767, round(value))))
                self.pending.clear()
        result = array.array("h", (self.output.popleft() for _ in samples))
        if sys.byteorder != "little":
            result.byteswap()
        return result.tobytes()

    def close(self):
        if self.state:
            self.model.lib.rnnoise_destroy(self.state)
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
    parser.add_argument("--library", required=True)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8125)
    parser.add_argument("--max-sessions", type=int, default=32)
    args = parser.parse_args()
    if args.max_sessions < 1:
        parser.error("max-sessions must be positive")
    model = RNNoise(args.library)
    warm = Stream(model, 24000)
    for _ in range(10):
        warm.process(bytes(4800))
    warm.close()
    server = ThreadingHTTPServer(
        (args.host, args.port), handler_for(Sessions(model, args.max_sessions))
    )
    print(
        json.dumps(
            {"ready": True, "model": "rnnoise", "address": server.server_address}
        ),
        flush=True,
    )
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
