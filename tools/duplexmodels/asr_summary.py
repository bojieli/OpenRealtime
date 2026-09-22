#!/usr/bin/env python3
"""Tabulate tools/asrbench results (.runtime/duplex-plan/results/asr/*.json).

One row per result file: recogniser, fixture language, error rate (WER for
English and French, CER for Mandarin), first hypothesis and first committed
text (p50, from the start of the audio, which carries 300 ms of pre-roll),
finalization delay after the last frame, committed withdrawals, provisional
rewrites, failures, and replay concurrency.

    python tools/duplexmodels/asr_summary.py [files ...]
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2] / ".runtime/duplex-plan/results/asr"


def p50(distribution: dict) -> str:
    value = distribution.get("p50") if distribution else None
    return f"{value:.0f}" if value is not None else "-"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("files", nargs="*")
    args = parser.parse_args()
    files = [Path(name) for name in args.files] or sorted(ROOT.glob("*.json"))
    print("| result | recogniser | lang | error rate | n | first hyp p50 ms | first committed p50 ms "
          "| finalize p50 ms | withdrawals | rewrites | failed | concurrency |")
    print("|" + " --- |" * 12)
    for path in files:
        try:
            data = json.loads(path.read_text())
        except (OSError, json.JSONDecodeError):
            continue
        summary = data.get("summary")
        if not isinstance(summary, dict) or "error_rate" not in summary:
            continue
        rates = summary["error_rate"]
        for language, rate in sorted(rates.items()) or [("-", None)]:
            name = data.get("label") or data.get("model") or data.get("provider")
            print(f"| {path.stem} | {data.get('provider')}/{name} | {language} | "
                  f"{rate * 100:.1f}% | {summary['utterances'] - summary['failed']} | "
                  f"{p50(summary.get('first_partial_ms'))} | {p50(summary.get('first_committed_ms'))} | "
                  f"{p50(summary.get('finalize_ms'))} | {summary.get('committed_withdrawals', '-')} | "
                  f"{summary.get('provisional_rewrites', '-')} | {summary['failed']} | {data.get('concurrency', '-')} |"
                  if rate is not None else f"| {path.stem} | - | - | - | - | - | - | - | - | - | - | - |")


if __name__ == "__main__":
    main()
