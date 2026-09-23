#!/usr/bin/env python3
"""Independently transcribe retained mono output; no synthesis text is sent."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import time
import urllib.parse
import urllib.request


def request(url, data=None):
    req = urllib.request.Request(url, data=data)
    with urllib.request.urlopen(req, timeout=60) as response:
        return json.load(response)


def transcribe(path, endpoint, out):
    if out.exists():
        raise FileExistsError(out)
    raw = path.read_bytes()
    # FFmpeg's bandlimited resampler converts to the service's float32/16kHz input.
    conversion = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", str(path), "-ac", "1", "-ar", "16000",
         "-f", "f32le", "-"], capture_output=True, check=True
    )
    pcm = conversion.stdout
    health = request(endpoint + "/health")
    session = request(endpoint + "/api/start", b"")["session_id"]
    query = urllib.parse.urlencode({"session_id": session})
    rows = []
    error = None
    final = None
    try:
        for offset in range(0, len(pcm), 2560 * 4):
            began = time.monotonic()
            answer = request(endpoint + "/api/chunk?" + query,
                             pcm[offset:offset + 2560 * 4])
            rows.append({"source_end_s": min(offset + 2560 * 4, len(pcm)) / 64000,
                         "response": answer, "wall_s": time.monotonic() - began})
    except Exception as exc:
        error = str(exc)
    finally:
        try:
            final = request(endpoint + "/api/finish?" + query, b"")
        except Exception as exc:
            error = f"{error or ''} finish: {exc}"
    report = {
        "input_wav": str(path), "input_sha256": hashlib.sha256(raw).hexdigest(),
        "health_before": health, "resampling": "ffmpeg mono f32le 16000 Hz",
        "chunks": rows, "final": final, "error": error,
        "execution_mode": "offline independent recognition; not interaction input",
        "word_alignment": "unavailable",
        "synthesis_text_supplied_to_recognizer": False,
    }
    with out.open("x") as handle:
        json.dump(report, handle, indent=2)
        handle.write("\n")
    if error:
        raise RuntimeError(error)
    print(final)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("wav", type=Path)
    parser.add_argument("--endpoint", default="http://127.0.0.1:9110")
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    transcribe(args.wav, args.endpoint.rstrip("/"), args.out)
