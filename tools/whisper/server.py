"""Whisper large-v3-turbo behind the transcription API the runtime already speaks.

SenseVoice was the recogniser, and it is fast and small and cannot say
"capybara". Round-tripped through this project's own synthesiser, "A capybara
wandered over and sat down next to me" came back as "A ki bara wandered over
and SAT down next to me", and in the live pipeline as "A cap borroworer" and
"A capy borroworough1". A scenario about counting animals cannot be measured
through a recogniser that deletes the animal, and neither can a phone call
about a person's name.

Turbo is both more accurate and faster here: 84 to 104 milliseconds against
SenseVoice's 271 on the same clips, because it decodes with a fraction of the
layers of full large-v3.

The wire contract is OpenAI's transcription route, which is what the runtime
already talks, so switching recognisers is a URL and not a code change.
"""

import argparse
import io
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from faster_whisper import WhisperModel


class Recogniser:
    """Owns the model and serialises access to it.

    One CUDA stream shared by concurrent decodes interleaves badly, and an
    utterance is short enough that queueing costs less than the contention.
    """

    def __init__(self, model, device, compute_type, language):
        self.model = WhisperModel(model, device=device, compute_type=compute_type)
        self.language = language
        self.lock = threading.Lock()

    def transcribe(self, audio):
        with self.lock:
            # Greedy, because this runs on the path between somebody speaking
            # and the agent deciding whether to answer. A beam buys accuracy
            # that a partial re-transcribed three hundred milliseconds later
            # will supersede anyway.
            # transcribe, never translate. Whisper can do both, and told the
            # audio is English when it is not, it produces English - which is
            # translation wearing a recogniser's clothes. Measured, a Mandarin
            # line came back as "Hello. I'm very happy to meet you.", so an
            # agent asked to interpret was handed the interpretation and asked
            # to interpret it again. It said "man. man. man. man."
            #
            # A recogniser that rewrites what somebody said is worse than one
            # that hears them badly, because nothing downstream can tell.
            segments, info = self.model.transcribe(
                io.BytesIO(audio), language=self.language, task="transcribe",
                beam_size=1, condition_on_previous_text=False,
            )
            return "".join(segment.text for segment in segments).strip(), info.language

    def warm(self, audio):
        try:
            self.transcribe(audio)
        except Exception as error:  # noqa: BLE001 - reported, not swallowed
            print(f"warm-up failed: {error}", file=sys.stderr, flush=True)


def field(body, boundary, name):
    """Pull one multipart field out without a dependency."""
    marker = f'name="{name}"'.encode()
    for part in body.split(b"--" + boundary):
        if marker not in part:
            continue
        head, _, payload = part.partition(b"\r\n\r\n")
        return payload.rsplit(b"\r\n", 1)[0]
    return None


def handler_for(recogniser):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def do_POST(self):
            if not self.path.rstrip("/").endswith("/audio/transcriptions"):
                self.send_error(404)
                return
            content_type = self.headers.get("Content-Type", "")
            if "boundary=" not in content_type:
                self.send_error(400, "expected multipart/form-data")
                return
            boundary = content_type.split("boundary=", 1)[1].strip().strip('"').encode()
            body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
            audio = field(body, boundary, "file")
            if not audio:
                self.send_error(400, "no file")
                return
            started = time.perf_counter()
            try:
                text, language = recogniser.transcribe(audio)
            except Exception as error:  # noqa: BLE001 - reported, not swallowed
                print(f"transcription failed: {error}", file=sys.stderr, flush=True)
                self.send_error(500, str(error))
                return
            payload = json.dumps({"text": text, "language": language}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            print(f"{(time.perf_counter() - started) * 1000:.0f} ms  {text[:60]!r}", flush=True)

        def do_GET(self):
            if self.path.rstrip("/").endswith("/models"):
                payload = json.dumps({"object": "list", "data": [{"id": "whisper-turbo"}]}).encode()
            elif self.path.rstrip("/") == "/health":
                payload = b"ok"
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    return Handler


def silence(seconds=1.0, rate=16_000):
    import struct
    frames = b"\x00\x00" * int(rate * seconds)
    return (b"RIFF" + struct.pack("<I", 36 + len(frames)) + b"WAVEfmt "
            + struct.pack("<IHHIIHH", 16, 1, 1, rate, rate * 2, 2, 16)
            + b"data" + struct.pack("<I", len(frames)) + frames)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", default="mobiuslabsgmbh/faster-whisper-large-v3-turbo")
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--compute-type", default="float16")
    # Empty means detect it. A fixed language is a claim about what is about
    # to be said, and it is wrong the moment somebody else in the room speaks
    # another one - which is the case two scenarios in this suite exist to
    # test. Detection costs a little on very short audio and buys not silently
    # rewriting what people say.
    parser.add_argument("--language", default="")
    parser.add_argument("--port", type=int, default=8003)
    arguments = parser.parse_args()

    started = time.perf_counter()
    recogniser = Recogniser(arguments.model, arguments.device,
                            arguments.compute_type, arguments.language or None)
    recogniser.warm(silence())
    print(f"ready on :{arguments.port} after {time.perf_counter() - started:.1f}s", flush=True)
    ThreadingHTTPServer(("127.0.0.1", arguments.port), handler_for(recogniser)).serve_forever()


if __name__ == "__main__":
    main()
