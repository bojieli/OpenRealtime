#!/usr/bin/env python3
"""Wall-clock native-grammar probe with continuing TTS and paced recordings."""
import argparse
import asyncio
import hashlib
import json
from pathlib import Path
import sys
import time

from native_pair_probe import schedule
from paced_recorder import PacedRecorder
from native_cleanup import finish

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "sidecars"))
from duplexcascade_model import DuplexCascadeModel
from duplexcascade_speech import DuplexCascadeSpeech
from microturn_sidecar import SpeechContext


def context_error(context):
    """Return the failure a synthesis context reports, if any. Native control
    cancels superseded contexts; a cancelled reader is not a failure, and
    asking a cancelled task for its exception raises CancelledError."""
    if context.failure:
        return context.failure
    reader = context.reader
    if reader.done() and not reader.cancelled():
        return reader.exception()
    return None


async def trial(args, backend, pair, variant, withheld):
    name = variant["id"] + ("-nofeedback" if withheld else "")
    session_id = args.out.name + "/" + name
    directory = args.out / name
    directory.mkdir()
    recorder = PacedRecorder()
    recorder.start()
    contexts, text = [], []
    closing = False

    class TracedContext(SpeechContext):
        async def open(self, ignored):
            await super().open(recorder.callback())
            if self.sample_rate != recorder.rate:
                raise ValueError("unexpected synthesis sample rate")

    def factory():
        context = TracedContext(args.tts, "expresso/ex03-ex01_happy_001_channel1_334s.wav")
        contexts.append(context)
        return context

    speech = DuplexCascadeSpeech(factory, lambda pcm: None,
                                 lambda value: text.append({"at_s": recorder.now(), "text": value}),
                                 lambda: recorder.cancel("trial-end" if closing else "native-control"))
    session = backend.session()
    pending_inference = None
    result = {"trace_schema_version": 3, "session_id": session_id,
              "pair_id": pair["id"], "variant_id": name, "cell_id": "DC-native-diagnostic",
              "words_per_tick": args.words_per_tick,
              "status": "running", "variant": name, "requests": 0,
              "physical_playback": False, "audible_adaptation_score": None}
    try:
        with (directory / "actions.jsonl").open("x") as trace:
            # Delayed chunk schedule is frozen; overruns admit accumulated
            # chunks at the next actual request, never run a catch-up burst.
            rows = list(schedule(pair, variant, withheld, words_per_tick=args.words_per_tick))
            cursor = 0
            while cursor < len(rows):
                target = rows[cursor][0] / 1e9
                await asyncio.sleep(max(0, target - recorder.now()))
                at = recorder.now()
                fresh = []
                while cursor < len(rows) and rows[cursor][0] / 1e9 <= at:
                    fresh.extend(rows[cursor][1])
                    cursor += 1
                chunk = " ".join(e["text"] for e in fresh)
                pending_inference = asyncio.create_task(asyncio.to_thread(session.step, chunk))
                tokens = await asyncio.shield(pending_inference)
                events = list(session.events(tokens))
                await speech.apply(events)
                elapsed = recorder.now() - at
                trace.write(json.dumps({"trace_schema_version": 3, "session_id": session_id,
                                        "turn_id": "decision-" + str(result["requests"] + 1),
                                        "pair_id": pair["id"], "variant_id": name,
                                        "cell_id": "DC-native-diagnostic", "admission_s": at, "fresh_events": fresh, "input": chunk,
                                        "tokens": tokens, "events": events, "duration_s": elapsed,
                                        "deadline_missed": elapsed > .5}) + "\n")
                trace.flush()
                result["requests"] += 1
                for context in contexts:
                    error = context_error(context)
                    if error:
                        raise error
            result["status"] = "complete"
    except BaseException as exc:
        # A retained trial never stays "running": cancellation and interrupts
        # are terminal failures too, recorded before they propagate.
        result.update(status="failed", error=repr(exc))
        if not isinstance(exc, Exception):
            raise
    finally:
        closing = True
        cleanup_errors = await finish(pending_inference, speech, recorder)
        if cleanup_errors:
            result.update(status="failed", cleanup_errors=cleanup_errors)
        recorder.write(directory / "output.wav")
        result["text"] = text
        result["contexts"] = [{"id": c.id, "sample_rate": c.sample_rate,
                               "failure": str(c.failure) if c.failure else None} for c in contexts]
        (directory / "playback.json").write_text(json.dumps(recorder.marks, indent=2) + "\n")
        (directory / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({k: result[k] for k in ("variant", "status", "requests")}), flush=True)
    return result


async def run(args, backend, pair):
    results = []
    for withheld in (False, True):
        for variant in pair["variants"]:
            results.append(await trial(args, backend, pair, variant, withheld))
    failures = sum(r["status"] != "complete" for r in results)
    (args.out / "campaign.json").write_text(json.dumps({"attempts": len(results), "failures": failures}) + "\n")
    if failures:
        raise RuntimeError(f"{failures} native trials failed; evidence retained")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("source", "snapshot", "base", "prepared-pair", "out"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--tts", default="ws://127.0.0.1:9125/v1/tts/stream")
    parser.add_argument("--words-per-tick", type=int, default=2)
    args = parser.parse_args()
    if args.words_per_tick < 1:
        parser.error("--words-per-tick must be positive")
    args.out.mkdir()
    retained = args.out / "source"
    retained.mkdir()
    hashes = {}
    for path in [Path(__file__), Path(__file__).with_name("paced_recorder.py"),
                 Path(__file__).with_name("native_cleanup.py"),
                 Path(__file__).with_name("native_pair_probe.py"),
                 ROOT / "sidecars/duplexcascade_model.py", ROOT / "sidecars/duplexcascade_speech.py",
                 ROOT / "sidecars/microturn_sidecar.py", args.source / "model.py"]:
        data = path.read_bytes()
        hashes[str(path)] = hashlib.sha256(data).hexdigest()
        (retained / (path.parent.name + "__" + path.name + ".txt")).write_bytes(data)
    pair = json.loads(args.prepared_pair.read_text())
    (args.out / "prepared-pair.json").write_bytes(args.prepared_pair.read_bytes())
    report = {"scope": "native-grammar wall-clock development; no word alignment or semantic score",
              "source_sha256": hashes, "invocation": {k: str(v) for k, v in vars(args).items()},
              "words_per_tick": args.words_per_tick,
              "limitations": [f"delay-only {args.words_per_tick}-word delivery", "20ms canceled in-flight frame omitted",
                              "native control determines context cancellation", "no human ratings"]}
    (args.out / "manifest.json").write_text(json.dumps(report, indent=2) + "\n")
    stage = "model-load"
    try:
        backend = DuplexCascadeModel(args.source, args.snapshot, args.base, slow_tokenizer=True)
        report["model"] = backend.metadata
        # Keep cold CUDA/kernel setup outside the conversation clock and use an
        # isolated history. Record the warmup rather than silently dropping its cost.
        stage = "warmup"
        began = time.monotonic()
        warmup_tokens = backend.session().step("Hello")
        backend.torch.cuda.synchronize()
        report["warmup"] = {"input": "Hello", "generated_tokens": warmup_tokens,
                            "duration_s": time.monotonic() - began, "history": "disposable"}
        (args.out / "manifest.json").write_text(json.dumps(report, indent=2) + "\n")
        report["initialization_status"] = "ready"
        (args.out / "manifest.json").write_text(json.dumps(report, indent=2) + "\n")
        stage = "campaign"
        asyncio.run(run(args, backend, pair))
    except BaseException as exc:
        failure = {"status": "failed", "stage": stage, "error": repr(exc),
                   "capability_score": None,
                   "trials_started": stage == "campaign"}
        (args.out / "failure.json").write_text(json.dumps(failure, indent=2) + "\n")
        report["terminal_failure"] = failure
        (args.out / "manifest.json").write_text(json.dumps(report, indent=2) + "\n")
        raise



if __name__ == "__main__":
    main()
