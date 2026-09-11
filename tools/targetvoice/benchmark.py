"""Exercise automatic enrollment, then competing speech through the HTTP service.

Supply two clean mono recordings of different speakers (at least seven seconds).
The first speaker is alone for four seconds, then the recordings overlap. The
service receives only the mixture. SI-SDR is measured on held-out overlap after
allowing the three-second reference to finish. This is a synthetic-mixture test,
not a claim about far-field microphones or recognition accuracy.
"""

import argparse
import http.client
import json
import time
import uuid
from pathlib import Path
from urllib.parse import urlsplit
import numpy as np
import soundfile as sf
from scipy.signal import resample_poly


def sisdr(output, target):
    output, target = output - output.mean(), target - target.mean()
    projection = np.dot(output, target) / (np.dot(target, target) + 1e-10) * target
    return float(
        10
        * np.log10(
            (np.sum(projection**2) + 1e-10)
            / (np.sum((output - projection) ** 2) + 1e-10)
        )
    )


def read(path, rate):
    audio, source = sf.read(path, dtype="float32")
    if audio.ndim != 1:
        raise ValueError("provide clean mono recordings")
    if source != rate:
        audio = resample_poly(audio, rate, source)
    return audio / (np.sqrt(np.mean(audio**2)) + 1e-9) * 0.08


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True)
    parser.add_argument("--competitor", required=True)
    parser.add_argument("--url", default="http://127.0.0.1:8126")
    parser.add_argument(
        "--rate", type=int, choices=[16000, 24000, 48000], default=16000
    )
    parser.add_argument("--packet-ms", type=int, default=100)
    parser.add_argument(
        "--out", required=True, help="output directory for audio and JSON"
    )
    args = parser.parse_args()
    if not 1 <= args.packet_ms <= 100:
        parser.error("packet-ms must be 1..100")
    rate = args.rate
    a, b = read(args.target, rate), read(args.competitor, rate)
    n = min(len(a), len(b))
    if n < rate * 7:
        parser.error("both recordings must contain at least seven seconds")
    target = np.concatenate([np.zeros(rate), a[:n], np.zeros(rate)])
    competitor = np.concatenate([np.zeros(rate * 5), b[rate * 4 : n], np.zeros(rate)])
    mixture = target + competitor
    pcm = np.clip(np.rint(mixture * 32768), -32768, 32767).astype("<i2").tobytes()
    url = urlsplit(args.url)
    connection = http.client.HTTPConnection(url.hostname, url.port, timeout=5)
    path = url.path.rstrip("/") + "/v1/filter/" + uuid.uuid4().hex
    output, times, phases = [], [], []
    started = time.perf_counter()
    packet = rate * args.packet_ms // 1000 * 2
    try:
        for sequence, offset in enumerate(range(0, len(pcm), packet)):
            time.sleep(max(0, started + offset / (2 * rate) - time.perf_counter()))
            sent = time.perf_counter()
            body = pcm[offset : offset + packet]
            connection.request(
                "POST",
                path,
                body,
                {"X-Sample-Rate": str(rate), "X-Sequence": str(sequence)},
            )
            response = connection.getresponse()
            data = response.read()
            times.append((time.perf_counter() - sent) * 1000)
            if (
                response.status != 200
                or len(data) != len(body)
                or response.getheader("X-Filter-Model") != "real-tse"
                or response.getheader("X-Sequence") != str(sequence)
                or response.getheader("X-Audio-Delay-MS") != "65"
            ):
                raise ValueError(f"invalid filter response: {response.status}")
            phase = response.getheader("X-Target-Voice-State")
            if not phases or phase != phases[-1]["state"]:
                phases.append(dict(audio_seconds=offset / (rate * 2), state=phase))
            output.append(data)
    finally:
        connection.request("DELETE", path)
        connection.getresponse().read()
        connection.close()
    delayed = np.frombuffer(b"".join(output), dtype="<i2").astype(np.float64) / 32768
    # FIFO is exactly 64ms. Resampling contributes <1ms additional phase delay.
    filtered = delayed[rate * 64 // 1000 :]
    start, end = rate * 5, min(n + rate, len(filtered))
    metrics = dict(
        rate_hz=rate,
        packet_ms=args.packet_ms,
        phases=phases,
        requests=len(times),
        roundtrip_ms=dict(
            p50=float(np.median(times)),
            p99=float(np.percentile(times, 99)),
            max=max(times),
        ),
        over_50ms=sum(t > 50 for t in times),
        fifo_ms=64,
        advertised_audio_delay_bound_ms=65,
        input_sisdr_db=sisdr(mixture[start:end], target[start:end]),
        extracted_sisdr_db=sisdr(filtered[start:end], target[start:end]),
    )
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    for name, audio in [
        ("mixture", mixture),
        ("target", target),
        ("filtered", delayed),
    ]:
        sf.write(out / (name + ".wav"), audio, rate)
    (out / "report.json").write_text(json.dumps(metrics, indent=2) + "\n")
    print(json.dumps(metrics, indent=2))
    if metrics["over_50ms"] or not any(
        p["state"] == "extracting" and p["audio_seconds"] <= 5 for p in phases
    ):
        raise SystemExit(
            "Timing/enrollment check failed; do not treat this run as meeting the live budget."
        )


if __name__ == "__main__":
    main()
