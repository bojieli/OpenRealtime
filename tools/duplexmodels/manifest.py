#!/usr/bin/env python3
"""Pin the model assets of the duplex-plan deployment profiles.

For every Hugging Face repository the profiles use, record the exact
revision present in the local cache, its size on disk, the license the model
card declares, and whether the Hub gates it. Moving model cards are not
reproducibility records; this file is.

    python tools/duplexmodels/manifest.py --out deploy/duplex/manifest.json

A repository missing from the cache is recorded as such rather than skipped,
so a manifest never silently omits an asset a profile needs.
"""

from __future__ import annotations

import argparse
import json
import os
import re
from datetime import datetime, timezone
from pathlib import Path

HUB = Path(os.environ.get("HF_HUB_CACHE", Path.home() / ".cache/huggingface/hub"))

# role -> repositories. The role is the plan's, not the vendor's.
ASSETS = {
    "llm": ["Qwen/Qwen3-8B", "Qwen/Qwen2-7B-Instruct"],
    "streaming-asr": ["mistralai/Voxtral-Mini-4B-Realtime-2602", "Qwen/Qwen3-ASR-0.6B",
                      "nvidia/nemotron-speech-streaming-en-0.6b", "nvidia/nemotron-3.5-asr-streaming-0.6b",
                      "kyutai/stt-1b-en_fr"],
    "incremental-tts": ["kyutai/tts-1.6b-en_fr", "kyutai/tts-voices", "FunAudioLLM/Fun-CosyVoice3-0.5B-2512",
                        "microsoft/VibeVoice-Realtime-0.5B", "Qwen/Qwen3-TTS-12Hz-0.6B-Base",
                        "Qwen/Qwen3-TTS-Tokenizer-12Hz"],
    "sentence-tts": ["fishaudio/s2-pro"],
    "native-duplex": ["kyutai/moshiko-pytorch-bf16", "nvidia/NVIDIA-NemotronLabs-VoiceChat-11B",
                      "openbmb/MiniCPM-o-4_5", "HIT-TMG/Lychee-FD", "stepfun-ai/Step-Audio-2-mini",
                      "VITA-MLLM/Freeze-Omni", "BayLing-Models/BayLing-Duplex", "nvidia/personaplex-7b-v1"],
    "interaction": ["pipecat-ai/smart-turn-v3", "livekit/turn-detector", "anyreach-ai/dualturn-qwen2.5-mimi-0.5B"],
    "speaker": ["nvidia/diar_streaming_sortformer_4spk-v2.1", "nvidia/multitalker-parakeet-streaming-0.6b-v1"],
    "observer": ["nvidia/audio-flamingo-3", "nvidia/audio-flamingo-3-hf"],
    "task": ["kyutai/hibiki-2b-pytorch-bf16", "facebook/seamless-streaming", "tsinghua-ee/ELLSA"],
    "microturn-llm": ["sbintuitions/DuplexCascade"],
}


LOCAL_DOWNLOADS = {
    "fishaudio/s2-pro": Path(__file__).resolve().parents[2] / ".runtime/fish-speech/checkpoints/s2-pro",
}


def local(repository: str) -> dict:
    directory = LOCAL_DOWNLOADS.get(repository)
    if directory is not None and directory.is_dir():
        metadata = directory / ".cache/huggingface/download"
        revisions = {p.read_text().splitlines()[0] for p in metadata.rglob("*.metadata")}
        files = [p for p in directory.rglob("*") if p.is_file() and ".cache" not in p.relative_to(directory).parts]
        readme = (directory / "README.md").read_text(errors="ignore")
        license_name = re.search(r"^license_name:\s*(\S+)", readme, re.MULTILINE)
        return {"present": True, "local_directory": str(directory),
                "revision": next(iter(revisions)) if len(revisions) == 1 else None,
                "revision_source": "hf local-directory download metadata",
                "bytes": sum(p.stat().st_size for p in files),
                "license": license_name.group(1) if license_name else None,
                "license_file": "LICENSE.md"}
    root = HUB / ("models--" + repository.replace("/", "--"))
    ref = root / "refs/main"
    if not ref.exists():
        return {"present": False}
    revision = ref.read_text().strip()
    snapshot = root / "snapshots" / revision
    size = 0
    for path in snapshot.rglob("*"):
        if path.is_file():
            size += path.stat().st_size  # follows the blob symlink
    license_name = None
    readme = snapshot / "README.md"
    if readme.exists():
        match = re.search(r"^license:\s*(\S+)", readme.read_text(errors="ignore"), re.MULTILINE)
        license_name = match.group(1) if match else None
    return {"present": True, "revision": revision, "bytes": size, "license": license_name}


def hub(repository: str) -> dict:
    try:
        from huggingface_hub import HfApi  # noqa: PLC0415

        info = HfApi().model_info(repository)
        return {"hub_revision": info.sha, "gated": info.gated or False}
    except Exception as failure:  # noqa: BLE001
        return {"hub_error": f"{type(failure).__name__}: {str(failure)[:160]}"}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--out", required=True)
    parser.add_argument("--offline", action="store_true", help="do not ask the Hub for gating and head revision")
    args = parser.parse_args()
    assets = []
    for role, repositories in ASSETS.items():
        for repository in repositories:
            entry = {"role": role, "repository": repository, **local(repository)}
            if not args.offline:
                entry.update(hub(repository))
            assets.append(entry)
    Path(args.out).write_text(json.dumps({
        "generated": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "cache": str(HUB), "assets": assets,
    }, indent=2) + "\n")
    missing = [asset["repository"] for asset in assets if not asset["present"]]
    print(f"{len(assets)} assets, {len(missing)} not in the cache: {', '.join(missing) or 'none'}")


if __name__ == "__main__":
    main()
