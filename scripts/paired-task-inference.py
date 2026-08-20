#!/usr/bin/env python3
"""Quantify a paired tau-Voice condition difference over the same task set.

The existing paired reports state an exact population difference and decline
any confidence claim, because one fixed-order trial per task cannot support a
repeated-trial interval. That is the right refusal for a trial-level claim and
it leaves a different question unanswered: given that both conditions ran the
same 278 tasks, how much of the observed difference survives the possibility
that the per-task outcomes fell out the way they did by chance?

Pairing is by task, not by trial. Each task contributes one outcome under each
condition, so the exact sign test and the paired bootstrap below are computed
over tasks. This is a weaker claim than a repeated-trial interval and is
labelled as such in the output: it bounds task-sampling variation only, and it
says nothing about run-to-run variation, which needs repeated trials that the
current matrices (num_trials = 1) do not run.

Both tests are exact or resampled rather than normal-approximate, because a
reward distribution concentrated at 0 and 1 is not normal at these sizes.

Read-only. It reads archived populations and never participates in scoring.
"""

from __future__ import annotations

import argparse
import json
import math
import random
import sys
from fractions import Fraction
from pathlib import Path
from typing import Any

SCHEMA_VERSION = "1.0.0"

# Fixed so a report is reproducible from its inputs alone. A seed that varied
# per run would make two reports over identical evidence disagree.
BOOTSTRAP_SEED = 20260819
BOOTSTRAP_RESAMPLES = 20000


class PairedInferenceError(RuntimeError):
    """Raised when two populations are not a task-paired comparison."""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", required=True, type=Path)
    parser.add_argument("--treatment", required=True, type=Path)
    parser.add_argument("--baseline-label", default="baseline")
    parser.add_argument("--treatment-label", default="treatment")
    parser.add_argument("--output", type=Path)
    return parser.parse_args()


def load_rewards(directory: Path) -> dict[str, float]:
    """Map task to reward, refusing anything that is not a scored population."""
    results = directory / "results.json"
    if not results.is_file():
        raise PairedInferenceError(f"{directory}: no results.json")
    if results.stat().st_size == 0:
        raise PairedInferenceError(f"{directory}: results.json is empty")
    payload = json.loads(results.read_text())
    index = payload.get("simulation_index")
    if not isinstance(index, list) or not index:
        raise PairedInferenceError(f"{directory}: declares no simulations")

    rewards: dict[str, float] = {}
    for entry in index:
        task = entry.get("task_id")
        if task is None:
            raise PairedInferenceError(f"{directory}: a simulation has no task id")
        key = str(task)
        trial = entry.get("trial")
        if trial not in (0, None):
            # More than one trial per task is a different, stronger design; this
            # tool would silently discard trials, so it refuses instead.
            raise PairedInferenceError(
                f"{directory}: task {key!r} has trial {trial!r}; "
                "repeated trials need a repeated-trial estimator, not this one"
            )
        if key in rewards:
            raise PairedInferenceError(f"{directory}: duplicate task {key!r}")
        reward = entry.get("reward")
        if reward is None:
            # An unrun simulation. Dropping it silently would let one condition
            # be scored over fewer tasks than the other.
            continue
        if not isinstance(reward, (int, float)):
            raise PairedInferenceError(
                f"{directory}: task {key!r} has non-numeric reward {reward!r}"
            )
        rewards[key] = float(reward)
    if not rewards:
        raise PairedInferenceError(f"{directory}: no task scored")
    return rewards


def binomial_two_sided_p(successes: int, trials: int) -> float:
    """Exact two-sided sign-test p-value under p = 1/2.

    Computed with exact rationals so a small tail is not lost to floating
    point, then converted once at the end.
    """
    if trials == 0:
        raise PairedInferenceError("sign test over zero discordant pairs")
    half = Fraction(1, 2)
    weights = [
        math.comb(trials, k) * half**trials for k in range(trials + 1)
    ]
    observed = weights[successes]
    total = sum(weight for weight in weights if weight <= observed)
    return float(min(Fraction(1), total))


def paired_bootstrap(
    differences: list[float], resamples: int, seed: int
) -> dict[str, Any]:
    """Percentile interval for the mean paired difference."""
    if not differences:
        raise PairedInferenceError("bootstrap over zero pairs")
    generator = random.Random(seed)
    count = len(differences)
    means: list[float] = []
    for _ in range(resamples):
        total = 0.0
        for _ in range(count):
            total += differences[generator.randrange(count)]
        means.append(total / count)
    means.sort()

    def percentile(fraction: float) -> float:
        rank = max(1, math.ceil(fraction * len(means)))
        return means[min(rank, len(means)) - 1]

    return {
        "resamples": resamples,
        "seed": seed,
        "ci95_low": percentile(0.025),
        "ci95_high": percentile(0.975),
    }


def compare(baseline: dict[str, float], treatment: dict[str, float]) -> dict[str, Any]:
    shared = sorted(set(baseline) & set(treatment))
    if not shared:
        raise PairedInferenceError("the two conditions share no task")

    differences = [treatment[task] - baseline[task] for task in shared]
    better = sum(1 for value in differences if value > 0)
    worse = sum(1 for value in differences if value < 0)
    tied = sum(1 for value in differences if value == 0)

    mean_difference = sum(differences) / len(differences)
    sign_test: dict[str, Any] = {
        "discordant_pairs": better + worse,
        "treatment_better": better,
        "treatment_worse": worse,
        "tied": tied,
    }
    if better + worse == 0:
        sign_test["p_value"] = None
        sign_test["note"] = "every paired task tied; the sign test has no evidence"
    else:
        sign_test["p_value"] = binomial_two_sided_p(better, better + worse)

    bootstrap = paired_bootstrap(differences, BOOTSTRAP_RESAMPLES, BOOTSTRAP_SEED)
    crosses_zero = bootstrap["ci95_low"] <= 0.0 <= bootstrap["ci95_high"]

    return {
        "paired_tasks": len(shared),
        "baseline_only_tasks": sorted(set(baseline) - set(treatment)),
        "treatment_only_tasks": sorted(set(treatment) - set(baseline)),
        "baseline_mean": sum(baseline[task] for task in shared) / len(shared),
        "treatment_mean": sum(treatment[task] for task in shared) / len(shared),
        "mean_difference": mean_difference,
        "sign_test": sign_test,
        "paired_bootstrap": bootstrap,
        "interval_crosses_zero": crosses_zero,
        "inference_scope": (
            "bounds task-sampling variation over one fixed-order trial per task; "
            "it does not bound run-to-run variation, which requires repeated "
            "trials that these matrices do not run"
        ),
    }


def main() -> int:
    arguments = parse_args()
    try:
        baseline = load_rewards(arguments.baseline)
        treatment = load_rewards(arguments.treatment)
        comparison = compare(baseline, treatment)
    except PairedInferenceError as error:
        print(f"paired inference refused: {error}", file=sys.stderr)
        return 1

    report = {
        "schema_version": SCHEMA_VERSION,
        "baseline": {
            "label": arguments.baseline_label,
            "population": arguments.baseline.name,
            "scored_tasks": len(baseline),
        },
        "treatment": {
            "label": arguments.treatment_label,
            "population": arguments.treatment.name,
            "scored_tasks": len(treatment),
        },
        "comparison": comparison,
    }
    text = json.dumps(report, indent=2)
    if arguments.output:
        arguments.output.parent.mkdir(parents=True, exist_ok=True)
        arguments.output.write_text(text + "\n")
    else:
        print(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
