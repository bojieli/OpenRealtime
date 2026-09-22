"""Interaction-prediction service for the duplex plan (stage P6, cells I0 and I1).

One FastAPI process (default ``127.0.0.1:9130``) serves four small predictors.
They predict *different quantities*, and each route returns exactly one family
of typed evidence. Nothing here is a universal end-of-turn score, and nothing
here takes an action: the service is an observer (plan section 7.7).

``POST /v1/endpoint/smart-turn``
    Body: float32 little-endian mono 16 kHz PCM - the user's current turn up to
    now (Smart Turn uses the last 8 s; shorter input is zero-padded at the front
    exactly like upstream ``inference.py``). Query ``variant=gpu|cpu`` selects
    the fp32 CUDA or the int8 CPU ONNX export (default from ``--smart-turn-variant``).
    Returns ``{"evidence": "endpoint_probability", "basis": "acoustic",
    "probability": P(turn complete at the end of the audio), ...}``.

``POST /v1/endpoint/livekit``
    Body: JSON ``{"messages": [{"role": "user"|"assistant", "content": str}, ...],
    "language": "en"}``; the last message must be the user's (possibly partial)
    transcript. Model ``livekit/turn-detector`` multilingual ``v0.4.1-intl``
    (the variant the model card recommends for English too); preprocessing is the
    plugin's own (``livekit-plugins-turn-detector`` 1.8.2): NFKC, lower-case,
    strip punctuation except ``'`` and ``-``, collapse whitespace, merge adjacent
    same-role turns, last 6 turns, Qwen chat template with the final
    ``<|im_end|>`` removed, left-truncated to 128 tokens.
    Returns ``{"evidence": "endpoint_probability", "basis": "transcript",
    "probability": P(<|im_end|> next), "language_threshold": ...}``.

``POST /v1/forecast/vap``
    Body: float32 LE **planar** stereo 16 kHz, ``[user samples..., assistant
    samples...]`` with equal lengths (``layout=interleaved`` is also accepted).
    The assistant channel must be audio that was actually played, aligned to the
    same clock as the microphone. Returns the stereo VAP model's
    ``{"evidence": "voice_activity_forecast", "p_active": {speaker: [4 bins]},
    "bins_s": [[0,0.2],[0.2,0.6],[0.6,1.2],[1.2,2.0]], ...}`` at the last frame,
    plus the VAP ``p_now``/``p_future`` next-speaker aggregates and the model's
    own current-activity estimate.

``POST /v1/forecast/dualturn``
    Same body. Returns DualTurn's per-channel heads, grouped by evidence type:
    ``voice_activity_now``, ``voice_activity_forecast`` (240/480/960/2000 ms),
    and ``turn_event_likelihood`` (``eot``, ``hold``, ``bot``, ``backchannel``).

Endpoint-hook routes (the Go runtime's ``-turn-end-url`` contract in
``adapters/turnend``: body = the last <= 8 s of user audio as float32 LE mono
16 kHz, optional header ``X-OpenRealtime-Transcript`` with the user's turn so
far, answer ``{"probability": P(turn complete), "model": "<id@revision>"}``).
Every one of them also returns its own evidence unchanged, and says which rule
turned that evidence into the single probability:

``POST /v1/endpoint/smart-turn``   as above (``?score=calibrated`` applies the frozen Platt map).
``POST /v1/endpoint/livekit``      audio body ignored, transcript from the header; without a transcript
                                   it answers a flagged neutral value, never model evidence.
``POST /v1/endpoint/vap``          mono user audio, assistant channel assumed silent; probability =
                                   VAP's next-speaker aggregate for the assistant (``?aggregate=p_future``
                                   by default, ``p_now`` available).
``POST /v1/endpoint/dualturn``     mono user audio, assistant channel assumed silent; probability =
                                   eot/(eot+hold) for the user channel over the last 400 ms.
``POST /v1/endpoint/fusion``       the I0 calibrated fusion of Smart Turn and LiveKit; weights are read
                                   from ``--calibration`` (results/interaction/i0/calibration.json,
                                   re-read when the file changes) and it falls back to Platt-calibrated
                                   Smart Turn when no transcript is supplied.

Two further predictors need their own pinned environments and run as separate
processes of this same file (``deploy/duplex/services/turn.sh start x2turn|soulx``):

``POST /v1/turn-state/x2turn`` (:9131, venv ``x2turn``, large-GPU lease)
    Body: float32 LE mono 16 kHz. X2-Turn-4B-0812 re-decodes the buffer with the
    transformers path and returns ``{"evidence": "turn_state_distribution",
    "frames": [{"start_ms","end_ms","label","p": {idle, noidle, speaking,
    turn_end, backchannel, uncertain}}], "transcript"}`` (``frames=0`` = all).

``POST /v1/turn-state/soulx`` (:9132, venv ``soulx``)
    Body: float32 LE mono 16 kHz. A fresh SoulX-Duplug streaming session is fed
    the buffer in 160 ms chunks; returns ``{"evidence": "turn_state_decisions",
    "events": [{"available_s", "state": "speak", "text"}]}`` (``every=1`` also
    lists idle/nonidle chunks). ``speak`` is the model's own endpoint decision.

Causality. Every route answers for the instant at the end of the supplied
audio and never sees later samples. VAP (CPC encoder with symmetric conv
padding) is evaluated on exactly the samples supplied, so its last frame is
computed with zero right-padding rather than future audio. DualTurn only uses
complete 80 ms Mimi frames: trailing samples that do not fill a frame are
dropped and ``frame_end_s`` says where the evidence ends. ``frames=K`` returns
the last K frames as a time series; those earlier frames are only equal to
what a streaming run would have produced if the model is strictly causal
(``tools/duplexmodels/overlap_eval.py --check-causality`` measures this).

DualTurn fidelity. The published checkpoint was trained with per-task
softmax-weighted layer attention (``task_layer_weights.*``, config
``per_task_layer_attention: true``); the ``modeling_dualturn.py`` shipped on
the Hub discards those weights and feeds only the last layer to every head.
This service implements the research-repo inference path (``dualturn/model/
model.py`` mode ``inference``) and reports it as ``head_input``.

Every response carries ``model`` (id, revision, file) and ``latency_ms``.

Start: ``deploy/duplex/services/turn.sh`` (or ``python tools/duplexmodels/turn_server.py --port 9130``).
"""

from __future__ import annotations

import argparse
import asyncio
import glob
import json
import logging
import math
import os
import re
import sys
import threading
import time
import unicodedata
from dataclasses import dataclass
from typing import Any, Optional

import numpy as np
from fastapi import FastAPI, Request  # module level: route annotations are resolved from module globals
from fastapi.responses import JSONResponse

log = logging.getLogger("turn_server")

HF_HUB = os.path.expanduser(os.environ.get("HF_HUB_CACHE", "~/.cache/huggingface/hub"))
DUPLEX_SRC = os.environ.get(
    "DUPLEX_PLAN_SRC",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".runtime", "duplex-plan", "src"),
)

SMART_TURN_REPO = "pipecat-ai/smart-turn-v3"
SMART_TURN_REVISION = "f766f81d3cfdf7737ac64aad813d91bbfd56bf93"
LIVEKIT_REPO = "livekit/turn-detector"
LIVEKIT_TAG = "v0.4.1-intl"
LIVEKIT_REVISION = "87e35fcb1e60a569bea70346191c4886ea92e281"
DUALTURN_REPO = "anyreach-ai/dualturn-qwen2.5-mimi-0.5B"
DUALTURN_REVISION = "d7abba2c0c8d1ab8e992879c6a186384e00f94cb"
SOULX_REPO = "Soul-AILab/SoulX-Duplug-0.6B"
SOULX_REVISION = "61701c4ab8193cc1ee2220d3848872ac6c720142"
X2TURN_REPO = "x-square-robot/X2-Turn-4B-0812"
X2TURN_REVISION = "49c62a9b1170f2ab5168e763b84c2ac5c6b5e1ee"
MIMI_REPO = "kyutai/mimi"
MIMI_REVISION = "89091b3e466eb6a9d11e537bf26b144f194978f7"
QWEN_BASE_REPO = "Qwen/Qwen2.5-0.5B"
QWEN_BASE_REVISION = "060db6499f32faf8b98477b0a26969ef7d8b9987"
VAP_REPO = "ErikEkstedt/VoiceActivityProjection"
VAP_STATE_DICT = "example/VAP_3mmz3t0u_50Hz_ad20s_134-epoch9-val_2.56.pt"
VAP_CPC = "assets/checkpoints/cpc/60k_epoch4-d0f474de.pt"

SAMPLE_RATE = 16000


def snapshot(repo: str, revision: str) -> str:
    path = os.path.join(HF_HUB, "models--" + repo.replace("/", "--"), "snapshots", revision)
    if not os.path.isdir(path):
        raise FileNotFoundError(f"{repo}@{revision} is not in the HF cache ({path}); download it first")
    return path


def git_head(path: str) -> str:
    try:
        head = open(os.path.join(path, ".git", "HEAD")).read().strip()
        if head.startswith("ref:"):
            ref = head.split(" ", 1)[1]
            ref_path = os.path.join(path, ".git", ref)
            if os.path.exists(ref_path):
                return open(ref_path).read().strip()
            for line in open(os.path.join(path, ".git", "packed-refs")):
                if line.strip().endswith(ref):
                    return line.split()[0]
        return head
    except OSError:
        return "unknown"


def ms(seconds: float) -> float:
    return round(seconds * 1000.0, 3)


# --------------------------------------------------------------------------
# Smart Turn v3.2 (acoustic endpoint probability)


class SmartTurn:
    def __init__(self, default_variant: str, cpu_threads: int, fp32_device: str = "cuda"):
        import onnxruntime as ort
        from transformers import WhisperFeatureExtractor

        try:
            ort.preload_dlls()  # CUDA/cuDNN from the torch wheels
        except Exception:  # noqa: BLE001
            pass
        root = snapshot(SMART_TURN_REPO, SMART_TURN_REVISION)
        self.extractor = WhisperFeatureExtractor(chunk_length=8)
        self.sessions: dict[str, Any] = {}
        self.files = {"gpu": "smart-turn-v3.2-gpu.onnx", "cpu": "smart-turn-v3.2-cpu.onnx"}
        self.threads = cpu_threads
        for variant, file in self.files.items():
            so = ort.SessionOptions()
            so.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
            so.inter_op_num_threads = 1
            so.intra_op_num_threads = cpu_threads
            so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
            use_cuda = variant == "gpu" and fp32_device.startswith("cuda")
            providers = ["CUDAExecutionProvider", "CPUExecutionProvider"] if use_cuda else ["CPUExecutionProvider"]
            session = ort.InferenceSession(os.path.join(root, file), sess_options=so, providers=providers)
            self.sessions[variant] = session
        self.default_variant = default_variant
        self.locks = {k: threading.Lock() for k in self.sessions}

    def describe(self) -> dict:
        return {
            "id": SMART_TURN_REPO,
            "revision": SMART_TURN_REVISION,
            "files": self.files,
            "default_variant": self.default_variant,
            "providers": {k: s.get_providers()[0] for k, s in self.sessions.items()},
            "cpu_intra_op_threads": self.threads,
            "context_seconds": 8,
        }

    def predict(self, audio: np.ndarray, variant: Optional[str]) -> dict:
        variant = variant or self.default_variant
        if variant not in self.sessions:
            raise ValueError(f"variant must be one of {sorted(self.sessions)}")
        began = time.perf_counter()
        seconds = len(audio) / SAMPLE_RATE
        n = 8 * SAMPLE_RATE
        if len(audio) > n:
            audio = audio[-n:]
        elif len(audio) < n:
            audio = np.pad(audio, (n - len(audio), 0))
        features = self.extractor(
            audio, sampling_rate=SAMPLE_RATE, return_tensors="np", padding="max_length",
            max_length=n, truncation=True, do_normalize=True,
        ).input_features.astype(np.float32)
        featured = time.perf_counter()
        with self.locks[variant]:
            out = self.sessions[variant].run(None, {"input_features": features})
        done = time.perf_counter()
        probability = float(out[0].reshape(-1)[0])  # the export ends in a sigmoid
        return {
            "evidence": "endpoint_probability",
            "basis": "acoustic",
            "subject": "user",
            "probability": probability,
            "meaning": "P(the user's turn is complete at the end of the supplied audio)",
            "model": f"{SMART_TURN_REPO}@{SMART_TURN_REVISION}",
            "model_detail": {"id": SMART_TURN_REPO, "revision": SMART_TURN_REVISION, "file": self.files[variant]},
            "runtime": {
                "variant": variant,
                "provider": self.sessions[variant].get_providers()[0],
                "cpu_intra_op_threads": self.threads,
            },
            "audio": {"seconds": round(seconds, 4), "context_seconds_used": round(min(seconds, 8.0), 4)},
            "latency_ms": {"features": ms(featured - began), "inference": ms(done - featured), "total": ms(done - began)},
        }


# --------------------------------------------------------------------------
# LiveKit turn detector (transcript endpoint probability)


class LiveKit:
    MAX_TOKENS = 128
    MAX_TURNS = 6

    def __init__(self, threads: int):
        import onnxruntime as ort
        from transformers import AutoTokenizer

        root = snapshot(LIVEKIT_REPO, LIVEKIT_REVISION)
        so = ort.SessionOptions()
        so.intra_op_num_threads = threads
        so.inter_op_num_threads = 1
        so.add_session_config_entry("session.dynamic_block_base", "4")
        self.session = ort.InferenceSession(os.path.join(root, "onnx", "model_q8.onnx"), sess_options=so,
                                            providers=["CPUExecutionProvider"])
        self.tokenizer = AutoTokenizer.from_pretrained(root, truncation_side="left")
        self.languages = json.load(open(os.path.join(root, "languages.json")))
        self.threads = threads
        self.lock = threading.Lock()

    def describe(self) -> dict:
        return {"id": LIVEKIT_REPO, "tag": LIVEKIT_TAG, "revision": LIVEKIT_REVISION, "file": "onnx/model_q8.onnx",
                "variant": "multilingual", "provider": "CPUExecutionProvider", "cpu_intra_op_threads": self.threads,
                "preprocessing": "livekit-plugins-turn-detector 1.8.2 multilingual"}

    @staticmethod
    def normalize(text: str) -> str:
        if not text:
            return ""
        text = unicodedata.normalize("NFKC", text.lower())
        text = "".join(ch for ch in text if not (unicodedata.category(ch).startswith("P") and ch not in ["'", "-"]))
        return re.sub(r"\s+", " ", text).strip()

    def format(self, messages: list[dict]) -> str:
        merged: list[dict] = []
        for message in messages:
            if message.get("role") not in ("user", "assistant") or not message.get("content"):
                continue
            content = self.normalize(message["content"])
            if merged and merged[-1]["role"] == message["role"]:
                merged[-1]["content"] += f" {content}"
            else:
                merged.append({"role": message["role"], "content": content})
        merged = merged[-self.MAX_TURNS:]
        text = self.tokenizer.apply_chat_template(merged, add_generation_prompt=False, add_special_tokens=False,
                                                  tokenize=False)
        return text[: text.rfind("<|im_end|>")]

    def predict(self, messages: list[dict], language: str) -> dict:
        if not messages or messages[-1].get("role") != "user":
            raise ValueError("the last message must be the user's transcript")
        began = time.perf_counter()
        # The plugin keeps the last MAX_HISTORY_TURNS messages before merging.
        text = self.format(messages[-self.MAX_TURNS:])
        ids = self.tokenizer(text, add_special_tokens=False, return_tensors="np", max_length=self.MAX_TOKENS,
                             truncation=True)["input_ids"].astype("int64")
        formatted = time.perf_counter()
        with self.lock:
            out = self.session.run(None, {"input_ids": ids})
        done = time.perf_counter()
        probability = float(out[0].reshape(-1)[-1])
        lang = self.languages.get(language) or self.languages.get(language.split("-")[0])
        return {
            "evidence": "endpoint_probability",
            "basis": "transcript",
            "subject": "user",
            "probability": probability,
            "meaning": "P(<|im_end|> follows the last user message) - semantic completion of the transcript",
            "language": language,
            "language_threshold": None if lang is None else lang["threshold"],
            "model": f"{LIVEKIT_REPO}@{LIVEKIT_REVISION}",
            "model_detail": {"id": LIVEKIT_REPO, "revision": LIVEKIT_REVISION, "tag": LIVEKIT_TAG,
                             "file": "onnx/model_q8.onnx"},
            "runtime": {"provider": "CPUExecutionProvider", "cpu_intra_op_threads": self.threads},
            "input": {"text": text, "tokens": int(ids.shape[1])},
            "latency_ms": {"preprocess": ms(formatted - began), "inference": ms(done - formatted), "total": ms(done - began)},
        }


# --------------------------------------------------------------------------
# Stereo VAP (voice-activity projection)


class VAP:
    BINS_S = [[0.0, 0.2], [0.2, 0.6], [0.6, 1.2], [1.2, 2.0]]

    def __init__(self, device: str, max_context_s: float):
        import torch

        root = os.path.abspath(os.path.join(DUPLEX_SRC, "VoiceActivityProjection"))
        sys.path.insert(0, root)
        from vap.model import VapConfig, VapGPT  # noqa: E402

        # vap.model switches on global deterministic algorithms at import; that
        # would break unrelated kernels (DualTurn) in this process.
        torch.use_deterministic_algorithms(False)
        self.torch = torch
        self.device = device
        self.model = VapGPT(VapConfig())
        state = torch.load(os.path.join(root, VAP_STATE_DICT), map_location="cpu")
        self.model.load_state_dict(state, strict=True)
        states = self.model.objective.codebook.decode(torch.arange(self.model.objective.n_classes))  # (256, 2, 4)
        self.states = states.float().to(device)
        self.model = self.model.to(device).eval()
        self.max_context_s = max_context_s
        self.source_revision = git_head(root)
        self.lock = threading.Lock()

    def describe(self) -> dict:
        return {"id": VAP_REPO, "revision": self.source_revision, "file": VAP_STATE_DICT, "cpc": VAP_CPC,
                "device": self.device, "frame_hz": 50, "bins_s": self.BINS_S, "max_context_s": self.max_context_s,
                "training_chunk_s": 20}

    def predict(self, user: np.ndarray, assistant: np.ndarray, frames: int) -> dict:
        torch = self.torch
        began = time.perf_counter()
        total_s = len(user) / SAMPLE_RATE
        keep = int(self.max_context_s * SAMPLE_RATE)
        if len(user) > keep:
            user, assistant = user[-keep:], assistant[-keep:]
        wave = torch.from_numpy(np.stack([user, assistant])[None]).float().to(self.device)
        with self.lock, torch.inference_mode():
            out = self.model(wave)
            probs = out["logits"].softmax(dim=-1)  # (1, T, 256)
            vad = out["vad"].sigmoid()  # (1, T, 2)
            p_active = torch.einsum("btk,kcn->btcn", probs, self.states)  # (1, T, 2 speakers, 4 bins)
            p_now = self.model.objective.probs_next_speaker_aggregate(probs, from_bin=0, to_bin=1)
            p_future = self.model.objective.probs_next_speaker_aggregate(probs, from_bin=2, to_bin=3)
            entropy = -(probs * probs.clamp_min(1e-12).log2()).sum(-1)
            if self.device.startswith("cuda"):
                torch.cuda.synchronize()
        done = time.perf_counter()
        n = probs.shape[1]
        k = max(1, min(frames, n))
        sl = slice(n - k, n)
        pa = p_active[0, sl].cpu().numpy()
        vd = vad[0, sl].cpu().numpy()
        pn = p_now[0, sl].cpu().numpy()
        pf = p_future[0, sl].cpu().numpy()
        en = entropy[0, sl].cpu().numpy()
        frame_end_s = total_s  # the last frame ends at the last supplied sample

        def series(a):
            return a.tolist() if k > 1 else a[-1].tolist()

        return {
            "evidence": "voice_activity_forecast",
            "subjects": ["user", "assistant"],
            "meaning": "P(speaker is active in each future bin), marginalised from VAP's 256-state projection window",
            "frame_hz": 50,
            "frame_end_s": round(frame_end_s, 4),
            "frames": k,
            "bins_s": self.BINS_S,
            "p_active": {"user": series(pa[:, 0, :]), "assistant": series(pa[:, 1, :])},
            "next_speaker": {
                "meaning": "VAP p_now (bins 0-1, 0-0.6 s) and p_future (bins 2-3, 0.6-2.0 s): relative share of "
                           "projected activity per speaker; a floor-ownership forecast, not intent",
                "p_now": {"user": series(pn[:, 0]), "assistant": series(pn[:, 1])},
                "p_future": {"user": series(pf[:, 0]), "assistant": series(pf[:, 1])},
            },
            "voice_activity_now": {"user": series(vd[:, 0]), "assistant": series(vd[:, 1])},
            "entropy_bits": series(en),
            "model": f"{VAP_REPO}@{self.source_revision}",
            "model_detail": {"id": VAP_REPO, "revision": self.source_revision, "file": VAP_STATE_DICT},
            "runtime": {"device": self.device},
            "audio": {"seconds": round(total_s, 4), "context_seconds_used": round(len(user) / SAMPLE_RATE, 4)},
            "latency_ms": {"inference": ms(done - began), "total": ms(done - began)},
        }


# --------------------------------------------------------------------------
# DualTurn (dual-channel Mimi + Qwen2.5-0.5B)


class DualTurn:
    TASKS = ["eot", "hold", "bot", "bc", "vad", "fvad"]
    FVAD_MS = [240, 480, 960, 2000]
    FRAME_24K = 1920  # 80 ms at 24 kHz
    FRAME_16K = 1280

    def __init__(self, device: str, max_context_s: float):
        import torch
        import torch.nn as nn
        from safetensors.torch import load_file
        from transformers import MimiModel, Qwen2Config, Qwen2ForCausalLM

        self.torch = torch
        self.device = device
        self.max_context_s = max_context_s
        root = snapshot(DUALTURN_REPO, DUALTURN_REVISION)
        cfg = json.load(open(os.path.join(root, "config.json")))
        self.config = cfg
        raw = load_file(os.path.join(root, "model.safetensors"), device="cpu")
        qcfg = Qwen2Config.from_pretrained(snapshot(QWEN_BASE_REPO, QWEN_BASE_REVISION))
        qcfg._attn_implementation = "sdpa"
        backbone = Qwen2ForCausalLM(qcfg)
        bsd = {}
        for key, value in raw.items():
            if key.startswith("backbone.model.model."):
                bsd["model." + key[len("backbone.model.model."):]] = value
            elif key.startswith("backbone.model.lm_head."):
                bsd["lm_head." + key[len("backbone.model.lm_head."):]] = value
        missing, unexpected = backbone.load_state_dict(bsd, strict=False)
        if missing or unexpected:
            raise RuntimeError(f"DualTurn backbone mismatch: missing={missing[:4]} unexpected={unexpected[:4]}")
        # The heads only read hidden states; drop the 151936-token LM head from the GPU.
        # Inputs are Mimi embeddings, so the token embedding table is unused as well.
        backbone.lm_head = nn.Identity()
        backbone.model.embed_tokens = nn.Embedding(1, qcfg.hidden_size)
        self.backbone = backbone.model.to(device).eval().float()
        D, H = cfg["hidden_dim"], cfg.get("head_hidden_dim", 256)

        def linear(prefix, i, o):
            layer = nn.Linear(i, o)
            layer.weight.data = raw[prefix + ".weight"].float()
            layer.bias.data = raw[prefix + ".bias"].float()
            return layer

        self.proj = nn.Sequential(linear("mimi_projection.proj.0", 1024, D), nn.GELU(),
                                  linear("mimi_projection.proj.2", D, D)).to(device).eval()
        heads = {}
        for task in ["eot", "hold", "bot", "bc"]:
            for ch in (0, 1):
                name = f"{task}_head_ch{ch}"
                heads[name] = nn.Sequential(linear(f"{name}.0", D, H), nn.GELU(), nn.Dropout(0.1),
                                            linear(f"{name}.3", H, 1))
        for ch in (0, 1):
            heads[f"vad_head_ch{ch}"] = linear(f"vad_head_ch{ch}", D, 1)
        heads["fvad_head"] = linear("fvad_head", D, 8)
        self.heads = nn.ModuleDict(heads).to(device).eval()
        self.layer_weights = {t: torch.softmax(raw[f"task_layer_weights.{t}"].float(), 0).to(device) for t in self.TASKS}
        self.mimi = MimiModel.from_pretrained(snapshot(MIMI_REPO, MIMI_REVISION)).to(device).eval()
        self.lock = threading.Lock()

    def describe(self) -> dict:
        return {"id": DUALTURN_REPO, "revision": DUALTURN_REVISION, "mimi": f"{MIMI_REPO}@{MIMI_REVISION}",
                "qwen_config": f"{QWEN_BASE_REPO}@{QWEN_BASE_REVISION}", "device": self.device, "frame_hz": 12.5,
                "fvad_horizons_ms": self.FVAD_MS, "head_input": "per-task softmax layer attention (research repo)",
                "max_context_s": self.max_context_s, "training_window_s": 30,
                "note": "checkpoint card calls it an intermediate checkpoint"}

    def encode(self, wav24):
        torch = self.torch
        x = wav24[None, None]
        enc = self.mimi.encoder(x)
        et = self.mimi.encoder_transformer(enc.transpose(1, 2))
        if hasattr(et, "last_hidden_state"):
            et = et.last_hidden_state
        return self.mimi.downsample(et.transpose(1, 2)).squeeze(0).T.float()  # (T, 512)

    def predict(self, user: np.ndarray, assistant: np.ndarray, frames: int, head_input: str = "layer-attention") -> dict:
        import torchaudio.functional as AF

        torch = self.torch
        began = time.perf_counter()
        total_s = len(user) / SAMPLE_RATE
        usable = (len(user) // self.FRAME_16K) * self.FRAME_16K  # complete 80 ms frames only
        if usable == 0:
            raise ValueError("need at least 80 ms of audio")
        user, assistant = user[:usable], assistant[:usable]
        keep = int(self.max_context_s * SAMPLE_RATE) // self.FRAME_16K * self.FRAME_16K
        if len(user) > keep:
            user, assistant = user[-keep:], assistant[-keep:]
        with self.lock, torch.inference_mode():
            wav = torch.from_numpy(np.stack([user, assistant])).float().to(self.device)
            wav24 = AF.resample(wav, SAMPLE_RATE, 24000)
            f0, f1 = self.encode(wav24[0]), self.encode(wav24[1])
            n = min(f0.shape[0], f1.shape[0], len(user) // self.FRAME_16K)
            emb = self.proj(torch.cat([f0[:n], f1[:n]], dim=-1)[None])
            encoded = time.perf_counter()
            out = self.backbone(inputs_embeds=emb, output_hidden_states=True, return_dict=True)
            stack = torch.stack(out.hidden_states, 0)  # (25, 1, T, D)
            if head_input == "last-layer":
                h = {t: out.hidden_states[-1] for t in self.TASKS}
            else:
                h = {t: (stack * self.layer_weights[t][:, None, None, None]).sum(0) for t in self.TASKS}
            H = self.heads

            def two(task, name):
                return torch.sigmoid(torch.stack([H[f"{name}_ch0"](h[task]).squeeze(-1),
                                                  H[f"{name}_ch1"](h[task]).squeeze(-1)], -1))[0]

            res = {
                "eot": two("eot", "eot_head"), "hold": two("hold", "hold_head"), "bot": two("bot", "bot_head"),
                "bc": two("bc", "bc_head"), "vad": two("vad", "vad_head"),
                "fvad": torch.sigmoid(H["fvad_head"](h["fvad"]))[0],
            }
            res = {k: v.float().cpu().numpy() for k, v in res.items()}
        done = time.perf_counter()
        T = res["vad"].shape[0]
        k = max(1, min(frames, T))
        sl = slice(T - k, T)

        def series(a):
            return a[sl].tolist() if k > 1 else a[-1].tolist()

        frame_end_s = usable / SAMPLE_RATE
        return {
            "evidence": ["voice_activity_now", "voice_activity_forecast", "turn_event_likelihood"],
            "subjects": ["user", "assistant"],
            "frame_hz": 12.5,
            "frame_end_s": round(frame_end_s, 4),
            "frames": k,
            "voice_activity_now": {"user": series(res["vad"][:, 0]), "assistant": series(res["vad"][:, 1])},
            "voice_activity_forecast": {
                "meaning": "P(speaker active) averaged over the next horizon",
                "horizons_ms": self.FVAD_MS,
                "user": series(res["fvad"][:, 0:4]), "assistant": series(res["fvad"][:, 4:8]),
            },
            "turn_event_likelihood": {
                "meaning": "per-frame event heads trained on offline labels: eot = offset where the other "
                           "speaker takes the floor within 4 s; hold = other offsets; bot = onset of a >=1 s "
                           "turn after the other speaker; backchannel = isolated <=1 s utterance",
                "eot": {"user": series(res["eot"][:, 0]), "assistant": series(res["eot"][:, 1])},
                "hold": {"user": series(res["hold"][:, 0]), "assistant": series(res["hold"][:, 1])},
                "bot": {"user": series(res["bot"][:, 0]), "assistant": series(res["bot"][:, 1])},
                "backchannel": {"user": series(res["bc"][:, 0]), "assistant": series(res["bc"][:, 1])},
            },
            "model": f"{DUALTURN_REPO}@{DUALTURN_REVISION}",
            "model_detail": {"id": DUALTURN_REPO, "revision": DUALTURN_REVISION, "file": "model.safetensors",
                      "mimi": f"{MIMI_REPO}@{MIMI_REVISION}"},
            "runtime": {"device": self.device, "head_input": head_input},
            "audio": {"seconds": round(total_s, 4), "context_seconds_used": round(len(user) / SAMPLE_RATE, 4),
                      "dropped_tail_ms": round((total_s - frame_end_s) * 1000, 1)},
            "latency_ms": {"encode": ms(encoded - began), "backbone_heads": ms(done - encoded), "total": ms(done - began)},
        }



# --------------------------------------------------------------------------
# SoulX-Duplug (GLM-4-Voice tokenizer + Qwen3-0.6B LoRA + SenseVoice cascade
# ASR). Pinned upstream environment (transformers 4.52, funasr, WeTextProcessing),
# so it runs in its own venv (``--models soulx``, conventionally on :9132).


class SoulXDuplug:
    CHUNK = 2560  # 160 ms at 16 kHz; upstream also reads 40 ms ahead of each chunk

    def __init__(self, device: str, language: str):
        import tempfile

        import torch
        from omegaconf import OmegaConf

        root = os.path.abspath(os.path.join(DUPLEX_SRC, "SoulX-Duplug"))
        sys.path.insert(0, root)
        weights = snapshot(SOULX_REPO, SOULX_REVISION)
        cfg = OmegaConf.load(os.path.join(root, "config", "config.yaml"))
        cfg.model_config.glm_tokenizer_path = os.path.join(weights, "glm-4-voice-tokenizer")
        cfg.model_config.model_name = os.path.join(weights, "Qwen3-0.6B-expand_vocab_v2")
        cfg.model_config.init_ckpt_path_lora = os.path.join(weights, "SoulX-Duplug", "SoulX-Duplug-0.6B-Bilingual.pth")
        cfg.infer_config.device = device
        cfg.infer_config.asr = {"model_name": "sensevoice", "language": language}  # upstream advice for English
        handle = tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False)
        OmegaConf.save(cfg, handle.name)
        from service.model import TurnModel  # noqa: E402

        self.torch = torch
        self.model = TurnModel(config_path=handle.name)
        self.device = device
        self.language = language
        self.source_revision = git_head(root)
        self.max_wait_num = int(cfg.infer_config.max_wait_num)
        self.lock = threading.Lock()

    def describe(self) -> dict:
        return {"id": SOULX_REPO, "revision": SOULX_REVISION, "code_revision": self.source_revision,
                "device": self.device, "chunk_ms": 160, "lookahead_ms": 40, "asr": f"SenseVoiceSmall ({self.language})",
                "max_wait_num": self.max_wait_num,
                "runtime": "fresh upstream TurnModel session per request, fed 160 ms chunks in order"}

    def predict(self, audio: np.ndarray, every: bool) -> dict:
        began = time.perf_counter()
        events = []
        with self.lock:
            self.model.reset()
            self.model.past_state = None
            for i in range(0, len(audio) - self.CHUNK + 1, self.CHUNK):
                out = self.model.process(audio[i:i + self.CHUNK].astype(np.float32))
                available_s = (i + self.CHUNK) / SAMPLE_RATE
                if every or out["state"] == "speak":
                    events.append({"available_s": round(available_s, 3), "state": out["state"],
                                   **({"text": out.get("text", "")} if out["state"] == "speak" else {})})
        done = time.perf_counter()
        return {
            "evidence": "turn_state_decisions",
            "subject": "user",
            "meaning": "SoulX-Duplug streaming states per 160 ms chunk (idle / nonidle / speak); 'speak' is the "
                       "model's own endpoint decision (semantic completion + silence, with a max_wait_num "
                       "silence fallback). available_s is when the decision exists: the end of the audio pushed "
                       "so far (the chunk it describes ends 40 ms earlier)",
            "events": events,
            "audio": {"seconds": round(len(audio) / SAMPLE_RATE, 4)},
            "model": f"{SOULX_REPO}@{SOULX_REVISION}",
            "model_detail": {"id": SOULX_REPO, "revision": SOULX_REVISION, "code_revision": self.source_revision},
            "runtime": {"device": self.device, "asr": f"SenseVoiceSmall ({self.language})"},
            "latency_ms": {"total": ms(done - began),
                           "per_chunk_mean": ms((done - began) / max(1, len(audio) // self.CHUNK))},
        }


# --------------------------------------------------------------------------
# X2-Turn (Voxtral Mini 4B Realtime + 80 ms turn-state head). Needs
# transformers>=5.10 and the X2-Turn voxtral-realtime package, so it runs in its
# own venv (``--models x2turn``, conventionally on :9131) and needs the large
# GPU lease (~10 GB bf16).


class X2Turn:
    def __init__(self, device: str):
        import torch
        from transformers import AutoProcessor
        from voxtral_realtime.transformers import load_mtp_checkpoint

        root = snapshot(X2TURN_REPO, X2TURN_REVISION)
        self.torch = torch
        self.device = device
        self.processor = AutoProcessor.from_pretrained(root)
        self.model = load_mtp_checkpoint(root, device=device, dtype=torch.bfloat16).eval()
        self.lock = threading.Lock()

    def describe(self) -> dict:
        return {"id": X2TURN_REPO, "revision": X2TURN_REVISION, "device": self.device, "dtype": "bfloat16",
                "frame_ms": 80, "delay_ms": 480, "labels": ["idle", "noidle", "speaking", "turn_end", "backchannel",
                                                             "uncertain"],
                "runtime": "transformers offline re-decode of the supplied buffer (not the patched-vLLM stream)"}

    def predict(self, audio: np.ndarray, frames: int) -> dict:
        from mistral_common.tokens.tokenizers.audio import Audio
        from voxtral_realtime.transformers import infer_asr_turn

        began = time.perf_counter()
        clip = Audio(audio_array=audio.astype(np.float32), sampling_rate=SAMPLE_RATE, format="wav")
        with self.lock:
            result = infer_asr_turn(self.model, self.processor, clip)
            if self.device.startswith("cuda"):
                self.torch.cuda.synchronize()
        done = time.perf_counter()
        rows = result.turn_frames
        k = len(rows) if frames <= 0 else max(1, min(frames, len(rows)))
        rows = rows[len(rows) - k:]
        return {
            "evidence": "turn_state_distribution",
            "subject": "user",
            "meaning": "per-80 ms softmax over X2-Turn's six turn states (turn_end = user appears finished; "
                       "backchannel = short acknowledgment); predictions, not commands",
            "frame_ms": result.frame_ms,
            "audio": {"seconds": round(len(audio) / SAMPLE_RATE, 4)},
            "transcript": result.transcript,
            "frames": [{"index": f.index, "start_ms": f.start_ms, "end_ms": f.end_ms, "label": f.label,
                        "p": {k2: round(v, 5) for k2, v in f.probabilities.items()}} for f in rows],
            "model": f"{X2TURN_REPO}@{X2TURN_REVISION}",
            "model_detail": {"id": X2TURN_REPO, "revision": X2TURN_REVISION, "file": "model.safetensors"},
            "runtime": {"device": self.device, "dtype": "bfloat16"},
            "latency_ms": {"total": ms(done - began)},
        }

# --------------------------------------------------------------------------
# Endpoint-hook adapters (adapters/turnend contract: body = the last <= 8 s of
# user audio ending at the pause, float32 LE mono 16 kHz; optional header
# X-OpenRealtime-Transcript = url.PathEscape(transcript of the turn so far);
# answer {"probability": P(turn complete), "model": "<id@revision>"}).

TRANSCRIPT_HEADER = "X-OpenRealtime-Transcript"
DEFAULT_CALIBRATION = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".runtime", "duplex-plan",
                                   "results", "interaction", "i0", "calibration.json")


def _logit(p: float) -> float:
    p = min(max(float(p), 1e-6), 1 - 1e-6)
    return math.log(p / (1 - p))


def _sigmoid(x: float) -> float:
    return 1.0 / (1.0 + math.exp(-x))


class Calibration:
    """Frozen logistic maps fitted by endpoint_eval.py analyze (I0 calibration split); re-read when the file changes."""

    def __init__(self, path: str):
        self.path = os.path.abspath(path)
        self.mtime = None
        self.data: dict = {}

    def get(self) -> dict:
        try:
            mtime = os.path.getmtime(self.path)
        except OSError:
            return {}
        if mtime != self.mtime:
            self.data = json.load(open(self.path))
            self.mtime = mtime
        return self.data

    def platt(self, name: str, p: float) -> Optional[float]:
        entry = self.get().get("platt", {}).get(name)
        if not entry:
            return None
        return _sigmoid(entry["coef"] * _logit(p) + entry["intercept"])

    def describe(self, name: str) -> dict:
        data = self.get()
        return {"file": self.path, "fitted": data.get("fitted"), "map": data.get("platt", {}).get(name)
                if name != "fusion" else data.get("fusion")}


def choose(raw: float, calibrated: Optional[float], score: str) -> tuple[float, str]:
    if score == "calibrated":
        if calibrated is None:
            raise ValueError("no calibration file yet; use score=raw")
        return calibrated, "calibrated"
    return raw, "raw"


def vap_endpoint(model: "VAP", audio: np.ndarray, aggregate: str = "p_future") -> dict:
    out = model.predict(audio, np.zeros_like(audio), 1)
    value = float(out["next_speaker"][aggregate]["assistant"])
    return {"raw": value, "detail": out, "aggregate": aggregate,
            "rule": f"probability = VAP {aggregate} for the assistant at the last frame: its share of projected "
                    "voice activity over 0-0.6 s (p_now, bins 0-1) or 0.6-2.0 s (p_future, bins 2-3) with the "
                    "assistant channel silent, i.e. P(the assistant is the next speaker). p_future is the default "
                    "because it separated endpoints from within-turn pauses better on the I0 calibration split "
                    "(ROC-AUC 0.732 vs 0.678; test 0.739 vs 0.647)"}


def dualturn_endpoint(model: "DualTurn", audio: np.ndarray) -> dict:
    out = model.predict(audio, np.zeros_like(audio), 5)
    eot = max(out["turn_event_likelihood"]["eot"]["user"])
    hold = max(out["turn_event_likelihood"]["hold"]["user"])
    ratio = eot / (eot + hold) if eot + hold > 0 else 0.5
    return {"raw": float(ratio), "detail": out, "eot_max": eot, "hold_max": hold,
            "rule": "probability = eot / (eot + hold) for the user channel, each head's max over the last 5 "
                    "frames (400 ms, the training label width): both heads fire at a speech offset, EOT when "
                    "the other speaker takes the floor within 4 s, HOLD when the same speaker resumes"}


# --------------------------------------------------------------------------
# HTTP


def decode_stereo(raw: bytes, layout: str) -> tuple[np.ndarray, np.ndarray]:
    if len(raw) % 8:
        raise ValueError("body must be float32 stereo samples (length a multiple of 8 bytes)")
    audio = np.frombuffer(raw, dtype="<f4").astype(np.float32)
    if layout == "interleaved":
        return audio[0::2].copy(), audio[1::2].copy()
    half = len(audio) // 2
    return audio[:half].copy(), audio[half:].copy()


def build_app(models: dict[str, Any], calibration: Optional["Calibration"] = None):
    app = FastAPI(title="openrealtime interaction predictors")
    calibration = calibration or Calibration(DEFAULT_CALIBRATION)
    stats: dict[str, dict] = {name: {"requests": 0, "errors": 0, "seconds": 0.0} for name in models}

    def record(name: str, began: float, ok: bool) -> None:
        stats[name]["requests"] += 1
        stats[name]["seconds"] += time.perf_counter() - began
        if not ok:
            stats[name]["errors"] += 1

    async def run(name: str, fn):
        began = time.perf_counter()
        if name not in models:
            return JSONResponse({"error": f"model {name} is not loaded"}, status_code=503)
        try:
            body = await asyncio.to_thread(fn)
            record(name, began, True)
            return body
        except ValueError as error:
            record(name, began, False)
            return JSONResponse({"error": str(error)}, status_code=400)
        except Exception as error:  # noqa: BLE001
            log.exception("%s failed", name)
            record(name, began, False)
            return JSONResponse({"error": str(error)}, status_code=500)

    @app.get("/health")
    def health():
        return {"status": "ok", "service": "openrealtime-interaction-predictors",
                "models": {k: v.describe() for k, v in models.items()}, "stats": stats,
                "calibration": {"file": calibration.path, "fitted": calibration.get().get("fitted")},
                "note": "observation-only evidence; no route issues control actions"}

    @app.post("/v1/endpoint/smart-turn")
    async def smart_turn(request: Request, variant: Optional[str] = None, sample_rate: int = SAMPLE_RATE,
                         score: str = "raw"):
        raw = await request.body()
        if sample_rate != SAMPLE_RATE:
            return JSONResponse({"error": "sample_rate must be 16000"}, status_code=400)
        if len(raw) % 4 or not raw:
            return JSONResponse({"error": "body must be float32 mono samples"}, status_code=400)
        audio = np.frombuffer(raw, dtype="<f4").astype(np.float32)

        def answer():
            out = models["smart-turn"].predict(audio, variant)
            calibrated = calibration.platt("smart_turn", out["probability"])
            out["raw_probability"] = out["probability"]
            out["calibrated_probability"] = calibrated
            out["probability"], out["score"] = choose(out["probability"], calibrated, score)
            return out

        return await run("smart-turn", answer)

    def header_transcript(request: Request) -> Optional[str]:
        raw = request.headers.get(TRANSCRIPT_HEADER)
        if raw is None:
            return None
        from urllib.parse import unquote

        text = unquote(raw).strip()
        return text or None

    def mono_body(raw: bytes) -> np.ndarray:
        if len(raw) % 4 or not raw:
            raise ValueError("body must be float32 mono samples")
        audio = np.frombuffer(raw, dtype="<f4").astype(np.float32)
        return audio[-8 * SAMPLE_RATE:]

    @app.post("/v1/endpoint/livekit")
    async def livekit(request: Request, language: str = "en", score: Optional[str] = None):
        """JSON {"messages": [...]} form, or the endpoint-hook form (audio body + transcript header)."""
        if request.headers.get("content-type", "").startswith("application/json"):
            try:
                payload = await request.json()
            except Exception:  # noqa: BLE001
                return JSONResponse({"error": "body must be JSON"}, status_code=400)
            messages = payload.get("messages") or []
            language = payload.get("language", language)
            form = "messages"
        else:
            await request.body()
            text = header_transcript(request)
            messages = [{"role": "user", "content": text}] if text else []
            form = "hook"
        if score is None:
            # the JSON form keeps LiveKit's own scale; the hook form defaults to P(turn complete) on the
            # I0 pause distribution when a frozen calibration exists (LiveKit's raw 'en' threshold is 0.011)
            score = "calibrated" if form == "hook" and calibration.get().get("platt", {}).get("livekit") else "raw"

        def answer():
            lk = models["livekit"]
            lang = lk.languages.get(language) or lk.languages.get(language.split("-")[0]) or {}
            if not messages:
                neutral = 0.5 if score == "calibrated" else lang.get("threshold", 0.5)
                return {"evidence": "endpoint_probability", "basis": "transcript", "probability": neutral,
                        "score": score, "transcript_available": False, "confidence": "none",
                        "note": "no transcript: this is a neutral placeholder, not model evidence "
                                "(the calibrated scale's 0.5 or LiveKit's own language threshold on the raw scale)",
                        "model": f"{LIVEKIT_REPO}@{LIVEKIT_REVISION}", "form": form, "latency_ms": {"total": 0.0}}
            out = lk.predict(messages, language)
            calibrated = calibration.platt("livekit", out["probability"])
            out["raw_probability"] = out["probability"]
            out["calibrated_probability"] = calibrated
            out["probability"], out["score"] = choose(out["probability"], calibrated, score)
            out["calibration"] = calibration.describe("livekit")
            out["transcript_available"] = True
            out["form"] = form
            return out

        return await run("livekit", answer)

    @app.post("/v1/endpoint/vap")
    async def vap_mono(request: Request, score: str = "raw", aggregate: str = "p_future"):
        try:
            audio = mono_body(await request.body())
        except ValueError as error:
            return JSONResponse({"error": str(error)}, status_code=400)

        def answer():
            began = time.perf_counter()
            if aggregate not in ("p_now", "p_future"):
                raise ValueError("aggregate must be p_now or p_future")
            res = vap_endpoint(models["vap"], audio, aggregate)
            detail = res["detail"]
            calibrated = calibration.platt("vap", res["raw"])
            probability, used = choose(res["raw"], calibrated, score)
            return {"evidence": "endpoint_probability", "basis": "voice_activity_projection", "probability": probability,
                    "score": used, "raw_probability": res["raw"], "calibrated_probability": calibrated,
                    "rule": res["rule"], "aggregate": res["aggregate"], "assistant_channel": "assumed-silent",
                    "model": detail["model"], "model_detail": detail["model_detail"],
                    "vap": {k: detail[k] for k in ("bins_s", "p_active", "next_speaker", "voice_activity_now",
                                                   "entropy_bits", "frame_end_s")},
                    "calibration": calibration.describe("vap"),
                    "latency_ms": {"total": ms(time.perf_counter() - began)}}

        return await run("vap", answer)

    @app.post("/v1/endpoint/dualturn")
    async def dualturn_mono(request: Request, score: str = "raw"):
        try:
            audio = mono_body(await request.body())
        except ValueError as error:
            return JSONResponse({"error": str(error)}, status_code=400)

        def answer():
            began = time.perf_counter()
            res = dualturn_endpoint(models["dualturn"], audio)
            detail = res["detail"]
            calibrated = calibration.platt("dualturn", res["raw"])
            probability, used = choose(res["raw"], calibrated, score)
            return {"evidence": "endpoint_probability", "basis": "dual_channel_turn_events", "probability": probability,
                    "score": used, "raw_probability": res["raw"], "calibrated_probability": calibrated,
                    "rule": res["rule"], "assistant_channel": "assumed-silent",
                    "eot_user_max": res["eot_max"], "hold_user_max": res["hold_max"],
                    "model": detail["model"], "model_detail": detail["model_detail"],
                    "dualturn": {k: detail[k] for k in ("voice_activity_now", "voice_activity_forecast",
                                                        "turn_event_likelihood", "frame_end_s")},
                    "calibration": calibration.describe("dualturn"),
                    "latency_ms": {"total": ms(time.perf_counter() - began)}}

        return await run("dualturn", answer)

    @app.post("/v1/endpoint/fusion")
    async def fusion(request: Request, language: str = "en"):
        try:
            audio = mono_body(await request.body())
        except ValueError as error:
            return JSONResponse({"error": str(error)}, status_code=400)
        text = header_transcript(request)

        def answer():
            began = time.perf_counter()
            fused = calibration.get().get("fusion")
            if not fused:
                raise ValueError("no frozen fusion weights yet (run endpoint_eval.py analyze)")
            st = models["smart-turn"].predict(audio, "gpu")
            if text:
                lk = models["livekit"].predict([{"role": "user", "content": text}], language)
                w = fused["coef"]
                z = w[0] * _logit(st["probability"]) + w[1] * _logit(lk["probability"]) + fused["intercept"]
                probability, basis, lk_p = _sigmoid(z), "acoustic+transcript", lk["probability"]
            else:
                probability = calibration.platt("smart_turn", st["probability"])
                basis, lk_p = "acoustic-only fallback (no transcript): Platt-calibrated Smart Turn", None
            return {"evidence": "endpoint_probability", "basis": basis, "probability": probability,
                    "transcript_available": bool(text),
                    "components": {"smart_turn": st["probability"], "livekit": lk_p},
                    "model": f"fusion(smart-turn@{SMART_TURN_REVISION[:12]}+livekit@{LIVEKIT_REVISION[:12]})"
                             f"@{calibration.get().get('fitted', {}).get('sha256', 'unknown')[:12]}",
                    "calibration": calibration.describe("fusion"),
                    "latency_ms": {"total": ms(time.perf_counter() - began)}}

        return await run("smart-turn", answer)

    async def stereo_route(name: str, request: Request, layout: str, sample_rate: int, frames: int, **kw):
        raw = await request.body()
        if sample_rate != SAMPLE_RATE:
            return JSONResponse({"error": "sample_rate must be 16000"}, status_code=400)
        try:
            user, assistant = decode_stereo(raw, layout)
        except ValueError as error:
            return JSONResponse({"error": str(error)}, status_code=400)
        if len(user) < SAMPLE_RATE // 10:
            return JSONResponse({"error": "need at least 100 ms of audio"}, status_code=400)
        return await run(name, lambda: models[name].predict(user, assistant, frames, **kw))

    @app.post("/v1/forecast/vap")
    async def vap(request: Request, layout: str = "planar", sample_rate: int = SAMPLE_RATE, frames: int = 1):
        return await stereo_route("vap", request, layout, sample_rate, frames)

    @app.post("/v1/turn-state/x2turn")
    async def x2turn(request: Request, sample_rate: int = SAMPLE_RATE, frames: int = 1):
        raw = await request.body()
        if sample_rate != SAMPLE_RATE:
            return JSONResponse({"error": "sample_rate must be 16000"}, status_code=400)
        if len(raw) % 4 or not raw:
            return JSONResponse({"error": "body must be float32 mono samples"}, status_code=400)
        audio = np.frombuffer(raw, dtype="<f4").astype(np.float32)
        return await run("x2turn", lambda: models["x2turn"].predict(audio, frames))

    @app.post("/v1/turn-state/soulx")
    async def soulx(request: Request, sample_rate: int = SAMPLE_RATE, every: int = 0):
        raw = await request.body()
        if sample_rate != SAMPLE_RATE:
            return JSONResponse({"error": "sample_rate must be 16000"}, status_code=400)
        if len(raw) % 4 or not raw:
            return JSONResponse({"error": "body must be float32 mono samples"}, status_code=400)
        audio = np.frombuffer(raw, dtype="<f4").astype(np.float32)
        return await run("soulx", lambda: models["soulx"].predict(audio, bool(every)))

    @app.post("/v1/forecast/dualturn")
    async def dualturn(request: Request, layout: str = "planar", sample_rate: int = SAMPLE_RATE, frames: int = 1,
                       head_input: str = "layer-attention"):
        if head_input not in ("layer-attention", "last-layer"):
            return JSONResponse({"error": "head_input must be layer-attention or last-layer"}, status_code=400)
        return await stereo_route("dualturn", request, layout, sample_rate, frames, head_input=head_input)

    return app


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9130)
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--models", default="smart-turn,livekit,vap,dualturn")
    parser.add_argument("--smart-turn-variant", default="gpu", choices=["gpu", "cpu"])
    parser.add_argument("--smart-turn-fp32-device", default="cuda", choices=["cuda", "cpu"],
                        help="execution provider for the fp32 ('gpu') Smart Turn export")
    parser.add_argument("--cpu-threads", type=int, default=4, help="ONNX intra-op threads (Smart Turn CPU, LiveKit)")
    parser.add_argument("--vap-context", type=float, default=20.0)
    parser.add_argument("--dualturn-context", type=float, default=30.0)
    parser.add_argument("--soulx-language", default="en", help="SenseVoice language for SoulX-Duplug (en|zh|auto)")
    parser.add_argument("--calibration", default=DEFAULT_CALIBRATION,
                        help="frozen I0 calibration (Platt maps + fusion weights) written by endpoint_eval.py analyze")
    parser.add_argument("--pid-file", default="", help="write this process's PID here at startup")
    args = parser.parse_args()
    if args.pid_file:
        with open(args.pid_file, "w") as handle:
            handle.write(f"{os.getpid()}\n")
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")

    import torch

    if args.device.startswith("cuda") and not torch.cuda.is_available():
        log.warning("CUDA unavailable; using CPU")
        args.device = "cpu"
    wanted = [m.strip() for m in args.models.split(",") if m.strip()]
    models: dict[str, Any] = {}
    for name in wanted:
        began = time.perf_counter()
        if name == "smart-turn":
            models[name] = SmartTurn(args.smart_turn_variant, args.cpu_threads,
                                     "cpu" if args.device == "cpu" else args.smart_turn_fp32_device)
        elif name == "livekit":
            models[name] = LiveKit(args.cpu_threads)
        elif name == "vap":
            models[name] = VAP(args.device, args.vap_context)
        elif name == "soulx":
            models[name] = SoulXDuplug(args.device, args.soulx_language)
        elif name == "x2turn":
            models[name] = X2Turn(args.device)
        elif name == "dualturn":
            models[name] = DualTurn(args.device, args.dualturn_context)
        else:
            raise SystemExit(f"unknown model {name}")
        log.info("loaded %s in %.1f s", name, time.perf_counter() - began)
    app = build_app(models, Calibration(args.calibration))

    import uvicorn

    uvicorn.run(app, host=args.host, port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
