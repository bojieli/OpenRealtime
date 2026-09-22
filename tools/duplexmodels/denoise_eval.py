#!/usr/bin/env python3
"""DeepFilterNet as an optional acoustic-preprocessing branch (plan cell A0).

DeepFilterNet is a noise suppressor, not an echo canceller, a diariser, or a
source separator. This harness measures what the plan's section 7.8 asks a
preprocessing stage to declare, always keeping the raw signal next to the
processed one (the bypass):

``wer``      ASR word error on FD-Bench user turns, raw versus DeepFilterNet,
             through the local Voxtral realtime server (vLLM ``/v1/realtime``),
             plus words hallucinated on noise-only gaps.
``delay``    algorithmic delay (configured and measured by cross-correlation)
             and CPU real-time factor.
``quiet``    energy retention and ASR survival of short, low-level utterances:
             FDB v1.5 ``user_backchannel`` clips at their own level, attenuated,
             and under added noise; and third-party ``background_speech``
             (which a denoiser is expected to keep, not remove).
``labels``   classify each FD-Bench "noisy" conversation's interference as
             speech (MUSAN's list includes audiobook speech) or non-speech noise.
``summary``  fold the JSON files into one table.

FD-Bench ``*.timestamps`` are silero-VAD sample indices at **16 kHz** even
though the WAVs are 24 kHz (silero's ``read_audio`` resamples); reading them
at the file rate cuts every turn at two thirds of its length.

Run in the ``deepfilter`` venv (torch 2.0.1 CPU; DeepFilterNet 0.5.6 imports
``torchaudio.backend`` which later torchaudio removed)::

    V=.runtime/duplex-plan/venvs/deepfilter/bin/python
    $V tools/duplexmodels/denoise_eval.py delay --out .runtime/duplex-plan/results/perception/denoise-delay.json
    $V tools/duplexmodels/denoise_eval.py wer --condition bg-0dB --count 30 \
        --out .runtime/duplex-plan/results/perception/denoise-wer-bg-0dB.json
    $V tools/duplexmodels/denoise_eval.py quiet --count 30 --out .runtime/duplex-plan/results/perception/denoise-quiet.json
    $V tools/duplexmodels/denoise_eval.py summary --dir .runtime/duplex-plan/results/perception
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import glob
import json
import os
import random
import re
import time
from math import gcd
from pathlib import Path

import numpy as np
import soundfile as sf
from scipy.signal import resample_poly

ROOT = Path(__file__).resolve().parents[2]
FDBENCH = ROOT / ".runtime/fd-bench/dataset"
FDBENCH_TEXT = ROOT / ".runtime/fd-bench/upstream/tts-generation/conversation_round.json"
FDB15 = ROOT / ".runtime/full-duplex-bench-v1.5/dataset"
VOXTRAL_URL = os.environ.get("VOXTRAL_URL", "ws://127.0.0.1:9101/v1/realtime")
VOXTRAL_MODEL = os.environ.get("VOXTRAL_MODEL", "voxtral-realtime")
TIMESTAMP_RATE = 16_000  # FD-Bench timestamps are silero indices at 16 kHz

CONDITIONS = {
    "clean": "cosyvoice2-single-round-combine-easy",
    **{f"{kind}-{snr}dB": f"cosyvoice2-single-round-combine-easy-noisy-{kind}-{snr}dB"
       for kind in ("bg", "gap") for snr in (0, 10, 20)},
}


# --------------------------------------------------------------------------
# Signal helpers


def resample(x: np.ndarray, rate: int, target: int) -> np.ndarray:
    if rate == target:
        return x.astype(np.float32)
    g = gcd(rate, target)
    return resample_poly(x, target // g, rate // g).astype(np.float32)


def rms_db(x: np.ndarray) -> float:
    return float(20 * np.log10(np.sqrt(np.mean(np.square(x, dtype=np.float64))) + 1e-12))


class Denoiser:
    """DeepFilterNet3 on CPU, whole-stream (its GRU state runs across the stream)."""

    def __init__(self, threads: int | None = None):
        import torch
        from df.enhance import enhance, init_df

        if threads:
            torch.set_num_threads(threads)
        self.torch = torch
        self._enhance = enhance
        self.model, self.state, _ = init_df(log_level="ERROR")
        self.rate = self.state.sr()
        self.hop = self.state.hop_size()
        self.fft = self.state.fft_size()

    def __call__(self, x: np.ndarray, rate: int, out_rate: int = 16_000, pad: bool = True) -> tuple[np.ndarray, float]:
        x48 = resample(x, rate, self.rate)
        began = time.perf_counter()
        with self.torch.no_grad():
            y = self._enhance(self.model, self.state, self.torch.from_numpy(x48)[None], pad=pad)
        seconds = time.perf_counter() - began
        return resample(y[0].numpy(), self.rate, out_rate), seconds


# --------------------------------------------------------------------------
# Voxtral realtime client (vLLM /v1/realtime)


async def transcribe(audio16: np.ndarray, url: str = VOXTRAL_URL, model: str = VOXTRAL_MODEL,
                     attempts: int = 3) -> tuple[str, float]:
    """One utterance per socket. The shared server occasionally parks a session
    behind others (``Waiting: N reqs``); a stalled session is retried on a new
    socket rather than scored as an empty transcript."""
    for attempt in range(attempts):
        try:
            return await _transcribe_once(audio16, url, model)
        except (asyncio.TimeoutError, OSError) as error:
            if attempt == attempts - 1:
                raise RuntimeError(f"voxtral stalled {attempts} times: {error!r}") from error
            print(f"voxtral retry {attempt + 1}: {error!r}", flush=True)
    raise AssertionError("unreachable")


async def _transcribe_once(audio16: np.ndarray, url: str, model: str) -> tuple[str, float]:
    import websockets

    pcm = (np.clip(audio16, -1, 1) * 32767).astype("<i2").tobytes()
    async with websockets.connect(url, max_size=1 << 22, open_timeout=20) as ws:
        await ws.send(json.dumps({"type": "session.update", "model": model}))
        await ws.send(json.dumps({"type": "input_audio_buffer.commit"}))
        step = 3200 * 2  # 200 ms of PCM16 per append, sent without pacing
        for i in range(0, len(pcm), step):
            await ws.send(json.dumps({"type": "input_audio_buffer.append",
                                      "audio": base64.b64encode(pcm[i:i + step]).decode()}))
        began = time.perf_counter()
        await ws.send(json.dumps({"type": "input_audio_buffer.commit", "final": True}))
        text = ""
        while True:
            message = json.loads(await asyncio.wait_for(ws.recv(), timeout=30))
            if message.get("type") == "transcription.delta":
                text += message.get("delta", "")
            elif message.get("type") == "transcription.done":
                done = message.get("text", "")
                text = done if done.startswith(text) or not text else text
                return text.strip(), (time.perf_counter() - began) * 1000
            elif message.get("type") == "error":
                raise RuntimeError(f"voxtral error: {message}")


def normalize(text: str) -> list[str]:
    text = text.lower().replace("’", "'")
    text = re.sub(r"[^a-z0-9' ]+", " ", text)
    return [w.strip("'") for w in text.split() if w.strip("'")]


def word_errors(reference: str, hypothesis: str) -> tuple[int, int]:
    ref, hyp = normalize(reference), normalize(hypothesis)
    d = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        prev, d[0] = d[0], i
        for j, h in enumerate(hyp, 1):
            prev, d[j] = d[j], min(d[j] + 1, d[j - 1] + 1, prev + (r != h))
    return d[len(hyp)], len(ref)


# --------------------------------------------------------------------------
# wer


def fdbench_turns(conversation: str) -> list[str]:
    rounds = json.loads(FDBENCH_TEXT.read_text())
    return [s.strip() for s in re.split(r"<[^>]+>", rounds[conversation]["user"]) if s.strip()]


def sample_conversations(count: int, seed: int, offset: int = 0) -> list[str]:
    ids = sorted(re.search(r"conversation_(\d+)", p).group(1)
                 for p in glob.glob(str(FDBENCH / CONDITIONS["clean"] / "*.timestamps")))
    random.Random(seed).shuffle(ids)
    return ids[offset:offset + count]


async def run_wer(args) -> dict:
    denoiser = Denoiser(args.threads)
    folder = FDBENCH / CONDITIONS[args.condition]
    semaphore = asyncio.Semaphore(args.concurrency)
    turns, gaps, enhance_seconds, audio_seconds = [], [], 0.0, 0.0

    async def asr(audio):
        async with semaphore:
            return await transcribe(audio)

    for conversation in sample_conversations(args.count, args.seed, args.offset):
        wav = folder / f"conversation_{conversation}.wav"
        spans = json.loads((folder / f"conversation_{conversation}.timestamps").read_text())
        references = fdbench_turns(conversation)
        if len(spans) != len(references):
            continue
        audio, rate = sf.read(wav, dtype="float32")
        raw16 = resample(audio, rate, 16_000)
        processed16, seconds = denoiser(audio, rate)
        enhance_seconds += seconds
        audio_seconds += len(audio) / rate
        processed16 = processed16[: len(raw16)]
        jobs = []
        for index, (span, reference) in enumerate(zip(spans, references)):
            a = max(0, span["start"] - int(0.2 * TIMESTAMP_RATE))
            b = span["end"] + int(0.3 * TIMESTAMP_RATE)
            jobs.append(("turn", index, reference, raw16[a:b], processed16[a:b]))
        # Noise-only gaps between turns (inside the conversation), at most 5 s each.
        for index in range(len(spans) - 1):
            a = spans[index]["end"] + int(0.5 * TIMESTAMP_RATE)
            b = min(spans[index + 1]["start"] - int(0.5 * TIMESTAMP_RATE), a + 5 * TIMESTAMP_RATE)
            if b - a >= TIMESTAMP_RATE:
                jobs.append(("gap", index, "", raw16[a:b], processed16[a:b]))
        results = await asyncio.gather(*[asr(job[3]) for job in jobs], *[asr(job[4]) for job in jobs])
        for job, (raw_text, raw_ms), (proc_text, proc_ms) in zip(jobs, results[: len(jobs)], results[len(jobs):]):
            kind, index, reference, raw, processed = job
            record = {"conversation": conversation, "index": index,
                      "raw_rms_db": round(rms_db(raw), 2), "processed_rms_db": round(rms_db(processed), 2),
                      "raw": raw_text, "processed": proc_text,
                      "raw_finalize_ms": round(raw_ms, 1), "processed_finalize_ms": round(proc_ms, 1)}
            if kind == "turn":
                record["reference"] = reference
                record["raw_errors"], record["words"] = word_errors(reference, raw_text)
                record["processed_errors"], _ = word_errors(reference, proc_text)
                turns.append(record)
            else:
                record["raw_words"] = len(normalize(raw_text))
                record["processed_words"] = len(normalize(proc_text))
                gaps.append(record)
        print(f"{args.condition} conv {conversation}: turns={len(turns)} gaps={len(gaps)}", flush=True)

    words = sum(t["words"] for t in turns)
    summary = {
        "condition": args.condition, "dataset": CONDITIONS[args.condition],
        "conversations": args.count, "offset": args.offset, "seed": args.seed,
        "turns": len(turns), "reference_words": words,
        "wer_raw": round(sum(t["raw_errors"] for t in turns) / max(words, 1), 4),
        "wer_denoised": round(sum(t["processed_errors"] for t in turns) / max(words, 1), 4),
        "turns_worse_after_denoise": sum(t["processed_errors"] > t["raw_errors"] for t in turns),
        "turns_better_after_denoise": sum(t["processed_errors"] < t["raw_errors"] for t in turns),
        "gap_segments": len(gaps),
        "gap_words_raw": sum(g["raw_words"] for g in gaps),
        "gap_words_denoised": sum(g["processed_words"] for g in gaps),
        "gap_segments_with_words_raw": sum(g["raw_words"] > 0 for g in gaps),
        "gap_segments_with_words_denoised": sum(g["processed_words"] > 0 for g in gaps),
        "gap_rms_db_raw_mean": round(float(np.mean([g["raw_rms_db"] for g in gaps])), 2) if gaps else None,
        "gap_rms_db_denoised_mean": round(float(np.mean([g["processed_rms_db"] for g in gaps])), 2) if gaps else None,
        "denoise_rtf_cpu": round(enhance_seconds / max(audio_seconds, 1e-9), 4),
    }
    return {"summary": summary, "asr": {"url": VOXTRAL_URL, "model": VOXTRAL_MODEL,
            "pacing": "unpaced 200 ms appends, final commit"},
            "denoiser": {"model": "DeepFilterNet3", "package": "deepfilternet 0.5.6", "device": "cpu",
                         "path": "24 kHz -> 48 kHz -> DFN3 (whole stream) -> 16 kHz; raw: 24 kHz -> 16 kHz"},
            "turns": turns, "gaps": gaps}


# --------------------------------------------------------------------------
# delay


def run_delay(args) -> dict:
    import configparser

    config = configparser.ConfigParser()
    config.read(Path.home() / ".cache/DeepFilterNet/DeepFilterNet3/config.ini")
    rate = int(config["df"]["sr"])
    fft, hop = int(config["df"]["fft_size"]), int(config["df"]["hop_size"])
    df_lookahead, conv_lookahead = int(config["df"]["df_lookahead"]), int(config["deepfilternet"]["conv_lookahead"])
    frame_ms = hop / rate * 1000
    configured = {
        "sample_rate": rate, "fft_size": fft, "hop_size": hop,
        "window_ms": fft / rate * 1000, "hop_ms": frame_ms,
        "df_lookahead_frames": df_lookahead, "conv_lookahead_frames": conv_lookahead,
        "algorithmic_delay_ms": fft / rate * 1000 + max(df_lookahead, conv_lookahead) * frame_ms,
        "note": "streaming latency = one STFT window (20 ms) + max lookahead (2 hops = 20 ms); "
                "the offline enhance() hides it by padding and trimming",
    }

    denoiser = Denoiser(args.threads)
    audio, source_rate = sf.read(FDBENCH / CONDITIONS["bg-10dB"] / "conversation_100.wav", dtype="float32")
    audio = audio[: 20 * source_rate]
    x48 = resample(audio, source_rate, denoiser.rate)
    import torch

    measured = {}
    for pad in (True, False):
        with torch.no_grad():
            y = denoiser._enhance(denoiser.model, denoiser.state, torch.from_numpy(x48)[None], pad=pad)[0].numpy()
        n = min(len(y), len(x48))
        window = 48_000 * 5
        a, b = x48[48_000: 48_000 + window], y[48_000: 48_000 + window + 4800]
        corr = np.correlate(b, a, mode="valid")
        measured["pad_true" if pad else "pad_false"] = {
            "output_shift_samples_48k": int(np.argmax(np.abs(corr))),
            "output_shift_ms": round(int(np.argmax(np.abs(corr))) / 48, 3), "output_len": n,
        }

    rtf = {}
    for threads in (1, 4, torch.get_num_threads()):
        torch.set_num_threads(threads)
        with torch.no_grad():
            began = time.perf_counter()
            denoiser._enhance(denoiser.model, denoiser.state, torch.from_numpy(x48)[None])
            seconds = time.perf_counter() - began
        rtf[f"threads_{threads}"] = {"rtf": round(seconds / 20.0, 4),
                                     "ms_per_10ms_hop": round(seconds / (20.0 / (frame_ms / 1000)) * 1000, 4)}
    return {"configured": configured, "measured_alignment": measured, "cpu_rtf_whole_stream": rtf,
            "rtf_note": "whole-sequence (batched over time) CPU inference on 20 s; a frame-by-frame "
                        "real-time loop pays per-call overhead this does not include"}


# --------------------------------------------------------------------------
# quiet


async def run_quiet(args) -> dict:
    denoiser = Denoiser(args.threads)
    rng = random.Random(args.seed)
    noise_audio, noise_rate = sf.read(FDBENCH / CONDITIONS["gap-0dB"] / "conversation_100.wav", dtype="float32")
    noise_spans = json.loads((FDBENCH / CONDITIONS["gap-0dB"] / "conversation_100.timestamps").read_text())
    # Noise-only stretch between the first two turns, at 16 kHz.
    noise16 = resample(noise_audio, noise_rate, 16_000)[noise_spans[0]["end"] + 8000: noise_spans[1]["start"] - 8000]

    records = []
    semaphore = asyncio.Semaphore(args.concurrency)

    async def asr(audio):
        async with semaphore:
            return (await transcribe(audio))[0]

    for category, event_key in (("user_backchannel", "backchannel_text"), ("background_speech", "background_text")):
        ids = sorted(os.listdir(FDB15 / category), key=lambda s: int(s) if s.isdigit() else 0)
        ids = [i for i in ids if i.isdigit()]
        rng.shuffle(ids)
        for sample in ids[: args.count]:
            folder = FDB15 / category / sample
            meta = json.loads((folder / "metadata.json").read_text())
            audio, rate = sf.read(folder / "input.wav", dtype="float32")
            audio16 = resample(audio, rate, 16_000)
            s0, s1 = meta["timestamps"]
            a, b = int(s0 * 16_000), int(s1 * 16_000)
            for level in ([0.0, -20.0, -30.0] if category == "user_backchannel" else [0.0]):
                for noisy in ([False, True] if category == "user_backchannel" else [False]):
                    x = audio16 * (10 ** (level / 20))
                    if noisy:
                        event_rms = np.sqrt(np.mean(x[a:b] ** 2)) + 1e-9
                        n = np.resize(noise16, len(x))
                        n = n * event_rms / (np.sqrt(np.mean(n ** 2)) + 1e-9) / (10 ** (args.noise_snr / 20))
                        x = x + n
                    y, _ = denoiser(x, 16_000)
                    y = y[: len(x)]
                    window = slice(max(0, a - 1600), min(len(x), b + 3200))
                    raw_text, proc_text = await asyncio.gather(asr(x[window]), asr(y[window]))
                    target = normalize(meta.get(event_key, ""))
                    records.append({
                        "category": category, "sample": sample, "level_db": level, "noisy": noisy,
                        "event_text": meta.get(event_key, ""),
                        "raw_rms_db": round(rms_db(x[a:b]), 2), "processed_rms_db": round(rms_db(y[a:b]), 2),
                        "retention_db": round(rms_db(y[a:b]) - rms_db(x[a:b]), 2),
                        "raw_asr": raw_text, "processed_asr": proc_text,
                        "raw_event_word_recall": recall(target, normalize(raw_text)),
                        "processed_event_word_recall": recall(target, normalize(proc_text)),
                    })
            print(f"{category} {sample} done", flush=True)

    groups = {}
    for r in records:
        key = f"{r['category']}|level={r['level_db']:+.0f}dB|noise={'snr' + str(args.noise_snr) + 'dB' if r['noisy'] else 'none'}"
        groups.setdefault(key, []).append(r)
    summary = {key: {
        "n": len(rs),
        "event_rms_db_raw_mean": round(float(np.mean([r["raw_rms_db"] for r in rs])), 2),
        "retention_db_mean": round(float(np.mean([r["retention_db"] for r in rs])), 2),
        "retention_db_min": round(float(np.min([r["retention_db"] for r in rs])), 2),
        "clips_losing_more_than_6db": sum(r["retention_db"] < -6 for r in rs),
        "event_word_recall_raw": round(float(np.mean([r["raw_event_word_recall"] for r in rs])), 3),
        "event_word_recall_denoised": round(float(np.mean([r["processed_event_word_recall"] for r in rs])), 3),
    } for key, rs in sorted(groups.items())}
    return {"summary": summary, "noise_source": "FD-Bench cosyvoice2 noisy-gap-0dB conversation_100 gap (MUSAN)",
            "noise_snr_db": args.noise_snr, "records": records}


def recall(target: list[str], hypothesis: list[str]) -> float:
    if not target:
        return 1.0
    pool = list(hypothesis)
    hit = 0
    for w in target:
        if w in pool:
            pool.remove(w)
            hit += 1
    return hit / len(target)


# --------------------------------------------------------------------------
# interference labels


async def run_labels(args) -> dict:
    """Label what the FD-Bench "noise" actually is, per conversation.

    FD-Bench mixes one noise file per conversation from its MUSAN list, and
    that list includes speech recordings (e.g. Portuguese and Dutch
    audiobooks), so a "noisy" conversation may carry a background talker
    rather than noise. A speech enhancer keeps speech, so DeepFilterNet's
    attenuation of a noise-only gap separates the two: little attenuation plus
    recognisable words means interfering speech; deep attenuation and no words
    means non-speech noise.
    """
    denoiser = Denoiser(args.threads)
    folder = FDBENCH / CONDITIONS[args.condition]
    labels = {}
    for conversation in sample_conversations(args.count, args.seed, args.offset):
        spans = json.loads((folder / f"conversation_{conversation}.timestamps").read_text())
        audio, rate = sf.read(folder / f"conversation_{conversation}.wav", dtype="float32")
        raw16 = resample(audio, rate, 16_000)
        a = spans[0]["end"] + int(0.5 * TIMESTAMP_RATE)
        b = min(spans[1]["start"] - int(0.5 * TIMESTAMP_RATE), a + 5 * TIMESTAMP_RATE)
        gap = raw16[a:b]
        processed, _ = denoiser(gap, 16_000)
        attenuation = rms_db(gap) - rms_db(processed[: len(gap)])
        text, _ = await transcribe(gap)
        words = len(normalize(text))
        if attenuation < 6 and words >= 3:
            kind = "speech"
        elif attenuation > 10 and words == 0:
            kind = "noise"
        else:
            kind = "ambiguous"
        labels[conversation] = {"interference": kind, "gap_attenuation_db": round(attenuation, 2),
                                "gap_rms_db": round(rms_db(gap), 2), "gap_asr": text, "gap_asr_words": words}
        print(conversation, kind, round(attenuation, 1), words, text[:60], flush=True)
    counts = {k: sum(v["interference"] == k for v in labels.values()) for k in ("speech", "noise", "ambiguous")}
    return {"summary": {"condition": args.condition, "conversations": len(labels), **counts,
                        "rule": "speech: DFN gap attenuation < 6 dB and >= 3 ASR words; "
                                "noise: attenuation > 10 dB and 0 words"}, "labels": labels}


# --------------------------------------------------------------------------
# summary


def run_summary(args) -> dict:
    shards: dict[str, list[dict]] = {}
    for path in sorted(Path(args.dir).glob("denoise-wer-*.json")):
        data = json.loads(path.read_text())
        shards.setdefault(data["summary"]["condition"], []).append(data)
    rows = []
    for condition, parts in sorted(shards.items()):
        turns = [t for part in parts for t in part["turns"]]
        gaps = [g for part in parts for g in part["gaps"]]
        words = sum(t["words"] for t in turns)
        rows.append({
            "condition": condition, "shards": len(parts),
            "conversations": len({t["conversation"] for t in turns}), "turns": len(turns),
            "reference_words": words,
            "wer_raw": round(sum(t["raw_errors"] for t in turns) / max(words, 1), 4),
            "wer_denoised": round(sum(t["processed_errors"] for t in turns) / max(words, 1), 4),
            "turns_worse_after_denoise": sum(t["processed_errors"] > t["raw_errors"] for t in turns),
            "turns_better_after_denoise": sum(t["processed_errors"] < t["raw_errors"] for t in turns),
            "gap_segments": len(gaps),
            "gap_words_raw": sum(g["raw_words"] for g in gaps),
            "gap_words_denoised": sum(g["processed_words"] for g in gaps),
            "gap_segments_with_words_raw": sum(g["raw_words"] > 0 for g in gaps),
            "gap_segments_with_words_denoised": sum(g["processed_words"] > 0 for g in gaps),
            "gap_rms_db_raw_mean": round(float(np.mean([g["raw_rms_db"] for g in gaps])), 2) if gaps else None,
            "gap_rms_db_denoised_mean": round(float(np.mean([g["processed_rms_db"] for g in gaps])), 2) if gaps else None,
            "denoise_rtf_cpu_shards": [part["summary"]["denoise_rtf_cpu"] for part in parts],
        })
    out = {"wer": rows}
    for name in ("denoise-delay.json", "denoise-quiet.json"):
        path = Path(args.dir) / name
        if path.exists():
            data = json.loads(path.read_text())
            out[name.removesuffix(".json")] = {k: v for k, v in data.items() if k != "records"}
    return out


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    wer = sub.add_parser("wer")
    wer.add_argument("--condition", choices=sorted(CONDITIONS), required=True)
    wer.add_argument("--count", type=int, default=30)
    wer.add_argument("--seed", type=int, default=7)
    wer.add_argument("--offset", type=int, default=0, help="shard start in the seeded sample")
    wer.add_argument("--concurrency", type=int, default=2)
    delay = sub.add_parser("delay")
    quiet = sub.add_parser("quiet")
    quiet.add_argument("--count", type=int, default=30)
    quiet.add_argument("--seed", type=int, default=7)
    quiet.add_argument("--concurrency", type=int, default=2)
    quiet.add_argument("--noise-snr", type=float, default=5.0)
    labels = sub.add_parser("labels")
    labels.add_argument("--condition", choices=sorted(CONDITIONS), default="bg-10dB")
    labels.add_argument("--count", type=int, default=80)
    labels.add_argument("--seed", type=int, default=11)
    labels.add_argument("--offset", type=int, default=0)
    summary = sub.add_parser("summary")
    summary.add_argument("--dir", required=True)
    for p in (wer, delay, quiet, labels, summary):
        p.add_argument("--out", required=False)
        p.add_argument("--threads", type=int, default=8)
    args = parser.parse_args()

    if args.command == "wer":
        result = asyncio.run(run_wer(args))
    elif args.command == "delay":
        result = run_delay(args)
    elif args.command == "quiet":
        result = asyncio.run(run_quiet(args))
    elif args.command == "labels":
        result = asyncio.run(run_labels(args))
    else:
        result = run_summary(args)
    text = json.dumps(result, indent=2, ensure_ascii=False)
    if args.out:
        Path(args.out).parent.mkdir(parents=True, exist_ok=True)
        Path(args.out).write_text(text)
    printable = result.get("summary", result)
    print(json.dumps(printable, indent=2, ensure_ascii=False)[:4000])


if __name__ == "__main__":
    main()
