#!/usr/bin/env python3
"""Decompose room-selftest artifacts using one client monotonic clock.

This measures observable boundaries, not provider-internal compute time.
Missing or interrupted paths are retained and excluded from stage averages.
"""

import argparse
import json
from pathlib import Path
import statistics

STAGES = [
    "recognition_finalization_ms",
    "semantic_admission_ms",
    "admission_to_first_text_ms",
    "text_to_synthesis_start_ms",
    "synthesis_to_first_audio_ms",
    "first_audio_to_audible_ms",
]


def profile(root):
    result = json.loads((root / "result.json").read_text())
    events = json.loads((root / "events.json").read_text())
    rows = []
    for turn in result["turns"]:
        row = {"turn": turn["turn"], "total_ms": turn["response_latency_ms"]}
        rows.append(row)
        if turn["response_latency_ms"] is None:
            row["excluded"] = "no audible response"
            continue
        ended = turn["input_end_ms"]
        audible = ended + turn["response_latency_ms"]
        window = [x for x in events if ended <= x["at_ms"] <= audible + 1]

        def first(name, after, predicate=lambda e: True, run=None):
            return next(
                (
                    x
                    for x in window
                    if x["at_ms"] >= after
                    and (
                        x["event"].get("name") == name or x["event"].get("type") == name
                    )
                    and predicate(x["event"])
                    and (
                        run is None
                        or x["event"].get("attributes", {}).get("run_id") == run
                    )
                ),
                None,
            )

        admission = first(
            "semantic_admission_outcome",
            ended,
            lambda e: (
                (e.get("payload") or {}).get("value", {}).get("kind") == "admitted"
            ),
        )
        if admission is None:
            row["excluded"] = (
                "no post-input admission; speech may have started during input"
            )
            continue
        finals = [
            x
            for x in window
            if x["at_ms"] <= admission["at_ms"]
            and x["event"].get("type")
            == "conversation.item.input_audio_transcription.completed"
        ]
        text = first("prepared_text", admission["at_ms"])
        if not finals or text is None:
            row["excluded"] = "missing final transcript or prepared text boundary"
            continue
        run = text["event"].get("attributes", {}).get("run_id")
        synthesis = first(
            "tts_status",
            text["at_ms"],
            lambda e: e.get("correlation_id", "").endswith(":synthesis:generating"),
            run,
        )
        audio = first("gateway_speech_audio", text["at_ms"], run=run)
        if synthesis is None or audio is None:
            row["excluded"] = "missing synthesis or audio boundary for generation"
            continue
        if first("speech_cancel_request", admission["at_ms"]) is not None:
            row["excluded"] = "cancellation during response path"
            continue
        boundaries = [
            ended,
            finals[-1]["at_ms"],
            admission["at_ms"],
            text["at_ms"],
            synthesis["at_ms"],
            audio["at_ms"],
            audible,
        ]
        if any(b < a for a, b in zip(boundaries, boundaries[1:])):
            row["excluded"] = "overlapping or reordered boundaries"
            continue
        row["run_id"] = run
        row["boundaries_ms"] = dict(
            zip(
                [
                    "input_end",
                    "final_transcript",
                    "admitted",
                    "first_text",
                    "synthesis_start",
                    "first_audio",
                    "first_audible",
                ],
                boundaries,
            )
        )
        row.update(
            {key: b - a for key, a, b in zip(STAGES, boundaries, boundaries[1:])}
        )
    included = [r for r in rows if "excluded" not in r]
    summary = (
        {
            key: {
                "mean_ms": statistics.mean(r[key] for r in included),
                "median_ms": statistics.median(r[key] for r in included),
                "min_ms": min(r[key] for r in included),
                "max_ms": max(r[key] for r in included),
            }
            for key in STAGES + ["total_ms"]
        }
        if included
        else {}
    )
    return {
        "source": str(root),
        "attempted_turns": len(rows),
        "profiled_turns": len(included),
        "rows": rows,
        "summary": summary,
        "limitations": [
            "Client monotonic timestamps; includes server-to-client delivery and scheduling.",
            "Admission-to-text includes context/model HTTP, thinking, and text quarantine, not pure model compute.",
            "Synthesis-to-audio includes local speech synthesis and playback plumbing, not pure GPU compute.",
            "Audible onset uses room-selftest PCM RMS windows; input scheduling has 100 ms frame granularity.",
            "No physical-device/WebRTC latency or production percentile is inferred.",
        ],
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    report = profile(args.directory)
    if args.output:
        args.output.write_text(json.dumps(report, indent=2) + "\n")
    print(f"Profiled {report['profiled_turns']}/{report['attempted_turns']} turns")
    print("stage                                  mean ms   min ms   max ms")
    for key, stats in report["summary"].items():
        print(
            f"{key:38} {stats['mean_ms']:7.1f} {stats['min_ms']:8.1f} {stats['max_ms']:8.1f}"
        )
    for row in report["rows"]:
        if "excluded" in row:
            print(f"Excluded turn {row['turn']}: {row['excluded']}")
