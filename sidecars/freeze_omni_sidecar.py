#!/usr/bin/env python3
"""Freeze-Omni behind sidecar protocol v1.

Freeze-Omni (VITA-MLLM) wraps a frozen Qwen2-7B-Instruct with a chunk-wise
streaming speech encoder, an autoregressive single-codebook speech decoder
(TiCodec, 24 kHz) and a three-way *dialogue-state head* on the LLM's last
hidden state. This sidecar runs the model's own continuous duplex loop, ported
from the upstream demo server (``bin/server.py``: ``send_pcm`` /
``llm_prefill`` / ``generate``) with the same thresholds and chunking:

* input is resampled to 16 kHz and cut into 160 ms chunks (2560 samples);
* Silero VAD (upstream ``web/vad.py``, threshold 0.8, 2 s hangover) opens a
  user segment, replaying the six cached chunks before the trigger;
* each chunk runs encoder -> adapter -> LLM prefill, and the state head on the
  last position decides: state 0 keep listening (``cl``), state 1 the user
  finished and the model should answer (``ss``), state 2 the segment was not a
  turn (``el``, e.g. noise or a backchannel);
* ``ss`` interrupts any answer in progress and starts a new one; the answer is
  decoded token by token (``cs``) and synthesised sentence by sentence while
  the listening path keeps running on the same weights in another thread.

What the model really does, and so what the sidecar declares:

* ``full_duplex``: the listening path runs while an answer is being spoken;
* ``native_vad``: Silero VAD plus the state head own the user floor;
* ``native_interaction`` and ``barge_in``: the state head decides whether new
  speech is a turn, and a new turn interrupts the current answer;
* ``text_injection``: the backbone is a text LLM; injected text is prefilled
  into its KV cache as a system block and conditions the next answer.

It does not transcribe the user, so ``transcript`` is not declared. The text it
emits (``text_delta``/``text_done``) is the assistant's own answer.

Every control decision is logged to stderr (and ``--control-log`` JSONL) with
wall-clock milliseconds, so floor timing can be measured separately from the
first audio.

Modes::

    # plumbing only
    python sidecars/freeze_omni_sidecar.py --mock
    # load the model once and serve sessions over TCP (engine side)
    python sidecars/freeze_omni_sidecar.py --serve --listen 127.0.0.1:9141 --src DIR
    # per-session stdio process that relays to the running engine
    python sidecars/freeze_omni_sidecar.py --engine tcp:127.0.0.1:9141
    # self-contained stdio (loads the model per session; slow)
    python sidecars/freeze_omni_sidecar.py --src DIR

    openrealtime conformance sidecar -- python3 sidecars/freeze_omni_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import copy
import json
import os
import queue
import socket
import sys
import threading
import time
import traceback
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log  # noqa: E402
from openrealtime_sidecar.protocol import write_message  # noqa: E402

MODEL_NAME = "VITA-MLLM/Freeze-Omni"
MODEL_INPUT_RATE = 16_000
MODEL_OUTPUT_RATE = 24_000
#: Upstream VAD chunk: frame_shift 160 samples x chunk_size 16 = 160 ms.
CHUNK_SAMPLES = 2_560
#: Upstream default role (bin/server.py GlobalParams) when the engine sends none.
UPSTREAM_ROLE = ("You are a helpful voice assistant. Your answer should be coherent, natural, "
                 "simple, complete. Your name is Xiao Yun. Your inventor is Tencent.")
SENTENCE_END = ("。", "：", "？", "！", ".", "?", "!", "\n")
FIRST_PACK_END = (",", "，", "。", "：", "？", "！", ".", ":", "?", "!", "\n")
#: Upstream demo server stops an answer after 500 tokens; its offline
#: bin/inference.py after 128.
MAX_ANSWER_TOKENS = 500


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


class OutputPacer:
    """Release output audio at playback pace, in order with the frames around it.

    Freeze-Omni synthesises a sentence at a time, several times faster than
    real time. Sent as produced, a client receives seconds of speech in one
    burst, and "stopped speaking" would mean "stopped sending" long before
    the listener stops hearing it. Paced, the audio that has not been played
    yet is still here when the model yields, and is dropped rather than
    delivered. Text and turn frames queue behind the audio that preceded them,
    so a turn never ends before its audio.
    """

    AUDIO = "output_audio"

    def __init__(self, raw_send, rate: int, *, frame_ms: int = 40, lead_ms: int = 120,
                 should_drop=None) -> None:
        import collections  # noqa: PLC0415

        self._raw_send = raw_send
        self._frame_bytes = int(rate * frame_ms / 1000) * 2
        self._rate = rate
        self._lead = lead_ms / 1000
        self._queue = collections.deque()
        self._cv = threading.Condition()
        self._next_due = 0.0
        self._should_drop = should_drop
        self._closed = False
        self.dropped_samples = 0
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    def put(self, message_type: str, payload: bytes, header: dict) -> None:
        with self._cv:
            self._queue.append((message_type, payload, header))
            self._cv.notify()

    def drop_audio(self) -> int:
        """Discard audio not yet released; returns the discarded samples."""
        with self._cv:
            kept = [item for item in self._queue if item[0] != self.AUDIO]
            dropped = sum(len(item[1]) // 2 for item in self._queue if item[0] == self.AUDIO)
            self._queue.clear()
            self._queue.extend(kept)
            self._next_due = 0.0
            self.dropped_samples += dropped
            self._cv.notify()
        return dropped

    def queued_audio_seconds(self) -> float:
        with self._cv:
            return sum(len(i[1]) // 2 for i in self._queue if i[0] == self.AUDIO) / self._rate

    def close(self, drain_timeout: float = 30.0) -> None:
        deadline = time.monotonic() + drain_timeout
        with self._cv:
            while self._queue and time.monotonic() < deadline:
                self._cv.wait(0.05)
            self.dropped_samples += sum(len(i[1]) // 2 for i in self._queue if i[0] == self.AUDIO)
            self._queue.clear()
            self._closed = True
            self._cv.notify()
        self._thread.join(timeout=2)

    def _run(self) -> None:
        while True:
            with self._cv:
                while not self._queue and not self._closed:
                    self._cv.wait()
                if not self._queue:
                    return
                if self._should_drop is not None and self._should_drop():
                    kept = [i for i in self._queue if i[0] != self.AUDIO]
                    self.dropped_samples += sum(len(i[1]) // 2 for i in self._queue if i[0] == self.AUDIO)
                    self._queue.clear()
                    self._queue.extend(kept)
                    self._next_due = 0.0
                    if not self._queue:
                        continue
                message_type, payload, header = self._queue[0]
                if message_type == self.AUDIO:
                    now = time.monotonic()
                    if self._next_due < now:
                        self._next_due = now
                    wait = self._next_due - self._lead - now
                    if wait > 0:
                        self._cv.wait(wait)
                        continue
                    frame, rest = payload[:self._frame_bytes], payload[self._frame_bytes:]
                    if rest:
                        self._queue[0] = (message_type, rest, header)
                    else:
                        self._queue.popleft()
                    self._next_due += len(frame) / 2 / self._rate
                    item = (message_type, frame, header)
                else:
                    item = self._queue.popleft()
                self._cv.notify_all()
                # A frame removed from the queue is still pending until its
                # write completes. Keep drop_audio ordered after that write.
                try:
                    self._raw_send(item[0], item[1], **item[2])
                except Exception:  # noqa: BLE001 - the peer went away; keep draining
                    pass


class FreezeOmniEngine:
    """The loaded weights, shared by every session of one process."""

    def __init__(self, source: str, model_path: str, llm_path: str, *, top_k: int,
                 top_p: float, temperature: float) -> None:
        source = os.path.abspath(source)
        if source not in sys.path:
            sys.path.insert(0, source)
        import torch  # noqa: PLC0415
        from models.decoder.llm2tts import llm2TTS  # noqa: PLC0415
        from models.pipeline import inferencePipeline  # noqa: PLC0415

        self.torch = torch
        started = time.perf_counter()
        self.pipeline = inferencePipeline(argparse.Namespace(
            model_path=model_path, llm_path=llm_path, top_k=top_k, top_p=top_p,
            temperature=temperature))
        self.tts = llm2TTS(model_path)
        with open(os.path.join(model_path, "server.json"), encoding="utf-8") as handle:
            self.server_configs = json.load(handle)
        self.load_seconds = time.perf_counter() - started
        log(f"freeze-omni loaded in {self.load_seconds:.1f}s; "
            f"allocated={torch.cuda.memory_allocated() / 2**30:.1f} GiB")

    def new_vad(self):
        from web.vad import VAD  # noqa: PLC0415 - upstream module, needs the source on sys.path

        return VAD()


class FreezeOmniSidecar(Sidecar):
    """One Freeze-Omni session."""

    model_name = MODEL_NAME
    output_rate = MODEL_OUTPUT_RATE
    capabilities = (
        Capability.FULL_DUPLEX,
        Capability.NATIVE_VAD,
        Capability.NATIVE_INTERACTION,
        Capability.BARGE_IN,
        Capability.TEXT_INJECTION,
    )

    PACED = ("output_audio", "text_delta", "text_done", "turn_done")

    def __init__(self, input_stream, output_stream, *, mock: bool = False,
                 engine: FreezeOmniEngine | None = None, engine_factory=None,
                 control: ControlLog | None = None, pace: bool = True,
                 max_answer_tokens: int = MAX_ANSWER_TOKENS) -> None:
        super().__init__(input_stream, output_stream)
        self.max_answer_tokens = max_answer_tokens
        self._pacer = OutputPacer(super().send, MODEL_OUTPUT_RATE,
                                  should_drop=self.interrupted) if pace else None
        self.mock = mock
        self.engine = engine
        self.engine_factory = engine_factory
        self.control = control or ControlLog(None)
        self.session_id = f"fo-{os.getpid()}-{_now_ms()}"
        self._resampler = None
        self._pcm = np.zeros(0, dtype=np.float32)
        self._chunks: queue.Queue[np.ndarray | None] = queue.Queue()
        self._listener: threading.Thread | None = None
        self._closing = threading.Event()
        self._kv_lock = threading.Lock()
        self._pending_text: list[tuple[str, str]] = []
        self._user_open = False
        self._generation: threading.Thread | None = None
        self._gen_lock = threading.Lock()
        self._stop_generation = threading.Event()
        self.generate_outputs = None
        self.system_role = None
        self.vad = None
        self._mock_energy_open = False
        self._chunks_seen = 0
        self._chunk_ms: list[float] = []
        self._text_carry = ""

    def send(self, message_type: str, payload: bytes = b"", **header) -> None:
        if self._pacer is not None and message_type in self.PACED:
            self._pacer.put(message_type, payload, header)
        else:
            super().send(message_type, payload, **header)

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello) -> None:
        import soxr  # noqa: PLC0415

        self._resampler = soxr.ResampleStream(self.input_rate, MODEL_INPUT_RATE, 1, dtype="float32")
        if self.mock:
            log("freeze-omni sidecar running in mock mode; no model is loaded")
        else:
            if self.engine is None:
                self.engine = self.engine_factory()
            role = self.instructions.strip() or UPSTREAM_ROLE
            self.system_role = self.engine.pipeline.speech_dialogue(None, stat="pre", role=role)
            self.generate_outputs = copy.deepcopy(self.system_role)
            self.vad = self.engine.new_vad()
        self.control.event(self.session_id, "session_start", mock=self.mock,
                           input_rate=self.input_rate, pace=self._pacer is not None,
                           max_answer_tokens=self.max_answer_tokens,
                           role=(self.instructions.strip() or UPSTREAM_ROLE)[:200])
        self._listener = threading.Thread(target=self._listen_loop, daemon=True)
        self._listener.start()

    def on_close(self) -> None:
        self._closing.set()
        self._stop_generation.set()
        self._chunks.put(None)
        # Keep the server's session slot until model calls have actually
        # returned. A timeout does not stop a Python worker or its CUDA work;
        # releasing caches/the slot then allows overlapping retired sessions.
        # A stuck backend therefore remains busy until the service is restarted.
        if self._listener is not None:
            self._listener.join()
        with self._gen_lock:
            generation = self._generation
        if generation is not None:
            generation.join()
        if self._pacer is not None:
            self._pacer.close(drain_timeout=0.5)
        stats = {"dropped_unplayed_s": round(self._pacer.dropped_samples / MODEL_OUTPUT_RATE, 3)
                 if self._pacer is not None else 0.0}
        if self._chunk_ms:
            stats.update({"chunks": len(self._chunk_ms),
                          "chunk_ms_mean": round(float(np.mean(self._chunk_ms)), 2),
                          "chunk_ms_p95": round(float(np.percentile(self._chunk_ms, 95)), 2)})
        self.control.event(self.session_id, "session_end", **stats)
        if not self.mock:
            # Session state is per-session; releasing it keeps a long-lived
            # engine from accumulating KV caches of finished sessions.
            self.generate_outputs = None
            self.system_role = None
            self.engine.torch.cuda.empty_cache()

    # --- input --------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        samples = np.frombuffer(pcm16, dtype=np.int16).astype(np.float32) / 32768.0
        converted = self._resampler.resample_chunk(samples)
        if converted.size:
            self._pcm = np.concatenate([self._pcm, converted.astype(np.float32)])
        while self._pcm.size >= CHUNK_SAMPLES:
            self._chunks.put(self._pcm[:CHUNK_SAMPLES].copy())
            self._pcm = self._pcm[CHUNK_SAMPLES:]

    def on_text(self, text: str, role: str) -> None:
        text = text.strip()
        if not text:
            return
        self.control.event(self.session_id, "text_injected", role=role, chars=len(text))
        with self._kv_lock:
            self._pending_text.append((role or "system", text))
        if not self.mock and not self._user_open and not self._generating():
            with self._kv_lock:
                self.generate_outputs = self._apply_pending(self.generate_outputs)

    def on_respond(self) -> None:
        """An explicit turn request (hand-off or a client-driven turn)."""
        self.control.event(self.session_id, "respond_requested")
        if self.mock:
            self._respond_mock()
            return
        self._stop_current_generation("respond")
        with self._kv_lock:
            outputs = self._apply_pending(copy.deepcopy(self.generate_outputs))
        # Tracked like a model-initiated answer so the state head can still
        # interrupt it; the base class emits this turn's turn_done.
        thread = self._start_generation(self._to_speak(outputs), source="respond",
                                        emit_turn_done=False)
        thread.join()

    # --- the listening path (upstream send_pcm / llm_prefill) ---------------

    def _listen_loop(self) -> None:
        outputs = None
        while True:
            chunk = self._chunks.get()
            if chunk is None or self._closing.is_set():
                return
            backlog = self._chunks.qsize()
            if backlog > 3 and backlog % 10 == 0:
                log(f"freeze-omni listener backlog {backlog} chunks ({backlog * 160} ms)")
            try:
                started = time.perf_counter()
                if self.mock:
                    self._mock_listen(chunk)
                else:
                    outputs = self._listen_chunk(chunk, outputs)
                self._chunk_ms.append((time.perf_counter() - started) * 1000)
                self._chunks_seen += 1
            except Exception as failure:  # noqa: BLE001
                log(traceback.format_exc())
                self.error(f"listen: {failure}", code="listen_failed")

    def _listen_chunk(self, chunk: np.ndarray, outputs):
        torch = self.engine.torch
        with torch.no_grad():
            result = self.vad.predict(np.float32(chunk))
        status = result.get("status")
        if status == "sl":
            self._open_user("vad_start")
            with self._kv_lock:
                outputs = self._apply_pending(copy.deepcopy(self.generate_outputs))
            outputs["adapter_cache"] = None
            outputs["encoder_cache"] = None
            outputs["pe_index"] = 0
            outputs["stat"] = "sl"
            outputs["last_id"] = None
            outputs.pop("text", None)
            outputs.pop("hidden_state", None)
            for index, feature in enumerate(result["feature_last_chunk"]):
                outputs = self._prefill("sl" if index == 0 else "cl", feature, outputs, first_pack=True)
            outputs = self._prefill("cl", result["feature"], outputs)
        elif status in ("cl", "el") and outputs is not None:
            outputs = self._prefill(status, result["feature"], outputs)
        return outputs

    def _prefill(self, status: str, feature, outputs, first_pack: bool = False):
        torch = self.engine.torch
        pipeline = self.engine.pipeline
        if status == "sl":
            outputs = pipeline.speech_dialogue(torch.tensor(feature), **outputs)
        if status == "el":
            self.vad.in_dialog = False
            self._close_user("vad_timeout")
        if status == "cl":
            if outputs["stat"] == "cl":
                outputs = pipeline.speech_dialogue(torch.tensor(feature), **outputs)
            if first_pack:
                outputs["stat"] = "cl"
            if outputs["stat"] == "el":
                self.vad.in_dialog = False
                self._close_user("model_el")
            if outputs["stat"] == "ss":
                self.vad.in_dialog = False
                self._close_user("model_ss")
                self._stop_current_generation("model_ss")
                self._start_generation(copy.deepcopy(outputs), source="model_ss")
        return outputs

    def _open_user(self, reason: str) -> None:
        if not self._user_open:
            self._user_open = True
            self.control.event(self.session_id, reason, generating=self._generating())
            self.send("speech_started")

    def _close_user(self, reason: str) -> None:
        self.control.event(self.session_id, reason, generating=self._generating())
        if self._user_open:
            self._user_open = False
            self.send("speech_stopped")

    # --- the speaking path (upstream generate / decoder) --------------------

    def _generating(self) -> bool:
        with self._gen_lock:
            return self._generation is not None and self._generation.is_alive()

    def _stop_current_generation(self, reason: str) -> None:
        with self._gen_lock:
            generation = self._generation
        if generation is not None and generation.is_alive():
            self._stop_generation.set()
            dropped = self._pacer.drop_audio() if self._pacer is not None else 0
            self.control.event(self.session_id, "interrupt_answer", reason=reason,
                               dropped_unplayed_s=round(dropped / MODEL_OUTPUT_RATE, 3))
            generation.join()
        elif self._pacer is not None:
            # The answer finished generating but may still be playing.
            dropped = self._pacer.drop_audio()
            if dropped:
                self.control.event(self.session_id, "interrupt_playback", reason=reason,
                                   dropped_unplayed_s=round(dropped / MODEL_OUTPUT_RATE, 3))

    def _start_generation(self, outputs, source: str, emit_turn_done: bool = True) -> threading.Thread:
        self._stop_generation.clear()
        self._interrupted.clear()
        thread = threading.Thread(target=self._generate, args=(outputs,),
                                  kwargs={"source": source, "emit_turn_done": emit_turn_done},
                                  daemon=True)
        with self._gen_lock:
            self._generation = thread
        thread.start()
        return thread

    def _to_speak(self, outputs):
        outputs["adapter_cache"] = None
        outputs["encoder_cache"] = None
        outputs["pe_index"] = 0
        outputs["stat"] = "ss"
        outputs.pop("text", None)
        outputs.pop("hidden_state", None)
        return outputs

    def _emit_text(self, piece: str) -> None:
        """Send a text delta; whitespace-only pieces ride on the next one.

        The protocol rejects a delta without visible text, and Qwen emits
        bare newlines and spaces as their own tokens.
        """
        self._text_carry += piece
        if self._text_carry.strip():
            self.text_delta(self._text_carry)
            self._text_carry = ""

    def _should_stop(self) -> bool:
        return self._stop_generation.is_set() or self.interrupted() or self._closing.is_set()

    def _generate(self, outputs, *, source: str, emit_turn_done: bool) -> None:
        engine = self.engine
        pipeline = engine.pipeline
        t0 = _now_ms()
        whole_text = ""
        self._text_carry = ""
        first_audio_ms = None
        audio_samples = 0
        try:
            if outputs.get("stat") != "ss":
                outputs = self._to_speak(outputs)
            outputs = pipeline.speech_dialogue(None, **outputs)
            with self._kv_lock:
                self.generate_outputs = copy.deepcopy(outputs)
            if outputs.get("stat") != "cs":
                self.control.event(self.session_id, "answer_empty", source=source)
                return
            hidden = [outputs["hidden_state"]]
            cur_text, last_text = "", ""
            generate_num = 0
            first_text = True
            while not self._should_stop():
                if len(outputs["past_tokens"]) > self.max_answer_tokens:
                    break
                del outputs["text"]
                del outputs["hidden_state"]
                outputs = pipeline.speech_dialogue(None, **outputs)
                with self._kv_lock:
                    self.generate_outputs = copy.deepcopy(outputs)
                if outputs["stat"] != "cs":
                    break
                hidden.append(outputs["hidden_state"])
                piece = outputs["text"][len(last_text):]
                if "�" in piece:
                    continue
                if first_text:
                    first_text = False
                    self.control.event(self.session_id, "first_text", source=source,
                                       after_ms=_now_ms() - t0)
                whole_text += piece
                cur_text += piece
                self._emit_text(piece)
                ends = FIRST_PACK_END if (generate_num == 0 or len(hidden) >= 20) else SENTENCE_END
                if piece.endswith(ends) and len(hidden) >= 4 and not (
                        piece.endswith(".") and last_text and last_text[-1].isdigit()):
                    generate_num, emitted = self._synthesise(hidden, cur_text, generate_num)
                    if emitted and first_audio_ms is None:
                        first_audio_ms = _now_ms() - t0
                        self.control.event(self.session_id, "first_audio", source=source,
                                           after_ms=first_audio_ms)
                    audio_samples += emitted
                    cur_text, hidden = "", []
                last_text = outputs["text"]
            if hidden and cur_text and not self._should_stop():
                generate_num, emitted = self._synthesise(hidden, cur_text, generate_num)
                if emitted and first_audio_ms is None:
                    first_audio_ms = _now_ms() - t0
                    self.control.event(self.session_id, "first_audio", source=source,
                                       after_ms=first_audio_ms)
                audio_samples += emitted
        except Exception as failure:  # noqa: BLE001
            log(traceback.format_exc())
            self.error(f"generate: {failure}", code="generate_failed")
        finally:
            elapsed = _now_ms() - t0
            audio_s = audio_samples / MODEL_OUTPUT_RATE
            self.control.event(self.session_id, "answer_end", source=source,
                               interrupted=self._should_stop(), text=whole_text,
                               wall_ms=elapsed, audio_s=round(audio_s, 3),
                               rtf=round(elapsed / 1000 / audio_s, 3) if audio_s else None)
            if whole_text:
                self.text_done(whole_text)
            if emit_turn_done:
                self.turn_done()
            # Text that arrived during the answer applies to the context now.
            with self._kv_lock:
                if self._pending_text and self.generate_outputs is not None and not self._user_open:
                    self.generate_outputs = self._apply_pending(self.generate_outputs)

    def _synthesise(self, hidden, text: str, generate_num: int) -> tuple[int, int]:
        """Upstream ``decoder``: speak one text segment, streaming codec chunks."""
        engine = self.engine
        torch = engine.torch
        pipeline = engine.pipeline
        configs = engine.server_configs
        state = torch.cat(hidden).squeeze(1)
        text_processed = pipeline.post_process(text)
        embeddings = pipeline.model.llm_decoder.model.embed_tokens(
            torch.tensor(pipeline.model.tokenizer.encode(text_processed)).cuda())
        chunk_size = configs["decoder_first_chunk_size"]
        threshold = configs["decoder_seg_threshold_first_pack"]
        if generate_num != 0:
            chunk_size = configs["decoder_chunk_size"]
            threshold = configs["decoder_seg_threshold"]
        emitted = 0
        for segment in engine.tts.run(
                embeddings.reshape(-1, 896).unsqueeze(0), configs["decoder_top_k"],
                state.reshape(-1, 896).unsqueeze(0), chunk_size,
                configs["decoder_chunk_overlap_size"], configs["decoder_penalty_window_size"],
                configs["decoder_penalty"], configs["decoder_N"], threshold):
            if generate_num == 0:
                loud = torch.nonzero(segment.abs() > 0.03, as_tuple=True)[-1]
                if loud.numel():
                    segment = segment[:, :, loud[0]:]
            generate_num += 1
            if self._should_stop():
                break
            pcm = (segment.squeeze().float().clamp(-1, 1).cpu().numpy() * 32767).astype(np.int16)
            emitted += pcm.size
            self.audio(pcm.tobytes())
        return generate_num, emitted

    # --- text injection -----------------------------------------------------

    def _apply_pending(self, outputs):
        """Prefill queued text into the LLM context as a system block.

        Every block in Freeze-Omni's context is left open and the next one
        starts with ``<|im_end|>``, so injected text follows the same shape.
        Called with ``_kv_lock`` held.
        """
        if self.mock or not self._pending_text or outputs is None:
            return outputs
        torch = self.engine.torch
        model = self.engine.pipeline.model
        pending, self._pending_text = self._pending_text, []
        for role, text in pending:
            role = role if role in ("system", "user", "assistant") else "system"
            ids = [model.tokenizer.eod_id] + model.tokenizer(
                f"\n<|im_start|>{role}\n{text}")["input_ids"]
            ids_t = torch.tensor([ids], device="cuda")
            with torch.no_grad(), torch.autocast(device_type="cuda", dtype=torch.bfloat16):
                embeds = model.llm_decoder.transformer.wte(ids_t)
                past = outputs["past_key_values"]
                mask = torch.ones([1, past[0][0].size(2) + ids_t.size(1)], dtype=torch.bool, device="cuda")
                _, past, _, _ = model._generate_one_step(
                    {"inputs_embeds": embeds.half(), "attention_mask": mask, "past_key_values": past}, "sl")
            outputs["past_key_values"] = past
            self.control.event(self.session_id, "text_prefilled", role=role, tokens=len(ids))
        return outputs

    # --- mock ---------------------------------------------------------------

    def _mock_listen(self, chunk: np.ndarray) -> None:
        loud = float(np.sqrt(np.mean(np.square(chunk)))) > 0.02
        if loud and not self._mock_energy_open:
            self._mock_energy_open = True
            self._open_user("mock_energy_start")
        elif not loud and self._mock_energy_open:
            self._mock_energy_open = False
            self._close_user("mock_energy_stop")

    def _respond_mock(self) -> None:
        with self._kv_lock:
            pending = " ".join(text for _, text in self._pending_text)
            self._pending_text.clear()
        answer = pending or "This is the Freeze-Omni sidecar running without a model."
        self.text_delta(answer)
        self.text_done(answer)
        tone = (0.1 * np.sin(2 * np.pi * 220 * np.arange(MODEL_OUTPUT_RATE // 10) / MODEL_OUTPUT_RATE))
        frame = (tone * 32767).astype(np.int16).tobytes()
        for _ in range(4):
            if self.interrupted():
                return
            self.audio(frame)


# --- transports -------------------------------------------------------------

def parse_address(address: str) -> tuple[str, int]:
    address = address.removeprefix("tcp:")
    host, _, port = address.rpartition(":")
    return host or "127.0.0.1", int(port)


def relay(address: str, busy_wait: float = 30.0, pace: bool = True, max_turn_tokens: int = 0) -> None:
    """Relay one stdio session to a running engine, frame by frame.

    The relay replays the hello while the engine is still closing a previous
    session (``busy``), and it guarantees two protocol invariants whatever the
    engine sends: a ``text_delta`` never carries only whitespace (the engine's
    whitespace is carried into the next delta), and bytes are never split
    across frames. ``max_turn_tokens`` caps an answer from this side (one
    text delta is one token) for an engine started with a larger budget: the
    engine is asked to stop generating, and what it already said still plays.
    """
    from openrealtime_sidecar.protocol import read_message  # noqa: PLC0415

    host, port = parse_address(address)
    stdin, stdout = sys.stdin.buffer, sys.stdout.buffer
    hello = read_message(stdin)
    if hello is None:
        return
    deadline = time.monotonic() + busy_wait
    while True:
        connection = socket.create_connection((host, port))
        connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        reader, writer = connection.makefile("rb"), connection.makefile("wb")
        write_message(writer, hello.type, hello.payload, **{
            k: v for k, v in hello.header.items() if k not in ("type", "payload_bytes")})
        first = read_message(reader)
        if first is not None and first.type == "error" and first.get("code") == "busy" \
                and time.monotonic() < deadline:
            connection.close()
            time.sleep(0.5)
            continue
        break

    out_lock = threading.Lock()

    def raw_send(message_type, payload=b"", **header) -> None:
        with out_lock:
            write_message(stdout, message_type, payload, **header)

    rate = int(first.get("output_rate", MODEL_OUTPUT_RATE)) if first is not None else MODEL_OUTPUT_RATE
    pacer = OutputPacer(raw_send, rate) if pace else None

    def upstream_paced() -> None:
        try:
            while True:
                message = read_message(stdin)
                if message is None:
                    break
                if message.type == "interrupt" and pacer is not None:
                    pacer.drop_audio()
                write_message(writer, message.type, message.payload, **{
                    k: v for k, v in message.header.items() if k not in ("type", "payload_bytes")})
        except (OSError, ValueError):
            pass
        finally:
            try:
                connection.shutdown(socket.SHUT_WR)
            except OSError:
                pass

    threading.Thread(target=upstream_paced, daemon=True).start()
    carry = ""
    audio_in_turn = False
    yielded_mid_turn = False
    turn_tokens = 0
    capped = False
    message = first
    while message is not None:
        header = {k: v for k, v in message.header.items() if k not in ("type", "payload_bytes")}
        paced = pacer is not None and message.type in FreezeOmniSidecar.PACED
        if message.type == "text_delta":
            turn_tokens += 1
            if max_turn_tokens and turn_tokens > max_turn_tokens and not capped:
                capped = True
                log(f"relay: answer reached {max_turn_tokens} tokens; asking the engine to stop generating")
                try:
                    write_message(writer, "interrupt")
                except OSError:
                    pass
            text = carry + str(message.get("text", ""))
            if text.strip():
                carry = ""
                (pacer.put("text_delta", b"", {"text": text}) if paced else raw_send("text_delta", text=text))
        else:
            if message.type == "output_audio":
                audio_in_turn = True
            elif message.type == "speech_stopped" and audio_in_turn:
                # The model ended a user turn while its own answer was in
                # flight: the answer is about to be interrupted.
                yielded_mid_turn = True
            elif message.type in ("text_done", "turn_done"):
                carry = ""
                if message.type == "turn_done":
                    if yielded_mid_turn and pacer is not None:
                        pacer.drop_audio()
                    audio_in_turn = yielded_mid_turn = capped = False
                    turn_tokens = 0
            if paced:
                pacer.put(message.type, message.payload, header)
            else:
                raw_send(message.type, message.payload, **header)
        try:
            message = read_message(reader)
        except (OSError, ValueError):
            break
    if pacer is not None:
        pacer.close()


def serve(address: str, sidecar_factory, max_sessions: int, busy_wait: float = 15.0) -> None:
    host, port = parse_address(address)
    server = socket.create_server((host, port), reuse_port=False)
    active = threading.Semaphore(max_sessions)
    log(f"serving sidecar sessions on tcp:{host}:{port} (max {max_sessions})")

    def handle(connection: socket.socket) -> None:
        connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        reader = connection.makefile("rb")
        writer = connection.makefile("wb")
        try:
            # A new session may arrive while the previous one is still
            # stopping its answer; give it a moment before refusing.
            if not active.acquire(timeout=busy_wait):
                write_message(writer, "error", text="engine busy: all sessions in use",
                              code="busy", fatal=True)
                return
            try:
                sidecar_factory(reader, writer).run()
            finally:
                active.release()
        except Exception:  # noqa: BLE001
            log(traceback.format_exc())
        finally:
            for closable in (writer, reader):
                try:
                    closable.close()
                except OSError:
                    pass
            connection.close()

    while True:
        connection, _ = server.accept()
        threading.Thread(target=handle, args=(connection,), daemon=True).start()


def default_path(repository: str, subdir: str = "") -> str:
    from huggingface_hub import snapshot_download  # noqa: PLC0415

    root = snapshot_download(repository, local_files_only=True)
    return os.path.join(root, subdir) if subdir else root


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--mock", action="store_true", help="speak the protocol without loading a model")
    parser.add_argument("--serve", action="store_true", help="load once and serve sessions over TCP")
    parser.add_argument("--listen", default="127.0.0.1:9141", help="TCP address for --serve")
    parser.add_argument("--engine", default="", help="relay this stdio session to a running --serve engine")
    parser.add_argument("--max-sessions", type=int, default=1)
    parser.add_argument("--src", default=os.environ.get("FREEZE_OMNI_SRC", ""),
                        help="Freeze-Omni source checkout (github.com/VITA-MLLM/Freeze-Omni)")
    parser.add_argument("--model-path", default="", help="Freeze-Omni checkpoints dir (default: HF cache)")
    parser.add_argument("--llm-path", default="", help="Qwen2-7B-Instruct dir (default: HF cache)")
    parser.add_argument("--top-k", type=int, default=20)
    parser.add_argument("--top-p", type=float, default=0.8)
    parser.add_argument("--temperature", type=float, default=0.8)
    parser.add_argument("--control-log", default="", help="append control decisions as JSONL here")
    parser.add_argument("--max-answer-tokens", type=int, default=MAX_ANSWER_TOKENS,
                        help="stop an answer after this many tokens (upstream server 500, inference.py 128)")
    parser.add_argument("--max-turn-tokens", type=int, default=0,
                        help="relay mode: ask the engine to stop an answer after this many tokens (0 = engine's budget)")
    parser.add_argument("--no-pace", action="store_true",
                        help="send audio as synthesised instead of at playback pace")
    arguments = parser.parse_args()
    pace = not arguments.no_pace

    control = ControlLog(arguments.control_log or None)
    if arguments.engine:
        relay(arguments.engine, pace=pace, max_turn_tokens=arguments.max_turn_tokens)
        return
    # Upstream code print()s diagnostics (e.g. every state-head probability).
    # On stdio those bytes would corrupt the protocol stream, so the protocol
    # keeps the real stdout and Python-level printing goes to stderr.
    protocol_in, protocol_out = sys.stdin.buffer, sys.stdout.buffer
    sys.stdout = sys.stderr
    if arguments.mock:
        FreezeOmniSidecar(protocol_in, protocol_out, mock=True, control=control, pace=pace).run()
        return

    def load() -> FreezeOmniEngine:
        if not arguments.src:
            raise RuntimeError("--src (the Freeze-Omni source checkout) is required to load the model")
        model_path = arguments.model_path or default_path("VITA-MLLM/Freeze-Omni", "checkpoints")
        llm_path = arguments.llm_path or default_path("Qwen/Qwen2-7B-Instruct")
        return FreezeOmniEngine(arguments.src, model_path, llm_path, top_k=arguments.top_k,
                                top_p=arguments.top_p, temperature=arguments.temperature)

    if arguments.serve:
        engine = load()
        serve(arguments.listen,
              lambda reader, writer: FreezeOmniSidecar(reader, writer, engine=engine, control=control,
                                                     pace=pace,
                                                     max_answer_tokens=arguments.max_answer_tokens),
              arguments.max_sessions)
        return
    FreezeOmniSidecar(protocol_in, protocol_out, engine_factory=load, control=control,
                      pace=pace, max_answer_tokens=arguments.max_answer_tokens).run()


if __name__ == "__main__":
    main()
