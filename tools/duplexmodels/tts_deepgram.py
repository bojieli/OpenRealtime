#!/usr/bin/env python3
"""Deepgram Aura's streaming TTS WebSocket behind the duplex-plan contracts.

The closed incremental-synthesis reference of the streaming plan (stage P3):
Aura's /v1/speak WebSocket takes text in any number of ``Speak`` messages and
generates audio for everything spoken so far when it receives ``Flush``;
``Clear`` discards what is queued. That is same-context synthesis with
flush-bounded granularity, so this backend declares
``incremental_text=True, input_granularity="flush"`` and maps the contract
directly: text.append -> Speak, a clause boundary or text.flush -> Flush,
text.end -> Flush then wait for Flushed, context.cancel -> Clear.

Audio is requested as linear16 at 24 kHz. Network time is part of every
number this service produces; it runs on this host but the model does not.

    DEEPGRAM_API_KEY=... python tools/duplexmodels/tts_deepgram.py --port 9126 --model aura-2-thalia-en
"""

from __future__ import annotations

import argparse
import json
import os
import queue
import re
import sys
import threading
from pathlib import Path
from typing import Iterator, Optional

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import SynthesisCapabilities, Synthesizer, configure_logging, log, serve_synthesizer  # noqa: E402

RATE = 24_000
CLAUSE_END = re.compile(r"[,;:.!?，。！？；：]\s*$")


class DeepgramAura(Synthesizer):
    sample_rate = RATE
    capabilities = SynthesisCapabilities(incremental_text=True, nonterminal_flush=True, input_granularity="flush")

    def __init__(self, model: str, key: str) -> None:
        self.model = f"deepgram/{model}"
        self.voice_model = model
        self.key = key
        self.lock = threading.Lock()

    def _connect(self):
        from websockets.sync.client import connect  # noqa: PLC0415

        url = f"wss://api.deepgram.com/v1/speak?model={self.voice_model}&encoding=linear16&sample_rate={RATE}"
        return connect(url, additional_headers={"Authorization": f"Token {self.key}"}, max_size=None,
                       open_timeout=10)

    def synthesize(self, text: str, voice: str, cancel: threading.Event) -> Iterator[np.ndarray]:
        texts: "queue.Queue[Optional[str]]" = queue.Queue()
        texts.put(text)
        texts.put(None)
        yield from self.synthesize_incremental(texts, voice, cancel)

    def synthesize_incremental(self, texts: "queue.Queue[Optional[str]]", voice: str,
                               cancel: threading.Event) -> Iterator[np.ndarray]:
        socket = self._connect()
        audio: "queue.Queue[Optional[bytes]]" = queue.Queue()
        flushes = {"sent": 0, "done": 0}
        done = threading.Event()

        def receive() -> None:
            try:
                for message in socket:
                    if isinstance(message, bytes):
                        audio.put(message)
                        continue
                    event = json.loads(message)
                    if event.get("type") == "Flushed":
                        flushes["done"] += 1
                        if done.is_set() and flushes["done"] >= flushes["sent"]:
                            break
                    elif event.get("type") == "Warning" or event.get("type") == "Error":
                        log.warning("deepgram: %s", event)
            except Exception as failure:  # noqa: BLE001
                log.warning("deepgram socket ended: %s", failure)
            finally:
                audio.put(None)

        def send() -> None:
            pending = ""
            try:
                while not cancel.is_set():
                    item = texts.get()
                    flush = item is None or item == "\x00flush"
                    if item is not None and item != "\x00flush":
                        pending += item
                        socket.send(json.dumps({"type": "Speak", "text": item}))
                    if flush or CLAUSE_END.search(pending):
                        if pending.strip():
                            socket.send(json.dumps({"type": "Flush"}))
                            flushes["sent"] += 1
                        pending = ""
                    if item is None:
                        done.set()
                        if flushes["done"] >= flushes["sent"]:
                            socket.close()
                        return
                if cancel.is_set():
                    socket.send(json.dumps({"type": "Clear"}))
                    socket.close()
            except Exception as failure:  # noqa: BLE001
                log.warning("deepgram send ended: %s", failure)

        threading.Thread(target=receive, daemon=True).start()
        threading.Thread(target=send, daemon=True).start()
        carry = b""
        while True:
            chunk = audio.get()
            if chunk is None or cancel.is_set():
                break
            chunk = carry + chunk
            usable = len(chunk) // 2 * 2
            carry = chunk[usable:]
            if usable:
                yield np.frombuffer(chunk[:usable], dtype="<i2").copy()
        try:
            socket.close()
        except Exception:  # noqa: BLE001
            pass


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9126)
    parser.add_argument("--model", default="aura-2-thalia-en")
    args = parser.parse_args()
    key = os.environ.get("DEEPGRAM_API_KEY", "").strip()
    if not key:
        raise SystemExit("DEEPGRAM_API_KEY is required")
    configure_logging()
    serve_synthesizer(DeepgramAura(args.model, key), args.host, args.port)


if __name__ == "__main__":
    main()
