#!/usr/bin/env python3
"""Audit real admission against delayed schedule and report native run outcomes."""
import argparse
from collections import Counter
import json
from pathlib import Path
import statistics

from native_pair_probe import schedule


def summarize(root):
    pair = json.loads((root / "prepared-pair.json").read_text())
    manifest_path = root / "manifest.json"
    manifest = json.loads(manifest_path.read_text()) if manifest_path.exists() else {}
    words_per_tick = manifest.get("words_per_tick", 2)
    if type(words_per_tick) is not int or words_per_tick < 1:
        raise ValueError("invalid words_per_tick in manifest")
    expected_names = {v["id"] + suffix for v in pair["variants"]
                      for suffix in ("", "-nofeedback")}
    present_names = {p.parent.name for p in root.glob("*/result.json")}
    campaign_violations = []
    for name in sorted(expected_names - present_names):
        campaign_violations.append("missing terminal branch: " + name)
    for name in sorted(present_names - expected_names):
        campaign_violations.append("unexpected terminal branch: " + name)
    branches = []
    for result_path in sorted(root.glob("*/result.json")):
        result = json.loads(result_path.read_text())
        name = result_path.parent.name
        if name not in expected_names:
            continue
        original = name.removesuffix("-nofeedback")
        variant = next(v for v in pair["variants"] if v["id"] == original)
        expected = list(schedule(pair, variant, name != original, words_per_tick=words_per_tick))
        rows = [json.loads(line) for line in result_path.with_name("actions.jsonl").read_text().splitlines()]
        cursor, previous = 0, -1
        violations, controls = [], Counter()
        if result.get("words_per_tick", 2) != words_per_tick:
            violations.append("terminal words_per_tick differs from manifest")
        feedback_complete = None
        identity_checked = 0
        explicit_identity = result.get("trace_schema_version") == 3 or any(r.get("trace_schema_version") == 3 for r in rows)
        if explicit_identity:
            terminal_identity = {"trace_schema_version": 3, "session_id": root.name + "/" + name,
                                 "pair_id": pair["id"], "variant_id": name, "cell_id": "DC-native-diagnostic"}
            if any(result.get(k) != v for k, v in terminal_identity.items()):
                violations.append("missing or changed native v3 terminal identity")
        for index, row in enumerate(rows):
            fresh = []
            if explicit_identity:
                identity_checked += 1
                expected_identity = {"trace_schema_version": 3,
                    "session_id": root.name + "/" + name, "turn_id": "decision-" + str(index + 1),
                    "pair_id": pair["id"], "variant_id": name, "cell_id": "DC-native-diagnostic"}
                if any(row.get(k) != v for k, v in expected_identity.items()):
                    violations.append(f"{index}: missing or changed native v3 identity")
            at = row["admission_s"]
            while cursor < len(expected) and expected[cursor][0] / 1e9 <= at:
                fresh.extend(expected[cursor][1])
                cursor += 1
            if fresh != row["fresh_events"] or row["input"] != " ".join(e["text"] for e in fresh):
                violations.append(f"{index}: admission differs from causal delayed schedule")
            if at <= previous:
                violations.append(f"{index}: nonmonotonic request admission")
            previous = at
            if any(e["id"] == variant["expect"]["after"] for e in fresh):
                feedback_complete = at
            controls.update(value for kind, value in row["events"] if kind == "control")
        if result["status"] == "complete" and cursor != len(expected):
            violations.append("complete result does not cover trial horizon")
        anchor = next(e["available_at"] / 1e9 for e in variant["events"]
                      if e["id"] == variant["expect"]["after"])
        branches.append({"variant": name, "status": result["status"], "requests": len(rows),
                         "deadline_misses": sum(r["deadline_missed"] for r in rows),
                         "duration_median_s": statistics.median(r["duration_s"] for r in rows) if rows else None,
                         "source_feedback_available_s": anchor,
                         "feedback_fully_admitted_s": feedback_complete,
                         "original_scoring_window_end_s": anchor + variant["expect"]["within"] / 1e9,
                         "controls": dict(controls), "admission_violations": violations,
                         "generated_text": "".join(t["text"] for t in result["text"]),
                         "identity_rows_checked": identity_checked,
                         "identity_scope": "explicit v3" if explicit_identity else "legacy directory identity only",
                         "synthesis_contexts": len(result["contexts"])})
    for branch in branches:
        if branch["status"] != "complete":
            campaign_violations.append("incomplete terminal branch: " + branch["variant"])
    return {"scope": "native wall-clock development; admission audit and execution statistics",
            "expected_branches": sorted(expected_names),
            "campaign_violations": campaign_violations,
            "execution_complete": not campaign_violations,
            "words_per_tick": words_per_tick,
            "branches": branches, "capability_claim": None,
            "limitations": ["generated text is not an audible transcript", "no word alignment",
                            "delayed input delivery", "different model and grammar from A1-A3"]}


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("run", type=Path)
    p.add_argument("--out", type=Path, required=True)
    args = p.parse_args()
    result = summarize(args.run)
    with args.out.open("x") as output:
        json.dump(result, output, indent=2)
        output.write("\n")
    for branch in result["branches"]:
        print(branch["variant"], branch["requests"], branch["deadline_misses"], branch["admission_violations"])
