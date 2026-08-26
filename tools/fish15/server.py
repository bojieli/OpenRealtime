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
import os
import queue
import struct
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import soundfile
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


ENROLMENT = (
    "This is my voice. I sound like this whenever I speak, "
    "in this room and in this conversation."
)


def wav_file(pcm, sample_rate=SAMPLE_RATE, channels=1, bits=16):
    """A complete RIFF file, for a caller that wanted the whole utterance."""
    block_align = channels * bits // 8
    return (
        b"RIFF"
        + struct.pack("<I", 36 + len(pcm))
        + b"WAVEfmt "
        + struct.pack("<IHHIIHH", 16, 1, channels, sample_rate,
                      sample_rate * block_align, block_align, bits)
        + b"data"
        + struct.pack("<I", len(pcm))
        + pcm
    )


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

    def __init__(self, checkpoint, device, compile_graphs, voice_dir):
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
        self.voice_dir = voice_dir
        self.voices = {}
        self.voice_lock = threading.Lock()
        os.makedirs(voice_dir, exist_ok=True)

    def synthesize(self, request, voice=None):
        """Yield int16 PCM for one request, a text chunk at a time."""
        tokens, texts = ([], [])
        if voice:
            tokens, texts = self.voice_prompt(voice)
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
            prompt_tokens=tokens,
            prompt_text=texts,
        ), replies))

        while True:
            wrapped = replies.get()
            if wrapped.status == "error":
                raise RuntimeError(str(wrapped.response))
            reply = wrapped.response
            if reply.action == "next":
                return
            yield self.decode(reply.codes)

    def voice_prompt(self, name):
        """The prompt tokens that make this speaker sound like themselves.

        Fish is zero-shot: asked for speech with no reference it invents a
        speaker, and it invents a different one every call. That is fine for a
        demo and wrong for anything that has to sound like the same person
        twice - measured on the benchmark's own audio, two lines from the same
        scripted speaker embedded 0.27 apart, which is what two strangers score.
        A scenario about a third party talking near the microphone cannot pose
        that case if the user's own voice changes every sentence.

        So a speaker is enrolled once, on first use, and kept: the enrolment
        audio is written to disk, so restarting the server does not give
        everybody a new voice either.
        """
        with self.voice_lock:
            if name in self.voices:
                return self.voices[name]
        path = os.path.join(self.voice_dir, f"{name}.wav")
        if not os.path.exists(path):
            pcm = b"".join(self.synthesize(defaults(ENROLMENT)))
            with open(path, "wb") as handle:
                handle.write(wav_file(pcm))
            print(f"enrolled voice {name!r} from {len(pcm) / 2 / SAMPLE_RATE:.1f}s", flush=True)
        prompt = ([self.encode_reference(path)], [ENROLMENT])
        with self.voice_lock:
            self.voices[name] = prompt
        return prompt

    def encode_reference(self, path):
        # soundfile rather than torchaudio.load: 2.10 routes loading through
        # torchcodec, which is not installed and is a decoder we do not need
        # for a mono RIFF file this process wrote itself.
        samples, rate = soundfile.read(path, dtype="float32", always_2d=True)
        audio = torch.from_numpy(samples.T)
        if rate != self.decoder.spec_transform.sample_rate:
            audio = torchaudio.functional.resample(
                audio, rate, self.decoder.spec_transform.sample_rate)
        audio = audio.mean(dim=0, keepdim=True)[None].to(self.device)
        lengths = torch.tensor([audio.shape[2]], device=self.device, dtype=torch.long)
        with self.decode_lock:
            return self.decoder.encode(audio, lengths)[0][0]

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
            # A named voice is a speaker who has to sound the same every time
            # they talk. "default" is a name like any other.
            voice = (body.get("voice") or body.get("reference_id") or "").strip()

            started = time.perf_counter()
            # Streaming is for the agent, where the first sample matters more
            # than the file. A caller that wants the utterance gets a complete
            # RIFF file with real lengths in it rather than a stream with the
            # placeholders a decoder then has to know to ignore.
            if not body.get("streaming"):
                try:
                    pcm = b"".join(speech.synthesize(request, voice))
                except RuntimeError as error:
                    print(f"synthesis failed: {error}", file=sys.stderr, flush=True)
                    self.send_error(500, str(error))
                    return
                payload = wav_file(pcm)
                self.send_response(200)
                self.send_header("Content-Type", "audio/wav")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
                print(f'{(time.perf_counter() - started) * 1000:.0f} ms for '
                      f'"{text[:40]}" as {voice or "nobody in particular"}', flush=True)
                return

            self.send_response(200)
            self.send_header("Content-Type", "audio/wav")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()

            first = None
            try:
                self.write_chunk(wav_header())
                for pcm in speech.synthesize(request, voice):
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
    parser.add_argument("--voices", default=".runtime/fish-voices",
                        help="where enrolled speaker references are kept")
    arguments = parser.parse_args()

    started = time.perf_counter()
    speech = Speech(arguments.checkpoint, arguments.device, not arguments.no_compile,
                    arguments.voices)
    speech.warm()
    print(f"ready on :{arguments.port} after {time.perf_counter() - started:.1f}s", flush=True)

    ThreadingHTTPServer(("127.0.0.1", arguments.port), handler_for(speech)).serve_forever()


if __name__ == "__main__":
    main()
