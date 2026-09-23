#!/usr/bin/env python3
"""Retain exact original response-window audio; no latency-based window shifts."""
import argparse
import hashlib
import json
from pathlib import Path
import wave


def crop(pcm, rate, start_ns, end_ns):
    if rate <= 0 or start_ns < 0 or end_ns <= start_ns or len(pcm) % 2:
        raise ValueError("invalid PCM or scoring interval")
    first = start_ns * rate // 1_000_000_000
    last = end_ns * rate // 1_000_000_000
    data = pcm[first * 2:last * 2]
    return data + bytes((last - first) * 2 - len(data))


def package(root, out):
    pair = json.loads((root / "prepared-pair.json").read_text())
    out.mkdir()
    manifest = []
    for variant in pair["variants"]:
        start = next(e["available_at"] for e in variant["events"]
                     if e["id"] == variant["expect"]["after"])
        end = start + variant["expect"]["within"]
        for suffix in ("", "-nofeedback"):
            name = variant["id"] + suffix
            path = root / name / "output.wav"
            with wave.open(str(path)) as wav:
                if wav.getnchannels() != 1 or wav.getsampwidth() != 2:
                    raise ValueError("expected mono PCM16")
                rate = wav.getframerate()
                pcm = wav.readframes(wav.getnframes())
            clipped = crop(pcm, rate, start, end)
            with wave.open(str(out / (name + ".wav")), "wb") as wav:
                wav.setparams((1, 2, rate, 0, "NONE", "not compressed"))
                wav.writeframes(clipped)
            manifest.append({"variant": name, "start_ns": start, "end_ns": end,
                             "source_sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                             "crop_pcm_sha256": hashlib.sha256(clipped).hexdigest(),
                             "purpose": "original fixture window; no extension for delivery delay"})
    (out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")


def package_go(root, out):
    """Resolve identity from trace records, never by substring filename guesses."""
    pairs = json.loads((root / "fixtures.json").read_text())
    paths = sorted(root.glob("*/actions.jsonl"))
    if not paths:
        raise ValueError("no Go trial traces")
    out.mkdir()
    manifest = []
    for path in paths:
        with path.open() as trace:
            first = trace.readline()
        if not first:
            raise ValueError("trial has no identity-bearing request: " + str(path))
        identity = json.loads(first)
        pair = next(p for p in pairs if p["id"] == identity["pair_id"])
        original = identity["variant_id"].removesuffix("-nofeedback")
        variant = next(v for v in pair["variants"] if v["id"] == original)
        events = pair["prefix"] + variant["events"]
        start = next(e["available_at"] for e in events if e["id"] == variant["expect"]["after"])
        end = start + variant["expect"]["within"]
        source = path.with_name("output.wav")
        result_path = path.with_name("result.json")
        result = json.loads(result_path.read_text())
        if source.exists():
            with wave.open(str(source)) as wav:
                if wav.getnchannels() != 1 or wav.getsampwidth() != 2:
                    raise ValueError("expected mono PCM16")
                rate = wav.getframerate()
                pcm = wav.readframes(wav.getnframes())
            source_sha256 = hashlib.sha256(source.read_bytes()).hexdigest()
        else:
            # A branch that never spoke has no recording. Silence is inferred
            # only when the ledger confirms that nothing played; otherwise a
            # missing file is lost evidence and stays an error.
            played = sum(s.get("played_samples", 0) for s in (result.get("ledger") or {}).get("segments") or [])
            if played:
                raise ValueError(f"{source}: missing although the ledger records {played} played samples")
            with wave.open(str(path.with_name("input.wav"))) as wav:
                rate = wav.getframerate()
            pcm, source_sha256 = b"", "none: ledger records no played audio"
        data = crop(pcm, rate, start, end)
        name = path.parent.name + ".wav"
        with wave.open(str(out / name), "wb") as wav:
            wav.setparams((1, 2, rate, 0, "NONE", "not compressed"))
            wav.writeframes(data)
        manifest.append({"file": name, "pair_id": pair["id"],
                         "variant_id": identity["variant_id"], "cell_id": identity["cell_id"],
                         "status": result["status"], "start_ns": start, "end_ns": end,
                         "source_sha256": source_sha256,
                         "trace_sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                         "result_sha256": hashlib.sha256(result_path.read_bytes()).hexdigest(),
                         "crop_pcm_sha256": hashlib.sha256(data).hexdigest(),
                         "purpose": "original fixture window; no extension or capability score"})
    (out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("run", type=Path)
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--layout", choices=["native", "go"], default="native")
    args = p.parse_args()
    (package_go if args.layout == "go" else package)(args.run, args.out)
