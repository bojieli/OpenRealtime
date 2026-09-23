#!/usr/bin/env python3
"""Released micro-turn grammar on prepared pairs; offline, no audible score."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import time


def schedule(pair, variant, withheld=False, tick_ns=500_000_000, words_per_tick=0):
    events = pair["prefix"] + [e for e in variant["events"]
                               if not withheld or e["group"] != variant["feedback"]]
    words = sorted((e for e in events if e["kind"] == "word"),
                   key=lambda e: (e["available_at"], e["source_start"]))
    if any(e.get("provisional") or e.get("replaces") for e in words):
        raise ValueError("native probe supports committed append-only annotations")
    end = max(max(e["available_at"] for e in pair["prefix"] + v["events"])
              + v["expect"]["within"] + 5_000_000_000 for v in pair["variants"])
    if words_per_tick:
        # Conservative delivery intervention: delay known words, never release
        # estimated word boundaries early. Same extended horizon for controls.
        max_words = max(sum(e["kind"] == "word" for e in pair["prefix"] + v["events"])
                        for v in pair["variants"])
        end += ((max_words + words_per_tick - 1) // words_per_tick) * tick_ns
    first = words[0]["available_at"]
    # Match the upstream first-word-triggered start, with the first decision
    # one tick later. Offline compute never advances this virtual clock.
    cursor = 0
    at = first + tick_ns
    while at <= end:
        fresh = []
        while (cursor < len(words) and words[cursor]["available_at"] <= at
               and (not words_per_tick or len(fresh) < words_per_tick)):
            fresh.append(words[cursor])
            cursor += 1
        yield at, fresh
        at += tick_ns


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("source", "snapshot", "base", "prepared_pair", "out"):
        parser.add_argument("--" + name.replace("_", "-"), type=Path, required=True)
    parser.add_argument("--words-per-tick", type=int, default=0,
                        help="delay already-available words into bounded chunks; zero admits all")
    args = parser.parse_args()
    if args.words_per_tick < 0:
        parser.error("words-per-tick must be nonnegative")
    args.out.mkdir()  # Never overwrite a run.
    root = Path(__file__).resolve().parents[2]
    retained = args.out / "source"
    retained.mkdir()
    hashes = {}
    for path in [Path(__file__), root / "sidecars/duplexcascade_model.py", args.source / "model.py"]:
        data = path.read_bytes()
        hashes[str(path)] = hashlib.sha256(data).hexdigest()
        (retained / (path.parent.name + "__" + path.name + ".txt")).write_bytes(data)
    shutil.copyfile(args.prepared_pair, args.out / "prepared-pair.json")
    shutil.copyfile(args.prepared_pair.with_name("recordings.json"), args.out / "recordings.json")
    pair = json.loads(args.prepared_pair.read_text())
    report = {
        "scope": "offline annotated native-grammar development probe; no synthesis or playback",
        "cell": "DC-native-diagnostic", "pair_id": pair["id"],
        "timing": "virtual 500 ms clock, first recognized words plus one tick",
        "input": "newly available committed words; empty chunk maps to released no-voice token",
        "limitations": ["no-voice can include ASR unavailability", "English transfer unvalidated",
                        "no sound/cue/self-playback channels", "not matched A2", "no real-time deadline claim"],
        "source_sha256": hashes,
        "invocation": {k: str(v) for k, v in vars(args).items()},
        "words_per_tick": args.words_per_tick,
        "delivery_intervention": "delay-only bounded word chunks" if args.words_per_tick else "all available words",
        "gpu_before": subprocess.check_output(["nvidia-smi", "--query-gpu=memory.used,memory.free,utilization.gpu", "--format=csv"], text=True),
        "trials": [], "capability_claim": None,
    }
    def save():
        (args.out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    save()
    sys.path.insert(0, str(root / "sidecars"))
    from duplexcascade_model import DuplexCascadeModel
    try:
        backend = DuplexCascadeModel(args.source, args.snapshot, args.base, slow_tokenizer=True)
        report["model"] = backend.metadata
        save()
        for withheld in (False, True):
            for variant in pair["variants"]:
                name = variant["id"] + ("-nofeedback" if withheld else "")
                trial = {"variant": name, "status": "running", "requests": 0}
                report["trials"].append(trial)
                save()
                session = backend.session()
                try:
                    with (args.out / (name + ".jsonl")).open("x") as trace:
                        for at, fresh in schedule(pair, variant, withheld, words_per_tick=args.words_per_tick):
                            chunk = " ".join(e["text"] for e in fresh)
                            history_before = list(session.history)
                            backend.torch.cuda.synchronize()
                            began = time.monotonic()
                            generated = session.step(chunk, max_new_tokens=64)
                            backend.torch.cuda.synchronize()
                            duration = time.monotonic() - began
                            row = {"virtual_admission_ns": at, "fresh_events": fresh,
                                   "input": chunk, "history_before_tokens": history_before,
                                   "generated_tokens": generated,
                                   "generated": backend.tokenizer.decode(generated),
                                   "events": list(session.events(generated)), "compute_s": duration}
                            trace.write(json.dumps(row) + "\n")
                            trace.flush()
                            trial["requests"] += 1
                    trial["status"] = "complete"
                except Exception as exc:
                    trial.update(status="failed", error=repr(exc))
                    raise
                finally:
                    save()
                print(json.dumps(trial), flush=True)
        report["peak_allocated_gib"] = backend.torch.cuda.max_memory_allocated() / 2**30
    except Exception as exc:
        report["error"] = repr(exc)
        raise
    finally:
        save()


if __name__ == "__main__":
    main()
