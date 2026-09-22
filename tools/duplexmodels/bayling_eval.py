#!/usr/bin/env python3
"""BayLing-Duplex feasibility check on real audio (plan stage P8, task extensions).

BayLing-Duplex (GLM-4-Voice based, ~9B) interleaves user speech tokens,
assistant dialogue-state/text tokens and assistant speech tokens in one
autoregressive sequence, in blocks of 10:5:10 (0.8 s of user audio at 12.5 Hz,
then 5 text-channel tokens and 10 speech tokens). This harness answers three
questions with the upstream code (``bayling_duplex`` package, unmodified):

1. Does the released checkpoint load and run its own inference on real audio?
   (FDB v1.5 ``user_interruption`` clips: the question alone for turn-taking,
   and the full clip with the mid-answer interruption.)
2. Is block-by-block inference fast enough for live use? (Per-block compute
   time against the 800 ms of audio each block represents.)
3. Can the input side run continuously? The GLM-4-Voice tokenizer uses
   block-causal attention with 200-frame (4 s) blocks, so tokens of a partial
   block can change as audio arrives. We tokenize growing prefixes and measure
   how many already-emitted tokens would have been revised, then run a live
   simulation that feeds prefix tokens block by block through
   ``stream_audio_tokens`` with persistent KV state.

License: BayLing-Duplex weights follow the glm-4-voice License (academic
research free; commercial use requires registration with Zhipu AI).

    .runtime/duplex-plan/venvs/bayling/bin/python tools/duplexmodels/bayling_eval.py \
        --out .runtime/duplex-plan/results/tasks/bayling
"""

from __future__ import annotations

import argparse
import json
import os
import random
import time
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[2]
FDB15 = ROOT / ".runtime/full-duplex-bench-v1.5/dataset"
HUB = Path(os.environ.get("HF_HUB_CACHE", Path.home() / ".cache/huggingface/hub"))


def snapshot(repo: str) -> Path:
    return sorted((HUB / f"models--{repo.replace('/', '--')}" / "snapshots").glob("*"))[-1]


def load_16k(path: Path) -> np.ndarray:
    import soundfile as sf
    from math import gcd
    from scipy.signal import resample_poly

    audio, rate = sf.read(path, dtype="float32")
    if audio.ndim > 1:
        audio = audio.mean(axis=1)
    if rate != 16000:
        g = gcd(rate, 16000)
        audio = resample_poly(audio, 16000 // g, rate // g).astype(np.float32)
    return audio


def timed_stream(model, tokens, state, **kwargs):
    """Run stream_audio_tokens and time each block (one audio event per block)."""
    import torch

    block_ms = []
    torch.cuda.synchronize()
    began = time.perf_counter()
    for event in model.stream_audio_tokens(tokens, state=state, **kwargs):
        if event.kind == "audio":
            torch.cuda.synchronize()
            now = time.perf_counter()
            block_ms.append((now - began) * 1000)
            began = now
    return block_ms


def summarize(values):
    a = np.asarray(values, dtype=float)
    if not len(a):
        return None
    return {"n": int(len(a)), "mean": round(float(a.mean()), 1), "p50": round(float(np.percentile(a, 50)), 1),
            "p90": round(float(np.percentile(a, 90)), 1), "max": round(float(a.max()), 1)}


def run(args) -> dict:
    import soundfile as sf
    import torch
    from bayling_duplex import BayLingDuplex

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    report: dict = {"model": f"BayLing-Models/BayLing-Duplex@{snapshot('BayLing-Models/BayLing-Duplex').name[:12]}",
                    "speech_tokenizer": f"zai-org/glm-4-voice-tokenizer@{snapshot('zai-org/glm-4-voice-tokenizer').name[:12]}",
                    "speech_decoder": f"zai-org/glm-4-voice-decoder@{snapshot('zai-org/glm-4-voice-decoder').name[:12]}",
                    "interleave_ratio": "10:5:10", "temperature": args.temperature, "top_p": args.top_p,
                    "seed": args.seed}
    began = time.perf_counter()
    model = BayLingDuplex(model_path=str(snapshot("BayLing-Models/BayLing-Duplex")),
                          speech_tokenizer_path=str(snapshot("zai-org/glm-4-voice-tokenizer")),
                          decoder_path=str(snapshot("zai-org/glm-4-voice-decoder")),
                          interleave_ratio="10:5:10", device="cuda", torch_dtype="bfloat16")
    report["load_seconds"] = round(time.perf_counter() - began, 1)
    report["gpu_allocated_gb_after_load"] = round(torch.cuda.memory_allocated() / 2**30, 2)
    kwargs = dict(temperature=args.temperature, top_p=args.top_p)

    ids = sorted((d for d in os.listdir(FDB15 / "user_interruption") if d.isdigit()), key=int)
    random.Random(args.seed).shuffle(ids)
    cases = []
    all_block_ms = []
    for sample in ids[: args.count]:
        folder = FDB15 / "user_interruption" / sample
        meta = json.loads((folder / "metadata.json").read_text())
        for kind, audio, epads in (("question_only", load_16k(folder / "context.wav"), 1),
                                   ("with_interruption", load_16k(folder / "input.wav"), 2)):
            torch.manual_seed(args.seed)
            duration = len(audio) / 16000
            padded = model.pad_or_trim_audio(torch.from_numpy(audio)[None], args.max_duration)
            t0 = time.perf_counter()
            tokens, _ = model.tokenize_audio((padded, 16000))
            tokenize_ms = (time.perf_counter() - t0) * 1000
            state = model.create_stream_state()
            block_ms = timed_stream(model, tokens, state, max_epad_count=epads, **kwargs)
            all_block_ms += block_ms
            segments = model._extract_segments(state.text_tokens, state.audio_tokens, duration)
            record = {"sample": sample, "kind": kind, "user_audio_s": round(duration, 2),
                      "context_text": meta["context_text"], "interruption_text": meta.get("current_turn_text"),
                      "interruption_window_s": meta["timestamps"] if kind == "with_interruption" else None,
                      "tokenize_ms": round(tokenize_ms, 1), "blocks": len(block_ms),
                      "block_ms": summarize(block_ms), "stopped": state.stop_requested,
                      "assistant_turns": state.assistant_count, "epads": state.epad_count,
                      "segments": [{"text": s.text, "start_time_s": round(s.start_time, 2),
                                    "turn_taking_time_s": None if s.turn_taking_time is None else round(s.turn_taking_time, 2),
                                    "start_block": s.start_block, "end_block": s.end_block} for s in segments]}
            if segments and args.audio:
                t0 = time.perf_counter()
                wav = model.tokens_to_audio(state.audio_tokens if kind == "with_interruption" else segments[0].audio_tokens)
                record["decode_audio_ms"] = round((time.perf_counter() - t0) * 1000, 1)
                name = f"{sample}-{kind}.wav"
                sf.write(out / name, wav.squeeze(0).numpy(), model.output_sample_rate)
                record["audio_file"] = name
                record["audio_s"] = round(wav.shape[-1] / model.output_sample_rate, 2)
            cases.append(record)
            print(json.dumps({k: record[k] for k in ("sample", "kind", "blocks", "segments")}, ensure_ascii=False)[:600],
                  flush=True)
    report["cases"] = cases
    report["block_ms_all"] = summarize(all_block_ms)
    report["block_audio_ms"] = 800
    report["block_rtf_p90"] = round(report["block_ms_all"]["p90"] / 800, 3) if all_block_ms else None

    # Tokenizer prefix stability: tokens of growing prefixes versus the full-clip tokens.
    folder = FDB15 / "user_interruption" / ids[0]
    audio = load_16k(folder / "input.wav")
    full, _ = model.tokenize_audio((torch.from_numpy(audio)[None], 16000))
    revised, compared, revised_before_last_block = 0, 0, 0
    steps = []
    for end in range(12800, len(audio) + 1, 12800):  # 0.8 s steps
        prefix, _ = model.tokenize_audio((torch.from_numpy(audio[:end])[None], 16000))
        n = min(len(prefix), len(full))
        diff = int(sum(a != b for a, b in zip(prefix[:n], full[:n])))
        complete_block_tokens = (end // 16000 // 4) * 4 * 12.5  # tokens in completed 4 s blocks
        stable_part = int(min(n, complete_block_tokens))
        diff_complete = int(sum(a != b for a, b in zip(prefix[:stable_part], full[:stable_part])))
        steps.append({"prefix_s": round(end / 16000, 1), "tokens": n, "differ_from_final": diff,
                      "differ_in_completed_4s_blocks": diff_complete})
        revised += diff
        compared += n
        revised_before_last_block += diff_complete
    report["tokenizer_prefix_stability"] = {
        "clip": f"user_interruption/{ids[0]}", "steps": steps,
        "tokens_compared": compared, "tokens_differing": revised,
        "tokens_differing_in_completed_blocks": revised_before_last_block,
        "note": "block-causal VQ encoder (quantize_causal_block_size=200 frames = 4 s): tokens are final "
                "only once their 4 s block is complete"}

    # Live simulation: every 0.8 s, tokenize what has arrived and feed the next 10 tokens.
    torch.manual_seed(args.seed)
    audio = np.concatenate([audio, np.zeros(int(8 * 16000), dtype=np.float32)])
    state = model.create_stream_state()
    live_block_ms, fed = [], 0
    fed_tokens = []
    for end in range(12800, len(audio) + 1, 12800):
        t0 = time.perf_counter()
        prefix, _ = model.tokenize_audio((torch.from_numpy(audio[:end])[None], 16000))
        new = prefix[fed: fed + 10]
        if len(new) < 10:
            break
        for _ in model.stream_audio_tokens(new, state=state, max_epad_count=2, **kwargs):
            pass
        torch.cuda.synchronize()
        live_block_ms.append((time.perf_counter() - t0) * 1000)
        fed += 10
        fed_tokens += new
        if state.stop_requested:
            break
    final_tokens, _ = model.tokenize_audio((torch.from_numpy(audio[: fed * 1280])[None], 16000))
    live_segments = model._extract_segments(state.text_tokens, state.audio_tokens, None)
    report["live_simulation"] = {
        "clip": f"user_interruption/{ids[0]} (+8 s silence)",
        "blocks": len(live_block_ms), "block_ms_including_retokenize": summarize(live_block_ms),
        "fed_tokens_differing_from_offline": int(sum(a != b for a, b in zip(fed_tokens, final_tokens))),
        "fed_tokens": len(fed_tokens),
        "segments": [{"text": s.text, "start_time_s": round(s.start_time, 2)} for s in live_segments],
        "note": "re-tokenizes the whole prefix each block (O(n) per block); a production path would re-encode "
                "only the current 4 s block",
    }
    report["gpu_peak_allocated_gb"] = round(torch.cuda.max_memory_allocated() / 2**30, 2)
    (out / "bayling-eval.json").write_text(json.dumps(report, indent=2, ensure_ascii=False))
    print(json.dumps({k: v for k, v in report.items() if k != "cases"}, indent=2, ensure_ascii=False)[:5000])
    return report


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--out", required=True)
    parser.add_argument("--count", type=int, default=3)
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--temperature", type=float, default=0.6)
    parser.add_argument("--top-p", type=float, default=0.8)
    parser.add_argument("--max-duration", type=float, default=30.0)
    parser.add_argument("--no-audio", dest="audio", action="store_false")
    run(parser.parse_args())


if __name__ == "__main__":
    main()
