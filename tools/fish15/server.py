"""A Fish Speech 1.5 server that keeps first-audio latency near the model's floor.

The upstream API server routes every request through TTSInferenceEngine, which
empties the CUDA caching allocator and runs a full garbage collection after each
synthesis.  Both cost more than the synthesis itself: measured against the same
checkpoint, generation of a one-second phrase takes about 90ms and the vocoder
about 40ms, while the upstream server answers the identical request in 375ms.

This server speaks the same JSON /v1/tts contract, so the Go adapter that already
targets Fish Speech needs no change, but it goes straight from the semantic
token queue to the decoder and leaves the allocator alone.
"""

import argparse
import json
import queue
import struct
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch
import torchaudio

# torchaudio dropped list_audio_backends after the 1.5 release pinned it.  The
# reference loader only uses it to name a decoder, and soundfile is the one the
# probe would settle on anyway.
if not hasattr(torchaudio, "list_audio_backends"):
    torchaudio.list_audio_backends = lambda: ["soundfile"]

from fish_speech.models.text2semantic.inference import (  # noqa: E402
    GenerateRequest,
    launch_thread_safe_queue,
)
from fish_speech.models.vqgan.inference import load_model as load_decoder  # noqa: E402

SAMPLE_RATE = 44_100
AMPLITUDE = 32768


def wav_header(sample_rate=SAMPLE_RATE, channels=1, bits=16):
    """A RIFF header for a stream of unknown length.

    Streaming servers cannot know the total size in advance, so the two length
    fields carry the conventional placeholder.  The Go adapter ignores them.
    """
    block_align = channels * bits // 8
    return (
        b"RIFF"
        + struct.pack("<I", 0xFFFFFFFF)
        + b"WAVEfmt "
        + struct.pack("<IHHIIHH", 16, 1, channels, sample_rate,
                      sample_rate * block_align, block_align, bits)
        + b"data"
        + struct.pack("<I", 0xFFFFFFFF)
    )


class Speech:
    """Owns the two models and serialises access to them.

    The semantic model is served by its own worker thread behind a queue, so it
    is already serialised; the decoder is not, and one CUDA stream shared by
    concurrent decodes would interleave badly.  A single lock over the decode
    keeps the ordering honest without a second worker.
    """

    def __init__(self, checkpoint, device, compile_graphs):
        self.queue = launch_thread_safe_queue(
            checkpoint_path=checkpoint, device=device,
            precision=torch.half, compile=compile_graphs,
        )
        self.decoder = load_decoder(
            config_name="firefly_gan_vq",
            checkpoint_path=f"{checkpoint}/firefly-gan-vq-fsq-8x1024-21hz-generator.pth",
            device=device,
        )
        self.device = device
        self.compile_graphs = compile_graphs
        self.decode_lock = threading.Lock()

    def synthesize(self, request):
        """Yield int16 PCM for one request, a text chunk at a time."""
        replies = queue.Queue()
        self.queue.put(GenerateRequest(dict(
            device=self.device,
            max_new_tokens=request["max_new_tokens"],
            text=request["text"],
            top_p=request["top_p"],
            repetition_penalty=request["repetition_penalty"],
            temperature=request["temperature"],
            compile=self.compile_graphs,
            iterative_prompt=request["chunk_length"] > 0,
            chunk_length=request["chunk_length"],
            max_length=2048,
            prompt_tokens=[],
            prompt_text=[],
        ), replies))

        while True:
            wrapped = replies.get()
            if wrapped.status == "error":
                raise RuntimeError(str(wrapped.response))
            reply = wrapped.response
            if reply.action == "next":
                return
            yield self.decode(reply.codes)

    def decode(self, codes):
        with self.decode_lock:
            lengths = torch.tensor([codes.shape[1]], device=codes.device, dtype=torch.long)
            audio, _ = self.decoder.decode(indices=codes[None], feature_lengths=lengths)
            clipped = audio[0, 0].float().clamp(-1.0, 1.0)
            return (clipped * AMPLITUDE).to(torch.int16).cpu().numpy().tobytes()

    def warm(self, rounds=3):
        """Capture the CUDA graphs and the decoder's shapes before serving.

        Without this the first requests pay compilation, which is minutes with
        graphs enabled and would otherwise land on a caller.
        """
        for i in range(rounds):
            for _ in self.synthesize(defaults(f"Warming up, round {i + 1}.")):
                pass


def defaults(text):
    return {
        "text": text, "chunk_length": 200, "max_new_tokens": 1024,
        "top_p": 0.7, "repetition_penalty": 1.2, "temperature": 0.7,
    }


def handler_for(speech):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def do_POST(self):
            if self.path.rstrip("/") not in ("/v1/tts", "/v1/audio/speech"):
                self.send_error(404)
                return
            try:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            except (TypeError, ValueError) as error:
                self.send_error(400, str(error))
                return

            text = (body.get("text") or body.get("input") or "").strip()
            if not text:
                self.send_error(400, "no text")
                return

            request = defaults(text)
            for field in ("chunk_length", "max_new_tokens", "top_p",
                          "repetition_penalty", "temperature"):
                if body.get(field):
                    request[field] = body[field]

            started = time.perf_counter()
            self.send_response(200)
            self.send_header("Content-Type", "audio/wav")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()

            first = None
            try:
                self.write_chunk(wav_header())
                for pcm in speech.synthesize(request):
                    if first is None:
                        first = (time.perf_counter() - started) * 1000
                    self.write_chunk(pcm)
                self.write_chunk(b"")
            except (BrokenPipeError, ConnectionResetError):
                return
            except RuntimeError as error:
                print(f"synthesis failed: {error}", file=sys.stderr, flush=True)
                return
            print(f'first audio {first:.0f} ms for "{text[:48]}"', flush=True)

        def write_chunk(self, payload):
            self.wfile.write(b"%x\r\n" % len(payload) + payload + b"\r\n")
            self.wfile.flush()

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
    parser.add_argument("--checkpoint", default="checkpoints/fish-speech-1.5")
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--port", type=int, default=8080)
    parser.add_argument("--no-compile", action="store_true")
    arguments = parser.parse_args()

    started = time.perf_counter()
    speech = Speech(arguments.checkpoint, arguments.device, not arguments.no_compile)
    speech.warm()
    print(f"ready on :{arguments.port} after {time.perf_counter() - started:.1f}s", flush=True)

    ThreadingHTTPServer(("127.0.0.1", arguments.port), handler_for(speech)).serve_forever()


if __name__ == "__main__":
    main()
