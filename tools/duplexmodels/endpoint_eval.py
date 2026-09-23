"""Cell I0: acoustic (Smart Turn) vs transcript (LiveKit) vs calibrated fusion endpointing.

Question (plan section 9, cell I0): what do acoustic and semantic completion
evidence each contribute to the decision "has the user finished the turn?".

Decision points. A fixed VAD front end (Silero v6 as bundled with
faster-whisper, 32 ms windows, onset p>=0.5 / offset p<0.35, segments shorter
than 64 ms dropped) finds every silence onset. A silence that lasts 200 ms
creates a decision point; every predictor is then re-asked at fixed silence
checkpoints (0.2 ... 1.5 s after the onset) for as long as the silence lasts,
and a pure 2.0 s silence timer is the common fallback. Held fixed across
predictors: VAD, checkpoints, fallback, audio, transcripts, turn start.

Ground truth. A pause is labelled only where the dataset states the turn
structure (see ``label_pauses``):
  * FD-Bench (``fdbench/*``): the per-conversation ``.timestamps`` give every
    user turn. They are **16 kHz sample indices although the wavs are 24 kHz**
    (checked against the VAD on every file; files whose turns do not line up
    are dropped). The last speech offset of a turn is an endpoint; a pause
    whose next speech is inside the same turn is a hold. Reference text per
    round comes from ``upstream/tts-generation/conversation_round.json``.
  * FDB v3 (``fdbv3``): every file is one disfluent user turn (metadata has a
    single dialogue entry), so every internal pause is a hold and the final
    offset is the endpoint. Reference text is the scripted ``user`` string.
  * FDB v1.5 (``fdbv15-interruption``, ``fdbv15-backchannel``): the user's
    request, a long gap (where the agent was expected to speak), then the
    interruption or backchannel. Turns are split at the largest gap and
    checked against ``metadata.json`` timestamps (which lead the audio, so they
    are used only as a +-1.5 s sanity check). The request's final offset and the
    interruption's final offset are endpoints; internal pauses are holds; the
    backchannel itself is not scored here (that is cell I1).
Pauses that cannot be attributed (VAD speech outside every turn, a turn whose
last word the VAD missed) are counted and excluded.

Transcripts. The text model gets the reference transcript of the current turn
up to the pause: reference words are aligned to faster-whisper
(large-v3-turbo) word timestamps with difflib, and a word is included when its
aligned midpoint precedes the silence onset. ``--text asr`` instead uses the
raw offline Whisper words (full-context, so better than a streaming prefix).
FD-Bench adds the earlier rounds (user and scripted assistant text) as
history; the other sources have no assistant text.

Fusion. Logistic regression on [logit P_smart, logit P_livekit], fitted on a
calibration split (half of the files of every source, by hash of the file
id) using the 200 ms checkpoint, reported only on the test split. Single
models are compared at matched premature-endpoint rates chosen on the
calibration split; ROC-AUC is threshold-free.

Subcommands: ``prepare`` (VAD, labels), ``transcribe`` (Whisper word timings;
GPU), ``score`` (calls turn_server on :9130; resumable), ``analyze``.
Every stage caches under ``--work`` (default
``.runtime/duplex-plan/results/interaction/i0``).
"""

from __future__ import annotations

from x2_evidence import causal_timing

import argparse
import concurrent.futures as futures
import difflib
import hashlib
import json
import math
import os
import re
import sys
import time
from dataclasses import dataclass, field
from typing import Optional

import numpy as np

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
RUNTIME = os.path.join(REPO, ".runtime")
FDBENCH = os.path.join(RUNTIME, "fd-bench")
FDB15 = os.path.join(RUNTIME, "full-duplex-bench-v1.5", "dataset")
FDB3 = os.path.join(RUNTIME, "full-duplex-bench-v3", "dataset", "fdb_v3_data_released")
DEFAULT_WORK = os.path.join(RUNTIME, "duplex-plan", "results", "interaction", "i0")
SR = 16000

CHECKPOINTS = [0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 1.0, 1.2, 1.5]
TEXT_LAGS = [0.3, 0.6]  # simulated streaming-ASR lag for the transcript prefix
MONO_CHECKPOINTS = [0.2]  # VAP / DualTurn endpoint maps: single-shot (200 ms) comparison only
FALLBACK_S = 2.0
VAD_WINDOW = 512


# --------------------------------------------------------------------------
# audio + VAD (shared with overlap_eval.py)


def load_audio(path: str) -> np.ndarray:
    import soundfile as sf

    audio, sr = sf.read(path, dtype="float32", always_2d=True)
    audio = audio.mean(axis=1)
    if sr != SR:
        import librosa

        audio = librosa.resample(audio, orig_sr=sr, target_sr=SR).astype(np.float32)
    return audio


_VAD = None


def vad_probs(audio: np.ndarray) -> np.ndarray:
    global _VAD
    if _VAD is None:
        from faster_whisper.vad import get_vad_model

        _VAD = get_vad_model()
    n = len(audio) // VAD_WINDOW * VAD_WINDOW
    if n == 0:
        return np.zeros(0, np.float32)
    return _VAD(audio[:n]).reshape(-1)


def vad_segments(probs: np.ndarray, on: float = 0.5, off: float = 0.35, min_speech: float = 0.064):
    """Speech segments [(start_s, end_s)] from 32 ms Silero probabilities."""
    step = VAD_WINDOW / SR
    segments, active, start = [], False, 0.0
    for i, p in enumerate(probs):
        t = i * step
        if not active and p >= on:
            active, start = True, t
        elif active and p < off:
            active = False
            if t - start >= min_speech:
                segments.append((round(start, 3), round(t, 3)))
    if active:
        segments.append((round(start, 3), round(len(probs) * step, 3)))
    return segments


def file_split(file_id: str) -> str:
    return "calibration" if int(hashlib.sha1(file_id.encode()).hexdigest(), 16) % 2 == 0 else "test"


# --------------------------------------------------------------------------
# datasets


@dataclass
class Turn:
    start: float
    end: float
    text: str
    history: list = field(default_factory=list)  # [{"role","content"}] before this turn


@dataclass
class Item:
    source: str
    file_id: str
    path: str
    turns: list
    duration: float = 0.0


def fdbench_items(conditions: list[str], per_condition: int) -> list[Item]:
    rounds = json.load(open(os.path.join(FDBENCH, "upstream", "tts-generation", "conversation_round.json")))
    items = []
    for condition in conditions:
        folder = os.path.join(FDBENCH, "dataset", condition)
        ids = sorted(int(f.split("_")[1].split(".")[0]) for f in os.listdir(folder) if f.endswith(".timestamps"))
        ids = sorted(ids, key=lambda i: hashlib.sha1(f"{condition}:{i}".encode()).hexdigest())[:per_condition]
        for cid in sorted(ids):
            meta = rounds.get(str(cid))
            if meta is None:
                continue
            user = [t.strip() for t in re.split(r"<[^>]+>", meta["user"]) if t.strip()]
            ai = [t.strip() for t in re.split(r"<[^>]+>", meta["ai"]) if t.strip()]
            stamps = json.load(open(os.path.join(folder, f"conversation_{cid}.timestamps")))
            if len(user) != len(stamps):
                continue
            turns, history = [], []
            for k, stamp in enumerate(stamps):
                # .timestamps are 16 kHz sample indices (the wav is 24 kHz); verified per file in prepare
                turns.append(Turn(stamp["start"] / 16000.0, stamp["end"] / 16000.0, user[k], list(history)))
                history.append({"role": "user", "content": user[k]})
                if k < len(ai):
                    history.append({"role": "assistant", "content": ai[k]})
            short = condition.replace("-single-round-combine", "")
            items.append(Item(f"fdbench/{short}", f"{condition}/{cid}", os.path.join(folder, f"conversation_{cid}.wav"),
                              turns))
    return items


def fdb3_items() -> list[Item]:
    items = []
    for name in sorted(os.listdir(FDB3)):
        meta_path = os.path.join(FDB3, name, "metadata.json")
        if not os.path.exists(meta_path):
            continue
        meta = json.load(open(meta_path))
        if len(meta["dialogue"]) != 1:
            continue
        # turn bounds are filled from the VAD in prepare (single user turn per file)
        items.append(Item("fdbv3", name, os.path.join(FDB3, name, "input.wav"),
                          [Turn(-1.0, -1.0, meta["dialogue"][0]["user"], [])]))
    return items


def fdb15_items(category: str) -> list[Item]:
    items = []
    folder = os.path.join(FDB15, category)
    for name in sorted(os.listdir(folder), key=lambda s: int(s) if s.isdigit() else 10**9):
        meta_path = os.path.join(folder, name, "metadata.json")
        if not os.path.exists(meta_path):
            continue
        meta = json.load(open(meta_path))
        turns = [Turn(-1.0, -1.0, meta["context_text"], [])]
        if category == "user_interruption":
            turns.append(Turn(-1.0, -1.0, meta["current_turn_text"], []))
        else:
            turns.append(Turn(-1.0, -1.0, meta.get("backchannel_text", ""), []))
        item = Item(f"fdbv15-{category.split('_')[1]}", f"{category}/{name}", os.path.join(folder, name, "input.wav"),
                    turns)
        item.event_s = float(meta["timestamps"][0])  # type: ignore[attr-defined]
        items.append(item)
    return items


# --------------------------------------------------------------------------
# labelling


def split_at_largest_gap(segments):
    gaps = [(segments[i + 1][0] - segments[i][1], i) for i in range(len(segments) - 1)]
    if not gaps:
        return None
    gap, i = max(gaps)
    return gap, segments[: i + 1], segments[i + 1:]


def label_pauses(item: Item, segments, duration: float, stats: dict) -> list[dict]:
    """Return decision points with labels; mutates item.turns bounds where the dataset lacks them."""
    if not segments:
        stats["no_speech"] = stats.get("no_speech", 0) + 1
        return []
    scored_turns = list(range(len(item.turns)))
    if item.source == "fdbv3":
        item.turns[0].start, item.turns[0].end = segments[0][0], segments[-1][1]
    elif item.source.startswith("fdbv15"):
        split = split_at_largest_gap(segments)
        event_s = getattr(item, "event_s", None)
        if split is None or split[0] < 1.5 or abs(split[2][0][0] - event_s) > 1.5:
            stats["v15_structure_mismatch"] = stats.get("v15_structure_mismatch", 0) + 1
            return []
        _, first, second = split
        item.turns[0].start, item.turns[0].end = first[0][0], first[-1][1]
        item.turns[1].start, item.turns[1].end = second[0][0], second[-1][1]
        if item.source == "fdbv15-backchannel":
            scored_turns = [0]
    else:  # fdbench: verify the 16 kHz-sample interpretation against the VAD
        speech_in_turns = 0.0
        speech_total = sum(e - s for s, e in segments)
        for s, e in segments:
            for turn in item.turns:
                speech_in_turns += max(0.0, min(e, turn.end + 0.1) - max(s, turn.start - 0.1))
        if item.turns[-1].end > duration + 0.05 or speech_total <= 0 or speech_in_turns / speech_total < 0.9:
            stats["fdbench_timestamp_mismatch"] = stats.get("fdbench_timestamp_mismatch", 0) + 1
            return []

    def turn_of(seg):
        mid = 0.5 * (seg[0] + seg[1])
        for k, turn in enumerate(item.turns):
            if turn.start - 0.3 <= mid <= turn.end + 0.3:
                return k
        return None

    points = []
    for i, seg in enumerate(segments):
        onset = seg[1]
        nxt = segments[i + 1][0] if i + 1 < len(segments) else duration
        pause = nxt - onset
        if pause < CHECKPOINTS[0]:
            continue
        k = turn_of(seg)
        if k is None:
            stats["pause_outside_turns"] = stats.get("pause_outside_turns", 0) + 1
            continue
        if k not in scored_turns:
            continue
        turn = item.turns[k]
        same_turn_next = i + 1 < len(segments) and turn_of(segments[i + 1]) == k
        if same_turn_next:
            label = 0
        elif onset >= turn.end - 0.35:
            label = 1
        else:
            stats["pause_ambiguous"] = stats.get("pause_ambiguous", 0) + 1
            continue
        points.append({
            "source": item.source, "file_id": item.file_id, "split": file_split(item.file_id), "turn": k,
            "turn_start": round(turn.start, 3), "turn_end": round(turn.end, 3), "onset": onset,
            "pause": round(pause, 3) if i + 1 < len(segments) else None, "label": label,
        })
    return points


# --------------------------------------------------------------------------
# stages


def default_items(args) -> list[Item]:
    items = []
    if "fdbench" in args.sources:
        items += fdbench_items(args.fdbench_conditions.split(","), args.fdbench_per_condition)
    if "fdbv3" in args.sources:
        items += fdb3_items()
    if "fdbv15" in args.sources:
        items += fdb15_items("user_interruption") + fdb15_items("user_backchannel")
    return items


def cmd_prepare(args) -> None:
    os.makedirs(args.work, exist_ok=True)
    items = default_items(args)
    stats: dict = {"files": len(items)}
    points, manifest = [], []
    began = time.time()
    for n, item in enumerate(items):
        audio = load_audio(item.path)
        duration = len(audio) / SR
        probs = vad_probs(audio)
        segments = vad_segments(probs)
        file_points = label_pauses(item, segments, duration, stats)
        points += file_points
        manifest.append({"source": item.source, "file_id": item.file_id, "path": item.path, "duration": duration,
                         "segments": segments, "split": file_split(item.file_id),
                         "turns": [{"start": t.start, "end": t.end, "text": t.text, "history": t.history}
                                   for t in item.turns], "scored": bool(file_points)})
        if n % 50 == 0:
            print(f"[prepare] {n}/{len(items)} files, {len(points)} points, {time.time() - began:.0f}s", flush=True)
    by = {}
    for p in points:
        key = (p["source"], p["label"])
        by[key] = by.get(key, 0) + 1
    stats["points"] = {f"{s}:{'endpoint' if l else 'hold'}": c for (s, l), c in sorted(by.items())}
    json.dump(manifest, open(os.path.join(args.work, "manifest.json"), "w"))
    with open(os.path.join(args.work, "points.jsonl"), "w") as handle:
        for p in points:
            handle.write(json.dumps(p) + "\n")
    json.dump(stats, open(os.path.join(args.work, "prepare_stats.json"), "w"), indent=2)
    print(json.dumps(stats, indent=2))


def norm_word(w: str) -> str:
    return re.sub(r"[^a-z0-9']", "", w.lower())


def align_reference(reference: str, words: list[dict]) -> list[dict]:
    """Give every reference word a time by aligning it to Whisper words (difflib), interpolating gaps."""
    ref = reference.split()
    if not ref:
        return []
    rn = [norm_word(w) for w in ref]
    an = [norm_word(w["word"]) for w in words]
    times = [None] * len(ref)
    matcher = difflib.SequenceMatcher(None, rn, an, autojunk=False)
    for block in matcher.get_matching_blocks():
        for k in range(block.size):
            w = words[block.b + k]
            times[block.a + k] = (w["start"], w["end"])
    known = [i for i, t in enumerate(times) if t is not None]
    out = []
    for i, word in enumerate(ref):
        if times[i] is None:
            if not known:
                t = (math.nan, math.nan)
            else:
                before = [j for j in known if j < i]
                after = [j for j in known if j > i]
                if before and after:
                    a, b = before[-1], after[0]
                    frac = (i - a) / (b - a)
                    s = times[a][1] + frac * (times[b][0] - times[a][1])
                    t = (s, s)
                elif before:
                    t = (times[before[-1]][1], times[before[-1]][1])
                else:
                    t = (times[after[0]][0], times[after[0]][0])
        else:
            t = times[i]
        out.append({"word": word, "start": t[0], "end": t[1], "matched": times[i] is not None})
    return out


def cmd_transcribe(args) -> None:
    from faster_whisper import WhisperModel

    manifest = json.load(open(os.path.join(args.work, "manifest.json")))
    out_path = os.path.join(args.work, args.words_file)
    done = json.load(open(out_path)) if os.path.exists(out_path) else {}
    model = WhisperModel(args.whisper_model, device=args.whisper_device, compute_type=args.whisper_compute,
                         cpu_threads=args.whisper_threads)
    began = time.time()
    todo = [m for m in manifest if m["scored"] and m["file_id"] not in done]
    for n, entry in enumerate(todo):
        audio = load_audio(entry["path"])
        per_turn = []
        for turn in entry["turns"]:
            if turn["start"] < 0:
                per_turn.append([])
                continue
            a = max(0.0, turn["start"] - 0.3)
            b = min(len(audio) / SR, turn["end"] + 0.3)
            segments, _ = model.transcribe(audio[int(a * SR):int(b * SR)], language="en", word_timestamps=True,
                                           condition_on_previous_text=False, vad_filter=False, beam_size=5)
            words = []
            for seg in segments:
                for w in seg.words or []:
                    words.append({"word": w.word.strip(), "start": round(w.start + a, 3), "end": round(w.end + a, 3)})
            per_turn.append(words)
        done[entry["file_id"]] = per_turn
        if n % 25 == 0 or n == len(todo) - 1:
            json.dump(done, open(out_path, "w"))
            print(f"[transcribe] {n + 1}/{len(todo)} {time.time() - began:.0f}s", flush=True)
        if time.time() - began > args.budget:
            json.dump(done, open(out_path, "w"))
            print("[transcribe] budget reached; rerun to continue", flush=True)
            return
    json.dump(done, open(out_path, "w"))


def prefix_texts(point: dict, entry: dict, words: list) -> dict:
    turn = entry["turns"][point["turn"]]
    asr = words[point["turn"]] if words else []
    onset = point["onset"]
    aligned = align_reference(turn["text"], asr)
    def prefix(words, cutoff):
        return " ".join(w["word"] for w in words
                        if not math.isnan(w["start"]) and 0.5 * (w["start"] + w["end"]) < cutoff)

    texts = {"reference": prefix(aligned, onset), "asr": prefix(asr, onset), "reference_full": turn["text"],
             "matched_fraction": (sum(w["matched"] for w in aligned) / len(aligned)) if aligned else 0.0}
    # a streaming recogniser has not delivered the last few hundred ms of words at the pause; these
    # lagged prefixes bound how much that costs the text-only classifier
    for lag in TEXT_LAGS:
        texts[f"reference_lag{int(lag * 1000)}"] = prefix(aligned, onset - lag)
    return texts


class Client:
    def __init__(self, url: str):
        import requests

        self.url = url.rstrip("/")
        self.session = requests.Session()

    def smart_turn(self, audio: np.ndarray, variant: Optional[str] = None) -> dict:
        params = {"variant": variant} if variant else None
        r = self.session.post(f"{self.url}/v1/endpoint/smart-turn", params=params,
                              data=audio.astype("<f4").tobytes(), timeout=60)
        r.raise_for_status()
        return r.json()

    def forecast(self, route: str, user: np.ndarray, assistant: np.ndarray, frames: int) -> dict:
        body = np.concatenate([user, assistant]).astype("<f4").tobytes()
        r = self.session.post(f"{self.url}/v1/forecast/{route}", params={"frames": frames}, data=body, timeout=120)
        r.raise_for_status()
        return r.json()

    def livekit(self, messages: list, language: str = "en") -> dict:
        r = self.session.post(f"{self.url}/v1/endpoint/livekit", params={"score": "raw"},
                              json={"messages": messages, "language": language}, timeout=60)
        r.raise_for_status()
        return r.json()


def cmd_score(args) -> None:
    """--part smart: Smart Turn at every checkpoint; --part text: LiveKit on the transcript prefixes."""
    manifest = {m["file_id"]: m for m in json.load(open(os.path.join(args.work, "manifest.json")))}
    points = [json.loads(line) for line in open(os.path.join(args.work, "points.jsonl"))]
    words_all = json.load(open(os.path.join(args.work, args.words_file))) if args.part == "text" else {}
    out_path = os.path.join(args.work, f"scores_{args.part}.jsonl")
    seen = set()
    if os.path.exists(out_path):
        for line in open(out_path):
            rec = json.loads(line)
            seen.add((rec["file_id"], rec["onset"]))
    health = Client(args.url).session.get(args.url + "/health", timeout=10).json()
    json.dump(health, open(os.path.join(args.work, f"server_health_score_{args.part}.json"), "w"), indent=2)
    todo = [p for p in points if (p["file_id"], p["onset"]) not in seen
            and (args.part != "text" or p["file_id"] in words_all)
            and (args.part != "mono" or int(hashlib.sha1(f"{p['file_id']}@{p['onset']}".encode()).hexdigest(), 16)
                 % args.mono_sample_mod == 0)]
    print(f"[score:{args.part}] {len(todo)} points to score ({len(seen)} cached)", flush=True)
    audio_cache: dict = {}
    began = time.time()

    def work(point):
        client = Client(args.url)
        entry = manifest[point["file_id"]]
        key = {"file_id": point["file_id"], "onset": point["onset"]}
        if args.part == "smart":
            if point["file_id"] not in audio_cache:
                audio_cache[point["file_id"]] = load_audio(entry["path"])
            audio = audio_cache[point["file_id"]]
            start = max(0.0, point["turn_start"] - 0.1)  # the current turn, as upstream recommends
            smart, lat, provider = {}, [], None
            for s in CHECKPOINTS:
                if point["pause"] is not None and s >= point["pause"]:
                    break
                r = client.smart_turn(audio[int(start * SR):int((point["onset"] + s) * SR)], args.smart_variant)
                smart[f"{s:.1f}"] = r["probability"]
                lat.append(r["latency_ms"]["total"])
                provider = r["runtime"]["provider"]
            return {**key, "smart_turn": smart, "smart_turn_latency_ms": lat, "smart_turn_provider": provider}
        if args.part == "mono":
            # VAP / DualTurn as endpoint classifiers through the endpoint-hook contract: the last <= 8 s of
            # user audio ending at the checkpoint, assistant channel silent (same maps as turn_server.py)
            if point["file_id"] not in audio_cache:
                audio_cache[point["file_id"]] = load_audio(entry["path"])
            audio = audio_cache[point["file_id"]]
            out = {}
            for s in MONO_CHECKPOINTS:
                if point["pause"] is not None and s >= point["pause"]:
                    break
                end = point["onset"] + s
                user = audio[int(max(0.0, end - 8.0) * SR):int(end * SR)]
                silent = np.zeros_like(user)
                v = client.forecast("vap", user, silent, 1)
                d = client.forecast("dualturn", user, silent, 5)
                eot = max(d["turn_event_likelihood"]["eot"]["user"])
                hold = max(d["turn_event_likelihood"]["hold"]["user"])
                out[f"{s:.1f}"] = {
                    "vap_p_now_assistant": v["next_speaker"]["p_now"]["assistant"],
                    "vap_p_future_assistant": v["next_speaker"]["p_future"]["assistant"],
                    "vap_p_active_user": v["p_active"]["user"],
                    "dualturn_eot_over_eot_hold": eot / (eot + hold) if eot + hold > 0 else 0.5,
                    "dualturn_eot_max": eot, "dualturn_hold_max": hold,
                    "dualturn_fvad_user": d["voice_activity_forecast"]["user"][-1],
                    "latency_ms": {"vap": v["latency_ms"]["total"], "dualturn": d["latency_ms"]["total"]}}
            return {**key, "mono": out}
        texts = prefix_texts(point, entry, words_all[point["file_id"]])
        turn = entry["turns"][point["turn"]]
        live, lat_lk = {}, None
        for kind in ["reference", "asr"] + [f"reference_lag{int(x * 1000)}" for x in TEXT_LAGS]:
            if not texts[kind]:
                live[kind] = None
                continue
            r = client.livekit(turn["history"] + [{"role": "user", "content": texts[kind]}])
            live[kind] = r["probability"]
            lat_lk = r["latency_ms"]["total"]
        return {**key, "texts": texts, "livekit": live, "livekit_latency_ms": lat_lk}

    count = 0
    with open(out_path, "a") as handle, futures.ThreadPoolExecutor(args.concurrency) as pool:
        it = iter(todo)
        pending = {}
        for _ in range(args.concurrency * 2):
            nxt = next(it, None)
            if nxt is not None:
                pending[pool.submit(work, nxt)] = nxt
        while pending:
            done, _ = futures.wait(pending, return_when=futures.FIRST_COMPLETED)
            for fut in done:
                pending.pop(fut)
                handle.write(json.dumps(fut.result()) + "\n")
                count += 1
                if count % 100 == 0:
                    handle.flush()
                    print(f"[score:{args.part}] {count}/{len(todo)} {time.time() - began:.0f}s", flush=True)
                if time.time() - began < args.budget:
                    nxt = next(it, None)
                    if nxt is not None:
                        pending[pool.submit(work, nxt)] = nxt
            if len(audio_cache) > 64:
                audio_cache.clear()
    print(f"[score:{args.part}] wrote {count} in {time.time() - began:.0f}s", flush=True)


def cmd_score_external(args) -> None:
    """Run a whole-stream predictor (X2-Turn frames or SoulX-Duplug decisions) once per file."""
    import requests

    manifest = [m for m in json.load(open(os.path.join(args.work, "manifest.json")))
                if m["scored"] and (args.split == "all" or m["split"] == args.split)
                and int(hashlib.sha1(m["file_id"].encode()).hexdigest(), 16) % args.external_sample_mod == 0]
    out_path = os.path.join(args.work, f"external_{args.model}.jsonl")
    seen = {json.loads(line)["file_id"] for line in open(out_path)} if os.path.exists(out_path) else set()
    session = requests.Session()
    health = session.get(args.url + "/health", timeout=10).json()
    json.dump(health, open(os.path.join(args.work, f"server_health_{args.model}.json"), "w"), indent=2)
    began = time.time()
    todo = [m for m in manifest if m["file_id"] not in seen]
    print(f"[score-external] {args.model}: {len(todo)} files ({len(seen)} cached)", flush=True)
    with open(out_path, "a") as handle:
        for n, entry in enumerate(todo):
            if time.time() - began > args.budget:
                print("[score-external] budget reached; rerun to continue", flush=True)
                break
            audio = load_audio(entry["path"])
            last_end = max(t["end"] for t in entry["turns"] if t["end"] > 0)
            audio = audio[: int(min(len(audio) / SR, last_end + FALLBACK_S + 0.5) * SR)]
            body = audio.astype("<f4").tobytes()
            if args.model == "x2turn":
                r = session.post(f"{args.url}/v1/turn-state/x2turn", params={"frames": 0}, data=body, timeout=600)
                r.raise_for_status()
                d = r.json()
                rec = {"file_id": entry["file_id"], "frame_ms": d["frame_ms"], "transcript": d["transcript"],
                       "labels": [f["label"] for f in d["frames"]],
                       "p": {k: [f["p"][k] for f in d["frames"]] for k in
                             ("idle", "noidle", "speaking", "turn_end", "backchannel", "uncertain")},
                       "latency_ms": d["latency_ms"]["total"], "audio_s": len(audio) / SR}
            else:
                r = session.post(f"{args.url}/v1/turn-state/soulx", params={"every": 1}, data=body, timeout=600)
                r.raise_for_status()
                d = r.json()
                rec = {"file_id": entry["file_id"], "events": d["events"], "latency_ms": d["latency_ms"],
                       "audio_s": len(audio) / SR}
            handle.write(json.dumps(rec) + "\n")
            handle.flush()
            if n % 20 == 0:
                print(f"[score-external] {n + 1}/{len(todo)} {time.time() - began:.0f}s", flush=True)


def x2_timing_summary(rows: list, noise: list, tolerance: float = 1e-3) -> dict:
    """Pick the availability delay from prefix rows (see cmd_x2turn_timing).

    Exact prefix equality is preferred. Otherwise the delay is the smallest one
    at which silencing the future (same buffer length) changes nothing; the
    remaining prefix difference at that delay is reported as the buffer-length
    fidelity gap that x2_evidence.causal_timing bounds.
    """
    delays = sorted({int(a) for r in rows for a in r["max_abs_diff_by_delay"]})
    prefix = {a: max(r["max_abs_diff_by_delay"][a if a in r["max_abs_diff_by_delay"] else str(a)] for r in rows)
              for a in delays}
    future = {a: max(r["same_length_future_silenced"][a if a in r["same_length_future_silenced"] else str(a)]
                     for r in rows) for a in delays}
    prefix_ok = [a for a in delays if prefix[a] <= tolerance]
    future_ok = [a for a in delays if future[a] <= tolerance]
    delay = min(prefix_ok) if prefix_ok else (min(future_ok) if future_ok else None)
    return {"causal_prefix_verified": bool(prefix_ok), "future_independent_verified": bool(future_ok),
            "available_after_frames": delay,
            "max_abs_diff_prefix_vs_full": round(prefix[delay] if delay is not None else min(prefix.values()), 6),
            "max_abs_diff_future_silenced": round(future[delay], 6) if delay is not None else None,
            "buffer_length_max_abs_diff": round(prefix[delay], 6) if delay is not None else None,
            "tolerance": tolerance, "max_abs_diff_by_delay": prefix, "future_silenced_by_delay": future,
            "full_buffer_repeat": noise, "rows": rows,
            "meaning": "frame f (covering [f, f+1) x 80 ms) is first produced once (f + 1 + available_after_frames) x 80 "
                       "ms of audio exist; at a fixed buffer length no later audio changes it; frames a prefix call "
                       "returns past that point are computed over the runtime's synthetic right padding",
            "excludes": "inference compute time; the offline re-decode, not the patched-vLLM stream"}


def cmd_x2turn_timing(args) -> None:
    """Find when an X2-Turn frame stops depending on audio that has not arrived yet.

    The offline runtime pads the right edge of every buffer with synthetic
    silence and returns frames for it, so a prefix call's trailing frames are
    not observations. For each candidate delay A, frame f of a prefix of t
    seconds is admissible only if (f + 1 + A) x 80 ms <= t; admissible frames
    must equal the full-buffer frames. The smallest A meeting the tolerance on
    every prefix is the availability delay. Two controls separate causes: the
    full buffer decoded twice (run-to-run noise), and the prefix followed by
    silence to full length (future content versus buffer length). Inference
    compute time is excluded.
    """
    import requests

    session = requests.Session()
    health = session.get(args.url + "/health", timeout=10).json()
    files = [os.path.join(FDB15, "user_interruption", str(i), "input.wav") for i in (1, 2, 3)]
    tolerance, candidates = 1e-3, range(0, 16)
    rows, noise = [], []

    def frames(audio):
        return session.post(f"{args.url}/v1/turn-state/x2turn", params={"frames": 0},
                            data=audio.astype("<f4").tobytes(), timeout=600).json()["frames"]

    def differences(part, ref):
        return [max(abs(a["p"][c] - b["p"][c]) for c in a["p"]) for a, b in zip(part, ref)]

    def admissible(diffs, t, a):
        return [d for f, d in enumerate(diffs) if (f + 1 + a) * 0.08 <= t + 1e-9]

    for path in files:
        audio = load_audio(path)
        ref = frames(audio)
        again = frames(audio)
        noise.append({"file": path, "frames": len(ref), "identical_length": len(again) == len(ref),
                      "max_abs_diff": round(max(differences(again, ref), default=0.0), 6)})
        for t in (1.6, 2.4, 3.04, 4.0, 4.72, 6.0, 6.96, 8.0, 9.6):
            n = int(round(t * SR))
            if n >= len(audio):
                continue
            part = frames(audio[:n])
            diffs = differences(part, ref)
            masked = frames(np.concatenate([audio[:n], np.zeros(len(audio) - n, dtype=audio.dtype)]))
            masked_diffs = differences(masked, ref)
            row = {"file": path, "t": t, "prefix_frames": len(part), "full_frames": len(ref),
                   "max_abs_diff_by_delay": {}, "same_length_future_silenced": {}}
            for a in candidates:
                value = max(admissible(diffs, t, a), default=0.0)
                row["max_abs_diff_by_delay"][a] = round(value, 6)
                row["same_length_future_silenced"][a] = round(max(admissible(masked_diffs, t, a), default=0.0), 6)
            rows.append(row)
            print(json.dumps({"file": path, "t": t, "d6": row["max_abs_diff_by_delay"][6],
                              "masked_d6": row["same_length_future_silenced"][6]}), flush=True)
    out = x2_timing_summary(rows, noise, tolerance)
    out["server_health"] = health
    json.dump(out, open(os.path.join(args.work, "x2turn_timing.json"), "w"), indent=2)
    print(json.dumps({k: v for k, v in out.items() if k != "rows"}, indent=2))


# --------------------------------------------------------------------------
# analysis


def logit(p):
    p = np.clip(np.asarray(p, dtype=float), 1e-6, 1 - 1e-6)
    return np.log(p / (1 - p))


def auc(scores, labels):
    from sklearn.metrics import roc_auc_score

    labels = np.asarray(labels)
    if len(set(labels.tolist())) < 2:
        return None
    return float(roc_auc_score(labels, scores))


def policy_outcomes(records, score_fn, theta):
    """Endpoint time (s after silence onset) per record under 'fire at first checkpoint with score>=theta'."""
    times = []
    for rec in records:
        fired = None
        for s in CHECKPOINTS:
            if rec["pause"] is not None and s >= rec["pause"]:
                break
            sc = score_fn(rec, s)
            if sc is not None and sc >= theta:
                fired = s
                break
        if fired is None:
            fired = FALLBACK_S if (rec["pause"] is None or FALLBACK_S < rec["pause"]) else None
        times.append(fired)
    return times


def summarize(records, times):
    holds = [(r, t) for r, t in zip(records, times) if r["label"] == 0]
    ends = [(r, t) for r, t in zip(records, times) if r["label"] == 1]
    premature = sum(1 for _, t in holds if t is not None) / max(1, len(holds))
    waits = np.array([t - CHECKPOINTS[0] for _, t in ends if t is not None], dtype=float)
    fallback = sum(1 for _, t in ends if t == FALLBACK_S) / max(1, len(ends))
    out = {"holds": len(holds), "endpoints": len(ends), "premature_endpoint_rate": round(premature, 4),
           "fallback_rate": round(fallback, 4)}
    if len(waits):
        out.update({"added_wait_mean_ms": round(1000 * waits.mean(), 1),
                    "added_wait_p50_ms": round(1000 * np.percentile(waits, 50), 1),
                    "added_wait_p90_ms": round(1000 * np.percentile(waits, 90), 1),
                    "added_wait_p95_ms": round(1000 * np.percentile(waits, 95), 1)})
    return out


def choose_theta(records, score_fn, target):
    """Lowest threshold whose premature-endpoint rate on these records is <= target."""
    grid = sorted(set([0.0, 1.0] + [x for x in np.linspace(0, 1, 401)]))
    best = 1.0 + 1e-9
    for theta in reversed(grid):
        times = policy_outcomes(records, score_fn, theta)
        holds = [t for r, t in zip(records, times) if r["label"] == 0]
        rate = sum(1 for t in holds if t is not None) / max(1, len(holds))
        if rate <= target:
            best = theta
        else:
            break
    return best


def timer_summary(records, timeout):
    """Pure silence timer (OpenRealtime's engine floor default is 500 ms)."""
    times = []
    for rec in records:
        if rec["pause"] is not None and timeout >= rec["pause"]:
            times.append(None)
        else:
            times.append(timeout)
    return summarize(records, times)


def cmd_analyze(args) -> None:
    from sklearn.linear_model import LogisticRegression

    points = {(p["file_id"], p["onset"]): p for p in
              (json.loads(line) for line in open(os.path.join(args.work, "points.jsonl")))}
    recs = []
    smart = {(r["file_id"], r["onset"]): r for r in
             (json.loads(line) for line in open(os.path.join(args.work, "scores_smart.jsonl")))}
    text = {(r["file_id"], r["onset"]): r for r in
            (json.loads(line) for line in open(os.path.join(args.work, "scores_text.jsonl")))}
    for key, p in points.items():
        if key in smart and key in text:
            recs.append({**p, **smart[key], **text[key]})
    print(f"records with both evidence types: {len(recs)} of {len(points)} points")
    text_kind = args.text
    recs = [r for r in recs if r["smart_turn"].get("0.2") is not None and r["livekit"].get(text_kind) is not None]
    cal = [r for r in recs if r["split"] == "calibration"]
    test = [r for r in recs if r["split"] == "test"]

    def feats(rec, s):
        st = rec["smart_turn"].get(f"{s:.1f}")
        if st is None:
            return None
        return [float(logit(st)), float(logit(rec["livekit"][text_kind]))]

    X = np.array([feats(r, 0.2) for r in cal])
    y = np.array([r["label"] for r in cal])
    fusion = LogisticRegression(C=1.0, max_iter=1000).fit(X, y)
    # Platt calibration of each single model on the same split (monotone: does not change any curve)
    platt = {name: LogisticRegression(C=1.0, max_iter=1000).fit(X[:, [i]], y) for i, name in
             enumerate(["smart_turn", "livekit"])}

    def score_smart(rec, s):
        return rec["smart_turn"].get(f"{s:.1f}")

    def score_live(rec, s):
        return rec["livekit"][text_kind]

    # precompute the fused probability for every (record, checkpoint) in one call
    keys, rows = [], []
    for i, rec in enumerate(recs):
        for s in CHECKPOINTS:
            f = feats(rec, s)
            if f is not None:
                keys.append((i, s))
                rows.append(f)
    fused = fusion.predict_proba(np.array(rows))[:, 1]
    for (i, s), v in zip(keys, fused):
        recs[i].setdefault("_fusion", {})[f"{s:.1f}"] = float(v)

    def score_fusion(rec, s):
        return rec.get("_fusion", {}).get(f"{s:.1f}")

    scorers = {"smart_turn": score_smart, "livekit": score_live, "fusion": score_fusion}
    x2_path = os.path.join(args.work, "external_x2turn.jsonl")
    x2_timing = os.path.join(args.work, "x2turn_timing.json")
    if os.path.exists(x2_path) and os.path.exists(x2_timing) and causal_timing(x2_timing):
        x2 = {json.loads(l)["file_id"]: json.loads(l) for l in open(x2_path)}
        delay_frames = json.load(open(x2_timing))["available_after_frames"]

        def score_x2(rec, s, x2=x2, delay_frames=delay_frames):
            d = x2.get(rec["file_id"])
            if d is None:
                return None
            t = rec["onset"] + s
            # frame f (covering [f, f+1) * 80 ms) is available once audio reaches (f + 1 + delay) * 80 ms
            f = int(math.floor(t / (d["frame_ms"] / 1000.0))) - 1 - delay_frames
            if f < 0 or f >= len(d["p"]["turn_end"]):
                return None
            return d["p"]["turn_end"][f]

        def label_x2(rec, s, x2=x2, delay_frames=delay_frames):
            d = x2.get(rec["file_id"])
            f = int(math.floor((rec["onset"] + s) / (d["frame_ms"] / 1000.0))) - 1 - delay_frames
            return d["labels"][f] if 0 <= f < len(d["labels"]) else None

        x2_eval = {"score_turn_end": score_x2, "label": label_x2, "files": len(x2)}
    else:
        x2_eval = None

    result = {
        "cell": "I0", "text_condition": text_kind, "checkpoints_s": CHECKPOINTS, "fallback_s": FALLBACK_S,
        "records": {"calibration": len(cal), "test": len(test)},
        "fusion": {"features": ["logit P_smart_turn", f"logit P_livekit({text_kind})"],
                   "coef": fusion.coef_[0].round(4).tolist(), "intercept": round(float(fusion.intercept_[0]), 4),
                   "fit": "sklearn LogisticRegression(C=1) on the calibration split, 200 ms checkpoint"},
    }

    def calib_metrics(recs_, prob_fn):
        from sklearn.metrics import brier_score_loss

        p = np.array([prob_fn(r) for r in recs_])
        yy = np.array([r["label"] for r in recs_])
        bins = np.clip((p * 10).astype(int), 0, 9)
        ece = sum(abs(p[bins == b].mean() - yy[bins == b].mean()) * (bins == b).mean() for b in range(10)
                  if (bins == b).any())
        return {"brier": round(float(brier_score_loss(yy, p)), 4), "ece_10bin": round(float(ece), 4)}

    single_shot = {}
    for split_name, rs in (("calibration", cal), ("test", test)):
        yy = [r["label"] for r in rs]
        single_shot[split_name] = {
            "smart_turn_auc": auc([score_smart(r, 0.2) for r in rs], yy),
            "livekit_auc": auc([score_live(r, 0.2) for r in rs], yy),
            "fusion_auc": auc([score_fusion(r, 0.2) for r in rs], yy),
        }
    single_shot["test_calibration"] = {
        "smart_turn_platt": calib_metrics(test, lambda r: platt["smart_turn"].predict_proba(
            np.array([[feats(r, 0.2)[0]]]))[0, 1]),
        "livekit_platt": calib_metrics(test, lambda r: platt["livekit"].predict_proba(
            np.array([[feats(r, 0.2)[1]]]))[0, 1]),
        "fusion": calib_metrics(test, lambda r: score_fusion(r, 0.2)),
        "smart_turn_raw": calib_metrics(test, lambda r: score_smart(r, 0.2)),
    }
    result["single_shot_200ms"] = single_shot

    sources = sorted(set(r["source"] for r in recs))

    # VAP / DualTurn through the endpoint-hook maps (mono, assistant silent): single-shot AUC only
    mono_path = os.path.join(args.work, "scores_mono.jsonl")
    mono = {}
    if os.path.exists(mono_path):
        mono = {(r["file_id"], r["onset"]): r["mono"] for r in (json.loads(l) for l in open(mono_path))}
    if mono:
        feats_mono = ["vap_p_now_assistant", "vap_p_future_assistant", "dualturn_eot_over_eot_hold",
                      "dualturn_eot_max"]
        table = {}
        for s_key in ("0.2",):
            rows = {}
            for name in feats_mono:
                for split_name, rs in (("test", test),):
                    pairs = [(mono[(r["file_id"], r["onset"])][s_key][name], r["label"]) for r in rs
                             if (r["file_id"], r["onset"]) in mono and s_key in mono[(r["file_id"], r["onset"])]]
                    if pairs:
                        rows[name] = {"n": len(pairs), "auc": auc([a for a, _ in pairs], [b for _, b in pairs])}
                        rows[name]["per_source"] = {}
                        for src in sources:
                            sp = [(mono[(r["file_id"], r["onset"])][s_key][name], r["label"]) for r in rs
                                  if r["source"] == src and (r["file_id"], r["onset"]) in mono
                                  and s_key in mono[(r["file_id"], r["onset"])]]
                            rows[name]["per_source"][src] = auc([a for a, _ in sp], [b for _, b in sp]) if sp else None
            # the same test records scored by the dedicated endpoint models, for a like-for-like AUC
            same = [r for r in test if (r["file_id"], r["onset"]) in mono and s_key in mono[(r["file_id"], r["onset"])]]
            rows["smart_turn_same_points"] = auc([score_smart(r, float(s_key)) for r in same], [r["label"] for r in same])
            rows["livekit_same_points"] = auc([score_live(r, 0.2) for r in same], [r["label"] for r in same])
            table[f"{s_key}s"] = rows
        result["mono_vap_dualturn_endpoint_auc_test"] = {
            "note": "assistant channel assumed silent; last <= 8 s of user audio; maps as in turn_server.py "
                    "(/v1/endpoint/vap = p_future assistant - the aggregate chosen on the calibration split - "
                    "and /v1/endpoint/dualturn = eot/(eot+hold) over the last 400 ms)",
            **table}
    per_source = {}
    for src in sources:
        rs = [r for r in test if r["source"] == src]
        yy = [r["label"] for r in rs]
        per_source[src] = {"test_holds": yy.count(0), "test_endpoints": yy.count(1),
                           "smart_turn_auc": auc([score_smart(r, 0.2) for r in rs], yy),
                           "livekit_auc": auc([score_live(r, 0.2) for r in rs], yy),
                           "fusion_auc": auc([score_fusion(r, 0.2) for r in rs], yy)}
    result["per_source_auc_test"] = per_source

    operating = {}
    for target in args.targets:
        row = {}
        for name, fn in scorers.items():
            theta = choose_theta(cal, fn, target)
            row[name] = {"theta": round(theta, 4), "test": summarize(test, policy_outcomes(test, fn, theta)),
                         "calibration": summarize(cal, policy_outcomes(cal, fn, theta))}
            row[name]["test_per_source"] = {src: summarize([r for r in test if r["source"] == src],
                                                           policy_outcomes([r for r in test if r["source"] == src],
                                                                           fn, theta)) for src in sources}
        # the silence timer matched on the calibration split
        best_t = None
        for timeout in [x / 100 for x in range(20, 201, 5)]:
            if timer_summary(cal, timeout)["premature_endpoint_rate"] <= target:
                best_t = timeout
                break
        if best_t is not None:
            row["silence_timer"] = {"timeout_s": best_t, "test": timer_summary(test, best_t)}
        operating[f"premature<={target}"] = row
    result["matched_operating_points"] = operating
    result["openrealtime_default_timer_500ms_test"] = timer_summary(test, 0.5)
    if x2_eval is not None:
        rs_x2 = [r for r in test if r["file_id"] in x2]
        own = policy_outcomes(rs_x2, lambda r, s: 1.0 if x2_eval["label"](r, s) == "turn_end" else 0.0, 0.5)
        result["x2turn_test"] = {
            "files": x2_eval["files"],
            "auc_p_turn_end_200ms": auc([x2_eval["score_turn_end"](r, 0.2) or 0.0 for r in rs_x2],
                                        [r["label"] for r in rs_x2]),
            "auc_p_turn_end_200ms_per_source": {
                src: auc([x2_eval["score_turn_end"](r, 0.2) or 0.0 for r in rs_x2 if r["source"] == src],
                         [r["label"] for r in rs_x2 if r["source"] == src]) for src in sources},
            "own_rule_argmax_turn_end": summarize(rs_x2, own),
            "own_rule_per_source": {src: summarize([r for r in rs_x2 if r["source"] == src],
                                                   [t for r, t in zip(rs_x2, own) if r["source"] == src])
                                    for src in sources},
            "same_points_smart_turn_auc_200ms": auc([score_smart(r, 0.2) for r in rs_x2], [r["label"] for r in rs_x2]),
            "note": "frames used only once available (x2turn_timing.json); endpoint when the frame argmax is "
                    "turn_end at a checkpoint, shared 2 s silence fallback"}
    soulx_path = os.path.join(args.work, "external_soulx.jsonl")
    if os.path.exists(soulx_path):
        sx = {json.loads(l)["file_id"]: json.loads(l) for l in open(soulx_path)}

        def soulx_times(rs):
            times = []
            for rec in rs:
                speaks = [e["available_s"] for e in sx[rec["file_id"]]["events"] if e["state"] == "speak"]
                limit = rec["onset"] + (rec["pause"] if rec["pause"] is not None else 1e9)
                fired = [t - rec["onset"] for t in speaks if rec["onset"] <= t < limit]
                if fired:
                    times.append(fired[0])
                elif rec["pause"] is None or FALLBACK_S < rec["pause"]:
                    times.append(FALLBACK_S)  # the shared 2 s silence timer
                else:
                    times.append(None)
            return times

        rs_all = [r for r in test if r["file_id"] in sx]
        base = summarize(rs_all, soulx_times(rs_all))
        waits = [t for r, t in zip(rs_all, soulx_times(rs_all)) if r["label"] == 1 and t is not None]
        base["endpoint_latency_from_speech_offset_ms"] = {
            "mean": round(1000 * float(np.mean(waits)), 1), "p50": round(1000 * float(np.percentile(waits, 50)), 1),
            "p90": round(1000 * float(np.percentile(waits, 90)), 1)} if waits else None
        base["note"] = ("SoulX-Duplug's own state machine (semantic VAD + max_wait_num fallback); added_wait is "
                        "relative to the shared 200 ms VAD decision point and can be negative when it fires sooner")
        per = {src: summarize([r for r in rs_all if r["source"] == src],
                              soulx_times([r for r in rs_all if r["source"] == src])) for src in sources}
        # 'speak' while the user is audibly speaking inside a turn is also a premature endpoint,
        # but it is not attached to any pause, so count it per scored turn.
        manifest = {m["file_id"]: m for m in json.load(open(os.path.join(args.work, "manifest.json")))}
        turns_seen, in_speech = 0, 0
        for fid in {r["file_id"] for r in rs_all}:
            m = manifest[fid]
            for k in {r["turn"] for r in rs_all if r["file_id"] == fid}:
                turn = m["turns"][k]
                turns_seen += 1
                segs = [sg for sg in m["segments"] if turn["start"] - 0.05 <= sg[0] and sg[1] <= turn["end"] + 0.05]
                speaks = [e["available_s"] for e in sx[fid]["events"] if e["state"] == "speak"]
                if any(sg[0] + 0.1 < t < sg[1] for t in speaks for sg in segs):
                    in_speech += 1
        base["turns_with_speak_during_speech"] = f"{in_speech}/{turns_seen}"
        result["soulx_own_policy_test"] = {"test": base, "test_per_source": per, "files": len(sx)}
    result["tradeoff_curves_test"] = {
        name: [dict(theta=round(t, 3), **summarize(test, policy_outcomes(test, fn, t)))
               for t in [0.02, 0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.98]]
        for name, fn in scorers.items()}
    lat_st = [x for r in recs for x in r.get("smart_turn_latency_ms", [])]
    lat_lk = [r["livekit_latency_ms"] for r in recs if r.get("livekit_latency_ms") is not None]
    result["server_latency_ms"] = {
        "smart_turn": {"n": len(lat_st), "p50": float(np.percentile(lat_st, 50)), "p90": float(np.percentile(lat_st, 90))},
        "livekit": {"n": len(lat_lk), "p50": float(np.percentile(lat_lk, 50)), "p90": float(np.percentile(lat_lk, 90))},
    }
    ref_ok = [r for r in recs if r["label"] == 1]
    result["transcript_alignment_check"] = {
        "endpoint_points": len(ref_ok),
        "reference_prefix_equals_full_turn": round(sum(
            LiveKitNorm(r["texts"]["reference"]) == LiveKitNorm(r["texts"]["reference_full"]) for r in ref_ok)
            / max(1, len(ref_ok)), 4),
        "mean_reference_words_matched_to_asr": round(float(np.mean([r["texts"]["matched_fraction"] for r in recs])), 4),
    }
    if text_kind == "reference":
        calib = {"platt": {
            "smart_turn": {"coef": round(float(platt["smart_turn"].coef_[0][0]), 6),
                           "intercept": round(float(platt["smart_turn"].intercept_[0]), 6),
                           "input": "logit(Smart Turn v3.2 fp32 probability)"},
            "livekit": {"coef": round(float(platt["livekit"].coef_[0][0]), 6),
                        "intercept": round(float(platt["livekit"].intercept_[0]), 6),
                        "input": "logit(LiveKit v0.4.1-intl raw EOU probability, reference-transcript prefix)"}},
            "fusion": {"features": ["logit P_smart_turn", "logit P_livekit"], "coef": [round(float(c), 6) for c in
                                                                                     fusion.coef_[0]],
                       "intercept": round(float(fusion.intercept_[0]), 6)}}
        if mono:
            for name, key in (("vap", "vap_p_future_assistant"), ("dualturn", "dualturn_eot_over_eot_hold")):
                pairs = [(mono[(r["file_id"], r["onset"])]["0.2"][key], r["label"]) for r in cal
                         if (r["file_id"], r["onset"]) in mono and "0.2" in mono[(r["file_id"], r["onset"])]]
                if len(pairs) > 50:
                    lr = LogisticRegression(C=1.0, max_iter=1000).fit(
                        np.array([[float(logit(a))] for a, _ in pairs]), np.array([b for _, b in pairs]))
                    calib["platt"][name] = {"coef": round(float(lr.coef_[0][0]), 6),
                                            "intercept": round(float(lr.intercept_[0]), 6),
                                            "input": f"logit({key}) at the 200 ms checkpoint", "n": len(pairs)}
        digest = hashlib.sha256()
        for name in ("points.jsonl", "scores_smart.jsonl", "scores_text.jsonl", "scores_mono.jsonl"):
            path = os.path.join(args.work, name)
            if os.path.exists(path):
                digest.update(open(path, "rb").read())
        calib["fitted"] = {"by": "tools/duplexmodels/endpoint_eval.py analyze --text reference",
                           "split": "calibration (sha1(file_id) even), 200 ms checkpoint",
                           "records": len(cal), "at": time.strftime("%Y-%m-%dT%H:%M:%S"),
                           "sha256": digest.hexdigest()}
        calib["form"] = "calibrated = sigmoid(coef * logit(p) + intercept); fusion = sigmoid(w . [logit p_st, logit p_lk] + b)"
        json.dump(calib, open(os.path.join(args.work, "calibration.json"), "w"), indent=2)
        result["frozen_calibration_file"] = os.path.join(args.work, "calibration.json")
    out = os.path.join(args.work, f"i0_results_{text_kind}.json")
    json.dump(result, open(out, "w"), indent=2)
    print(json.dumps({k: result[k] for k in ("records", "fusion", "single_shot_200ms", "per_source_auc_test")},
                     indent=2))
    for key, row in operating.items():
        print(key)
        for name, v in row.items():
            print(f"  {name:13s} {v.get('theta', v.get('timeout_s'))} {v['test']}")
    print("default 500 ms timer:", result["openrealtime_default_timer_500ms_test"])
    print("wrote", out)


def LiveKitNorm(text: str) -> str:
    return re.sub(r"\s+", " ", re.sub(r"[^a-z0-9' ]", "", text.lower())).strip()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("command", choices=["prepare", "transcribe", "score", "score-external", "x2turn-timing",
                                            "analyze"])
    parser.add_argument("--model", default="x2turn", choices=["x2turn", "soulx"], help="score-external target")
    parser.add_argument("--external-sample-mod", type=int, default=1,
                        help="score-external: keep files whose hash %% N == 0 (these models are slow)")
    parser.add_argument("--split", default="test", choices=["test", "calibration", "all"],
                        help="score-external: which files (external models have no fitted parameters)")
    parser.add_argument("--work", default=DEFAULT_WORK)
    parser.add_argument("--sources", default="fdbench,fdbv3,fdbv15")
    parser.add_argument("--fdbench-conditions", default=",".join([
        "cosyvoice2-single-round-combine-easy", "f5tts-single-round-combine-easy",
        "chattts-single-round-combine-easy", "cosyvoice2-single-round-combine-easy-noisy-bg-0dB"]))
    parser.add_argument("--fdbench-per-condition", type=int, default=60)
    parser.add_argument("--whisper-model", default="mobiuslabsgmbh/faster-whisper-large-v3-turbo")
    parser.add_argument("--whisper-device", default="cuda")
    parser.add_argument("--whisper-compute", default="float16")
    parser.add_argument("--whisper-threads", type=int, default=8)
    parser.add_argument("--words-file", default="words.json", help="word-timing cache (transcribe writes, score reads)")
    parser.add_argument("--url", default="http://127.0.0.1:9130")
    parser.add_argument("--concurrency", type=int, default=4)
    parser.add_argument("--budget", type=float, default=540.0, help="stop starting new work after this many seconds")
    parser.add_argument("--text", default="reference",
                        choices=["reference", "asr"] + [f"reference_lag{int(x * 1000)}" for x in TEXT_LAGS])
    parser.add_argument("--part", default="smart", choices=["smart", "text", "mono"],
                        help="score: which evidence to collect")
    parser.add_argument("--mono-sample-mod", type=int, default=3,
                        help="score --part mono: keep points whose hash %% N == 0 (VAP/DualTurn are slow here)")
    parser.add_argument("--smart-variant", default=None, choices=[None, "gpu", "cpu"],
                        help="Smart Turn ONNX export (default: the server's default)")
    parser.add_argument("--targets", type=float, nargs="+", default=[0.05, 0.10, 0.20])
    args = parser.parse_args()
    {"prepare": cmd_prepare, "transcribe": cmd_transcribe, "score": cmd_score, "score-external": cmd_score_external,
     "x2turn-timing": cmd_x2turn_timing, "analyze": cmd_analyze}[args.command](args)


if __name__ == "__main__":
    main()
