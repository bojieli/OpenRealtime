#!/usr/bin/env python3
"""Run FD-Bench's released timing decision core and aggregate its metrics.

The upstream entry point unconditionally reads a Moshi-only WER file and a
precomputed Llama-3 CPPL file even for its Freeze-Omni/VITA paths. This wrapper
does not invent those artifacts. It invokes the pinned upstream parser and
`analyze_VAD_interruption_new2` implementation, then faithfully aggregates the
timing/interaction fields that implementation produced. WER, CPPL, and GPT
subjective scores remain explicitly unevaluated.

Two absences are reported separately and must not be confused. `not_evaluated`
names metrics this wrapper never computes because upstream does not produce the
inputs. `not_measured` names metrics that were computed but landed on an empty
population — no interruptions occurred, no response-delay samples were emitted —
and are therefore undefined. Neither is ever published as a zero: a rate over no
trials and a median over no samples are missing measurements, and for the latency
metrics a fabricated zero would read as the best score attainable.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
from typing import Any, Iterable


BENCHMARK_REVISION = "8a4b7df1b4dcb0fc50a7a5660247a2ffe8d394eb"
BENCHMARKING_SHA256 = "86a515a2c86fcd9ae89da9c96b2a4dd461b5d41a68dc752f5d3d719b114ee979"
GROUND_TRUTH_SHA256 = "28879c8765445aac809a5f856510cbef40c73fff9dbc7b753d90c984ec20092f"


def arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--upstream-root", required=True, help="pinned FD-Bench checkout")
    parser.add_argument("--trace", required=True, help="finalized five-field condition trace")
    parser.add_argument("--output", required=True, help="metric JSON output")
    return parser.parse_args()


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def flatten(values: Iterable[Any]) -> list[Any]:
    result: list[Any] = []
    for value in values:
        if isinstance(value, list):
            result.extend(flatten(value))
        else:
            result.append(value)
    return result


def median(values: list[int]) -> float | None:
    """Median of the observed samples, or None when nothing was observed.

    An empty sample set has no median. Returning 0 would publish the best
    attainable value for every latency metric here — a run that emitted no
    speech at all would report a 0 ms first-speech-emission delay, which reads
    as a perfect score rather than as a missing measurement.
    """
    if not values:
        return None
    ordered = sorted(values)
    middle = len(ordered) // 2
    if len(ordered) % 2:
        return float(ordered[middle])
    return (ordered[middle - 1] + ordered[middle]) / 2


def rate(numerator: int, denominator: int) -> float | None:
    """Ratio over an observed population, or None when that population is empty.

    A zero denominator makes the rate undefined, not zero. "0 of 0 interruptions
    succeeded" is the absence of a measurement, and publishing it as 0% is
    indistinguishable from having interrupted many times and failed every one.
    """
    if not denominator:
        return None
    return numerator / denominator


def percentage(numerator: int, denominator: int) -> float | None:
    value = rate(numerator, denominator)
    return None if value is None else round(100 * value, 2)


def milliseconds(values: list[int]) -> float | None:
    value = median(values)
    return None if value is None else round(value, 2)


def atomic_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(prefix=".fdbench-score-", suffix=".tmp", dir=path.parent)
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


def load_upstream(upstream_root: Path) -> tuple[Any, Path, Path]:
    revision = subprocess.run(
        ["git", "-C", str(upstream_root), "rev-parse", "HEAD"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    if revision != BENCHMARK_REVISION:
        raise ValueError(f"FD-Bench revision {revision}, expected {BENCHMARK_REVISION}")
    benchmarking_path = upstream_root / "benchmark" / "benchmarking.py"
    ground_truth_path = upstream_root / "openai" / "data_gen" / "output_all.jsonl"
    if sha256(benchmarking_path) != BENCHMARKING_SHA256:
        raise ValueError("FD-Bench benchmarking.py digest mismatch")
    if sha256(ground_truth_path) != GROUND_TRUTH_SHA256:
        raise ValueError("FD-Bench ground truth digest mismatch")
    specification = importlib.util.spec_from_file_location("fdbench_upstream", benchmarking_path)
    if specification is None or specification.loader is None:
        raise ValueError("cannot load pinned FD-Bench evaluator")
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module, benchmarking_path, ground_truth_path


def aggregate(benchmark: Any) -> dict[str, Any]:
    counters = {
        "rounds": 0,
        "interruptions": 0,
        "gaps": 0,
        "success_responses": 0,
        "success_responses_to_interruption": 0,
        "success_interruptions": 0,
        "early_interruptions": 0,
        "noise_interruptions": 0,
    }
    interruption_delays: list[Any] = []
    response_delays: list[Any] = []
    response_delays_to_interruption: list[Any] = []
    lead_times: list[Any] = []
    early_interrupt_times: list[Any] = []
    lead_times_to_interruption: list[Any] = []
    for data in benchmark.VAD_marks.values():
        if data["Number Round"] <= 0:
            continue
        counters["rounds"] += data["Number Round"]
        counters["interruptions"] += data["Number Interrupt"]
        counters["gaps"] += data["Number Gaps"]
        counters["success_responses"] += data["Success Response"]
        counters["success_responses_to_interruption"] += data["Success Response to Interruptions"]
        counters["success_interruptions"] += data["Success Interrupt"]
        counters["early_interruptions"] += data["Wrong Interrupt"]
        counters["noise_interruptions"] += data["Noise Interrupt"]
        interruption_delays.append(flatten(data["Interrupt Delay"]))
        response_delays.append(flatten(data["Response Delay"]))
        response_delays_to_interruption.append(flatten(data["Response Delay to Interruption"]))
        # Preserve upstream analyze_VAD_marks: post-interruption response delay
        # contributes to FSED as well as its separately retained observation.
        response_delays.append(flatten(data["Response Delay to Interruption"]))
        lead_times.append(flatten(data["Lead Times"]))
        early_interrupt_times.append(flatten(data["Lead Times Interrupt"]))
        lead_times_to_interruption.append(flatten(data["Lead Times to Interruption"]))

    def to_ms(values):
        return [int(value / 16) for value in flatten(values) if value > 0]

    if counters["rounds"] <= 0:
        raise ValueError(
            "FD-Bench scored no rounds; every metric would be computed over an "
            "empty population. This is a failed evaluation, not a result."
        )

    response_ms = to_ms(response_delays)
    lead_ms = to_ms(lead_times) + to_ms(lead_times_to_interruption)
    early_ms = to_ms(early_interrupt_times)
    interruption_ms = to_ms(interruption_delays)
    metrics = {
        "SRR_pct": percentage(counters["success_responses"], counters["rounds"]),
        "SIR_pct": percentage(counters["success_interruptions"], counters["interruptions"]),
        "EIR_pct": percentage(counters["early_interruptions"], counters["rounds"]),
        "NIR_pct": percentage(counters["noise_interruptions"], counters["gaps"]),
        "SRIR_pct": percentage(
            counters["success_responses_to_interruption"], counters["success_interruptions"]
        ),
        "FSED_ms": milliseconds(response_ms),
        "ERT_ms": milliseconds(lead_ms),
        "EIT_ms": milliseconds(early_ms),
        "IRD_ms": milliseconds(interruption_ms),
    }
    # Every undefined metric names the empty population that made it undefined,
    # so a reader never has to guess whether a null means "not observed" or
    # "not implemented". The two maps are kept in exact correspondence below.
    populations = {
        "SRR_pct": ("scored rounds", counters["rounds"]),
        "SIR_pct": ("observed interruptions", counters["interruptions"]),
        "EIR_pct": ("scored rounds", counters["rounds"]),
        "NIR_pct": ("observed gaps", counters["gaps"]),
        "SRIR_pct": ("successful interruptions", counters["success_interruptions"]),
        "FSED_ms": ("response-delay samples", len(response_ms)),
        "ERT_ms": ("lead-time samples", len(lead_ms)),
        "EIT_ms": ("early-interruption samples", len(early_ms)),
        "IRD_ms": ("interruption-delay samples", len(interruption_ms)),
    }
    not_measured = {
        name: f"no {label} were observed, so the value is undefined rather than zero"
        for name, (label, size) in populations.items()
        if size == 0
    }
    undefined = {name for name, value in metrics.items() if value is None}
    if undefined != set(not_measured):
        raise ValueError(
            "FD-Bench metric nullity and its explanations disagree: "
            f"undefined={sorted(undefined)}, explained={sorted(not_measured)}"
        )

    categories = {}
    for name, count in benchmark.interruption_cats.items():
        # An absent category is a disagreement between the upstream tallies, not
        # a category that scored zero, so it is refused rather than defaulted.
        for label, table in (
            ("interruption_success", benchmark.interruption_success_cats),
            ("response_success", benchmark.interruption_res_suc_cats),
        ):
            if name not in table:
                raise ValueError(
                    f"FD-Bench category {name!r} is missing from {label}; the "
                    "upstream tallies disagree and cannot be aggregated"
                )
        categories[name] = {
            "all": count,
            "interruption_success": benchmark.interruption_success_cats[name],
            "response_success": benchmark.interruption_res_suc_cats[name],
        }
    return {
        "metrics": metrics,
        "not_measured": not_measured,
        "counts": counters,
        "categories": categories,
    }


def main() -> None:
    args = arguments()
    upstream_root = Path(args.upstream_root).resolve()
    trace = Path(args.trace).resolve()
    output = Path(args.output).resolve()
    module, benchmarking_path, ground_truth_path = load_upstream(upstream_root)
    benchmark = module.Benchmarking(
        str(trace), str(ground_truth_path), "interruption", model_name="Freeze-omni"
    )
    benchmark.debug = False
    benchmark.strict = True
    benchmark.analyze_data()
    benchmark.analyze_VAD_interruption_new2()
    result = {
        "schema_version": "1.0.0",
        "benchmark": "FD-Bench",
        "revision": BENCHMARK_REVISION,
        "trace": {"path": str(trace), "sha256": sha256(trace), "samples": len(trace.read_text(encoding="utf-8").splitlines())},
        "upstream": {
            "benchmarking_path": str(benchmarking_path),
            "benchmarking_sha256": BENCHMARKING_SHA256,
            "ground_truth_path": str(ground_truth_path),
            "ground_truth_sha256": GROUND_TRUTH_SHA256,
            "decision_core": "Benchmarking.analyze_VAD_interruption_new2",
        },
        **aggregate(benchmark),
        "not_evaluated": {
            "WER": "The released non-Moshi entry points do not produce the WER file they unconditionally read.",
            "CPPL": "The released entry point expects a separately precomputed Llama-3 CPPL file.",
            "subjective_GPT_score": "Requires the separate upstream OpenAI batch-judge stage and ASR transcript preparation.",
        },
    }
    atomic_json(output, result)
    print(json.dumps(result, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
