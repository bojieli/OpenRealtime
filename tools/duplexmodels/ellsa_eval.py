#!/usr/bin/env python3
"""ELLSA speech-only evaluation feasibility (plan stage P8, cell X0-partial).

ELLSA (tsinghua-ee/ELLSA, SALMONN branch ``ELLSA``) is a full-duplex
listen/look/speak/act model built as a speech expert (Llama-3.1-8B based, with
a streaming Zipformer2 "spear" encoder) and a vision/action expert (UniVLA /
Emu3) with shared attention (SA-MoE). This driver runs the upstream
speech-only loop (``evaluate_speech.py`` -> ``EmuVLAModel.step_onlyspeech``)
on a handful of Llama Questions items from the released test data:
S2T only (``generate=False``; spoken output would need CosyVoice2-0.5B), no
simulated actions (robot simulation is out of scope).

Deviations from upstream, all forced by this host and recorded in the output:

* ``flash_attention_2`` is hard-coded upstream; no flash-attn build exists for
  torch 2.9.1+cu128 (the only torch line with sm_120 kernels), so attention is
  switched to PyTorch SDPA (the Emu3 code ships an SDPA class).
* ``kaldifeat`` is imported at module level upstream but only used by the
  ``mamba`` encoder; the released checkpoint uses ``zipformer2`` + lhotse fbank,
  so a stub module satisfies the import.
* ``LLAMA_CKPT_PATH`` is left empty: the speech expert is built from config and
  its weights come from the ELLSA checkpoint itself (the gated Llama weights
  would only be overwritten).

Upstream semantics kept: 1 s time blocks, fbank of the whole padded clip,
``step_onlyspeech`` re-runs ``generate`` over the full history each block (no
KV reuse across blocks - a research loop, not a live runtime), stop after
``<silence><eot>`` once twice the speech length has elapsed.

License: ELLSA code and weights Apache-2.0; the speech expert derives from
Llama 3.1 (Llama 3.1 Community License); UniVLA/Emu3 components Apache-2.0.

    .runtime/duplex-plan/venvs/ellsa/bin/python tools/duplexmodels/ellsa_eval.py --count 8 \
        --out .runtime/duplex-plan/results/tasks/ellsa/ellsa-llama-questions.json
"""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import sys
import time
import types
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PLAN = ROOT / ".runtime/duplex-plan"
SRC = PLAN / "src/SALMONN-ELLSA"
DATA = PLAN / "data/ellsa/ELLSA_test_data"
HUB = Path(os.environ.get("HF_HUB_CACHE", Path.home() / ".cache/huggingface/hub"))


def snapshot(repo: str) -> Path:
    return sorted((HUB / f"models--{repo.replace('/', '--')}" / "snapshots").glob("*"))[-1]


def setup_environment() -> None:
    os.environ["ELLSA_BASE_PATH"] = str(SRC)
    os.environ["ELLSA_DATA_PATH"] = str(DATA)
    os.environ["UNIVLA_CKPT_PATH"] = str(snapshot("Yuqi1997/UniVLA") / "UNIVLA_LIBERO_VIDEO_BS192_8K")
    os.environ["VISION_VQ_PATH"] = str(snapshot("BAAI/Emu3-VisionTokenizer"))
    os.environ["LLAMA_CKPT_PATH"] = ""
    os.environ["COSY_CKPT_PATH"] = ""
    sys.modules.setdefault("kaldifeat", types.ModuleType("kaldifeat"))
    for path in (SRC / "reference/Emu3", SRC / "reference/RoboVLMs/eval/libero", SRC / "reference/RoboVLMs", SRC):
        sys.path.insert(0, str(path))


def patch_attention(implementation: str) -> None:
    from emu3.mllm import Emu3ForMix

    original = Emu3ForMix.from_pretrained.__func__

    def from_pretrained(cls, *args, **kwargs):
        kwargs["attn_implementation"] = implementation
        if kwargs.get("config_vision") is not None:
            kwargs["config_vision"]._attn_implementation = implementation
        return original(cls, *args, **kwargs)

    Emu3ForMix.from_pretrained = classmethod(from_pretrained)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--count", type=int, default=8)
    parser.add_argument("--attn", default="sdpa")
    parser.add_argument("--time-block", type=float, default=1.0)
    parser.add_argument("--out", required=True)
    args = parser.parse_args()

    setup_environment()
    import soundfile as sf
    import torch
    from lhotse import Fbank, FbankConfig

    try:
        import flash_attn  # noqa: F401
        attn = "flash_attention_2"
    except ImportError:
        attn = args.attn
    patch_attention(attn)
    from model_wrapper_emu import EmuVLAModel

    report = {"model": f"tsinghua-ee/ELLSA@{snapshot('tsinghua-ee/ELLSA').name[:12]}",
              "code": "bytedance/SALMONN branch ELLSA (reference/RoboVLMs/eval/libero)",
              "attn_implementation": attn, "task_suite": "llama_questions", "generate_speech": False,
              "time_block_s": args.time_block}
    began = time.perf_counter()
    model = EmuVLAModel(emu_hub=str(snapshot("tsinghua-ee/ELLSA")), vq_hub="", vision_hub=os.environ["VISION_VQ_PATH"],
                        device=torch.device("cuda"), speech=True, moe=True, mix=True, attn_adapter=False,
                        attn_adapter_type="None", merge_speech_lora=True, lora_modules="qkv", generate=False,
                        encoder_type="zipformer2", time_block=args.time_block, predict_action_frames=10)
    if attn != "flash_attention_2":
        model.model._use_flash_attention_2 = False
        model.model._use_sdpa = True
    report["load_seconds"] = round(time.perf_counter() - began, 1)
    report["gpu_allocated_gb_after_load"] = round(torch.cuda.memory_allocated() / 2**30, 2)

    items = json.loads((DATA / "json/llama_questions.json").read_text())["annotation"]
    fbank = Fbank(FbankConfig(num_mel_bins=128))
    records = []
    for item in items:
        if len(records) >= args.count:
            break
        wav = DATA / "llama_questions" / Path(item["path"][0]).name
        if not wav.exists():
            continue
        model.reset()
        # torchaudio 2.9 routes load() through torchcodec; soundfile reads the same PCM.
        samples, fs = sf.read(str(wav), dtype="float32", always_2d=True)
        audio = torch.from_numpy(samples.T.copy()[:1])
        frames = int(args.time_block * fs)
        speech_len = math.ceil(audio.shape[1] / frames)
        max_steps = min(int(speech_len * 12), int(300 / args.time_block))
        padded = torch.cat((audio, torch.zeros((1, int(max_steps * frames)))), dim=1)
        feature = fbank.extract(padded.squeeze(), sampling_rate=fs)
        feature = torch.as_tensor(feature)
        step_ms, outputs, hyp = [], [], ""
        t = 0
        while t < max_steps:
            torch.cuda.synchronize()
            t0 = time.perf_counter()
            with torch.no_grad():
                piece = model.step_onlyspeech(feature, feature.size(0), t, item["task"])
            torch.cuda.synchronize()
            step_ms.append((time.perf_counter() - t0) * 1000)
            outputs.append(piece)
            hyp += piece
            if piece == "<silence><eot>" and t > speech_len * 2:
                break
            t += 1
        answer = re.sub(r"<[^>]+>", " ", hyp)
        answer = " ".join(answer.split())
        first_text = next((i for i, p in enumerate(outputs) if re.sub(r"<[^>]+>", "", p).strip()), None)
        correct = item["text"].lower() in answer.lower()
        records.append({
            "wav": wav.name, "question": item["Q"], "reference": item["text"], "answer": answer,
            "contains_reference": correct, "speech_blocks": speech_len, "blocks_run": len(step_ms),
            "first_text_block": first_text,
            "response_onset_after_speech_end_s": None if first_text is None else (first_text + 1 - speech_len) * args.time_block,
            "block_ms_mean": round(sum(step_ms) / len(step_ms), 1), "block_ms_max": round(max(step_ms), 1),
            "raw_steps": outputs,
        })
        print(json.dumps({k: records[-1][k] for k in ("question", "reference", "answer", "first_text_block",
                                                        "speech_blocks", "block_ms_mean")}, ensure_ascii=False),
              flush=True)
    report["items"] = records
    report["accuracy_contains_reference"] = round(sum(r["contains_reference"] for r in records) / max(len(records), 1), 3)
    report["gpu_peak_allocated_gb"] = round(torch.cuda.max_memory_allocated() / 2**30, 2)
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(report, indent=2, ensure_ascii=False))
    print(json.dumps({k: v for k, v in report.items() if k != "items"}, indent=2))


if __name__ == "__main__":
    main()
