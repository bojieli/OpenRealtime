#!/usr/bin/env python3
"""Regenerate the generated tables in docs/full-duplex-results.md.

The document's measured tables are produced from the result files rather than
typed, so a number in the prose cannot drift from the run that produced it.
Each generated block is delimited:

    <!-- generated: e2e -->
    ...
    <!-- end generated -->

    python tools/duplexmodels/results_doc.py            # rewrite in place
    python tools/duplexmodels/results_doc.py --check    # fail if stale
"""

import argparse
import io
import re
import subprocess
import sys
from contextlib import redirect_stdout
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DOCUMENT = ROOT / "docs/full-duplex-results.md"
BLOCK = re.compile(r"(<!-- generated: (?P<name>[a-z0-9-]+) -->\n)(?P<body>.*?)(<!-- end generated -->)", re.S)


def run(script: str, *arguments: str) -> str:
    """Run one of the sibling summary tools and return its stdout."""
    completed = subprocess.run([sys.executable, str(ROOT / "tools/duplexmodels" / script), *arguments],
                               capture_output=True, text=True, cwd=ROOT)
    if completed.returncode != 0:
        return f"_{script} failed: {completed.stderr.strip().splitlines()[-1] if completed.stderr else 'no output'}_\n"
    return completed.stdout


def generate(name: str) -> str:
    if name == "asr":
        return run("asr_summary.py")
    if name == "e2e":
        return run("e2e_summary.py")
    if name == "microturn":
        traces = sorted((ROOT / ".runtime/duplex-plan/traces").glob("*.jsonl"))
        return run("microturn_trace.py", *[str(path) for path in traces]) if traces else "_no traces_\n"
    raise SystemExit(f"unknown generated block {name!r}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--document", default=str(DOCUMENT))
    arguments = parser.parse_args()
    path = Path(arguments.document)
    original = path.read_text()

    def replace(match: re.Match) -> str:
        body = generate(match.group("name"))
        if not body.endswith("\n"):
            body += "\n"
        return match.group(1) + body + match.group(4)

    updated = BLOCK.sub(replace, original)
    if arguments.check:
        if updated != original:
            print(f"{path} is stale; run tools/duplexmodels/results_doc.py", file=sys.stderr)
            raise SystemExit(1)
        print(f"{path} is current")
        return
    path.write_text(updated)
    print(f"{path}: {len(BLOCK.findall(original))} generated blocks refreshed")


if __name__ == "__main__":
    main()
