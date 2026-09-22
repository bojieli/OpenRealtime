#!/usr/bin/env python3
"""Minimal OpenAI-Realtime client for the native-duplex e2e checks.

Connects to an ``openrealtime serve`` endpoint, optionally declares tools, streams
one recorded question (24 kHz PCM16, wall-clock paced, then silence), and logs
every server event with its arrival time. Used to record exactly what the
engine does with a sidecar ``tool_call`` (N2) and to sanity-check a served
native-duplex binding before the benchmark suites run.
"""

from __future__ import annotations

import argparse
import base64
import json
import time
from pathlib import Path

import numpy as np
from websockets.sync.client import connect

RATE = 24_000


def load(path: str, channel: int) -> np.ndarray:
    import soundfile as sf  # noqa: PLC0415

    data, rate = sf.read(path, dtype="float32", always_2d=True)
    data = data[:, min(channel, data.shape[1] - 1)]
    if rate != RATE:
        count = int(len(data) * RATE / rate)
        data = np.interp(np.linspace(0, len(data) - 1, count), np.arange(len(data)), data)
    return data.astype(np.float32)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", default="ws://127.0.0.1:9241/v1/realtime")
    parser.add_argument("--wav", required=True)
    parser.add_argument("--channel", type=int, default=0)
    parser.add_argument("--trim", type=float, default=0.0)
    parser.add_argument("--tools", default=None, help="JSON list of Realtime function tools")
    parser.add_argument("--instructions", default=None)
    parser.add_argument("--lead", type=float, default=1.0)
    parser.add_argument("--duration", type=float, default=30.0)
    parser.add_argument("--function-output", default=None,
                        help="answer any response.function_call_arguments.done with this output after --delay")
    parser.add_argument("--delay", type=float, default=3.0)
    parser.add_argument("--out", required=True)
    arguments = parser.parse_args()

    audio = load(arguments.wav, arguments.channel)
    if arguments.trim > 0:
        audio = audio[: int(arguments.trim * RATE)]
    lead = np.zeros(int(arguments.lead * RATE), dtype=np.float32)
    total = int(arguments.duration * RATE)
    timeline = np.concatenate([lead, audio])
    timeline = np.concatenate([timeline, np.zeros(max(0, total - len(timeline)), dtype=np.float32)])
    events: list[dict] = []
    t0 = None
    pending_outputs: list[tuple[float, str]] = []
    with connect(arguments.endpoint, max_size=None, open_timeout=30) as ws:
        session: dict = {"type": "realtime", "audio": {
            "input": {"format": {"type": "audio/pcm", "rate": RATE}},
            "output": {"format": {"type": "audio/pcm", "rate": RATE}}}}
        if arguments.tools:
            session["tools"] = json.loads(arguments.tools)
        if arguments.instructions:
            session["instructions"] = arguments.instructions
        ws.send(json.dumps({"type": "session.update", "session": session}))
        t0 = time.monotonic()
        sent = 0
        packet = 480
        while sent < len(timeline):
            due = t0 + sent / RATE
            while True:
                remaining = due - time.monotonic()
                try:
                    raw = ws.recv(timeout=max(0.0, remaining)) if remaining > 0 else ws.recv(timeout=0)
                except TimeoutError:
                    break
                event = json.loads(raw)
                t = round(time.monotonic() - t0, 3)
                kind = event.get("type", "")
                compact = {"t": t, "type": kind}
                if kind.endswith("audio.delta"):
                    compact["bytes"] = len(base64.b64decode(event.get("delta", "")))
                else:
                    compact.update({k: v for k, v in event.items() if k != "type"})
                events.append(compact)
                if kind not in ("response.output_audio.delta", "response.audio.delta"):
                    print(json.dumps(compact, ensure_ascii=False)[:400], flush=True)
                if kind == "response.function_call_arguments.done" and arguments.function_output is not None:
                    pending_outputs.append((time.monotonic() + arguments.delay, event.get("call_id", "")))
            now = time.monotonic()
            for item in [p for p in pending_outputs if p[0] <= now]:
                pending_outputs.remove(item)
                ws.send(json.dumps({"type": "conversation.item.create", "item": {
                    "type": "function_call_output", "call_id": item[1], "output": arguments.function_output}}))
                events.append({"t": round(now - t0, 3), "type": "client.function_call_output", "call_id": item[1]})
                print(f"sent function_call_output for {item[1]}", flush=True)
            chunk = timeline[sent:sent + packet]
            ws.send(json.dumps({"type": "input_audio_buffer.append", "audio": base64.b64encode(
                (np.clip(chunk, -1, 1) * 32767).astype("<i2").tobytes()).decode("ascii")}))
            sent += packet
    Path(arguments.out).write_text("".join(json.dumps(e, ensure_ascii=False) + "\n" for e in events))
    kinds: dict[str, int] = {}
    for event in events:
        kinds[event["type"]] = kinds.get(event["type"], 0) + 1
    print(json.dumps(kinds, indent=1))


if __name__ == "__main__":
    main()
