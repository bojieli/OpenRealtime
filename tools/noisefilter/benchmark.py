"""Measure HTTP-inclusive filtering latency and optional speech preservation.

Uses deterministic noise if no PCM16 mono WAV is supplied. An attenuation
measurement on noise is not a speech-recognition or speaker-separation score.
"""

import argparse
import array
import http.client
import json
import math
import random
import statistics
import time
import uuid
import wave
from urllib.parse import urlsplit


def percentile(values, fraction):
    return sorted(values)[min(len(values) - 1, math.ceil(len(values) * fraction) - 1)]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:8125")
    parser.add_argument("--wav")
    parser.add_argument("--out-wav")
    parser.add_argument("--report")
    parser.add_argument("--seconds", type=int, default=30)
    parser.add_argument("--packet-ms", type=int, default=100)
    parser.add_argument("--budget-ms", type=float, default=50)
    parser.add_argument("--realtime", action="store_true", help="pace packets by source audio time")
    args = parser.parse_args()
    if args.seconds <= 0 or not 1 <= args.packet_ms <= 100:
        parser.error("seconds must be positive; packet-ms must be 1..100")
    rate = 24000
    if args.wav:
        with wave.open(args.wav, "rb") as source:
            if source.getnchannels() != 1 or source.getsampwidth() != 2:
                parser.error("WAV must be mono PCM16")
            rate = source.getframerate()
            pcm = source.readframes(source.getnframes())
    else:
        rng = random.Random(7)
        pcm = array.array(
            "h", (rng.randint(-5000, 5000) for _ in range(rate * args.seconds))
        ).tobytes()
    endpoint = urlsplit(args.url)
    connection = http.client.HTTPConnection(endpoint.hostname, endpoint.port, timeout=5)
    identity = uuid.uuid4().hex
    path = endpoint.path.rstrip("/") + "/v1/filter/" + identity
    times, server_times, output = [], [], []
    contract = None
    chunk = rate * args.packet_ms // 1000 * 2
    replay_started = time.perf_counter()
    send_lateness = []
    try:
        for sequence, offset in enumerate(range(0, len(pcm), chunk)):
            due = replay_started + offset / (rate * 2)
            if args.realtime:
                time.sleep(max(0, due - time.perf_counter()))
            body = pcm[offset : offset + chunk]
            started = time.perf_counter()
            if args.realtime:
                send_lateness.append(max(0, started - due) * 1000)
            connection.request(
                "POST",
                path,
                body,
                {
                    "X-Sample-Rate": str(rate),
                    "X-Sequence": str(sequence),
                    "Content-Type": "application/octet-stream",
                },
            )
            response = connection.getresponse()
            filtered = response.read()
            elapsed = (time.perf_counter() - started) * 1000
            if response.status != 200 or len(filtered) != len(body):
                raise RuntimeError(
                    f"filter failed: {response.status} {filtered[:100]!r}"
                )
            observed = (response.getheader("X-Filter-Model"), response.getheader("X-Audio-Delay-MS"))
            if not all(observed) or response.getheader("X-Sequence") != str(sequence):
                raise RuntimeError("missing filter identity/delay or incorrect sequence")
            if contract is not None and observed != contract:
                raise RuntimeError("filter contract changed during session")
            contract = observed
            times.append(elapsed)
            server_times.append(float(response.getheader("X-Processing-MS")))
            output.append(filtered)
    finally:
        connection.request("DELETE", path)
        connection.getresponse().read()
        connection.close()
    filtered = b"".join(output)
    original_samples, filtered_samples = array.array("h"), array.array("h")
    original_samples.frombytes(pcm[rate * 2 :])
    filtered_samples.frombytes(filtered[rate * 2 :])
    original_energy = sum(x * x for x in original_samples)
    filtered_energy = sum(x * x for x in filtered_samples)
    report = {
        "input": args.wav or "deterministic white noise",
        "samples": len(pcm) // 2,
        "rate_hz": rate,
        "packet_ms": args.packet_ms,
        "realtime": args.realtime,
        "send_lateness_ms_max": max(send_lateness) if send_lateness else None,
        "requests": len(times),
        "roundtrip_ms": {
            "p50": statistics.median(times),
            "p95": percentile(times, 0.95),
            "p99": percentile(times, 0.99),
            "max": max(times),
        },
        "server_p99_ms": percentile(server_times, 0.99),
        "model": contract[0],
        "fixed_audio_delay_ms": float(contract[1]),
        "budget_ms": args.budget_ms,
        "deadline_misses": sum(t > args.budget_ms for t in times),
        "energy_change_db_after_first_second": 10
        * math.log10(max(1, filtered_energy) / max(1, original_energy)),
        "scope": "single-session audio frontend; no ASR, speaker, or dialogue accuracy claim",
    }
    if args.out_wav:
        with wave.open(args.out_wav, "wb") as target:
            target.setnchannels(1)
            target.setsampwidth(2)
            target.setframerate(rate)
            target.writeframes(filtered)
    if args.report:
        with open(args.report, "w") as target:
            json.dump(report, target, indent=2)
            target.write("\n")
    print(json.dumps(report, indent=2))
    if report["deadline_misses"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
