"""Cell I1: can dual-channel forecasts tell an acknowledgment from a bid for the floor?

Events: FDB v1.5 ``user_backchannel`` (98; the agent should keep speaking)
and ``user_interruption`` (200; the agent should yield). The user channel is
the dataset's ``input.wav``; FDB ships **no assistant audio**, so this script
constructs the assistant channel that would have been *played*:

* the agent starts speaking ``--response-delay`` (0.8 s) after the user's
  request ends (VAD offset) and keeps speaking through the event, because
  until a policy decides to yield nothing has stopped playback. That is the
  exact counterfactual the hold/yield decision is about, and it never
  contains information from the future of the user channel;
* the speech is LibriSpeech test-clean read speech (a deterministic speaker
  per event, sentence gaps of 250 ms), RMS-matched to the user's request;
* both channels share one sample clock (no playout delay and no echo in the
  user channel, i.e. an ideal headset/AEC; real loudspeaker echo is untested).

Every predictor is evaluated causally: at each checkpoint (every 80 ms from
the VAD onset of the event up to 1.6 s after it) the model gets both channels
up to that instant only and nothing after it.

Declared rules (fixed before the run; thresholds also swept):
  * ``sustained-300ms`` - OpenRealtime's ``interaction.NewSustainedBargeIn``
    default: yield once user speech (Silero VAD) has been continuous for 300 ms.
  * ``vap-p_future`` - yield at the first checkpoint where VAP's next-speaker
    aggregate over 0.6-2.0 s says the user owns the floor (p_future_user >= 0.5).
  * ``dualturn-fvad960`` - yield at the first checkpoint where DualTurn's
    user voice-activity forecast at 960 ms is >= 0.5.
  * ``dualturn-fvad960-bcveto`` - as above, but not while the user backchannel
    head is >= 0.5.

Metrics: accuracy (keep on backchannel, yield on interruption within the
window), false-stop rate (yield during a backchannel), missed-interruption
rate (no yield within 1.6 s), decision latency from event onset for
detected interruptions. Smart Turn and LiveKit are reported separately on
the completed event (they answer "is the user's utterance complete?", which
is true for both classes); they are not overlap controllers.

Subcommands: ``prepare``, ``score`` (resumable, calls :9130), ``analyze``,
``check-causality``.
"""

from __future__ import annotations

import argparse
import concurrent.futures as futures
import hashlib
import io
import json
import os
import sys
import time

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from endpoint_eval import FDB15, RUNTIME, SR, load_audio, vad_probs, vad_segments, split_at_largest_gap  # noqa: E402

DEFAULT_WORK = os.path.join(RUNTIME, "duplex-plan", "results", "interaction", "i1")
LIBRI = os.path.join(RUNTIME, "duplex-plan", "data", "libri", "clean", "test", "0000.parquet")
STEP_S = 0.08
WINDOW_S = 1.6
CHECKS = [round(STEP_S * k, 2) for k in range(1, int(round(WINDOW_S / STEP_S)) + 1)]


def split_of(key: str) -> str:
    return "calibration" if int(hashlib.sha1(key.encode()).hexdigest(), 16) % 2 == 0 else "test"


def libri_speakers():
    import pyarrow.parquet as pq
    import soundfile as sf

    table = pq.read_table(LIBRI).to_pylist()
    by = {}
    for row in table:
        by.setdefault(row["speaker_id"], []).append(row)
    speakers = sorted(s for s, rows in by.items() if sum(len(r["text"]) for r in rows) > 2000)
    cache = {}

    def speech(speaker_index: int, seconds: float) -> np.ndarray:
        spk = speakers[speaker_index % len(speakers)]
        if spk not in cache:
            parts = []
            for row in sorted(by[spk], key=lambda r: r["id"]):
                audio, sr = sf.read(io.BytesIO(row["audio"]["bytes"]), dtype="float32")
                assert sr == SR
                parts.append(audio)
                parts.append(np.zeros(int(0.25 * SR), np.float32))
            cache[spk] = np.concatenate(parts)
        base = cache[spk]
        reps = int(np.ceil(seconds * SR / len(base))) + 1
        return np.tile(base, reps)[: int(seconds * SR)], spk

    return speech


def cmd_prepare(args) -> None:
    os.makedirs(args.work, exist_ok=True)
    speech = libri_speakers()
    events, stats = [], {"skipped": {}}
    for category, label in (("user_backchannel", 0), ("user_interruption", 1)):
        folder = os.path.join(FDB15, category)
        for name in sorted(os.listdir(folder), key=lambda s: int(s) if s.isdigit() else 10 ** 9):
            meta_path = os.path.join(folder, name, "metadata.json")
            if not os.path.exists(meta_path):
                continue
            meta = json.load(open(meta_path))
            user = load_audio(os.path.join(folder, name, "input.wav"))
            segments = vad_segments(vad_probs(user))
            split = split_at_largest_gap(segments)
            reason = None
            if split is None or split[0] < 1.5:
                reason = "no_request_event_gap"
            else:
                _, request, event = split
                onset = event[0][0]
                if abs(onset - float(meta["timestamps"][0])) > 1.5:
                    reason = "event_far_from_metadata"
                request_end = request[-1][1]
                agent_start = request_end + args.response_delay
                if reason is None and onset - agent_start < 1.0:
                    reason = "agent_not_speaking_1s_before_event"
            if reason:
                stats["skipped"][reason] = stats["skipped"].get(reason, 0) + 1
                continue
            key = f"{category}/{name}"
            spk_index = int(hashlib.sha1(key.encode()).hexdigest(), 16) % 1000
            need = len(user) / SR - agent_start + 0.1
            agent_speech, speaker = speech(spk_index, need)
            req_audio = user[int(request[0][0] * SR):int(request_end * SR)]
            gain = (np.sqrt(np.mean(req_audio ** 2)) + 1e-8) / (np.sqrt(np.mean(agent_speech[agent_speech != 0] ** 2)) + 1e-8)
            assistant = np.zeros_like(user)
            a0 = int(agent_start * SR)
            assistant[a0:a0 + len(agent_speech)] = (agent_speech * gain)[: len(user) - a0]
            np.save(os.path.join(args.work, f"assistant_{category}_{name}.npy"), assistant.astype(np.float32))
            event_end = event[-1][1]
            first_len = event[0][1] - event[0][0]
            events.append({
                "key": key, "category": category, "label": label, "split": split_of(key),
                "user_path": os.path.join(folder, name, "input.wav"),
                "assistant_path": os.path.join(args.work, f"assistant_{category}_{name}.npy"),
                "request_end": request_end, "agent_start": round(agent_start, 3), "onset": onset,
                "event_end": event_end, "event_segments": event, "event_first_segment_s": round(first_len, 3),
                "metadata_timestamps": meta["timestamps"], "libri_speaker": speaker,
                "event_text": meta.get("backchannel_text") or meta.get("current_turn_text"),
                "request_text": meta["context_text"],
            })
    stats["events"] = {c: sum(1 for e in events if e["category"] == c) for c in ("user_backchannel", "user_interruption")}
    stats["response_delay_s"] = args.response_delay
    json.dump(events, open(os.path.join(args.work, "events.json"), "w"), indent=1)
    json.dump(stats, open(os.path.join(args.work, "prepare_stats.json"), "w"), indent=2)
    print(json.dumps(stats, indent=2))


def post_stereo(session, url, route, user, assistant, **params):
    body = np.concatenate([user, assistant]).astype("<f4").tobytes()
    r = session.post(f"{url}/v1/forecast/{route}", params=params, data=body, timeout=120)
    r.raise_for_status()
    return r.json()


def cmd_score(args) -> None:
    import requests

    events = json.load(open(os.path.join(args.work, "events.json")))
    out_path = os.path.join(args.work, "scores.jsonl")
    seen = set()
    if os.path.exists(out_path):
        seen = {json.loads(line)["key"] for line in open(out_path)}
    todo = [e for e in events if e["key"] not in seen]
    health = requests.get(args.url + "/health", timeout=10).json()
    json.dump(health, open(os.path.join(args.work, "server_health_at_score.json"), "w"), indent=2)
    print(f"[score] {len(todo)} events ({len(seen)} cached)", flush=True)
    began = time.time()

    def work(event):
        session = requests.Session()
        user = load_audio(event["user_path"])
        assistant = np.load(event["assistant_path"])
        rec = {"key": event["key"], "vap": {}, "dualturn": {}, "latency_ms": {"vap": [], "dualturn": []}}
        for c in CHECKS:
            t = event["onset"] + c
            n = int(round(t * SR))
            if n > len(user):
                break
            v = post_stereo(session, args.url, "vap", user[:n], assistant[:n])
            rec["vap"][f"{c:.2f}"] = {"p_future_user": v["next_speaker"]["p_future"]["user"],
                                      "p_now_user": v["next_speaker"]["p_now"]["user"],
                                      "p_active_user": v["p_active"]["user"],
                                      "p_active_assistant": v["p_active"]["assistant"],
                                      "vad_user": v["voice_activity_now"]["user"]}
            rec["latency_ms"]["vap"].append(v["latency_ms"]["total"])
            d = post_stereo(session, args.url, "dualturn", user[:n], assistant[:n])
            rec["dualturn"][f"{c:.2f}"] = {
                "fvad_user": d["voice_activity_forecast"]["user"], "fvad_assistant": d["voice_activity_forecast"]["assistant"],
                "vad_user": d["voice_activity_now"]["user"], "vad_assistant": d["voice_activity_now"]["assistant"],
                "eot_user": d["turn_event_likelihood"]["eot"]["user"], "hold_user": d["turn_event_likelihood"]["hold"]["user"],
                "bot_user": d["turn_event_likelihood"]["bot"]["user"], "bc_user": d["turn_event_likelihood"]["backchannel"]["user"],
                "frame_end_s": d["frame_end_s"]}
            rec["latency_ms"]["dualturn"].append(d["latency_ms"]["total"])
        # Endpoint models on the completed event (not overlap controllers; reported separately)
        end = event["event_end"] + 0.2
        seg = user[int(max(0, event["onset"] - 0.1) * SR):int(end * SR)]
        r = session.post(f"{args.url}/v1/endpoint/smart-turn", data=seg.astype("<f4").tobytes(), timeout=60).json()
        rec["smart_turn_event_end"] = r["probability"]
        r = session.post(f"{args.url}/v1/endpoint/livekit", json={"messages": [
            {"role": "user", "content": event["request_text"]},
            {"role": "assistant", "content": "Sure, let me explain."},
            {"role": "user", "content": event["event_text"]}]}, timeout=60).json()
        rec["livekit_event_text"] = r["probability"]
        return rec

    count = 0
    with open(out_path, "a") as handle, futures.ThreadPoolExecutor(args.concurrency) as pool:
        it = iter(todo)
        pending = {pool.submit(work, e): e for e in [next(it, None) for _ in range(args.concurrency)] if e}
        while pending:
            done, _ = futures.wait(pending, return_when=futures.FIRST_COMPLETED)
            for fut in done:
                pending.pop(fut)
                handle.write(json.dumps(fut.result()) + "\n")
                handle.flush()
                count += 1
                if time.time() - began < args.budget:
                    nxt = next(it, None)
                    if nxt is not None:
                        pending[pool.submit(work, nxt)] = nxt
            if count and count % 20 == 0:
                print(f"[score] {count}/{len(todo)} {time.time() - began:.0f}s", flush=True)
    print(f"[score] wrote {count} in {time.time() - began:.0f}s", flush=True)


def cmd_score_external(args) -> None:
    """Single-channel whole-stream predictors on the user channel (they never see the assistant)."""
    import requests

    events = json.load(open(os.path.join(args.work, "events.json")))
    out_path = os.path.join(args.work, f"external_{args.model}.jsonl")
    seen = {json.loads(line)["key"] for line in open(out_path)} if os.path.exists(out_path) else set()
    session = requests.Session()
    json.dump(session.get(args.url + "/health", timeout=10).json(),
              open(os.path.join(args.work, f"server_health_{args.model}.json"), "w"), indent=2)
    began = time.time()
    todo = [e for e in events if e["key"] not in seen
            and int(hashlib.sha1(e["key"].encode()).hexdigest(), 16) % args.external_sample_mod == 0]
    print(f"[score-external] {args.model}: {len(todo)} events ({len(seen)} cached)", flush=True)
    with open(out_path, "a") as handle:
        for e in todo:
            if time.time() - began > args.budget:
                print("[score-external] budget reached; rerun to continue", flush=True)
                break
            user = load_audio(e["user_path"])
            user = user[: int(min(len(user) / SR, e["onset"] + WINDOW_S + 1.0) * SR)]
            body = user.astype("<f4").tobytes()
            if args.model == "x2turn":
                d = session.post(f"{args.url}/v1/turn-state/x2turn", params={"frames": 0}, data=body, timeout=600).json()
                rec = {"key": e["key"], "frame_ms": d["frame_ms"], "labels": [f["label"] for f in d["frames"]],
                       "p": {k: [f["p"][k] for f in d["frames"]] for k in
                             ("idle", "noidle", "speaking", "turn_end", "backchannel", "uncertain")},
                       "transcript": d["transcript"]}
            else:
                d = session.post(f"{args.url}/v1/turn-state/soulx", params={"every": 1}, data=body, timeout=600).json()
                rec = {"key": e["key"], "events": d["events"]}
            handle.write(json.dumps(rec) + "\n")
            handle.flush()


# --------------------------------------------------------------------------
# rules


def sustained_yield(event, hold_s):
    """Yield time (s after onset) for 'continuous VAD speech >= hold_s', or None within the window."""
    for s, e in event["event_segments"]:
        if e - s >= hold_s and s + hold_s - event["onset"] <= WINDOW_S + 1e-9:
            return round(s + hold_s - event["onset"], 3)
    return None


def first_check(rec, family, fn):
    for c in CHECKS:
        v = rec[family].get(f"{c:.2f}")
        if v is None:
            break
        if fn(v):
            return c
    return None


RULES = {
    "vap-p_future": lambda th: ("vap", lambda v: v["p_future_user"] >= th),
    "vap-p_active_bin2": lambda th: ("vap", lambda v: v["p_active_user"][2] >= th),
    "dualturn-fvad960": lambda th: ("dualturn", lambda v: v["fvad_user"][2] >= th),
    "dualturn-fvad960-bcveto": lambda th: ("dualturn", lambda v: v["fvad_user"][2] >= th and v["bc_user"] < 0.5),
}


def evaluate(events, recs, yield_fn):
    rows = []
    for e in events:
        rec = recs.get(e["key"])
        if rec is None:
            continue
        rows.append((e["label"], yield_fn(e, rec)))
    bc = [t for l, t in rows if l == 0]
    it = [t for l, t in rows if l == 1]
    false_stop = sum(t is not None for t in bc) / max(1, len(bc))
    missed = sum(t is None for t in it) / max(1, len(it))
    lat = np.array([t for t in it if t is not None], dtype=float)
    correct = sum(t is None for t in bc) + sum(t is not None for t in it)
    out = {"backchannels": len(bc), "interruptions": len(it), "accuracy": round(correct / max(1, len(rows)), 4),
           "balanced_accuracy": round(1 - 0.5 * (false_stop + missed), 4),
           "false_stop_rate": round(false_stop, 4), "missed_interruption_rate": round(missed, 4)}
    if len(lat):
        out.update({"latency_mean_ms": round(1000 * lat.mean(), 1), "latency_p50_ms": round(1000 * np.percentile(lat, 50), 1),
                    "latency_p90_ms": round(1000 * np.percentile(lat, 90), 1)})
    fs_bc = np.array([t for t in bc if t is not None], dtype=float)
    if len(fs_bc):
        out["false_stop_latency_p50_ms"] = round(1000 * np.percentile(fs_bc, 50), 1)
    return out


def cmd_analyze(args) -> None:
    from sklearn.metrics import roc_auc_score

    events = json.load(open(os.path.join(args.work, "events.json")))
    recs = {}
    for line in open(os.path.join(args.work, "scores.jsonl")):
        r = json.loads(line)
        recs[r["key"]] = r
    events = [e for e in events if e["key"] in recs]
    result = {"cell": "I1", "window_s": WINDOW_S, "step_s": STEP_S,
              "events": {"backchannel": sum(e["label"] == 0 for e in events),
                         "interruption": sum(e["label"] == 1 for e in events)},
              "assistant_channel": "LibriSpeech test-clean, starts response_delay after the request's VAD offset, "
                                   "continues through the event (not yet yielded), RMS-matched; no echo in user channel",
              "declared_rules": {}}
    declared = {
        "sustained-300ms (OpenRealtime default)": lambda e, r: sustained_yield(e, 0.3),
        "vap-p_future>=0.5": lambda e, r: first_check(r, *RULES["vap-p_future"](0.5)),
        "dualturn-fvad960>=0.5": lambda e, r: first_check(r, *RULES["dualturn-fvad960"](0.5)),
        "dualturn-fvad960>=0.5-bcveto": lambda e, r: first_check(r, *RULES["dualturn-fvad960-bcveto"](0.5)),
    }
    for name, fn in declared.items():
        result["declared_rules"][name] = evaluate(events, recs, fn)

    sweeps = {"sustained": [], **{k: [] for k in RULES}}
    for hold in [0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.8, 1.0, 1.2]:
        sweeps["sustained"].append({"hold_s": hold, **evaluate(events, recs, lambda e, r: sustained_yield(e, hold))})
    for name, make in RULES.items():
        for th in [0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9]:
            fam, fn = make(th)
            sweeps[name].append({"theta": th, **evaluate(events, recs, lambda e, r, fam=fam, fn=fn: first_check(r, fam, fn))})
    result["sweeps_all_events"] = sweeps

    external = {}
    x2_path, x2_timing = os.path.join(args.work, "external_x2turn.jsonl"), os.path.join(args.work, "x2turn_timing.json")
    if os.path.exists(x2_path) and os.path.exists(x2_timing):
        x2 = {json.loads(l)["key"]: json.loads(l) for l in open(x2_path)}
        delay = json.load(open(x2_timing))["available_after_frames"]

        def x2_yield(e, r, rule, th=0.5):
            d = x2.get(e["key"])
            if d is None:
                return None
            fs = d["frame_ms"] / 1000.0
            for f, label in enumerate(d["labels"]):
                avail = (f + 1 + delay) * fs
                if avail <= e["onset"] or f * fs < e["onset"] - 0.08:
                    continue
                if avail > e["onset"] + WINDOW_S + 1e-9:
                    break
                p = {k: d["p"][k][f] for k in d["p"]}
                hit = label in ("speaking", "turn_end") if rule == "label" else p["speaking"] + p["turn_end"] >= th
                if hit:
                    return round(avail - e["onset"], 3)
            return None

        ev_x2 = [e for e in events if e["key"] in x2]
        external["x2turn-label(speaking|turn_end) [upstream barge-in rule]"] = evaluate(
            ev_x2, recs, lambda e, r: x2_yield(e, r, "label"))
        external["x2turn-sweep"] = [{"theta": th, **evaluate(ev_x2, recs, lambda e, r, th=th: x2_yield(e, r, "p", th))}
                                    for th in (0.3, 0.5, 0.7, 0.9)]
    sx_path = os.path.join(args.work, "external_soulx.jsonl")
    if os.path.exists(sx_path):
        sx = {json.loads(l)["key"]: json.loads(l) for l in open(sx_path)}

        def sx_yield(e, r):
            d = sx.get(e["key"])
            if d is None:
                return None
            for ev in d["events"]:
                t = ev["available_s"]
                if t <= e["onset"]:
                    continue
                if t > e["onset"] + WINDOW_S + 1e-9:
                    break
                if ev["state"] in ("nonidle", "speak"):
                    return round(t - e["onset"], 3)
            return None

        external["soulx-nonidle [semantic VAD: backchannels map to idle]"] = evaluate(
            [e for e in events if e["key"] in sx], recs, sx_yield)
    result["single_channel_models_user_only"] = external

    # matched operating points: choose the parameter on the calibration half, report the test half
    cal = [e for e in events if e["split"] == "calibration"]
    test = [e for e in events if e["split"] == "test"]
    matched = {}
    for target in args.false_stop_targets:
        row = {}
        cands = {"sustained": [(h, lambda e, r, h=h: sustained_yield(e, h)) for h in
                               [x / 100 for x in range(8, 161, 4)]]}
        for name, make in RULES.items():
            cands[name] = [(th, (lambda fam, fn: (lambda e, r: first_check(r, fam, fn)))(*make(th)))
                           for th in [x / 100 for x in range(5, 100, 5)]]
        for name, options in cands.items():
            ok = [(p, fn, evaluate(cal, recs, fn)) for p, fn in options]
            ok = [x for x in ok if x[2]["false_stop_rate"] <= target]
            if not ok:
                continue
            best = min(ok, key=lambda x: (x[2]["missed_interruption_rate"], x[2].get("latency_mean_ms", 1e9)))
            row[name] = {"parameter": best[0], "calibration": best[2], "test": evaluate(test, recs, best[1])}
        matched[f"false_stop<={target}"] = row
    result["matched_on_calibration_reported_on_test"] = matched

    # Post-hoc (not declared before the run): decide once at a fixed latency instead of at the first
    # crossing, because both forecasts spike at every speech onset. Thresholds chosen on the calibration
    # half at a false-stop target, reported on the test half, next to the VAD rule at the same latency.
    fixed = {}
    for decide_at in (0.48, 0.8, 1.2):
        row = {}

        def at(family, key, idx):
            def yield_fn(e, r, family=family, key=key, idx=idx, decide_at=decide_at, th=0.5):
                v = r[family].get(f"{decide_at:.2f}")
                if v is None:
                    return None
                x = v[key] if idx is None else v[key][idx]
                return decide_at if x >= th else None
            return yield_fn

        for name, (family, key, idx) in {
                "vap.p_future_user": ("vap", "p_future_user", None),
                "dualturn.fvad_user[960ms]": ("dualturn", "fvad_user", 2),
                "dualturn.1-bc_user": ("dualturn", "bc_user", None)}.items():
            best = None
            for th in [x / 100 for x in range(5, 100, 5)]:
                def fn(e, r, family=family, key=key, idx=idx, th=th, decide_at=decide_at, name=name):
                    v = r[family].get(f"{decide_at:.2f}")
                    if v is None:
                        return None
                    x = v[key] if idx is None else v[key][idx]
                    if name.endswith("1-bc_user"):
                        x = 1 - x
                    return decide_at if x >= th else None
                cal_m = evaluate(cal, recs, fn)
                if cal_m["false_stop_rate"] <= args.false_stop_targets[0] and (
                        best is None or cal_m["missed_interruption_rate"] < best[1]["missed_interruption_rate"]):
                    best = (th, cal_m, fn)
            if best:
                row[name] = {"theta": best[0], "calibration": best[1], "test": evaluate(test, recs, best[2])}
        row["vad: still speaking at this instant"] = {
            "test": evaluate(test, recs, lambda e, r, decide_at=decide_at: decide_at if any(
                sg[0] <= e["onset"] + decide_at <= sg[1] for sg in e["event_segments"]) else None)}
        fixed[f"decide_at_{decide_at:.2f}s (false_stop<={args.false_stop_targets[0]} on calibration)"] = row
    result["post_hoc_fixed_time_decision"] = fixed

    # threshold-free separability at fixed times after onset
    labels = np.array([e["label"] for e in events])
    auc_rows = {}
    for c in [0.16, 0.32, 0.48, 0.8, 1.2, 1.6]:
        row = {}
        for fam, key, idx in [("vap", "p_future_user", None), ("vap", "p_active_user", 2),
                              ("dualturn", "fvad_user", 2), ("dualturn", "bc_user", None), ("dualturn", "bot_user", None)]:
            vals, labs = [], []
            for e, lab in zip(events, labels):
                v = recs[e["key"]][fam].get(f"{c:.2f}")
                if v is None:
                    continue
                x = v[key] if idx is None else v[key][idx]
                vals.append(x)
                labs.append(lab)
            if len(set(labs)) == 2:
                row[f"{fam}.{key}{'' if idx is None else f'[{idx}]'}"] = round(float(roc_auc_score(labs, vals)), 4)
        auc_rows[f"{c:.2f}s"] = row
    result["auc_interruption_vs_backchannel_by_time_after_onset"] = auc_rows
    st = [recs[e["key"]]["smart_turn_event_end"] for e in events]
    lk = [recs[e["key"]]["livekit_event_text"] for e in events]
    result["endpoint_models_on_completed_event"] = {
        "note": "scored 200 ms after the event ends; answers 'is the utterance complete', not hold/yield",
        "smart_turn_auc_interruption_vs_backchannel": round(float(roc_auc_score(labels, st)), 4),
        "smart_turn_mean_p": {"backchannel": round(float(np.mean([s for s, l in zip(st, labels) if l == 0])), 4),
                              "interruption": round(float(np.mean([s for s, l in zip(st, labels) if l == 1])), 4)},
        "livekit_auc_interruption_vs_backchannel": round(float(roc_auc_score(labels, lk)), 4),
        "livekit_mean_p": {"backchannel": round(float(np.mean([s for s, l in zip(lk, labels) if l == 0])), 4),
                           "interruption": round(float(np.mean([s for s, l in zip(lk, labels) if l == 1])), 4)},
        "event_duration_s": {"backchannel_mean": round(float(np.mean([e["event_end"] - e["onset"] for e in events if e["label"] == 0])), 3),
                             "interruption_mean": round(float(np.mean([e["event_end"] - e["onset"] for e in events if e["label"] == 1])), 3)},
    }
    lat = {k: [x for r in recs.values() for x in r["latency_ms"][k]] for k in ("vap", "dualturn")}
    result["server_latency_ms"] = {k: {"n": len(v), "p50": float(np.percentile(v, 50)), "p90": float(np.percentile(v, 90))}
                                   for k, v in lat.items() if v}
    out = os.path.join(args.work, "i1_results.json")
    json.dump(result, open(out, "w"), indent=2)
    print(json.dumps({k: result[k] for k in ("events", "declared_rules", "single_channel_models_user_only",
                                             "matched_on_calibration_reported_on_test", "post_hoc_fixed_time_decision",
                                             "auc_interruption_vs_backchannel_by_time_after_onset",
                                             "endpoint_models_on_completed_event", "server_latency_ms")}, indent=1))
    print("wrote", out)


def cmd_check_causality(args) -> None:
    """Compare frames=K from one long call against per-instant truncated calls."""
    import requests

    events = json.load(open(os.path.join(args.work, "events.json")))
    session = requests.Session()
    report = {}
    for e in events[: args.n] + events[-args.n:]:
        user = load_audio(e["user_path"])
        assistant = np.load(e["assistant_path"])
        t_end = e["onset"] + 1.6
        n_end = int(round(t_end * SR))
        for route, hz in (("vap", 50), ("dualturn", 12.5)):
            frames = int(1.6 * hz)
            full = post_stereo(session, args.url, route, user[:n_end], assistant[:n_end], frames=frames)
            if route == "vap":
                series, last = full["next_speaker"]["p_future"]["user"], t_end
            else:
                series, last = [x[2] for x in full["voice_activity_forecast"]["user"]], full["frame_end_s"]
            diffs = []
            for j in range(0, frames, max(1, frames // 8)):
                t = last - (frames - 1 - j) / hz
                # truncate on a whole model frame, otherwise the comparison is off by up to one frame
                n = int(round(t * SR / (SR / hz))) * int(SR / hz)
                one = post_stereo(session, args.url, route, user[:n], assistant[:n])
                val = one["next_speaker"]["p_future"]["user"] if route == "vap" else one["voice_activity_forecast"]["user"][2]
                diffs.append(abs(val - series[j]))
            report.setdefault(route, []).append(max(diffs))
    summary = {k: {"events": len(v), "max_abs_diff": float(np.max(v)), "median_of_max": float(np.median(v)),
                   "note": "one long call with frames=K vs one truncated call per frame boundary; both are fed "
                           "only audio up to that instant, so a difference is model lookahead or an edge effect, "
                           "not leaked future"}
               for k, v in report.items()}
    json.dump(summary, open(os.path.join(args.work, "causality_check.json"), "w"), indent=2)
    print(json.dumps(summary, indent=2))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("command", choices=["prepare", "score", "score-external", "analyze", "check-causality"])
    parser.add_argument("--model", default="x2turn", choices=["x2turn", "soulx"])
    parser.add_argument("--external-sample-mod", type=int, default=1,
                        help="score-external: keep events whose hash %% N == 0 (these models are slow)")
    parser.add_argument("--work", default=DEFAULT_WORK)
    parser.add_argument("--url", default="http://127.0.0.1:9130")
    parser.add_argument("--response-delay", type=float, default=0.8)
    parser.add_argument("--concurrency", type=int, default=2)
    parser.add_argument("--budget", type=float, default=540.0)
    parser.add_argument("--false-stop-targets", type=float, nargs="+", default=[0.1, 0.2])
    parser.add_argument("--n", type=int, default=3)
    args = parser.parse_args()
    {"prepare": cmd_prepare, "score": cmd_score, "score-external": cmd_score_external, "analyze": cmd_analyze,
     "check-causality": cmd_check_causality}[args.command](args)


if __name__ == "__main__":
    main()
