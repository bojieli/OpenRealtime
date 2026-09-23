#!/usr/bin/env python3
"""Audit and summarize offline native-grammar traces without audible scoring."""
import argparse
from collections import Counter
import json
from pathlib import Path
import statistics

from native_pair_probe import schedule


def summarize(root):
    report = json.loads((root / "report.json").read_text())
    pair = json.loads((root / "prepared-pair.json").read_text())
    branches, violations = [], []
    for trial in report["trials"]:
        name = trial["variant"]
        path = root / (name + ".jsonl")
        rows = [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []
        original = name.removesuffix("-nofeedback")
        variant = next(v for v in pair["variants"] if v["id"] == original)
        expected = list(schedule(pair, variant, name != original,
                                 words_per_tick=report.get("words_per_tick", 0)))
        if trial["status"] == "complete" and len(rows) != len(expected):
            violations.append(name + ": incomplete tick schedule")
        for index, row in enumerate(rows):
            if index >= len(expected):
                violations.append(name + ": extra tick")
                break
            at, fresh = expected[index]
            if row["virtual_admission_ns"] != at or row["fresh_events"] != fresh:
                violations.append(f"{name}:{index}: evidence differs from causal schedule")
            if row["input"] != " ".join(e["text"] for e in fresh):
                violations.append(f"{name}:{index}: input differs from admitted words")
            if index:
                prior = rows[index - 1]
                prefix = prior["history_before_tokens"]
                history = row["history_before_tokens"]
                generated = prior["generated_tokens"]
                if history[:len(prefix)] != prefix or (generated and history[-len(generated):] != generated):
                    violations.append(f"{name}:{index}: native history continuity mismatch")
        anchor = next(e["available_at"] for e in variant["events"]
                      if e["id"] == variant["expect"]["after"])
        durations = [r["compute_s"] for r in rows]
        controls = Counter(value for r in rows for kind, value in r["events"] if kind == "control")
        text = "".join(value for r in rows for kind, value in r["events"] if kind == "text")
        following = "".join(value for r in rows if r["virtual_admission_ns"] >= anchor
                            for kind, value in r["events"] if kind == "text")
        branches.append({"variant": name, "status": trial["status"], "requests": len(rows),
                         "controls": dict(controls), "generated_text": text,
                         "post_feedback_generated_text": following,
                         "compute_median_s": statistics.median(durations) if durations else None,
                         "compute_over_500ms": sum(d > .5 for d in durations),
                         "missing_turn_end": sum(not r["generated"].endswith("<|im_end|>") for r in rows)})
    return {"scope": report["scope"], "branches": branches, "violations": violations,
            "capability_claim": None,
            "limitations": ["generated text is not heard speech", "virtual clock is not real-time playback",
                            "history audit checks prefix and generated suffix, not tokenizer reconstruction",
                            "phrase-end admission differs from the training input distribution"]}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run", type=Path)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    result = summarize(args.run)
    with args.out.open("x") as handle:
        json.dump(result, handle, indent=2)
        handle.write("\n")
    print(json.dumps({"branches": len(result["branches"]), "violations": result["violations"]}))
