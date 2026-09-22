#!/usr/bin/env python3
"""FunASR's streaming Paraformer behind the duplex-plan recognition contract.

The mature Mandarin streaming baseline of the plan's ASR survey: a chunked
conformer encoder with CIF token emission and cached lookahead, which is
neither an autoregressive decoder nor a re-read of a growing recording. Its
chunk configuration is ``[left, current, right]`` in 60 ms units, so the
default ``0,10,5`` is a 600 ms chunk with 300 ms of lookahead, and the
lookahead is the latency the accuracy is bought with - both halves are
configurable here and reported in /health.

Emission is append-only (CIF fires a token and moves on), so every chunk's
text is committed; the service reports ``stability="decoder-append-only"``
and the committed prefix, and the ``--verify-append-only`` flag checks that
claim over a fixture instead of asserting it.

    .runtime/sensevoice/bin/python tools/duplexmodels/asr_funasr.py --port 9113
    .runtime/sensevoice/bin/python tools/duplexmodels/asr_funasr.py --verify-append-only .runtime/duplex-plan/fixtures/asr-zh.jsonl
"""

import argparse
import json
import sys
import time
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import Hypothesis, Recognizer, RecognizerSession, configure_logging, log, serve_recognizer  # noqa: E402

RATE = 16_000
UNIT_MS = 60  # one chunk-size unit of the streaming Paraformer


class ParaformerSession(RecognizerSession):
    def __init__(self, model, chunk_size, look_back, decoder_look_back):
        self.model = model
        self.chunk_size = chunk_size
        self.look_back = look_back
        self.decoder_look_back = decoder_look_back
        self.samples = int(chunk_size[1] * UNIT_MS / 1000 * RATE)
        self.cache = {}
        self.buffer = np.zeros(0, dtype=np.float32)
        self.text = ""

    def _run(self, audio: np.ndarray, final: bool) -> None:
        result = self.model.generate(
            input=audio, cache=self.cache, is_final=final, chunk_size=self.chunk_size,
            encoder_chunk_look_back=self.look_back, decoder_chunk_look_back=self.decoder_look_back,
            disable_pbar=True,
        )
        for item in result or []:
            piece = (item.get("text") or "").strip()
            if piece:
                # Mandarin needs no separator; keep one for Latin script.
                separator = "" if not self.text or _cjk(piece[0]) else " "
                self.text += separator + piece

    def push(self, audio: np.ndarray) -> Hypothesis:
        self.buffer = np.concatenate([self.buffer, audio])
        while len(self.buffer) >= self.samples:
            block, self.buffer = self.buffer[: self.samples], self.buffer[self.samples:]
            self._run(block, False)
        return Hypothesis(text=self.text, stable_text=self.text, language="zh")

    def finish(self) -> Hypothesis:
        if len(self.buffer):
            self._run(self.buffer, True)
            self.buffer = np.zeros(0, dtype=np.float32)
        else:
            self._run(np.zeros(self.samples, dtype=np.float32), True)
        return Hypothesis(text=self.text, stable_text=self.text, language="zh")


def _cjk(character: str) -> bool:
    return "぀" <= character <= "鿿"


class Paraformer(Recognizer):
    stability = "decoder-append-only"

    def __init__(self, name: str, chunk_size, look_back: int, decoder_look_back: int, device: str) -> None:
        from funasr import AutoModel  # noqa: PLC0415

        self.model_name = name
        self.model = f"{name} chunk={chunk_size} lookback={look_back}"
        self.chunk_size = chunk_size
        self.look_back = look_back
        self.decoder_look_back = decoder_look_back
        log.info("loading %s on %s", name, device)
        self.engine = AutoModel(model=name, device=device, disable_update=True)

    def start(self) -> RecognizerSession:
        return ParaformerSession(self.engine, self.chunk_size, self.look_back, self.decoder_look_back)

    def health(self) -> dict:
        return {"chunk_ms": self.chunk_size[1] * UNIT_MS, "lookahead_ms": self.chunk_size[2] * UNIT_MS,
                "encoder_chunk_look_back": self.look_back}


def verify(recognizer: Paraformer, manifest: Path, limit: int) -> None:
    """Replay fixtures and check that committed text is only ever extended."""
    violations, utterances = 0, 0
    for line in manifest.read_text().splitlines()[:limit]:
        entry = json.loads(line)
        audio = np.frombuffer(Path(entry["pcm"]).read_bytes(), dtype="<i2").astype(np.float32) / 32768.0
        # Fixtures are 24 kHz; the recogniser reads 16 kHz.
        audio = np.interp(np.arange(0, len(audio), 1.5), np.arange(len(audio)), audio).astype(np.float32)
        session = recognizer.start()
        previous = ""
        for offset in range(0, len(audio), RATE // 10):
            current = session.push(audio[offset: offset + RATE // 10]).text
            if not current.startswith(previous):
                violations += 1
                log.warning("committed text changed: %r -> %r", previous, current)
            previous = current
        final = session.finish().text
        if not final.startswith(previous):
            violations += 1
            log.warning("final withdrew committed text: %r -> %r", previous, final)
        utterances += 1
        print(f"{entry['id']}: {final}")
    print(json.dumps({"utterances": utterances, "committed_withdrawals": violations,
                      "append_only": violations == 0}))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9113)
    parser.add_argument("--model", default="paraformer-zh-streaming")
    parser.add_argument("--chunk-size", default="0,10,5", help="[left,current,right] in 60 ms units")
    parser.add_argument("--encoder-look-back", type=int, default=4)
    parser.add_argument("--decoder-look-back", type=int, default=1)
    parser.add_argument("--device", default="cuda:0")
    parser.add_argument("--verify-append-only", default="", help="fixture manifest to replay instead of serving")
    parser.add_argument("--limit", type=int, default=5)
    args = parser.parse_args()
    configure_logging()
    chunk_size = [int(value) for value in args.chunk_size.split(",")]
    recognizer = Paraformer(args.model, chunk_size, args.encoder_look_back, args.decoder_look_back, args.device)
    if args.verify_append_only:
        started = time.perf_counter()
        verify(recognizer, Path(args.verify_append_only), args.limit)
        print(f"verified in {time.perf_counter() - started:.1f}s")
        return
    serve_recognizer(recognizer, args.host, args.port)


if __name__ == "__main__":
    main()
