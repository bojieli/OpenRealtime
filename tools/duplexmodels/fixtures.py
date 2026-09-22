#!/usr/bin/env python3
"""Prepare replay fixtures for the duplex model component benchmarks.

Writes raw PCM16 mono 24 kHz files plus JSONL manifests that tools/asrbench
and the other component benchmarks replay at wall-clock speed:

- ``asr-en``: a seeded sample of LibriSpeech test-clean utterances (3-15 s).
- ``asr-zh``: a seeded sample of FLEURS cmn_hans_cn test utterances.

Every utterance gets 300 ms of leading silence, the pre-roll the cascade's
energy gate hands a recogniser, so first-hypothesis times are comparable to a
live session. Reference text is kept verbatim; scoring normalises it.

    python tools/duplexmodels/fixtures.py --data .runtime/duplex-plan/data \
        --out .runtime/duplex-plan/fixtures --count 40
"""

from __future__ import annotations

import argparse
import io
import json
import random
from pathlib import Path

import numpy as np
import pyarrow.parquet as pq
import soundfile as sf
from scipy.signal import resample_poly

RATE = 24_000
PREROLL = int(0.3 * RATE)


def to_pcm(samples: np.ndarray, rate: int) -> bytes:
    if samples.ndim > 1:
        samples = samples.mean(axis=1)
    if rate != RATE:
        from math import gcd
        g = gcd(rate, RATE)
        samples = resample_poly(samples, RATE // g, rate // g)
    samples = np.concatenate([np.zeros(PREROLL), samples])
    return (np.clip(samples, -1, 1) * 32767).astype("<i2").tobytes()


def decode(cell) -> tuple[np.ndarray, int]:
    data = cell["bytes"] if isinstance(cell, dict) else cell
    samples, rate = sf.read(io.BytesIO(data), dtype="float32")
    return samples, rate


def build(rows, language, out: Path, count: int, seed: int, text_key: str, id_key: str) -> int:
    out.mkdir(parents=True, exist_ok=True)
    random.Random(seed).shuffle(rows)
    manifest = out.parent / f"{out.name}.jsonl"
    written = 0
    with manifest.open("w") as handle:
        for row in rows:
            samples, rate = decode(row["audio"])
            seconds = len(samples) / rate
            if not 3.0 <= seconds <= 15.0:
                continue
            identifier = str(row[id_key])
            path = out / f"{identifier}.pcm"
            path.write_bytes(to_pcm(samples, rate))
            handle.write(json.dumps({"id": identifier, "pcm": str(path.resolve()),
                                     "text": row[text_key].strip(), "language": language},
                                    ensure_ascii=False) + "\n")
            written += 1
            if written == count:
                break
    return written


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--data", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--count", type=int, default=40)
    parser.add_argument("--seed", type=int, default=20260922)
    args = parser.parse_args()
    libri = pq.read_table(args.data / "libri/clean/test/0000.parquet").to_pylist()
    print("asr-en", build(libri, "en", args.out / "asr-en", args.count, args.seed, "text", "id"))
    fleurs = pq.read_table(args.data / "fleurs/parquet-data/cmn_hans_cn/test-00000-of-00001.parquet").to_pylist()
    print("asr-zh", build(fleurs, "zh", args.out / "asr-zh", args.count, args.seed, "raw_transcription", "id"))


if __name__ == "__main__":
    main()
