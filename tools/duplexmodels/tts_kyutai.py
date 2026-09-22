#!/usr/bin/env python3
"""Kyutai delayed-streams TTS behind the duplex-plan synthesis contract.

Serves ``kyutai/tts-1.6b-en_fr`` (English and French) through
:func:`common.serve_synthesizer`: OpenAI ``/v1/audio/speech`` for complete
text and the ``openrealtime-incremental-speech/1`` WebSocket for text that is
still arriving.

**Incremental text is real.** Delayed-streams TTS co-generates a time-aligned
text stream with the audio: at every 80 ms step the model either pads or asks
for the next word, and a state machine feeds it that word's tokens. Words are
therefore consumed *during* generation, and this service appends them to the
running generation's word queue as ``text.append`` messages arrive - it never
restarts or re-synthesises. Two properties of the model bound how early audio
can start (both are the model's, not the service's):

* The model reads each word together with a *lookahead* stream carrying the
  word two positions ahead (``second_stream_ahead = 2``), so a generation only
  steps while more than two words are queued; with fewer it pauses, keeping
  its state, until text arrives, a ``text.flush`` or ``text.end``.
* Audio is delayed 1.28 s (16 frames) behind the text stream, so the first
  audio frame needs ~16 steps of text consumption (at generation speed, not
  wall-clock speed).

Text arrives as arbitrary fragments (LLM tokens); a fragment is split into
words at whitespace and a trailing partial word is held until whitespace,
``text.flush`` or ``text.end`` completes it. The declared capabilities are
``incremental_text=True``, ``input_granularity="token"`` (the model conditions
on a growing prefix within one generation; the unit it consumes is a word) and
``nonterminal_flush=True``: a flush lets the generation consume every buffered
word, speak it and pause, and later text continues the same generation (the
model has then seen the end of its text once, so prosody may treat the flush
as a sentence end).

Concurrency: one worker thread owns the model and a batched streaming state of
``--rows`` rows. Every context (incremental or complete-text) takes one row with
its own voice conditioning, word queue and Mimi decoder state; each step runs
the rows that can advance (``exec_mask``) and leaves waiting rows untouched, so
a context waiting for text never blocks another, and nothing holds a lock
across a wait. ``context.cancel`` stops that row's audio at the next step.

Output: PCM at 24 kHz, 1920 samples (80 ms) per chunk, emitted as each frame is
decoded (generation runs faster than real time; the client paces playback).

Voices: a path in ``kyutai/tts-voices`` (e.g. ``expresso/ex03-ex01_happy_001_channel1_334s.wav``
or ``vctk/p225_023.wav``), its basename without extension, or ``default``.

Serve::

    python tools/duplexmodels/tts_kyutai.py --port 9125

Prove incrementality against a running server, and measure TTFA/RTF::

    python tools/duplexmodels/tts_kyutai.py --probe ws://127.0.0.1:9125 --probe-out probe.json
    python tools/duplexmodels/tts_kyutai.py --bench ws://127.0.0.1:9125 --bench-out bench.json
"""

from __future__ import annotations

import argparse
import json
import logging
import queue
import sys
import threading
import time
import traceback
from collections import deque
from pathlib import Path
from typing import Iterator, Optional

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import SynthesisCapabilities, Synthesizer, configure_logging, serve_synthesizer  # noqa: E402

log = logging.getLogger("tts_kyutai")

DEFAULT_REPOSITORY = "kyutai/tts-1.6b-en_fr"
DEFAULT_VOICE_REPOSITORY = "kyutai/tts-voices"
DEFAULT_VOICE = "expresso/ex03-ex01_happy_001_channel1_334s.wav"
#: CFG strength baked into the conditioning (the model is CFG-distilled).
CFG_COEF = 2.0


class _NoLock:
    """The scaffold wraps complete-text synthesis in ``synthesizer.lock``.

    Here the worker thread owns the model and interleaves rows, so complete-text
    requests need no serialisation of their own.
    """

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def acquire(self, *args, **kwargs):
        return True

    def release(self):
        pass


class Row:
    """One synthesis context occupying one row of the batched state."""

    def __init__(self, index: int, voice: str) -> None:
        self.index = index
        self.voice = voice
        #: Internal: set when the caller cancelled or went away. The scaffold's
        #: own cancel event is separate - it decides between ``audio.done``
        #: and ``context.cancelled`` and must only mean a real cancel.
        self.cancel = threading.Event()
        self.out: "queue.Queue[Optional[np.ndarray]]" = queue.Queue()
        self.state = None  # moshi.models.tts.State
        self.steps = 0
        self.partial = ""  # trailing fragment not yet known to be a whole word
        #: Whole words received but not yet admitted into the generation. Only
        #: the worker touches ``state``; feeders hand words over through here.
        self.incoming: list[str] = []
        self.flush_requested = False
        self.end_requested = False
        self.first_words = True
        self.flushing = False
        self.ending = False
        self.done = False
        self.words_in = 0
        self.frames_out = 0
        self.opened = time.perf_counter()
        self.first_audio: float | None = None


class KyutaiTTS(Synthesizer):
    capabilities = SynthesisCapabilities(incremental_text=True, nonterminal_flush=True, input_granularity="token")

    def __init__(self, repository: str, voice_repository: str, device: str, rows: int, n_q: int,
                 temperature: float) -> None:
        import torch  # noqa: PLC0415
        from moshi.models import LMGen, loaders  # noqa: PLC0415
        from moshi.models.tts import TTSModel, script_to_entries  # noqa: PLC0415
        from moshi.modules.transformer import StreamingMultiheadAttention  # noqa: PLC0415

        self.torch = torch
        self.script_to_entries = script_to_entries
        self.model = repository
        self.device = device
        self.lock = _NoLock()
        info = loaders.CheckpointInfo.from_hf_repo(repository)
        self.tts = TTSModel.from_checkpoint_info(info, voice_repo=voice_repository, n_q=n_q, temp=temperature,
                                                 device=device)
        tts = self.tts
        if not tts.valid_cfg_conditionings or not tts.multi_speaker:
            raise RuntimeError("this service expects a CFG-distilled multi-speaker DSM TTS checkpoint")
        self.lm = tts.lm
        self.mimi = tts.mimi
        self.sample_rate = int(self.mimi.sample_rate)
        self.frame_rate = float(self.mimi.frame_rate)
        self.frame_size = int(self.mimi.frame_size)
        self.machine = tts.machine
        self.token_ids = self.machine.token_ids
        self.lookahead = int(self.machine.second_stream_ahead)
        self.delay_steps = int(tts.delay_steps)
        self.final_padding = int(tts.final_padding)
        self.rows_count = rows
        self.max_steps = int(tts.max_gen_length)
        self.lm.dep_q = tts.n_q
        self.audio_offset = self.lm.audio_offset
        self.delays = list(self.lm.delays)
        self.max_delay = max(self.delays)
        # Per codebook: the row step before which that codebook must stay "zero".
        self.audio_start = torch.tensor(
            [self.delays[q + self.audio_offset] + self.delay_steps for q in range(self.lm.dep_q)],
            device=device, dtype=torch.long)
        self.voice_root = Path(tts.get_voice_path(DEFAULT_VOICE)).parents[1]
        self.voices = self._index_voices()
        self.cross_cache: dict[str, object] = {}
        self.cross_modules = [module for module in self.lm.modules()
                              if isinstance(module, StreamingMultiheadAttention) and module.cross_attention]

        default_attributes = self._attributes(self.resolve_voice("default"))
        condition_tensors = self.lm.condition_provider.prepare_and_provide([default_attributes] * rows)
        self.row_offsets = torch.zeros(rows, dtype=torch.long, device=device)
        self.rows: list[Optional[Row]] = [None] * rows
        self.lm_gen = LMGen(self.lm, temp=temperature, temp_text=temperature, cfg_coef=1.0,
                            condition_tensors=condition_tensors, on_text_hook=self._on_text,
                            on_audio_hook=self._on_audio, support_out_of_sync=True)
        self.no_depformer = torch.full((rows, self.lm.dep_q, 1), self.token_ids.zero, dtype=torch.long, device=device)
        self.no_input = torch.full((rows, self.lm.n_q - self.lm.dep_q, 1), self.token_ids.zero,
                                   dtype=torch.long, device=device)
        self.wake = threading.Condition()
        self.pending_open: deque[Row] = deque()
        self.stats = {"steps": 0, "step_ms": deque(maxlen=4000), "rows_per_step": deque(maxlen=4000),
                      "contexts": 0, "cancelled": 0}
        with torch.no_grad():
            self.lm_gen.streaming_forever(rows)
            self.mimi.streaming_forever(rows)
            self._warmup()
        self.worker_error: str | None = None
        self.worker = threading.Thread(target=self._run, name="kyutai-tts-worker", daemon=True)
        self.worker.start()
        log.info("loaded %s: %d rows, n_q=%d, delay %d steps, lookahead %d words, %d voices",
                 repository, rows, n_q, self.delay_steps, self.lookahead, len(self.voices))

    # -- voices ------------------------------------------------------------

    def _index_voices(self) -> dict[str, Path]:
        suffix = self.tts.voice_suffix
        voices: dict[str, Path] = {}
        for path in sorted(self.voice_root.rglob("*" + suffix)):
            relative = str(path.relative_to(self.voice_root))[: -len(suffix)]
            voices[relative] = path
            stem = Path(relative).name.rsplit(".", 1)[0]
            voices.setdefault(stem, path)
        voices["default"] = voices[DEFAULT_VOICE]
        return voices

    def resolve_voice(self, voice: str) -> Path:
        voice = (voice or "default").strip()
        if voice in self.voices:
            return self.voices[voice]
        # OpenAI voice names (alloy, ...) and unknown names use the default.
        return self.voices["default"]

    def _attributes(self, voice_path: Path):
        return self.tts.make_condition_attributes([voice_path], cfg_coef=CFG_COEF)

    def _set_row_voice(self, index: int, voice_path: Path) -> None:
        key = str(voice_path)
        cross = self.cross_cache.get(key)
        if cross is None:
            tensors = self.lm.condition_provider.prepare_and_provide([self._attributes(voice_path)])
            cross = self.lm.fuser.get_cross(tensors).to(self.lm.dtype)
            self.cross_cache[key] = cross
        state = self.lm_gen._streaming_state
        state.condition_cross[index].copy_(cross[0])
        for module in self.cross_modules:
            module_state = module._streaming_state
            if module_state is not None and module_state.k_cross is not None:
                k, v = module._compute_cross_attention(cross, cross)
                module_state.k_cross[index].copy_(k[0])
                module_state.v_cross[index].copy_(v[0])

    # -- model hooks (run inside LMGen.step on the worker thread) ------------

    def _on_text(self, text_tokens) -> None:
        executing = self.executing_rows
        if not executing:
            return
        values = text_tokens.tolist()
        for row in executing:
            out, _ = self.machine.process(row.steps, row.state, values[row.index])
            values[row.index] = out
        text_tokens[:] = self.torch.tensor(values, dtype=self.torch.long, device=text_tokens.device)

    def _on_audio(self, audio_tokens) -> None:
        # audio_tokens [rows, dep_q]: codebook q of a row stays "zero" until
        # that row has taken delays[q] + delay_steps steps.
        silent = self.row_offsets[:, None] < self.audio_start[None, :]
        audio_tokens[:] = self.torch.where(silent, self.torch.full_like(audio_tokens, self.token_ids.zero),
                                           audio_tokens)

    # -- scheduling ------------------------------------------------------------

    def _warmup(self) -> None:
        torch = self.torch
        everyone = torch.ones(self.rows_count, dtype=torch.bool, device=self.device)
        self.executing_rows: list[Row] = []
        self.lm_gen.set_exec_mask(everyone)
        for _ in range(self.max_delay + 3):
            frame = self.lm_gen.step(self.no_input)
        codes = torch.zeros(self.rows_count, self.lm.dep_q, 1, dtype=torch.long, device=self.device)
        self.mimi.set_exec_mask(everyone)
        for _ in range(3):
            self.mimi.decode(codes)
        self.lm_gen.reset_streaming(everyone)
        self.mimi.reset_streaming(everyone)
        del frame

    def open(self, voice: str) -> Row:
        row = Row(-1, voice)
        with self.wake:
            self.pending_open.append(row)
            self.wake.notify_all()
        return row

    def append_text(self, row: Row, text: str) -> None:
        """Take a text fragment; whole words go to the generation, a trailing partial word waits."""
        with self.wake:
            row.partial += text
            words = row.partial.split()
            if row.partial[-1:].isspace() or not words:
                ready, row.partial = words, ""
            else:
                ready, row.partial = words[:-1], words[-1]
            if ready:
                row.incoming.extend(ready)
                self.wake.notify_all()

    def flush(self, row: Row, final: bool) -> None:
        with self.wake:
            if row.partial.strip():
                row.incoming.extend(row.partial.split())
            row.partial = ""
            if final:
                row.end_requested = True
            else:
                row.flush_requested = True
            self.wake.notify_all()

    def _admit(self, row: Row) -> None:
        """Move a row's received words into its running generation (worker thread, under ``wake``)."""
        if row.incoming:
            text = " ".join(row.incoming)
            row.incoming = []
            entries = self.script_to_entries(self.tts.tokenizer, self.token_ids, self.frame_rate, [text],
                                             multi_speaker=row.first_words and self.tts.multi_speaker,
                                             padding_between=1)
            row.first_words = False
            row.words_in += len(entries)
            row.state.entries.extend(entries)
            if row.state.end_step is not None and not row.ending:
                # Text after a flush continues the same generation.
                row.state.end_step = None
        if row.flush_requested:
            row.flush_requested = False
            row.flushing = True
        if row.end_requested:
            row.ending = True

    def _activate(self, row: Row) -> bool:
        torch = self.torch
        free = [index for index, occupant in enumerate(self.rows) if occupant is None]
        if not free:
            return False
        index = free[0]
        row.index = index
        row.state = self.machine.new_state([])
        self.rows[index] = row
        mask = torch.zeros(self.rows_count, dtype=torch.bool, device=self.device)
        mask[index] = True
        self.lm_gen.reset_streaming(mask)
        self.mimi.reset_streaming(mask)
        self.row_offsets[index] = 0
        self._set_row_voice(index, self.resolve_voice(row.voice))
        self.stats["contexts"] += 1
        return True

    def _can_step(self, row: Row) -> bool:
        state = row.state
        if row.ending or row.flushing:
            if state.end_step is None:
                return True
            if row.steps < state.end_step + self.delay_steps + self.final_padding:
                return True
            if row.flushing and not row.ending:
                row.flushing = False  # drained: pause until more text
            return False
        if row.steps >= self.max_steps:
            log.warning("row %d reached %d steps without finishing; ending it", row.index, row.steps)
            row.ending = True
            state.end_step = state.end_step if state.end_step is not None else row.steps
            return False
        return len(state.entries) > self.lookahead

    def _finish(self, row: Row) -> None:
        row.done = True
        row.out.put(None)
        self.rows[row.index] = None

    def _run(self) -> None:
        torch = self.torch
        try:
            with torch.no_grad():
                self._loop()
        except BaseException:  # noqa: BLE001
            self.worker_error = traceback.format_exc()
            log.exception("TTS worker died")
            raise

    def _loop(self) -> None:
        while True:
            with self.wake:
                while True:
                    while self.pending_open:
                        row = self.pending_open[0]
                        try:
                            if not self._activate(row):
                                break
                        except Exception:  # noqa: BLE001
                            log.exception("could not open a synthesis row")
                            self.pending_open.popleft()
                            if row.index >= 0 and self.rows[row.index] is row:
                                self.rows[row.index] = None
                            row.done = True
                            row.out.put(None)
                            continue
                        self.pending_open.popleft()
                    for row in self.rows:
                        if row is None:
                            continue
                        self._admit(row)
                        if row.cancel.is_set():
                            self.stats["cancelled"] += 1
                            self._finish(row)
                        elif row.ending and not self._can_step(row):
                            self._finish(row)
                    ready = [row for row in self.rows if row is not None and self._can_step(row)]
                    if ready:
                        break
                    self.wake.wait(timeout=0.5)
            try:
                self._step(ready)
            except Exception:  # noqa: BLE001
                log.exception("generation step failed")
                with self.wake:
                    for row in ready:
                        row.cancel.set()

    def _step(self, ready: list[Row]) -> None:
        torch = self.torch
        began = time.perf_counter()
        mask = torch.zeros(self.rows_count, dtype=torch.bool)
        offsets = torch.zeros(self.rows_count, dtype=torch.long)
        for row in ready:
            mask[row.index] = True
            offsets[row.index] = row.steps
        mask = mask.to(self.device)
        self.row_offsets.copy_(offsets)
        self.executing_rows = ready
        self.lm_gen.set_exec_mask(mask)
        replace = self.no_depformer if all(row.steps < self.delay_steps for row in ready) else None
        frame = self.lm_gen.step(self.no_input, depformer_replace_tokens=replace)
        self.executing_rows = []
        decodable = []
        for row in ready:
            row.steps += 1
        if frame is not None:
            audio = frame[:, 1:, :]
            valid = (audio >= 0).all(dim=2).all(dim=1)
            valid_cpu = valid.tolist()
            for row in ready:
                index = row.index
                k = row.steps - 1 - self.max_delay  # frame index of this row's output
                if k < 0 or not valid_cpu[index]:
                    continue
                decodable.append((row, k))
            if decodable:
                decode_mask = torch.zeros(self.rows_count, dtype=torch.bool)
                for row, _ in decodable:
                    decode_mask[row.index] = True
                decode_mask = decode_mask.to(self.device)
                codes = torch.where(decode_mask[:, None, None], audio, torch.zeros_like(audio))
                self.mimi.set_exec_mask(decode_mask)
                pcm = self.mimi.decode(codes)
                pcm = torch.clamp(pcm, -1, 1)[:, 0].float().cpu().numpy()
                for row, k in decodable:
                    end_step = row.state.end_step
                    # Same trimming as TTSModel.simple_generate: drop the first
                    # two decoded frames and stop at end_step.
                    if k < self.delay_steps + 2:
                        continue
                    if (row.ending or row.flushing) and end_step is not None and k >= end_step + self.delay_steps + 2:
                        continue
                    if row.cancel.is_set():
                        continue
                    if row.first_audio is None:
                        row.first_audio = time.perf_counter()
                    row.frames_out += 1
                    row.out.put(pcm[row.index].copy())
        self.stats["steps"] += 1
        self.stats["step_ms"].append((time.perf_counter() - began) * 1000)
        self.stats["rows_per_step"].append(len(ready))

    # -- Synthesizer API ----------------------------------------------------------

    def _stop(self, row: Row) -> None:
        if not row.done:
            row.cancel.set()
            with self.wake:
                self.wake.notify_all()

    def _drain(self, row: Row, cancel: threading.Event) -> Iterator[np.ndarray]:
        while True:
            if cancel.is_set():
                self._stop(row)
                return
            try:
                chunk = row.out.get(timeout=0.02)
            except queue.Empty:
                if not self.worker.is_alive():
                    # Without this a response would wait forever on a dead
                    # worker (and hold the server's graceful shutdown open).
                    raise RuntimeError(f"TTS worker is not running: {self.worker_error}")
                continue
            if chunk is None:
                return
            if cancel.is_set():
                self._stop(row)
                return
            yield chunk

    def synthesize(self, text: str, voice: str, cancel: threading.Event) -> Iterator[np.ndarray]:
        row = self.open(voice)
        self.append_text(row, text)
        self.flush(row, final=True)
        try:
            yield from self._drain(row, cancel)
        finally:
            # A client that disconnects closes this generator early.
            self._stop(row)

    def synthesize_incremental(self, texts: "queue.Queue[Optional[str]]", voice: str,
                               cancel: threading.Event) -> Iterator[np.ndarray]:
        row = self.open(voice)

        def feed() -> None:
            while not cancel.is_set():
                item = texts.get()
                if item is None:
                    self.flush(row, final=True)
                    return
                if item == "\x00flush":
                    self.flush(row, final=False)
                    continue
                self.append_text(row, item)

        threading.Thread(target=feed, name="kyutai-tts-feed", daemon=True).start()
        try:
            yield from self._drain(row, cancel)
        finally:
            self._stop(row)

    def health(self) -> dict:
        steps = sorted(self.stats["step_ms"])
        rows = list(self.stats["rows_per_step"])
        p50 = steps[len(steps) // 2] if steps else None
        frame_ms = 1000 / self.frame_rate
        return {
            "worker_alive": self.worker.is_alive(),
            "worker_error": self.worker_error,
            "languages": ["en", "fr"],
            "text_unit": "word",
            "lookahead_words": self.lookahead,
            "audio_delay_s": self.delay_steps / self.frame_rate,
            "frame_ms": frame_ms,
            "rows": self.rows_count,
            "rows_busy": sum(1 for row in self.rows if row is not None),
            "steps": self.stats["steps"],
            "step_ms_p50": p50,
            "step_ms_p95": steps[int(len(steps) * 0.95)] if steps else None,
            "rows_per_step_mean": (sum(rows) / len(rows)) if rows else None,
            "rtf_per_step_p50": (p50 / frame_ms) if p50 else None,
            "contexts_total": self.stats["contexts"],
            "cancelled": self.stats["cancelled"],
            "voices": len(self.voices),
            "default_voice": DEFAULT_VOICE,
        }


# ------------------------------------------------------------------------------
# Probe: prove incrementality and cancellation against a running server.


def probe(url: str, voice: str, out: str, wait: float = 3.0) -> None:
    import asyncio  # noqa: PLC0415
    import base64  # noqa: PLC0415

    import websockets  # noqa: PLC0415

    first_half = "The weather in Paris this morning is bright and cool, and the river"
    second_half = " is calm under a clear blue sky, so we will walk along the quay before lunch."
    cancel_text = ("This sentence is deliberately long, so that the listener can hear the synthesiser "
                   "keep talking for a good while before somebody interrupts it in the middle of a word.")

    async def run() -> dict:
        report: dict = {"url": url, "voice": voice}
        async with websockets.connect(url.rstrip("/") + "/v1/tts/stream", max_size=None) as ws:
            # 1. Held-back suffix: audio must arrive before the second half is sent.
            events: list[dict] = []
            began = time.perf_counter()

            def stamp(kind: str, **extra) -> None:
                events.append({"t": round(time.perf_counter() - began, 4), "type": kind, **extra})

            await ws.send(json.dumps({"type": "context.open", "context_id": "a", "voice": voice}))
            ready = json.loads(await ws.recv())
            report["capabilities"] = ready.get("capabilities")
            report["sample_rate"] = ready.get("sample_rate")
            sent_first = time.perf_counter()
            await ws.send(json.dumps({"type": "text.append", "context_id": "a", "text": first_half}))
            stamp("text.append", part="first", chars=len(first_half))

            samples_before_second = 0
            pcm = bytearray()
            deadline = time.perf_counter() + wait
            while True:
                timeout = deadline - time.perf_counter()
                if timeout <= 0:
                    break
                try:
                    message = json.loads(await asyncio.wait_for(ws.recv(), timeout))
                except asyncio.TimeoutError:
                    break
                if message["type"] == "audio":
                    chunk = base64.b64decode(message["pcm16"])
                    pcm.extend(chunk)
                    samples = len(chunk) // 2
                    samples_before_second += samples
                    stamp("audio", seq=message["seq"], samples=samples)
                else:
                    stamp(message["type"])
            await ws.send(json.dumps({"type": "text.append", "context_id": "a", "text": second_half}))
            stamp("text.append", part="second", chars=len(second_half))
            await ws.send(json.dumps({"type": "text.end", "context_id": "a"}))
            stamp("text.end")
            total = samples_before_second
            while True:
                message = json.loads(await ws.recv())
                if message["type"] == "audio":
                    chunk = base64.b64decode(message["pcm16"])
                    pcm.extend(chunk)
                    total += len(chunk) // 2
                    stamp("audio", seq=message["seq"], samples=len(chunk) // 2)
                else:
                    stamp(message["type"])
                if message["type"] in ("audio.done", "error", "context.cancelled"):
                    break
            audio_events = [event for event in events if event["type"] == "audio"]
            first_audio = audio_events[0]["t"] if audio_events else None
            report["held_back_suffix"] = {
                "first_half": first_half,
                "second_half": second_half,
                "wait_before_second_half_s": wait,
                "first_audio_s_after_first_text": (first_audio - (sent_first - began)) if first_audio is not None else None,
                "audio_s_received_before_second_half": samples_before_second / 24_000,
                "audio_s_total": total / 24_000,
                "audio_before_second_half": samples_before_second > 0,
                "events": events[:12] + [{"...": len(events) - 24}] + events[-12:] if len(events) > 24 else events,
            }

            # 2. Cancel: audio must stop after context.cancel.
            events = []
            began = time.perf_counter()
            await ws.send(json.dumps({"type": "context.open", "context_id": "b", "voice": voice}))
            await ws.send(json.dumps({"type": "text.append", "context_id": "b", "text": cancel_text}))
            await ws.send(json.dumps({"type": "text.end", "context_id": "b"}))
            audio_before = 0
            cancelled_at = None
            audio_after = 0
            terminal = None
            while True:
                try:
                    message = json.loads(await asyncio.wait_for(ws.recv(), 10))
                except asyncio.TimeoutError:
                    terminal = "timeout"
                    break
                if message["type"] == "audio":
                    if cancelled_at is None:
                        audio_before += len(base64.b64decode(message["pcm16"])) // 2
                        if audio_before >= 24_000 * 1.0:
                            await ws.send(json.dumps({"type": "context.cancel", "context_id": "b"}))
                            cancelled_at = time.perf_counter() - began
                    else:
                        audio_after += len(base64.b64decode(message["pcm16"])) // 2
                elif message["type"] in ("context.cancelled", "audio.done", "error"):
                    terminal = message["type"]
                    terminal_at = time.perf_counter() - began
                    break
            # Anything after the terminal event would be a defect.
            late = 0
            try:
                while True:
                    message = json.loads(await asyncio.wait_for(ws.recv(), 1.0))
                    if message.get("context_id") == "b" and message["type"] == "audio":
                        late += 1
            except asyncio.TimeoutError:
                pass
            report["cancel"] = {
                "audio_s_before_cancel": audio_before / 24_000,
                "cancel_sent_s": cancelled_at,
                "audio_s_after_cancel_before_ack": audio_after / 24_000,
                "terminal": terminal,
                "terminal_s": terminal_at if terminal not in (None, "timeout") else None,
                "audio_frames_after_terminal": late,
            }
            if pcm and out:
                # The whole held-back-suffix utterance, for listening or an ASR check.
                Path(out).with_suffix(".pcm").write_bytes(bytes(pcm))
                report["held_back_suffix"]["pcm_file"] = str(Path(out).with_suffix(".pcm"))
        return report

    report = asyncio.run(run())
    text = json.dumps(report, indent=2)
    print(text)
    if out:
        Path(out).write_text(text)


BENCH_SENTENCES = [
    "Sure, I can help you with that.",
    "The train to Lyon leaves at seven forty-five from platform three.",
    "Let me check the weather for tomorrow before we decide.",
    "Your balance is one hundred and twenty euros, and the next payment is due on Friday.",
    "I'm sorry, I didn't catch that. Could you say it again, a little more slowly?",
    "Photosynthesis turns light, water and carbon dioxide into sugar and oxygen.",
    "Okay.",
    "If you leave now, you should arrive a few minutes before the meeting starts.",
]


def bench(url: str, voice: str, out: str, words_per_second: float) -> None:
    """Time-to-first-audio and real-time factor over three input modes.

    * ``http``: complete text through ``/v1/audio/speech`` (first PCM byte).
    * ``ws-complete``: the whole sentence in one ``text.append`` + ``text.end``.
    * ``ws-trickle``: word by word at ``words_per_second`` (an LLM's pace), then
      ``text.end``; TTFA is measured from the first word, and the lag of each
      word's arrival is recorded against when its sentence finished arriving.
    RTF = wall time from first input to last audio / audio duration.
    """
    import asyncio  # noqa: PLC0415
    import base64  # noqa: PLC0415

    import requests  # noqa: PLC0415
    import websockets  # noqa: PLC0415

    base = url.rstrip("/")
    http_base = base.replace("ws://", "http://").replace("wss://", "https://")
    results: dict = {"url": base, "voice": voice, "words_per_second": words_per_second, "modes": {}}

    def summarise(rows: list[dict]) -> dict:
        def stat(key):
            values = sorted(row[key] for row in rows if row.get(key) is not None)
            if not values:
                return None
            return {"p50": values[len(values) // 2], "mean": sum(values) / len(values), "max": values[-1]}
        return {"n": len(rows), "ttfa_s": stat("ttfa_s"), "rtf": stat("rtf"), "audio_s": stat("audio_s"),
                "text_done_to_first_audio_s": stat("text_done_to_first_audio_s")}

    rows = []
    for sentence in BENCH_SENTENCES:
        began = time.perf_counter()
        first = None
        total = 0
        with requests.post(http_base + "/v1/audio/speech", json={"input": sentence, "voice": voice,
                           "response_format": "pcm", "stream": True}, stream=True, timeout=120) as response:
            response.raise_for_status()
            for chunk in response.iter_content(chunk_size=None):
                if chunk and first is None:
                    first = time.perf_counter() - began
                total += len(chunk)
        wall = time.perf_counter() - began
        audio = total / 2 / 24_000
        rows.append({"text": sentence, "ttfa_s": first, "audio_s": audio, "wall_s": wall,
                     "rtf": wall / audio if audio else None})
    results["modes"]["http"] = {"summary": summarise(rows), "rows": rows}

    async def ws_mode(trickle: bool) -> list[dict]:
        rows = []
        async with websockets.connect(base + "/v1/tts/stream", max_size=None) as ws:
            for number, sentence in enumerate(BENCH_SENTENCES):
                context = f"c{number}"
                await ws.send(json.dumps({"type": "context.open", "context_id": context, "voice": voice}))
                while json.loads(await ws.recv())["type"] != "context.ready":
                    pass
                began = time.perf_counter()
                first = None
                total = 0
                text_done = None

                async def send_text():
                    nonlocal text_done
                    if trickle:
                        words = sentence.split(" ")
                        for index, word in enumerate(words):
                            fragment = word if index == 0 else " " + word
                            await ws.send(json.dumps({"type": "text.append", "context_id": context,
                                                      "text": fragment}))
                            await asyncio.sleep(1.0 / words_per_second)
                    else:
                        await ws.send(json.dumps({"type": "text.append", "context_id": context, "text": sentence}))
                    await ws.send(json.dumps({"type": "text.end", "context_id": context}))
                    text_done = time.perf_counter() - began

                sender = asyncio.create_task(send_text())
                while True:
                    message = json.loads(await ws.recv())
                    if message.get("context_id") != context:
                        continue
                    if message["type"] == "audio":
                        if first is None:
                            first = time.perf_counter() - began
                        total += len(base64.b64decode(message["pcm16"])) // 2
                    elif message["type"] in ("audio.done", "error", "context.cancelled"):
                        break
                await sender
                wall = time.perf_counter() - began
                audio = total / 24_000
                rows.append({"text": sentence, "ttfa_s": first, "audio_s": audio, "wall_s": wall,
                             "text_done_s": text_done,
                             "text_done_to_first_audio_s": (first - text_done) if first is not None else None,
                             "audio_before_text_done": first is not None and first < text_done,
                             "rtf": wall / audio if audio else None})
        return rows

    # Four complete-text requests at once: rows share one batched step.
    from concurrent.futures import ThreadPoolExecutor  # noqa: PLC0415

    def one(sentence: str) -> dict:
        began = time.perf_counter()
        first = None
        total = 0
        with requests.post(http_base + "/v1/audio/speech", json={"input": sentence, "voice": voice,
                           "response_format": "pcm", "stream": True}, stream=True, timeout=120) as response:
            for chunk in response.iter_content(chunk_size=None):
                if chunk and first is None:
                    first = time.perf_counter() - began
                total += len(chunk)
        wall = time.perf_counter() - began
        audio = total / 2 / 24_000
        return {"text": sentence, "ttfa_s": first, "audio_s": audio, "wall_s": wall, "rtf": wall / audio if audio else None}

    began = time.perf_counter()
    with ThreadPoolExecutor(4) as pool:
        rows = list(pool.map(one, BENCH_SENTENCES[:4] * 2))
    wall = time.perf_counter() - began
    results["modes"]["http-concurrent-4"] = {
        "summary": {**summarise(rows), "aggregate_audio_s": sum(row["audio_s"] for row in rows), "wall_s": wall,
                    "aggregate_rtf": wall / sum(row["audio_s"] for row in rows)},
        "rows": rows}

    for name, trickle in (("ws-complete", False), ("ws-trickle", True)):
        rows = asyncio.run(ws_mode(trickle))
        results["modes"][name] = {"summary": summarise(rows), "rows": rows}
    try:
        results["server_health"] = requests.get(http_base + "/health", timeout=5).json()
    except Exception:  # noqa: BLE001
        pass
    text = json.dumps(results, indent=2)
    print(json.dumps({name: mode["summary"] for name, mode in results["modes"].items()}, indent=2))
    if out:
        Path(out).write_text(text)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--model", default=DEFAULT_REPOSITORY)
    parser.add_argument("--voice-repo", default=DEFAULT_VOICE_REPOSITORY)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9125)
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--rows", type=int, default=4, help="concurrent contexts (rows of the batched state)")
    parser.add_argument("--n-q", type=int, default=32, help="audio codebooks generated (8-32)")
    parser.add_argument("--temperature", type=float, default=0.6)
    parser.add_argument("--probe", default="", help="ws://host:port of a running server: run the incrementality probe")
    parser.add_argument("--probe-voice", default="default")
    parser.add_argument("--probe-out", default="")
    parser.add_argument("--probe-wait", type=float, default=3.0,
                        help="seconds to hold back the second half of the sentence")
    parser.add_argument("--bench", default="", help="ws://host:port of a running server: TTFA/RTF benchmark")
    parser.add_argument("--bench-out", default="")
    parser.add_argument("--bench-words-per-second", type=float, default=4.0)
    args = parser.parse_args()
    configure_logging()
    if args.probe:
        probe(args.probe, args.probe_voice, args.probe_out, args.probe_wait)
        return
    if args.bench:
        bench(args.bench, args.probe_voice, args.bench_out, args.bench_words_per_second)
        return
    synthesizer = KyutaiTTS(args.model, args.voice_repo, args.device, args.rows, args.n_q, args.temperature)
    serve_synthesizer(synthesizer, args.host, args.port)


if __name__ == "__main__":
    main()
