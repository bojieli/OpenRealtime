#!/usr/bin/env python3
"""Sidecar for NVIDIA NemotronLabs VoiceChat 11B, a native full-duplex model.

VoiceChat listens and speaks on one 80 ms frame clock: every microphone frame
advances a Conformer + NemotronH thinker by one text token, a talker turns the
text timeline into codec frames, and a codec decodes 22.05 kHz speech. The model
decides for itself when to speak, when to stop, and - through a separate
function channel - when to propose a tool call. That makes it the reference
for a native speech model with a tool channel.

This sidecar does not reimplement that scheduler. It is a per-session client of
the pinned serving route, vLLM-Omni's native duplex Realtime endpoint
(``/v1/realtime?duplex=1``, see ``deploy/duplex/services/voicechat.sh``):

* engine audio (PCM16 at the hello rate) is resampled to 16 kHz float32 and
  appended as 80 ms frames. When the engine stops sending audio - a client that
  stopped streaming, a benchmark's trailing gap - the sidecar keeps the model's
  clock running with silence frames, because a frame-locked model that is not
  fed does not merely wait, it stops existing in time;
* model audio is forwarded at the model's own 22.05 kHz rate;
* the text channel is the *assistant's* words (``text_delta``/``text_done``).
  This route exposes no user transcript, so ``transcript`` is not declared;
* the function channel is surfaced as protocol ``tool_call`` frames carrying the
  model's proposal. Whatever ``tool_result`` comes back - a real output or the
  engine's refusal - is returned to the model as a Realtime
  ``function_call_output``, which the server injects into the function channel
  on the frame clock. The model then speaks about the result (or the failure)
  by itself.

The route documents no client barge-in: an engine ``interrupt`` is applied as a
local output gate (the rest of the current response is dropped and the turn
ends), while the model keeps its own state. Overlap handling proper is the
model's: it hears the user while it talks and yields on its own.

    openrealtime conformance sidecar -- python3 sidecars/voicechat_sidecar.py --mock
    openrealtime serve -binding duplex \\
        -sidecar "python3 sidecars/voicechat_sidecar.py --server ws://127.0.0.1:9140/v1/realtime" ...
"""

from __future__ import annotations

import argparse
import base64
import json
import math
import queue
import threading
import time
import unicodedata
import uuid
import warnings
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log, run  # noqa: E402

# The sync client is held for the session's lifetime rather than a with-block;
# websockets 16+ warns about exactly that usage.
warnings.filterwarnings("ignore", message=r"connect\(\) must be used as a context manager")

DEFAULT_SERVER = "ws://127.0.0.1:9140/v1/realtime"
DEFAULT_MODEL = "nemotron-voicechat"
MODEL_IDENTITY = "nvidia/NVIDIA-NemotronLabs-VoiceChat-11B@a4c40ca5 via configured-vllm-omni@9005d789 duplex"
#: The model hears 16 kHz and speaks 22.05 kHz, one frame per 80 ms.
MODEL_INPUT_RATE = 16_000
MODEL_OUTPUT_RATE = 22_050
FRAME_SAMPLES = 1_280
FRAME_SECONDS = FRAME_SAMPLES / MODEL_INPUT_RATE
#: The route accepts at most five tools per session.
MAX_TOOLS = 5
#: The model card's system prompt, used when the engine supplies none. The
#: route's own default asks the model to open by greeting the user, which a
#: measured conversation should not start with.
DEFAULT_INSTRUCTIONS = (
    "You are an AI voice assistant developed by NVIDIA. Your name is NVIDIA Voice Chat. Your job is "
    "to be helpful and harmless and have engaging conversations in English. Maintain a warm and "
    "friendly tone. Keep the dialogue open and ongoing. Be clear and direct, especially when "
    "answering yes or no questions and multiple-choice questions. Avoid long answers unless the user "
    "asks you to provide details or context. You must provide diverse responses and rephrase answers "
    "if the user asks the same question. DO NOT interrupt the user when they are speaking, let them "
    "finish their turn before answering.")
#: The model card's function-calling decision process, added when tools exist.
TOOL_INSTRUCTIONS = (
    "When you receive a request, follow this decision process: 1. Does the request match one of your "
    "available tools below? If yes, you MUST call that tool - never answer it directly from your own "
    "knowledge. 2. Is it a general knowledge question? If yes, answer directly from your own knowledge "
    "- do not call any tool. 3. Does it require an external action or live data that none of your "
    "tools cover? If yes, politely say you don't have that capability. Tool-call arguments must be "
    "values the user spoke. If a required argument is missing, ask the user; never guess. If a tool "
    "call fails or returns an error, do not retry the tool call for the same request. Tell the user "
    "that the API has an issue.")
#: Leading engine audio that may be discarded when a tool catalog arrives late.
RESTART_AUDIO_FRAMES = 25


class StreamResampler:
    """Stateful mono resampler: windowed-sinc anti-alias FIR plus interpolation.

    Chunk boundaries are invisible because the filter history and the
    fractional read position are carried between calls - resampling each
    engine frame independently would click at every 20 ms boundary.
    """

    def __init__(self, source_rate: int, target_rate: int, taps: int = 63) -> None:
        self.source_rate = int(source_rate)
        self.target_rate = int(target_rate)
        self.step = self.source_rate / self.target_rate
        self.position = 0.0
        self.buffer = np.zeros(0, dtype=np.float64)
        self.kernel: np.ndarray | None = None
        self.history = np.zeros(0, dtype=np.float64)
        if self.target_rate < self.source_rate:
            cutoff = 0.45 * self.target_rate / self.source_rate
            n = np.arange(taps) - (taps - 1) / 2
            kernel = 2 * cutoff * np.sinc(2 * cutoff * n) * np.hamming(taps)
            self.kernel = kernel / kernel.sum()
            self.history = np.zeros(taps - 1, dtype=np.float64)

    def process(self, samples: np.ndarray) -> np.ndarray:
        if self.source_rate == self.target_rate:
            return samples.astype(np.float32)
        samples = samples.astype(np.float64)
        if self.kernel is not None:
            extended = np.concatenate([self.history, samples])
            filtered = np.convolve(extended, self.kernel, mode="valid")
            self.history = extended[len(extended) - len(self.history):]
            samples = filtered
        self.buffer = np.concatenate([self.buffer, samples])
        available = len(self.buffer) - 1
        if available <= self.position:
            return np.zeros(0, dtype=np.float32)
        count = int(math.floor((available - self.position) / self.step)) + 1
        positions = self.position + self.step * np.arange(count)
        positions = positions[positions < available]
        base = np.floor(positions).astype(np.int64)
        fraction = positions - base
        out = self.buffer[base] * (1 - fraction) + self.buffer[base + 1] * fraction
        next_position = (positions[-1] + self.step) if positions.size else self.position
        drop = int(math.floor(next_position))
        drop = min(drop, len(self.buffer) - 1)
        self.buffer = self.buffer[drop:]
        self.position = next_position - drop
        return out.astype(np.float32)


def ascii_prompt(text: str) -> str:
    """VoiceChat requires ASCII-only system prompts and tool responses."""
    replacements = {
        "—": " - ", "–": "-", "‘": "'", "’": "'",
        "“": '"', "”": '"', "…": "...", "°": " degrees",
    }
    for source, target in replacements.items():
        text = text.replace(source, target)
    normalized = unicodedata.normalize("NFKD", text)
    return normalized.encode("ascii", "ignore").decode("ascii")


class ControlTap:
    """Let a duplex sidecar act on ``interrupt`` the moment it is read.

    The base read loop only sets a flag for ``interrupt``, because turn-based
    sidecars check it between generation chunks. A duplex model speaks with no
    ``respond`` in flight, so nothing would ever read that flag. The tap sees
    each header line (payloads are read with ``read``, never ``readline``) and
    calls back before the base loop handles the frame.
    """

    def __init__(self, stream, on_interrupt) -> None:
        self._stream = stream
        self._on_interrupt = on_interrupt

    def readline(self, limit: int = -1) -> bytes:
        line = self._stream.readline(limit)
        if line and (b"interrupt" in line or b"stop-speaking" in line):
            try:
                header = json.loads(line)
            except ValueError:
                return line
            if header.get("type") == "interrupt" or (
                    header.get("type") == "interaction_act" and header.get("act") in ("stop-speaking", "interrupt")):
                try:
                    self._on_interrupt()
                except Exception as failure:  # noqa: BLE001
                    log(f"interrupt handler failed: {failure}")
        return line

    def read(self, size: int = -1) -> bytes:
        return self._stream.read(size)


class VoiceChatSidecar(Sidecar):
    """NemotronLabs VoiceChat behind the sidecar protocol."""

    model_name = MODEL_IDENTITY
    output_rate = MODEL_OUTPUT_RATE
    capabilities = (
        Capability.FULL_DUPLEX,
        Capability.NATIVE_INTERACTION,
        # The model owns its floor (model-native turn policy). This route
        # exposes no user-activity events, so no speech_started/stopped frames
        # are emitted: the declaration is about ownership, not a VAD stream.
        Capability.NATIVE_VAD,
        Capability.BARGE_IN,
        Capability.TOOLS,
    )

    def __init__(self, input_stream, output_stream, *, server: str, served_model: str,
                 mock: bool, instructions: str | None, idle_fill_ms: int,
                 forward_commit: bool, event_log: str | None, connect_timeout: float,
                 mock_tool_call: bool, output_quiet_ms: int = 0) -> None:
        super().__init__(ControlTap(input_stream, self.on_interrupt), output_stream)
        self.server = server
        self.served_model = served_model
        self.mock = mock
        if mock:
            self.model_name = "voicechat-protocol-mock (no model inference)"
        self.instructions_override = instructions
        self.idle_fill_seconds = max(0.0, idle_fill_ms / 1000.0)
        self.forward_commit = forward_commit
        self.connect_timeout = connect_timeout
        self.mock_tool_call = mock_tool_call
        if output_quiet_ms < 0:
            raise ValueError("output_quiet_ms cannot be negative")
        self.output_quiet_ms = output_quiet_ms
        self._output_audible = False
        self._output_quiet_samples = 0
        self._event_log = open(event_log, "a", encoding="utf-8") if event_log else None
        self._event_log_lock = threading.Lock()
        self._t0 = time.monotonic()
        self._resampler: StreamResampler | None = None
        self._pending = np.zeros(0, dtype=np.float32)
        self._frames: queue.Queue[np.ndarray] = queue.Queue(maxsize=512)
        self._stop = threading.Event()
        self._ws = None
        self._ws_ready = threading.Event()
        self._ws_failed: str | None = None
        self._session_closed = threading.Event()
        self._threads: list[threading.Thread] = []
        self._audio_end_ms = 0
        self._real_frames = 0
        self._fill_frames = 0
        self._tools_signature = "[]"
        self._session_tools: list[dict] = []
        # Response state (reader thread).
        self._state_lock = threading.Lock()
        # Serialize output admission/write with the local interrupt boundary.
        self._output_lock = threading.Lock()
        self._response_active = False
        self._response_text: list[str] = []
        self._gated_response: str | None = None
        self._current_response: str | None = None
        self._turn_finished = threading.Condition(self._state_lock)
        self._turns_done = 0
        self._calls: dict[str, dict] = {}
        # Mock state.
        self._mock_speaking = threading.Event()
        self._mock_cancel = threading.Event()

    # --- diagnostics ----------------------------------------------------------

    def _record(self, direction: str, event: dict) -> None:
        if self._event_log is None:
            return
        entry = {"t": round(time.monotonic() - self._t0, 4), "dir": direction}
        compact = dict(event)
        for key in ("audio", "delta"):
            value = compact.get(key)
            if isinstance(value, str) and len(value) > 64 and compact.get("type", "").endswith(
                ("audio.delta", "buffer.append")):
                compact[key] = f"<{len(value)} b64 chars>"
        entry["event"] = compact
        with self._event_log_lock:
            self._event_log.write(json.dumps(entry, ensure_ascii=False) + "\n")
            self._event_log.flush()

    # --- lifecycle ------------------------------------------------------------

    def configure(self, hello) -> None:
        self._resampler = StreamResampler(self.input_rate, MODEL_INPUT_RATE)
        self._session_tools = self._realtime_tools(self.tools)
        log(f"voicechat hello: rate={self.input_rate} tools={[t['name'] for t in self._session_tools]} "
            f"instructions={self.instructions[:160]!r}")
        if self.mock:
            log("voicechat sidecar running in mock mode; no model server is contacted")
            thread = threading.Thread(target=self._mock_clock, daemon=True, name="mock-clock")
            thread.start()
            self._threads.append(thread)
            return
        self._open_session()

    def on_close(self) -> None:
        self._stop.set()
        if self._ws is not None and not self.mock:
            try:
                self._ws_send({"type": "session.close"})
                self._session_closed.wait(timeout=5.0)
            except Exception:  # noqa: BLE001 - closing is best effort
                pass
            try:
                self._ws.close()
            except Exception:  # noqa: BLE001
                pass
        for thread in self._threads:
            thread.join(timeout=5)
        log(f"voicechat session closed: real_frames={self._real_frames} "
            f"fill_frames={self._fill_frames} audio_end_ms={self._audio_end_ms}")
        if self._event_log is not None:
            self._event_log.close()

    # --- upstream session -------------------------------------------------------

    def _instructions(self) -> str:
        if self.instructions_override is not None:
            text = ascii_prompt(self.instructions_override).strip()
        else:
            text = ascii_prompt(self.instructions or "").strip() or DEFAULT_INSTRUCTIONS
        if self._session_tools:
            text = f"{text}\n\n{TOOL_INSTRUCTIONS}"
        return text

    @staticmethod
    def _realtime_tools(tools: list[dict]) -> list[dict]:
        converted = []
        for tool in tools or []:
            definition = tool.get("function", tool) if isinstance(tool, dict) else None
            if not isinstance(definition, dict) or not str(definition.get("name", "")).strip():
                continue
            converted.append({
                "type": "function",
                "name": str(definition["name"]),
                "description": ascii_prompt(str(definition.get("description", ""))),
                "parameters": definition.get("parameters") or {"type": "object", "properties": {}},
            })
        if len(converted) > MAX_TOOLS:
            log(f"voicechat accepts at most {MAX_TOOLS} tools; keeping the first {MAX_TOOLS} of {len(converted)}")
            converted = converted[:MAX_TOOLS]
        return converted

    def _open_session(self) -> None:
        from websockets.sync.client import connect  # noqa: PLC0415

        session_id = f"openrealtime-{uuid.uuid4().hex}"
        separator = "&" if "?" in self.server else "?"
        url = (f"{self.server}{separator}duplex=1&model={self.served_model}"
               f"&autostart=0&session_id={session_id}")
        deadline = time.monotonic() + self.connect_timeout
        last_error = ""
        # The route admits one live session. A previous session that is still
        # draining (disconnect grace) refuses admission briefly, so retry
        # until the connect deadline rather than failing the engine session.
        while True:
            try:
                self._ws = connect(url, max_size=None, open_timeout=10, close_timeout=5)
                self._session_closed.clear()
                payload: dict = {
                    "session_id": session_id,
                    "model": self.served_model,
                    "modalities": ["audio", "text"],
                    "input_audio_format": "pcm_f32le",
                    "output_audio_format": "pcm16",
                    "idle_timeout_s": 600,
                    "turn_detection": None,
                    "extra_body": {"auto_response": True},
                }
                instructions = self._instructions()
                if instructions:
                    payload["instructions"] = instructions
                if self._session_tools:
                    payload["tools"] = self._session_tools
                self._tools_signature = json.dumps(self._session_tools, sort_keys=True)
                self._ws_send({"type": "session.update", "session": payload})
                created = self._await_created(deadline)
                if created is None:
                    raise RuntimeError(self._ws_failed or "no session.created")
                break
            except Exception as failure:  # noqa: BLE001
                last_error = str(failure)
                try:
                    if self._ws is not None:
                        self._ws.close()
                except Exception:  # noqa: BLE001
                    pass
                self._ws = None
                if time.monotonic() >= deadline:
                    raise RuntimeError(f"voicechat session could not start: {last_error}") from failure
                log(f"voicechat session admission failed ({last_error}); retrying")
                time.sleep(1.0)
        session = created.get("session") if isinstance(created, dict) else None
        capabilities = session.get("capabilities") if isinstance(session, dict) else None
        log(f"voicechat upstream session {session_id} created; capabilities="
            f"{json.dumps(capabilities, sort_keys=True) if capabilities else None}")
        reader = threading.Thread(target=self._read_upstream, daemon=True, name="upstream-reader")
        sender = threading.Thread(target=self._send_frames, daemon=True, name="frame-clock")
        reader.start()
        sender.start()
        self._threads.extend([reader, sender])
        self._ws_ready.set()

    def _await_created(self, deadline: float) -> dict | None:
        while time.monotonic() < deadline:
            raw = self._ws.recv(timeout=max(0.1, deadline - time.monotonic()))
            event = json.loads(raw)
            self._record("in", event)
            kind = event.get("type")
            if kind == "session.created":
                return event
            if kind == "error" or kind == "session.error":
                self._ws_failed = json.dumps(event.get("error") or event)[:500]
                return None
        return None

    def _ws_send(self, event: dict) -> None:
        self._record("out", event)
        self._ws.send(json.dumps(event))

    # --- input ------------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        samples = np.frombuffer(pcm16, dtype=np.int16).astype(np.float32) / 32768.0
        converted = self._resampler.process(samples) if self._resampler else samples
        self._pending = np.concatenate([self._pending, converted])
        while self._pending.size >= FRAME_SAMPLES:
            frame = self._pending[:FRAME_SAMPLES].copy()
            self._pending = self._pending[FRAME_SAMPLES:]
            try:
                self._frames.put_nowait(frame)
            except queue.Full:
                # A backlog the model hears late is worse than a gap: drop the
                # oldest frame, and say so.
                try:
                    self._frames.get_nowait()
                    self._frames.put_nowait(frame)
                    log("voicechat input backlog full; dropped the oldest 80 ms frame")
                except queue.Empty:
                    pass

    def _append(self, frame: np.ndarray) -> None:
        self._audio_end_ms += 80
        self._ws_send({
            "type": "input_audio_buffer.append",
            "audio": base64.b64encode(frame.astype("<f4").tobytes()).decode("ascii"),
            "format": "pcm_f32le",
            "sample_rate_hz": MODEL_INPUT_RATE,
            "duration_ms": 80,
            "audio_end_ms": self._audio_end_ms,
        })

    def _send_frames(self) -> None:
        """Feed the model's frame clock.

        Real frames go out as soon as they arrive. When none has arrived for
        ``idle_fill`` seconds, silence frames are appended at the 80 ms period
        until real audio resumes: silence is what a microphone that sends
        nothing is hearing.
        """
        silence = np.zeros(FRAME_SAMPLES, dtype=np.float32)
        last_sent = time.monotonic()
        next_fill: float | None = None
        try:
            while not self._stop.is_set():
                try:
                    frame = self._frames.get(timeout=0.01)
                except queue.Empty:
                    frame = None
                now = time.monotonic()
                if frame is not None:
                    self._append(frame)
                    self._real_frames += 1
                    last_sent = now
                    next_fill = None
                    continue
                if self.idle_fill_seconds <= 0:
                    continue
                if next_fill is None:
                    if now - last_sent >= self.idle_fill_seconds:
                        next_fill = now
                    else:
                        continue
                if now >= next_fill:
                    self._append(silence)
                    self._fill_frames += 1
                    last_sent = now
                    next_fill += FRAME_SECONDS
        except Exception as failure:  # noqa: BLE001
            if not self._stop.is_set():
                self.error(f"voicechat frame clock stopped: {failure}", code="upstream_send_failed", fatal=True)

    # --- output -----------------------------------------------------------------

    def _read_upstream(self) -> None:
        try:
            while not self._stop.is_set():
                try:
                    raw = self._ws.recv(timeout=0.5)
                except TimeoutError:
                    continue
                event = json.loads(raw)
                self._record("in", event)
                self._handle_event(event)
        except Exception as failure:  # noqa: BLE001
            if not self._stop.is_set():
                log(f"voicechat upstream closed: {failure}")
                self.error(f"voicechat upstream closed: {failure}", code="upstream_closed", fatal=True)
        finally:
            self._session_closed.set()
            with self._state_lock:
                self._turn_finished.notify_all()

    def _handle_event(self, event: dict) -> None:
        with self._output_lock:
            self._handle_output_event(event)

    def _handle_output_event(self, event: dict) -> None:
        kind = str(event.get("type", ""))
        response_id = str(event.get("response_id") or (event.get("response") or {}).get("id") or "")
        if kind == "response.created":
            with self._state_lock:
                self._current_response = response_id or f"resp-{uuid.uuid4().hex[:8]}"
                self._response_active = True
                self._response_text = []
            return
        if kind == "response.output_audio.delta":
            if self._gated(response_id):
                return
            payload = base64.b64decode(str(event.get("delta", "")))
            rate = int(event.get("sample_rate_hz") or MODEL_OUTPUT_RATE)
            if rate != MODEL_OUTPUT_RATE:
                log(f"voicechat audio arrived at {rate} Hz, declared {MODEL_OUTPUT_RATE}")
            if self.output_quiet_ms:
                self._segmented_audio(payload)
            else:
                with self._state_lock:
                    self._response_active = True
                self.audio(payload)
            return
        if kind in ("response.output_audio_transcript.delta", "response.output_text.delta",
                    "response.text.delta"):
            if self._gated(response_id):
                return
            delta = str(event.get("delta", ""))
            if delta:
                with self._state_lock:
                    self._response_text.append(delta)
                    self._response_active = True
                self.text_delta(delta)
            return
        if kind == "response.function_call_arguments.done":
            self._propose_call(event)
            return
        if kind == "response.output_item.done":
            item = event.get("item")
            if isinstance(item, dict) and item.get("type") == "function_call":
                self._propose_call(item)
            return
        if kind == "response.done":
            self._finish_turn(response_id, event)
            return
        if kind == "session.closed":
            self._session_closed.set()
            return
        if kind == "error":
            detail = event.get("error") or event
            # Preserve upstream failure as protocol evidence; a log-only
            # message lets a broken session appear successful to benchmarks.
            self.error(f"voicechat upstream error: {json.dumps(detail)[:400]}",
                       code="upstream_error", fatal=False)
            return

    def _segmented_audio(self, payload: bytes) -> None:
        """Adapter output boundary; upstream EOS may wait for the next user.

        Count decoded PCM time, not a wall-clock gap caused by a stalled GPU.
        Retain quiet packets during speech; suppress idle codec silence.
        """
        samples = np.frombuffer(payload, dtype="<i2").astype(np.float32) / 32768.0
        loud = bool(samples.size) and float(np.sqrt(np.mean(samples * samples))) >= 0.01
        if loud:
            self._output_audible = True
            self._output_quiet_samples = 0
        elif self._output_audible:
            self._output_quiet_samples += samples.size
        if not self._output_audible:
            return
        with self._state_lock:
            self._response_active = True
        self.audio(payload)
        if not loud and self._output_quiet_samples * 1000 >= self.output_quiet_ms * MODEL_OUTPUT_RATE:
            self._output_audible = False
            self._output_quiet_samples = 0
            with self._state_lock:
                text = "".join(self._response_text)
                self._response_text = []
                self._response_active = False
                self._turns_done += 1
                self._turn_finished.notify_all()
            self.send("log", text=f"voicechat adapter output boundary: {self.output_quiet_ms} ms decoded silence; not model EOS")
            if text.strip():
                self.text_done(text)
            self.turn_done()

    def _gated(self, response_id: str) -> bool:
        with self._state_lock:
            return bool(self._gated_response) and (
                not response_id or self._gated_response in ("*", response_id))

    def _propose_call(self, event: dict) -> None:
        call_id = str(event.get("call_id") or "")
        name = str(event.get("name") or "")
        if not call_id or not name or call_id in self._calls:
            return
        raw_arguments = event.get("arguments") or "{}"
        try:
            arguments = json.loads(raw_arguments) if isinstance(raw_arguments, str) else raw_arguments
        except json.JSONDecodeError:
            arguments = {"_unparsed": str(raw_arguments)}
        if not isinstance(arguments, dict):
            arguments = {"value": arguments}
        self._calls[call_id] = {"name": name, "proposed_at": time.monotonic(), "arguments": arguments}
        log(f"voicechat function channel proposed {name}({json.dumps(arguments)}) call_id={call_id}")
        self.send("tool_call", call_id=call_id, name=name, arguments=arguments)

    def _finish_turn(self, response_id: str, event: dict) -> None:
        self._output_audible = False
        self._output_quiet_samples = 0
        with self._state_lock:
            was_gated = bool(self._gated_response) and (
                not response_id or self._gated_response in ("*", response_id))
            if was_gated:
                self._gated_response = None
            text = "".join(self._response_text)
            active = self._response_active
            self._response_active = False
            self._response_text = []
            self._current_response = None
            self._turns_done += 1
            self._turn_finished.notify_all()
        if was_gated:
            # The engine already received this turn's boundary at interrupt.
            return
        if text.strip():
            self.text_done(text)
        if active or text:
            self.turn_done()

    # --- engine controls ----------------------------------------------------------

    def on_commit(self) -> None:
        if self.mock or not self.forward_commit or self._ws is None:
            return
        # Non-final: the client said its turn ended; the session goes on.
        self._ws_send({"type": "input_audio_buffer.commit"})

    def on_text(self, text: str, role: str) -> None:
        # The route has no mid-session text injection: the prompt is resident
        # in the thinker's KV and cannot change inside an incarnation. The
        # background reasoner's answer is therefore not spliced in.
        log(f"voicechat ignored injected {role} text ({len(text)} chars): no text-injection seam on this route")

    def on_tool_result(self, message) -> None:
        call_id = str(message.get("call_id", ""))
        call = self._calls.get(call_id)
        if call is None:
            self.error(f"voicechat result has no proposed call: {call_id}",
                       code="unknown_tool_call", fatal=False)
            return
        if call.get("result_delivered"):
            self.send("log", text=f"voicechat duplicate tool result ignored: {call_id}")
            return
        if message.get("error"):
            output = ascii_prompt(f"Error: {message.get('error')}")
        else:
            value = message.get("output")
            output = ascii_prompt(value if isinstance(value, str) else json.dumps(value))
        latency = (time.monotonic() - call["proposed_at"]) if call else float("nan")
        log(f"voicechat tool_result for {call_id} after {latency:.2f}s: {output[:200]}")
        if self.mock:
            self._mock_speak(f"The tool said: {output[:80]}")
            call["result_delivered"] = True
            return
        if self._ws is None:
            return
        self._ws_send({
            "type": "conversation.item.create",
            "item": {"type": "function_call_output", "call_id": call_id, "output": output},
        })
        call["result_delivered"] = True

    def on_tools_update(self, tools: list[dict]) -> None:
        self.tools = tools
        converted = self._realtime_tools(tools)
        signature = json.dumps(converted, sort_keys=True)
        if signature == self._tools_signature:
            return
        if self.mock:
            self._session_tools, self._tools_signature = converted, signature
            return
        if self._real_frames > RESTART_AUDIO_FRAMES:
            # The tool prompt is resident in the thinker's KV; changing it would
            # need a new incarnation and would forget the conversation.
            self.error("voicechat cannot change tools once the conversation has started",
                       code="tools_immutable")
            return
        log(f"voicechat tools changed after {self._real_frames * 80} ms of engine audio; "
            "reopening the upstream session so the tool prompt enters the model")
        self._session_tools = converted
        self._restart_session()

    def _restart_session(self) -> None:
        self._stop.set()
        try:
            self._ws_send({"type": "session.close"})
            self._session_closed.wait(timeout=5.0)
        except Exception:  # noqa: BLE001
            pass
        try:
            self._ws.close()
        except Exception:  # noqa: BLE001
            pass
        for thread in self._threads:
            thread.join(timeout=5)
        self._threads = []
        self._stop = threading.Event()
        self._audio_end_ms = 0
        self._open_session()

    def on_respond(self) -> None:
        """An explicit turn request.

        The model owns its floor, so there is no way to make it speak now.
        In mock mode a canned turn is produced; with the model, the request
        waits (bounded) for the model's own next turn to finish so that the
        engine's boundary lands after it rather than splitting it.
        """
        if self.mock:
            self._mock_speak("This is the VoiceChat sidecar running without a model.")
            return
        with self._state_lock:
            target = self._turns_done + (1 if self._response_active else 0)
            if not self._response_active:
                self.send("log", text="voicechat owns its floor; respond is advisory and waits for its next turn")
            deadline = time.monotonic() + 20.0
            while self._turns_done < max(target, 1) and not self.interrupted() and not self._stop.is_set():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                self._turn_finished.wait(timeout=min(remaining, 0.5))

    def on_interrupt(self) -> None:
        """Stop the current response now, on the protocol read thread."""
        if self.mock:
            self._mock_cancel.set()
            return
        with self._output_lock:
            self._interrupt_output()

    def _interrupt_output(self) -> None:
        with self._state_lock:
            active = self._response_active
            if active:
                self._gated_response = self._current_response or "*"
                self._response_active = False
                text = "".join(self._response_text)
                self._response_text = []
        if active:
            if text.strip():
                self.text_done(text)
            self.turn_done()
        try:
            self._ws_send({"type": "response.cancel"})
        except Exception:  # noqa: BLE001
            pass

    # --- mock -------------------------------------------------------------------

    def _mock_clock(self) -> None:
        """Emulate a frame-locked duplex model for plumbing tests.

        An energy detector stands in for the model's hearing: speech for 240 ms
        opens a user turn, 800 ms of quiet ends it, and the mock answers - or,
        with ``--mock-tool-call``, proposes a call first and answers once the
        result arrives. Speech during the mock's answer stops it (native
        barge-in).
        """
        voiced, quiet, in_speech, turns = 0, 0, False, 0
        while not self._stop.is_set():
            try:
                frame = self._frames.get(timeout=0.2)
            except queue.Empty:
                frame = None
            if frame is None:
                if in_speech:
                    quiet += 3
                else:
                    continue
            else:
                energy = float(np.sqrt(np.mean(np.square(frame))))
                if energy > 0.01:
                    voiced, quiet = voiced + 1, 0
                else:
                    voiced, quiet = 0, quiet + 1
            if not in_speech and voiced >= 3:
                in_speech = True
                if self._mock_speaking.is_set():
                    self._mock_cancel.set()
            elif in_speech and quiet >= 10:
                in_speech = False
                turns += 1
                if self.mock_tool_call and self._session_tools and turns == 1:
                    tool = self._session_tools[0]
                    call_id = f"call_{uuid.uuid4().hex[:12]}"
                    self._calls[call_id] = {"name": tool["name"], "proposed_at": time.monotonic(),
                                            "arguments": {}}
                    self.send("tool_call", call_id=call_id, name=tool["name"], arguments={})
                    continue
                threading.Thread(target=self._mock_speak,
                                 args=(f"Mock answer number {turns}.",), daemon=True).start()

    def _mock_speak(self, text: str) -> None:
        if self._mock_speaking.is_set():
            return
        self._mock_speaking.set()
        self._mock_cancel.clear()
        try:
            self.text_delta(text)
            packet = int(MODEL_OUTPUT_RATE * FRAME_SECONDS)
            t = np.arange(packet) / MODEL_OUTPUT_RATE
            for index in range(10):
                if self._mock_cancel.is_set() or self.interrupted() or self._stop.is_set():
                    break
                tone = 0.05 * np.sin(2 * np.pi * 180 * (t + index * FRAME_SECONDS))
                self.audio((tone * 32767).astype(np.int16).tobytes())
                time.sleep(FRAME_SECONDS / 4)
            self.text_done(text)
            self.turn_done()
        finally:
            self._mock_speaking.clear()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--server", default=DEFAULT_SERVER, help="vLLM-Omni Realtime URL of the VoiceChat engine")
    parser.add_argument("--served-model", default=DEFAULT_MODEL, help="served model name on that engine")
    parser.add_argument("--instructions", default=None,
                        help="override the session instructions (default: the engine hello's, ASCII-folded)")
    parser.add_argument("--idle-fill-ms", type=int, default=240,
                        help="append silence frames after this long without engine audio; 0 disables")
    parser.add_argument("--no-forward-commit", action="store_true",
                        help="do not forward engine commit frames as input_audio_buffer.commit")
    parser.add_argument("--output-quiet-ms", type=int, default=0,
                        help="opt-in adapter output boundary after decoded silence (0: upstream EOS only)")
    parser.add_argument("--event-log", default=None, help="append every upstream event (JSONL) here")
    parser.add_argument("--connect-timeout", type=float, default=45.0,
                        help="seconds to wait for the one-session engine to admit this session")
    parser.add_argument("--mock", action="store_true",
                        help="speak the protocol without a model server, for plumbing and conformance")
    parser.add_argument("--mock-tool-call", action="store_true",
                        help="in mock mode, propose a call to the first declared tool after the first user turn")
    arguments = parser.parse_args()
    run(
        VoiceChatSidecar, server=arguments.server, served_model=arguments.served_model,
        mock=arguments.mock, instructions=arguments.instructions,
        idle_fill_ms=arguments.idle_fill_ms, forward_commit=not arguments.no_forward_commit,
        event_log=arguments.event_log, connect_timeout=arguments.connect_timeout,
        mock_tool_call=arguments.mock_tool_call, output_quiet_ms=arguments.output_quiet_ms,
    )


if __name__ == "__main__":
    main()
