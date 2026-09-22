#!/usr/bin/env python3
"""Sidecar for MiniCPM-o 4.5 in its official full-duplex mode (plan cell N1).

The existing ``minicpm_o_sidecar.py`` drives the *streaming* interface: audio
is prefilled and the engine asks for a turn. This one runs the mode the model
was trained for and the official demo serves (``OpenBMB/MiniCPM-o-Demo``,
"Audio Full-Duplex"): a continuous listen/speak loop on a one-second clock.
Every second of microphone audio is prefilled as one unit; the model then
decides - for itself, every unit - whether to keep listening or to speak, and
while speaking it keeps hearing the next unit. There is no ``respond``: the
model owns its floor and its interaction policy.

The loop is the official one, called through the demo's own model code
(``MiniCPMO45/modeling_minicpmo_unified.py`` via ``core.processors.unified``)::

    prepare(system prompt, reference voice)            once per session
    per 1 s unit:  prefill(16 kHz audio) -> generate() -> emit -> finalize()

with the demo's defaults (sampling, temperature 0.7, top-k 20, top-p 0.8,
three forced-listen units at session start, 20 speak tokens per unit, the
English call preset's prompt and reference voice, length penalty 1.05).

What the engine sees:

* ``output_audio`` at the model's own 24 kHz, ``text_delta``/``text_done`` for
  the *assistant's* words, ``turn_done`` when the model ends its turn (an
  explicit end-of-turn, or a return to listening). The duplex mode reports no
  user transcript, so ``transcript`` is not declared;
* when the engine stops sending audio the sidecar keeps feeding silence at
  wall-clock rate: a model on a one-second clock that is not fed does not
  wait, it stops existing in time;
* ``interrupt`` maps to the demo's own Force-Listen control on the next unit,
  which closes the speaking turn inside the model (``<|turn_eos|>``) and resets
  its speech caches; audio still in flight for the current unit is dropped.

The model loads once. Served over TCP it is shared by consecutive engine
sessions (one at a time - the duplex state is a per-model singleton)::

    python sidecars/minicpm_o_duplex_sidecar.py --listen tcp:127.0.0.1:9145
    openrealtime serve -binding duplex -sidecar-address tcp:127.0.0.1:9145 ...

and ``--mock`` speaks the protocol without loading anything::

    openrealtime conformance sidecar -- python3 sidecars/minicpm_o_duplex_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import json
import math
import os
import queue
import socket
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log  # noqa: E402

REPOSITORY = Path(__file__).resolve().parents[1]
DEFAULT_DEMO = REPOSITORY / ".runtime/duplex-plan/src/MiniCPM-o-Demo"
DEFAULT_MODEL = os.path.expanduser(
    "~/.cache/huggingface/hub/models--openbmb--MiniCPM-o-4_5/snapshots/"
    "503e754207c94da6bb26850b4469f367c9ea3582")
#: The model hears 16 kHz in one-second units and speaks 24 kHz.
MODEL_INPUT_RATE = 16_000
MODEL_OUTPUT_RATE = 24_000
UNIT_SECONDS = 1.0
UNIT_SAMPLES = int(MODEL_INPUT_RATE * UNIT_SECONDS)
#: The official demo's "English Call" audio-duplex preset.
OFFICIAL_PROMPT = (
    "Replicate the tone and style from the input audio. Your task is to be a helpful "
    "assistant using this voice pattern. Please answer the user's questions seriously and "
    "in a high quality. Please chat with the user in a high naturalness style. You are in "
    "duplex mode, where you can listen and speak at the same time.")
OFFICIAL_REF_AUDIO = "assets/ref_audio/ref_en_dlc_1.wav"


class StreamResampler:
    """Stateful mono resampler: windowed-sinc anti-alias FIR plus interpolation."""

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
        drop = min(int(math.floor(next_position)), len(self.buffer) - 1)
        self.buffer = self.buffer[drop:]
        self.position = next_position - drop
        return out.astype(np.float32)


class ControlTap:
    """Act on ``interrupt`` the moment its header is read (see voicechat_sidecar)."""

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


class ModelHost:
    """The loaded model, shared by consecutive sessions, one at a time."""

    def __init__(self, *, model_path: str, demo_root: str, device: str, compile_model: bool,
                 attn: str, ref_audio: str, prompt: str, audio_only: bool) -> None:
        self.demo_root = str(Path(demo_root).resolve())
        if self.demo_root not in sys.path:
            sys.path.insert(0, self.demo_root)
        import torch  # noqa: PLC0415
        from core.processors.unified import UnifiedProcessor  # noqa: PLC0415
        from core.schemas.duplex import DuplexConfig  # noqa: PLC0415

        self.torch = torch
        self.DuplexConfig = DuplexConfig
        self.ref_audio = ref_audio if os.path.isabs(ref_audio) else os.path.join(self.demo_root, ref_audio)
        self.prompt = prompt
        self.model_path = model_path
        if audio_only:
            # Audio duplex never touches the vision tower (it runs only when a
            # unit carries frames), so it is not built: ~0.9 GB of VRAM on a
            # shared GPU. Vision is a separately labelled treatment.
            from MiniCPMO45.modeling_minicpmo_unified import MiniCPMO  # noqa: PLC0415

            original = MiniCPMO.from_pretrained.__func__

            def from_pretrained(cls, *args, **kwargs):
                kwargs.setdefault("init_vision", False)
                return original(cls, *args, **kwargs)

            MiniCPMO.from_pretrained = classmethod(from_pretrained)
        self.audio_only = audio_only
        free_before, total = torch.cuda.mem_get_info()
        started = time.monotonic()
        # The demo resolves its own relative asset paths from its root.
        previous = os.getcwd()
        os.chdir(self.demo_root)
        try:
            self.processor = UnifiedProcessor(
                model_path=model_path, ref_audio_path=self.ref_audio, device=device,
                compile=compile_model, attn_implementation=attn,
            )
        finally:
            os.chdir(previous)
        self.load_seconds = time.monotonic() - started
        free_after, _ = torch.cuda.mem_get_info()
        self.load_vram_gib = (free_before - free_after) / 2**30
        self.session_lock = threading.Lock()
        self.duplex = self.processor.set_duplex_mode()
        log(f"minicpm-o duplex model loaded in {self.load_seconds:.1f}s; device VRAM "
            f"delta {self.load_vram_gib:.2f} GiB; torch allocated "
            f"{torch.cuda.memory_allocated() / 2**30:.2f} GiB")


class MiniCPMODuplexSidecar(Sidecar):
    """MiniCPM-o 4.5 official full-duplex mode behind the sidecar protocol."""

    model_name = "openbmb/MiniCPM-o-4_5@503e7542 official duplex (MiniCPM-o-Demo@47709a92)"
    output_rate = MODEL_OUTPUT_RATE
    capabilities = (
        Capability.FULL_DUPLEX,
        Capability.NATIVE_INTERACTION,
        # The model owns its floor: it decides every unit whether to listen or
        # speak. It emits no user-activity events, so no speech_started/stopped
        # frames are produced; the declaration is about ownership.
        Capability.NATIVE_VAD,
        Capability.BARGE_IN,
    )

    def __init__(self, input_stream, output_stream, *, host: ModelHost | None, mock: bool,
                 idle_fill_ms: int, use_hello_instructions: bool, busy_timeout: float,
                 metrics_log: str | None, length_penalty: float, force_listen_count: int,
                 pace_output: bool = True, playout_lead_ms: int = 60) -> None:
        super().__init__(ControlTap(input_stream, self.on_interrupt), output_stream)
        self.host = host
        self.mock = mock
        self.idle_fill_seconds = max(0.0, idle_fill_ms / 1000.0)
        self.use_hello_instructions = use_hello_instructions
        self.busy_timeout = busy_timeout
        self.length_penalty = length_penalty
        self.force_listen_count = force_listen_count
        self._metrics = open(metrics_log, "a", encoding="utf-8") if metrics_log else None
        self._t0 = time.monotonic()
        self._resampler: StreamResampler | None = None
        self._buffer = np.zeros(0, dtype=np.float32)
        self._buffer_lock = threading.Condition()
        self._last_real_audio = time.monotonic()
        self._fill_cursor = time.monotonic()
        self._stop = threading.Event()
        self._threads: list[threading.Thread] = []
        self._holding_model = False
        self._force_listen_next = threading.Event()
        self._drop_unit = threading.Event()
        self._speaking = False
        self._spoken: list[str] = []
        self._turn_lock = threading.Condition()
        self._turns_done = 0
        self._units = 0
        # Output pacing (model path): a unit's second of speech is released at
        # the model's own clock in 80 ms packets instead of as one burst, so
        # "is the agent audible now" means the same thing as for a frame-clock
        # model, and an interrupt can drop the part not yet played.
        self._out: queue.Queue = queue.Queue()
        self._out_generation = 0
        self._out_lock = threading.RLock()
        self._pace = pace_output
        self._playout_lead = max(0.0, playout_lead_ms / 1000.0)
        self._real_samples = 0
        self._fill_samples = 0
        self._unit_seconds: list[float] = []

    # --- lifecycle ------------------------------------------------------------

    def configure(self, hello) -> None:
        self._resampler = StreamResampler(self.input_rate, MODEL_INPUT_RATE)
        log(f"minicpm-o duplex hello: rate={self.input_rate} instructions={self.instructions[:160]!r}")
        if not self.mock:
            if self.host is None:
                raise RuntimeError("no model host")
            if not self.host.session_lock.acquire(timeout=self.busy_timeout):
                raise RuntimeError("the MiniCPM-o duplex model is serving another session")
            self._holding_model = True
            try:
                self._prepare_session()
            except BaseException:
                # The base class does not call on_close after a failed
                # configure; a model lock held here would refuse every later
                # session.
                self._holding_model = False
                self.host.session_lock.release()
                raise
        self._last_real_audio = self._fill_cursor = time.monotonic()
        for target, name in ((self._fill_clock, "fill-clock"), (self._unit_loop, "unit-loop"),
                             (self._pacer, "output-pacer")):
            thread = threading.Thread(target=target, daemon=True, name=name)
            thread.start()
            self._threads.append(thread)

    def _prepare_session(self) -> None:
        prompt = self.host.prompt
        if self.use_hello_instructions and self.instructions.strip():
            prompt = f"{prompt} {self.instructions.strip()}"
        view = self.host.duplex
        view.config = self.host.DuplexConfig(
            length_penalty=self.length_penalty, force_listen_count=self.force_listen_count)
        model = view._model  # noqa: SLF001 - the demo's own view of the model
        if hasattr(model, "duplex") and model.duplex is not None:
            model.duplex.force_listen_count = self.force_listen_count
        started = time.monotonic()
        previous = os.getcwd()
        os.chdir(self.host.demo_root)
        try:
            view.prepare(system_prompt_text=prompt, ref_audio_path=self.host.ref_audio)
        finally:
            os.chdir(previous)
        log(f"minicpm-o duplex session prepared in {time.monotonic() - started:.2f}s")

    def on_close(self) -> None:
        self._stop.set()
        with self._buffer_lock:
            self._buffer_lock.notify_all()
        for thread in self._threads:
            thread.join(timeout=10)
        if self._holding_model:
            try:
                self.host.duplex.stop()
                self.host.duplex.cleanup()
                self.host.torch.cuda.empty_cache()
            except Exception as failure:  # noqa: BLE001
                log(f"minicpm-o duplex cleanup failed: {failure}")
            self.host.session_lock.release()
            self._holding_model = False
        if self._unit_seconds:
            costs = np.array(self._unit_seconds)
            log(f"minicpm-o duplex session closed: units={self._units} real={self._real_samples / MODEL_INPUT_RATE:.1f}s "
                f"fill={self._fill_samples / MODEL_INPUT_RATE:.1f}s unit_cost p50={np.median(costs):.3f}s "
                f"p95={np.percentile(costs, 95):.3f}s max={costs.max():.3f}s")
        if self._metrics is not None:
            self._metrics.close()

    # --- input ------------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        samples = np.frombuffer(pcm16, dtype=np.int16).astype(np.float32) / 32768.0
        converted = self._resampler.process(samples) if self._resampler else samples
        with self._buffer_lock:
            self._buffer = np.concatenate([self._buffer, converted])
            self._real_samples += converted.size
            now = time.monotonic()
            self._last_real_audio = now
            self._fill_cursor = now
            self._buffer_lock.notify_all()

    def _fill_clock(self) -> None:
        """Feed silence at wall-clock rate while the engine sends nothing."""
        while not self._stop.is_set():
            time.sleep(0.02)
            if self.idle_fill_seconds <= 0:
                continue
            with self._buffer_lock:
                now = time.monotonic()
                if now - self._last_real_audio < self.idle_fill_seconds:
                    continue
                missing = int((now - self._fill_cursor) * MODEL_INPUT_RATE)
                if missing <= 0:
                    continue
                self._buffer = np.concatenate([self._buffer, np.zeros(missing, dtype=np.float32)])
                self._fill_samples += missing
                self._fill_cursor += missing / MODEL_INPUT_RATE
                self._buffer_lock.notify_all()

    # --- the official duplex loop ---------------------------------------------------

    def _next_unit(self) -> np.ndarray | None:
        with self._buffer_lock:
            while self._buffer.size < UNIT_SAMPLES and not self._stop.is_set():
                self._buffer_lock.wait(timeout=0.1)
            if self._stop.is_set():
                return None
            unit = self._buffer[:UNIT_SAMPLES].copy()
            self._buffer = self._buffer[UNIT_SAMPLES:]
            return unit

    def _unit_loop(self) -> None:
        try:
            while not self._stop.is_set():
                unit = self._next_unit()
                if unit is None:
                    return
                if self.mock:
                    self._mock_unit(unit)
                else:
                    self._model_unit(unit)
        except Exception as failure:  # noqa: BLE001
            import traceback  # noqa: PLC0415

            log(traceback.format_exc())
            if not self._stop.is_set():
                self.error(f"minicpm-o duplex loop failed: {failure}", code="duplex_loop_failed", fatal=True)

    def _model_unit(self, unit: np.ndarray) -> None:
        view = self.host.duplex
        model = view._model  # noqa: SLF001
        config = view.config
        force_listen = self._force_listen_next.is_set()
        self._force_listen_next.clear()
        started = time.monotonic()
        view.prefill(audio_waveform=unit)
        prefilled = time.monotonic()
        result = model.duplex_generate(
            decode_mode=config.decode_mode, temperature=config.temperature, top_k=config.top_k,
            top_p=config.top_p, listen_prob_scale=config.listen_prob_scale,
            listen_top_k=config.listen_top_k, text_repetition_penalty=config.text_repetition_penalty,
            text_repetition_window_size=config.text_repetition_window_size,
            length_penalty=config.length_penalty, force_listen_override=force_listen,
        )
        generated = time.monotonic()
        dropped = force_listen or self._drop_unit.is_set()
        self._drop_unit.clear()
        if dropped:
            # An interrupted turn still ends: the engine waits for a boundary.
            if self._speaking:
                self._end_turn()
        else:
            self._emit(result)
        emitted = time.monotonic()
        view.finalize()
        finished = time.monotonic()
        self._units += 1
        cost = finished - started
        self._unit_seconds.append(cost)
        with self._buffer_lock:
            backlog = self._buffer.size / MODEL_INPUT_RATE
        waveform = result.get("audio_waveform")
        record = {
            "t": round(started - self._t0, 3), "unit": self._units, "listen": bool(result.get("is_listen", True)),
            "text": str(result.get("text") or ""), "end_of_turn": bool(result.get("end_of_turn")),
            "audio_s": round(len(waveform) / MODEL_OUTPUT_RATE, 3) if waveform is not None else 0.0,
            "prefill_s": round(prefilled - started, 4), "generate_s": round(generated - prefilled, 4),
            "emit_s": round(emitted - generated, 4), "finalize_s": round(finished - emitted, 4),
            "unit_cost_s": round(cost, 4), "rtf": round(cost / UNIT_SECONDS, 4),
            "backlog_s": round(backlog, 3), "force_listen": force_listen, "dropped": dropped,
            "out_queue": self._out.qsize(),
        }
        for key in ("cost_llm", "cost_tts_prep", "cost_tts", "cost_token2wav", "n_tokens", "n_tts_tokens"):
            value = result.get(key)
            if isinstance(value, (int, float)):
                record[key] = round(float(value), 4)
        if self._metrics is not None:
            self._metrics.write(json.dumps(record, ensure_ascii=False) + "\n")
            self._metrics.flush()

    def _emit(self, result: dict) -> None:
        if result.get("is_listen", True):
            if self._speaking:
                self._end_turn()
            return
        text = str(result.get("text") or "")
        if text:
            self._speaking = True
            self._spoken.append(text)
            self._queue_out("text", text)
        waveform = result.get("audio_waveform")
        if waveform is not None:
            if hasattr(waveform, "detach"):
                waveform = waveform.detach().float().cpu().numpy()
            waveform = np.asarray(waveform, dtype=np.float32).reshape(-1)
            if waveform.size:
                self._speaking = True
                self._queue_out("audio", (np.clip(waveform, -1.0, 1.0) * 32767.0).astype(np.int16).tobytes())
        if result.get("end_of_turn"):
            self._end_turn()

    def _end_turn(self) -> None:
        text = "".join(self._spoken)
        self._spoken = []
        self._speaking = False
        self._queue_out("end", text)

    def _queue_out(self, kind: str, value) -> None:
        if not self._pace or self.mock:
            self._deliver(kind, value)
            return
        self._out.put((self._out_generation, kind, value))

    def _deliver(self, kind: str, value) -> None:
        if kind == "text":
            self.text_delta(value)
        elif kind == "audio":
            self.audio(value)
        elif kind == "end":
            if value.strip():
                self.text_done(value)
            self.turn_done()
            with self._turn_lock:
                self._turns_done += 1
                self._turn_lock.notify_all()

    def _pacer(self) -> None:
        """Release queued speech at playback speed, a small lead ahead.

        The engine does not pace sidecar audio, so a unit's whole second of
        speech would otherwise arrive at once: nothing could be taken back when
        the model stops, and "was the agent audible then" would be a question
        about arrival bursts rather than about speech. This is the
        micro-turn sidecar's playout discipline (``_playout_loop``) applied to
        the duplex unit clock.
        """
        packet_bytes = int(MODEL_OUTPUT_RATE * 0.08) * 2
        next_at: float | None = None
        while not self._stop.is_set():
            try:
                generation, kind, value = self._out.get(timeout=0.05)
            except queue.Empty:
                continue
            if kind != "audio":
                self._deliver(kind, value)
                continue
            for offset in range(0, len(value), packet_bytes):
                if generation != self._out_generation or self._stop.is_set():
                    break  # interrupted: the unplayed rest is dropped
                now = time.monotonic()
                if next_at is None or next_at < now:
                    next_at = now
                wait = next_at - self._playout_lead - now
                if wait > 0:
                    time.sleep(wait)
                packet = value[offset:offset + packet_bytes]
                # Interrupt can arrive during the pacing sleep. Serialize the
                # final check and write with interrupt acknowledgement so no
                # old packet is sent after on_interrupt returns.
                with self._out_lock:
                    if generation != self._out_generation or self._stop.is_set():
                        break
                    self.audio(packet)
                next_at += len(packet) / 2 / MODEL_OUTPUT_RATE

    # --- engine controls ----------------------------------------------------------

    def on_interrupt(self) -> None:
        """Force the model to listen on its next unit and drop unplayed audio."""
        self._force_listen_next.set()
        self._drop_unit.set()
        with self._out_lock:
            self._out_generation += 1

    def on_text(self, text: str, role: str) -> None:
        log(f"minicpm-o duplex ignored injected {role} text ({len(text)} chars): the duplex mode has no text seam")

    def on_respond(self) -> None:
        """An explicit turn request: advisory for a model that owns its floor."""
        if self.mock:
            self._mock_speak("This is the MiniCPM-o duplex sidecar running without a model.")
            return
        with self._turn_lock:
            target = self._turns_done + 1
            deadline = time.monotonic() + 20.0
            self.send("log", text="minicpm-o duplex owns its floor; respond waits for its next turn")
            while self._turns_done < target and not self.interrupted() and not self._stop.is_set():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                self._turn_lock.wait(timeout=min(remaining, 0.5))

    # --- mock -------------------------------------------------------------------

    def _mock_unit(self, unit: np.ndarray) -> None:
        energy = float(np.sqrt(np.mean(np.square(unit)))) if unit.size else 0.0
        state = getattr(self, "_mock_state", "idle")
        if energy > 0.01:
            self._mock_state = "heard"
            if self._speaking:
                self._end_turn()
        elif state == "heard":
            self._mock_state = "idle"
            self._mock_speak("Mock duplex answer.")

    def _mock_speak(self, text: str) -> None:
        self._speaking = True
        self._spoken.append(text)
        self.text_delta(text)
        tone = 0.05 * np.sin(2 * np.pi * 200 * np.arange(MODEL_OUTPUT_RATE // 2) / MODEL_OUTPUT_RATE)
        for _ in range(2):
            if self.interrupted() or self._drop_unit.is_set():
                self._drop_unit.clear()
                break
            self.audio((tone * 32767).astype(np.int16).tobytes())
        self._end_turn()


def serve_stdio(arguments, host: ModelHost | None) -> None:
    sidecar = MiniCPMODuplexSidecar(sys.stdin.buffer, sys.stdout.buffer, host=host, **session_options(arguments))
    sidecar.run()


def session_options(arguments) -> dict:
    return dict(
        mock=arguments.mock, idle_fill_ms=arguments.idle_fill_ms,
        use_hello_instructions=arguments.use_hello_instructions, busy_timeout=arguments.busy_timeout,
        metrics_log=arguments.metrics_log, length_penalty=arguments.length_penalty,
        force_listen_count=arguments.force_listen_count, pace_output=not arguments.no_pace_output,
        playout_lead_ms=arguments.playout_lead_ms,
    )


def serve_tcp(arguments, host: ModelHost | None) -> None:
    address = arguments.listen.removeprefix("tcp:")
    hostname, _, port = address.rpartition(":")
    server = socket.create_server((hostname or "127.0.0.1", int(port)), reuse_port=False)
    log(f"minicpm-o duplex sidecar listening on tcp:{hostname}:{port}")

    def serve(connection: socket.socket) -> None:
        connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        reader = connection.makefile("rb", buffering=1 << 16)
        writer = connection.makefile("wb", buffering=0)
        try:
            MiniCPMODuplexSidecar(reader, writer, host=host, **session_options(arguments)).run()
        except Exception as failure:  # noqa: BLE001
            log(f"session failed: {failure}")
        finally:
            for closable in (reader, writer, connection):
                try:
                    closable.close()
                except Exception:  # noqa: BLE001
                    pass

    while True:
        connection, _ = server.accept()
        threading.Thread(target=serve, args=(connection,), daemon=True).start()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--model", default=DEFAULT_MODEL, help="MiniCPM-o 4.5 checkpoint directory")
    parser.add_argument("--demo", default=str(DEFAULT_DEMO), help="OpenBMB/MiniCPM-o-Demo checkout")
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--attn", default="auto", help="auto / flash_attention_2 / sdpa (the demo's option)")
    parser.add_argument("--compile", action="store_true", help="the demo's torch.compile option")
    parser.add_argument("--with-vision", action="store_true",
                        help="also build the vision tower (the audio-only duplex path never uses it)")
    parser.add_argument("--ref-audio", default=OFFICIAL_REF_AUDIO, help="reference voice (demo-relative or absolute)")
    parser.add_argument("--prompt", default=OFFICIAL_PROMPT, help="duplex system prompt (default: official English call preset)")
    parser.add_argument("--use-hello-instructions", action="store_true",
                        help="append the engine hello's instructions to the duplex prompt")
    parser.add_argument("--length-penalty", type=float, default=1.05, help="the demo UI default")
    parser.add_argument("--force-listen-count", type=int, default=3, help="forced-listen units at session start")
    parser.add_argument("--idle-fill-ms", type=int, default=300,
                        help="feed silence after this long without engine audio; 0 disables")
    parser.add_argument("--busy-timeout", type=float, default=60.0,
                        help="seconds a new session waits for the model to be released")
    parser.add_argument("--metrics-log", default=None, help="append one JSON line per model unit here")
    parser.add_argument("--playout-lead-ms", type=int, default=60,
                        help="how far ahead of playback time paced audio is handed to the engine")
    parser.add_argument("--no-pace-output", action="store_true",
                        help="emit each unit's speech as one burst instead of 80 ms packets at the model clock")
    parser.add_argument("--listen", default=None, help="serve sessions on tcp:HOST:PORT with one loaded model")
    parser.add_argument("--mock", action="store_true",
                        help="speak the protocol without loading a model, for plumbing and conformance")
    arguments = parser.parse_args()
    host = None
    if not arguments.mock:
        host = ModelHost(model_path=arguments.model, demo_root=arguments.demo, device=arguments.device,
                         compile_model=arguments.compile, attn=arguments.attn,
                         ref_audio=arguments.ref_audio, prompt=arguments.prompt,
                         audio_only=not arguments.with_vision)
    if arguments.listen:
        serve_tcp(arguments, host)
    else:
        serve_stdio(arguments, host)


if __name__ == "__main__":
    main()
