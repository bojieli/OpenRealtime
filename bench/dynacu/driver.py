#!/usr/bin/env python3
"""Run DynaCU-Bench against an OpenRealtime endpoint.

This file is OpenRealtime's runner for a benchmark that lives somewhere else.
It is copied into a prepared AOI checkout and executed there, so the suite,
its 150 task pages, its browser environment, and its evaluator are all the
published ones - nothing about what a task is or whether it passed is decided
here.

Pointing the suite at OpenRealtime needs no bridge. The AOI harness already
carries a provider-agnostic GA Realtime baseline whose websocket base,
credential, and image support are constructor arguments, because OpenAI and
xAI both speak that protocol; OpenRealtime is a strict superset of it, so it
is a third value for the same argument. That is the protocol claim tested by
software that has never heard of this project.

Results are written as one JSON object per line, which is what lets a cell
that was interrupted be resumed rather than lost.
"""
from __future__ import annotations

import argparse
import json
import logging
import sys
import time
from pathlib import Path

CHECKOUT = Path(__file__).resolve().parent
sys.path.insert(0, str(CHECKOUT))

from aoi.realtime_baselines import OpenAIRealtimeWSBaseline  # noqa: E402
from dynacubench.tasks_v3 import DynaCUBenchV3  # noqa: E402

log = logging.getLogger("openrealtime-dynacu")


def select(bench, category, difficulty, task_ids, limit):
    """Choose the tasks this run covers, in the suite's own order."""
    if task_ids:
        wanted = [identifier.strip() for identifier in task_ids.split(",") if identifier.strip()]
        tasks = [bench.get_task(identifier) for identifier in wanted]
        missing = [name for name, task in zip(wanted, tasks) if task is None]
        if missing:
            raise SystemExit(f"unknown task ids: {', '.join(missing)}")
        return tasks
    tasks = list(bench)
    if category:
        tasks = [task for task in tasks
                 if task.category.value == category or task.task_id.startswith(category)]
    if difficulty:
        tasks = [task for task in tasks if task.difficulty.value == difficulty]
    if limit and limit > 0:
        tasks = tasks[:limit]
    return tasks


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", required=True,
                        help="OpenRealtime websocket base, e.g. ws://127.0.0.1:8765/v1/realtime")
    parser.add_argument("--model", default="openrealtime")
    parser.add_argument("--token-env", default="OPENREALTIME_TOKEN")
    parser.add_argument("--out", required=True, help="JSONL file, one object per task")
    parser.add_argument("--category", default="")
    parser.add_argument("--difficulty", default="")
    parser.add_argument("--task-ids", default="")
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--max-steps", type=int, default=15)
    parser.add_argument("--step-interval", type=float, default=2.0)
    parser.add_argument("--no-images", action="store_true",
                        help="withhold screenshots, leaving audio and the element list")
    parser.add_argument("--no-page-elements", action="store_true",
                        help="withhold the interactive-element list, leaving the screenshot alone")
    parser.add_argument("--count-only", action="store_true",
                        help="print the task count this selection covers and exit")
    arguments = parser.parse_args()

    logging.basicConfig(level=logging.INFO,
                        format="%(asctime)s [%(levelname)s] %(message)s", datefmt="%H:%M:%S")

    bench = DynaCUBenchV3(html_tasks_dir=CHECKOUT / "benchmark_env" / "html_tasks")
    tasks = select(bench, arguments.category, arguments.difficulty,
                   arguments.task_ids, arguments.limit)
    if arguments.count_only:
        print(json.dumps({"declared": len(bench), "selected": len(tasks)}))
        return 0
    if not tasks:
        raise SystemExit("the selection covers no tasks")

    # An interrupted run resumes rather than restarting. A 150-task cell is
    # hours, and losing all of it to one crash is how a measurement program
    # stops being run.
    done = set()
    out = Path(arguments.out)
    if out.exists():
        for line in out.read_text().splitlines():
            try:
                record = json.loads(line)
            except ValueError:
                continue
            if record.get("error") is None and "success" in record:
                done.add(record["task_id"])

    evaluator = OpenAIRealtimeWSBaseline(
        model=arguments.model,
        max_steps=arguments.max_steps,
        step_interval_s=arguments.step_interval,
        provide_page_elements=not arguments.no_page_elements,
        ws_base=arguments.endpoint,
        api_key_env=arguments.token_env,
        send_images=not arguments.no_images,
    )

    with out.open("a", buffering=1) as handle:
        for index, task in enumerate(tasks, start=1):
            if task.task_id in done:
                log.info("[%d/%d] %s already done", index, len(tasks), task.task_id)
                continue
            log.info("[%d/%d] %s (%s, %s)", index, len(tasks),
                     task.task_id, task.category.value, task.difficulty.value)
            started = time.time()
            try:
                record = evaluator.run_task(task).to_dict()
            except Exception as failure:  # noqa: BLE001 - a crash is a result, not an exit
                log.exception("task %s crashed", task.task_id)
                record = {
                    "task_id": task.task_id,
                    "category": task.category.value,
                    "difficulty": task.difficulty.value,
                    "success": False,
                    "error": f"CRASH: {failure}",
                }
            record["wall_s"] = round(time.time() - started, 1)
            handle.write(json.dumps(record) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
