#!/usr/bin/env python3
"""Engine-side behavioural harness for the native duplex sidecars (N1/N2).

It plays the engine's half of the sidecar protocol against one sidecar (spawned
over stdio or reached over TCP) with wall-clock-paced 24 kHz audio, and records
every frame the sidecar returns with its arrival time. Three scenarios:

* ``question``   - one recorded question, then silence (fed by the harness, or
                   left to the sidecar's own idle fill with ``--stop-after``).
                   Measures first-audio latency after the end of the question.
* ``interrupt``  - the question, then a second recorded question injected over
                   the model's answer ``--interrupt-after`` seconds after its
                   first audio. Measures whether (and how fast) the answer
                   stops and whether the new question is answered.
* ``tool``       - a recorded tool request with ``--tools`` declared in hello;
                   each ``tool_call`` is answered after ``--tool-delay`` seconds
                   with ``--tool-output`` (or ``--tool-error``). Records what the
                   model says while waiting and after the result.

Timing uses one monotonic clock on this side of the pipe: ``t`` is seconds
since the first audio frame was sent; audio input positions are known exactly
because the harness schedules them. Output timing is packet arrival, not
rendered playback; no playback acknowledgement is fabricated.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import shlex
import socket
import subprocess
import sys
import threading
import time
import wave
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "sidecars"))
from openrealtime_sidecar.protocol import read_message, write_message  # noqa: E402

RATE = 24_000
PACKET = 480  # 20 ms


def load_audio(path: str, channel: int = 0) -> np.ndarray:
    """Load any wav as float32 mono 24 kHz."""
    try:
        import soundfile as sf  # noqa: PLC0415

        data, rate = sf.read(path, dtype="float32", always_2d=True)
        data = data[:, min(channel, data.shape[1] - 1)]
    except ImportError:
        with wave.open(path, "rb") as handle:
            rate = handle.getframerate()
            raw = np.frombuffer(handle.readframes(handle.getnframes()), dtype="<i2")
            data = raw.reshape(-1, handle.getnchannels())[:, channel].astype(np.float32) / 32768.0
    if rate != RATE:
        count = int(len(data) * RATE / rate)
        data = np.interp(np.linspace(0, len(data) - 1, count), np.arange(len(data)), data).astype(np.float32)
    return data.astype(np.float32)


def pcm16(samples: np.ndarray) -> bytes:
    return (np.clip(samples, -1.0, 1.0) * 32767).astype("<i2").tobytes()


class Session:
    def __init__(self, arguments) -> None:
        self.arguments = arguments
        if arguments.address:
            host, port = arguments.address.removeprefix("tcp:").rsplit(":", 1)
            self.sock = socket.create_connection((host, int(port)))
            self.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            self.reader = self.sock.makefile("rb")
            self.writer = self.sock.makefile("wb", buffering=0)
            self.process = None
        else:
            stderr = open(arguments.stderr, "w") if arguments.stderr else None
            self.process = subprocess.Popen(
                shlex.split(arguments.sidecar), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                stderr=stderr or None)
            self.reader, self.writer = self.process.stdout, self.process.stdin
        self.lock = threading.Lock()
        self.events: list[dict] = []
        self.output = bytearray()
        self.output_rate = 0
        self.t0: float | None = None
        self.ready = None
        self.closed = threading.Event()

    def now(self) -> float:
        return time.monotonic() - (self.t0 if self.t0 is not None else time.monotonic())

    def send(self, kind: str, payload: bytes = b"", **header) -> None:
        with self.lock:
            write_message(self.writer, kind, payload, **header)

    def hello(self, tools: list[dict]) -> dict:
        started = time.monotonic()
        self.send("hello", version=self.arguments.protocol, sample_rate=RATE,
                  instructions=self.arguments.instructions, voice="default", tools=tools or None,
                  interaction_owner="model" if self.arguments.protocol >= 2 else None,
                  floor_owner="model" if self.arguments.protocol >= 2 else None)
        ready = read_message(self.reader)
        self.ready = {"type": ready.type, **ready.header, "handshake_s": round(time.monotonic() - started, 3)}
        if ready.type != "ready":
            raise SystemExit(f"handshake failed: {self.ready}")
        self.output_rate = int(ready.get("output_rate", 24000))
        return self.ready

    def read_loop(self, on_frame) -> None:
        try:
            while True:
                message = read_message(self.reader)
                if message is None:
                    break
                t = self.now()
                entry = {"t": round(t, 3), "type": message.type}
                if message.type == "output_audio":
                    samples = len(message.payload) // 2
                    entry["samples"] = samples
                    entry["rms"] = round(float(np.sqrt(np.mean(np.square(
                        np.frombuffer(message.payload, dtype="<i2").astype(np.float32) / 32768.0)))), 4)
                    self.output.extend(message.payload)
                else:
                    entry.update({k: v for k, v in message.header.items() if k not in ("type",)})
                self.events.append(entry)
                on_frame(message, t)
        finally:
            self.closed.set()

    def close(self) -> None:
        try:
            self.send("bye")
        except Exception:  # noqa: BLE001
            pass
        self.closed.wait(timeout=15)
        if self.process is not None:
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.kill()


def audible_segments(events: list[dict], rate: int, threshold: float = 0.01, gap: float = 0.4) -> list[list[float]]:
    """Group output packets whose RMS exceeds threshold into [start, end] runs."""
    segments: list[list[float]] = []
    for event in events:
        if event["type"] != "output_audio" or event.get("rms", 0) < threshold:
            continue
        duration = event["samples"] / rate
        start = event["t"]
        if segments and start - segments[-1][1] <= gap:
            segments[-1][1] = max(segments[-1][1], start + duration)
        else:
            segments.append([start, start + duration])
    return segments


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--sidecar", help="sidecar command (stdio)")
    parser.add_argument("--address", help="tcp:HOST:PORT of a listening sidecar")
    parser.add_argument("--stderr", help="write the spawned sidecar's stderr here")
    parser.add_argument("--protocol", type=int, default=1)
    parser.add_argument("--scenario", choices=["question", "interrupt", "tool"], required=True)
    parser.add_argument("--question", required=True, help="wav with the first user turn")
    parser.add_argument("--question-channel", type=int, default=0)
    parser.add_argument("--question-trim", type=float, default=0.0,
                        help="seconds of the question wav to use (0 = all)")
    parser.add_argument("--lead-silence", type=float, default=2.0)
    parser.add_argument("--interruption", help="wav with the interrupting question")
    parser.add_argument("--interrupt-after", type=float, default=1.5,
                        help="seconds after the first answer audio to start the interruption")
    parser.add_argument("--send-interrupt", action="store_true",
                        help="also send the protocol interrupt frame at the interruption onset "
                             "(what an engine barge-in policy would do)")
    parser.add_argument("--tools", default=None, help="JSON list of hello tools")
    parser.add_argument("--tool-delay", type=float, default=4.0)
    parser.add_argument("--tool-output", default=None)
    parser.add_argument("--tool-error", default=None)
    parser.add_argument("--instructions", default="")
    parser.add_argument("--duration", type=float, default=40.0, help="total seconds of session")
    parser.add_argument("--stop-after", type=float, default=0.0,
                        help="stop sending audio this many seconds after the question ends (0 = keep sending silence)")
    parser.add_argument("--label", default="run")
    parser.add_argument("--out", required=True, help="JSON result path; a .wav of the output is written beside it")
    arguments = parser.parse_args()

    tools = json.loads(arguments.tools) if arguments.tools else []
    session = Session(arguments)
    ready = session.hello(tools)
    print(f"ready: {json.dumps(ready)[:300]}", flush=True)

    question = load_audio(arguments.question, arguments.question_channel)
    if arguments.question_trim > 0:
        question = question[: int(arguments.question_trim * RATE)]
    interruption = load_audio(arguments.interruption) if arguments.interruption else None
    lead = int(arguments.lead_silence * RATE)
    question_start = lead / RATE
    question_end = question_start + len(question) / RATE

    state = {"first_audio": None, "interrupt_at": None, "tool_calls": [], "interrupt_injected": False}
    pending_results: list[tuple[float, dict]] = []
    result_lock = threading.Lock()

    def on_frame(message, t: float) -> None:
        if message.type == "output_audio" and t >= question_start:
            rms = float(np.sqrt(np.mean(np.square(np.frombuffer(message.payload, dtype="<i2").astype(np.float32) / 32768.0))))
            if state["first_audio"] is None and rms >= 0.01 and t > question_end - 0.5:
                state["first_audio"] = t
                if interruption is not None:
                    state["interrupt_at"] = t + arguments.interrupt_after
        if message.type == "tool_call":
            call = {"t": round(t, 3), "call_id": message.get("call_id"), "name": message.get("name"),
                    "arguments": message.get("arguments")}
            state["tool_calls"].append(call)
            print(f"[{t:6.2f}] tool_call {call}", flush=True)
            if arguments.scenario == "tool":
                with result_lock:
                    pending_results.append((t + arguments.tool_delay, call))
        elif message.type in ("text_delta", "text_done", "turn_done", "error", "log", "transcript"):
            text = message.get("text", "")
            print(f"[{t:6.2f}] {message.type} {text!r}" if text else f"[{t:6.2f}] {message.type}", flush=True)

    reader = threading.Thread(target=session.read_loop, args=(on_frame,), daemon=True)
    session.t0 = time.monotonic()
    reader.start()

    total = int(arguments.duration * RATE)
    timeline = np.zeros(total + len(question) + lead, dtype=np.float32)
    timeline[lead:lead + len(question)] = question
    sent_samples = 0
    interrupt_start_sample = None
    interrupt_control = None
    stop_sending_at = question_end + arguments.stop_after if arguments.stop_after > 0 else None
    tool_results_sent = []
    while sent_samples < total and not session.closed.is_set():
        t_packet = sent_samples / RATE
        # Wall-clock pacing: never send ahead of real time.
        delay = session.t0 + t_packet - time.monotonic()
        if delay > 0:
            time.sleep(delay)
        now = session.now()
        if interruption is not None and state["interrupt_at"] is not None and not state["interrupt_injected"] \
                and now >= state["interrupt_at"]:
            start = sent_samples
            end = min(start + len(interruption), len(timeline))
            timeline[start:end] = interruption[: end - start]
            state["interrupt_injected"] = True
            interrupt_start_sample = start
            if arguments.send_interrupt:
                control_started = session.now()
                session.send("interrupt")
                interrupt_control = {"write_started_s": control_started,
                                     "write_completed_s": session.now()}
            print(f"[{now:6.2f}] injecting interruption ({len(interruption) / RATE:.2f}s)", flush=True)
        with result_lock:
            due = [item for item in pending_results if item[0] <= now]
            for item in due:
                pending_results.remove(item)
        for _, call in due:
            header = {"call_id": call["call_id"], "name": call["name"]}
            if arguments.tool_error:
                header["error"] = arguments.tool_error
            else:
                header["output"] = json.loads(arguments.tool_output) if arguments.tool_output and \
                    arguments.tool_output.strip()[:1] in "{[\"0123456789" else (arguments.tool_output or "ok")
            session.send("tool_result", **header)
            tool_results_sent.append({"t": round(now, 3), **header})
            print(f"[{now:6.2f}] tool_result sent {header}", flush=True)
        if stop_sending_at is None or t_packet < stop_sending_at:
            packet = timeline[sent_samples:sent_samples + PACKET]
            session.send("audio", pcm16(packet))
        sent_samples += PACKET

    session.close()
    events = session.events
    segments = audible_segments(events, session.output_rate)
    texts = [e for e in events if e["type"] in ("text_delta", "text_done")]
    summary: dict = {
        "label": arguments.label, "scenario": arguments.scenario, "ready": ready,
        "timing_basis": "received packets; no rendered playback receipts",
        "load_average": list(os.getloadavg()),
        "question": arguments.question, "question_start_s": round(question_start, 3),
        "question_end_s": round(question_end, 3),
        "first_audio_s": state["first_audio"],
        "first_audio_latency_s": round(state["first_audio"] - question_end, 3) if state["first_audio"] else None,
        "audible_segments": [[round(a, 3), round(b, 3)] for a, b in segments],
        "turn_done_s": [e["t"] for e in events if e["type"] == "turn_done"],
        "text_done": [{"t": e["t"], "text": e.get("text", "")} for e in events if e["type"] == "text_done"],
        "text_deltas": "".join(e.get("text", "") for e in events if e["type"] == "text_delta"),
        "tool_calls": state["tool_calls"], "tool_results": tool_results_sent,
        "errors": [e for e in events if e["type"] == "error"],
        "logs": [e for e in events if e["type"] == "log"],
        "output_seconds": round(len(session.output) / 2 / max(session.output_rate, 1), 3),
    }
    if interrupt_control is not None:
        # Write completion is a client-side timestamp, not a server ack. Frames
        # already in transport may still arrive; keep this diagnostic separate
        # from rendered playback and model-native interruption measurements.
        sent_at = interrupt_control["write_completed_s"]
        boundary = next((e["t"] for e in events
                         if e["type"] == "turn_done" and e["t"] >= sent_at), None)
        trailing = [e for e in events if e["type"] == "output_audio"
                    and e["t"] >= sent_at and (boundary is None or e["t"] <= boundary)]
        summary["interrupt_control"] = {
            **interrupt_control, "next_turn_done_s": boundary,
            "boundary_latency_ms": (boundary - sent_at) * 1000 if boundary is not None else None,
            "packets_received_before_boundary": len(trailing),
            "audio_received_before_boundary_ms": sum(e["samples"] for e in trailing)
                * 1000 / max(session.output_rate, 1),
            "interpretation": "client receive timing; includes in-flight packets; no server acknowledgement",
        }
    if interrupt_start_sample is not None:
        onset = interrupt_start_sample / RATE
        summary["interrupt_start_s"] = round(onset, 3)
        summary["interrupt_end_s"] = round(onset + len(interruption) / RATE, 3)
        before = [s for s in segments if s[0] < onset]
        overlapping = [s for s in segments if s[0] < onset <= s[1]]
        after = [s for s in segments if s[0] >= onset]
        summary["answer_audio_stopped_after_onset_s"] = (
            round(overlapping[0][1] - onset, 3) if overlapping else 0.0 if before else None)
        interruption_end = onset + len(interruption) / RATE
        second = [s for s in segments if s[0] >= interruption_end - 0.5]
        summary["second_answer_start_s"] = round(second[0][0], 3) if second else None
        summary["second_answer_latency_s"] = round(second[0][0] - interruption_end, 3) if second else None
        summary["text_before_interrupt"] = "".join(
            e.get("text", "") for e in events if e["type"] == "text_delta" and e["t"] < onset)
        summary["text_after_interrupt"] = "".join(
            e.get("text", "") for e in events if e["type"] == "text_delta" and e["t"] >= onset)
        summary["segments_after_onset"] = [[round(a, 3), round(b, 3)] for a, b in after]
    out = Path(arguments.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(summary, indent=2, ensure_ascii=False))
    Path(str(out.with_suffix("")) + ".events.jsonl").write_text(
        "".join(json.dumps(e, ensure_ascii=False) + "\n" for e in events))
    if session.output:
        with wave.open(str(out.with_suffix(".wav")), "wb") as handle:
            handle.setnchannels(1)
            handle.setsampwidth(2)
            handle.setframerate(session.output_rate)
            handle.writeframes(bytes(session.output))
    print(json.dumps({k: summary[k] for k in summary if k not in ("logs", "ready")}, indent=1, ensure_ascii=False)[:3000])


if __name__ == "__main__":
    main()
