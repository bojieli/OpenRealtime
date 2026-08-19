#!/usr/bin/env python3

from __future__ import annotations

import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("summarize-local-gpu-ownership.py")
SPEC = importlib.util.spec_from_file_location("summarize_local_gpu_ownership", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"cannot load {SCRIPT}")
SUMMARY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SUMMARY)


def process(component: str, pid: int) -> dict:
    return {
        "gpu_uuid": "GPU-0",
        "component": component,
        "component_pid": pid,
        "pid": pid,
        "proc_start_time_ticks": pid * 10,
    }


def snapshot() -> dict:
    return {
        "schema_version": "1.0.0",
        "exclusive": True,
        "host_boot_id": "boot",
        "expected_components": ["fast", "slow"],
        "component_roots": {"fast": 11, "slow": 22},
        "gpu_uuids": ["GPU-0"],
        "processes": [process("fast", 11), process("slow", 22)],
    }


class GpuOwnershipSummaryTest(unittest.TestCase):
    """Drive the summariser over a complete guard log, then remove one fact."""

    def setUp(self) -> None:
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(
            lambda: [item.unlink() for item in self.directory.iterdir()]
            and self.directory.rmdir()
        )
        self.records = [
            {
                "type": "guard.started",
                "recorded_at": "2026-08-19T00:00:00Z",
                "interval_seconds": 30,
                "capture_timeout_seconds": 60,
            },
            {"type": "guard.check", "status": "ok", "ownership": snapshot()},
            {"type": "guard.check", "status": "ok", "ownership": snapshot()},
            {
                "type": "guard.completed",
                "status": "complete",
                "exit_status": 0,
                "recorded_at": "2026-08-19T01:00:00Z",
            },
        ]

    def summarize(self) -> dict:
        log = self.directory / "guard.jsonl"
        log.write_text(
            "".join(json.dumps(record) + "\n" for record in self.records),
            encoding="utf-8",
        )
        output = self.directory / "summary.json"
        argv = [
            "--log",
            str(log),
            "--repository-root",
            str(self.directory),
            "--output",
            str(output),
        ]
        original = SUMMARY.os.sys.argv
        SUMMARY.os.sys.argv = ["summarize-local-gpu-ownership.py", *argv]
        try:
            SUMMARY.main()
        finally:
            SUMMARY.os.sys.argv = original
        return json.loads(output.read_text(encoding="utf-8"))

    def test_summarizes_a_complete_guard_log(self) -> None:
        summary = self.summarize()
        self.assertEqual(summary["status"], "complete")
        self.assertEqual(summary["checks"], 2)
        self.assertEqual(summary["host_boot_id"], "boot")
        self.assertEqual(len(summary["processes"]), 2)

    def refuses(self, message: str) -> None:
        with self.assertRaises(RuntimeError) as caught:
            self.summarize()
        self.assertIn(message, str(caught.exception))

    def mutate(self, index: int, mutation) -> None:
        record = copy.deepcopy(self.records[index])
        mutation(record)
        self.records[index] = record

    def test_refuses_exclusivity_claimed_over_no_processes(self) -> None:
        for index in (1, 2):
            self.mutate(index, lambda r: r["ownership"].__setitem__("processes", []))
        self.refuses("no GPU processes")

    def test_refuses_a_snapshot_that_named_no_host(self) -> None:
        for index in (1, 2):
            self.mutate(index, lambda r: r["ownership"].pop("host_boot_id"))
        self.refuses("recorded no host_boot_id")

    def test_refuses_a_snapshot_that_named_no_device(self) -> None:
        for index in (1, 2):
            self.mutate(index, lambda r: r["ownership"].pop("gpu_uuids"))
        self.refuses("recorded no gpu_uuids")

    def test_refuses_a_snapshot_that_named_no_component_roots(self) -> None:
        for index in (1, 2):
            self.mutate(index, lambda r: r["ownership"].pop("component_roots"))
        self.refuses("recorded no component_roots")

    def test_refuses_a_process_missing_its_identity(self) -> None:
        for index in (1, 2):
            self.mutate(
                index, lambda r: r["ownership"]["processes"][0].pop("proc_start_time_ticks")
            )
        self.refuses("recorded no proc_start_time_ticks")

    def test_refuses_a_capture_that_left_a_component_unoccupied(self) -> None:
        for index in (1, 2):
            self.mutate(index, lambda r: r["ownership"]["processes"].pop())
        self.refuses("does not occupy every expected component")

    def test_refuses_a_check_that_recorded_no_ownership(self) -> None:
        for index in (1, 2):
            self.mutate(index, lambda r: r.pop("ownership"))
        self.refuses("is not a record")

    def test_refuses_a_start_record_missing_its_interval(self) -> None:
        self.mutate(0, lambda r: r.pop("interval_seconds"))
        self.refuses("recorded no interval_seconds")

    def test_refuses_a_completion_missing_its_timestamp(self) -> None:
        self.mutate(3, lambda r: r.pop("recorded_at"))
        self.refuses("recorded no recorded_at")

    def test_refuses_a_device_change_between_checks(self) -> None:
        self.mutate(2, lambda r: r["ownership"].__setitem__("gpu_uuids", ["GPU-1"]))
        self.refuses("device changed")


if __name__ == "__main__":
    unittest.main()
