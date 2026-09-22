#!/usr/bin/env python3
"""A micro-turn cascade behind the sidecar protocol: streaming ASR -> a
clocked micro-turn language model -> incremental TTS, owning its own floor.

This is the composable streaming cascade of docs/full-duplex-streaming-plan.md
(stages P2-P4, cells C1-C4) packaged as one duplex model, so the existing
``duplex`` binding and every benchmark that drives it can measure it without
engine changes. The composition is explicit in the flags:

* ``--asr`` a streaming recogniser: ``vllm-realtime`` (Voxtral / Qwen3-ASR on
  vLLM's /v1/realtime) or ``chunk`` (the start/chunk/finish services in
  tools/duplexmodels, which report a committed prefix).
* ``--llm`` the micro-turn language model:
  ``orchestrated`` - an ordinary instruct model behind an OpenAI-compatible
  endpoint, asked one enumerated control question per tick and, when it
  chooses to speak, a separate streamed answer. This is the plan's
  "orchestrated micro-turns" fallback: the model is not trained on the
  micro-turn protocol, and the execution mode says so.
  ``duplexcascade`` - the released DuplexCascade checkpoint with its own
  control-token grammar (sbintuitions/DuplexCascade; gated on the Hub).
* ``--tts-url`` an openrealtime-incremental-speech/1 WebSocket
  (tools/duplexmodels); the answer's text is appended to one synthesis context
  as the model writes it.

The clock is the point. Every ``--tick-ms`` the controller is handed the words
committed since the previous tick - or ``<no voice>`` - whether or not the
recogniser said anything, and decides: wait, respond, or backchannel while it
is silent; continue or stop while it is speaking. Silence is therefore an input
the model reasons over, not the absence of one, and new speech is admitted
while the assistant is talking. Late words keep their arrival time but are
admitted at the current tick; nothing is backdated.

Every tick is written to ``--trace`` (JSONL) with the evidence it admitted,
the decision, and how long deciding took, so deadline misses and
evidence-to-tick waits are measurable.

    openrealtime serve -binding duplex -sidecar "python sidecars/microturn_sidecar.py \\
        --asr vllm-realtime --llm orchestrated --tts-url ws://127.0.0.1:9120/v1/tts/stream"
    openrealtime conformance sidecar -- python sidecars/microturn_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import math
import queue
import re
import sys
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log, run  # noqa: E402

ASR_RATE = 16_000
FRAME_MS = 40

WAIT, RESPOND, BACKCHANNEL = "wait", "respond", "backchannel"
_CJK = re.compile(r"[\u3040-\u30ff\u3400-\u9fff\uac00-\ud7af]")
CONTINUE, STOP = "continue", "stop"


class Activity:
    """Acoustic speech activity from input energy, with an adaptive floor.

    It is evidence the words cannot give: that the user is making sound now,
    before a recogniser that trails speech has decoded anything. It says
    nothing about what the sound means.
    """

    def __init__(self, rate: int = 16_000, frame_ms: int = 20, margin_db: float = 12.0,
                 hangover_ms: int = 200) -> None:
        self.frame = rate * frame_ms // 1000
        self.frame_s = frame_ms / 1000
        self.margin = 10 ** (margin_db / 20)
        self.hangover = hangover_ms / 1000
        self.floor = 1e-3
        self.buffer = np.zeros(0, dtype=np.float32)
        self.voiced_since = 0.0
        self.last_voiced = 0.0
        self.lock = threading.Lock()

    def push(self, samples: np.ndarray) -> bool:
        """Add audio; report whether a new run of sound began."""
        self.buffer = np.concatenate([self.buffer, samples])
        now = time.monotonic()
        onset = False
        while len(self.buffer) >= self.frame:
            frame, self.buffer = self.buffer[: self.frame], self.buffer[self.frame:]
            rms = float(np.sqrt(np.mean(frame ** 2)) + 1e-9)
            voiced = rms > max(self.floor * self.margin, 0.01)
            if not voiced:
                self.floor = 0.95 * self.floor + 0.05 * rms
            with self.lock:
                if voiced:
                    if not self.voiced_since or now - self.last_voiced > self.hangover:
                        self.voiced_since = now
                        onset = True
                    self.last_voiced = now
        return onset

    def settled(self, now: float) -> float:
        """The end of the run of sound that has just gone quiet, or 0.

        It returns the timestamp rather than a flag so a caller can act on
        each quiet period once: the value is the same for as long as the user
        stays quiet, and changes when they speak again.
        """
        with self.lock:
            if not self.last_voiced or now - self.last_voiced < self.hangover:
                return 0.0
            return self.last_voiced

    def state(self, now: float) -> tuple[float, float]:
        """(seconds of current sound, seconds since the last sound)."""
        with self.lock:
            if not self.last_voiced:
                return 0.0, math.inf
            quiet = now - self.last_voiced
            if quiet > self.hangover:
                return 0.0, quiet
            return now - self.voiced_since, quiet


class LinearResampler:
    """Streaming linear-interpolation resampler."""

    def __init__(self, source_rate: int, target_rate: int) -> None:
        self.step = source_rate / target_rate
        self.position = 0.0
        self.tail = np.zeros(0, dtype=np.float32)

    def __call__(self, samples: np.ndarray) -> np.ndarray:
        data = np.concatenate([self.tail, samples.astype(np.float32)])
        if len(data) < 2 or self.position > len(data) - 1:
            self.tail = data
            return np.zeros(0, dtype=np.float32)
        count = int(np.floor((len(data) - 1 - self.position) / self.step)) + 1
        positions = self.position + np.arange(count) * self.step
        out = np.interp(positions, np.arange(len(data)), data).astype(np.float32)
        following = self.position + count * self.step
        keep = min(int(np.floor(following)), len(data) - 1)
        self.tail = data[keep:]
        self.position = following - keep
        return out


@dataclass
class Word:
    text: str
    arrived: float  # monotonic time the recogniser committed it
    spoken: float = 0.0  # estimated monotonic time it was said

    def __post_init__(self) -> None:
        if not self.spoken:
            self.spoken = self.arrived


@dataclass
class Tick:
    index: int
    at: float
    words: list[Word] = field(default_factory=list)
    speaking: bool = False
    trigger: str = "clock"

    def rendered(self) -> str:
        return " ".join(word.text for word in self.words).strip() or "<no voice>"


def _quote(words: list[Word]) -> str:
    return " ".join(word.text for word in words).strip()


@dataclass
class Evidence:
    """What the controller knows at one tick, with times relative to now."""

    now: float
    ticks: list[Tick]
    pending: list[Word]
    speaking: bool
    began_speaking: float = 0.0
    answering: str = ""
    said: str = ""
    sounding: float = 0.0  # seconds of current user sound
    quiet: float = math.inf  # seconds since the last user sound
    backchannels: bool = False

    def choices(self) -> list[str]:
        if self.speaking:
            return [CONTINUE, STOP]
        return [WAIT, RESPOND, BACKCHANNEL] if self.backchannels else [WAIT, RESPOND]

    def acoustics(self) -> str:
        if self.sounding > 0:
            return f"Microphone: the user is making sound now (for {self.sounding:.1f} s)."
        if math.isinf(self.quiet):
            return "Microphone: no user sound yet."
        return f"Microphone: quiet for {self.quiet:.1f} s."

    def render(self) -> str:
        lines = ["Recent recognition, one line per tick (newest last):"]
        for tick in self.ticks[-16:]:
            lines.append(f"  {tick.at - self.now:+.1f}s  {tick.rendered()}"
                         + ("  [assistant speaking]" if tick.speaking else ""))
        lines.append(self.acoustics())
        if not self.speaking:
            lines.append("\nThe assistant is SILENT.")
            if self.pending:
                ago = self.now - self.pending[-1].spoken
                lines.append(f"Unanswered user speech: \"{_quote(self.pending)}\"")
                lines.append(f"Its last word was said about {ago:.1f} s ago.")
            return "\n".join(lines)
        before = [word for word in self.pending if word.spoken < self.began_speaking]
        during = [word for word in self.pending if word.spoken >= self.began_speaking]
        lines.append(f"\nThe assistant is SPEAKING. It began {self.now - self.began_speaking:.1f} s ago, "
                     f"answering: \"{self.answering}\"")
        lines.append(f"It has said so far: \"{self.said[-240:]}\"")
        lines.append(f"User words said BEFORE it began, recognised late: \"{_quote(before)}\"")
        lines.append(f"User words said WHILE it has been speaking: \"{_quote(during)}\"")
        if self.sounding > 0 and not during:
            lines.append("The user is making sound over the assistant, and no words have been recognised yet.")
        return "\n".join(lines)


# --------------------------------------------------------------------------
# Recognisers: each pushes committed words into ``words`` as they arrive.


class RealtimeRecognizer:
    """vLLM /v1/realtime: one generation fed continuously; deltas are final."""

    def __init__(self, url: str, model: str, words: "asyncio.Queue[Word]", delay_s: float,
                 restart_s: float = 240.0) -> None:
        self.url, self.model, self.words, self.delay_s = url, model, words, delay_s
        self.last_delta = 0.0
        self.idle_flush_s = 0.15
        self.audio: "asyncio.Queue[Optional[bytes]]" = asyncio.Queue()
        self.restart_s = restart_s
        self.carry = ""

    async def run(self) -> None:
        import websockets  # noqa: PLC0415

        while True:
            began = time.monotonic()
            async with websockets.connect(self.url, max_size=None) as socket:
                await socket.recv()  # session.created
                await socket.send(json.dumps({"type": "session.update", "model": self.model}))
                await socket.send(json.dumps({"type": "input_audio_buffer.commit"}))

                async def pump() -> None:
                    while time.monotonic() - began < self.restart_s:
                        chunk = await self.audio.get()
                        if chunk is None:
                            break
                        await socket.send(json.dumps({"type": "input_audio_buffer.append",
                                                      "audio": base64.b64encode(chunk).decode()}))
                    await socket.send(json.dumps({"type": "input_audio_buffer.commit", "final": True}))

                sender = asyncio.create_task(pump())
                try:
                    async for raw in socket:
                        message = json.loads(raw)
                        kind = message.get("type")
                        if kind == "transcription.delta":
                            self._emit(message.get("delta", ""))
                        elif kind == "transcription.done":
                            self._emit("", flush=True)
                            break
                        elif kind == "error":
                            log(f"realtime recogniser error: {message.get('error')}")
                finally:
                    sender.cancel()

    def _emit(self, delta: str, flush: bool = False) -> None:
        # Deltas are token pieces, so a word is complete when the next piece
        # starts with a space. The last word of an utterance has no next
        # piece until the user speaks again, so it is also committed at
        # sentence punctuation and after a short quiet spell (_flush_idle);
        # otherwise it would wait for the next utterance.
        self.carry += delta
        self.last_delta = time.monotonic()
        if _CJK.search(self.carry) or re.search(r"[.?!,;:]\s*$", self.carry):
            flush = True
        parts = re.split(r"(?=\s)", self.carry)
        complete, self.carry = (parts, "") if flush else (parts[:-1], parts[-1] if parts else "")
        now = time.monotonic()
        for part in complete:
            if part.strip():
                self.words.put_nowait(Word(part.strip(), now, now - self.delay_s))
        if self.carry.strip():
            asyncio.get_running_loop().call_later(self.idle_flush_s, self._flush_idle)

    def _flush_idle(self) -> None:
        if self.carry.strip() and time.monotonic() - self.last_delta >= self.idle_flush_s:
            self._emit("", flush=True)

    def push(self, pcm16: bytes) -> None:
        self.audio.put_nowait(pcm16)


class ChunkRecognizer:
    """start/chunk/finish services: committed prefix via stable_text."""

    def __init__(self, url: str, words: "asyncio.Queue[Word]", delay_s: float) -> None:
        self.url, self.words, self.delay_s = url.rstrip("/"), words, delay_s
        self.audio: "asyncio.Queue[Optional[tuple[bytes, float]]]" = asyncio.Queue()
        self.committed = ""

    async def run(self) -> None:
        import httpx  # noqa: PLC0415

        async with httpx.AsyncClient(timeout=30) as client:
            while True:
                session = (await client.post(f"{self.url}/api/start")).json()["session_id"]
                self.committed = ""
                chunks = 0
                while True:
                    item = await self.audio.get()
                    if item is None:
                        return
                    chunk, received = item
                    # A service slower than real time must not build a
                    # backlog: everything already queued goes in one request.
                    while not self.audio.empty():
                        extra = self.audio.get_nowait()
                        if extra is None:
                            return
                        chunk, received = chunk + extra[0], extra[1]
                    samples = np.frombuffer(chunk, dtype="<i2").astype("<f4") / 32768.0
                    answer = (await client.post(f"{self.url}/api/chunk", params={"session_id": session},
                                                content=samples.tobytes())).json()
                    stable = answer.get("stable_text")
                    if stable is None:
                        # No committed prefix: treat all but the last word as
                        # committed, which is the conservative reading.
                        text = answer.get("text", "")
                        stable = " ".join(text.split()[:-1])
                    if stable.startswith(self.committed):
                        fresh = stable[len(self.committed):]
                        self.committed = stable
                        now = time.monotonic()
                        # Words committed on this chunk were said at most the
                        # recogniser's delay before the chunk's last sample.
                        spoken = received - self.delay_s
                        pieces = [fresh.strip()] if _CJK.search(fresh) else fresh.split()
                        for word in pieces:
                            if word:
                                self.words.put_nowait(Word(word, now, spoken))
                    chunks += 1
                    if chunks >= 3000:  # ~5 minutes of 100 ms chunks: start a fresh session
                        await client.post(f"{self.url}/api/finish", params={"session_id": session})
                        break

    def push(self, pcm16: bytes) -> None:
        self.audio.put_nowait((pcm16, time.monotonic()))


# --------------------------------------------------------------------------
# The micro-turn language model.


CONTROL_PROMPT = """You are the turn-taking controller of a voice assistant in a live, full-duplex spoken conversation.
Every {tick_ms} ms you are shown what streaming speech recognition has committed, with how long ago each part was said.
Recognition trails speech by about half a second, so the newest words can be missing.

When the assistant is SILENT, answer exactly one of:
- respond: the unanswered speech is a complete question, request, or remark addressed to the assistant AND the user has stopped talking.
- wait: the user is still talking, stopped mid-sentence or mid-thought, used a filler, or said something that needs no reply.
- backchannel: the user is in the middle of a long story and a brief "mm-hmm" would help; never when a question is waiting.

When the assistant is SPEAKING, answer exactly one of:
- stop: words said WHILE it has been speaking take the floor: a new question, a correction, "wait", "stop", "hold on", "actually", a change of topic; or late-recognised words show the request it is answering had not been finished and means something else.
- continue: nothing was said while it has been speaking, or only acknowledgements ("yeah", "uh-huh", "okay", "right"), laughter, or talk to someone else.
Sound on the microphone with no recognised words yet is the start of something the recogniser has not decoded; recognition catches up within about a second.

Answer with the single word only."""


ANSWER_PROMPT = """You are a helpful voice assistant speaking in a live conversation.
Answer what the user just said in one to three short spoken sentences. No lists, no markdown, no emoji.
If the user interrupted you, address their new words directly."""


class OrchestratedModel:
    """An ordinary instruct model driven through micro-turns."""

    execution_mode = "orchestrated-micro-turns"

    def __init__(self, url: str, model: str, tick_ms: int, instructions: str) -> None:
        self.url, self.model, self.tick_ms = url.rstrip("/"), model, tick_ms
        self.instructions = instructions.strip()
        self.client = None

    async def start(self) -> None:
        import httpx  # noqa: PLC0415

        self.client = httpx.AsyncClient(timeout=httpx.Timeout(30.0, connect=5.0))

    async def decide(self, evidence: "Evidence") -> str:
        body = {
            "model": self.model, "temperature": 0, "max_tokens": 3,
            "messages": [{"role": "system", "content": CONTROL_PROMPT.format(tick_ms=self.tick_ms)},
                         {"role": "user", "content": evidence.render()}],
            "structured_outputs": {"choice": evidence.choices()},
            "chat_template_kwargs": {"enable_thinking": False},
        }
        response = await self.client.post(f"{self.url}/chat/completions", json=body)
        response.raise_for_status()
        answer = response.json()["choices"][0]["message"]["content"].strip().lower()
        return answer if answer in evidence.choices() else evidence.choices()[0]

    async def answer(self, dialogue: list[dict], notes: list[str]):
        system = ANSWER_PROMPT
        if self.instructions:
            system += "\n\nSession instructions:\n" + self.instructions
        if notes:
            system += "\n\nBackground notes you may use:\n" + "\n".join(notes[-5:])
        body = {
            "model": self.model, "temperature": 0.3, "max_tokens": 160, "stream": True,
            "messages": [{"role": "system", "content": system}] + dialogue[-20:],
            "chat_template_kwargs": {"enable_thinking": False},
        }
        async with self.client.stream("POST", f"{self.url}/chat/completions", json=body) as response:
            response.raise_for_status()
            async for line in response.aiter_lines():
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    return
                delta = json.loads(payload)["choices"][0].get("delta", {}).get("content")
                if delta:
                    yield delta


# --------------------------------------------------------------------------
# Incremental synthesis client (openrealtime-incremental-speech/1).


class SpeechContext:
    def __init__(self, url: str, voice: str, *, audio_window_bytes: int = 0) -> None:
        self.audio_window_bytes = audio_window_bytes
        self.url, self.voice = url, voice
        self.socket = None
        self.id = f"ctx-{time.monotonic_ns()}"
        self.sample_rate = 24_000
        self.failure = None

    async def open(self, on_audio) -> None:
        import websockets  # noqa: PLC0415

        self.socket = await websockets.connect(self.url, max_size=None)
        await self.socket.send(json.dumps({"type": "context.open", "context_id": self.id, "voice": self.voice,
                                           "audio_window_bytes": self.audio_window_bytes}))
        while True:
            message = json.loads(await self.socket.recv())
            if message.get("type") == "context.ready":
                self.sample_rate = int(message["sample_rate"])
                if self.audio_window_bytes and message.get("audio_window_bytes") != self.audio_window_bytes:
                    raise RuntimeError("TTS service does not support requested audio credits")
                break
            if message.get("type") == "error":
                raise RuntimeError(message.get("message"))
        self.reader = asyncio.create_task(self._read(on_audio))

    async def _read(self, on_audio) -> None:
        async for raw in self.socket:
            message = json.loads(raw)
            kind = message.get("type")
            if kind == "audio":
                pcm = base64.b64decode(message["pcm16"])
                result = on_audio(pcm)
                if asyncio.iscoroutine(result):
                    await result
                if self.audio_window_bytes:
                    await self.socket.send(json.dumps({"type": "audio.credit", "context_id": self.id,
                                                       "bytes": len(pcm)}))
            elif kind in ("audio.done", "context.cancelled"):
                on_audio(None)
                return
            elif kind == "error":
                self.failure = RuntimeError(f"tts error: {message.get('message')}")
                log(str(self.failure))
                on_audio(None)
                return

    async def append(self, text: str) -> None:
        await self.socket.send(json.dumps({"type": "text.append", "context_id": self.id, "text": text}))

    async def flush(self) -> None:
        await self.socket.send(json.dumps({"type": "text.flush", "context_id": self.id}))

    async def end(self) -> None:
        await self.socket.send(json.dumps({"type": "text.end", "context_id": self.id}))

    async def cancel(self) -> None:
        try:
            await self.socket.send(json.dumps({"type": "context.cancel", "context_id": self.id}))
        except Exception:  # noqa: BLE001
            pass
        await self.close()

    async def close(self) -> None:
        if self.socket is not None:
            await self.socket.close()


# --------------------------------------------------------------------------
# The sidecar.


class MicroTurnSidecar(Sidecar):
    capabilities = (
        Capability.FULL_DUPLEX,
        Capability.NATIVE_VAD,
        Capability.NATIVE_INTERACTION,
        Capability.BARGE_IN,
        Capability.TRANSCRIPT,
        Capability.TEXT_INJECTION,
    )

    def __init__(self, input_stream, output_stream, *, args: argparse.Namespace) -> None:
        super().__init__(input_stream, output_stream)
        self.args = args
        self.tick_s = args.tick_ms / 1000.0
        self.model_name = f"microturn/{args.asr}+{args.llm}:{args.llm_model}+tts"
        self.output_rate = args.output_rate
        self.loop: Optional[asyncio.AbstractEventLoop] = None
        self.resampler: Optional[LinearResampler] = None
        self.activity = Activity(rate=ASR_RATE)
        self.recognizer = None
        self.words: Optional["asyncio.Queue[Word]"] = None
        self.ticks: deque[Tick] = deque(maxlen=400)
        self.pending: list[Word] = []
        self.dialogue: list[dict] = []
        self.notes: list[str] = []
        self.user_active = False
        self.last_word_at = 0.0
        # Response state
        self.response_task: Optional[asyncio.Task] = None
        self.speech: Optional[SpeechContext] = None
        self.spoken_text = ""
        self.answering = ""
        self.speaking_since = 0.0
        self.response_started = 0.0
        self.playout: "queue.Queue[Optional[bytes]]" = queue.Queue()
        self.playout_epoch = 0
        self.playing = threading.Event()
        self.finished_epochs: set[int] = set()
        self.played_samples = 0
        self.generated_samples = 0
        self.trace = open(args.trace, "a", encoding="utf-8") if args.trace else None
        self.decision_ms: deque[float] = deque(maxlen=10_000)
        self.word_event: Optional[asyncio.Event] = None
        # The end of the last run of sound already decided on, so one quiet
        # period produces one decision.
        self.decided_quiet_at = 0.0
        self.deadline_misses = 0
        self.tick_count = 0

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello) -> None:
        if self.args.llm != "orchestrated" and not self.args.mock:
            raise RuntimeError("--llm duplexcascade is not integrated in this sidecar yet; "
                               "use --llm orchestrated for the explicit fallback")
        if self.input_rate != ASR_RATE:
            self.resampler = LinearResampler(self.input_rate, ASR_RATE)
        if not self.args.mock:
            self.output_rate = self._probe_tts_rate()
        ready = threading.Event()
        threading.Thread(target=self._run_loop, args=(ready,), daemon=True).start()
        ready.wait(10)
        threading.Thread(target=self._playout_loop, daemon=True).start()
        log(f"micro-turn cascade ready: tick {self.args.tick_ms} ms, output {self.output_rate} Hz, "
            f"llm {self.args.llm} ({'mock' if self.args.mock else self.args.llm_url})")

    def _probe_tts_rate(self) -> int:
        import httpx  # noqa: PLC0415

        health = self.args.tts_url.replace("ws://", "http://").replace("wss://", "https://")
        health = health.split("/v1/")[0] + "/health"
        try:
            return int(httpx.get(health, timeout=5).json()["sample_rate"])
        except Exception as failure:  # noqa: BLE001
            log(f"could not read the synthesiser's rate from {health} ({failure}); assuming {self.output_rate}")
            return self.output_rate

    def _run_loop(self, ready: threading.Event) -> None:
        self.loop = asyncio.new_event_loop()
        asyncio.set_event_loop(self.loop)
        self.words = asyncio.Queue()
        if self.args.mock:
            self.recognizer = None
        elif self.args.asr == "vllm-realtime":
            self.recognizer = RealtimeRecognizer(self.args.asr_url, self.args.asr_model, self.words,
                                                 self.args.asr_delay_ms / 1000)
        else:
            self.recognizer = ChunkRecognizer(self.args.asr_url, self.words, self.args.asr_delay_ms / 1000)
        self.model = OrchestratedModel(self.args.llm_url, self.args.llm_model, self.args.tick_ms, self.instructions)
        ready.set()
        self.loop.run_until_complete(self._main())

    async def _main(self) -> None:
        tasks = [asyncio.create_task(self._tick_loop()), asyncio.create_task(self._collect_words())]
        if not self.args.mock:
            await self.model.start()
            tasks.append(asyncio.create_task(self.recognizer.run()))
        await asyncio.gather(*tasks)

    def on_close(self) -> None:
        if self.trace:
            summary = {"summary": True, "ticks": self.tick_count, "deadline_misses": self.deadline_misses,
                       "decision_ms_p50": _percentile(self.decision_ms, 0.5),
                       "decision_ms_p90": _percentile(self.decision_ms, 0.9)}
            self.trace.write(json.dumps(summary) + "\n")
            self.trace.close()

    # --- input ---------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        samples = np.frombuffer(pcm16, dtype="<i2").astype(np.float32) / 32768.0
        if self.resampler is not None:
            samples = self.resampler(samples)
        if (self.activity.push(samples) and self.args.sound_evidence and self.loop is not None
                and self.word_event is not None):
            self.loop.call_soon_threadsafe(self.word_event.set)
        if self.args.mock:
            if samples.size and float(np.sqrt(np.mean(samples ** 2))) > 0.02 and self.loop is not None:
                self.loop.call_soon_threadsafe(self.words.put_nowait, Word("hello", time.monotonic()))
            return
        chunk = (np.clip(samples, -1, 1) * 32767).astype("<i2").tobytes()
        if self.loop is not None and self.recognizer is not None and chunk:
            self.loop.call_soon_threadsafe(self.recognizer.push, chunk)

    def on_text(self, text: str, role: str) -> None:
        # The background reasoner's findings: context for the next answer.
        self.notes.append(text.strip())

    def on_respond(self) -> None:
        # The floor is the model's; an explicit request is honoured by
        # answering now, and the call returns when the answer has been played
        # (the base class ends the turn when it returns).
        if self.loop is None:
            return
        future = asyncio.run_coroutine_threadsafe(self._force_respond(), self.loop)
        try:
            future.result(timeout=120)
        except Exception as failure:  # noqa: BLE001
            log(f"explicit response failed: {failure}")

    async def _force_respond(self) -> None:
        if self.response_task is None or self.response_task.done():
            self._start_response()
        task = self.response_task
        if task is not None:
            try:
                await task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass

    async def _collect_words(self) -> None:
        while True:
            word = await self.words.get()
            self.pending.append(word)
            if self.word_event is not None:
                self.word_event.set()
            self.last_word_at = word.arrived
            if not self.user_active:
                self.user_active = True
                self.send("speech_started")

    # --- the clock ------------------------------------------------------------

    async def _tick_loop(self) -> None:
        self.word_event = asyncio.Event()
        next_tick = time.monotonic() + self.tick_s
        index = 0
        admitted = 0
        while True:
            delay = next_tick - time.monotonic()
            triggered = False
            if delay > 0 and self.args.decide_on_pause and not (
                    self.response_task is not None and not self.response_task.done()) and self.pending:
                # The user has words outstanding and the assistant is silent:
                # the moment they stop speaking is when the floor is free, and
                # waiting for the next tick spends up to a whole tick of it.
                while time.monotonic() < next_tick:
                    settled = self.activity.settled(time.monotonic())
                    if settled and settled != self.decided_quiet_at:
                        self.decided_quiet_at = settled
                        triggered = True
                        break
                    await asyncio.sleep(0.02)
                delay = next_tick - time.monotonic()
            if delay > 0 and not triggered:
                speaking_now = self.response_task is not None and not self.response_task.done()
                if self.args.stop_on_words and speaking_now:
                    # While speaking, recognised words are decided on as they
                    # arrive rather than at the next tick: the clock's phase
                    # would otherwise add up to a whole tick to every yield.
                    try:
                        await asyncio.wait_for(self.word_event.wait(), delay)
                        triggered = True
                    except asyncio.TimeoutError:
                        pass
                else:
                    await asyncio.sleep(delay)
            self.word_event.clear()
            now = time.monotonic()
            if not triggered:
                next_tick = max(now, next_tick + self.tick_s)
            index += 1
            fresh = self.pending[admitted:]
            admitted = len(self.pending)
            speaking = self.response_task is not None and not self.response_task.done()
            trigger = "clock"
            if triggered:
                trigger = "pause" if not speaking else "word"
            tick = Tick(index, now, list(fresh), speaking, trigger)
            self.ticks.append(tick)
            # With nothing unanswered and the assistant silent there is no
            # question to ask: a <no voice> tick can only mean wait.
            if not speaking and not self.pending:
                self._trace(tick, "idle", 0.0)
                continue
            sounding_now, _ = self.activity.state(now)
            if speaking and not fresh and not (self.args.sound_evidence and sounding_now > 0):
                self._trace(tick, CONTINUE, 0.0)
                continue
            # Two rules the controller is never asked about, because they are
            # about who holds the floor rather than about what was meant.
            if not speaking and sounding_now > 0 and not self.args.respond_while_sounding:
                # The user is still audibly speaking: the floor is theirs, and
                # an answer now would be an answer to half a sentence.
                self._trace(tick, WAIT, 0.0, fresh=fresh)
                continue
            began = self.speaking_since or self.response_started or now
            if speaking and fresh and all(word.spoken < began for word in fresh):
                # Words said before the assistant took the floor cannot be an
                # interruption of it; they are the recogniser catching up.
                self._trace(tick, CONTINUE, 0.0, fresh=fresh)
                continue
            began = time.monotonic()
            sounding, quiet = self.activity.state(now)
            evidence = Evidence(now=now, ticks=list(self.ticks), pending=list(self.pending), speaking=speaking,
                                began_speaking=self.speaking_since or self.response_started or now,
                                answering=self.answering, said=self.spoken_text,
                                sounding=sounding if self.args.sound_evidence else 0.0,
                                quiet=quiet if self.args.sound_evidence else math.inf,
                                backchannels=self.args.backchannels)
            try:
                if self.args.mock:
                    decision = RESPOND if not speaking and not fresh else (WAIT if not speaking else CONTINUE)
                else:
                    decision = await self.model.decide(evidence)
            except Exception as failure:  # noqa: BLE001
                log(f"control decision failed: {failure}")
                decision = CONTINUE if speaking else WAIT
            spent = (time.monotonic() - began) * 1000
            self.decision_ms.append(spent)
            if spent > self.args.tick_ms:
                self.deadline_misses += 1
            self._trace(tick, decision, spent, fresh=fresh)
            if speaking and decision == CONTINUE:
                # Words judged not to take the floor were heard and let go;
                # they must not linger as an unanswered request.
                self.pending = [word for word in self.pending if word not in fresh]
            await self._apply(decision, speaking)
            admitted = min(admitted, len(self.pending))

    async def _apply(self, decision: str, speaking: bool) -> None:
        if decision == RESPOND and not speaking:
            self._start_response()
        elif decision == BACKCHANNEL and not speaking:
            self._start_response(backchannel=True)
        elif decision == STOP and speaking:
            await self._stop_response("user took the floor")
            # The interrupting words stay pending and are answered when the
            # user finishes them.

    def _trace(self, tick: Tick, decision: str, spent_ms: float, fresh: Optional[list[Word]] = None) -> None:
        self.tick_count += 1
        if not self.trace:
            return
        lag = [round((tick.at - word.arrived) * 1000, 1) for word in (fresh or [])]
        self.trace.write(json.dumps({
            "tick": tick.index, "t": round(tick.at, 3), "trigger": tick.trigger,
            "admitted": tick.rendered(), "evidence_wait_ms": lag,
            "speaking": tick.speaking, "decision": decision, "decision_ms": round(spent_ms, 1),
            "sounding_s": round(self.activity.state(tick.at)[0], 2),
        }) + "\n")
        self.trace.flush()

    # --- responding -----------------------------------------------------------

    def _start_response(self, backchannel: bool = False) -> None:
        heard = " ".join(word.text for word in self.pending).strip()
        self.speaking_since = 0.0
        self.response_started = time.monotonic()
        if not backchannel:
            self.answering = heard
            if heard:
                self.transcript(heard, final=True)
                self.dialogue.append({"role": "user", "content": heard})
            self.pending.clear()
            if self.user_active:
                self.user_active = False
                self.send("speech_stopped")
        self.response_task = asyncio.ensure_future(self._respond(backchannel))

    async def _respond(self, backchannel: bool) -> None:
        self.spoken_text = ""
        self.playout_epoch += 1
        epoch = self.playout_epoch
        self.played_samples = self.generated_samples = 0
        if self.args.mock:
            text = "mm-hmm" if backchannel else "This is the micro-turn cascade in mock mode."
            self.text_delta(text)
            self.spoken_text = text
            tone = (np.sin(np.arange(self.output_rate // 2) / self.output_rate * 2 * math.pi * 220) * 3000)
            self._enqueue_audio(tone.astype("<i2").tobytes(), epoch)
            self._enqueue_audio(None, epoch)
            await self._drain(epoch)
            self._finish_response(text, epoch)
            return
        speech = SpeechContext(self.args.tts_url, self.args.tts_voice)
        self.speech = speech
        try:
            await speech.open(lambda pcm: self._enqueue_audio(pcm, epoch))
            if backchannel:
                await speech.append("Mm-hmm.")
                await speech.end()
                text = "Mm-hmm."
            else:
                text = ""
                carried = ""
                async for delta in self.model.answer(self.dialogue, self.notes):
                    text += delta
                    self.spoken_text = text
                    carried += delta
                    if carried.strip():
                        # The protocol refuses an empty delta, so whitespace
                        # rides on the next piece that carries text.
                        self.text_delta(carried)
                        carried = ""
                    await speech.append(delta)
                await speech.end()
            await self._drain(epoch)
            self._finish_response(text, epoch)
        except asyncio.CancelledError:
            await speech.cancel()
            raise
        except Exception as failure:  # noqa: BLE001
            log(f"response failed: {failure}")
            await speech.cancel()
            self._finish_response(self.spoken_text, epoch)
        finally:
            await speech.close()

    async def _drain(self, epoch: int) -> None:
        # Speaking ends when the audio has been played, not when it was
        # generated: that is when the user has actually heard it. The playout
        # loop marks an epoch finished when it reaches the synthesiser's end
        # marker (audio.done) for it.
        deadline = time.monotonic() + 120
        while epoch == self.playout_epoch and epoch not in self.finished_epochs and time.monotonic() < deadline:
            await asyncio.sleep(0.02)

    def _finish_response(self, text: str, epoch: int) -> None:
        if epoch != self.playout_epoch:
            return
        if text.strip():
            self.text_done(text)
            self.dialogue.append({"role": "assistant", "content": text})
        self.turn_done()

    async def _stop_response(self, reason: str) -> None:
        task = self.response_task
        self.playout_epoch += 1  # everything queued for the old epoch is stale
        while not self.playout.empty():
            try:
                self.playout.get_nowait()
            except queue.Empty:
                break
        heard = self._heard_text()
        if task is not None and not task.done():
            task.cancel()
            try:
                await task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass
        if heard.strip():
            self.text_done(heard)
            self.dialogue.append({"role": "assistant", "content": heard + " -"})
        self.turn_done()
        log(f"stopped speaking: {reason}")

    def _heard_text(self) -> str:
        # Without word alignments from the synthesiser, what was heard is
        # estimated by the share of generated audio actually played.
        if self.generated_samples <= 0:
            return ""
        share = min(1.0, self.played_samples / self.generated_samples)
        words = self.spoken_text.split()
        return " ".join(words[: int(len(words) * share)])

    # --- paced playout ----------------------------------------------------------

    def _enqueue_audio(self, pcm: Optional[bytes], epoch: int) -> None:
        if epoch != self.playout_epoch:
            return
        if pcm:
            self.generated_samples += len(pcm) // 2
        self.playout.put((epoch, pcm) if pcm else (epoch, b""))

    def _playout_loop(self) -> None:
        """Hand audio to the engine at playback speed, a small lead ahead.

        The engine does not pace sidecar audio, so a synthesiser faster than
        real time would otherwise deliver seconds of speech at once, and a stop
        could no longer take back what had not yet been heard.
        """
        frame_bytes = int(self.output_rate * FRAME_MS / 1000) * 2
        lead = self.args.playout_lead_ms / 1000.0
        buffer = b""
        clock = None
        current_epoch = -1
        while True:
            try:
                epoch, pcm = self.playout.get(timeout=0.05)
            except queue.Empty:
                if not buffer:
                    self.playing.clear()
                    clock = None
                continue
            if epoch != self.playout_epoch:
                buffer, clock = b"", None
                continue
            if epoch != current_epoch:
                buffer, clock, current_epoch = b"", None, epoch
            self.playing.set()
            buffer += pcm
            while len(buffer) >= frame_bytes or (not pcm and buffer):
                frame, buffer = buffer[:frame_bytes], buffer[frame_bytes:]
                if clock is None:
                    clock = time.monotonic()
                    if not self.speaking_since:
                        self.speaking_since = clock
                wait = clock - lead - time.monotonic()
                if wait > 0:
                    time.sleep(wait)
                if epoch != self.playout_epoch:
                    buffer = b""
                    break
                try:
                    self.audio(frame)
                except (OSError, ValueError):
                    return  # the engine closed the session
                self.played_samples += len(frame) // 2
                clock += (len(frame) // 2) / self.output_rate
            if not pcm:
                self.playing.clear()
                self.finished_epochs.add(epoch)


def _percentile(values, fraction: float) -> Optional[float]:
    if not values:
        return None
    ordered = sorted(values)
    return round(ordered[min(len(ordered) - 1, int(fraction * len(ordered)))], 1)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--mock", action="store_true")
    parser.add_argument("--tick-ms", type=int, default=500)
    parser.add_argument("--asr", choices=["vllm-realtime", "chunk"], default="vllm-realtime")
    parser.add_argument("--asr-url", default="ws://127.0.0.1:9101/v1/realtime")
    parser.add_argument("--asr-model", default="voxtral-realtime")
    parser.add_argument("--asr-delay-ms", type=int, default=500,
                        help="how far the recogniser's committed words trail the audio (Voxtral 480, Kyutai STT 500)")
    parser.add_argument("--llm", choices=["orchestrated", "duplexcascade"], default="orchestrated")
    parser.add_argument("--llm-url", default="http://127.0.0.1:9100/v1")
    parser.add_argument("--llm-model", default="qwen3-8b")
    parser.add_argument("--tts-url", default="ws://127.0.0.1:9120/v1/tts/stream")
    parser.add_argument("--tts-voice", default="default")
    parser.add_argument("--output-rate", type=int, default=24_000)
    parser.add_argument("--playout-lead-ms", type=int, default=60)
    parser.add_argument("--decide-on-pause", action="store_true",
                        help="while silent with words outstanding, decide as soon as the user stops speaking "
                             "instead of at the next tick")
    parser.add_argument("--respond-while-sounding", action="store_true",
                        help="allow taking the floor while the user is still audibly speaking (off: wait)")
    parser.add_argument("--sound-evidence", action="store_true",
                        help="give the controller acoustic activity (sound with no words yet) as evidence")
    parser.add_argument("--backchannels", action="store_true",
                        help="let the controller choose to backchannel while silent")
    parser.add_argument("--stop-on-words", action="store_true",
                        help="while speaking, decide as soon as a recognised word arrives instead of at the next tick")
    parser.add_argument("--trace", default="")
    args = parser.parse_args()
    run(MicroTurnSidecar, args=args)


if __name__ == "__main__":
    main()
