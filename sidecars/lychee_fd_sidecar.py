#!/usr/bin/env python3
"""Lychee-FD behind sidecar protocol v1.

Lychee-FD (HITsz-TMG, ACL 2026) is a native full-duplex speech model: shared
lower layers of a Qwen2-7B-sized backbone, then separate upper semantic
(text), acoustic (speech token) and dialogue-control streams, plus the
Step-Audio-2-mini Token2Wav vocoder (24 kHz). The control stream runs an
early-exit head (branching after layer 8) that emits one decision per
10-token control chunk: while listening keep-listening (K-L) or
start-speaking (S-S), while speaking keep-speaking (K-S) or start-listening
(S-L, i.e. yield or be interrupted).

The model is served by the upstream realtime backend (``lychee_fd.app`` with
its patched vLLM, plus the Token2Wav server), which already runs the
continuous duplex loop for its own browser frontend. This sidecar is a
per-session client of that realtime API (the same calls the upstream
frontend makes)::

    POST /api/realtime/session/start           -> session_id
    POST /api/realtime/session/{id}/chunk      16 kHz WAV chunks, paced by arrival
    GET  /api/realtime/session/{id}/events     SSE: state_change, assistant_text,
                                               audio_chunk_pcm (24 kHz s16le), audio_interrupt
    POST /api/realtime/session/{id}/stop

What the model really does, and so what the sidecar declares:

* ``full_duplex``: input keeps flowing into the model while it speaks;
* ``native_interaction``: the control stream decides when to speak and when
  to yield; an engine ``commit`` is accepted and ignored, because the model
  does not take endpoints from outside;
* ``barge_in``: while speaking, the control head decides S-L on overlapping
  speech and the backend aborts the vocoder stream (``audio_interrupt``).

It does not declare ``native_vad``: the model exposes no user speech start or
stop, only its own speak/listen decisions, so the engine must keep the
acoustic floor (``-binding duplex -floor engine``). It does not declare
``transcript``: its text stream is what the assistant says, not what it heard
(assistant text goes out as ``text_delta``/``text_done``). It cannot take
injected text or be ordered to speak: a ``respond`` waits for the model's own
next turn and ends empty if none comes.

Every control decision is logged (stderr and ``--control-log`` JSONL) with
the backend's commit time and the input-audio position it applies to, so the
control-event delay can be measured separately from first audio.

    python sidecars/lychee_fd_sidecar.py --mock
    python sidecars/lychee_fd_sidecar.py --backend http://127.0.0.1:9143
    openrealtime conformance sidecar -- python3 sidecars/lychee_fd_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import base64
import http.client
import json
import os
import queue
import sys
import threading
import time
import traceback
import urllib.parse
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log  # noqa: E402

MODEL_NAME = "HIT-TMG/Lychee-FD"
MODEL_INPUT_RATE = 16_000
MODEL_OUTPUT_RATE = 24_000
DEFAULT_BACKEND = "http://127.0.0.1:9143"


def _now_ms() -> int:
    return int(time.time() * 1000)


class ControlLog:
    """Timestamped control decisions: stderr and an optional JSONL file."""

    def __init__(self, path: str | None) -> None:
        self._lock = threading.Lock()
        self._file = open(path, "a", encoding="utf-8") if path else None  # noqa: SIM115

    def event(self, session: str, name: str, **fields) -> None:
        record = {"t_ms": _now_ms(), "session": session, "event": name, **fields}
        line = json.dumps(record, ensure_ascii=False)
        log(f"[control] {line}")
        if self._file is not None:
            with self._lock:
                self._file.write(line + "\n")
                self._file.flush()


def wav_bytes(pcm16: bytes, rate: int) -> bytes:
    """A minimal PCM16 mono WAV container around raw samples."""
    import struct  # noqa: PLC0415

    header = struct.pack("<4sI4s4sIHHIIHH4sI", b"RIFF", 36 + len(pcm16), b"WAVE", b"fmt ", 16,
                         1, 1, rate, rate * 2, 2, 16, b"data", len(pcm16))
    return header + pcm16


class LycheeSession:
    """One upstream realtime session: chunk uploads plus the SSE event stream.

    ``on_event(event, recv_ms)`` is called on the SSE thread for every event.
    Chunk uploads run on their own thread so a slow request never blocks the
    caller that is feeding audio.
    """

    def __init__(self, base_url: str, *, start_payload: dict, on_event, chunk_ms: int = 200,
                 idle_fill_ms: int = 400) -> None:
        parsed = urllib.parse.urlparse(base_url)
        self.host = parsed.hostname or "127.0.0.1"
        self.port = parsed.port or 80
        self.start_payload = start_payload
        self.on_event = on_event
        self.chunk_bytes = int(MODEL_INPUT_RATE * chunk_ms / 1000) * 2
        self.session_id = ""
        self._pending = bytearray()
        self._uploads: queue.Queue[bytes | None] = queue.Queue()
        self._sent_samples = 0
        #: (first sample, sample count, wall ms when POSTed) per uploaded chunk.
        self.chunk_times: list[tuple[int, int, int]] = []
        self._times_lock = threading.Lock()
        self._done = threading.Event()
        self._threads: list[threading.Thread] = []
        self.upload_errors = 0
        # The backend only advances when input arrives: a client that stops
        # sending (a replayed file ending, a muted track) freezes an answer
        # mid-sentence. Like the Moshi loop, keep wall-clock time flowing with
        # silence once input falls further behind than this; 0 disables it.
        self.idle_fill_ms = idle_fill_ms
        self.filled_samples = 0
        self._pushed_samples = 0
        self._first_push: float | None = None
        self._push_lock = threading.Lock()
        self._stopping = threading.Event()

    def _request(self, method: str, path: str, body: bytes | None = None,
                 headers: dict | None = None, timeout: float = 30.0) -> dict:
        connection = http.client.HTTPConnection(self.host, self.port, timeout=timeout)
        try:
            connection.request(method, path, body=body, headers=headers or {})
            response = connection.getresponse()
            data = response.read()
            if response.status >= 400:
                raise RuntimeError(f"{method} {path}: HTTP {response.status}: {data[:300]!r}")
            return json.loads(data) if data else {}
        finally:
            connection.close()

    def start(self) -> dict:
        result = self._request("POST", "/api/realtime/session/start",
                               json.dumps(self.start_payload).encode(),
                               {"Content-Type": "application/json"})
        self.session_id = result["session_id"]
        for target in (self._events_loop, self._upload_loop, self._fill_loop):
            thread = threading.Thread(target=target, daemon=True)
            thread.start()
            self._threads.append(thread)
        return result

    def push(self, pcm16: bytes, now: float | None = None) -> None:
        with self._push_lock:
            if self._first_push is None:
                self._first_push = time.monotonic() if now is None else now
            self._append(pcm16)

    def _append(self, pcm16: bytes) -> None:
        self._pushed_samples += len(pcm16) // 2
        self._pending.extend(pcm16)
        while len(self._pending) >= self.chunk_bytes:
            self._uploads.put(bytes(self._pending[:self.chunk_bytes]))
            del self._pending[:self.chunk_bytes]

    def fill_silence(self, now: float | None = None) -> int:
        """Bring input up to the wall clock with silence once it lags by idle_fill_ms."""
        with self._push_lock:
            if self.idle_fill_ms <= 0 or self._first_push is None:
                return 0
            now = time.monotonic() if now is None else now
            behind = int((now - self._first_push) * MODEL_INPUT_RATE) - self._pushed_samples
            if behind * 1000 < self.idle_fill_ms * MODEL_INPUT_RATE:
                return 0
            self._append(bytes(2 * behind))
            self.filled_samples += behind
            return behind

    def _fill_loop(self) -> None:
        while not self._stopping.wait(0.1) and not self._done.is_set():
            self.fill_silence()

    def wall_ms_of(self, audio_ms: float) -> int | None:
        """Wall time at which input position ``audio_ms`` was handed to the backend."""
        sample = int(audio_ms * MODEL_INPUT_RATE / 1000)
        with self._times_lock:
            for first, count, sent in self.chunk_times:
                if first <= sample < first + count:
                    return sent
            if self.chunk_times and sample >= self.chunk_times[-1][0]:
                return self.chunk_times[-1][2]
        return None

    def _upload_loop(self) -> None:
        connection = http.client.HTTPConnection(self.host, self.port, timeout=30)
        path = f"/api/realtime/session/{self.session_id}/chunk"
        while True:
            chunk = self._uploads.get()
            if chunk is None:
                break
            sent = _now_ms()
            count = len(chunk) // 2
            with self._times_lock:
                self.chunk_times.append((self._sent_samples, count, sent))
            self._sent_samples += count
            body = wav_bytes(chunk, MODEL_INPUT_RATE)
            for attempt in range(2):
                try:
                    connection.request("POST", path, body=body, headers={
                        "Content-Type": "audio/wav", "x-client-chunk-sent-epoch-ms": str(sent)})
                    response = connection.getresponse()
                    response.read()
                    if response.status >= 400:
                        self.upload_errors += 1
                        log(f"lychee chunk upload HTTP {response.status}")
                    break
                except (OSError, http.client.HTTPException):
                    connection.close()
                    connection = http.client.HTTPConnection(self.host, self.port, timeout=30)
                    if attempt:
                        self.upload_errors += 1
        connection.close()

    def _events_loop(self) -> None:
        connection = http.client.HTTPConnection(self.host, self.port, timeout=None)
        try:
            connection.request("GET", f"/api/realtime/session/{self.session_id}/events",
                               headers={"Accept": "text/event-stream"})
            response = connection.getresponse()
            event_type, data_lines = "message", []
            while True:
                line = response.readline()
                if not line:
                    break
                line = line.decode("utf-8").rstrip("\r\n")
                if line.startswith("event:"):
                    event_type = line[6:].strip()
                elif line.startswith("data:"):
                    data_lines.append(line[5:].lstrip())
                elif not line:
                    if data_lines and event_type != "heartbeat":
                        recv = _now_ms()
                        try:
                            payload = json.loads("\n".join(data_lines))
                        except json.JSONDecodeError:
                            payload = {"raw": "\n".join(data_lines)}
                        payload.setdefault("type", event_type)
                        try:
                            self.on_event(payload, recv)
                        except Exception:  # noqa: BLE001
                            log(traceback.format_exc())
                        if event_type == "done":
                            break
                    event_type, data_lines = "message", []
        except Exception:  # noqa: BLE001
            log(traceback.format_exc())
        finally:
            connection.close()
            self._done.set()

    def stop(self, wait: float = 10.0) -> None:
        self._stopping.set()
        if self._pending:
            self._uploads.put(bytes(self._pending))
            self._pending.clear()
        self._uploads.put(None)
        if self.session_id:
            try:
                self._request("POST", f"/api/realtime/session/{self.session_id}/stop", b"")
            except Exception as failure:  # noqa: BLE001
                log(f"lychee stop: {failure}")
        self._done.wait(wait)


class LycheeSidecar(Sidecar):
    """One Lychee-FD session."""

    model_name = MODEL_NAME
    output_rate = MODEL_OUTPUT_RATE
    capabilities = (
        Capability.FULL_DUPLEX,
        Capability.NATIVE_INTERACTION,
        Capability.BARGE_IN,
    )

    def __init__(self, input_stream, output_stream, *, mock: bool, backend: str, voice: str,
                 start_speak_factor: float, chunk_ms: int, drain_ms: int, respond_wait: float,
                 control: ControlLog, idle_fill_ms: int = 400) -> None:
        super().__init__(input_stream, output_stream)
        self.mock = mock
        self.backend = backend
        self.voice_id = voice
        self.start_speak_factor = start_speak_factor
        self.chunk_ms = chunk_ms
        self.drain_ms = drain_ms
        self.idle_fill_ms = idle_fill_ms
        self.respond_wait = respond_wait
        self.control = control
        self.session_label = f"lfd-{os.getpid()}-{_now_ms()}"
        self._resampler = None
        self.session: LycheeSession | None = None
        self._lock = threading.RLock()
        self._model_state = "L"
        self._turn_open = False
        self._turn_text = ""
        self._turn_audio_samples = 0
        self._turn_started_ms = 0
        self._last_audio_ms = 0
        self._muted = False
        self._respond_waiter: threading.Event | None = None
        self._respond_started = threading.Event()
        self._closing = threading.Event()
        self._drainer: threading.Thread | None = None
        self.stats = {"state_changes": 0, "audio_chunks": 0, "turns": 0, "interrupts": 0}

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello) -> None:
        import soxr  # noqa: PLC0415

        self._resampler = soxr.ResampleStream(self.input_rate, MODEL_INPUT_RATE, 1, dtype="int16")
        if self.mock:
            log("lychee-fd sidecar running in mock mode; no backend is contacted")
            self.control.event(self.session_label, "session_start", mock=True)
            return
        self.session = LycheeSession(self.backend, start_payload={
            "start_speak_factor": self.start_speak_factor,
            "start_listen_factor": 1.2,
            "end_speak_factor": 1.0,
            "prompt_voice": self.voice_id,
            "tts_chunk_size": 1,
            "infer_window_ms": 400,
            "stage_timing_log": True,
            "control_prob_trace_log": True,
        }, on_event=self._on_backend_event, chunk_ms=self.chunk_ms, idle_fill_ms=self.idle_fill_ms)
        result = self.session.start()
        self.control.event(self.session_label, "session_start", backend_session=result.get("session_id"),
                           infer_window_ms=result.get("infer_window_ms"),
                           stage_timing_log_path=result.get("stage_timing_log_path"),
                           control_prob_trace_path=result.get("control_prob_trace_path"))
        self._drainer = threading.Thread(target=self._drain_loop, daemon=True)
        self._drainer.start()

    def on_close(self) -> None:
        self._closing.set()
        if self.session is not None:
            self.session.stop()
        self.control.event(self.session_label, "session_end", **self.stats,
                           upload_errors=self.session.upload_errors if self.session else 0,
                           filled_input_ms=round(self.session.filled_samples * 1000 / MODEL_INPUT_RATE)
                           if self.session else 0)

    # --- input --------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        samples = np.frombuffer(pcm16, dtype=np.int16)
        converted = self._resampler.resample_chunk(samples)
        if self.session is not None and converted.size:
            self.session.push(converted.astype(np.int16).tobytes())

    def on_commit(self) -> None:
        # The control stream owns the decision to answer; an engine endpoint is
        # recorded, not obeyed.
        self.control.event(self.session_label, "engine_commit_ignored", model_state=self._model_state)

    def on_text(self, text: str, role: str) -> None:
        # Not declared (no text_injection): the realtime API has no way to add
        # text to the model's context. Recorded so a run shows it was dropped.
        self.control.event(self.session_label, "text_dropped", role=role, chars=len(text))

    def on_respond(self) -> None:
        if self.mock:
            self._respond_mock()
            return
        # Lychee-FD cannot be ordered to speak. Wait for its own next turn and
        # let this request's boundary be that turn's end.
        waiter = threading.Event()
        with self._lock:
            self._respond_waiter = waiter
            self._respond_started.clear()
            already = self._turn_open
        self.control.event(self.session_label, "respond_requested", turn_open=already)
        if not already and not self._respond_started.wait(self.respond_wait):
            with self._lock:
                self._respond_waiter = None
            self.control.event(self.session_label, "respond_no_turn", waited_s=self.respond_wait)
            return
        while not waiter.wait(0.1):
            if self._closing.is_set() or self.interrupted():
                break
        with self._lock:
            self._respond_waiter = None

    # --- backend events -----------------------------------------------------

    def _on_backend_event(self, event: dict, recv_ms: int) -> None:
        kind = event.get("type")
        if kind == "state_change":
            self._on_state_change(event, recv_ms)
        elif kind == "assistant_text":
            delta = event.get("delta") or ""
            if delta and not self._muted:
                self._open_turn("text")
                with self._lock:
                    self._turn_text += delta
                self.text_delta(delta)
        elif kind == "audio_chunk_pcm":
            pcm = base64.b64decode(event.get("pcm_b64") or "")
            self.stats["audio_chunks"] += 1
            disposition = ("muted" if self._muted else "empty" if not pcm else
                           "engine_interrupt" if self.interrupted() and self._turn_open else "forward")
            self.control.event(self.session_label, "audio_delivery", round_id=event.get("round_id"),
                               server_emit_ms=event.get("server_audio_emit_epoch_ms"), recv_ms=recv_ms,
                               pcm_bytes=len(pcm), disposition=disposition,
                               model_state=self._model_state,
                               timing_basis="SSE receipt before sidecar write; not rendered playback")
            if self._muted or not pcm:
                return
            if self.interrupted() and self._turn_open:
                self._interrupt_turn("engine_interrupt")
                return
            first = not self._turn_open or self._turn_audio_samples == 0
            self._open_turn("audio")
            with self._lock:
                self._turn_audio_samples += len(pcm) // 2
                self._last_audio_ms = recv_ms
            if first:
                self.control.event(self.session_label, "first_audio", round_id=event.get("round_id"),
                                   server_emit_ms=event.get("server_audio_emit_epoch_ms"), recv_ms=recv_ms,
                                   after_turn_start_ms=recv_ms - self._turn_started_ms)
            self.audio(pcm)
        elif kind == "audio_interrupt":
            self.stats["interrupts"] += 1
            self.control.event(self.session_label, "model_audio_interrupt", reason=event.get("reason"),
                               source=event.get("source"),
                               server_ms=event.get("server_interrupt_epoch_ms"), recv_ms=recv_ms,
                               dropped_audio_events=event.get("dropped_audio_events"))
            self._finish_turn("model_interrupt")
        elif kind == "error":
            detail = event.get("error") or event.get("detail") or "backend error"
            self.error(f"lychee backend: {detail}", code="backend_error")
        elif kind == "done":
            self._finish_turn("backend_done")

    def _on_state_change(self, event: dict, recv_ms: int) -> None:
        source = event.get("source")
        old, new = str(event.get("from", "")).upper(), str(event.get("to", "")).upper()
        commit_ms = event.get("server_state_commit_epoch_ms")
        audio_ms = event.get("aligned_audio_ms", event.get("audio_ms"))
        fed_ms = self.session.wall_ms_of(float(audio_ms)) if (self.session and audio_ms is not None) else None
        self.stats["state_changes"] += 1
        self.control.event(
            self.session_label, "model_state", source=source, trace_source=event.get("trace_source"),
            from_state=old, to_state=new, reason=event.get("reason"), chunk=event.get("chunk"),
            round_id=event.get("round_id"), audio_ms=audio_ms, audio_fed_wall_ms=fed_ms,
            server_commit_ms=commit_ms, recv_ms=recv_ms,
            control_delay_ms=(int(commit_ms) - fed_ms) if (commit_ms and fed_ms) else None,
            interrupt=event.get("interrupt"), probs=event.get("probs") or event.get("control_probs"))
        # The early-exit head and the round trace both report a transition; the
        # first one to arrive moves the state, the second is a duplicate.
        with self._lock:
            if new == "S" and self._model_state != "S":
                self._model_state = "S"
                self._muted = False
                self._interrupted.clear()
                if self._turn_open:
                    self._finish_turn("new_turn")
            elif new == "L" and self._model_state != "L":
                self._model_state = "L"

    # --- turns --------------------------------------------------------------

    def _open_turn(self, cause: str) -> None:
        with self._lock:
            if self._turn_open:
                return
            self._turn_open = True
            self._turn_text = ""
            self._turn_audio_samples = 0
            self._turn_started_ms = _now_ms()
            self._last_audio_ms = self._turn_started_ms
            self.stats["turns"] += 1
            self._respond_started.set()
        self.control.event(self.session_label, "turn_start", cause=cause, model_state=self._model_state)

    def _finish_turn(self, reason: str) -> None:
        with self._lock:
            if not self._turn_open:
                return
            self._turn_open = False
            text = self._turn_text
            samples = self._turn_audio_samples
            waiter = self._respond_waiter
        if reason == "audio_stalled":
            self.error("Lychee output stalled before a model completion or yield",
                       code="audio_stalled", fatal=False)
        self.control.event(self.session_label, "turn_end", reason=reason, text=text,
                           audio_s=round(samples / MODEL_OUTPUT_RATE, 3),
                           wall_ms=_now_ms() - self._turn_started_ms)
        if text:
            self.text_done(text)
        if waiter is not None:
            waiter.set()  # the base class emits this turn's turn_done
        else:
            self.turn_done()

    def _interrupt_turn(self, reason: str) -> None:
        self._muted = True
        self._finish_turn(reason)

    def _drain_loop(self) -> None:
        """End a turn once the model has yielded and its audio has drained."""
        while not self._closing.wait(0.1):
            with self._lock:
                idle = _now_ms() - self._last_audio_ms
                ready = self._turn_open and self._model_state == "L" and idle >= self.drain_ms
                stalled = self._turn_open and idle >= 10 * self.drain_ms
            if ready:
                self._finish_turn("yielded")
            elif stalled:
                self._finish_turn("audio_stalled")

    # --- mock ---------------------------------------------------------------

    def _respond_mock(self) -> None:
        answer = "This is the Lychee-FD sidecar running without a model."
        self.text_delta(answer)
        self.text_done(answer)
        tone = 0.1 * np.sin(2 * np.pi * 220 * np.arange(MODEL_OUTPUT_RATE // 10) / MODEL_OUTPUT_RATE)
        frame = (tone * 32767).astype(np.int16).tobytes()
        for _ in range(4):
            if self.interrupted():
                return
            self.audio(frame)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--mock", action="store_true", help="speak the protocol without a backend")
    parser.add_argument("--backend", default=os.environ.get("LYCHEE_FD_BACKEND", DEFAULT_BACKEND),
                        help="Lychee-FD realtime backend (lychee_fd.app) base URL")
    parser.add_argument("--voice", default="default_female", help="upstream prompt voice id")
    parser.add_argument("--start-speak-factor", type=float, default=1.2,
                        help="upstream S-S logit factor (frontend 'Start factor', default 1.2)")
    parser.add_argument("--chunk-ms", type=int, default=200, help="upload chunk size (frontend test page: 200)")
    parser.add_argument("--drain-ms", type=int, default=800,
                        help="after the model yields, end the turn once no audio arrived for this long")
    parser.add_argument("--idle-fill-ms", type=int, default=400,
                        help="feed wall-clock silence once input lags this far behind (0 disables)")
    parser.add_argument("--respond-wait", type=float, default=8.0,
                        help="seconds a respond request waits for the model's own turn")
    parser.add_argument("--control-log", default=os.environ.get("LYCHEE_FD_CONTROL_LOG", ""),
                        help="append control decisions as JSONL here")
    arguments = parser.parse_args()
    protocol_in, protocol_out = sys.stdin.buffer, sys.stdout.buffer
    sys.stdout = sys.stderr
    LycheeSidecar(
        protocol_in, protocol_out, mock=arguments.mock, backend=arguments.backend,
        voice=arguments.voice, start_speak_factor=arguments.start_speak_factor,
        chunk_ms=arguments.chunk_ms, drain_ms=arguments.drain_ms,
        respond_wait=arguments.respond_wait, control=ControlLog(arguments.control_log or None),
        idle_fill_ms=arguments.idle_fill_ms,
    ).run()


if __name__ == "__main__":
    main()
