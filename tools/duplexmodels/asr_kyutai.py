#!/usr/bin/env python3
"""Kyutai delayed-streams STT behind the duplex-plan recogniser contract.

Serves ``kyutai/stt-1b-en_fr`` (English and French; ~0.5 s text delay) through
:func:`common.serve_recognizer` (start/chunk/finish + ``stable_text``).

How a session runs - truly frame by frame, nothing recomputed:

* Audio arrives as float32 16 kHz pushes (~100 ms from the Go adapter). Each
  session owns a streaming soxr resampler to Mimi's 24 kHz, so no window is
  resampled twice and no future sample is needed.
* Every complete 80 ms Mimi frame (1920 samples) is encoded by the streaming
  Mimi encoder (32 codebooks) and fed to one ``LMGen.step``; the model emits
  exactly one text token per frame. Mimi's convolution/transformer state and
  the LM's KV cache are carried across pushes - a push never re-reads audio.
  (The stateless residual quantizer is additionally captured in a CUDA graph.)
* Sessions share one batched streaming state (``--slots`` rows). A session owns
  one row; ``exec_mask`` selects the rows that run a step, and ``reset_streaming``
  clears only the row a new session takes. The scaffold serialises model calls,
  so one push steps one row; the batch exists to carry many sessions' state
  side by side, and costs no more per step than batch 1 on this host (the step
  is launch-bound: ~8 ms at batch 1 and at batch 8, i.e. ~0.1 of real time).
* ``finish`` zero-pads the last partial frame and then feeds
  ``ceil(audio_delay * 12.5) + 1`` frames of silence (0.64 s for the 0.5 s
  model) so the delayed text for the final words is emitted.

Text tokens: ``3`` is padding and ``0`` ("end of padding") precedes the start
of a word. Sampling is greedy and a token, once emitted, is never revised, so
the text is append-only by construction. ``stable_text`` is the text up to the
last *completed* word: a word is complete once a later token starts a new word
(sentencepiece ``▁``) or an end-of-padding marker follows it. The in-progress
word (which can still grow, e.g. ``consum`` -> ``consumption``) is left in the
unstable suffix, and at ``finish`` everything is stable. Stability is reported
as ``decoder-append-only``. The service checks the claim on every response: a
hypothesis that does not extend its predecessor, or a ``stable_text`` that does
not extend the previous one, is counted in ``/health`` (``append_only``) and
logged, and ``stable_text`` then falls back to the common prefix. ``--verify``
runs the same checks offline over a fixture manifest, pushing 100 ms chunks.

Language: the model transcribes English and French (it does not report which);
it has no Mandarin support.

Serve::

    python tools/duplexmodels/asr_kyutai.py --port 9112

Verify (offline, no server)::

    python tools/duplexmodels/asr_kyutai.py \
        --verify .runtime/duplex-plan/fixtures/asr-en.jsonl --verify-limit 10
"""

from __future__ import annotations

import argparse
import json
import logging
import math
import sys
import threading
import time
from collections import deque
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import Hypothesis, Recognizer, RecognizerSession, configure_logging, serve_recognizer  # noqa: E402

log = logging.getLogger("asr_kyutai")

DEFAULT_REPOSITORY = "kyutai/stt-1b-en_fr"
INPUT_RATE = 16_000
PAD_TOKEN = 3
END_OF_PADDING = 0


def _common_prefix(a: str, b: str) -> str:
    length = 0
    for left, right in zip(a, b):
        if left != right:
            break
        length += 1
    return a[:length]


class KyutaiSession(RecognizerSession):
    def __init__(self, recognizer: "KyutaiRecognizer", slot: int) -> None:
        import soxr  # noqa: PLC0415

        self.recognizer = recognizer
        self.slot = slot
        self.resampler = soxr.ResampleStream(INPUT_RATE, recognizer.sample_rate, 1, dtype="float32")
        self.pending = np.zeros(0, dtype=np.float32)
        #: Emitted text tokens (pads dropped) and whether each starts a word.
        self.tokens: list[int] = []
        #: Index into ``tokens`` below which every word is complete.
        self.complete = 0
        self.frames = 0
        self.last_text = ""
        self.last_stable = ""
        self.closed = False
        self.evicted = False
        self.touched = time.monotonic()

    # -- model steps ---------------------------------------------------------

    def _consume(self, audio24: np.ndarray) -> None:
        if audio24.size:
            self.pending = np.concatenate([self.pending, audio24.astype(np.float32, copy=False)])
        frame = self.recognizer.frame_size
        count = self.pending.size // frame
        if count == 0:
            return
        block = self.pending[: count * frame]
        self.pending = self.pending[count * frame:]
        began = time.perf_counter()
        for token in self.recognizer.step(self.slot, block):
            self._token(token)
        elapsed = time.perf_counter() - began
        if elapsed > 0.5:
            # A step that takes longer than a few frames of audio means the
            # stream is falling behind real time; say so where it happened.
            self.recognizer.counters["slow_pushes"] += 1
            log.warning("slot %d: %d frames took %.2f s (%.0f ms/frame)", self.slot, count, elapsed,
                        1000 * elapsed / count)

    def _token(self, token: int) -> None:
        self.frames += 1
        if token == PAD_TOKEN or token < 0:
            return
        if token == END_OF_PADDING:
            # The next token starts a word: everything so far is complete.
            self.complete = len(self.tokens)
            return
        if self.recognizer.starts_word[token]:
            self.complete = len(self.tokens)
        self.tokens.append(token)

    # -- hypotheses ------------------------------------------------------------

    def _hypothesis(self, final: bool) -> Hypothesis:
        decode = self.recognizer.tokenizer.decode
        text = decode(self.tokens).strip()
        stable = text if final else decode(self.tokens[: self.complete]).strip()
        counters = self.recognizer.counters
        counters["responses"] += 1
        if not text.startswith(self.last_text):
            counters["text_regressions"] += 1
            log.warning("text regressed: %r -> %r", self.last_text, text)
        if not text.startswith(stable):
            counters["stable_not_prefix"] += 1
            log.warning("stable %r is not a prefix of %r", stable, text)
            stable = _common_prefix(stable, text)
        if not stable.startswith(self.last_stable):
            counters["stable_withdrawals"] += 1
            log.warning("stable withdrew: %r -> %r", self.last_stable, stable)
            stable = _common_prefix(stable, self.last_stable)
            if not text.startswith(stable):
                stable = _common_prefix(stable, text)
        self.last_text = text
        self.last_stable = stable
        return Hypothesis(text=text, stable_text=stable, language="")

    def _check(self) -> None:
        if self.evicted:
            raise RuntimeError("session was evicted: every streaming slot was taken and it was the idlest")
        if self.closed:
            raise RuntimeError("session is finished")
        self.touched = time.monotonic()

    def push(self, audio: np.ndarray) -> Hypothesis:
        self._check()
        audio24 = self.resampler.resample_chunk(np.asarray(audio, dtype=np.float32), last=False)
        self._consume(audio24)
        return self._hypothesis(final=False)

    def finish(self) -> Hypothesis:
        self._check()
        tail = self.resampler.resample_chunk(np.zeros(0, dtype=np.float32), last=True)
        frame = self.recognizer.frame_size
        audio = np.concatenate([self.pending, tail.astype(np.float32, copy=False)])
        self.pending = np.zeros(0, dtype=np.float32)
        remainder = (-audio.size) % frame
        flush = np.zeros(remainder + self.recognizer.flush_frames * frame, dtype=np.float32)
        self._consume(np.concatenate([audio, flush]))
        return self._hypothesis(final=True)

    def close(self) -> None:
        if not self.closed:
            self.closed = True
            self.recognizer.release(self.slot, self)


class KyutaiRecognizer(Recognizer):
    stability = "decoder-append-only"

    def __init__(self, repository: str, device: str, slots: int, flush_frames: int | None) -> None:
        import torch  # noqa: PLC0415
        from moshi.models import LMGen, loaders  # noqa: PLC0415

        self.torch = torch
        self.model = repository
        self.device = device
        info = loaders.CheckpointInfo.from_hf_repo(repository)
        self.mimi = info.get_mimi(device=device)
        self.tokenizer = info.get_text_tokenizer()
        self.lm = info.get_moshi(device=device, dtype=torch.bfloat16)
        self.lm_gen = LMGen(self.lm, temp=0.0, temp_text=0.0)
        # Mimi graphs its encoder and transformer but not the 32-level residual
        # quantizer, which is ~6 ms of small kernels per step on this host. It
        # is stateless and always sees the same [slots, D, 1] shape here, so
        # it is safe to capture as well (~1 ms).
        from moshi.utils.compile import CUDAGraphed  # noqa: PLC0415
        self.mimi.quantizer.encode = CUDAGraphed(self.mimi.quantizer.encode, disable=not str(device).startswith("cuda"))
        self.sample_rate = int(self.mimi.sample_rate)
        self.frame_size = int(self.mimi.frame_size)
        self.frame_rate = float(self.mimi.frame_rate)
        self.audio_delay = float(info.stt_config.get("audio_delay_seconds", 0.5))
        self.flush_frames = flush_frames if flush_frames is not None else math.ceil(self.audio_delay * self.frame_rate) + 1
        self.slots = slots
        vocabulary = self.tokenizer.get_piece_size()
        self.starts_word = [self.tokenizer.id_to_piece(i).startswith("▁") for i in range(vocabulary)]
        self.free = deque(range(slots))
        self.slot_lock = threading.Lock()
        self.owners: dict[int, KyutaiSession] = {}
        self.counters = {"responses": 0, "text_regressions": 0, "stable_not_prefix": 0, "stable_withdrawals": 0,
                         "evictions": 0, "slow_pushes": 0}
        self.step_ms: deque[float] = deque(maxlen=2000)
        self.frames = 0
        with torch.no_grad():
            self.mimi.streaming_forever(slots)
            self.lm_gen.streaming_forever(slots)
            self._warmup()
        log.info("loaded %s: %d slots, %.0f ms frames, delay %.2f s, flush %d frames",
                 repository, slots, 1000 / self.frame_rate, self.audio_delay, self.flush_frames)

    def _mask(self, slot: int):
        mask = self.torch.zeros(self.slots, dtype=self.torch.bool, device=self.device)
        mask[slot] = True
        return mask

    def _warmup(self) -> None:
        # Captures the LM's CUDA graphs and exercises every row once.
        everyone = self.torch.ones(self.slots, dtype=self.torch.bool, device=self.device)
        self.mimi.set_exec_mask(everyone)
        self.lm_gen.set_exec_mask(everyone)
        silence = self.torch.zeros(self.slots, 1, self.frame_size, device=self.device)
        for _ in range(4):
            codes = self.mimi.encode(silence)
            self.lm_gen.step(codes)
        self.mimi.reset_streaming(everyone)
        self.lm_gen.reset_streaming(everyone)

    def start(self) -> RecognizerSession:
        with self.slot_lock:
            if self.free:
                slot = self.free.popleft()
            else:
                # Every row is taken. A client that abandoned its session
                # (never called finish) would otherwise hold a row until the
                # scaffold's 10-minute expiry, so the idlest session gives its
                # row up and fails loudly on its next call.
                victim = min(self.owners.values(), key=lambda session: session.touched)
                victim.evicted = True
                victim.closed = True
                slot = victim.slot
                self.counters["evictions"] += 1
                log.warning("evicted idle session on slot %d (idle %.1f s)", slot, time.monotonic() - victim.touched)
            session = KyutaiSession(self, slot)
            self.owners[slot] = session
        mask = self._mask(slot)
        with self.torch.no_grad():
            self.mimi.reset_streaming(mask)
            self.lm_gen.reset_streaming(mask)
        return session

    def release(self, slot: int, session: "KyutaiSession") -> None:
        with self.slot_lock:
            if self.owners.get(slot) is session:
                del self.owners[slot]
                self.free.append(slot)

    def step(self, slot: int, block: np.ndarray) -> list[int]:
        """Run one Mimi encode + LM step per 80 ms frame of ``block`` for ``slot``."""
        torch = self.torch
        mask = self._mask(slot)
        tokens: list[int] = []
        frames = block.size // self.frame_size
        with torch.no_grad():
            self.mimi.set_exec_mask(mask)
            self.lm_gen.set_exec_mask(mask)
            batch = torch.zeros(self.slots, 1, self.frame_size, device=self.device)
            for index in range(frames):
                began = time.perf_counter()
                frame = block[index * self.frame_size:(index + 1) * self.frame_size]
                batch.zero_()
                batch[slot, 0] = torch.from_numpy(frame).to(self.device)
                codes = self.mimi.encode(batch)
                out = self.lm_gen.step(codes)
                token = -1 if out is None else int(out[slot, 0, 0].item())
                tokens.append(token)
                self.step_ms.append((time.perf_counter() - began) * 1000)
                self.frames += 1
        return tokens

    def health(self) -> dict:
        steps = sorted(self.step_ms)
        p50 = steps[len(steps) // 2] if steps else None
        p95 = steps[int(len(steps) * 0.95)] if steps else None
        frame_ms = 1000 / self.frame_rate
        return {
            "languages": ["en", "fr"],
            "sample_rate": self.sample_rate,
            "frame_ms": frame_ms,
            "text_delay_s": self.audio_delay,
            "flush_frames": self.flush_frames,
            "slots": self.slots,
            "slots_free": len(self.free),
            "frames": self.frames,
            "step_ms_p50": p50,
            "step_ms_p95": p95,
            "rtf_per_stream_p50": (p50 / frame_ms) if p50 else None,
            "append_only": dict(self.counters),
        }


def verify(recognizer: KyutaiRecognizer, manifest: str, limit: int, push_ms: int, out: str) -> None:
    """Replay fixtures in ``push_ms`` pushes (no wall-clock pacing) and check the stream."""
    import soxr  # noqa: PLC0415

    rows = [json.loads(line) for line in Path(manifest).read_text().splitlines() if line.strip()][:limit]
    records = []
    push = INPUT_RATE * push_ms // 1000
    for row in rows:
        pcm24 = np.fromfile(row["pcm"], dtype="<i2").astype(np.float32) / 32768.0
        audio = soxr.resample(pcm24, 24_000, INPUT_RATE).astype(np.float32)
        session = recognizer.start()
        began = time.perf_counter()
        trace = []
        try:
            for offset in range(0, audio.size, push):
                hypothesis = session.push(audio[offset:offset + push])
                trace.append((round((offset + push) / INPUT_RATE, 2), hypothesis.stable_text, hypothesis.text))
            final = session.finish()
        finally:
            session.close()
        elapsed = time.perf_counter() - began
        records.append({"id": row["id"], "reference": row["text"], "final": final.text,
                        "audio_s": round(audio.size / INPUT_RATE, 2), "compute_s": round(elapsed, 3),
                        "trace_tail": trace[-3:]})
        print(f"{row['id']}: {elapsed:.2f}s for {audio.size / INPUT_RATE:.1f}s | {final.text}", flush=True)
    summary = {"model": recognizer.model, "counters": recognizer.counters, "health": recognizer.health(),
               "utterances": records}
    print(json.dumps({"counters": recognizer.counters, "step_ms_p50": recognizer.health()["step_ms_p50"]}))
    if out:
        Path(out).write_text(json.dumps(summary, indent=2, ensure_ascii=False))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--model", default=DEFAULT_REPOSITORY)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9112)
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--slots", type=int, default=8, help="concurrent sessions (rows of the batched state)")
    parser.add_argument("--flush-frames", type=int, default=None,
                        help="silent frames fed at finish (default: ceil(delay*12.5)+1)")
    parser.add_argument("--verify", default="", help="fixture manifest: run the offline checks instead of serving")
    parser.add_argument("--verify-limit", type=int, default=10)
    parser.add_argument("--verify-push-ms", type=int, default=100)
    parser.add_argument("--verify-out", default="")
    args = parser.parse_args()
    configure_logging()
    recognizer = KyutaiRecognizer(args.model, args.device, args.slots, args.flush_frames)
    if args.verify:
        verify(recognizer, args.verify, args.verify_limit, args.verify_push_ms, args.verify_out)
        return
    serve_recognizer(recognizer, args.host, args.port)


if __name__ == "__main__":
    main()
