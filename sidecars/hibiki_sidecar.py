#!/usr/bin/env python3
"""Hibiki simultaneous French-to-English speech translation behind the sidecar protocol.

Hibiki (kyutai/hibiki-2b-pytorch-bf16) is a Moshi-architecture multistream
model: every 80 ms Mimi frame it consumes 16 codebooks of *source* speech and
emits one text token plus 16 codebooks of *translated* speech. It is a
translation task profile, not a conversation: the text stream is the English
translation the model is speaking, never a transcript of the French source,
and the model decides its own lag (it waits until a target word is predictable
from the source). There is no floor, no barge-in, and no injected context.

What the sidecar maps onto the protocol:

* ``audio``     source speech, consumed continuously; any input rate is
                resampled to Mimi's 24 kHz with a streaming resampler.
* ``text_delta`` translated text pieces as the model emits them. With
                ``--timing-fields`` each header also carries ``step`` (input
                frames consumed in the current segment), ``source_seconds``
                (real source audio consumed) and ``segment`` so a harness can
                measure lag in model time. Off by default: the engine decodes
                headers strictly and would reject the extra fields.
* ``output_audio`` translated speech, one 80 ms frame per input frame.
* ``commit``    the source segment ended. The sidecar feeds Hibiki's
                end-of-stream marker (every source codebook set to the codec
                cardinality, as ``moshi.run_inference`` does), keeps stepping on
                silence until the model emits its text EOS (or a frame budget
                runs out), sends ``text_done`` with the segment's translation
                and ``turn_done``, and resets the streaming state for the next
                segment. Source audio that arrives during that flush is held
                and fed after the reset, so nothing the speaker says is lost.
* ``respond``   the same flush, answered by the base class's ``turn_done``.

Without ``commit`` the model simply keeps translating a continuous stream
(its context is 40 s of attention over sequences trained up to 120 s).

Run it on its own for conformance:

    openrealtime conformance sidecar -- python3 sidecars/hibiki_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import json
import queue
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, MessageType, Sidecar, log, run  # noqa: E402

DEFAULT_REPOSITORY = "kyutai/hibiki-2b-pytorch-bf16"
#: Mimi runs at 24 kHz with 80 ms frames.
MODEL_RATE = 24_000
FRAME_SAMPLES = 1_920
FRAME_SECONDS = FRAME_SAMPLES / MODEL_RATE
#: Frames buffered before the oldest is dropped. Twenty seconds: a translation
#: that loses source audio is wrong, so this is a backstop for a stalled GPU,
#: not a pacing mechanism.
QUEUE_FRAMES = 250


class _Resampler:
    """Streaming PCM resampler into Mimi's rate."""

    def __init__(self, source_rate: int) -> None:
        self.source_rate = source_rate
        self._stream = None
        if source_rate != MODEL_RATE:
            import soxr  # noqa: PLC0415

            self._stream = soxr.ResampleStream(source_rate, MODEL_RATE, 1, dtype="float32", quality="HQ")

    def __call__(self, samples: np.ndarray) -> np.ndarray:
        if self._stream is None:
            return samples
        return self._stream.resample_chunk(samples, last=False)


class HibikiSidecar(Sidecar):
    """Hibiki behind the sidecar protocol."""

    model_name = DEFAULT_REPOSITORY
    output_rate = MODEL_RATE
    # Declared honestly: it listens and speaks at once. It reports no
    # transcript of what it heard (its text is the translation), takes no
    # injected text, calls no tools, detects no voice activity, and does not
    # yield to overlap.
    capabilities = (Capability.FULL_DUPLEX,)

    def __init__(self, input_stream, output_stream, *, repository: str, mock: bool, device: str,
                 cfg_coef: float, seed: int, max_flush_seconds: float, flush_pace: str,
                 timing_fields: bool = False) -> None:
        super().__init__(input_stream, output_stream)
        self.repository = repository
        self.mock = mock
        self.device = device
        self.cfg_coef = cfg_coef
        self.seed = seed
        self.max_flush_frames = max(1, int(round(max_flush_seconds / FRAME_SECONDS)))
        self.flush_pace = flush_pace
        self.timing_fields = timing_fields
        # (sequence, frame): the sequence number is what orders source audio
        # against a flush request that arrives while frames are still queued.
        self._frames: queue.Queue[tuple[int, np.ndarray]] = queue.Queue(maxsize=QUEUE_FRAMES)
        self._enqueued = 0
        self._flush_mark = 0
        self._buffer = np.zeros(0, dtype=np.float32)
        self._resampler: _Resampler | None = None
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        # Flush (end of segment) state, owned by the stream thread once set.
        self._flush_requested = threading.Event()
        self._flush_done = threading.Event()
        self._flush_turn_done = False
        self._flush_lock = threading.Lock()
        self._dropped = 0

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello) -> None:
        self.model_name = self.repository
        self._resampler = _Resampler(self.input_rate)
        if self.mock:
            log("hibiki sidecar running in mock mode; no model is loaded")
            self._thread = threading.Thread(target=self._mock_stream, daemon=True)
            self._thread.start()
            return
        import torch  # noqa: PLC0415
        from moshi.models import LMGen, loaders  # noqa: PLC0415
        from moshi.run_inference import get_condition_tensors  # noqa: PLC0415

        torch.manual_seed(self.seed)
        began = time.perf_counter()
        info = loaders.CheckpointInfo.from_hf_repo(self.repository)
        if info.model_type != "hibiki":
            raise RuntimeError(f"{self.repository} is a {info.model_type!r} checkpoint, not hibiki")
        self.mimi = info.get_mimi(device=self.device)
        self.tokenizer = info.get_text_tokenizer()
        # get_moshi also aliases the text EOS embedding to PAD for hibiki, so
        # an EOS sampled before the end of input is harmlessly ignored.
        lm = info.get_moshi(device=self.device, dtype=torch.bfloat16)
        conditions = get_condition_tensors(info.model_type, lm, 1, self.cfg_coef)
        self.lm_gen = LMGen(lm, cfg_coef=self.cfg_coef, condition_tensors=conditions, **info.lm_gen_config)
        self.eos_id = self.tokenizer.eos_id()
        # 0 is the word-boundary marker and 3 padding in the text stream; BOS
        # opens the model's speech and EOS closes it. None of them is text.
        self._special_ids = {0, 3, self.eos_id, self.tokenizer.bos_id(), self.tokenizer.pad_id()}
        self.eos_codes = torch.full((1, self.mimi.num_codebooks, 1), self.mimi.cardinality,
                                    device=self.device, dtype=torch.long)
        self.silence = torch.zeros((1, 1, FRAME_SAMPLES), device=self.device)
        self._torch = torch
        self.mimi.streaming_forever(1)
        self.lm_gen.streaming_forever(1)
        # Warm up so the CUDA graphs are captured before the first real frame.
        with torch.no_grad():
            for _ in range(4):
                codes = self.mimi.encode(self.silence)
                tokens = self.lm_gen.step(codes)
                if tokens is not None:
                    self.mimi.decode(tokens[:, 1:])
            torch.cuda.synchronize()
        self._reset()
        log(f"hibiki ready in {time.perf_counter() - began:.1f}s: {self.mimi.num_codebooks} codebooks, "
            f"lm_gen={info.lm_gen_config}, cfg_coef={self.cfg_coef}, seed={self.seed}")
        self._thread = threading.Thread(target=self._stream, daemon=True)
        self._thread.start()

    def on_close(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=5)

    # --- session ------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        samples = np.frombuffer(pcm16, dtype="<i2").astype(np.float32) / 32768.0
        if self._resampler is not None:
            samples = self._resampler(samples)
        self._buffer = np.concatenate([self._buffer, samples.astype(np.float32, copy=False)])
        while self._buffer.size >= FRAME_SAMPLES:
            item = (self._enqueued, self._buffer[:FRAME_SAMPLES].copy())
            self._enqueued += 1
            self._buffer = self._buffer[FRAME_SAMPLES:]
            try:
                self._frames.put_nowait(item)
            except queue.Full:
                self._dropped += 1
                try:
                    self._frames.get_nowait()
                    self._frames.put_nowait(item)
                except queue.Empty:
                    pass

    def on_commit(self) -> None:
        """The source segment ended: flush the translation and reset."""
        self._request_flush(turn_done=True)

    def on_respond(self) -> None:
        """Translate what has been heard so far to its end, then return.

        The base class sends ``turn_done`` when this returns, so the flush
        itself does not. An interrupt stops the wait, not the flush: the
        translation of audio already heard still completes and is sent.
        """
        if self.mock:
            self._respond_mock()
            return
        done = self._request_flush(turn_done=False)
        while not done.wait(0.05):
            if self.interrupted() or self._stop.is_set():
                return

    def _request_flush(self, *, turn_done: bool) -> threading.Event:
        with self._flush_lock:
            if not self._flush_requested.is_set():
                self._flush_done = threading.Event()
                self._flush_turn_done = turn_done
                # Audio read before this request is the segment's source and
                # is translated before the end-of-stream marker. The read
                # thread enqueues audio inline, so every frame that preceded
                # the commit/respond on the wire is already counted.
                self._flush_mark = self._enqueued
                self._flush_requested.set()
            elif turn_done:
                self._flush_turn_done = True
            return self._flush_done

    def _respond_mock(self) -> None:
        answer = "(mock translation)"
        self.text_delta(answer)
        self.text_done(answer)
        for _ in range(4):
            if self.interrupted():
                return
            self.audio(np.zeros(FRAME_SAMPLES, dtype=np.int16).tobytes())

    def _mock_stream(self) -> None:
        while not self._stop.is_set():
            try:
                self._frames.get(timeout=0.2)
            except queue.Empty:
                pass
            if self._flush_requested.is_set():
                self._respond_mock()
                if self._flush_turn_done:
                    self.turn_done()
                self._flush_requested.clear()
                self._flush_done.set()

    # --- streaming ----------------------------------------------------------

    def _reset(self) -> None:
        self.mimi.reset_streaming()
        self.lm_gen.reset_streaming()
        self._first_step = True
        self._segment_steps = 0      # input frames consumed this segment
        self._segment_source = 0     # of which real source frames
        self._segment_text: list[str] = []
        self._word_start = False
        self._bytes = bytearray()
        self._eos_fed = False
        self._flush_steps = 0
        self._compute: list[float] = []

    def _stream(self) -> None:
        """Consume source frames forever; flush and reset on request."""
        torch = self._torch
        self._segment = 0
        deferred: list[tuple[int, np.ndarray]] = []
        next_tick = 0.0
        with torch.no_grad():
            while not self._stop.is_set():
                if self._flush_requested.is_set() and not self._eos_fed:
                    # Source still queued from before the request comes first.
                    try:
                        item = self._frames.get_nowait()
                    except queue.Empty:
                        item = None
                    if item is not None:
                        if item[0] < self._flush_mark:
                            self._step(item[1])
                            continue
                        deferred.append(item)
                if self._flush_requested.is_set():
                    # Input keeps its clock during the flush: each arriving
                    # frame is one silent model step and is itself kept for
                    # the next segment. Without input, silence self-clocks
                    # every 80 ms from the previous step's start (real time),
                    # or back to back with --flush-pace fast, so a respond or
                    # commit with no audio behind it still completes.
                    now = time.monotonic()
                    if self._flush_steps == 0 and not self._eos_fed:
                        next_tick = now
                    wait = 0.0 if self.flush_pace == "fast" else max(0.0, next_tick - now)
                    try:
                        if wait > 0:
                            deferred.append(self._frames.get(timeout=wait))
                        else:
                            deferred.append(self._frames.get_nowait())
                    except queue.Empty:
                        pass
                    next_tick = time.monotonic() + FRAME_SECONDS
                    finished = self._step(None)
                    if finished:
                        self._finish_segment()
                        # Replay held frames ahead of anything newer.
                        backlog = deferred + self._drain()
                        deferred = []
                        for item in backlog:
                            self._requeue(item)
                    continue
                try:
                    _, frame = self._frames.get(timeout=0.5)
                except queue.Empty:
                    continue
                self._step(frame)

    def _drain(self) -> list[tuple[int, np.ndarray]]:
        frames = []
        while True:
            try:
                frames.append(self._frames.get_nowait())
            except queue.Empty:
                return frames

    def _requeue(self, item: tuple[int, np.ndarray]) -> None:
        try:
            self._frames.put_nowait(item)
        except queue.Full:
            self._dropped += 1

    def _step(self, frame: np.ndarray | None) -> bool:
        """Run one 80 ms frame. Returns True when a requested flush finished."""
        torch = self._torch
        began = time.perf_counter()
        if frame is not None:
            codes = self.mimi.encode(torch.from_numpy(frame).to(self.device)[None, None])
            self._segment_source += 1
        elif not self._eos_fed:
            # First flush step: the end-of-stream marker Hibiki was trained on.
            codes = self.eos_codes
            self._eos_fed = True
        else:
            codes = self.mimi.encode(self.silence)
        if self._first_step:
            # As moshi.run_inference: the first slice is otherwise replaced by
            # the initial tokens and never seen by the transformer.
            self.lm_gen.step(codes)
            self._first_step = False
        tokens = self.lm_gen.step(codes)
        self._segment_steps += 1
        if frame is None:
            self._flush_steps += 1
        finished = False
        if tokens is not None:
            text_token = int(tokens[0, 0, 0].item())
            pcm = self.mimi.decode(tokens[:, 1:])[0, 0].float().clamp(-1, 1).cpu().numpy()
            piece = ""
            if text_token == self.eos_id and self._eos_fed:
                finished = True
            elif text_token == 0:
                # The text stream marks every word start with token 0. Some
                # word-initial pieces (digits especially) carry no "▁", so
                # the marker, not the piece, is where the space goes.
                self._word_start = True
            elif text_token not in self._special_ids:
                piece = self._piece(text_token)
            if piece:
                if self._word_start and self._segment_text and not piece.startswith(" "):
                    piece = " " + piece
                self._word_start = False
                self._segment_text.append(piece)
                if self.timing_fields:
                    # Opt-in only: the Go engine decodes headers strictly and
                    # rejects fields it does not know.
                    self.send(MessageType.TEXT_DELTA, text=piece, segment=self._segment,
                              step=self._segment_steps,
                              source_seconds=round(self._segment_source * FRAME_SECONDS, 3))
                else:
                    self.text_delta(piece)
            self.audio((pcm * 32767).astype("<i2").tobytes())
        self._compute.append(time.perf_counter() - began)
        if self._eos_fed and self._flush_steps >= self.max_flush_frames:
            log(f"hibiki: no EOS after {self._flush_steps} flush frames; ending the segment")
            finished = True
        return finished

    def _piece(self, token: int) -> str:
        """One token's text; byte-fallback tokens are joined into UTF-8."""
        if self.tokenizer.is_byte(token):
            self._bytes.append(int(self.tokenizer.id_to_piece(token)[3:5], 16))
            try:
                text = self._bytes.decode("utf-8")
            except UnicodeDecodeError:
                return "" if len(self._bytes) < 4 else self._flush_bytes()
            self._bytes.clear()
            return text
        return self._flush_bytes() + self.tokenizer.id_to_piece(token).replace("\u2581", " ")

    def _flush_bytes(self) -> str:
        text = self._bytes.decode("utf-8", errors="replace") if self._bytes else ""
        self._bytes.clear()
        return text

    def _finish_segment(self) -> None:
        text = " ".join("".join(self._segment_text).split())
        self.text_done(text)
        compute = np.asarray(self._compute) * 1000.0
        stats = {
            "segment": self._segment, "steps": self._segment_steps, "source_frames": self._segment_source,
            "flush_frames": self._flush_steps, "eos": self._flush_steps < self.max_flush_frames,
            "compute_ms_mean": round(float(compute.mean()), 2) if compute.size else None,
            "compute_ms_p50": round(float(np.percentile(compute, 50)), 2) if compute.size else None,
            "compute_ms_p95": round(float(np.percentile(compute, 95)), 2) if compute.size else None,
            "compute_ms_max": round(float(compute.max()), 2) if compute.size else None,
            "rtf": round(float(compute.mean()) / (FRAME_SECONDS * 1000.0), 4) if compute.size else None,
            "dropped_frames": self._dropped,
        }
        self.send(MessageType.LOG, text="hibiki-stats " + json.dumps(stats), level="info")
        with self._flush_lock:
            turn_done = self._flush_turn_done
            done = self._flush_done
            self._flush_requested.clear()
        self._segment += 1
        self._reset()
        if turn_done:
            self.turn_done()
        done.set()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repository", default=DEFAULT_REPOSITORY, help="Hibiki weights repository")
    parser.add_argument("--device", default="cuda", help="device to place the model on")
    parser.add_argument("--cfg-coef", type=float, default=1.0,
                        help="classifier-free guidance; >1 increases voice similarity (upstream default 1)")
    parser.add_argument("--seed", type=int, default=4242, help="sampling seed (upstream run_inference uses 4242)")
    parser.add_argument("--max-flush-seconds", type=float, default=16.0,
                        help="model time to wait for the text EOS after the end of a segment")
    parser.add_argument("--flush-pace", choices=["realtime", "fast"], default="realtime",
                        help="after commit/respond with no input behind it, step the silent flush every 80 ms "
                             "(as live silence would arrive) or as fast as the GPU allows")
    parser.add_argument("--timing-fields", action="store_true",
                        help="add step/source_seconds/segment to text_delta headers (evaluation harnesses only; "
                             "the Go engine rejects unknown header fields)")
    parser.add_argument("--mock", action="store_true",
                        help="speak the protocol without loading a model, for plumbing and conformance")
    arguments = parser.parse_args()
    run(HibikiSidecar, repository=arguments.repository, mock=arguments.mock, device=arguments.device,
        cfg_coef=arguments.cfg_coef, seed=arguments.seed, max_flush_seconds=arguments.max_flush_seconds,
        flush_pace=arguments.flush_pace, timing_fields=arguments.timing_fields)


if __name__ == "__main__":
    main()
