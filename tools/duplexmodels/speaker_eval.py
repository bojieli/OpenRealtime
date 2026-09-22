#!/usr/bin/env python3
"""Cell S0: speaker attribution under overlap.

Compares, on controlled LibriSpeech test-clean conversations with a declared
overlap ratio,

(a) **mixed**: ordinary single-stream recognition of the mixture with Nemotron
    Speech Streaming en 0.6B (the incremental cache-aware path from
    ``asr_nemotron.py``), which has no notion of speakers; and
(b) **multitalker**: Streaming Sortformer 4spk v2.1 driving one Multitalker
    Parakeet Streaming 0.6B instance per active speaker, run exactly as NeMo's
    ``examples/asr/asr_cache_aware_streaming/speech_to_text_multitalker_streaming_infer.py``
    runs it (parallel speaker strategy, speaker-kernel injection, cache gating,
    Sortformer chunk = ASR chunk, no diarizer right context).

Subcommands::

    build        write mixtures (16 kHz wav), RTTM and per-speaker reference SegLST
    mixed        run system (a) over a manifest and score it
    multitalker  run system (b) over a manifest and score it
    summarize    print a markdown table from result JSON files

Mixtures. Each conversation has K speakers (2 or 4), two utterances per speaker
(3-12 s, leading/trailing silence trimmed at 40 dB), taken in turn order with no
speaker speaking twice in a row. Consecutive turns overlap: the next talker
starts before the previous one stops. Per-transition overlaps are proportional
to the shorter of the two turns, capped at 45% of it (so at most two people talk
at once), and scaled so that overlapped time / speech time hits the declared
ratio. Each speaker gets a random gain in [-2, +2] dB around -23 dBFS RMS.
Ground truth is the turn intervals (RTTM) and the LibriSpeech transcript of each
turn (SegLST); pauses inside an utterance count as speech in the RTTM.

Metrics.

* cpWER (meeteval): concatenated minimum-permutation WER over speakers; the
  mixed system is one hypothesis stream, so every other reference speaker is
  all deletions - this is the cost of having no attribution.
* ORC-WER (meeteval): recognition quality with attribution ignored (each
  reference utterance is assigned to whichever hypothesis stream fits best).
* DER: frame-level (10 ms), optimal one-to-one speaker mapping (Hungarian),
  overlap scored (not excluded), reported at collar 0.25 s (standard) and 0 s.
  Hypothesis = Sortformer's streamed per-80 ms speaker probabilities > 0.5.
* Speaker count: speakers with >= 0.5 s of predicted activity vs K.
* Attribution lag, per reference turn onset t0 (mapped speaker): the first
  80 ms frame in [t0-0.25 s, turn end] where the mapped speaker is active; its
  *detection offset* (frame time - t0) and its *availability lag*: the audio
  time at the end of the streaming step that committed it, plus that step's
  measured compute, minus t0. First-attributed-word lag: the same availability
  measure for the first step after t0 at which the mapped speaker's transcript
  grows.
* Compute: RTF (wall processing / audio), per-step ms, torch peak allocated VRAM.

Run in the NeMo venv, e.g.::

    python tools/duplexmodels/speaker_eval.py build --out .runtime/duplex-plan/results/speaker/mixtures
    python tools/duplexmodels/speaker_eval.py multitalker --manifest .../mixtures/2spk-ov25.jsonl \
        --att-context-size 70,13 --out .runtime/duplex-plan/results/speaker/multitalker-2spk-ov25-c13.json
"""

from __future__ import annotations

import argparse
import importlib.util
import io
import json
import logging
import math
import os
import random
import re
import sys
import time
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
NEMO_SRC = REPO / ".runtime/duplex-plan/src/Speech"
LIBRI = REPO / ".runtime/duplex-plan/data/libri/clean/test/0000.parquet"
RATE = 16_000
FRAME = 0.08  # Sortformer / ASR output frame
log = logging.getLogger("speaker_eval")


# --------------------------------------------------------------------------
# Mixture construction


def build(args) -> None:
    import pyarrow.parquet as pq
    import soundfile as sf
    import librosa

    rows = pq.read_table(args.libri).to_pylist()
    by_speaker: dict[int, list] = {}
    for row in rows:
        by_speaker.setdefault(row["speaker_id"], []).append(row)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    def load(row):
        audio, rate = sf.read(io.BytesIO(row["audio"]["bytes"]), dtype="float32")
        assert rate == RATE, rate
        trimmed, _ = librosa.effects.trim(audio, top_db=40)
        return trimmed

    cache: dict[str, np.ndarray] = {}
    for condition in args.conditions.split(","):
        speakers_per_mix, ratio = condition.split(":")
        k, ratio = int(speakers_per_mix), float(ratio)
        name = f"{k}spk-ov{round(ratio * 100)}"
        rng = random.Random(f"{args.seed}-{name}")
        manifest = out / f"{name}.jsonl"
        lines = []
        for index in range(args.count):
            session = f"{name}-{index:02d}"
            speakers = rng.sample(sorted(by_speaker), k)
            turns = {}
            for speaker in speakers:
                pool = [r for r in by_speaker[speaker]]
                rng.shuffle(pool)
                chosen = []
                for row in pool:
                    if row["id"] not in cache:
                        cache[row["id"]] = load(row)
                    seconds = cache[row["id"]].size / RATE
                    if 3.0 <= seconds <= 12.0:
                        chosen.append(row)
                    if len(chosen) == 2:
                        break
                turns[speaker] = chosen
            order = speakers[:]
            rng.shuffle(order)
            second = order[:]
            while True:
                rng.shuffle(second)
                if k == 2:
                    second = order[:]
                if second[0] != order[-1]:
                    break
            sequence = [(s, turns[s][0]) for s in order] + [(s, turns[s][1]) for s in second]
            gains = {s: 10 ** (rng.uniform(-2, 2) / 20) for s in speakers}
            lengths = [cache[row["id"]].size / RATE for _, row in sequence]
            shorter = [min(a, b) for a, b in zip(lengths, lengths[1:])]
            wanted = ratio / (1 + ratio) * sum(lengths)
            scale = wanted / sum(shorter) if shorter else 0.0
            overlaps = [min(0.45 * s, scale * s) for s in shorter]
            lead = 0.5
            starts = [lead]
            for length, overlap in zip(lengths, overlaps):
                starts.append(starts[-1] + length - overlap)
            total = starts[len(lengths) - 1] + lengths[-1] + 1.0
            mix = np.zeros(int(math.ceil(total * RATE)) + 1, dtype=np.float32)
            reference, rttm = [], []
            for (speaker, row), start, length in zip(sequence, starts, lengths):
                audio = cache[row["id"]]
                rms = float(np.sqrt(np.mean(audio ** 2)) + 1e-9)
                scaled = audio * (10 ** (-23 / 20) / rms) * gains[speaker]
                offset = int(round(start * RATE))
                mix[offset: offset + scaled.size] += scaled
                reference.append({"session_id": session, "speaker": str(speaker), "start_time": round(start, 3),
                                  "end_time": round(start + length, 3), "words": row["text"], "utterance": row["id"]})
                rttm.append(f"SPEAKER {session} 1 {start:.3f} {length:.3f} <NA> <NA> {speaker} <NA> <NA>")
            peak = float(np.max(np.abs(mix)))
            if peak > 0.99:
                mix *= 0.99 / peak
            wav = out / f"{session}.wav"
            sf.write(wav, mix, RATE, subtype="PCM_16")
            (out / f"{session}.rttm").write_text("\n".join(rttm) + "\n")
            (out / f"{session}.seglst.json").write_text(json.dumps(reference, indent=1))
            actual = overlap_ratio(reference)
            lines.append(json.dumps({"session_id": session, "audio_filepath": str(wav.resolve()),
                                     "rttm_filepath": str((out / f"{session}.rttm").resolve()),
                                     "seglst_filepath": str((out / f"{session}.seglst.json").resolve()),
                                     "duration": round(mix.size / RATE, 3), "num_speakers": k,
                                     "declared_overlap_ratio": ratio, "overlap_ratio": round(actual, 4),
                                     "offset": 0}))
        manifest.write_text("\n".join(lines) + "\n")
        ratios = [json.loads(line)["overlap_ratio"] for line in lines]
        durations = [json.loads(line)["duration"] for line in lines]
        print(f"{name}: {len(lines)} mixtures, overlap ratio mean {np.mean(ratios):.3f} "
              f"[{min(ratios):.3f}, {max(ratios):.3f}], {sum(durations) / 60:.1f} min audio")


def overlap_ratio(reference: list[dict], step: float = 0.01) -> float:
    end = max(seg["end_time"] for seg in reference)
    count = np.zeros(int(end / step) + 2, dtype=np.int32)
    for seg in reference:
        count[int(round(seg["start_time"] / step)): int(round(seg["end_time"] / step))] += 1
    speech = np.sum(count >= 1)
    return float(np.sum(count >= 2) / speech) if speech else 0.0


# --------------------------------------------------------------------------
# Scoring


def normalise(text: str) -> str:
    return " ".join(re.sub(r"[^a-z0-9' ]+", " ", text.lower()).split())


def wer_scores(reference: list[dict], hypothesis: list[dict]) -> dict:
    from meeteval.io import SegLST
    from meeteval.wer import api

    def clean(segments):
        return SegLST([{**{k: v for k, v in seg.items() if k in ("session_id", "speaker", "start_time", "end_time")},
                        "words": normalise(seg["words"])} for seg in segments])

    ref, hyp = clean(reference), clean(hypothesis)
    if not len(hyp):
        hyp = SegLST([{"session_id": reference[0]["session_id"], "speaker": "none", "start_time": 0.0,
                       "end_time": 0.0, "words": ""}])
    cp = list(api.cpwer(ref, hyp).values())[0]
    orc = list(api.orcwer(ref, hyp).values())[0]
    return {"cp_errors": cp.errors, "cp_length": cp.length, "cp_insertions": cp.insertions,
            "cp_deletions": cp.deletions, "cp_substitutions": cp.substitutions,
            "cp_missed_speaker": cp.missed_speaker, "cp_falarm_speaker": cp.falarm_speaker,
            "orc_errors": orc.errors, "orc_length": orc.length}


def reference_matrix(reference: list[dict], speakers: list[str], frames: int, step: float) -> np.ndarray:
    matrix = np.zeros((frames, len(speakers)), dtype=bool)
    for seg in reference:
        column = speakers.index(seg["speaker"])
        matrix[int(round(seg["start_time"] / step)): int(round(seg["end_time"] / step)), column] = True
    return matrix


def der(reference: list[dict], hyp_frames: np.ndarray, collar: float, step: float = 0.01) -> dict:
    """Frame DER. ``hyp_frames``: (T80, S) boolean at 80 ms."""
    from scipy.optimize import linear_sum_assignment

    speakers = sorted({seg["speaker"] for seg in reference})
    end = max(max(seg["end_time"] for seg in reference), hyp_frames.shape[0] * FRAME)
    frames = int(math.ceil(end / step)) + 1
    ref = reference_matrix(reference, speakers, frames, step)
    factor = int(round(FRAME / step))
    hyp = np.repeat(hyp_frames, factor, axis=0)[:frames]
    if hyp.shape[0] < frames:
        hyp = np.pad(hyp, ((0, frames - hyp.shape[0]), (0, 0)))
    scored = np.ones(frames, dtype=bool)
    if collar > 0:
        width = int(round(collar / step))
        for seg in reference:
            for edge in (seg["start_time"], seg["end_time"]):
                centre = int(round(edge / step))
                scored[max(0, centre - width): centre + width] = False
    overlap = (ref[scored].T.astype(np.int64) @ hyp[scored].astype(np.int64))  # (R, H)
    rows, cols = linear_sum_assignment(-overlap)
    mapping = {speakers[r]: int(c) for r, c in zip(rows, cols) if overlap[r, c] > 0}
    n_ref = ref[scored].sum(axis=1)
    n_hyp = hyp[scored].sum(axis=1)
    correct = np.zeros(scored.sum(), dtype=np.int64)
    for speaker, column in mapping.items():
        correct += (ref[scored][:, speakers.index(speaker)] & hyp[scored][:, column])
    total = int(n_ref.sum())
    missed = int(np.maximum(0, n_ref - n_hyp).sum())
    falarm = int(np.maximum(0, n_hyp - n_ref).sum())
    confusion = int((np.minimum(n_ref, n_hyp) - correct).sum())
    return {"total_s": total * step, "missed_s": missed * step, "falarm_s": falarm * step,
            "confusion_s": confusion * step, "der": (missed + falarm + confusion) / total if total else 0.0,
            "mapping": mapping}


def percentiles(values: list[float]) -> dict:
    if not values:
        return {"count": 0}
    array = np.asarray(values, dtype=float)
    return {"count": int(array.size), "mean": round(float(array.mean()), 3),
            "p50": round(float(np.percentile(array, 50)), 3), "p90": round(float(np.percentile(array, 90)), 3),
            "max": round(float(array.max()), 3)}


# --------------------------------------------------------------------------
# System (a): mixed-audio streaming recognition


def run_mixed(args) -> None:
    import torch
    sys.path.insert(0, str(HERE))
    import asr_nemotron
    import soundfile as sf

    torch.set_float32_matmul_precision("high")
    context = [int(v) for v in args.att_context_size.split(",")]
    recognizer = asr_nemotron.NemotronRecognizer(args.model, context, "en")
    entries = [json.loads(line) for line in Path(args.manifest).read_text().splitlines() if line.strip()]
    entries = entries[: args.limit] if args.limit else entries
    push = RATE // 10
    rows = []
    torch.cuda.reset_peak_memory_stats()
    # The first mixture is run once untimed: the first steps pay one-off CUDA
    # kernel selection/compilation that would otherwise land in RTF and lag.
    for position, entry in enumerate([entries[0]] + entries):
        warmup = position == 0
        audio, rate = sf.read(entry["audio_filepath"], dtype="float32")
        reference = json.loads(Path(entry["seglst_filepath"]).read_text())
        session = recognizer.start()
        trace = []  # (audio_end_s, compute_s, words)
        began = time.perf_counter()
        for offset in range(0, audio.size, push):
            step_began = time.perf_counter()
            hypothesis = session.push(audio[offset: offset + push])
            torch.cuda.synchronize()
            trace.append((min(audio.size, offset + push) / RATE, time.perf_counter() - step_began,
                          len(normalise(hypothesis.text).split())))
        step_began = time.perf_counter()
        final = session.finish()
        torch.cuda.synchronize()
        trace.append((audio.size / RATE, time.perf_counter() - step_began, len(normalise(final.text).split())))
        elapsed = time.perf_counter() - began
        hypothesis = [{"session_id": entry["session_id"], "speaker": "mixed", "start_time": 0.0,
                       "end_time": audio.size / RATE, "words": final.text}]
        scores = wer_scores(reference, hypothesis)
        # "New words after a turn onset" lag: when the single stream first grows
        # after t0 (no attribution exists to be measured).
        lags = []
        for seg in reference[1:]:
            before = [w for (t, _, w) in trace if t <= seg["start_time"]]
            base = before[-1] if before else 0
            for t, compute, words in trace:
                if t > seg["start_time"] and words > base:
                    lags.append(t + compute - seg["start_time"])
                    break
        if warmup:
            continue
        rows.append({"session_id": entry["session_id"], "num_speakers": entry["num_speakers"],
                     "overlap_ratio": entry["overlap_ratio"], "duration_s": audio.size / RATE,
                     "hypothesis": final.text, **scores, "rtf": elapsed / (audio.size / RATE),
                     "push_ms": [round(c * 1000, 2) for (_, c, _) in trace], "new_words_lag_s": lags})
        log.info("%s cpWER=%.3f orcWER=%.3f rtf=%.3f", entry["session_id"], scores["cp_errors"] / scores["cp_length"],
                 scores["orc_errors"] / scores["orc_length"], rows[-1]["rtf"])
    peak = torch.cuda.max_memory_allocated() / 2 ** 30
    write_report(args, "mixed", rows, peak, {"model": args.model, "att_context_size": recognizer.att_context_size,
                                              "chunk_ms": recognizer.chunk_ms})


# --------------------------------------------------------------------------
# System (b): Sortformer + Multitalker Parakeet


def _load_example():
    path = NEMO_SRC / "examples/asr/asr_cache_aware_streaming/speech_to_text_multitalker_streaming_infer.py"
    spec = importlib.util.spec_from_file_location("mt_streaming_example", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def run_multitalker(args) -> None:
    import torch
    from omegaconf import OmegaConf
    import nemo.collections.asr as nemo_asr
    from nemo.collections.asr.parts.submodules.subsampling import FeatureStacking
    from nemo.collections.asr.parts.utils.multispk_transcribe_utils import (
        SpeakerTaggedASR, configure_diar_streaming, validate_feature_frame_strides)
    from nemo.collections.asr.parts.utils.streaming_utils import CacheAwareStreamingAudioBuffer

    example = _load_example()
    cfg = OmegaConf.structured(example.MultitalkerTranscriptionConfig())
    cfg.diar_model = args.diar_model
    cfg.asr_model = args.model
    cfg.att_context_size = [int(v) for v in args.att_context_size.split(",")]
    cfg.diar_right_context = args.diar_right_context
    cfg.generate_realtime_scripts = False
    cfg.log = False
    cfg.print_time = False
    cfg.batch_size = 1
    torch.set_float32_matmul_precision(cfg.matmul_precision)
    device = torch.device("cuda")
    torch.cuda.reset_peak_memory_stats()
    # Restore on the CPU, then move: restoring onto the GPU briefly doubles the weights.
    diar_model = example.load_diar_model(cfg.diar_model, torch.device("cpu"))
    use_bf16 = str(cfg.precision).lower().startswith("bf16") and cfg.use_amp and torch.cuda.is_bf16_supported()
    diar_model = diar_model.to(device=device, dtype=torch.bfloat16 if use_bf16 else torch.float32).eval()
    asr_model = nemo_asr.models.ASRModel.from_pretrained(model_name=cfg.asr_model, map_location=torch.device("cpu"))
    asr_model.encoder.set_default_att_context_size(att_context_size=list(cfg.att_context_size))
    example.configure_asr_for_multitalker_streaming(cfg, asr_model)
    asr_model = asr_model.to(cfg.device).eval()
    autocast = torch.amp.autocast(device_type="cuda", dtype=torch.bfloat16, enabled=use_bf16)
    validate_feature_frame_strides(asr_model=asr_model, diar_model=diar_model)
    streaming_cfg = asr_model.encoder.streaming_cfg
    diar_chunk_len = streaming_cfg.valid_out_len + streaming_cfg.cache_drop_size
    configure_diar_streaming(diar_model=diar_model, cfg=cfg,
                             output_subsampling_factor=asr_model.encoder.subsampling_factor,
                             diar_chunk_len=diar_chunk_len)
    if isinstance(diar_model.encoder.pre_encode, FeatureStacking) and not cfg.pad_and_drop_preencoded:
        cfg.pad_and_drop_preencoded = True
    right_offset = cfg.diar_right_context * diar_model.encoder.subsampling_factor
    load_peak = torch.cuda.max_memory_allocated() / 2 ** 30
    log.info("multitalker: att_context=%s diar chunk_len=%d right_context=%d fifo=%d spkcache=%d update=%d",
             cfg.att_context_size, diar_model.sortformer_modules.chunk_len,
             diar_model.sortformer_modules.chunk_right_context, diar_model.sortformer_modules.fifo_len,
             diar_model.sortformer_modules.spkcache_len, diar_model.sortformer_modules.spkcache_update_period)

    entries = [json.loads(line) for line in Path(args.manifest).read_text().splitlines() if line.strip()]
    entries = entries[: args.limit] if args.limit else entries
    rows = []
    torch.cuda.reset_peak_memory_stats()
    # The first mixture is run once untimed: the first steps pay one-off CUDA
    # kernel selection/compilation that would otherwise land in RTF and lag.
    for position, entry in enumerate([entries[0]] + entries):
        warmup = position == 0
        reference = json.loads(Path(entry["seglst_filepath"]).read_text())
        cfg.audio_file = entry["audio_filepath"]
        buffer = CacheAwareStreamingAudioBuffer(model=asr_model, online_normalization=cfg.online_normalization,
                                                pad_and_drop_preencoded=cfg.pad_and_drop_preencoded)
        buffer.append_audio_file(audio_filepath=cfg.audio_file, stream_id=-1)
        total_frames = int(buffer.streams_length[0])
        streamer = SpeakerTaggedASR(cfg, asr_model, diar_model)
        trace = []  # per step: audio_end_s, compute_s, committed diar frames, words per speaker
        began = time.perf_counter()
        for step, (chunk, lengths, diar_chunk, diar_lengths) in enumerate(buffer.iter_with_right_context(right_offset)):
            drop = 0 if step == 0 and not cfg.pad_and_drop_preencoded else streaming_cfg.drop_extra_pre_encoded
            step_began = time.perf_counter()
            with torch.inference_mode(), autocast, torch.no_grad():
                streamer.perform_parallel_streaming_stt_spk(
                    step_num=step, chunk_audio=chunk, chunk_lengths=lengths, diar_chunk_audio=diar_chunk,
                    diar_chunk_lengths=diar_lengths, is_buffer_empty=buffer.is_buffer_empty(),
                    drop_extra_pre_encoded=drop)
            torch.cuda.synchronize()
            compute = time.perf_counter() - step_began
            # The step consumed features up to buffer_idx plus the diarizer's right context.
            audio_end = min(total_frames, buffer.buffer_idx + right_offset) * 0.01
            state = streamer.instance_manager.batch_asr_states[0]
            words = {}
            for speaker in range(len(state.previous_hypothesis)):
                hypothesis = state.previous_hypothesis[speaker]
                if hypothesis is not None and hypothesis.text:
                    words[speaker] = len(normalise(hypothesis.text).split())
            committed = int(streamer.instance_manager.diar_states.diar_pred_out_stream.shape[1])
            trace.append({"audio_end_s": audio_end, "compute_s": compute, "diar_frames": committed,
                          "words": words})
        elapsed = time.perf_counter() - began
        seglst = streamer.generate_seglst_dicts_from_parallel_streaming(samples=[{"audio_filepath": cfg.audio_file}])
        hypothesis = [{"session_id": entry["session_id"], "speaker": seg["speaker"], "start_time": seg["start_time"],
                       "end_time": seg["end_time"], "words": seg["words"]} for seg in seglst]
        streamer.instance_manager.seglst_dict_list = []
        preds = streamer.instance_manager.diar_states.diar_pred_out_stream[0].float().cpu().numpy()
        active = preds > 0.5
        scores = wer_scores(reference, hypothesis)
        der25 = der(reference, active, collar=0.25)
        der0 = der(reference, active, collar=0.0)
        durations = active.sum(axis=0) * FRAME
        estimated = int(np.sum(durations >= 0.5))
        transcribed = len({seg["speaker"] for seg in hypothesis if normalise(seg["words"])})
        mapping = der0["mapping"]
        detection, availability, word_lags, undetected = [], [], [], 0
        for index, seg in enumerate(reference):
            column = mapping.get(seg["speaker"])
            if column is None:
                undetected += 1
                continue
            lo = max(0, int(math.floor((seg["start_time"] - 0.25) / FRAME)))
            hi = min(active.shape[0], int(math.ceil(seg["end_time"] / FRAME)))
            hits = np.nonzero(active[lo:hi, column])[0]
            if hits.size == 0:
                undetected += 1
                continue
            frame = lo + int(hits[0])
            detection.append(frame * FRAME - seg["start_time"])
            for item in trace:
                if item["diar_frames"] > frame:
                    availability.append(item["audio_end_s"] + item["compute_s"] - seg["start_time"])
                    break
            before = [item["words"].get(column, 0) for item in trace if item["audio_end_s"] <= seg["start_time"]]
            base = before[-1] if before else 0
            for item in trace:
                if item["audio_end_s"] > seg["start_time"] and item["words"].get(column, 0) > base:
                    word_lags.append(item["audio_end_s"] + item["compute_s"] - seg["start_time"])
                    break
        if warmup:
            continue
        rows.append({"session_id": entry["session_id"], "num_speakers": entry["num_speakers"],
                     "overlap_ratio": entry["overlap_ratio"], "duration_s": total_frames * 0.01,
                     **scores, "der_collar025": der25, "der_collar0": der0,
                     "estimated_speakers": estimated, "transcribed_speakers": transcribed,
                     "speaker_activity_s": [round(float(d), 2) for d in durations],
                     "detection_offset_s": detection, "availability_lag_s": availability,
                     "first_attributed_word_lag_s": word_lags, "undetected_turns": undetected,
                     "rtf": elapsed / (total_frames * 0.01),
                     "step_ms": [round(item["compute_s"] * 1000, 2) for item in trace],
                     "active_instances_max": max((len(item["words"]) for item in trace), default=0),
                     "hypothesis": hypothesis})
        log.info("%s cpWER=%.3f DER25=%.3f spk=%d/%d rtf=%.3f", entry["session_id"],
                 scores["cp_errors"] / scores["cp_length"], der25["der"], estimated, entry["num_speakers"],
                 rows[-1]["rtf"])
    peak = torch.cuda.max_memory_allocated() / 2 ** 30
    sortformer = diar_model.sortformer_modules
    write_report(args, "multitalker", rows, peak, {
        "asr_model": args.model, "diar_model": args.diar_model, "att_context_size": list(cfg.att_context_size),
        "chunk_ms": int(round(streaming_cfg.valid_out_len * 80)),
        "diar": {"chunk_len_frames": sortformer.chunk_len, "right_context_frames": sortformer.chunk_right_context,
                 "fifo_len": sortformer.fifo_len, "spkcache_len": sortformer.spkcache_len,
                 "spkcache_update_period": sortformer.spkcache_update_period,
                 "input_buffer_s": round((sortformer.chunk_len + sortformer.chunk_right_context) * FRAME, 3)},
        "masked_asr": cfg.masked_asr, "cache_gating": cfg.cache_gating, "binary_diar_preds": cfg.binary_diar_preds,
        "precision": "bf16-autocast" if use_bf16 else "fp32", "model_load_peak_vram_gib": round(load_peak, 3)})


# --------------------------------------------------------------------------
# Reporting


def write_report(args, system: str, rows: list[dict], peak_gib: float, settings: dict) -> None:
    import torch

    cp_errors = sum(r["cp_errors"] for r in rows)
    cp_length = sum(r["cp_length"] for r in rows)
    orc_errors = sum(r["orc_errors"] for r in rows)
    orc_length = sum(r["orc_length"] for r in rows)
    audio = sum(r["duration_s"] for r in rows)
    summary = {"system": system, "mixtures": len(rows), "audio_min": round(audio / 60, 2),
               "overlap_ratio_mean": round(float(np.mean([r["overlap_ratio"] for r in rows])), 4),
               "cpWER": round(cp_errors / cp_length, 4), "ORC_WER": round(orc_errors / orc_length, 4),
               "cp_missed_speakers": sum(r["cp_missed_speaker"] for r in rows),
               "cp_falarm_speakers": sum(r["cp_falarm_speaker"] for r in rows),
               "rtf_mean": round(float(np.mean([r["rtf"] for r in rows])), 4),
               "peak_vram_gib": round(peak_gib, 3), "gpu": torch.cuda.get_device_name(0)}
    if system == "multitalker":
        for collar in ("der_collar025", "der_collar0"):
            totals = {key: sum(r[collar][key] for r in rows) for key in ("total_s", "missed_s", "falarm_s",
                                                                         "confusion_s")}
            summary[collar] = {"der": round((totals["missed_s"] + totals["falarm_s"] + totals["confusion_s"])
                                            / totals["total_s"], 4),
                               "missed": round(totals["missed_s"] / totals["total_s"], 4),
                               "falarm": round(totals["falarm_s"] / totals["total_s"], 4),
                               "confusion": round(totals["confusion_s"] / totals["total_s"], 4)}
        errors = [r["estimated_speakers"] - r["num_speakers"] for r in rows]
        summary["speaker_count"] = {"exact": sum(e == 0 for e in errors), "over": sum(e > 0 for e in errors),
                                    "under": sum(e < 0 for e in errors),
                                    "mean_abs_error": round(float(np.mean(np.abs(errors))), 3),
                                    "transcribed_exact": sum(r["transcribed_speakers"] == r["num_speakers"]
                                                             for r in rows)}
        summary["detection_offset_s"] = percentiles([v for r in rows for v in r["detection_offset_s"]])
        summary["availability_lag_s"] = percentiles([v for r in rows for v in r["availability_lag_s"]])
        summary["first_attributed_word_lag_s"] = percentiles(
            [v for r in rows for v in r["first_attributed_word_lag_s"]])
        summary["undetected_turns"] = sum(r["undetected_turns"] for r in rows)
        summary["step_ms"] = percentiles([v for r in rows for v in r["step_ms"]])
    else:
        summary["new_words_lag_s"] = percentiles([v for r in rows for v in r["new_words_lag_s"]])
        summary["push_ms"] = percentiles([v for r in rows for v in r["push_ms"]])
    report = {"summary": summary, "settings": settings, "manifest": args.manifest, "mixtures": rows,
              "label": args.label}
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(report, indent=1, ensure_ascii=False))
    print(json.dumps(summary, indent=1))


def summarize(args) -> None:
    rows = []
    for path in args.results:
        report = json.loads(Path(path).read_text())
        s, settings = report["summary"], report["settings"]
        rows.append((Path(path).stem, s, settings))
    print("| run | mixtures | overlap | cpWER | ORC-WER | DER c0.25 | DER c0 | spk exact/over/under | "
          "detect p50 | avail lag p50/p90 | word lag p50/p90 | RTF | peak VRAM GiB |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for name, s, settings in rows:
        der25 = s.get("der_collar025", {}).get("der", "-")
        der0 = s.get("der_collar0", {}).get("der", "-")
        count = s.get("speaker_count")
        count = f"{count['exact']}/{count['over']}/{count['under']}" if count else "-"
        detect = s.get("detection_offset_s", {}).get("p50", "-")
        avail = s.get("availability_lag_s", {})
        avail = f"{avail.get('p50', '-')}/{avail.get('p90', '-')}" if avail else "-"
        word = s.get("first_attributed_word_lag_s") or s.get("new_words_lag_s") or {}
        word = f"{word.get('p50', '-')}/{word.get('p90', '-')}"
        print(f"| {name} | {s['mixtures']} | {s['overlap_ratio_mean']} | {s['cpWER']} | {s['ORC_WER']} | {der25} | "
              f"{der0} | {count} | {detect} | {avail} | {word} | {s['rtf_mean']} | {s['peak_vram_gib']} |")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    b = sub.add_parser("build")
    b.add_argument("--libri", default=str(LIBRI))
    b.add_argument("--out", required=True)
    b.add_argument("--conditions", default="2:0.25,4:0.25", help="K:overlap_ratio,...")
    b.add_argument("--count", type=int, default=20)
    b.add_argument("--seed", default="20260922")
    for name in ("mixed", "multitalker"):
        p = sub.add_parser(name)
        p.add_argument("--manifest", required=True)
        p.add_argument("--out", required=True)
        p.add_argument("--label", default="")
        p.add_argument("--limit", type=int, default=0)
        p.add_argument("--att-context-size", default="70,13")
        if name == "mixed":
            p.add_argument("--model", default="nvidia/nemotron-speech-streaming-en-0.6b")
        else:
            p.add_argument("--model", default="nvidia/multitalker-parakeet-streaming-0.6b-v1")
            p.add_argument("--diar-model", default="nvidia/diar_streaming_sortformer_4spk-v2.1")
            p.add_argument("--diar-right-context", type=int, default=0,
                           help="extra Sortformer right context in 80 ms frames (adds latency)")
    s = sub.add_parser("summarize")
    s.add_argument("results", nargs="+")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s")
    os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")
    {"build": build, "mixed": run_mixed, "multitalker": run_multitalker, "summarize": summarize}[args.command](args)


if __name__ == "__main__":
    main()
