#!/usr/bin/env python3
"""Finalize FD-Bench outputs with the upstream Silero-VAD contract.

The Go runner records transport timing and aligned audio without claiming that
the repository's deterministic energy VAD is paper-equivalent. This explicit
phase applies the same Silero settings as FD-Bench's released Moshi client and
writes the five-field trace consumed by the upstream evaluator.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import tempfile
from typing import Any
import wave


BENCHMARK_REVISION = "8a4b7df1b4dcb0fc50a7a5660247a2ffe8d394eb"
DATASET_REVISION = "995e7178445b90d57dee8fab500c6e37a2f2c49e"
TIMESTAMP_RATE_HZ = 16_000


def arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--output-root",
        default=".runtime/benchmark-runs/fd-bench/openrealtime-v1/output",
        help="fdbench run output root",
    )
    parser.add_argument(
        "--provider-label", default="openrealtime", help="trace filename label"
    )
    return parser.parse_args()


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def atomic_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(prefix=".fdbench-", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as target:
            json.dump(value, target, indent=2, ensure_ascii=False)
            target.write("\n")
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def atomic_text(path: Path, value: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(prefix=".fdbench-trace-", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as target:
            target.write(value)
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def conversation_number(result: dict[str, Any]) -> int:
    return int(result["sample"]["conversation"])


def read_pcm16(path: Path, sampling_rate: int) -> Any:
    """Decode OpenRealtime's guaranteed PCM16 WAV without a codec runtime."""
    import torch
    import torchaudio

    with wave.open(str(path), "rb") as source:
        channels = source.getnchannels()
        source_rate = source.getframerate()
        sample_width = source.getsampwidth()
        frames = source.readframes(source.getnframes())
    if channels < 1 or source_rate < 1 or sample_width != 2:
        raise ValueError(f"{path} is not a PCM16 WAV")
    audio = torch.frombuffer(bytearray(frames), dtype=torch.int16).clone().to(torch.float32)
    audio = audio.reshape(-1, channels).mean(dim=1) / 32768.0
    if source_rate != sampling_rate:
        audio = torchaudio.transforms.Resample(source_rate, sampling_rate)(audio)
    return audio


def validate_result(result_path: Path, result: dict[str, Any]) -> Path:
    if (
        result.get("schema_version") != "1.0.0"
        or result.get("benchmark") != "FD-Bench"
        or result.get("revision") != BENCHMARK_REVISION
        or result.get("dataset_revision") != DATASET_REVISION
        or result.get("status") != "completed"
    ):
        raise ValueError(f"{result_path} is not a completed pinned FD-Bench result")
    output_path = Path(result["output_wav"])
    if not output_path.is_absolute():
        output_path = Path.cwd() / output_path
    if not output_path.is_file() or sha256(output_path) != result.get("output_sha256"):
        raise ValueError(f"{result_path} output audio identity does not match")
    return output_path


def main() -> None:
    args = arguments()
    try:
        from silero_vad import get_speech_timestamps, load_silero_vad
    except ImportError as error:
        raise SystemExit(
            "silero-vad is required; install the pinned FD-Bench evaluation environment"
        ) from error

    try:
        package_version = importlib.metadata.version("silero-vad")
    except importlib.metadata.PackageNotFoundError:
        package_version = "unknown"
    model = load_silero_vad()
    output_root = Path(args.output_root)
    result_paths = sorted(output_root.glob("*/results/conversation_*.json"))
    if not result_paths:
        raise SystemExit(f"no FD-Bench results found below {output_root}")

    by_cell: dict[str, list[dict[str, Any]]] = {}
    for result_path in result_paths:
        result = json.loads(result_path.read_text(encoding="utf-8"))
        output_path = validate_result(result_path, result)
        audio = read_pcm16(output_path, TIMESTAMP_RATE_HZ)
        segments = get_speech_timestamps(
            audio,
            model,
            sampling_rate=TIMESTAMP_RATE_HZ,
            return_seconds=False,
            threshold=0.5,
            min_silence_duration_ms=1500,
        )
        result["output_segments"] = [
            {"start": int(segment["start"]), "end": int(segment["end"])}
            for segment in segments
        ]
        result["output_vad"] = {
            "status": "completed",
            "name": "silero-vad",
            "package_version": package_version,
            "threshold": 0.5,
            "min_silence_duration_ms": 1500,
            "timestamp_rate_hz": TIMESTAMP_RATE_HZ,
            "audio_loader": "pcm16-wave+torchaudio.transforms.Resample",
        }
        atomic_json(result_path, result)
        by_cell.setdefault(result["sample"]["cell"], []).append(result)

    traces: dict[str, dict[str, Any]] = {}
    for cell, results in sorted(by_cell.items()):
        results.sort(key=conversation_number)
        trace_path = output_root / cell / f"{args.provider_label}.txt"
        lines = []
        for result in results:
            name = f"conversation_{conversation_number(result)}.wav"
            input_segments = json.dumps(result["sample"]["input_segments"], separators=(",", ":"))
            output_segments = json.dumps(result["output_segments"], separators=(",", ":"))
            lines.append(f"{name} || [] || [] || {input_segments} || {output_segments}\n")
        atomic_text(trace_path, "".join(lines))
        traces[cell] = {
            "path": str(trace_path),
            "samples": len(results),
            "sha256": sha256(trace_path),
        }
    print(
        json.dumps(
            {
                "benchmark": "FD-Bench",
                "revision": BENCHMARK_REVISION,
                "vad": {
                    "name": "silero-vad",
                    "package_version": package_version,
                    "threshold": 0.5,
                    "min_silence_duration_ms": 1500,
                    "timestamp_rate_hz": TIMESTAMP_RATE_HZ,
                    "audio_loader": "pcm16-wave+torchaudio.transforms.Resample",
                },
                "results": len(result_paths),
                "traces": traces,
            },
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
