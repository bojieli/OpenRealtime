"""Speaker embeddings, so the agent can tell who is talking.

A cascade hears one microphone and transcribes it, and everything downstream is
text. That loses the one fact that separates a question put to the agent from a
question put to somebody else in the room: whose voice it was. Without it the
situation handed to the interaction model asserts that the user said whatever
arrived on the microphone, which is an attribution nothing supports - and the
model, told the user asked about the milk, answers about the milk.

This serves ECAPA-TDNN embeddings over raw PCM. Comparing them is the caller's
business: the threshold is a policy decision and policy does not belong here.
"""

import argparse
import json
import struct
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import torch
import torchaudio

try:
    from speechbrain.inference.speaker import EncoderClassifier
except ImportError:  # speechbrain moved this between releases
    from speechbrain.pretrained import EncoderClassifier

MODEL_RATE = 16_000


class Voices:
    """Owns the encoder and serialises access to it.

    One CUDA stream shared by concurrent encodes interleaves badly, and an
    utterance is short enough that queueing costs less than the contention.
    """

    def __init__(self, source, device):
        self.model = EncoderClassifier.from_hparams(source=source, run_opts={"device": device})
        self.device = device
        self.lock = threading.Lock()
        self.resamplers = {}

    def resampler(self, rate):
        if rate not in self.resamplers:
            self.resamplers[rate] = torchaudio.transforms.Resample(rate, MODEL_RATE).to(self.device)
        return self.resamplers[rate]

    def embed(self, pcm16, rate):
        audio = torch.from_numpy(
            np.frombuffer(pcm16, dtype="<i2").astype(np.float32) / 32768.0
        ).to(self.device)
        with self.lock:
            if rate != MODEL_RATE:
                audio = self.resampler(rate)(audio)
            vector = self.model.encode_batch(audio[None])[0, 0]
            # Unit length, so the caller's comparison is a dot product and two
            # embeddings of the same voice at different volumes agree.
            vector = vector / vector.norm().clamp(min=1e-9)
            return vector.detach().cpu().numpy().tolist()

    def warm(self):
        self.embed(np.zeros(MODEL_RATE, dtype="<i2").tobytes(), MODEL_RATE)


def handler_for(voices):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def do_POST(self):
            if self.path.rstrip("/") != "/embed":
                self.send_error(404)
                return
            rate = int(self.headers.get("X-Sample-Rate") or MODEL_RATE)
            length = int(self.headers.get("Content-Length") or 0)
            pcm = self.rfile.read(length)
            if len(pcm) < rate // 4:  # under 250ms is not a voice, it is a noise
                self.reply({"embedding": None, "reason": "too short"})
                return
            started = time.perf_counter()
            try:
                vector = voices.embed(pcm, rate)
            except Exception as error:  # noqa: BLE001 - reported, not swallowed
                print(f"embed failed: {error}", file=sys.stderr, flush=True)
                self.send_error(500, str(error))
                return
            self.reply({"embedding": vector, "took_ms": (time.perf_counter() - started) * 1000})

        def reply(self, payload):
            body = json.dumps(payload).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path.rstrip("/") != "/health":
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"ok")

    return Handler


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", default="speechbrain/spkrec-ecapa-voxceleb")
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--port", type=int, default=8124)
    arguments = parser.parse_args()

    started = time.perf_counter()
    voices = Voices(arguments.source, arguments.device)
    voices.warm()
    print(f"ready on :{arguments.port} after {time.perf_counter() - started:.1f}s", flush=True)
    ThreadingHTTPServer(("127.0.0.1", arguments.port), handler_for(voices)).serve_forever()


if __name__ == "__main__":
    main()
