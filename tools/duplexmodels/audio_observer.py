#!/usr/bin/env python3
"""Audio Flamingo 3 as a bounded, asynchronous audio observer (plan cell A0).

The observer answers one question about one bounded window of already-heard
audio and returns a timestamped, expiring *hypothesis*. It is deliberately
**off the 500 ms micro-turn critical path**: nothing in the cascade waits for
it, a result is only usable by micro-turns that start after ``available_at_ms``,
and the consumer must discard it after ``expires_at_ms``. Inferred emotion,
addressee, or background-speech labels are hypotheses about the audio, never
verified facts (plan section 7.8, survey section 4).

Wire contract (``openrealtime-audio-observation/1``)::

    POST /v1/observe   application/json
      {"task": "turn_intent",            # see GET /v1/tasks
       "audio_f32": "<base64 float32 LE mono>" | "audio_pcm16": "<base64 int16 LE>",
       "sample_rate": 16000,             # resampled to 16 kHz if different
       "source": {"stream_id": "...", "start_ms": 12000, "end_ms": 20000},  # caller's clock, echoed
       "prompt": "...",                  # only for task "custom"
       "ttl_ms": 3000,                   # optional override of the task default
       "max_queue_ms": 1500}             # optional: drop instead of running late
    POST /v1/observe?task=T&stream_id=S&start_ms=N   application/octet-stream (float32 LE 16 kHz)

    -> {"schema": "openrealtime-audio-observation/1", "id", "status": "ok"|"expired",
        "task", "question", "answer", "label", "confidence", "distribution", "option_mass",
        "model", "source_window": {..., "duration_ms", "samples", "sha256"},
        "received_at_ms", "started_at_ms", "completed_at_ms", "available_at_ms", "expires_at_ms",
        "queue_ms", "inference_ms", "latency_ms", "critical_path": false, "hypothesis": true}
    GET /v1/tasks, GET /health

Windows longer than 10 s are refused (413). One GPU worker runs requests in
order; at most ``--max-pending`` wait, beyond that the service sheds load with
429 rather than producing stale answers. For closed-set tasks the label comes
from the first decoding step's distribution restricted to the option tokens;
``option_mass`` is how much probability the model put on answering in format.

Serve (Audio Flamingo 3 bf16 is ~17 GB resident: it needs the large-model lease)::

    deploy/duplex/services/audio-observer.sh            # port 9160

Evaluate a running observer::

    .runtime/duplex-plan/venvs/perception/bin/python tools/duplexmodels/audio_observer.py eval \
        --url http://127.0.0.1:9160 --per-class 25 --out .runtime/duplex-plan/results/perception/observer-eval.json

License: Audio Flamingo 3 weights are NVIDIA OneWay Non-Commercial (research only).
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import hashlib
import json
import logging
import os
import queue
import random
import threading
import time
import uuid
from math import gcd
from pathlib import Path
from typing import Optional

import numpy as np

try:  # Serving only; the eval client does not need FastAPI.
    # Imported at module scope on purpose: ``from __future__ import annotations``
    # postpones the route signature, and FastAPI resolves it against module
    # globals - a function-local import makes it read ``request`` as a query
    # parameter and answer every POST with 422.
    from fastapi import Request as HTTPRequest
except ImportError:  # pragma: no cover - serving dependency
    HTTPRequest = None

log = logging.getLogger("audio_observer")

SCHEMA = "openrealtime-audio-observation/1"
RATE = 16_000
MAX_WINDOW_S = 10.0
ROOT = Path(__file__).resolve().parents[2]

# Closed-set tasks name their options; the label is the option whose answer
# token is most probable at the first decoding step.
TASKS: dict[str, dict] = {
    "turn_intent": {
        "kind": "choice",
        "ttl_ms": 3000,
        "prompt": (
            "This clip is from a person talking to a voice assistant. Listen to the LAST speech in the clip "
            "and classify it. (A) the user is addressing the voice assistant with a new request or question; "
            "(B) the user is talking to another person in the room, not to the assistant; "
            "(C) it is background speech from a different, more distant speaker or a TV; "
            "(D) the user only gives a short acknowledgement such as 'yeah', 'okay' or 'uh-huh'. "
            "Answer with only the letter."
        ),
        "options": {"A": "assistant_request", "B": "talking_to_other", "C": "background_speech", "D": "backchannel"},
    },
    "background_speech": {
        "kind": "choice",
        "ttl_ms": 4000,
        "prompt": (
            "Besides the main, closest speaker, is there speech from a different person in this clip, such as "
            "background talk, a distant speaker, or a TV? Answer yes or no."
        ),
        "options": {"yes": "yes", "no": "no"},
    },
    "addressee": {
        "kind": "choice",
        "ttl_ms": 3000,
        "prompt": (
            "The speaker at the start of this clip is talking to a voice assistant. In the LAST utterance, is the "
            "speaker still talking to the voice assistant? Answer yes or no."
        ),
        "options": {"yes": "assistant", "no": "someone_else"},
    },
    "noise": {
        "kind": "choice",
        "ttl_ms": 10000,
        "prompt": "Is there non-speech background noise or music in this clip? Answer yes or no.",
        "options": {"yes": "yes", "no": "no"},
    },
    "emotion": {
        "kind": "choice",
        "ttl_ms": 15000,
        "prompt": (
            "What emotion does the main speaker's voice convey? (A) neutral (B) happy (C) sad (D) angry "
            "(E) surprised (F) fearful. Answer with only the letter."
        ),
        "options": {"A": "neutral", "B": "happy", "C": "sad", "D": "angry", "E": "surprised", "F": "fearful"},
    },
    "sound_events": {
        "kind": "open",
        "ttl_ms": 10000,
        "max_new_tokens": 48,
        "prompt": (
            "List the non-speech sound events audible in this clip (for example door, music, traffic, keyboard, "
            "dog, alarm, crowd). Answer with a short comma-separated list, or 'none'."
        ),
    },
    "prosody": {
        "kind": "open",
        "ttl_ms": 10000,
        "max_new_tokens": 48,
        "prompt": "Describe the main speaker's tone and prosody (pace, loudness, hesitation) in one short sentence.",
    },
    "custom": {"kind": "open", "ttl_ms": 5000, "max_new_tokens": 64, "prompt": None},
}


def resample(x: np.ndarray, rate: int, target: int = RATE) -> np.ndarray:
    if rate == target:
        return x.astype(np.float32)
    from scipy.signal import resample_poly

    g = gcd(rate, target)
    return resample_poly(x, target // g, rate // g).astype(np.float32)


def now_ms() -> int:
    return int(time.time() * 1000)


# --------------------------------------------------------------------------
# Model


class Observer:
    def __init__(self, model_id: str, device: str, quantize: str = "none"):
        import torch
        from transformers import AudioFlamingo3ForConditionalGeneration, AutoProcessor

        self.torch = torch
        self.processor = AutoProcessor.from_pretrained(model_id)
        kwargs = {"dtype": torch.bfloat16, "attn_implementation": "sdpa"}
        if quantize == "nf4":
            from transformers import BitsAndBytesConfig

            kwargs["quantization_config"] = BitsAndBytesConfig(
                load_in_4bit=True, bnb_4bit_quant_type="nf4", bnb_4bit_compute_dtype=torch.bfloat16,
                llm_int8_skip_modules=["audio_tower", "multi_modal_projector", "lm_head"])
        elif quantize == "int8":
            from transformers import BitsAndBytesConfig

            kwargs["quantization_config"] = BitsAndBytesConfig(
                load_in_8bit=True, llm_int8_skip_modules=["audio_tower", "multi_modal_projector", "lm_head"])
        began = time.perf_counter()
        self.model = AudioFlamingo3ForConditionalGeneration.from_pretrained(model_id, device_map=device, **kwargs).eval()
        self.load_seconds = time.perf_counter() - began
        self.device = self.model.device
        self.quantize = quantize
        revision = "unknown"
        try:
            from huggingface_hub import snapshot_download

            revision = Path(snapshot_download(model_id, local_files_only=True)).name
        except Exception:  # noqa: BLE001
            pass
        self.model_label = f"{model_id}@{revision[:12]}" + ("" if quantize == "none" else f"+{quantize}")
        tokenizer = self.processor.tokenizer
        self.option_tokens: dict[str, dict[str, list[int]]] = {}
        for name, task in TASKS.items():
            if task["kind"] != "choice":
                continue
            ids = {}
            for option in task["options"]:
                variants = {option, option.capitalize(), option.upper(), " " + option, " " + option.capitalize(),
                            "(" + option}
                ids[option] = sorted({tokenizer.encode(v, add_special_tokens=False)[0] for v in variants})
            self.option_tokens[name] = ids

    def run(self, task_name: str, audio: np.ndarray, prompt: Optional[str]) -> dict:
        torch = self.torch
        task = TASKS[task_name]
        question = prompt if task_name == "custom" else task["prompt"]
        conversation = [{"role": "user", "content": [
            {"type": "audio", "audio": audio}, {"type": "text", "text": question}]}]
        inputs = self.processor.apply_chat_template(
            conversation, tokenize=True, add_generation_prompt=True, return_dict=True).to(self.device)
        inputs["input_features"] = inputs["input_features"].to(torch.bfloat16)
        max_new = 4 if task["kind"] == "choice" else task.get("max_new_tokens", 48)
        with torch.inference_mode():
            out = self.model.generate(**inputs, max_new_tokens=max_new, do_sample=False,
                                      output_scores=True, return_dict_in_generate=True)
        generated = out.sequences[0, inputs["input_ids"].shape[1]:]
        answer = self.processor.tokenizer.decode(generated, skip_special_tokens=True).strip()
        result = {"question": question, "answer": answer, "label": None, "confidence": None,
                  "distribution": None, "option_mass": None, "generated_tokens": int(generated.shape[0])}
        if task["kind"] == "choice":
            probs = torch.softmax(out.scores[0][0].float(), dim=-1)
            mass = {option: float(probs[ids].sum()) for option, ids in self.option_tokens[task_name].items()}
            total = sum(mass.values())
            dist = {task["options"][o]: (m / total if total > 0 else 0.0) for o, m in mass.items()}
            label = max(dist, key=dist.get)
            result.update({"label": label, "confidence": round(dist[label], 4),
                           "distribution": {k: round(v, 4) for k, v in dist.items()},
                           "option_mass": round(total, 4)})
        return result


# --------------------------------------------------------------------------
# Service


class Job:
    def __init__(self, payload: dict):
        self.payload = payload
        self.done = threading.Event()
        self.result: Optional[dict] = None
        self.error: Optional[str] = None


def serve(args) -> None:
    import torch
    from fastapi import FastAPI
    from fastapi.responses import JSONResponse
    import uvicorn

    observer = Observer(args.model, args.device, args.quantize)
    log.info("loaded %s in %.1fs", observer.model_label, observer.load_seconds)
    pending: "queue.Queue[Job]" = queue.Queue()
    stats = {"completed": 0, "expired": 0, "rejected_busy": 0, "errors": 0, "inference_ms": [], "queue_ms": []}
    lock = threading.Lock()

    # Warm up kernels so the first measured request is not a cold start.
    silence = np.zeros(RATE, dtype=np.float32)
    for _ in range(2):
        observer.run("background_speech", silence, None)

    def worker() -> None:
        while True:
            job = pending.get()
            p = job.payload
            started = now_ms()
            queue_ms = started - p["received_at_ms"]
            try:
                if p.get("max_queue_ms") is not None and queue_ms > p["max_queue_ms"]:
                    job.result = {"status": "expired", "queue_ms": queue_ms}
                    with lock:
                        stats["expired"] += 1
                else:
                    began = time.perf_counter()
                    out = observer.run(p["task"], p["audio"], p.get("prompt"))
                    inference_ms = (time.perf_counter() - began) * 1000
                    job.result = {"status": "ok", "started_at_ms": started, "queue_ms": queue_ms,
                                  "inference_ms": round(inference_ms, 1), **out}
                    with lock:
                        stats["completed"] += 1
                        stats["inference_ms"].append(inference_ms)
                        stats["queue_ms"].append(queue_ms)
                        del stats["inference_ms"][:-1000], stats["queue_ms"][:-1000]
            except Exception as error:  # noqa: BLE001
                log.exception("observe failed")
                job.error = str(error)
                with lock:
                    stats["errors"] += 1
            finally:
                job.done.set()

    threading.Thread(target=worker, daemon=True).start()
    app = FastAPI()

    def percentiles(values):
        if not values:
            return None
        a = np.asarray(values)
        return {"n": len(a), "p50": round(float(np.percentile(a, 50)), 1),
                "p90": round(float(np.percentile(a, 90)), 1), "max": round(float(a.max()), 1)}

    @app.get("/health")
    def health():
        with lock:
            snapshot = {k: v for k, v in stats.items() if not isinstance(v, list)}
            snapshot["inference_ms"] = percentiles(stats["inference_ms"])
            snapshot["queue_ms"] = percentiles(stats["queue_ms"])
        return {"status": "ok", "schema": SCHEMA, "model": observer.model_label, "device": str(observer.device),
                "quantize": observer.quantize, "critical_path": False, "max_window_s": MAX_WINDOW_S,
                "pending": pending.qsize(), "max_pending": args.max_pending,
                "gpu_allocated_gb": round(torch.cuda.memory_allocated() / 2**30, 2),
                "gpu_peak_allocated_gb": round(torch.cuda.max_memory_allocated() / 2**30, 2),
                "gpu_reserved_gb": round(torch.cuda.memory_reserved() / 2**30, 2),
                "load_seconds": round(observer.load_seconds, 1), "stats": snapshot}

    @app.get("/v1/tasks")
    def tasks():
        return {name: {k: v for k, v in task.items()} for name, task in TASKS.items()}

    @app.post("/v1/observe")
    async def observe(request: HTTPRequest):
        received = now_ms()
        content_type = request.headers.get("content-type", "")
        query = request.query_params
        if content_type.startswith("application/json"):
            body = await request.json()
            if "audio_f32" in body:
                audio = np.frombuffer(base64.b64decode(body["audio_f32"]), dtype="<f4").copy()
            elif "audio_pcm16" in body:
                audio = np.frombuffer(base64.b64decode(body["audio_pcm16"]), dtype="<i2").astype(np.float32) / 32768
            else:
                return JSONResponse({"error": "audio_f32 or audio_pcm16 required"}, status_code=400)
            rate = int(body.get("sample_rate", RATE))
            task = body.get("task", "")
            source = dict(body.get("source") or {})
            prompt, ttl, max_queue = body.get("prompt"), body.get("ttl_ms"), body.get("max_queue_ms")
        else:
            raw = await request.body()
            if len(raw) % 4:
                return JSONResponse({"error": "body must be float32 LE samples"}, status_code=400)
            audio = np.frombuffer(raw, dtype="<f4").copy()
            rate = int(query.get("sample_rate", RATE))
            task = query.get("task", "")
            source = {k: query[k] for k in ("stream_id", "start_ms", "end_ms") if k in query}
            prompt = query.get("prompt")
            ttl = int(query["ttl_ms"]) if "ttl_ms" in query else None
            max_queue = int(query["max_queue_ms"]) if "max_queue_ms" in query else None
        if task not in TASKS:
            return JSONResponse({"error": f"unknown task {task!r}", "tasks": sorted(TASKS)}, status_code=400)
        if task == "custom" and not prompt:
            return JSONResponse({"error": "task custom needs prompt"}, status_code=400)
        seconds = len(audio) / rate
        if seconds > MAX_WINDOW_S + 1e-6:
            return JSONResponse({"error": f"window {seconds:.2f}s exceeds {MAX_WINDOW_S}s"}, status_code=413)
        if len(audio) < rate // 10:
            return JSONResponse({"error": "window shorter than 100 ms"}, status_code=400)
        if pending.qsize() >= args.max_pending:
            with lock:
                stats["rejected_busy"] += 1
            return JSONResponse({"error": "observer busy", "pending": pending.qsize()}, status_code=429)
        audio16 = resample(audio, rate)
        source_window = {**source, "duration_ms": round(seconds * 1000, 1), "samples": int(len(audio)),
                         "sample_rate": rate,
                         "sha256": hashlib.sha256(audio.astype("<f4").tobytes()).hexdigest()[:16]}
        if "start_ms" in source and "end_ms" not in source:
            source_window["end_ms"] = float(source["start_ms"]) + seconds * 1000
        job = Job({"task": task, "audio": audio16, "prompt": prompt, "received_at_ms": received,
                   "max_queue_ms": max_queue})
        pending.put(job)
        await asyncio.to_thread(job.done.wait)
        if job.error:
            return JSONResponse({"error": job.error}, status_code=500)
        completed = now_ms()
        ttl_ms = int(ttl if ttl is not None else TASKS[task]["ttl_ms"])
        return {"schema": SCHEMA, "id": "obs_" + uuid.uuid4().hex[:12], "task": task,
                "model": observer.model_label, "source_window": source_window,
                "received_at_ms": received, "completed_at_ms": completed, "available_at_ms": completed,
                "expires_at_ms": completed + ttl_ms, "ttl_ms": ttl_ms, "latency_ms": completed - received,
                "critical_path": False, "hypothesis": True, **job.result}

    uvicorn.run(app, host=args.host, port=args.port, log_level="warning")


# --------------------------------------------------------------------------
# Evaluation client


FDB15 = ROOT / ".runtime/full-duplex-bench-v1.5/dataset"
FDBENCH = ROOT / ".runtime/fd-bench/dataset"
CATEGORY_LABEL = {"user_interruption": "assistant_request", "talking_to_other": "talking_to_other",
                  "background_speech": "background_speech", "user_backchannel": "backchannel"}


def fdb15_windows(per_class: int, seed: int, tail_s: float) -> list[dict]:
    import soundfile as sf

    items = []
    rng = random.Random(seed)
    for category in CATEGORY_LABEL:
        ids = [d for d in os.listdir(FDB15 / category) if d.isdigit()]
        ids.sort(key=int)
        rng.shuffle(ids)
        for sample in ids[:per_class]:
            folder = FDB15 / category / sample
            meta = json.loads((folder / "metadata.json").read_text())
            audio, rate = sf.read(folder / "input.wav", dtype="float32")
            audio = resample(audio, rate)
            end = min(len(audio), int((meta["timestamps"][1] + tail_s) * RATE))
            start = max(0, end - int(MAX_WINDOW_S * RATE))
            items.append({"category": category, "sample": sample, "audio": audio[start:end],
                          "start_ms": start / RATE * 1000, "event": meta["timestamps"]})
    return items


def fdbench_noise_windows(count: int, seed: int) -> list[dict]:
    """User turns from FD-Bench cosyvoice2 easy: clean versus the 10 dB
    background condition, split by what the interference really is. FD-Bench's
    MUSAN "noise" list includes speech recordings, so the noisy cell holds both
    non-speech noise and background talkers; ``denoise_eval.py labels`` sorts
    them (DeepFilterNet gap attenuation + ASR words on a noise-only gap)."""
    import soundfile as sf

    labels_path = ROOT / ".runtime/duplex-plan/results/perception/fdbench-interference-labels.json"
    labels = json.loads(labels_path.read_text())["labels"]
    rng = random.Random(seed)
    by_kind = {"speech": [], "noise": []}
    for conversation, info in sorted(labels.items(), key=lambda kv: int(kv[0])):
        if info["interference"] in by_kind:
            by_kind[info["interference"]].append(conversation)
    items = []
    for kind, conversations in by_kind.items():
        rng.shuffle(conversations)
        for conversation in conversations[:count]:
            for label, cell in (("clean", "cosyvoice2-single-round-combine-easy"),
                                (kind, "cosyvoice2-single-round-combine-easy-noisy-bg-10dB")):
                if label == "clean" and kind == "noise" and any(i["conversation"] == conversation for i in items):
                    continue
                path = FDBENCH / cell / f"conversation_{conversation}.wav"
                spans = json.loads(path.with_suffix(".timestamps").read_text())
                audio, rate = sf.read(path, dtype="float32")
                audio = resample(audio, rate)
                s = spans[0]
                a, b = s["start"], min(s["end"], s["start"] + int(MAX_WINDOW_S * RATE))  # 16 kHz indices
                items.append({"label": label, "conversation": conversation, "audio": audio[a:b]})
    return items


async def observe(client, url: str, task: str, audio: np.ndarray, source: dict | None = None) -> dict:
    body = {"task": task, "sample_rate": RATE, "audio_f32": base64.b64encode(audio.astype("<f4").tobytes()).decode(),
            "source": source or {}}
    response = await client.post(url + "/v1/observe", json=body, timeout=120)
    response.raise_for_status()
    return response.json()


def summarize_choice(records: list[dict], gold_key: str) -> dict:
    labels = sorted({r[gold_key] for r in records} | {r["label"] for r in records})
    confusion = {g: {p: 0 for p in labels} for g in labels}
    for r in records:
        confusion[r[gold_key]][r["label"]] += 1
    per_class = {}
    f1s = []
    for label in sorted({r[gold_key] for r in records}):
        tp = confusion[label][label]
        support = sum(confusion[label].values())
        predicted = sum(confusion[g][label] for g in labels)
        recall = tp / support if support else 0.0
        precision = tp / predicted if predicted else 0.0
        f1 = 2 * precision * recall / (precision + recall) if precision + recall else 0.0
        f1s.append(f1)
        per_class[label] = {"support": support, "recall": round(recall, 3), "precision": round(precision, 3),
                            "f1": round(f1, 3)}
    accuracy = sum(r[gold_key] == r["label"] for r in records) / len(records)
    return {"n": len(records), "accuracy": round(accuracy, 3), "macro_f1": round(float(np.mean(f1s)), 3),
            "per_class": per_class, "confusion": confusion,
            "mean_option_mass": round(float(np.mean([r["option_mass"] for r in records])), 3)}


def auroc(scores: list[float], positives: list[bool]) -> Optional[float]:
    pos = [s for s, p in zip(scores, positives) if p]
    neg = [s for s, p in zip(scores, positives) if not p]
    if not pos or not neg:
        return None
    wins = sum((p > n) + 0.5 * (p == n) for p in pos for n in neg)
    return round(wins / (len(pos) * len(neg)), 3)


def latency_summary(values: list[float]) -> dict:
    a = np.asarray(values)
    return {"n": int(len(a)), "mean": round(float(a.mean()), 1), "p50": round(float(np.percentile(a, 50)), 1),
            "p90": round(float(np.percentile(a, 90)), 1), "p99": round(float(np.percentile(a, 99)), 1),
            "max": round(float(a.max()), 1)}


async def voxtral_probe(audios: list[np.ndarray], url: str) -> list[float]:
    import websockets

    finalize = []
    for audio in audios:
        pcm = (np.clip(audio, -1, 1) * 32767).astype("<i2").tobytes()
        async with websockets.connect(url, max_size=1 << 22) as ws:
            await ws.send(json.dumps({"type": "session.update", "model": "voxtral-realtime"}))
            await ws.send(json.dumps({"type": "input_audio_buffer.commit"}))
            step = 3200  # 100 ms PCM16 at 16 kHz, paced at wall-clock speed
            for i in range(0, len(pcm), step):
                await ws.send(json.dumps({"type": "input_audio_buffer.append",
                                          "audio": base64.b64encode(pcm[i:i + step]).decode()}))
                await asyncio.sleep(0.1)
            began = time.perf_counter()
            await ws.send(json.dumps({"type": "input_audio_buffer.commit", "final": True}))
            while True:
                message = json.loads(await asyncio.wait_for(ws.recv(), timeout=60))
                if message.get("type") in ("transcription.done", "error"):
                    break
            finalize.append((time.perf_counter() - began) * 1000)
    return finalize


async def run_eval(args) -> dict:
    import httpx

    out: dict = {"url": args.url}
    async with httpx.AsyncClient() as client:
        health = (await client.get(args.url + "/health")).json()
        out["observer"] = {k: health[k] for k in ("model", "device", "quantize", "gpu_allocated_gb",
                                                   "gpu_peak_allocated_gb", "gpu_reserved_gb", "load_seconds")}
        latencies: dict[str, list[float]] = {}

        # 1. FDB v1.5: can the observer tell the four overlap categories apart?
        windows = fdb15_windows(args.per_class, args.seed, args.tail)
        records = []
        for item in windows:
            row = {"category": item["category"], "sample": item["sample"], "gold": CATEGORY_LABEL[item["category"]],
                   "window_s": round(len(item["audio"]) / RATE, 2)}
            for task in ("turn_intent", "background_speech", "addressee"):
                r = await observe(client, args.url, task, item["audio"],
                                  {"stream_id": f"fdb15/{item['category']}/{item['sample']}",
                                   "start_ms": item["start_ms"]})
                latencies.setdefault(task, []).append(r["inference_ms"])
                row[task] = {k: r[k] for k in ("label", "confidence", "distribution", "option_mass", "answer",
                                              "inference_ms", "latency_ms")}
            records.append(row)
            print(item["category"], item["sample"], row["turn_intent"]["label"], row["background_speech"]["label"],
                  row["addressee"]["label"], flush=True)
        out["fdb15_turn_intent"] = summarize_choice(
            [{"gold": r["gold"], **r["turn_intent"]} for r in records], "gold")
        bg = [{"gold": "yes" if r["category"] == "background_speech" else "no", **r["background_speech"]}
              for r in records]
        out["fdb15_background_speech"] = summarize_choice(bg, "gold")
        out["fdb15_background_speech"]["auroc"] = auroc(
            [r["distribution"]["yes"] for r in bg], [r["gold"] == "yes" for r in bg])
        ad = [{"gold": "assistant" if r["category"] == "user_interruption" else "someone_else", **r["addressee"]}
              for r in records if r["category"] in ("user_interruption", "talking_to_other")]
        out["fdb15_addressee"] = summarize_choice(ad, "gold")
        out["fdb15_addressee"]["auroc"] = auroc(
            [r["distribution"]["assistant"] for r in ad], [r["gold"] == "assistant" for r in ad])
        out["fdb15_records"] = records

        # 2. FD-Bench user turns: clean vs non-speech noise vs background talker (10 dB).
        noise_records = []
        for item in fdbench_noise_windows(args.noise_count, args.seed):
            row = {"gold": item["label"], "conversation": item["conversation"]}
            for task in ("noise", "background_speech"):
                r = await observe(client, args.url, task, item["audio"])
                latencies.setdefault(task, []).append(r["inference_ms"])
                row[task] = {k: r[k] for k in ("label", "confidence", "distribution", "option_mass", "inference_ms")}
            noise_records.append(row)
        nz = [{"gold": "yes" if r["gold"] == "noise" else "no", **r["noise"]}
              for r in noise_records if r["gold"] in ("noise", "clean")]
        out["fdbench_noise_vs_clean"] = summarize_choice(nz, "gold")
        out["fdbench_noise_vs_clean"]["auroc"] = auroc([r["distribution"]["yes"] for r in nz],
                                                       [r["gold"] == "yes" for r in nz])
        talker = [r for r in noise_records if r["gold"] == "speech"]
        out["fdbench_noise_task_on_background_talker_yes_rate"] = (
            round(sum(r["noise"]["label"] == "yes" for r in talker) / len(talker), 3) if talker else None)
        bs = [{"gold": "yes" if r["gold"] == "speech" else "no", **r["background_speech"]} for r in noise_records]
        out["fdbench_background_talker"] = summarize_choice(bs, "gold")
        out["fdbench_background_talker"]["auroc"] = auroc([r["distribution"]["yes"] for r in bs],
                                                          [r["gold"] == "yes" for r in bs])
        out["fdbench_background_talker"]["yes_rate_by_condition"] = {
            kind: round(sum(r["background_speech"]["label"] == "yes" for r in noise_records if r["gold"] == kind)
                        / max(1, sum(r["gold"] == kind for r in noise_records)), 3)
            for kind in ("clean", "noise", "speech")}
        out["fdbench_noise_records"] = noise_records

        # 3. Latency versus window length and task kind (open tasks decode more tokens).
        sweep = {}
        base = windows[0]["audio"]
        for seconds in (2, 5, 10):
            clip = np.resize(base, int(seconds * RATE))
            for task in ("background_speech", "sound_events", "emotion"):
                values = []
                for _ in range(args.repeats):
                    r = await observe(client, args.url, task, clip)
                    values.append(r["inference_ms"])
                sweep[f"{task}@{seconds}s"] = latency_summary(values)
        out["latency_by_window"] = sweep
        out["latency_by_task"] = {task: latency_summary(v) for task, v in latencies.items()}
        out["examples"] = {}
        for item in windows[:: max(1, len(windows) // 4)][:4]:
            ex = {}
            for task in ("sound_events", "emotion", "prosody"):
                r = await observe(client, args.url, task, item["audio"])
                ex[task] = {"answer": r["answer"], "label": r["label"], "confidence": r["confidence"]}
            out["examples"][f"{item['category']}/{item['sample']}"] = ex

        # 4. Interference: Voxtral finalize latency with the observer idle versus saturated.
        if args.voxtral_url:
            import soundfile as sf

            probe = []
            for path in sorted((FDBENCH / "cosyvoice2-single-round-combine-easy").glob("conversation_*.wav"))[:args.probe]:
                spans = json.loads(path.with_suffix(".timestamps").read_text())
                audio, rate = sf.read(path, dtype="float32")
                audio = resample(audio, rate)
                probe.append(audio[spans[0]["start"]:spans[0]["end"] + 4800])
            idle = await voxtral_probe(probe, args.voxtral_url)
            stop = asyncio.Event()
            load_count = 0

            async def hammer():
                nonlocal load_count
                clip = np.resize(windows[0]["audio"], int(10 * RATE))
                while not stop.is_set():
                    await observe(client, args.url, "sound_events", clip)
                    load_count += 1

            loaders = [asyncio.create_task(hammer()) for _ in range(2)]
            began = time.perf_counter()
            loaded = await voxtral_probe(probe, args.voxtral_url)
            elapsed = time.perf_counter() - began
            stop.set()
            await asyncio.gather(*loaders, return_exceptions=True)
            out["interference"] = {
                "probe": f"{len(probe)} FD-Bench clean first turns, paced 100 ms appends, finalize latency",
                "voxtral_finalize_ms_observer_idle": latency_summary(idle),
                "voxtral_finalize_ms_observer_saturated": latency_summary(loaded),
                "observer_requests_during_probe": load_count,
                "observer_throughput_per_s": round(load_count / elapsed, 2),
            }
        out["health_after"] = (await client.get(args.url + "/health")).json()
    return out


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    s = sub.add_parser("serve")
    s.add_argument("--host", default="127.0.0.1")
    s.add_argument("--port", type=int, default=9160)
    s.add_argument("--model", default="nvidia/audio-flamingo-3-hf")
    s.add_argument("--device", default="cuda:0")
    s.add_argument("--quantize", choices=["none", "int8", "nf4"], default="none")
    s.add_argument("--max-pending", type=int, default=4)
    e = sub.add_parser("eval")
    e.add_argument("--url", default="http://127.0.0.1:9160")
    e.add_argument("--per-class", type=int, default=25)
    e.add_argument("--noise-count", type=int, default=15, help="conversations per interference kind")
    e.add_argument("--seed", type=int, default=7)
    e.add_argument("--tail", type=float, default=1.5, help="seconds kept after the event's end timestamp")
    e.add_argument("--repeats", type=int, default=5)
    e.add_argument("--probe", type=int, default=10)
    e.add_argument("--voxtral-url", default="ws://127.0.0.1:9101/v1/realtime")
    e.add_argument("--out")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    if args.command == "serve":
        serve(args)
    else:
        result = asyncio.run(run_eval(args))
        if args.out:
            Path(args.out).parent.mkdir(parents=True, exist_ok=True)
            Path(args.out).write_text(json.dumps(result, indent=2))
        brief = {k: v for k, v in result.items() if not k.endswith("records") and k != "health_after"}
        print(json.dumps(brief, indent=2)[:6000])


if __name__ == "__main__":
    main()
