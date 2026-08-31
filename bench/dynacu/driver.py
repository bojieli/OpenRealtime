#!/usr/bin/env python3
"""Run pinned DynaCU-Bench with create-only synchronized review evidence.

The upstream suite remains authoritative for task selection, browser behavior,
and scoring. This runner subclasses only its GA Realtime client and observes
the exact existing wire hooks: ``_send`` supplies PCM/JPEG inputs and
``_await_tool_call`` supplies actions returned by the endpoint. Every task
opens a private evidence directory before ``run_task`` can create a browser or
websocket. A manifest is published last after exact wire inputs, an action
trace, and a synchronized review MP4 have been retained.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import logging
import os
import shutil
import subprocess
import sys
import time
import wave
import zipfile
from pathlib import Path


CHECKOUT = Path(__file__).resolve().parent
RECORDER_FORMAT = "openrealtime.dynacu-wire-evidence"
RECORDER_VERSION = 1
MAX_TASK_DURATION_US = 30 * 60 * 1_000_000
MAX_WIRE_ITEM_BYTES = 16 << 20
MAX_WIRE_ARCHIVE_BYTES = 64 << 20
log = logging.getLogger("openrealtime-dynacu")


def _canonical(value) -> bytes:
    return (json.dumps(value, sort_keys=True, separators=(",", ":"),
                       ensure_ascii=False) + "\n").encode("utf-8")


def _write_exclusive(path: Path, payload: bytes) -> None:
    if not payload:
        raise ValueError("empty evidence payload")
    with path.open("xb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())


def _file_identity(path: Path) -> dict:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as handle:
        while True:
            chunk = handle.read(1 << 20)
            if not chunk:
                break
            size += len(chunk)
            digest.update(chunk)
    return {"path": path.name, "sha256": "sha256:" + digest.hexdigest(),
            "size_bytes": size}


def _task_value(task, field: str) -> str:
    value = getattr(task, field)
    return str(getattr(value, "value", value))


class WireTaskRecorder:
    """One create-only task recorder attached to the upstream wire hooks."""

    def __init__(self, root: Path, task):
        self.task_id = str(task.task_id)
        self.category = _task_value(task, "category")
        self.difficulty = _task_value(task, "difficulty")
        self.started_monotonic = time.monotonic()
        self.directory = root / hashlib.sha256(self.task_id.encode("utf-8")).hexdigest()
        self.directory.mkdir(mode=0o700, parents=False, exist_ok=False)
        self.frames_directory = self.directory / "frames"
        self.audio_directory = self.directory / "audio"
        self.frames_directory.mkdir(mode=0o700)
        self.audio_directory.mkdir(mode=0o700)
        self.wire = []
        self.actions = []
        self.transcripts = []
        self.capture_errors = []
        self.audio = []
        self.frames = []
        attempt = {
            "format": RECORDER_FORMAT,
            "format_version": RECORDER_VERSION,
            "task_id": self.task_id,
            "category": self.category,
            "difficulty": self.difficulty,
            "terminal": "active",
        }
        _write_exclusive(self.directory / "attempt.json", _canonical(attempt))

    def _at_us(self) -> int:
        return max(0, int(round((time.monotonic() - self.started_monotonic) * 1_000_000)))

    def record_error(self, code: str) -> None:
        if code not in self.capture_errors and len(self.capture_errors) < 64:
            self.capture_errors.append(code)

    def observe_send(self, event: dict) -> None:
        event_type = str(event.get("type", ""))
        at_us = self._at_us()
        if event_type == "input_audio_buffer.append":
            try:
                payload = base64.b64decode(event.get("audio", ""), validate=True)
                if not payload or len(payload) > MAX_WIRE_ITEM_BYTES or len(payload) % 2:
                    raise ValueError("invalid PCM")
                ordinal = len(self.audio) + 1
                relative = f"audio/{ordinal:06d}.pcm"
                _write_exclusive(self.directory / relative, payload)
                item = {
                    "kind": "input_audio_pcm16_24000_mono", "at_us": at_us,
                    "path": relative, "size_bytes": len(payload),
                    "sha256": "sha256:" + hashlib.sha256(payload).hexdigest(),
                    "sample_count": len(payload) // 2,
                }
                self.audio.append(item)
                self.wire.append(item)
            except Exception:  # recorder failure must not change upstream scoring
                self.record_error("audio_capture_failed")
            return
        if event_type == "conversation.item.create":
            content = event.get("item", {}).get("content", [])
            for part in content if isinstance(content, list) else []:
                if part.get("type") != "input_image":
                    continue
                try:
                    prefix = "data:image/jpeg;base64,"
                    source = part.get("image_url", "")
                    if not isinstance(source, str) or not source.startswith(prefix):
                        raise ValueError("unsupported image")
                    payload = base64.b64decode(source[len(prefix):], validate=True)
                    if not payload or len(payload) > MAX_WIRE_ITEM_BYTES:
                        raise ValueError("invalid JPEG")
                    ordinal = len(self.frames) + 1
                    relative = f"frames/{ordinal:06d}.jpg"
                    _write_exclusive(self.directory / relative, payload)
                    item = {
                        "kind": "input_image_jpeg", "at_us": at_us,
                        "path": relative, "size_bytes": len(payload),
                        "sha256": "sha256:" + hashlib.sha256(payload).hexdigest(),
                    }
                    self.frames.append(item)
                    self.wire.append(item)
                except Exception:
                    self.record_error("image_capture_failed")
        if event_type:
            self.wire.append({"kind": "client_event", "at_us": at_us,
                              "event_type": event_type})

    def observe_tool_call(self, name, arguments, error, heard) -> None:
        at_us = self._at_us()
        safe_arguments = arguments if isinstance(arguments, dict) else {}
        self.actions.append({
            "at_us": at_us,
            "name": str(name or ""),
            "arguments": safe_arguments,
            "error": bool(error),
        })
        for value in heard:
            text = str(value or "")
            if text:
                self.transcripts.append({"at_us": at_us, "text": text[:16_384]})

    def _duration_us(self, deterministic_result: dict) -> int:
        candidates = [self._at_us()]
        for group in (self.audio, self.frames, self.actions, self.transcripts):
            candidates.extend(int(item.get("at_us", 0)) for item in group)
        try:
            candidates.append(int(float(deterministic_result.get("total_time_s", 0)) * 1_000_000))
        except (TypeError, ValueError):
            pass
        duration = max(candidates + [100_000])
        if duration > MAX_TASK_DURATION_US:
            self.record_error("capture_duration_exceeded")
            duration = MAX_TASK_DURATION_US
        return max(duration, 100_000)

    def _write_audio_wav(self, duration_us: int) -> Path:
        sample_rate = 24_000
        total_samples = max(1, (duration_us * sample_rate + 999_999) // 1_000_000)
        payload = bytearray(total_samples * 2)
        for item in self.audio:
            raw = (self.directory / item["path"]).read_bytes()
            samples = len(raw) // 2
            end = min(total_samples, (int(item["at_us"]) * sample_rate + 999_999) // 1_000_000)
            start = max(0, end - samples)
            available = max(0, min(samples, total_samples - start))
            payload[start * 2:(start + available) * 2] = raw[:available * 2]
        target = self.directory / "timeline.wav"
        with target.open("xb") as output:
            with wave.open(output, "wb") as wav:
                wav.setnchannels(1)
                wav.setsampwidth(2)
                wav.setframerate(sample_rate)
                wav.writeframes(payload)
            output.flush()
            os.fsync(output.fileno())
        return target

    def _write_review_video(self, duration_us: int, audio_path: Path) -> Path | None:
        if not self.frames:
            self.record_error("review_video_missing_frames")
            return None
        ffmpeg = shutil.which("ffmpeg")
        if not ffmpeg:
            self.record_error("review_video_ffmpeg_missing")
            return None
        timeline = self.directory / "frames.ffconcat"
        lines = ["ffconcat version 1.0"]
        for index, frame in enumerate(self.frames):
            current = int(frame["at_us"])
            following = (int(self.frames[index + 1]["at_us"])
                         if index + 1 < len(self.frames) else duration_us)
            frame_duration = max(50_000, following - current) / 1_000_000
            lines.append(f"file 'frames/{index + 1:06d}.jpg'")
            lines.append(f"duration {frame_duration:.6f}")
        lines.append(f"file 'frames/{len(self.frames):06d}.jpg'")
        _write_exclusive(timeline, ("\n".join(lines) + "\n").encode("ascii"))
        target = self.directory / "review.mp4"
        partial = self.directory / "review.partial.mp4"
        first_s = int(self.frames[0]["at_us"]) / 1_000_000
        command = [
            ffmpeg, "-nostdin", "-v", "error", "-f", "concat", "-safe", "1",
            "-i", str(timeline), "-i", str(audio_path),
            "-filter_complex", f"[0:v]setpts=PTS+{first_s:.6f}/TB,pad=ceil(iw/2)*2:ceil(ih/2)*2[v]",
            "-map", "[v]", "-map", "1:a:0", "-t", f"{duration_us / 1_000_000:.6f}",
            "-vsync", "vfr", "-c:v", "libx264", "-preset", "medium", "-crf", "18",
            "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k",
            "-movflags", "+faststart", "-map_metadata", "-1", "-n", str(partial),
        ]
        try:
            completed = subprocess.run(command, stdin=subprocess.DEVNULL,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                                       timeout=180, check=False)
        except Exception:
            self.record_error("review_video_encode_failed")
            return None
        if completed.returncode != 0 or not partial.is_file() or partial.stat().st_size <= 0:
            self.record_error("review_video_encode_failed")
            return None
        os.link(partial, target)
        partial.unlink()
        return target

    def _write_wire_archive(self) -> Path:
        index_payload = _canonical({"format": RECORDER_FORMAT + ".wire-index",
                                    "format_version": RECORDER_VERSION,
                                    "events": self.wire})
        archive = self.directory / "wire.zip"
        members = [("attempt.json", (self.directory / "attempt.json").read_bytes()),
                   ("wire-index.json", index_payload)]
        for item in sorted(self.audio + self.frames, key=lambda value: value["path"]):
            members.append((item["path"], (self.directory / item["path"]).read_bytes()))
        with zipfile.ZipFile(archive, mode="x", compression=zipfile.ZIP_STORED,
                             allowZip64=True) as output:
            for name, payload in members:
                info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                info.compress_type = zipfile.ZIP_STORED
                info.external_attr = 0o600 << 16
                output.writestr(info, payload)
        if archive.stat().st_size <= 0 or archive.stat().st_size > MAX_WIRE_ARCHIVE_BYTES:
            self.record_error("wire_archive_size_exceeded")
            archive.unlink(missing_ok=True)
            raise ValueError("wire archive is outside the retained bound")
        return archive

    def finish(self, deterministic_result: dict) -> Path:
        duration_us = self._duration_us(deterministic_result)
        wire_path = self._write_wire_archive()
        audio_path = self._write_audio_wav(duration_us)
        video_path = self._write_review_video(duration_us, audio_path)
        self.capture_errors.sort()
        trace = {
            "format": RECORDER_FORMAT + ".action-trace",
            "format_version": RECORDER_VERSION,
            "task_id": self.task_id,
            "category": self.category,
            "difficulty": self.difficulty,
            "duration_us": duration_us,
            "actions": self.actions,
            "transcripts": self.transcripts,
            "wire_events": self.wire,
            "deterministic_result": deterministic_result,
            "capture_errors": self.capture_errors,
        }
        trace_path = self.directory / "trace.json"
        _write_exclusive(trace_path, _canonical(trace))
        files = {
            "trace": _file_identity(trace_path),
            "wire_archive": _file_identity(wire_path),
            "timeline_audio": _file_identity(audio_path),
        }
        if video_path is not None:
            files["review_media"] = _file_identity(video_path)
            files["review_media"].update({"kind": "video", "media_type": "video/mp4",
                                           "role": "synchronized_screen_audio_and_actions"})
        manifest = {
            "format": RECORDER_FORMAT,
            "format_version": RECORDER_VERSION,
            "complete": video_path is not None and not self.capture_errors,
            "task_id": self.task_id,
            "category": self.category,
            "difficulty": self.difficulty,
            "duration_us": duration_us,
            "audio_chunk_count": len(self.audio),
            "frame_count": len(self.frames),
            "action_count": len(self.actions),
            "capture_errors": self.capture_errors,
            "files": files,
            "terminal": "completed",
        }
        manifest_path = self.directory / "manifest.json"
        _write_exclusive(manifest_path, _canonical(manifest))
        directory = os.open(self.directory, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        return manifest_path


def _recorder_self_test(destination: str) -> int:
    class Value:
        def __init__(self, value):
            self.value = value

    class Task:
        task_id = "self-test"
        category = Value("S_static")
        difficulty = Value("easy")

    root = Path(destination).resolve()
    root.mkdir(mode=0o700, parents=False, exist_ok=False)
    recorder = WireTaskRecorder(root, Task())
    pcm = b"\x00\x00" * 4_800
    recorder.observe_send({"type": "input_audio_buffer.append",
                           "audio": base64.b64encode(pcm).decode("ascii")})
    jpeg = base64.b64decode(
        "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////"
        "2wBDAf//////////////////////////////////////////////////////////////////////////////////////"
        "wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAf/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIQAxAAAAF//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABBQJ//8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAgBAwEBPwF//8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAgBAgEBPwF//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQAGPwJ//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPyF//9oADAMBAAIAAwAAABD/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oACAEDAQE/EP/EABQRAQAAAAAAAAAAAAAAAAAAABD/2gAIAQIBAT8Q/8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPxB//9k=",
        validate=True,
    )
    recorder.observe_send({
        "type": "conversation.item.create",
        "item": {"content": [{"type": "input_image",
                                "image_url": "data:image/jpeg;base64," +
                                base64.b64encode(jpeg).decode("ascii")}]},
    })
    recorder.observe_tool_call("click", {"x": 1, "y": 1}, None, ["heard words"])
    manifest = recorder.finish({
        "task_id": "self-test", "category": "S_static", "difficulty": "easy",
        "success": True, "result_val": "passed", "steps_taken": 1,
        "total_time_s": 0.2, "final_score": 1.0,
    })
    print(str(manifest))
    return 0


if __name__ == "__main__" and len(sys.argv) == 3 and sys.argv[1] == "--recorder-self-test":
    sys.exit(_recorder_self_test(sys.argv[2]))


sys.path.insert(0, str(CHECKOUT))
from aoi.realtime_baselines import OpenAIRealtimeWSBaseline  # noqa: E402
from dynacubench.tasks_v3 import DynaCUBenchV3  # noqa: E402


class RecordingOpenAIRealtimeWSBaseline(OpenAIRealtimeWSBaseline):
    """The pinned baseline plus passive recording at its existing wire seam."""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._openrealtime_recorder = None

    def begin_evidence(self, root: Path, task) -> None:
        if self._openrealtime_recorder is not None:
            raise RuntimeError("previous DynaCU recorder is still active")
        self._openrealtime_recorder = WireTaskRecorder(root, task)

    def finish_evidence(self, result: dict) -> Path:
        recorder = self._openrealtime_recorder
        self._openrealtime_recorder = None
        if recorder is None:
            raise RuntimeError("DynaCU recorder was not started")
        return recorder.finish(result)

    def _send(self, ws, event):
        recorder = self._openrealtime_recorder
        if recorder is not None:
            try:
                recorder.observe_send(event)
            except Exception:
                recorder.record_error("wire_observer_failed")
        return OpenAIRealtimeWSBaseline._send(ws, event)

    def _await_tool_call(self, ws, heard):
        before = len(heard)
        name, arguments, error = super()._await_tool_call(ws, heard)
        recorder = self._openrealtime_recorder
        if recorder is not None:
            try:
                recorder.observe_tool_call(name, arguments, error, heard[before:])
            except Exception:
                recorder.record_error("action_observer_failed")
        return name, arguments, error


def select(bench, category, difficulty, task_ids, limit):
    """Choose tasks in the pinned suite's own canonical order."""
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
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--model", default="openrealtime")
    parser.add_argument("--token-env", default="OPENREALTIME_TOKEN")
    parser.add_argument("--out", required=True, help="create-only JSONL deterministic results")
    parser.add_argument("--evidence-dir", required=False,
                        help="create-only raw synchronized evidence root")
    parser.add_argument("--category", default="")
    parser.add_argument("--difficulty", default="")
    parser.add_argument("--task-ids", default="")
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--max-steps", type=int, default=15)
    parser.add_argument("--step-interval", type=float, default=2.0)
    parser.add_argument("--no-images", action="store_true")
    parser.add_argument("--no-page-elements", action="store_true")
    parser.add_argument("--count-only", action="store_true")
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
    if not arguments.evidence_dir:
        raise SystemExit("--evidence-dir is required for every DynaCU attempt")
    if arguments.no_images:
        raise SystemExit("--no-images cannot produce the mandatory computer-use review video")

    evidence_root = Path(arguments.evidence_dir).resolve()
    evidence_root.mkdir(mode=0o700, parents=False, exist_ok=False)
    out = Path(arguments.out).resolve()
    evaluator = RecordingOpenAIRealtimeWSBaseline(
        model=arguments.model,
        max_steps=arguments.max_steps,
        step_interval_s=arguments.step_interval,
        provide_page_elements=not arguments.no_page_elements,
        ws_base=arguments.endpoint,
        api_key_env=arguments.token_env,
        send_images=True,
    )

    with out.open("x", buffering=1) as handle:
        for index, task in enumerate(tasks, start=1):
            log.info("[%d/%d] %s (%s, %s)", index, len(tasks),
                     task.task_id, task.category.value, task.difficulty.value)
            started = time.time()
            evaluator.begin_evidence(evidence_root, task)
            try:
                record = evaluator.run_task(task).to_dict()
            except Exception as failure:  # a crash is retained, never dropped
                log.exception("task %s crashed", task.task_id)
                record = {
                    "task_id": task.task_id,
                    "category": task.category.value,
                    "difficulty": task.difficulty.value,
                    "success": False,
                    "error": f"CRASH: {failure}",
                }
            record["wall_s"] = round(time.time() - started, 1)
            try:
                evaluator.finish_evidence(record)
            except Exception:
                log.exception("task %s evidence finalization failed", task.task_id)
                record["evidence_error"] = "synchronized recorder finalization failed"
            handle.write(json.dumps(record, sort_keys=True) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
