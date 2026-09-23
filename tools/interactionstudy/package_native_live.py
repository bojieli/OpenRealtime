#!/usr/bin/env python3
"""Attach exact input channels and audit native paced-recorder sample receipts."""
import argparse
import hashlib
import json
from pathlib import Path
import struct
import wave


def read(path):
    with wave.open(str(path)) as wav:
        if wav.getnchannels() != 1 or wav.getsampwidth() != 2:
            raise ValueError("expected mono PCM16")
        return wav.getframerate(), wav.readframes(wav.getnframes())


def audit(marks, frames, rate):
    errors, last, played, epoch = [], 0, 0, 0
    generated, consumed = {}, {}
    peak_backlog, discarded = 0, []
    for mark in marks:
        kind = mark["kind"]
        e = mark["epoch"]
        if kind == "generated":
            generated[e] = generated.get(e, 0) + mark["samples"]
            peak_backlog = max(peak_backlog, generated[e] - consumed.get(e, 0))
        elif kind == "cancel":
            discarded.append({"epoch": e, "at_s": mark.get("at_s"),
                              "reason": mark.get("reason", "unknown"),
                              "unplayed_generated_samples": generated.get(e, 0) - consumed.get(e, 0)})
            epoch = max(epoch, e + 1)
        elif kind == "played":
            start, size = mark["start_sample"], mark["samples"]
            if e < epoch:
                errors.append("playback after epoch cancellation")
            if start < last or size <= 0 or start + size > frames:
                errors.append("overlapping or out-of-waveform playback receipt")
            # Sample conversion rounds to nearest, allowing half a sample.
            if (start + size - .5) / rate > mark["acknowledged_at_s"]:
                errors.append("playback acknowledged before sample-clock completion")
            consumed[e] = consumed.get(e, 0) + size
            if consumed[e] > generated.get(e, 0):
                errors.append("played more samples than generated")
            last = start + size
            played += size
    return {"violations": errors, "played_samples": played,
            "played_seconds": played / rate, "waveform_frames": frames,
            "generated_audio_seconds": sum(generated.values()) / rate,
            "peak_generated_audio_backlog_seconds": peak_backlog / rate,
            "cancellation_backlogs": discarded,
            "scope": "sample bounds, chronology, cancellation epochs and generated/played counts",
            "word_alignment": False}


def feedback_opportunity(pcm, rate, marks, onset_ns):
    """Quantify recorded output before the original feedback, including controls.

    Nonzero PCM and receipt overlap cannot establish substantive speech. Keep
    these measurements separate from the listening judgment and receipt audit.
    """
    if rate <= 0 or onset_ns < 0 or len(pcm) % 2:
        raise ValueError("invalid PCM or feedback onset")
    boundary = onset_ns * rate // 1_000_000_000
    samples = [value[0] for value in struct.iter_unpack("<h", pcm[:boundary * 2])]
    played = [m for m in marks if m["kind"] == "played"]
    return {
        "feedback_source_onset_ns": onset_ns,
        "anchor_scope": "original feedback onset, including muted controls",
        "first_played_s": min((m["start_sample"] / rate for m in played), default=None),
        "nonzero_samples_before_feedback": sum(value != 0 for value in samples),
        "pre_feedback_peak_pcm": max((abs(value) for value in samples), default=0),
        "pre_feedback_receipt_samples": sum(
            max(0, min(m["start_sample"] + m["samples"], boundary) - m["start_sample"])
            for m in played),
        "receipt_spans_feedback_onset": any(
            m["start_sample"] < boundary < m["start_sample"] + m["samples"] for m in played),
        "substantive_speech_underway": None,
        "scope": "paced recording samples; requires valid receipts and listening, not physical playback",
    }


def package(root, prepared, output):
    retained_pair = (root / "prepared-pair.json").read_bytes()
    if retained_pair != (prepared / "pair.json").read_bytes():
        raise ValueError("prepared fixture differs from run-retained fixture")
    pair = json.loads(retained_pair)
    output.mkdir()
    reports = []
    expected = {v["id"] + suffix for v in pair["variants"] for suffix in ("", "-nofeedback")}
    for result_path in sorted(root.glob("*/result.json")):
        name = result_path.parent.name
        if name not in expected:
            raise ValueError("unexpected branch: " + name)
        result = json.loads(result_path.read_text())
        original = name.removesuffix("-nofeedback")
        variant = next(v for v in pair["variants"] if v["id"] == original)
        input_path = prepared / (original + ".input.wav")
        rate, user = read(input_path)
        user = bytearray(user)
        if name != original:
            for e in variant["events"]:
                if e["kind"] == "sound" and e["group"] == variant["feedback"]:
                    start = e["source_start"] * rate // 1_000_000_000 * 2
                    end = e["source_end"] * rate // 1_000_000_000 * 2
                    if not 0 <= start <= end <= len(user):
                        raise ValueError("feedback outside input waveform")
                    user[start:end] = bytes(end - start)
        output_path = result_path.with_name("output.wav")
        out_rate, assistant = read(output_path)
        if out_rate != rate:
            raise ValueError("sample rate mismatch")
        frames = max(len(user), len(assistant)) // 2
        user.extend(bytes(frames * 2 - len(user)))
        assistant += bytes(frames * 2 - len(assistant))
        stereo = bytearray(frames * 4)
        for i in range(frames):
            stereo[4*i:4*i+2] = user[2*i:2*i+2]
            stereo[4*i+2:4*i+4] = assistant[2*i:2*i+2]
        with wave.open(str(output / (name + ".stereo.wav")), "wb") as wav:
            wav.setparams((2, 2, rate, 0, "NONE", "not compressed"))
            wav.writeframes(stereo)
        marks = json.loads(result_path.with_name("playback.json").read_text())
        actual_rate, actual_pcm = read(output_path)
        checked = audit(marks, len(actual_pcm)//2, actual_rate)
        onset = min(e["source_start"] for e in variant["events"]
                    if e["group"] == variant["feedback"])
        checked["feedback_opportunity"] = feedback_opportunity(actual_pcm, actual_rate, marks, onset)
        checked.update(variant=name, status=result.get("status", "unknown"),
                       execution_error=result.get("error"), cleanup_errors=result.get("cleanup_errors"),
                       feedback_muted=name != original,
                       input_sha256=hashlib.sha256(input_path.read_bytes()).hexdigest(),
                       output_sha256=hashlib.sha256(output_path.read_bytes()).hexdigest())
        reports.append(checked)
    (output / "audit.json").write_text(json.dumps({"branches": reports,
        "prepared_pair_sha256": hashlib.sha256(retained_pair).hexdigest(),
        "prepared_fixture_matches_run": True,
        "missing_branches": sorted(expected - {r["variant"] for r in reports}),
        "execution_complete": len(reports) == len(expected) and all(r["status"] == "complete" for r in reports),
        "channels": {"left": "retained user input", "right": "paced assistant output"},
        "capability_claim": None}, indent=2) + "\n")
    return reports


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("run", type=Path)
    p.add_argument("--prepared", type=Path, required=True)
    p.add_argument("--out", type=Path, required=True)
    args = p.parse_args()
    for report in package(args.run, args.prepared, args.out):
        print(json.dumps(report))
