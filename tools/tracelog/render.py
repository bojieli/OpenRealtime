#!/usr/bin/env python3
"""Render a server debug log as one timeline per session.

The server writes every runtime debug event to its log at debug level when
started with `-log-level debug -log-format json`. That is the complete story
of a turn - each transcript revision the recogniser produced, the choice the
interaction policy made on it, what the model wrote, what was synthesised -
but as one JSON object per line it is not readable. This prints it as one line
per event in arrival order, so "why did it speak there" can be answered by
reading down the page.

    tools/tracelog/render.py .runtime/latest-deploy/companion.log
    tools/tracelog/render.py --session sess_000000000003 companion.log
    journalctl --user -u openrealtime-cloudflare -o cat | tools/tracelog/render.py -

Nothing here interprets the events; it only lays them out.

A server started with `-timeline-log <file>` writes the same story directly,
one plain line per event, without needing this renderer.
"""
import argparse
import json
import sys


def event_of(line):
    start = line.find("{")
    if start < 0:
        return None
    try:
        record = json.loads(line[start:])
    except ValueError:
        return None
    if record.get("msg") != "debug event":
        return None
    return record


def describe(record):
    name = record.get("name", "")
    payload = record.get("payload") or {}
    value = payload.get("value") if isinstance(payload, dict) else None
    value = value if isinstance(value, dict) else {}
    if name == "transcript_events":
        kind = "final" if value.get("final") else "partial"
        return "ASR   ", f"{kind:<7} rev={value.get('revision')!s:<3} {value.get('text', '')!r}"
    if name == "observation_events":
        return "OBS   ", f"{value.get('observer', '')} {value.get('text', '')!r}"
    if name == "semantic_decision":
        return "POLICY", (
            f"event={value.get('event') or '-':<7} choice={value.get('choice')!s:<11} "
            f"stage={value.get('decision_stage')} confidence={value.get('confidence')} "
            f"stream={str(value.get('stream_id', ''))[-12:]} rev={value.get('source_revision')}"
        )
    if name == "semantic_admission_outcome":
        return "ADMIT ", f"{value.get('kind')} {value.get('code', '')} event={value.get('event') or '-'}"
    if name == "model_result":
        text = value.get("assistant_text") or value.get("AssistantText") or ""
        return "MODEL ", f"{text!r}"
    if name == "model_outcome":
        return "MODEL ", f"outcome {value.get('kind', '')} {value.get('code', '')}"
    if name == "tts.utterance_started":
        return "TTS   ", f"{(payload.get('text') if isinstance(payload, dict) else '')!r}"
    if name == "speech.word_timing_failed":
        return "TIMING", f"failed: {(record.get('attributes') or {}).get('error', '')}"
    if name == "segmentation_outcome":
        return "SEGMT ", f"{value.get('kind', '')} {value.get('code', '')} segments={value.get('segments')}"
    if name.startswith("gateway_speech") or name.startswith("gateway_turn"):
        return "GATE  ", name
    return "      ", name


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("log", help="log file, or - for stdin")
    parser.add_argument("--session", help="only this session id")
    parser.add_argument("--all", action="store_true", help="include every event, not only the turn's story")
    arguments = parser.parse_args()
    source = sys.stdin if arguments.log == "-" else open(arguments.log, encoding="utf-8", errors="replace")
    story = {"transcript_events", "observation_events", "semantic_decision", "semantic_admission_outcome",
             "model_result", "model_outcome", "tts.utterance_started", "speech.word_timing_failed",
             "segmentation_outcome"}
    current = None
    for line in source:
        record = event_of(line)
        if record is None:
            continue
        session = record.get("session", "")
        if arguments.session and session != arguments.session:
            continue
        if not arguments.all and record.get("name") not in story:
            continue
        if session != current:
            print(f"\n== session {session} ==")
            current = session
        label, detail = describe(record)
        print(f"{record.get('time', '')[11:23]}  {label}  {detail}")


if __name__ == "__main__":
    main()
