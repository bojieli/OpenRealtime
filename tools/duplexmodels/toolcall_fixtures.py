#!/usr/bin/env python3
"""Synthesize the spoken requests of the tool-correctness suite (bench/toolcall).

Each task is one short English request needing one tool call; two contain a
mid-sentence correction. Audio comes from a local synthesis service through
its complete-text route (POST /v1/audio/speech, PCM), is resampled to 24 kHz
mono PCM16 with 0.5 s of leading silence, and is written with its provenance
to bench/toolcall/testdata. Regenerating replaces the audio, so the committed
fixtures.json records the service, voice and a hash of each file.

    python3 tools/duplexmodels/toolcall_fixtures.py --url http://127.0.0.1:9125
"""
import argparse
import hashlib
import json
import wave
from pathlib import Path

import httpx
import numpy as np
from scipy.signal import resample_poly

ROOT = Path(__file__).resolve().parents[2] / "bench/toolcall/testdata"
RATE = 24_000

TASKS = [
    {"id": "weather-paris", "text": "What's the weather like in Paris right now?",
     "tool": "get_weather", "arguments": {"city": "Paris"},
     "result": {"city": "Paris", "temperature_c": 17, "condition": "light rain"}, "spoken": ["17", "seventeen"]},
    {"id": "weather-tokyo", "text": "How warm is it in Tokyo today?",
     "tool": "get_weather", "arguments": {"city": "Tokyo"},
     "result": {"city": "Tokyo", "temperature_c": 24, "condition": "sunny"}, "spoken": ["24", "twenty-four", "twenty four"]},
    {"id": "weather-london", "text": "Is it going to rain in London?",
     "tool": "get_weather", "arguments": {"city": "London"},
     "result": {"city": "London", "temperature_c": 12, "condition": "heavy rain"}, "spoken": ["12", "twelve", "heavy"]},
    {"id": "add-small", "text": "Can you add seventeen and twenty-five for me?",
     "tool": "add_numbers", "arguments": {"a": 17, "b": 25},
     "result": {"sum": 42}, "spoken": ["42", "forty-two", "forty two"]},
    {"id": "add-large", "text": "What is one hundred and twelve plus thirty?",
     "tool": "add_numbers", "arguments": {"a": 112, "b": 30},
     "result": {"sum": 142}, "spoken": ["142", "one hundred forty-two", "one hundred and forty-two",
                                        "one hundred forty two", "one hundred and forty two"]},
    {"id": "timer-ten", "text": "Set a timer for ten minutes, please.",
     "tool": "set_timer", "arguments": {"minutes": 10},
     "result": {"status": "set", "minutes": 10, "ends_at": "3:40 PM"}, "spoken": ["3:40", "three forty", "3 40"]},
    {"id": "timer-five", "text": "Start a five minute timer.",
     "tool": "set_timer", "arguments": {"minutes": 5},
     "result": {"status": "set", "minutes": 5, "ends_at": "3:35 PM"}, "spoken": ["3:35", "three thirty-five", "three thirty five", "3 35"]},
    {"id": "currency-usd-eur", "text": "How much is fifty US dollars in euros?",
     "tool": "convert_currency", "arguments": {"amount": 50, "from": "USD", "to": "EUR"},
     "result": {"amount": 46.1, "currency": "EUR"}, "spoken": ["46", "forty-six", "forty six"]},
    {"id": "correction-weather", "text": "What's the weather in Rome... sorry, I mean Madrid?",
     "tool": "get_weather", "arguments": {"city": "Madrid"}, "stale_arguments": {"city": "Rome"},
     "result": {"city": "Madrid", "temperature_c": 29, "condition": "clear"}, "spoken": ["29", "twenty-nine", "twenty nine"]},
    {"id": "correction-add", "text": "Add eight and nine. No, wait, eight and nineteen.",
     "tool": "add_numbers", "arguments": {"a": 8, "b": 19}, "stale_arguments": {"a": 8, "b": 9},
     "result": {"sum": 27}, "spoken": ["27", "twenty-seven", "twenty seven"]},
]


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--url", default="http://127.0.0.1:9125")
    parser.add_argument("--voice", default="default")
    args = parser.parse_args()
    health = httpx.get(f"{args.url}/health", timeout=10).json()
    (ROOT / "audio").mkdir(parents=True, exist_ok=True)
    fixtures = []
    for task in TASKS:
        with httpx.stream("POST", f"{args.url}/v1/audio/speech", timeout=120, json={
                "input": task["text"], "voice": args.voice, "response_format": "pcm", "stream": True}) as response:
            response.raise_for_status()
            rate = int(response.headers.get("X-Sample-Rate", RATE))
            pcm = b"".join(response.iter_bytes())
        samples = np.frombuffer(pcm, dtype="<i2").astype(np.float64)
        if rate != RATE:
            from math import gcd
            g = gcd(rate, RATE)
            samples = resample_poly(samples, RATE // g, rate // g)
        samples = np.concatenate([np.zeros(RATE // 2), samples])
        data = np.clip(np.rint(samples), -32768, 32767).astype("<i2").tobytes()
        path = ROOT / "audio" / f"{task['id']}.wav"
        with wave.open(str(path), "wb") as out:
            out.setnchannels(1); out.setsampwidth(2); out.setframerate(RATE); out.writeframes(data)
        fixtures.append({**task, "audio": path.name, "seconds": round(len(data) / 2 / RATE, 3),
                         "sha256": hashlib.sha256(path.read_bytes()).hexdigest()})
        print(task["id"], fixtures[-1]["seconds"], "s")
    (ROOT / "fixtures.json").write_text(json.dumps({
        "synthesis": {"url": args.url, "voice": args.voice, "model": health.get("model"),
                      "sample_rate": RATE, "leading_silence_s": 0.5},
        "tasks": fixtures}, indent=2) + "\n")


if __name__ == "__main__":
    main()
