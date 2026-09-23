#!/usr/bin/env python3
"""Create a randomized listening package; labels remain in a separate key."""
import argparse
import hashlib
import json
from pathlib import Path
import random
import shutil


def build(run, out, key, seed):
    recordings = sorted(run.glob("*/conversation.stereo.wav"))
    if not recordings:
        raise ValueError("no complete stereo recordings")
    if out.exists() or key.exists():
        raise FileExistsError("package and key must be new paths")
    for wav in recordings:
        if not (wav.parent / "result.json").exists():
            raise ValueError(f"unfinished branch: {wav.parent}")
    random.Random(seed).shuffle(recordings)
    out.mkdir()
    mapping = []
    for index, source in enumerate(recordings, 1):
        name = f"conversation-{index:03d}.wav"
        shutil.copyfile(source, out / name)
        mapping.append({
            "file": name, "source_branch": source.parent.name,
            "sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
        })
    (out / "instructions.md").write_text(
        "# Listening review\n\n"
        "Listen to each entire conversation in the supplied order. "
        "The user is on the left and assistant on the right.\n\n"
        "Rate intelligibility, pleasantness, conversational naturalness, and "
        "responsiveness separately from 1 (poor) to 5 (excellent). "
        "Describe whether the assistant fulfilled the latest request, including "
        "any audible error or interruption. Do not reward hesitations by themselves. "
        "Mark uncertainty rather than guessing unheard words.\n\n"
        "These are synthetic development recordings. Ratings have not yet been collected.\n"
    )
    (out / "ratings.csv").write_text(
        "file,rater_id,intelligibility,pleasantness,naturalness,responsiveness,"
        "request_fulfilled,uncertain,notes\n"
        + "".join(f"{row['file']},,,,,,,,\n" for row in mapping)
    )
    key.write_text(json.dumps({
        "seed": seed, "source_run": str(run), "order": mapping,
        "ratings_collected": False,
    }, indent=2) + "\n")
    return len(mapping)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run", type=Path)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--key", type=Path, required=True)
    parser.add_argument("--seed", type=int, default=1729)
    args = parser.parse_args()
    print(build(args.run, args.out, args.key, args.seed))
