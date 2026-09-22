"""Shared serving scaffold for the duplex-plan model services.

Every open recogniser and synthesiser integrated for the streaming/full-duplex
plan is served through one of two wire contracts, so the Go runtime needs one
adapter per contract rather than one per model:

**Streaming recognition** - the start/chunk/finish dialect the ``qwen-asr``
adapter already speaks, extended with an optional committed prefix:

    POST /api/start                      -> {"session_id": "..."}
    POST /api/chunk?session_id=ID        body: float32 LE mono 16 kHz
                                         -> {"text", "stable_text", "language"}
    POST /api/finish?session_id=ID       -> {"text", "stable_text", "language"}
    GET  /health                         -> {"status": "ok", "model", ...}

``text`` is the whole current hypothesis. ``stable_text``, when present, is a
prefix of ``text`` the recogniser guarantees it will not revise, and
``stability`` names why ("decoder-append-only", "provider-committed", or
"none"). A recogniser that cannot commit anything early omits it.

**Speech synthesis** - two routes over one backend:

    POST /v1/audio/speech   OpenAI-compatible; {"input","voice","response_format":"pcm","stream":true}
                            -> audio/pcm stream, X-Sample-Rate header
    WS   /v1/tts/stream     incremental same-context synthesis (below)
    GET  /health

The WebSocket contract ("openrealtime-incremental-speech/1"):

    client -> {"type":"context.open","context_id":C,"voice":V}
    server <- {"type":"context.ready","context_id":C,"sample_rate":R,
               "capabilities":{"incremental_text":bool,"nonterminal_flush":bool,
                               "input_granularity":"token"|"clause"|"sentence"|"complete"}}
    client -> {"type":"text.append","context_id":C,"text":"..."}     (any number)
    client -> {"type":"text.flush","context_id":C}   synthesise what is buffered now
    client -> {"type":"text.end","context_id":C}     no more text for this context
    client -> {"type":"context.cancel","context_id":C}
    server <- {"type":"text.accepted","context_id":C,"chars":N}   cumulative text consumed
    server <- {"type":"audio","context_id":C,"seq":n,"pcm16":"<base64>"}
    server <- {"type":"audio.done","context_id":C}  after text.end and the last frame
    server <- {"type":"context.cancelled","context_id":C}
    server <- {"type":"error","context_id":C,"message":"..."}

A client may request ``audio_window_bytes`` in context.open (PCM16 bytes,
maximum five seconds). context.ready echoes the accepted window. It replenishes
that window with ``audio.credit`` / ``bytes`` after consuming each audio packet.
This bounds transport audio without blocking WebSocket control reads; it does
not by itself bound a backend's internal generation queue.

A backend declares honestly how it consumes text: a model that can condition on
a growing text prefix inside one generation declares ``incremental_text`` and
``input_granularity: token``; a model that needs a whole clause or sentence
before it can start declares that granularity, and the scaffold buffers to it.
The declaration is what the Go adapter reports; a benchmark with a held-back
suffix (``tools/duplexmodels/ttsbench.py``) checks it.

Backends implement :class:`Recognizer` or :class:`Synthesizer` and call
:func:`serve_recognizer` / :func:`serve_synthesizer`.
"""


import asyncio
import base64
import json
import logging
import queue
import re
import threading
import time
import uuid
from dataclasses import dataclass, field
from typing import Iterator, Optional

import numpy as np

log = logging.getLogger("duplexmodels")


# --------------------------------------------------------------------------
# Recognition


@dataclass
class Hypothesis:
    text: str
    stable_text: Optional[str] = None
    language: str = ""


class RecognizerSession:
    """One utterance. Calls are serialised by the scaffold."""

    def push(self, audio: np.ndarray) -> Hypothesis:  # float32 16 kHz mono
        raise NotImplementedError

    def finish(self) -> Hypothesis:
        raise NotImplementedError

    def close(self) -> None:
        pass


class Recognizer:
    model: str = "unknown"
    stability: str = "none"

    def start(self) -> RecognizerSession:
        raise NotImplementedError

    def health(self) -> dict:
        return {}


def serve_recognizer(recognizer: Recognizer, host: str, port: int) -> None:
    from fastapi import FastAPI, Request
    from fastapi.responses import JSONResponse
    import uvicorn

    app = FastAPI()
    sessions: dict[str, tuple[RecognizerSession, threading.Lock, float]] = {}
    guard = threading.Lock()
    # One GPU worker: model calls from concurrent sessions are serialised here
    # rather than colliding inside the framework.
    gpu = threading.Lock()
    stats = {"chunks": 0, "chunk_seconds": 0.0, "finishes": 0, "finish_seconds": 0.0}

    def answer(hypothesis: Hypothesis) -> dict:
        body = {"text": hypothesis.text, "language": hypothesis.language, "stability": recognizer.stability}
        if hypothesis.stable_text is not None:
            if not hypothesis.text.startswith(hypothesis.stable_text):
                raise RuntimeError("stable_text must be a prefix of text")
            body["stable_text"] = hypothesis.stable_text
        return body

    def expire() -> None:
        now = time.monotonic()
        for key, (session, _, touched) in list(sessions.items()):
            if now - touched > 600:
                session.close()
                sessions.pop(key, None)

    @app.get("/health")
    def health():
        return {"status": "ok", "model": recognizer.model, "stability": recognizer.stability,
                "sessions": len(sessions), "stats": stats, **recognizer.health()}

    @app.post("/api/start")
    def start():
        with guard:
            expire()
            with gpu:
                session = recognizer.start()
            key = uuid.uuid4().hex
            sessions[key] = (session, threading.Lock(), time.monotonic())
        return {"session_id": key}

    def lookup(key: str):
        with guard:
            entry = sessions.get(key)
            if entry is None:
                return None
            sessions[key] = (entry[0], entry[1], time.monotonic())
            return entry

    @app.post("/api/chunk")
    async def chunk(session_id: str, request: Request):
        raw = await request.body()
        if len(raw) % 4:
            return JSONResponse({"error": "body must be float32 samples"}, status_code=400)
        entry = lookup(session_id)
        if entry is None:
            return JSONResponse({"error": "unknown session"}, status_code=404)
        audio = np.frombuffer(raw, dtype="<f4").copy()

        def run():
            with entry[1], gpu:
                began = time.perf_counter()
                result = entry[0].push(audio)
                stats["chunks"] += 1
                stats["chunk_seconds"] += time.perf_counter() - began
                return result

        try:
            return answer(await asyncio.to_thread(run))
        except Exception as error:  # noqa: BLE001
            log.exception("chunk failed")
            return JSONResponse({"error": str(error)}, status_code=500)

    @app.post("/api/finish")
    async def finish(session_id: str):
        entry = lookup(session_id)
        if entry is None:
            return JSONResponse({"error": "unknown session"}, status_code=404)

        def run():
            with entry[1], gpu:
                began = time.perf_counter()
                result = entry[0].finish()
                stats["finishes"] += 1
                stats["finish_seconds"] += time.perf_counter() - began
                return result

        try:
            result = await asyncio.to_thread(run)
        except Exception as error:  # noqa: BLE001
            log.exception("finish failed")
            return JSONResponse({"error": str(error)}, status_code=500)
        finally:
            with guard:
                popped = sessions.pop(session_id, None)
            if popped is not None:
                popped[0].close()
        return answer(result)

    uvicorn.run(app, host=host, port=port, log_level="warning")


# --------------------------------------------------------------------------
# Synthesis

_CLAUSE = re.compile(r"(?<=[,;:，；：、])\s*|(?<=[.!?。！？…])\s+|(?<=[。！？])")
_SENTENCE = re.compile(r"(?<=[.!?。！？…])\s+|(?<=[。！？])")


@dataclass
class SynthesisCapabilities:
    #: The model conditions on a growing text prefix within one generation.
    incremental_text: bool = False
    #: A nonterminal flush makes the model speak buffered text immediately.
    nonterminal_flush: bool = True
    #: Smallest text unit the backend needs before it can start: token,
    #: clause, sentence, or complete.
    input_granularity: str = "sentence"


class Synthesizer:
    model: str = "unknown"
    sample_rate: int = 24_000
    capabilities = SynthesisCapabilities()
    #: Serialises model calls. The scaffold holds it around each complete-text
    #: request; an incremental backend takes it itself, and must not hold it
    #: while waiting for more text.
    lock = threading.Lock()

    def synthesize(self, text: str, voice: str, cancel: threading.Event) -> Iterator[np.ndarray]:
        """Yield float32 or int16 mono chunks for complete text."""
        raise NotImplementedError

    def synthesize_incremental(self, texts: "queue.Queue[Optional[str]]", voice: str,
                               cancel: threading.Event) -> Iterator[np.ndarray]:
        """Yield audio while text keeps arriving on ``texts`` (None ends it).

        Only backends that declare ``incremental_text`` override this. The
        default buffers to the declared granularity and calls synthesize().
        """
        buffered = ""
        splitter = _CLAUSE if self.capabilities.input_granularity == "clause" else _SENTENCE
        while not cancel.is_set():
            item = texts.get()
            flush = item is None or item == "\x00flush"
            if item is not None and item != "\x00flush":
                buffered += item
            pieces = [piece for piece in splitter.split(buffered)]
            ready, buffered = (pieces, "") if flush else (pieces[:-1], pieces[-1] if pieces else "")
            for piece in ready:
                if piece.strip() and not cancel.is_set():
                    with self.lock:
                        chunks = list(self.synthesize(piece.strip(), voice, cancel))
                    yield from chunks
            if item is None:
                return

    def health(self) -> dict:
        return {}


def to_pcm16(chunk: np.ndarray) -> bytes:
    if chunk.dtype != np.int16:
        chunk = (np.clip(np.asarray(chunk, dtype=np.float32), -1.0, 1.0) * 32767).astype(np.int16)
    return chunk.astype("<i2").tobytes()


class AudioCredits:
    """Optional byte credits bound audio sent ahead of a consuming client.

    Waiting occurs in the context producer thread, never the WebSocket reader.
    Cancellation remains observable even when the client grants no more credit.
    """
    def __init__(self, window):
        self.window = window
        self.available = window
        self.condition = threading.Condition()

    def grant(self, count):
        if count <= 0 or count % 2:
            raise ValueError("audio credit must be a positive whole PCM16 sample")
        with self.condition:
            self.available = min(self.window, self.available + count)
            self.condition.notify_all()

    def packets(self, pcm, cancel, packet_bytes):
        offset = 0
        while offset < len(pcm):
            size = min(packet_bytes, self.window, len(pcm)-offset)
            with self.condition:
                while self.available < size and not cancel.is_set():
                    self.condition.wait(.1)
                if cancel.is_set():
                    return
                self.available -= size
            yield pcm[offset:offset+size]
            offset += size


def serve_synthesizer(synthesizer: Synthesizer, host: str, port: int) -> None:
    from fastapi import FastAPI, Request, WebSocket, WebSocketDisconnect
    from fastapi.responses import JSONResponse, StreamingResponse
    import uvicorn

    app = FastAPI()
    gpu = synthesizer.lock
    stats = {"requests": 0, "contexts": 0, "first_audio_seconds": []}

    @app.get("/health")
    def health():
        first = sorted(stats["first_audio_seconds"])[-200:]
        return {"status": "ok", "model": synthesizer.model, "sample_rate": synthesizer.sample_rate,
                "capabilities": synthesizer.capabilities.__dict__,
                "requests": stats["requests"], "contexts": stats["contexts"],
                "first_audio_p50_s": first[len(first) // 2] if first else None, **synthesizer.health()}

    @app.post("/v1/audio/speech")
    async def speech(request: Request):
        body = await request.json()
        text = str(body.get("input", "")).strip()
        voice = str(body.get("voice") or "default")
        if not text:
            return JSONResponse({"error": "input is empty"}, status_code=400)
        if body.get("response_format", "pcm") != "pcm":
            return JSONResponse({"error": "only response_format=pcm is served"}, status_code=400)
        stats["requests"] += 1
        cancel = threading.Event()
        chunks: "queue.Queue[Optional[bytes]]" = queue.Queue(maxsize=64)

        def produce():
            began = time.perf_counter()
            first = True
            try:
                with gpu:
                    for chunk in synthesizer.synthesize(text, voice, cancel):
                        if cancel.is_set():
                            break
                        if first:
                            stats["first_audio_seconds"].append(time.perf_counter() - began)
                            first = False
                        chunks.put(to_pcm16(chunk))
            except Exception:  # noqa: BLE001
                log.exception("synthesis failed")
            finally:
                chunks.put(None)

        threading.Thread(target=produce, daemon=True).start()

        async def stream():
            try:
                while True:
                    item = await asyncio.to_thread(chunks.get)
                    if item is None:
                        return
                    yield item
            finally:
                cancel.set()

        return StreamingResponse(stream(), media_type="audio/pcm",
                                 headers={"X-Sample-Rate": str(synthesizer.sample_rate)})

    @app.websocket("/v1/tts/stream")
    async def incremental(websocket: WebSocket):
        await websocket.accept()
        loop = asyncio.get_running_loop()
        contexts: dict[str, dict] = {}
        outgoing: "asyncio.Queue[dict]" = asyncio.Queue()

        def emit(message: dict) -> None:
            loop.call_soon_threadsafe(outgoing.put_nowait, message)

        def run(context_id: str, state: dict) -> None:
            began = time.perf_counter()
            seq = 0
            try:
                if True:
                    for chunk in synthesizer.synthesize_incremental(state["texts"], state["voice"], state["cancel"]):
                        if state["cancel"].is_set():
                            break
                        if seq == 0:
                            stats["first_audio_seconds"].append(time.perf_counter() - began)
                        pcm = to_pcm16(chunk)
                        credits = state["credits"]
                        packets = (credits.packets(pcm, state["cancel"], synthesizer.sample_rate//10*2)
                                   if credits is not None else (pcm,))
                        for packet in packets:
                            if state["cancel"].is_set():
                                break
                            emit({"type": "audio", "context_id": context_id, "seq": seq,
                                  "pcm16": base64.b64encode(packet).decode()})
                            seq += 1
                if state["cancel"].is_set():
                    emit({"type": "context.cancelled", "context_id": context_id})
                else:
                    emit({"type": "audio.done", "context_id": context_id})
            except Exception as error:  # noqa: BLE001
                log.exception("incremental synthesis failed")
                emit({"type": "error", "context_id": context_id, "message": str(error)})

        async def sender():
            while True:
                message = await outgoing.get()
                await websocket.send_text(json.dumps(message))

        send_task = asyncio.create_task(sender())
        try:
            while True:
                message = json.loads(await websocket.receive_text())
                kind = message.get("type")
                context_id = str(message.get("context_id", "default"))
                if kind == "context.open":
                    window = int(message.get("audio_window_bytes", 0))
                    if window < 0 or window % 2 or window > synthesizer.sample_rate*2*5:
                        await outgoing.put({"type": "error", "context_id": context_id,
                                            "message": "invalid audio window (maximum five seconds)"})
                        continue
                    state = {"texts": queue.Queue(), "cancel": threading.Event(), "chars": 0,
                             "credits": AudioCredits(window) if window else None,
                             "voice": str(message.get("voice") or "default")}
                    contexts[context_id] = state
                    stats["contexts"] += 1
                    threading.Thread(target=run, args=(context_id, state), daemon=True).start()
                    await outgoing.put({"type": "context.ready", "context_id": context_id,
                                        "sample_rate": synthesizer.sample_rate, "model": synthesizer.model,
                                        "audio_window_bytes": window,
                                        "capabilities": synthesizer.capabilities.__dict__})
                    continue
                state = contexts.get(context_id)
                if state is None:
                    await outgoing.put({"type": "error", "context_id": context_id, "message": "unknown context"})
                    continue
                if kind == "audio.credit":
                    if state["credits"] is not None:
                        state["credits"].grant(int(message.get("bytes", 0)))
                elif kind == "text.append":
                    text = str(message.get("text", ""))
                    state["texts"].put(text)
                    state["chars"] += len(text)
                    await outgoing.put({"type": "text.accepted", "context_id": context_id, "chars": state["chars"]})
                elif kind == "text.flush":
                    state["texts"].put("\x00flush")
                elif kind == "text.end":
                    state["texts"].put(None)
                elif kind == "context.cancel":
                    state["cancel"].set()
                    state["texts"].put(None)
                else:
                    await outgoing.put({"type": "error", "context_id": context_id, "message": f"unknown type {kind}"})
        except WebSocketDisconnect:
            pass
        finally:
            for state in contexts.values():
                state["cancel"].set()
                state["texts"].put(None)
            send_task.cancel()

    uvicorn.run(app, host=host, port=port, log_level="warning")


def configure_logging() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s")
