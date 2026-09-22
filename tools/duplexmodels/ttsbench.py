#!/usr/bin/env python3
"""Component benchmark for the duplex-plan synthesis services.

Drives one service over both routes of the synthesis contract
(``tools/duplexmodels/common.py``) and writes a JSON report:

``complete``     ``POST /v1/audio/speech`` for ~20 English and ~20 Mandarin
                 sentences of mixed length: time to first audio byte and
                 real-time factor (wall time / audio seconds).
``incremental``  ``WS /v1/tts/stream``, the plan's acceptance test (section 6):
                 append the first half of a sentence to one context, wait
                 1.5 s and record whether (and when) audio arrived *before* the
                 rest was sent, then append the rest to the same context and
                 end it. Also: time to first audio when text arrives word by
                 word at LLM speed (one word, or two Han characters, every
                 60 ms).
``cancel``       cancel a context mid-audio; count audio frames received after
                 ``context.cancelled`` (must be 0) and between the cancel and
                 its acknowledgement.
``score``        transcribe every saved output with the local Qwen3-ASR service
                 (start/chunk/finish, float32 LE 16 kHz mono) and report WER for
                 English and CER for Mandarin against the input text.

Audio is saved under ``<out-dir>/<name>/`` so ``score`` can be re-run alone.

    python3 tools/duplexmodels/ttsbench.py --name cosyvoice --url http://127.0.0.1:9120
    python3 tools/duplexmodels/ttsbench.py --summary

Runs with the system python (numpy, scipy, httpx, websockets).
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import os
import re
import statistics
import subprocess
import time
import unicodedata
import wave
from math import gcd
from pathlib import Path

import httpx
import numpy as np
from scipy.signal import resample_poly

ROOT = Path(__file__).resolve().parents[2]
RESULTS = ROOT / ".runtime" / "duplex-plan" / "results" / "tts"

EN = [
    "Good morning, how are you today?",
    "Please close the door behind you.",
    "That sounds like a great idea.",
    "I will call you back in a minute.",
    "The weather is lovely this afternoon.",
    "Could you pass me the salt, please?",
    "We are almost there.",
    "The library opens early on weekdays, but it closes before dinner on Sundays.",
    "She packed a small bag, grabbed her umbrella, and walked to the station in the rain.",
    "If you have any questions about your order, our support team is happy to help.",
    "The museum added a new exhibit about ancient ships and the people who sailed them.",
    "He forgot his keys at the office, so he had to wait outside until his sister came home.",
    "Fresh vegetables from the local farm arrive every morning before the market opens.",
    "Turn left at the second traffic light and you will see the bakery on your right.",
    "When the storm finally passed, the whole neighborhood came outside to clear the fallen branches, "
    "and by evening the streets looked almost as if nothing had happened.",
    "Our team spent the last few weeks testing the new design with real customers, and the feedback "
    "suggests that people find it much easier to use than the old version.",
    "Although the train was delayed by nearly an hour, most of the passengers stayed calm, reading books "
    "or chatting quietly with the strangers sitting next to them.",
    "The chef explained that the secret to a good soup is patience, because the flavors need time to "
    "develop slowly over a gentle heat.",
    "Before you submit the application, please make sure that every section is complete and that you "
    "have signed the form at the bottom of the last page.",
    "Scientists believe that the tiny island has been home to the same family of birds for thousands of "
    "years, which makes it a remarkable place to study evolution.",
]

ZH = [
    "你好，今天过得怎么样？",
    "请把门关上。",
    "这个主意听起来不错。",
    "我一会儿给你回电话。",
    "今天下午天气很好。",
    "麻烦把盐递给我。",
    "我们快到了。",
    "图书馆工作日开门很早，但是星期天晚饭前就关门了。",
    "她收拾了一个小包，拿上雨伞，冒着雨走到了车站。",
    "如果您对订单有任何疑问，我们的客服团队很乐意为您提供帮助。",
    "博物馆新增了一个关于古代船只和航海者的展览。",
    "他把钥匙忘在了办公室，只好在门外等妹妹回家。",
    "每天早上市场开门之前，本地农场的新鲜蔬菜就会送到。",
    "在第二个红绿灯左转，你就会看到右手边的面包店。",
    "暴风雨终于过去之后，整个街区的人都走出家门清理倒下的树枝，到了傍晚，街道看起来几乎和什么都没发生过一样。",
    "我们的团队在过去几周里和真实用户一起测试了新的设计，反馈表明大家觉得它比旧版本好用得多。",
    "虽然火车晚点了将近一个小时，但大多数乘客都很平静，有的在看书，有的在和旁边的陌生人小声聊天。",
    "厨师解释说，做好一锅汤的秘诀在于耐心，因为味道需要在小火上慢慢地熬出来。",
    "在提交申请之前，请确认每一部分都已经填写完整，并且在最后一页的底部签上了您的名字。",
    "科学家认为，这个小岛几千年来一直是同一种鸟类的家园，这使它成为研究进化的绝佳地点。",
]

# Held-back-suffix and word-by-word sentences: single sentences (no sentence
# boundary inside the first half), medium and long.
INCREMENTAL = {"en": [EN[i] for i in (8, 11, 14, 15, 16, 18)], "zh": [ZH[i] for i in (8, 11, 14, 15, 16, 18)]}
CANCEL = {"en": EN[19], "zh": ZH[19]}


# ---------------------------------------------------------------------------
# helpers


def pieces_word_by_word(text: str, language: str) -> list[str]:
    if language == "zh":
        return [text[i:i + 2] for i in range(0, len(text), 2)]
    words = text.split(" ")
    return [words[0]] + [" " + word for word in words[1:]]


def split_half(text: str, language: str) -> tuple[str, str]:
    if language == "zh":
        middle = len(text) // 2
        return text[:middle], text[middle:]
    words = text.split(" ")
    middle = len(words) // 2
    return " ".join(words[:middle]), " " + " ".join(words[middle:])


def save_wav(path: Path, pcm: bytes, rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(1)
        handle.setsampwidth(2)
        handle.setframerate(rate)
        handle.writeframes(pcm)


def load_wav(path: Path) -> tuple[np.ndarray, int]:
    with wave.open(str(path), "rb") as handle:
        rate = handle.getframerate()
        data = np.frombuffer(handle.readframes(handle.getnframes()), dtype="<i2")
    return data.astype(np.float32) / 32768.0, rate


def percentile(values: list[float], q: float):
    values = sorted(v for v in values if v is not None)
    if not values:
        return None
    index = min(len(values) - 1, max(0, int(round(q / 100 * (len(values) - 1)))))
    return round(values[index], 4)


def gpu_memory(port: int) -> dict:
    """Resident GPU memory of the process listening on ``port`` (nvidia-smi)."""
    try:
        listing = subprocess.run(["ss", "-ltnpH", f"sport = :{port}"], capture_output=True, text=True).stdout
        pids = {int(p) for p in re.findall(r"pid=(\d+)", listing)}
        apps = subprocess.run(["nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits"],
                              capture_output=True, text=True).stdout
        used = {int(pid): int(mib) for pid, mib in (line.split(", ") for line in apps.strip().splitlines() if line)}
        total = sum(used.get(pid, 0) for pid in pids)
        return {"pids": sorted(pids), "nvidia_smi_mib": total}
    except Exception as error:  # noqa: BLE001
        return {"error": str(error)}


# ---------------------------------------------------------------------------
# complete text


def run_complete(url: str, name: str, languages: list[str], out: Path) -> list[dict]:
    rows = []
    with httpx.Client(timeout=120) as client:
        for language in languages:
            for index, text in enumerate(EN if language == "en" else ZH):
                began = time.perf_counter()
                first = None
                pcm = bytearray()
                rate = None
                with client.stream("POST", f"{url}/v1/audio/speech",
                                   json={"model": name, "input": text, "voice": "default",
                                         "response_format": "pcm", "stream": True}) as response:
                    response.raise_for_status()
                    rate = int(response.headers.get("X-Sample-Rate", "24000"))
                    for chunk in response.iter_raw():
                        if chunk and first is None:
                            first = time.perf_counter() - began
                        pcm += chunk
                total = time.perf_counter() - began
                if len(pcm) % 2:
                    pcm = pcm[:-1]
                seconds = len(pcm) / 2 / rate
                path = out / f"complete-{language}-{index:02d}.wav"
                save_wav(path, bytes(pcm), rate)
                rows.append({"kind": "complete", "language": language, "index": index, "text": text,
                             "ttfa_s": round(first, 4) if first is not None else None, "total_s": round(total, 4),
                             "audio_s": round(seconds, 3), "rtf": round(total / seconds, 4) if seconds else None,
                             "sample_rate": rate, "wav": str(path)})
                print(f"  complete {language}{index:02d} ttfa={rows[-1]['ttfa_s']} rtf={rows[-1]['rtf']} "
                      f"audio={seconds:.2f}s", flush=True)
    return rows


# ---------------------------------------------------------------------------
# incremental WebSocket


class Context:
    def __init__(self, websocket, context_id: str) -> None:
        self.ws = websocket
        self.id = context_id
        self.events: list[tuple[float, dict]] = []
        self.audio = bytearray()
        self.frames: list[tuple[float, int]] = []
        self.done = asyncio.Event()
        self.ready = asyncio.Event()
        self.cancelled_at = None
        self.capabilities = {}
        self.rate = 24000

    async def reader(self) -> None:
        async for raw in self.ws:
            now = time.perf_counter()
            message = json.loads(raw)
            kind = message.get("type")
            if kind == "audio":
                data = base64.b64decode(message["pcm16"])
                self.audio += data
                self.frames.append((now, len(data) // 2))
                continue
            self.events.append((now, message))
            if kind == "context.ready":
                self.capabilities = message.get("capabilities", {})
                self.rate = int(message.get("sample_rate", 24000))
                self.ready.set()
            elif kind in ("audio.done", "error"):
                self.done.set()
            elif kind == "context.cancelled":
                self.cancelled_at = now
                self.done.set()

    async def send(self, kind: str, **fields) -> float:
        await self.ws.send(json.dumps({"type": kind, "context_id": self.id, **fields}))
        return time.perf_counter()

    def audio_between(self, start: float, end: float) -> tuple[int, float]:
        frames = [(t, n) for t, n in self.frames if start <= t <= end]
        return len(frames), sum(n for _, n in frames) / self.rate


async def open_context(url: str, context_id: str):
    from websockets.asyncio.client import connect

    websocket = await connect(url.replace("http", "ws", 1) + "/v1/tts/stream", max_size=None, open_timeout=30)
    context = Context(websocket, context_id)
    task = asyncio.create_task(context.reader())
    await context.send("context.open", voice="default")
    await asyncio.wait_for(context.ready.wait(), 30)
    return context, task


async def held_back(url: str, text: str, language: str, index: int, out: Path) -> dict:
    head, tail = split_half(text, language)
    context, task = await open_context(url, f"held-{language}-{index}")
    try:
        start = await context.send("text.append", text=head)
        await asyncio.sleep(1.5)
        rest_at = await context.send("text.append", text=tail)
        await context.send("text.end")
        await asyncio.wait_for(context.done.wait(), 120)
        before_frames, before_seconds = context.audio_between(start, rest_at)
        first = context.frames[0][0] - start if context.frames else None
        path = out / f"held-{language}-{index:02d}.wav"
        save_wav(path, bytes(context.audio), context.rate)
        errors = [m for _, m in context.events if m.get("type") == "error"]
        return {"kind": "held_back", "language": language, "index": index, "text": text, "head": head,
                "audio_before_rest": before_frames > 0, "frames_before_rest": before_frames,
                "audio_s_before_rest": round(before_seconds, 3),
                "first_audio_s": round(first, 4) if first is not None else None,
                "audio_s": round(len(context.audio) / 2 / context.rate, 3), "errors": errors,
                "capabilities": context.capabilities, "wav": str(path)}
    finally:
        await context.ws.close()
        task.cancel()


async def word_by_word(url: str, text: str, language: str, index: int, out: Path, interval: float) -> dict:
    context, task = await open_context(url, f"words-{language}-{index}")
    try:
        pieces = pieces_word_by_word(text, language)
        start = time.perf_counter()
        for position, piece in enumerate(pieces):
            await context.send("text.append", text=piece)
            target = start + interval * (position + 1)
            await asyncio.sleep(max(0.0, target - time.perf_counter()))
        text_done = await context.send("text.end")
        await asyncio.wait_for(context.done.wait(), 120)
        first = context.frames[0][0] - start if context.frames else None
        path = out / f"words-{language}-{index:02d}.wav"
        save_wav(path, bytes(context.audio), context.rate)
        end = context.frames[-1][0] - start if context.frames else None
        return {"kind": "word_by_word", "language": language, "index": index, "text": text,
                "pieces": len(pieces), "interval_s": interval, "text_span_s": round(text_done - start, 3),
                "first_audio_s": round(first, 4) if first is not None else None,
                "first_audio_before_text_end": first is not None and start + first < text_done,
                "last_audio_s": round(end, 4) if end is not None else None,
                "audio_s": round(len(context.audio) / 2 / context.rate, 3), "wav": str(path)}
    finally:
        await context.ws.close()
        task.cancel()


async def cancel_trial(url: str, text: str, language: str, index: int) -> dict:
    context, task = await open_context(url, f"cancel-{language}-{index}")
    try:
        start = await context.send("text.append", text=text)
        await context.send("text.end")
        waited = time.perf_counter()
        while len(context.frames) < 2 and time.perf_counter() - waited < 60 and not context.done.is_set():
            await asyncio.sleep(0.005)
        if len(context.frames) < 2:
            return {"kind": "cancel", "language": language, "index": index, "error": "no audio before cancel"}
        audio_before = len(context.audio) / 2 / context.rate
        cancel_at = await context.send("context.cancel")
        await asyncio.wait_for(context.done.wait(), 30)
        await asyncio.sleep(1.0)  # collect anything late
        acknowledged = context.cancelled_at
        after_ack = [n for t, n in context.frames if acknowledged is not None and t > acknowledged]
        between = [n for t, n in context.frames if t > cancel_at and (acknowledged is None or t <= acknowledged)]
        done_events = [t for t, m in context.events if m.get("type") == "audio.done"]
        return {"kind": "cancel", "language": language, "index": index,
                "audio_s_before_cancel": round(audio_before, 3),
                "cancelled_ack": acknowledged is not None,
                "cancel_ack_latency_s": round(acknowledged - cancel_at, 4) if acknowledged else None,
                "frames_between_cancel_and_ack": len(between),
                "audio_s_between_cancel_and_ack": round(sum(between) / context.rate, 3),
                "frames_after_ack": len(after_ack), "audio_done_sent": bool(done_events),
                # audio.done instead of context.cancelled: the context had
                # finished generating before the server read the cancel.
                "audio_done_minus_cancel_s": round(done_events[0] - cancel_at, 4) if done_events else None,
                "first_audio_s": round(context.frames[0][0] - start, 4)}
    finally:
        await context.ws.close()
        task.cancel()


async def run_incremental(url: str, languages: list[str], out: Path, interval: float) -> list[dict]:
    rows = []
    for language in languages:
        for index, text in enumerate(INCREMENTAL[language]):
            row = await held_back(url, text, language, index, out)
            rows.append(row)
            print(f"  held-back {language}{index} before_rest={row['audio_before_rest']} "
                  f"({row['audio_s_before_rest']}s) first={row['first_audio_s']}", flush=True)
        for index, text in enumerate(INCREMENTAL[language]):
            row = await word_by_word(url, text, language, index, out, interval)
            rows.append(row)
            print(f"  words {language}{index} first={row['first_audio_s']} span={row['text_span_s']}", flush=True)
    return rows


async def run_cancel(url: str, languages: list[str], trials: int) -> list[dict]:
    rows = []
    for language in languages:
        for index in range(trials):
            row = await cancel_trial(url, CANCEL[language], language, index)
            rows.append(row)
            print(f"  cancel {language}{index} {row}", flush=True)
    return rows


# ---------------------------------------------------------------------------
# intelligibility


def transcribe(client: httpx.Client, asr: str, audio: np.ndarray, rate: int) -> str:
    if rate != 16000:
        divisor = gcd(rate, 16000)
        audio = resample_poly(audio, 16000 // divisor, rate // divisor).astype(np.float32)
    session = client.post(f"{asr}/api/start").json()["session_id"]
    step = 16000 * 2
    for offset in range(0, len(audio), step):
        body = np.ascontiguousarray(audio[offset:offset + step], dtype="<f4").tobytes()
        client.post(f"{asr}/api/chunk", params={"session_id": session}, content=body,
                    headers={"Content-Type": "application/octet-stream"}).raise_for_status()
    result = client.post(f"{asr}/api/finish", params={"session_id": session}).json()
    return result.get("text", "")


def normalise_en(text: str) -> list[str]:
    text = unicodedata.normalize("NFKC", text).lower().replace("’", "'")
    text = re.sub(r"[^a-z0-9' ]+", " ", text)
    return text.split()


def normalise_zh(text: str) -> list[str]:
    text = unicodedata.normalize("NFKC", text)
    return [c for c in text if re.match(r"[㐀-鿿0-9a-zA-Z]", c)]


def edit_distance(reference: list[str], hypothesis: list[str]) -> int:
    previous = list(range(len(hypothesis) + 1))
    for i, ref in enumerate(reference, 1):
        current = [i] + [0] * len(hypothesis)
        for j, hyp in enumerate(hypothesis, 1):
            current[j] = min(previous[j] + 1, current[j - 1] + 1, previous[j - 1] + (ref != hyp))
        previous = current
    return previous[-1]


class LocalASR:
    """Qwen3-ASR-0.6B in-process (transformers backend), for when :9102 is down.

    Same checkpoint as the coordinator's service, run offline over the whole
    utterance instead of through the streaming start/chunk/finish loop. Needs
    the ``qwen_asr`` package: run ttsbench with ``.runtime/qwen-asr/bin/python``.
    """

    def __init__(self) -> None:
        import torch
        from qwen_asr import Qwen3ASRModel

        self.model = Qwen3ASRModel.from_pretrained("Qwen/Qwen3-ASR-0.6B", dtype=torch.bfloat16, device_map="cuda",
                                                   max_inference_batch_size=8)

    def __call__(self, audio: np.ndarray, rate: int) -> str:
        if rate != 16000:
            divisor = gcd(rate, 16000)
            audio = resample_poly(audio, 16000 // divisor, rate // divisor).astype(np.float32)
        return self.model.transcribe(audio=(audio, 16000))[0].text


def run_score(rows: list[dict], asr: str, backend: str = "service") -> None:
    local = LocalASR() if backend == "local" else None
    with httpx.Client(timeout=120) as client:
        for row in rows:
            if "wav" not in row or not Path(row["wav"]).exists():
                continue
            audio, rate = load_wav(Path(row["wav"]))
            if len(audio) == 0:
                row.update(asr_text="", errors_count=None, ref_units=None, error_rate=1.0)
                continue
            hypothesis = local(audio, rate) if local else transcribe(client, asr, audio, rate)
            split = normalise_zh if row["language"] == "zh" else normalise_en
            reference, hyp = split(row["text"]), split(hypothesis)
            errors = edit_distance(reference, hyp)
            row.update(asr_text=hypothesis, errors_count=errors, ref_units=len(reference),
                       error_rate=round(errors / max(1, len(reference)), 4))


def summarise(report: dict) -> dict:
    summary = {}
    rows = report.get("rows", [])
    for language in ("en", "zh"):
        complete = [r for r in rows if r["kind"] == "complete" and r["language"] == language]
        if complete:
            scored = [r for r in complete if r.get("errors_count") is not None]
            summary[f"complete_{language}"] = {
                "n": len(complete),
                "ttfa_p50_s": percentile([r["ttfa_s"] for r in complete], 50),
                "ttfa_p90_s": percentile([r["ttfa_s"] for r in complete], 90),
                "rtf_mean": round(statistics.mean(r["rtf"] for r in complete if r["rtf"]), 4),
                "rtf_p90": percentile([r["rtf"] for r in complete], 90),
                "audio_s_total": round(sum(r["audio_s"] for r in complete), 2),
                "error_rate": round(sum(r["errors_count"] for r in scored) / max(1, sum(r["ref_units"] for r in scored)), 4)
                if scored else None,
                "metric": "CER" if language == "zh" else "WER",
            }
        held = [r for r in rows if r["kind"] == "held_back" and r["language"] == language]
        if held:
            scored = [r for r in held if r.get("errors_count") is not None]
            summary[f"held_back_{language}"] = {
                "n": len(held), "audio_before_rest": sum(r["audio_before_rest"] for r in held),
                "audio_s_before_rest_mean": round(statistics.mean(r["audio_s_before_rest"] for r in held), 3),
                "first_audio_p50_s": percentile([r["first_audio_s"] for r in held], 50),
                "error_rate": round(sum(r["errors_count"] for r in scored) / max(1, sum(r["ref_units"] for r in scored)), 4)
                if scored else None,
            }
        words = [r for r in rows if r["kind"] == "word_by_word" and r["language"] == language]
        if words:
            scored = [r for r in words if r.get("errors_count") is not None]
            summary[f"word_by_word_{language}"] = {
                "n": len(words), "first_audio_p50_s": percentile([r["first_audio_s"] for r in words], 50),
                "first_audio_p90_s": percentile([r["first_audio_s"] for r in words], 90),
                "first_audio_before_text_end": sum(r["first_audio_before_text_end"] for r in words),
                "text_span_s_mean": round(statistics.mean(r["text_span_s"] for r in words), 3),
                "error_rate": round(sum(r["errors_count"] for r in scored) / max(1, sum(r["ref_units"] for r in scored)), 4)
                if scored else None,
            }
        cancels = [r for r in rows if r["kind"] == "cancel" and r["language"] == language and "error" not in r]
        if cancels:
            summary[f"cancel_{language}"] = {
                "n": len(cancels), "acknowledged": sum(r["cancelled_ack"] for r in cancels),
                "frames_after_ack": sum(r["frames_after_ack"] for r in cancels),
                "frames_between_cancel_and_ack": sum(r["frames_between_cancel_and_ack"] for r in cancels),
                "ack_latency_p50_s": percentile([r["cancel_ack_latency_s"] for r in cancels], 50),
                "audio_done_after_cancel": sum(r["audio_done_sent"] for r in cancels),
            }
    return summary


# ---------------------------------------------------------------------------
# summary markdown


def fmt(value, digits=3):
    if value is None:
        return "-"
    if isinstance(value, float):
        return f"{value:.{digits}f}"
    return str(value)


def write_summary() -> Path:
    reports = []
    for path in sorted(RESULTS.glob("*.json")):
        try:
            report = json.loads(path.read_text())
        except Exception:  # noqa: BLE001
            continue
        # Other agents also write into results/tts; only ttsbench reports.
        if isinstance(report, dict) and {"name", "rows", "summary"} <= set(report):
            reports.append(report)
    scorers = sorted({report.get("scorer", "not scored") for report in reports})
    lines = ["# Local incremental TTS - component results (plan P3 / cell C4)", "",
             "Generated by `tools/duplexmodels/ttsbench.py --summary` from the per-service JSON files in this "
             "directory. Intelligibility: WER for English, CER for Mandarin, after lower-casing and stripping "
             f"punctuation; transcribed by {'; '.join(scorers)}.", ""]
    lines += ["## Services", "",
              "| Service | Model | Port | Declared incremental_text / granularity / nonterminal flush | Voice | GPU (nvidia-smi) | host load avg (1 min) during runs |",
              "| --- | --- | --- | --- | --- | --- | --- |"]
    for report in reports:
        health = report.get("health", {})
        caps = health.get("capabilities", {})
        gpu = report.get("gpu", {})
        lines.append(f"| {report['name']} | {health.get('model', '-')} | {report['url'].rsplit(':', 1)[-1]} | "
                     f"{caps.get('incremental_text')} / {caps.get('input_granularity')} / {caps.get('nonterminal_flush')} | "
                     f"{health.get('voice', '-')} | {fmt(gpu.get('nvidia_smi_mib'))} MiB | "
                     f"{', '.join(str(v.get('before', ['-'])[0]) for v in report.get('load_average', {}).values())} "
                     f"(of {os.cpu_count()} CPUs) |")
    lines += ["", "## Complete text (`/v1/audio/speech`)", "",
              "| Service | Lang | n | TTFA p50 (s) | TTFA p90 (s) | RTF mean | RTF p90 | WER/CER |",
              "| --- | --- | --- | --- | --- | --- | --- | --- |"]
    for report in reports:
        for language in ("en", "zh"):
            row = report.get("summary", {}).get(f"complete_{language}")
            if row:
                lines.append(f"| {report['name']} | {language} | {row['n']} | {fmt(row['ttfa_p50_s'])} | "
                             f"{fmt(row['ttfa_p90_s'])} | {fmt(row['rtf_mean'])} | {fmt(row['rtf_p90'])} | "
                             f"{row['metric']} {fmt(row['error_rate'])} |")
    lines += ["", "## Incremental text (`/v1/tts/stream`, same context)", "",
              "Held-back suffix: first half appended, 1.5 s wait, then the rest. Word-by-word: one word "
              "(or two Han characters) every 60 ms, time measured from the first append.", "",
              "| Service | Lang | audio before rest (sentences) | mean audio before rest (s) | held-back WER/CER | "
              "word-by-word first audio p50 / p90 (s) | text span (s) | word-by-word WER/CER |",
              "| --- | --- | --- | --- | --- | --- | --- | --- |"]
    for report in reports:
        summary = report.get("summary", {})
        for language in ("en", "zh"):
            held = summary.get(f"held_back_{language}")
            words = summary.get(f"word_by_word_{language}")
            if held or words:
                held = held or {}
                words = words or {}
                lines.append(f"| {report['name']} | {language} | {held.get('audio_before_rest', '-')}/{held.get('n', '-')} | "
                             f"{fmt(held.get('audio_s_before_rest_mean'))} | {fmt(held.get('error_rate'))} | "
                             f"{fmt(words.get('first_audio_p50_s'))} / {fmt(words.get('first_audio_p90_s'))} | "
                             f"{fmt(words.get('text_span_s_mean'))} | {fmt(words.get('error_rate'))} |")
    lines += ["", "## Cancel mid-audio", "",
              "| Service | Lang | trials | acknowledged | frames after `context.cancelled` | frames between cancel and ack | "
              "ack latency p50 (s) | `audio.done` after cancel |",
              "| --- | --- | --- | --- | --- | --- | --- | --- |"]
    for report in reports:
        for language in ("en", "zh"):
            row = report.get("summary", {}).get(f"cancel_{language}")
            if row:
                lines.append(f"| {report['name']} | {language} | {row['n']} | {row['acknowledged']} | "
                             f"{row['frames_after_ack']} | {row['frames_between_cancel_and_ack']} | "
                             f"{fmt(row['ack_latency_p50_s'])} | {row['audio_done_after_cancel']} |")
    notes = [r for r in reports if r.get("notes")]
    if notes:
        lines += ["", "## Notes", ""]
        for report in notes:
            lines.append(f"- **{report['name']}**: {report['notes']}")
    path = RESULTS / "SUMMARY.md"
    path.write_text("\n".join(lines) + "\n")
    return path


# ---------------------------------------------------------------------------


def main() -> None:
    global RESULTS
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--name", help="service name (report file name)")
    parser.add_argument("--url", help="service base URL, e.g. http://127.0.0.1:9120")
    parser.add_argument("--languages", default="en,zh")
    parser.add_argument("--phases", default="complete,incremental,cancel,score")
    parser.add_argument("--asr", default="http://127.0.0.1:9102")
    parser.add_argument("--asr-backend", choices=["service", "local"], default="service",
                        help="service: Qwen3-ASR start/chunk/finish at --asr; local: same checkpoint in-process")
    parser.add_argument("--interval", type=float, default=0.06, help="seconds between word appends")
    parser.add_argument("--cancel-trials", type=int, default=3)
    parser.add_argument("--notes", default="")
    parser.add_argument("--summary", action="store_true", help="only rebuild SUMMARY.md")
    parser.add_argument("--results-dir", type=Path, default=RESULTS)
    args = parser.parse_args()
    RESULTS = args.results_dir
    if args.summary:
        print(write_summary())
        return
    languages = [language for language in args.languages.split(",") if language]
    phases = set(args.phases.split(","))
    report_path = RESULTS / f"{args.name}.json"
    report = json.loads(report_path.read_text()) if report_path.exists() else {"name": args.name, "rows": []}
    report["name"], report["url"] = args.name, args.url
    if args.notes:
        report["notes"] = args.notes
    out = RESULTS / args.name
    health = httpx.get(f"{args.url}/health", timeout=10).json()
    report["health"] = health
    report["gpu"] = gpu_memory(int(args.url.rsplit(":", 1)[-1]))
    # The host is shared; a saturated CPU inflates every Python-driven decode
    # loop, so the load at measurement time is part of the result.
    report.setdefault("load_average", {})[",".join(sorted(phases))] = {
        "before": [round(x, 1) for x in os.getloadavg()], "cpus": os.cpu_count()}
    keep = []
    for row in report["rows"]:
        kind = {"complete": "complete", "held_back": "incremental", "word_by_word": "incremental",
                "cancel": "cancel"}[row["kind"]]
        if kind not in phases or row["language"] not in languages:
            keep.append(row)
    rows = keep
    if "complete" in phases:
        print("complete text", flush=True)
        rows += run_complete(args.url, args.name, languages, out)
    if "incremental" in phases:
        print("incremental", flush=True)
        rows += asyncio.run(run_incremental(args.url, languages, out, args.interval))
    if "cancel" in phases:
        print("cancel", flush=True)
        rows += asyncio.run(run_cancel(args.url, languages, args.cancel_trials))
    if "score" in phases:
        print("scoring with Qwen3-ASR", flush=True)
        run_score(rows, args.asr, args.asr_backend)
        report["scorer"] = ("Qwen3-ASR-0.6B service " + args.asr) if args.asr_backend == "service" \
            else "Qwen3-ASR-0.6B in-process (transformers backend)"
    report["rows"] = rows
    report["health_after"] = httpx.get(f"{args.url}/health", timeout=10).json()
    report["gpu"] = gpu_memory(int(args.url.rsplit(":", 1)[-1]))
    report["load_average"][",".join(sorted(phases))]["after"] = [round(x, 1) for x in os.getloadavg()]
    report["summary"] = summarise(report)
    report["updated"] = time.strftime("%Y-%m-%dT%H:%M:%S")
    RESULTS.mkdir(parents=True, exist_ok=True)
    report_path.write_text(json.dumps(report, indent=1, ensure_ascii=False))
    print(json.dumps(report["summary"], indent=1, ensure_ascii=False))
    print(write_summary())


if __name__ == "__main__":
    main()
