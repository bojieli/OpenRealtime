#!/usr/bin/env python3
"""Summarize immutable live diagnostic traces without promoting them to pilot scores."""
import argparse
import json
import math
import statistics
from collections import Counter
import struct
import wave
from pathlib import Path



def acoustic_overlap(path, start_ns, end_ns, threshold=0.01):
    """Energy diagnostic only: no claim of speech intelligibility or content."""
    with wave.open(str(path)) as wav:
        if wav.getnchannels() != 2 or wav.getsampwidth() != 2:
            raise ValueError("expected stereo PCM16")
        rate = wav.getframerate()
        pcm = wav.readframes(wav.getnframes())
    start = start_ns * rate // 1_000_000_000
    end = min(end_ns * rate // 1_000_000_000, len(pcm) // 4)
    frame = max(1, rate // 50)
    both = checked = 0
    for at in range(start, end, frame):
        samples = list(struct.iter_unpack("<hh", pcm[at * 4:min(at + frame, end) * 4]))
        if not samples:
            continue
        rms = [math.sqrt(sum(s[c] ** 2 for s in samples) / len(samples)) / 32768
               for c in (0, 1)]
        checked += len(samples)
        if min(rms) > threshold:
            both += len(samples)
    return {"simultaneous_energy_seconds": both / rate,
            "checked_seconds": checked / rate, "threshold_rms": threshold,
            "frame_ms": 20, "speech_content_verified": False}



def latency_summary(actions):
    def stats(rows):
        values = sorted(r["decision"]["duration_ns"] / 1e6 for r in rows)
        if not values:
            return {"count": 0}
        return {"count": len(values), "median": statistics.median(values),
                "p95_nearest_rank": values[max(0, math.ceil(0.95 * len(values)) - 1)],
                "max": values[-1]}
    return {
        "all": stats(actions),
        "with_text": stats([r for r in actions if r["decision"]["action"].get("text")]),
        "without_text": stats([r for r in actions if not r["decision"]["action"].get("text")]),
        "interpretation": "observed request wall time; includes service and shared-host effects",
    }


def execution_diagnostics(actions, result, marks, feedback_available_ns):
    """Keep requested content separate from synthesis admission and completion."""
    segments = (result.get("ledger") or {}).get("segments") or []
    lengths = [len(s["text"].split()) for s in segments]
    post_feedback = [a for a in actions
                     if a["model_admission_ns"] >= feedback_available_ns]
    causes = Counter(m.get("reason", "unknown") for m in marks
                     if m.get("mark") == "cancel")
    known = {s["id"]: s["text"] for s in segments}
    identical_revisions = sum(
        a["decision"]["action"].get("act") == "revise"
        and a["execution_status"] == "synthesis-started"
        and a["decision"]["action"].get("text") == known.get(
            a["decision"]["action"].get("replaces_pending"))
        for a in actions
    )
    return {
        "feedback_available_ns": feedback_available_ns,
        "post_feedback_requested_acts": dict(Counter(
            a["decision"]["action"].get("act", "invalid") for a in post_feedback)),
        "post_feedback_execution_outcomes": dict(Counter(
            a["execution_status"] for a in post_feedback)),
        "cancellation_requests_by_reason": dict(causes),
        "executed_identical_text_revisions": identical_revisions,
        "synthesized_segment_word_counts": lengths,
        "synthesized_segment_words_median": statistics.median(lengths) if lengths else None,
        "synthesized_segment_words_max": max(lengths) if lengths else None,
        "interpretation": "descriptive execution evidence; no semantic success or word alignment claim",
    }


def normalize_text(text):
    """Mirror bench/capability normalizeText: lower-case letter/number runs."""
    words, current = [], []
    for ch in text.lower():
        if ch.isalpha() or ch.isnumeric():
            current.append(ch)
        elif current:
            words.append("".join(current))
            current = []
    if current:
        words.append("".join(current))
    return " ".join(words)


def lexical_match(expect, text):
    """Require/forbid/min-word rule of the Go screen, applied to arbitrary text."""
    padded = " " + normalize_text(text) + " "
    has = lambda term: " " + normalize_text(term) + " " in padded
    missing = [g for g in expect.get("require_any_of") or [] if not any(has(t) for t in g)]
    forbidden = [t for t in expect.get("forbid") or [] if has(t)]
    short = len(normalize_text(text).split()) < expect.get("min_words", 0)
    return not missing and not forbidden and not short


def played_in_window(segments, opens, closes):
    """Upper bound: full generated text of every segment with any playback in
    the window, regardless of generation time or completion."""
    return " ".join(s["text"] for s in segments
                    if s.get("first_played_at") is not None and s.get("last_played_at") is not None
                    and s["first_played_at"] < closes and s["last_played_at"] > opens)


def paired_discrimination(root, pair, branches):
    """A feedback pass counts only if its matched control cannot pass even the
    upper-bound screen over played-in-window text."""
    out = []
    # Directories end in the repeat index; a feedback branch pairs only with
    # the control of the same cell and repeat.
    groups = {}
    for b in branches:
        if b.get("pair_id") == pair["id"]:
            key = (b.get("cell_id"), b["directory"].rsplit("-", 1)[-1])
            groups.setdefault(key, {})[b.get("variant_id")] = b
    for (cell, repeat), by_variant in sorted(groups.items(), key=lambda kv: (str(kv[0][0]), kv[0][1])):
        out.extend(pair_rows(root, pair, by_variant, cell, repeat))
    return out


def pair_rows(root, pair, by_variant, cell, repeat):
    out = []
    for variant in pair["variants"]:
        expect = variant["expect"]
        if expect.get("silent"):
            continue
        feedback, control = by_variant.get(variant["id"]), by_variant.get(variant["id"] + "-nofeedback")
        if not feedback or not control or "lexical_screen" not in feedback or "lexical_screen" not in control:
            out.append({"variant_id": variant["id"], "cell_id": cell, "repeat": repeat, "verdict": "incomplete-pair"})
            continue
        anchor = next(e for e in pair["prefix"] + variant["events"] if e["id"] == expect["after"])
        opens = anchor["available_at"]
        closes = opens + expect["within"]
        texts = {}
        for name, branch in (("feedback", feedback), ("control", control)):
            result = json.loads((root / branch["directory"] / "result.json").read_text())
            texts[name] = played_in_window((result.get("ledger") or {}).get("segments") or [], opens, closes)
        feedback_pass = bool((feedback["lexical_screen"] or {}).get("passed"))
        control_upper = lexical_match(expect, texts["control"])
        if control_upper:
            verdict = "non-discriminating"
        elif feedback_pass:
            verdict = "feedback-only-pass"
        else:
            verdict = "no-feedback-pass"
        out.append({"variant_id": variant["id"], "cell_id": cell, "repeat": repeat, "window_ns": [opens, closes],
                    "feedback_completed_pass": feedback_pass,
                    "control_completed_pass": bool((control["lexical_screen"] or {}).get("passed")),
                    "feedback_upper_bound_pass": lexical_match(expect, texts["feedback"]),
                    "control_upper_bound_pass": control_upper,
                    "control_played_in_window_text": texts["control"],
                    "verdict": verdict,
                    "interpretation": "lexical diagnostic on synthesis text; not word alignment or semantic scoring"})
    return out


def summarize(root):
    branches = []
    fixtures = json.loads((root / "fixtures.json").read_text())
    pairs = {pair["id"]: pair for pair in fixtures}
    for path in sorted(root.glob("*/actions.jsonl")):
        actions = [json.loads(line) for line in path.read_text().splitlines()]
        if not actions:
            row = {"directory": path.parent.name, "status": "empty-trace", "requests": 0}
            result_path = path.parent / "result.json"
            if result_path.exists():
                result = json.loads(result_path.read_text())
                row.update(status=result.get("status", "empty-trace"),
                           trial_error=result.get("trial_error", ""),
                           session_id=result.get("session_id"))
            branches.append(row)
            continue
        identity = actions[0]
        pair = pairs[identity["pair_id"]]
        variant_id = identity["variant_id"]
        original_id = variant_id.removesuffix("-nofeedback")
        variant = next(v for v in pair["variants"] if v["id"] == original_id)
        feedback = [e for e in variant["events"] if e["group"] == variant["feedback"]]
        start = min(e["source_start"] for e in feedback)
        end = max(e["source_end"] for e in feedback)
        result_path = path.parent / "result.json"
        row = {
            "directory": path.parent.name,
            "pair_id": pair["id"],
            "variant_id": variant_id,
            "cell_id": identity["cell_id"],
            "requests": len(actions),
            "request_latency_ms": latency_summary(actions),

            "deadline_misses": sum(a["deadline_missed"] for a in actions),
            "execution_rejections": sum(a["execution_status"] in (
                "invalid-replacement", "pending-conflict", "control-reference-in-speech") for a in actions),
            "policy_errors": [a["decision"]["error"] for a in actions
                              if a["decision"].get("error")],
            "status": "complete" if result_path.exists() else "incomplete",
        }
        if result_path.exists():
            result = json.loads(result_path.read_text())
            row["status"] = result.get("status", "failed" if row["policy_errors"] else "complete")
            row["trial_error"] = result.get("trial_error", "")
            segments = result["ledger"].get("segments") or []
            # Segment-span overlap is only an upper bound: stalls and silence
            # inside a segment require sample-level acoustic verification.
            candidate_overlap = any(
                s.get("first_played_at") is not None
                and s.get("last_played_at") is not None
                and s["first_played_at"] < end
                and s["last_played_at"] > start
                for s in segments
            )
            row.update({
                "completed_segments": sum(s.get("completed_at") is not None for s in segments),
                "cut_segments": sum(s["cut"] for s in segments),
                "feedback_interval_ns": [start, end],
                "feedback_withheld": variant_id != original_id,
                "segment_span_overlaps_feedback": candidate_overlap,
                "audible_overlap_verified": False,
                "lexical_screen": result.get("lexical_playback_screen"),
                "speech_errors": result.get("speech_errors") or [],
            })
            anchor = next(e for e in pair["prefix"] + variant["events"]
                          if e["id"] == variant["expect"]["after"])
            marks_path = path.parent / "playback.json"
            marks = json.loads(marks_path.read_text()) if marks_path.exists() else []
            row["execution_diagnostics"] = execution_diagnostics(
                actions, result, marks or [], anchor["available_at"])
        stereo = path.parent / "conversation.stereo.wav"
        if stereo.exists() and variant_id == original_id:
            row["acoustic_overlap"] = acoustic_overlap(stereo, start, end)
        branches.append(row)
    paired = [row for pair in fixtures if any(b.get("pair_id") == pair["id"] for b in branches)
              for row in paired_discrimination(root, pair, branches)]
    return {
        "scope": "development diagnostic, not preregistered pilot",
        "run": str(root),
        "branches": branches,
        "paired_discrimination": paired,
        "total_requests": sum(b.get("requests", 0) for b in branches),
        "total_deadline_misses": sum(b.get("deadline_misses", 0) for b in branches),
        "incomplete_branches": sum(b["status"] != "complete" for b in branches),
        "capability_claim": None,
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run", type=Path)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    report = summarize(args.run)
    # Never replace an earlier analysis.
    with args.out.open("x") as output:
        json.dump(report, output, indent=2)
        output.write("\n")
    print(f"{report['total_requests']} requests; "
          f"{report['total_deadline_misses']} deadline misses; "
          f"{report['incomplete_branches']} incomplete branches")
