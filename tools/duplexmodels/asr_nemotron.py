#!/usr/bin/env python3
"""NVIDIA Nemotron cache-aware streaming ASR behind the duplex-plan recogniser contract.

Serves ``nvidia/nemotron-speech-streaming-en-0.6b`` (English) and
``nvidia/nemotron-3.5-asr-streaming-0.6b`` (40 locales, language-ID prompt)
through :func:`common.serve_recognizer` (start/chunk/finish + ``stable_text``).

Each session runs NeMo's cache-aware FastConformer-RNNT *incrementally*:

* Audio arrives in ~100 ms float32 16 kHz pushes. Log-mel frames (10 ms hop,
  512-point centred STFT) are computed only once every sample of their analysis
  window has arrived (frame ``i`` needs ``i*160 + 256`` samples), from a short
  audio tail, so nothing is computed over the future and nothing is recomputed.
* Every complete encoder chunk is run as soon as its last feature frame exists,
  with ``conformer_stream_step`` carrying the attention/convolution caches and
  the previous RNNT hypothesis per session. This mirrors
  ``CacheAwareStreamingAudioBuffer`` (first chunk shorter, 9-frame pre-encode
  cache afterwards, ``drop_extra_pre_encoded`` after step 0).
* ``finish`` computes the remaining frames with the same end padding an offline
  pass uses and runs the tail chunks, the last with ``keep_all_outputs``.

Greedy RNNT decoding never revises an emitted token, so the service reports the
whole decoded text as ``stable_text`` with ``stability="decoder-append-only"``.
The service does not take that on trust: every response is checked against the
previous one and any text that does not extend its predecessor is counted in
``/health`` (``append_only``) and logged; ``stable_text`` then falls back to the
common prefix, which the Go adapter reports as a withdrawal. ``--verify`` runs
the same check offline over a fixture manifest and compares the incremental
stream with NeMo's reference chunked streaming and with a whole-utterance pass.

Latency (``--att-context-size L,R``; every unit is an 80 ms encoder frame):
a chunk is ``1+R`` frames, i.e. ``(1+R)*80`` ms of audio, and it is encoded when
its last frame arrives. The oldest frame in a chunk therefore waits ``R*80`` ms
of look-ahead (mean ``R*40`` ms), plus up to one push interval (100 ms) and
16 ms of STFT half-window, plus compute. ``L`` is the left (history) context.

========================  =================  ================  =========================
model                     ``R`` (right ctx)  chunk             trained ``L,R`` values
========================  =================  ================  =========================
nemotron-speech-...-en    0 / 1 / 6 / 13     80/160/560/1120   70,13  70,6  70,1  70,0
nemotron-3.5-asr-...      0 / 3 / 6 / 13     80/320/560/1120   56,3 (default) 56,0 56,6 56,13
========================  =================  ================  =========================

(The 3.5 card also lists R=1 / 160 ms; it is not in the checkpoint's trained
list and NeMo warns when it is selected.)

Serve::

    python tools/duplexmodels/asr_nemotron.py --model nvidia/nemotron-speech-streaming-en-0.6b \
        --att-context-size 70,1 --port 9110
    python tools/duplexmodels/asr_nemotron.py --model nvidia/nemotron-3.5-asr-streaming-0.6b \
        --att-context-size 56,3 --language auto --port 9111

Verify (offline, no server)::

    python tools/duplexmodels/asr_nemotron.py --model ... --att-context-size 70,1 \
        --verify .runtime/duplex-plan/fixtures/asr-en.jsonl --verify-limit 10 --verify-out out.json
"""

from __future__ import annotations

import argparse
import copy
import json
import logging
import os
import re
import sys
import threading
import time
from pathlib import Path
from typing import Optional

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
from common import Hypothesis, Recognizer, RecognizerSession, configure_logging, serve_recognizer  # noqa: E402

log = logging.getLogger("duplexmodels.nemotron")

SAMPLE_RATE = 16_000
LANG_TAG = re.compile(r"\s*<([a-z]{2,3}-[A-Z]{2})>")
# How many feature frames before the first new frame are recomputed so that
# no new frame's analysis window reaches the tail slice's left edge (its
# zero padding and the unfiltered first pre-emphasis sample).
FEATURE_GUARD_FRAMES = 3


def _pick(value, first: bool):
    if isinstance(value, (list, tuple)):
        return value[0] if first else value[1]
    return value


class NemotronRecognizer(Recognizer):
    stability = "decoder-append-only"

    def __init__(self, model: str, att_context_size: Optional[list[int]], language: str,
                 device: str = "cuda", cuda_graphs: bool = False, matmul_precision: str = "high"):
        import torch
        from omegaconf import OmegaConf, open_dict
        import nemo.collections.asr as nemo_asr
        from nemo.collections.asr.parts.submodules.rnnt_decoding import RNNTDecodingConfig

        torch.set_grad_enabled(False)
        # NeMo's cache-aware example default ("high" allows TF32 matmuls).
        torch.set_float32_matmul_precision(matmul_precision)
        self.torch = torch
        self.device = torch.device(device)
        # Restore on the CPU and move once: restoring straight onto the GPU
        # briefly holds two copies of the weights (~5 GB peak instead of ~2.5).
        cpu = torch.device("cpu")
        if model.endswith(".nemo") and os.path.exists(model):
            asr = nemo_asr.models.ASRModel.restore_from(model, map_location=cpu)
        else:
            asr = nemo_asr.models.ASRModel.from_pretrained(model_name=model, map_location=cpu)
        self.model = model
        self.trained_contexts = [list(c) for c in asr.cfg.encoder.att_context_size]
        if att_context_size is not None:
            asr.encoder.set_default_att_context_size(att_context_size=list(att_context_size))
        asr.encoder.setup_streaming_params()
        self.att_context_size = list(asr.encoder.att_context_size)
        # Same decoding the NeMo cache-aware example uses: greedy_batch RNNT
        # (label looping). With encoder CUDA graphs the decoder's own CUDA
        # graphs are switched off: with both enabled, the decoder graph replay
        # fails with "CUDA error: an illegal memory access" on this stack
        # (NeMo 3.1.0 / torch 2.9.1 / Blackwell). Tokens are unchanged either way.
        decoding = RNNTDecodingConfig(fused_batch_size=-1)
        if cuda_graphs:
            decoding.greedy.use_cuda_graph_decoder = False
        asr.change_decoding_strategy(decoding)
        self.cuda_graphs = cuda_graphs
        self.prompted = hasattr(asr, "set_inference_prompt")
        if self.prompted:
            asr.set_inference_prompt(language)
            # Keep tags in the decoded text; the session strips them and
            # reports the detected locale as ``language``.
            asr.decoding.set_strip_lang_tags(False)
            self.language = language
        else:
            if language not in ("", "auto", "en", "en-US"):
                raise SystemExit(f"{model} is English-only; --language {language} is not available")
            self.language = "en"
        asr = asr.to(device=self.device, dtype=torch.float32).eval()
        if cuda_graphs:
            asr.encoder.set_streaming_cuda_graphs(enabled=True)
        self.asr = asr
        self.cfg = asr.encoder.streaming_cfg
        if hasattr(asr.encoder, "pre_encode") and hasattr(asr.encoder.pre_encode, "get_sampling_frames"):
            self.sampling_frames = asr.encoder.pre_encode.get_sampling_frames()
        else:
            self.sampling_frames = None

        # Feature extractor exactly as CacheAwareStreamingAudioBuffer builds it
        # (no dither, no pad_to); model normalisation must be causal.
        cfg = copy.deepcopy(asr._cfg.preprocessor)
        OmegaConf.set_struct(cfg, False)
        with open_dict(cfg):
            cfg.dither = 0.0
            cfg.pad_to = 0
        if str(cfg.get("normalize", "NA")) not in ("NA", "None", "none", ""):
            raise SystemExit(f"preprocessor normalize={cfg.normalize} needs the whole utterance; not causal")
        self.preprocessor = asr.from_config_dict(cfg).to(self.device).eval()
        featurizer = self.preprocessor.featurizer
        self.hop = int(featurizer.hop_length)
        self.n_fft = int(featurizer.n_fft)
        self.feat_dim = int(asr.encoder._feat_in)
        right = self.att_context_size[1]
        chunk_frames = _pick(self.cfg.chunk_size, False)
        self.chunk_ms = chunk_frames * 1000 * self.hop // SAMPLE_RATE
        self.lookahead_ms = right * 80
        self.stats_lock = threading.Lock()
        self.append_only = {"responses": 0, "text_changes": 0, "violations": 0, "examples": []}
        log.info("loaded %s att_context_size=%s chunk=%d ms (first %d frames) lookahead=%d ms language=%s",
                 model, self.att_context_size, self.chunk_ms, _pick(self.cfg.chunk_size, True),
                 self.lookahead_ms, self.language)
        self._warm()

    def _warm(self) -> None:
        # Long enough for several steady-state chunks, so CUDA graphs (captured
        # after three identical steps) exist before the first real session.
        rng = np.random.default_rng(0)
        chunk_s = (_pick(self.cfg.chunk_size, True) + 6 * _pick(self.cfg.chunk_size, False)) * self.hop / SAMPLE_RATE
        for _ in range(2):
            session = self.start()
            for _ in range(int(chunk_s * 10) + 2):
                session.push((rng.standard_normal(1600) * 0.01).astype(np.float32))
            session.finish()
        with self.stats_lock:
            self.append_only = {"responses": 0, "text_changes": 0, "violations": 0, "examples": []}

    def start(self) -> "NemotronSession":
        return NemotronSession(self)

    def record(self, previous: str, current: str) -> bool:
        """Count one response against the append-only claim."""
        with self.stats_lock:
            stats = self.append_only
            stats["responses"] += 1
            if current != previous:
                stats["text_changes"] += 1
            if not current.startswith(previous):
                stats["violations"] += 1
                if len(stats["examples"]) < 20:
                    stats["examples"].append({"before": previous, "after": current})
                log.warning("append-only violated: %r -> %r", previous, current)
                return False
        return True

    def health(self) -> dict:
        with self.stats_lock:
            append_only = copy.deepcopy(self.append_only)
        return {"pid": os.getpid(), "att_context_size": self.att_context_size, "trained_att_context_sizes": self.trained_contexts,
                "chunk_ms": self.chunk_ms, "lookahead_ms": self.lookahead_ms, "language": self.language,
                "cuda_graphs": self.cuda_graphs, "append_only": append_only}


class NemotronSession(RecognizerSession):
    def __init__(self, recognizer: NemotronRecognizer):
        torch = recognizer.torch
        self.r = recognizer
        self.audio = np.zeros(0, dtype=np.float32)
        self.audio_base = 0          # absolute sample index of self.audio[0]
        self.samples = 0             # absolute samples received
        self.feats = torch.zeros((1, recognizer.feat_dim, 0), device=recognizer.device)
        self.feat_base = 0           # absolute frame index of self.feats[..., 0]
        self.frames = 0              # absolute feature frames computed
        self.cursor = 0              # absolute frame index of the next encoder chunk
        self.step = 0
        with torch.inference_mode():
            self.cache = recognizer.asr.encoder.get_initial_cache_state(batch_size=1)
        self.hypotheses = None
        self.pred_out = None
        self.raw_text = ""
        self.text = ""
        self.stable = ""
        self.language = recognizer.language if recognizer.language != "auto" else ""
        self.finished = False
        self.encoder_steps = 0

    # -- features ---------------------------------------------------------
    def _extend_features(self, final: bool) -> None:
        r, torch = self.r, self.r.torch
        total = self.samples
        if final:
            target = total // r.hop  # the offline frame count (NeMo's get_seq_len)
        elif total >= r.n_fft // 2:
            target = min(total // r.hop, (total - r.n_fft // 2) // r.hop + 1)
        else:
            target = 0
        if target <= self.frames:
            return
        first = max(0, self.frames - FEATURE_GUARD_FRAMES)
        start = first * r.hop
        segment = self.audio[start - self.audio_base: total - self.audio_base]
        with torch.inference_mode():
            signal = torch.from_numpy(np.ascontiguousarray(segment)).to(r.device).unsqueeze(0)
            length = torch.tensor([segment.shape[0]], device=r.device)
            feats, _ = r.preprocessor(input_signal=signal, length=length)
            fresh = feats[:, :, self.frames - first: target - first]
            if fresh.size(-1) != target - self.frames:
                raise RuntimeError(f"feature slice {fresh.size(-1)} != {target - self.frames}")
            self.feats = torch.cat((self.feats, fresh), dim=-1)
        self.frames = target
        keep_from = max(0, self.frames - FEATURE_GUARD_FRAMES) * r.hop
        if keep_from > self.audio_base:
            self.audio = self.audio[keep_from - self.audio_base:]
            self.audio_base = keep_from

    # -- encoder ----------------------------------------------------------
    def _run_chunks(self, final: bool) -> None:
        r, torch = self.r, self.r.torch
        cfg = r.cfg
        while self.cursor < self.frames:
            first = self.step == 0
            chunk_size = _pick(cfg.chunk_size, first)
            shift = _pick(cfg.shift_size, first)
            cache_size = _pick(cfg.pre_encode_cache_size, first)
            end = self.cursor + chunk_size
            if end > self.frames and not final:
                return
            local = self.cursor - self.feat_base
            chunk = self.feats[:, :, local: local + chunk_size]
            if r.sampling_frames is not None and chunk.size(-1) < _pick(r.sampling_frames, first):
                return  # the reference buffer drops a sub-sampling-window tail too
            with torch.inference_mode():
                if first:
                    cache = torch.zeros((1, r.feat_dim, cache_size), device=r.device)
                    pad = None
                else:
                    cache = self.feats[:, :, max(0, local - cache_size): local]
                    pad = None
                    if cache.size(-1) < cache_size:
                        pad = torch.zeros((1, r.feat_dim, cache_size - cache.size(-1)), device=r.device)
                added = cache.size(-1) + (pad.size(-1) if pad is not None else 0)
                pieces = [pad, cache, chunk] if pad is not None else [cache, chunk]
                signal = torch.cat(pieces, dim=-1)
                valid = min(self.frames - self.cursor + added, signal.size(-1))
                length = torch.tensor([valid], device=r.device)
                self.cursor += shift
                keep_all = final and self.cursor >= self.frames
                (self.pred_out, texts, c1, c2, c3, self.hypotheses) = r.asr.conformer_stream_step(
                    processed_signal=signal,
                    processed_signal_length=length,
                    cache_last_channel=self.cache[0],
                    cache_last_time=self.cache[1],
                    cache_last_channel_len=self.cache[2],
                    keep_all_outputs=keep_all,
                    previous_hypotheses=self.hypotheses,
                    previous_pred_out=self.pred_out,
                    drop_extra_pre_encoded=0 if first else cfg.drop_extra_pre_encoded,
                    return_transcription=True,
                )
            self.cache = (c1, c2, c3)
            self.step += 1
            self.encoder_steps += 1
            hypothesis = texts[0]
            self.raw_text = hypothesis.text if hasattr(hypothesis, "text") else str(hypothesis)
            # Features before the next chunk's pre-encode cache are no longer needed.
            keep = max(self.feat_base, self.cursor - _pick(cfg.pre_encode_cache_size, False))
            if keep > self.feat_base:
                self.feats = self.feats[:, :, keep - self.feat_base:]
                self.feat_base = keep

    # -- contract ---------------------------------------------------------
    def _answer(self) -> Hypothesis:
        text = self.raw_text
        tags = LANG_TAG.findall(text)
        if tags:
            self.language = tags[-1]
            text = LANG_TAG.sub("", text)
        text = text.strip()
        previous = self.text
        self.text = text
        if self.r.record(previous, text):
            self.stable = text
        else:
            # Honest fallback: commit only what both texts share (the adapter
            # will see this as a withdrawal, which is what it is).
            common = os.path.commonprefix([self.stable, text])
            self.stable = common
        return Hypothesis(text=text, stable_text=self.stable, language=self.language)

    def push(self, audio: np.ndarray) -> Hypothesis:
        if self.finished:
            raise RuntimeError("session already finished")
        audio = np.asarray(audio, dtype=np.float32).reshape(-1)
        if audio.size:
            self.audio = np.concatenate((self.audio, audio))
            self.samples += audio.size
            self._extend_features(final=False)
            self._run_chunks(final=False)
        return self._answer()

    def finish(self) -> Hypothesis:
        if not self.finished:
            self.finished = True
            self._extend_features(final=True)
            self._run_chunks(final=True)
        return self._answer()

    def close(self) -> None:
        self.cache = None
        self.hypotheses = None
        self.feats = None


# --------------------------------------------------------------------------
# Offline verification: append-only property and streaming/offline equivalence


def _load_fixture(path: str) -> np.ndarray:
    from scipy.signal import resample_poly
    pcm = np.fromfile(path, dtype="<i2").astype(np.float32) / 32768.0
    return resample_poly(pcm, 2, 3).astype(np.float32)  # 24 kHz -> 16 kHz


def _words(text: str) -> list[str]:
    return re.sub(r"[^\w' ]+", " ", text.lower()).split()


def _edit(a: list, b: list) -> int:
    previous = list(range(len(b) + 1))
    for i in range(1, len(a) + 1):
        current = [i] + [0] * len(b)
        for j in range(1, len(b) + 1):
            current[j] = min(previous[j] + 1, current[j - 1] + 1, previous[j - 1] + (a[i - 1] != b[j - 1]))
        previous = current
    return previous[len(b)]


def verify(recognizer: NemotronRecognizer, manifest: str, limit: int, push_ms: int, out: Optional[str]) -> dict:
    """Stream fixtures push by push; compare with NeMo's reference paths."""
    torch = recognizer.torch
    from nemo.collections.asr.parts.utils.streaming_utils import CacheAwareStreamingAudioBuffer

    asr = recognizer.asr
    reference_buffer = CacheAwareStreamingAudioBuffer(model=asr, online_normalization=False)
    entries = [json.loads(line) for line in Path(manifest).read_text().splitlines() if line.strip()]
    entries = entries[:limit] if limit else entries
    push = SAMPLE_RATE * push_ms // 1000
    rows = []
    totals = {"responses": 0, "text_changes": 0, "violations": 0, "stream_vs_reference_text_mismatch": 0,
              "stream_vs_reference_token_mismatch": 0, "stream_vs_offline_text_mismatch": 0,
              "stream_vs_offline_token_mismatch": 0, "ref_words": 0,
              "stream_word_errors": 0, "offline_word_errors": 0, "audio_s": 0.0, "compute_s": 0.0}

    def strip(text: str) -> str:
        return LANG_TAG.sub("", text).strip()

    for entry in entries:
        audio = _load_fixture(entry["pcm"])
        session = recognizer.start()
        texts, violations = [], []
        began = time.perf_counter()
        for offset in range(0, audio.size, push):
            before = session.text
            hypothesis = session.push(audio[offset: offset + push])
            texts.append(hypothesis.text)
            if not hypothesis.text.startswith(before):
                violations.append({"at_ms": (offset + push) * 1000 // SAMPLE_RATE, "before": before,
                                   "after": hypothesis.text})
        final = session.finish()
        compute = time.perf_counter() - began
        stream_tokens = [int(t) for t in session.hypotheses[0].y_sequence] if session.hypotheses else []
        stream_pred = session.pred_out[0] if session.pred_out else None

        # (a) NeMo's reference chunked streaming over the whole-utterance features.
        with torch.inference_mode():
            reference_buffer.reset_buffer()
            reference_buffer.append_audio(audio, stream_id=-1)
            c1, c2, c3 = asr.encoder.get_initial_cache_state(batch_size=1)
            hyps, pred = None, None
            ref_texts = None
            for step, (chunk, lengths) in enumerate(iter(reference_buffer)):
                (pred, ref_texts, c1, c2, c3, hyps) = asr.conformer_stream_step(
                    processed_signal=chunk, processed_signal_length=lengths, cache_last_channel=c1,
                    cache_last_time=c2, cache_last_channel_len=c3,
                    keep_all_outputs=reference_buffer.is_buffer_empty(), previous_hypotheses=hyps,
                    previous_pred_out=pred,
                    drop_extra_pre_encoded=0 if step == 0 else asr.encoder.streaming_cfg.drop_extra_pre_encoded,
                    return_transcription=True)
            reference_text = strip(ref_texts[0].text) if ref_texts else ""
            reference_tokens = [int(t) for t in hyps[0].y_sequence] if hyps else []
            # (b) One whole-utterance pass (the "offline" comparison NeMo's example makes).
            signal, lengths = reference_buffer.get_all_audios()
            (pred_off, off_texts, *_rest) = asr.conformer_stream_step(
                processed_signal=signal, processed_signal_length=lengths, return_transcription=True)
            offline_text = strip(off_texts[0].text)
            offline_tokens = [int(t) for t in off_texts[0].y_sequence]
        # Features: incremental vs whole-utterance, same frames.
        reference = entry.get("text", "")
        stream_errors = _edit(_words(reference), _words(final.text))
        offline_errors = _edit(_words(reference), _words(offline_text))
        row = {"id": entry["id"], "audio_s": round(audio.size / SAMPLE_RATE, 3), "reference": reference,
               "stream": final.text, "nemo_reference_stream": reference_text, "offline": offline_text,
               "stream_equals_reference": final.text == reference_text and stream_tokens == reference_tokens,
               "stream_equals_offline_text": final.text == offline_text,
               "stream_equals_offline_tokens": stream_tokens == offline_tokens,
               "pushes": len(texts), "text_changes": sum(1 for a, b in zip([""] + texts, texts) if a != b),
               "violations": violations, "stream_word_errors": stream_errors,
               "offline_word_errors": offline_errors, "ref_words": len(_words(reference)),
               "encoder_steps": session.encoder_steps, "compute_s": round(compute, 3),
               "language": final.language}
        rows.append(row)
        totals["responses"] += len(texts) + 1
        totals["text_changes"] += row["text_changes"]
        totals["violations"] += len(violations)
        totals["stream_vs_reference_text_mismatch"] += final.text != reference_text
        totals["stream_vs_reference_token_mismatch"] += stream_tokens != reference_tokens
        totals["stream_vs_offline_text_mismatch"] += final.text != offline_text
        totals["stream_vs_offline_token_mismatch"] += stream_tokens != offline_tokens
        totals["ref_words"] += row["ref_words"]
        totals["stream_word_errors"] += stream_errors
        totals["offline_word_errors"] += offline_errors
        totals["audio_s"] += audio.size / SAMPLE_RATE
        totals["compute_s"] += compute
        log.info("%s violations=%d ref=%s off=%s %r", entry["id"], len(violations),
                 row["stream_equals_reference"], row["stream_equals_offline_tokens"], final.text)
    totals["utterances"] = len(rows)
    totals["rtf_single_stream"] = round(totals["compute_s"] / max(totals["audio_s"], 1e-9), 4)
    totals["stream_wer_rough"] = round(totals["stream_word_errors"] / max(1, totals["ref_words"]), 4)
    totals["offline_wer_rough"] = round(totals["offline_word_errors"] / max(1, totals["ref_words"]), 4)
    report = {"model": recognizer.model, "att_context_size": recognizer.att_context_size,
              "chunk_ms": recognizer.chunk_ms, "language": recognizer.language, "push_ms": push_ms,
              "manifest": manifest, "summary": totals, "utterances": rows,
              "note": "WER here is a rough whitespace/punctuation-stripped count; asrbench is the scored metric."}
    if out:
        Path(out).parent.mkdir(parents=True, exist_ok=True)
        Path(out).write_text(json.dumps(report, indent=2, ensure_ascii=False))
    print(json.dumps(totals, indent=2))
    return report


def patch_scaffold_annotations() -> None:
    """Work around a scaffold defect (FastAPI 0.141 + postponed annotations).

    ``common.py`` has ``from __future__ import annotations`` and imports
    ``Request`` inside ``serve_recognizer``; FastAPI resolves the string
    annotation against the module globals, fails, and treats ``request`` as a
    required query parameter, so every ``/api/chunk`` answers HTTP 422.
    Publishing the names in ``common``'s globals restores the intended route.
    """
    import common  # noqa: PLC0415
    import fastapi  # noqa: PLC0415

    for name in ("Request", "WebSocket"):
        if not hasattr(common, name):
            setattr(common, name, getattr(fastapi, name))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--model", default="nvidia/nemotron-speech-streaming-en-0.6b",
                        help="Hugging Face model id or local .nemo path")
    parser.add_argument("--att-context-size", default="",
                        help="left,right attention context in 80 ms frames (e.g. 70,1 or 56,6); "
                             "default: the checkpoint's first trained value")
    parser.add_argument("--language", default="auto",
                        help="language-ID prompt for prompt-conditioned models: auto, en-US, zh-CN, ...")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9110)
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--cuda-graphs", action="store_true",
                        help="capture the steady-state encoder stream step in CUDA graphs (decoder graphs off)")
    parser.add_argument("--matmul-precision", default="high", choices=["highest", "high", "medium"])
    parser.add_argument("--verify", default="", help="fixture manifest: run the offline checks instead of serving")
    parser.add_argument("--verify-limit", type=int, default=10)
    parser.add_argument("--verify-push-ms", type=int, default=100)
    parser.add_argument("--verify-out", default="")
    args = parser.parse_args()
    configure_logging()
    os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")
    context = [int(value) for value in args.att_context_size.split(",")] if args.att_context_size else None
    recognizer = NemotronRecognizer(args.model, context, args.language, args.device, args.cuda_graphs,
                                    args.matmul_precision)
    if args.verify:
        verify(recognizer, args.verify, args.verify_limit, args.verify_push_ms, args.verify_out or None)
        return
    patch_scaffold_annotations()
    serve_recognizer(recognizer, args.host, args.port)


if __name__ == "__main__":
    main()
