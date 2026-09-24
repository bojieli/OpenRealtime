#!/usr/bin/env python3
"""Build the 600 s long-session input used by the endurance probes.

Concatenates FDB v1.5 ``user_interruption/<n>/input.wav`` recordings in
numeric order (each keeps its own silences: a question, a pause, an
interruption) until the target duration, then pads to it exactly. 16 kHz mono
PCM16. The original 2026-09-22 file was lost with .runtime/duplex-plan/build
and its recipe was not recorded; runs using this file are compared with it on
memory growth, not on answers.

    python3 tools/duplexmodels/long_input.py --out .runtime/duplex-plan/build/long-input-600s.wav
"""
import argparse
import hashlib
from pathlib import Path

import numpy as np
import soundfile as sf

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / ".runtime/full-duplex-bench-v1.5/dataset/user_interruption"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True)
    parser.add_argument("--seconds", type=float, default=600.0)
    args = parser.parse_args()
    rate, target = 16_000, int(args.seconds * 16_000)
    parts, total, used = [], 0, []
    for folder in sorted((p for p in SOURCE.iterdir() if p.name.isdigit()), key=lambda p: int(p.name)):
        audio, source_rate = sf.read(folder / "input.wav", dtype="int16")
        if source_rate != rate or audio.ndim != 1:
            continue
        parts.append(audio[: target - total])
        total += len(parts[-1])
        used.append(folder.name)
        if total >= target:
            break
    audio = np.concatenate(parts)
    audio = np.pad(audio, (0, target - len(audio)))
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    sf.write(args.out, audio, rate, subtype="PCM_16")
    digest = hashlib.sha256(Path(args.out).read_bytes()).hexdigest()
    print(f"{args.out}: {len(audio) / rate:.1f} s from recordings {used[0]}-{used[-1]} ({len(used)}); sha256 {digest}")


if __name__ == "__main__":
    main()
