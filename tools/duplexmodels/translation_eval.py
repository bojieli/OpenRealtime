"""Simultaneous speech translation evaluation (plan cell X0): Hibiki fr->en on FLEURS.

This is a translation task profile, scored separately from conversation
benchmarks: translation overlap is not a barge-in decision.

The harness speaks the sidecar protocol (docs/sidecar-protocol-1.md) to a
translation sidecar process over stdin/stdout, exactly as the engine would.
Each French FLEURS test utterance is replayed at real time: frame k (80 ms of
source) is written when the wall clock reaches the end of that frame, as a
live microphone would deliver it. After the last source frame the harness
sends ``commit`` (end of source segment); the sidecar feeds Hibiki's
end-of-stream marker, keeps stepping on self-clocked real-time silence until
the model's text EOS, then answers ``text_done`` + ``turn_done`` and resets.
``--mode continuous`` instead streams ``--trailing-seconds`` of real silence
first and commits afterwards, to see how much translation arrives without the
end-of-stream marker.

Metrics (all delays in seconds of source audio, source duration |X| = the
utterance's real length):

* BLEU: sacrebleu corpus BLEU, default 13a tokenisation, of the concatenated
  text stream against the English FLEURS ``raw_transcription`` (cased,
  punctuated). A second score lowercases and strips punctuation on both sides.
  chrF is reported alongside.
* Word delays. Text arrives as SentencePiece pieces; a piece starting with a
  space opens a word. A word's delay is when its *last* piece arrived.
  ``d_wall`` = wall time since source start (computation-aware: includes
  queueing, the model step, and the pipe); ``d_model`` = source audio the
  model had consumed when it emitted the word, from the ``step`` field of
  the sidecar's text_delta, capped at |X| (non-computation-aware).
* Average Lagging, as SimulEval's AL (Ma et al. 2019) with the reference
  length: gamma = |Y_ref| / |X|; words are taken in order while
  d_i <= |X| (stopping after the first word with d_i == |X|),
  tau = number taken, AL = (1/tau) * sum_{i=1..tau} (d_i - (i-1)/gamma).
  If the first word already comes after |X|, AL = d_1. LAAL uses
  max(|Y_hyp|, |Y_ref|) in gamma. AL is reported from ``d_model`` and, as
  AL_CA, from ``d_wall``.
* First-word latency, end offset (last word time minus |X|), time from the
  end of source to turn_done, output speech seconds, and the sidecar's own
  per-frame compute (RTF = mean compute per 80 ms frame / 80 ms).

Usage:

    python tools/duplexmodels/translation_eval.py download
    python tools/duplexmodels/translation_eval.py run --offset 0 --limit 10 --out results/translation/hibiki-fleurs-fr-en.jsonl
    python tools/duplexmodels/translation_eval.py summarize --jsonl ... --out ...-summary.json
    python tools/duplexmodels/translation_eval.py file --path some_french.mp3
"""

from __future__ import annotations

import argparse
import io
import json
import os
import queue
import re
import statistics
import subprocess
import sys
import threading
import time
import unicodedata
import wave
from pathlib import Path

import numpy as np

REPOSITORY = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPOSITORY / "sidecars"))

from openrealtime_sidecar.protocol import MessageType, read_message, write_message  # noqa: E402

RUNTIME = REPOSITORY / ".runtime" / "duplex-plan"
DATA = RUNTIME / "data" / "fleurs"
RESULTS = RUNTIME / "results" / "translation"
DEFAULT_PYTHON = RUNTIME / "venvs" / "kyutai" / "bin" / "python"
DEFAULT_SIDECAR = REPOSITORY / "sidecars" / "hibiki_sidecar.py"
FRAME_SECONDS = 0.08
FLEURS_RATE = 16_000


# --------------------------------------------------------------------------
# Data


def _token() -> str | None:
    token = os.environ.get("HF_TOKEN")
    if token:
        return token
    try:
        for line in (Path.home() / ".bashrc").read_text().splitlines():
            if line.startswith("export HF_TOKEN="):
                token = line.split("=", 1)[1].strip().strip("\"'")
    except OSError:
        pass
    return token


def parquet_path(language: str) -> Path:
    return DATA / "parquet-data" / language / "test-00000-of-00001.parquet"


def download() -> None:
    from huggingface_hub import hf_hub_download

    for language in ("fr_fr", "en_us"):
        target = parquet_path(language)
        if target.exists() and target.stat().st_size > 0:
            print(f"{target} present ({target.stat().st_size / 1e6:.0f} MB)")
            continue
        path = hf_hub_download("google/fleurs", f"parquet-data/{language}/test-00000-of-00001.parquet",
                               repo_type="dataset", local_dir=str(DATA), token=_token())
        print(f"downloaded {path}")


def load_pairs(offset: int, limit: int) -> list[dict]:
    """French source recordings paired with English references by FLEURS id.

    FLEURS has several recordings per sentence id; the first French recording
    in file order is used, and ids are taken in file order of first
    appearance, so a selection is reproducible.
    """
    import pyarrow.parquet as pq
    import soundfile

    english = pq.read_table(parquet_path("en_us"), columns=["id", "transcription", "raw_transcription"]).to_pandas()
    references = {}
    for row in english.itertuples():
        references.setdefault(int(row.id), (row.raw_transcription, row.transcription))
    french = pq.read_table(parquet_path("fr_fr"),
                           columns=["id", "num_samples", "audio", "raw_transcription"]).to_pandas()
    seen: set[int] = set()
    chosen = []
    for row in french.itertuples():
        key = int(row.id)
        if key in seen or key not in references:
            continue
        seen.add(key)
        chosen.append(row)
    pairs = []
    for row in chosen[offset:offset + limit]:
        audio, rate = soundfile.read(io.BytesIO(row.audio["bytes"]), dtype="float32")
        if audio.ndim > 1:
            audio = audio.mean(axis=1)
        if rate != FLEURS_RATE:
            raise ValueError(f"FLEURS id {row.id} is {rate} Hz, expected {FLEURS_RATE}")
        pairs.append({
            "id": int(row.id), "audio": audio, "rate": rate,
            "source_text": row.raw_transcription,
            "reference": references[int(row.id)][0],
            "reference_normalized": references[int(row.id)][1],
        })
    return pairs


# --------------------------------------------------------------------------
# Sidecar process


class SidecarProcess:
    def __init__(self, command: list[str], sample_rate: int, log_path: Path) -> None:
        self.log_file = open(log_path, "ab")
        self.process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=self.log_file, cwd=str(REPOSITORY))
        self.messages: "queue.Queue[tuple[float, object]]" = queue.Queue()
        self.sample_rate = sample_rate
        self.write_lock = threading.Lock()
        began = time.perf_counter()
        self.send(MessageType.HELLO, version=1, sample_rate=sample_rate, instructions="", voice="default")
        while True:
            message = read_message(self.process.stdout)
            if message is None:
                raise RuntimeError(f"sidecar exited during the handshake; see {log_path}")
            if message.type == MessageType.READY:
                self.ready = message.header
                break
            if message.type == MessageType.ERROR:
                raise RuntimeError(f"sidecar refused the handshake: {message.header}")
        self.load_seconds = time.perf_counter() - began
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self) -> None:
        while True:
            try:
                message = read_message(self.process.stdout)
            except Exception as failure:  # noqa: BLE001
                self.messages.put((time.monotonic(), failure))
                return
            self.messages.put((time.monotonic(), message))
            if message is None:
                return

    def send(self, kind: str, payload: bytes = b"", **header) -> None:
        with self.write_lock:
            write_message(self.process.stdin, kind, payload, **header)

    def close(self) -> None:
        try:
            self.send(MessageType.BYE)
            self.process.stdin.close()
        except (BrokenPipeError, OSError, ValueError):
            pass
        try:
            self.process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()
        self.log_file.close()


# --------------------------------------------------------------------------
# One utterance


def _words(pieces: list[dict]) -> list[dict]:
    words: list[dict] = []
    for piece in pieces:
        text = piece["text"]
        if not words or text.startswith(" "):
            # A lone space piece ("\u2581") still opens a word: digits and some
            # other words follow it as separate pieces.
            words.append({"word": text.strip(), "d_wall": piece["d_wall"], "step": piece["step"]})
        else:
            words[-1]["word"] += text
            words[-1]["d_wall"] = piece["d_wall"]
            words[-1]["step"] = piece["step"]
    return [word for word in words if word["word"]]


def translate(sidecar: SidecarProcess, audio: np.ndarray, rate: int, *, mode: str, trailing_seconds: float,
              max_wait_seconds: float) -> dict:
    frame = int(round(rate * FRAME_SECONDS))
    source_seconds = audio.size / rate
    frames = int(np.ceil(audio.size / frame))
    padded = np.zeros(frames * frame, dtype=np.float32)
    padded[:audio.size] = audio
    pcm = (np.clip(padded, -1, 1) * 32767).astype("<i2")
    silence = np.zeros(frame, dtype="<i2").tobytes()
    # Drain anything left from a previous utterance.
    while not sidecar.messages.empty():
        sidecar.messages.get_nowait()
    start = time.monotonic()
    for index in range(frames):
        # Frame k covers source [k*80ms, (k+1)*80ms); a live microphone has
        # it at the end of that span.
        delay = start + (index + 1) * FRAME_SECONDS - time.monotonic()
        if delay > 0:
            time.sleep(delay)
        sidecar.send(MessageType.AUDIO, pcm[index * frame:(index + 1) * frame].tobytes())
    source_end = start + frames * FRAME_SECONDS
    commit_at = None
    if mode == "commit":
        sidecar.send(MessageType.COMMIT)
        commit_at = time.monotonic()
    else:
        trailing = int(round(trailing_seconds / FRAME_SECONDS))
        for index in range(trailing):
            delay = source_end + (index + 1) * FRAME_SECONDS - time.monotonic()
            if delay > 0:
                time.sleep(delay)
            sidecar.send(MessageType.AUDIO, silence)
        sidecar.send(MessageType.COMMIT)
        commit_at = time.monotonic()
    pieces: list[dict] = []
    audio_out = bytearray()
    text_done = None
    stats = None
    errors: list[str] = []
    turn_done_at = None
    deadline = commit_at + max_wait_seconds
    # Messages were queued with their arrival time, so reading them after the
    # send loop loses no timing.
    while True:
        timeout = deadline - time.monotonic()
        if timeout <= 0:
            errors.append("no turn_done before the wait budget")
            break
        try:
            arrived, message = sidecar.messages.get(timeout=timeout)
        except queue.Empty:
            continue
        if message is None or isinstance(message, Exception):
            errors.append(f"sidecar stream ended: {message}")
            break
        kind = message.type
        if kind == MessageType.TEXT_DELTA:
            pieces.append({"text": message.text, "d_wall": round(arrived - start, 3),
                           "step": int(message.get("step", 0)),
                           "after_commit": arrived >= commit_at})
        elif kind == MessageType.OUTPUT_AUDIO:
            audio_out.extend(message.payload)
        elif kind == MessageType.TEXT_DONE:
            text_done = message.text
        elif kind == MessageType.LOG and message.text.startswith("hibiki-stats "):
            stats = json.loads(message.text[len("hibiki-stats "):])
        elif kind == MessageType.ERROR:
            errors.append(json.dumps(message.header))
        elif kind == MessageType.TURN_DONE:
            turn_done_at = arrived
            break
    words = _words(pieces)
    for word in words:
        word["d_model"] = round(min(word["step"], frames) * FRAME_SECONDS, 3)
        word["d_model"] = min(word["d_model"], round(source_seconds, 3))
    stream_text = "".join(piece["text"] for piece in pieces)
    before_commit = "".join(piece["text"] for piece in pieces if not piece["after_commit"])
    return {
        "source_seconds": round(source_seconds, 3),
        "source_frames": frames,
        "hypothesis": re.sub(r"\s+", " ", stream_text).strip(),
        "hypothesis_before_commit": re.sub(r"\s+", " ", before_commit).strip(),
        "text_done": text_done,
        "words": words,
        "pieces": pieces,
        "turn_done_after_source_end_s": round(turn_done_at - source_end, 3) if turn_done_at else None,
        "output_audio_seconds": round(len(audio_out) / 2 / int(sidecar.ready.get("output_rate", 24000)), 3),
        "output_audio": bytes(audio_out),
        "sidecar_stats": stats,
        "errors": errors,
        "words_after_commit": sum(1 for piece in pieces if piece["after_commit"] and piece["text"].startswith(" ")),
        "mode": mode,
    }


# --------------------------------------------------------------------------
# Metrics


def average_lagging(delays: list[float], source: float, reference_words: int, hypothesis_words: int | None = None) -> float | None:
    if not delays or source <= 0:
        return None
    if delays[0] > source:
        return delays[0]
    length = reference_words if hypothesis_words is None else max(reference_words, hypothesis_words)
    gamma = max(length, 1) / source
    total, tau = 0.0, 0
    for index, delay in enumerate(delays):
        if delay > source:
            break
        total += delay - index / gamma
        tau = index + 1
        if delay >= source:
            break
    return total / tau


_PUNCT = re.compile(r"[^\w\s']|_")


def normalize(text: str) -> str:
    text = unicodedata.normalize("NFKC", text).lower().replace("’", "'")
    text = "".join(" " if unicodedata.category(ch).startswith("P") and ch != "'" else ch for ch in text)
    return re.sub(r"\s+", " ", text).strip()


def summarize(records: list[dict]) -> dict:
    import sacrebleu

    hypotheses = [record["hypothesis"] for record in records]
    references = [record["reference"] for record in records]
    bleu_metric = sacrebleu.BLEU()
    bleu = bleu_metric.corpus_score(hypotheses, [references])
    bleu_norm = sacrebleu.BLEU().corpus_score([normalize(h) for h in hypotheses],
                                              [[normalize(r) for r in references]])
    chrf_metric = sacrebleu.CHRF()
    chrf = chrf_metric.corpus_score(hypotheses, [references])
    before = [record.get("hypothesis_before_commit") for record in records]
    bleu_before = (sacrebleu.BLEU().corpus_score(before, [references]).score
                   if all(value is not None for value in before) else None)

    def stat(values):
        values = [value for value in values if value is not None]
        if not values:
            return None
        return {"mean": round(statistics.fmean(values), 3), "median": round(statistics.median(values), 3),
                "min": round(min(values), 3), "max": round(max(values), 3), "n": len(values)}

    compute = [record["sidecar_stats"]["compute_ms_mean"] for record in records if record.get("sidecar_stats")]
    p95 = [record["sidecar_stats"]["compute_ms_p95"] for record in records if record.get("sidecar_stats")]
    return {
        "utterances": len(records),
        "source_seconds_total": round(sum(record["source_seconds"] for record in records), 1),
        "bleu_13a_cased_punct_vs_raw_transcription": round(bleu.score, 2),
        "bleu_signature": str(bleu_metric.get_signature()),
        "bleu_lowercase_nopunct": round(bleu_norm.score, 2),
        "bleu_text_before_commit": round(bleu_before, 2) if bleu_before is not None else None,
        "chrf": round(chrf.score, 2),
        "chrf_signature": str(chrf_metric.get_signature()),
        "bleu_detail": str(bleu),
        "AL_model_s": stat([record["AL_model"] for record in records]),
        "AL_CA_wall_s": stat([record["AL_wall"] for record in records]),
        "LAAL_model_s": stat([record["LAAL_model"] for record in records]),
        "LAAL_CA_wall_s": stat([record["LAAL_wall"] for record in records]),
        "first_word_model_s": stat([record["first_word_model"] for record in records]),
        "first_word_wall_s": stat([record["first_word_wall"] for record in records]),
        "end_offset_wall_s": stat([record["end_offset_wall"] for record in records]),
        "turn_done_after_source_end_s": stat([record["turn_done_after_source_end_s"] for record in records]),
        "eos_reached": sum(1 for record in records if (record.get("sidecar_stats") or {}).get("eos")),
        "hypothesis_words_total": sum(len(record["hypothesis"].split()) for record in records),
        "reference_words_total": sum(len(record["reference"].split()) for record in records),
        "empty_hypotheses": sum(1 for record in records if not record["hypothesis"]),
        "errors": sum(len(record["errors"]) for record in records),
        "frame_compute_ms_mean": round(statistics.fmean(compute), 2) if compute else None,
        "frame_compute_ms_p95_mean": round(statistics.fmean(p95), 2) if p95 else None,
        "rtf_mean": round(statistics.fmean(compute) / (FRAME_SECONDS * 1000), 4) if compute else None,
        "examples": [{"id": record["id"], "source": record["source_text"], "hypothesis": record["hypothesis"],
                      "reference": record["reference"]} for record in records[:5]],
    }


def score(result: dict, pair: dict) -> dict:
    words = result["words"]
    source = result["source_seconds"]
    reference_words = len(pair["reference"].split())
    model = [word["d_model"] for word in words]
    wall = [word["d_wall"] for word in words]
    return {
        "AL_model": average_lagging(model, source, reference_words),
        "AL_wall": average_lagging(wall, source, reference_words),
        "LAAL_model": average_lagging(model, source, reference_words, len(words)),
        "LAAL_wall": average_lagging(wall, source, reference_words, len(words)),
        "first_word_model": model[0] if model else None,
        "first_word_wall": wall[0] if wall else None,
        "end_offset_wall": round(wall[-1] - source, 3) if wall else None,
    }


def write_wav(path: Path, pcm16: bytes, rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(1)
        handle.setsampwidth(2)
        handle.setframerate(rate)
        handle.writeframes(pcm16)


# --------------------------------------------------------------------------
# Commands


def sidecar_command(arguments) -> list[str]:
    # --timing-fields: the model-time step of each text piece, for d_model.
    command = [str(arguments.python), str(arguments.sidecar), "--timing-fields"]
    if arguments.sidecar_args:
        command += arguments.sidecar_args.split()
    return command


def command_run(arguments) -> None:
    pairs = load_pairs(arguments.offset, arguments.limit)
    RESULTS.mkdir(parents=True, exist_ok=True)
    log_path = RESULTS / "hibiki-sidecar-stderr.log"
    sidecar = SidecarProcess(sidecar_command(arguments), FLEURS_RATE, log_path)
    print(f"sidecar ready in {sidecar.load_seconds:.1f}s: {sidecar.ready}", flush=True)
    out = Path(arguments.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    try:
        with out.open("a") as handle:
            for position, pair in enumerate(pairs):
                result = translate(sidecar, pair["audio"], pair["rate"], mode=arguments.mode,
                                   trailing_seconds=arguments.trailing_seconds,
                                   max_wait_seconds=arguments.max_wait_seconds)
                audio = result.pop("output_audio")
                if arguments.offset + position < arguments.save_audio:
                    write_wav(RESULTS / "audio" / f"hibiki-{pair['id']}.wav", audio,
                              int(sidecar.ready.get("output_rate", 24000)))
                record = {"id": pair["id"], "source_text": pair["source_text"], "reference": pair["reference"],
                          "reference_normalized": pair["reference_normalized"], **result, **score(result, pair)}
                handle.write(json.dumps(record, ensure_ascii=False) + "\n")
                handle.flush()
                print(f"[{arguments.offset + position}] id={pair['id']} src={result['source_seconds']:.1f}s "
                      f"AL={record['AL_model']} AL_CA={record['AL_wall']} end+{record['end_offset_wall']} "
                      f"eos={(result['sidecar_stats'] or {}).get('eos')} "
                      f"rtf={(result['sidecar_stats'] or {}).get('rtf')}\n  hyp: {result['hypothesis']}\n"
                      f"  ref: {pair['reference']}", flush=True)
    finally:
        sidecar.close()


def command_summarize(arguments) -> None:
    records = [json.loads(line) for line in Path(arguments.jsonl).read_text().splitlines() if line.strip()]
    seen: dict[int, dict] = {}
    for record in records:
        seen[record["id"]] = record  # a rerun of an id replaces the earlier record
    summary = summarize(list(seen.values()))
    summary["jsonl"] = str(arguments.jsonl)
    text = json.dumps(summary, indent=2, ensure_ascii=False)
    if arguments.out:
        Path(arguments.out).write_text(text + "\n")
    print(text)


def command_asr_bleu(arguments) -> None:
    """Transcribe the saved translated speech and score it (ASR-BLEU).

    Uses a recogniser speaking the duplex-plan streaming ASR contract
    (tools/duplexmodels/common.py: start/chunk/finish, float32 16 kHz), by
    default the Kyutai STT 1B en/fr service on :9112. The score is of the
    speech Hibiki produced, as heard by an independent recogniser, so it
    includes that recogniser's errors.
    """
    import sacrebleu
    import requests
    import soxr

    records = [json.loads(line) for line in Path(arguments.jsonl).read_text().splitlines() if line.strip()]
    rows = []
    for record in records:
        wav = RESULTS / "audio" / f"hibiki-{record['id']}.wav"
        if not wav.exists():
            continue
        with wave.open(str(wav), "rb") as handle:
            rate = handle.getframerate()
            pcm = np.frombuffer(handle.readframes(handle.getnframes()), dtype="<i2").astype(np.float32) / 32768.0
        audio = soxr.resample(pcm, rate, 16_000).astype("<f4")
        session = requests.post(f"{arguments.url}/api/start", timeout=30).json()["session_id"]
        for begin in range(0, audio.size, 16_000):
            requests.post(f"{arguments.url}/api/chunk", params={"session_id": session},
                          data=audio[begin:begin + 16_000].tobytes(), timeout=60).raise_for_status()
        text = requests.post(f"{arguments.url}/api/finish", params={"session_id": session}, timeout=120).json()["text"]
        rows.append({"id": record["id"], "asr": text.strip(), "hypothesis": record["hypothesis"],
                     "reference": record["reference"]})
        print(f"{record['id']}: {text.strip()}", flush=True)
    if not rows:
        raise SystemExit("no saved output audio to transcribe")
    asr = [normalize(row["asr"]) for row in rows]
    text = [normalize(row["hypothesis"]) for row in rows]
    references = [normalize(row["reference"]) for row in rows]
    summary = {
        "utterances": len(rows), "recogniser": arguments.url,
        "asr_bleu_lowercase_nopunct": round(sacrebleu.BLEU().corpus_score(asr, [references]).score, 2),
        "text_bleu_lowercase_nopunct_same_subset": round(sacrebleu.BLEU().corpus_score(text, [references]).score, 2),
        "asr_vs_text_stream_bleu": round(sacrebleu.BLEU().corpus_score(asr, [text]).score, 2),
        "rows": rows,
    }
    health = requests.get(f"{arguments.url}/health", timeout=10).json()
    summary["recogniser_model"] = health.get("model")
    Path(arguments.out).write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n")
    print(json.dumps({key: value for key, value in summary.items() if key != "rows"}, indent=2))


def command_file(arguments) -> None:
    import sphn

    audio, _ = sphn.read(arguments.path, sample_rate=FLEURS_RATE)
    audio = audio[0].astype(np.float32)
    if arguments.seconds:
        audio = audio[: int(arguments.seconds * FLEURS_RATE)]
    sidecar = SidecarProcess(sidecar_command(arguments), FLEURS_RATE, RESULTS / "hibiki-sidecar-stderr.log")
    print(f"sidecar ready in {sidecar.load_seconds:.1f}s", flush=True)
    try:
        result = translate(sidecar, audio, FLEURS_RATE, mode=arguments.mode,
                           trailing_seconds=arguments.trailing_seconds, max_wait_seconds=arguments.max_wait_seconds)
    finally:
        sidecar.close()
    audio_out = result.pop("output_audio")
    if arguments.wav:
        write_wav(Path(arguments.wav), audio_out, 24000)
    print(json.dumps(result, indent=1, ensure_ascii=False))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("download", help="fetch the FLEURS fr_fr and en_us test parquet files")

    def add_sidecar(p):
        p.add_argument("--python", default=str(DEFAULT_PYTHON))
        p.add_argument("--sidecar", default=str(DEFAULT_SIDECAR))
        p.add_argument("--sidecar-args", default="", help="extra sidecar arguments, space separated")
        p.add_argument("--mode", choices=["commit", "continuous"], default="commit")
        p.add_argument("--trailing-seconds", type=float, default=6.0, help="continuous mode: silence before commit")
        p.add_argument("--max-wait-seconds", type=float, default=30.0)

    run_parser = sub.add_parser("run", help="replay FLEURS utterances at real time")
    add_sidecar(run_parser)
    run_parser.add_argument("--offset", type=int, default=0)
    run_parser.add_argument("--limit", type=int, default=10)
    run_parser.add_argument("--out", default=str(RESULTS / "hibiki-fleurs-fr-en.jsonl"))
    run_parser.add_argument("--save-audio", type=int, default=5, help="save output speech for the first N utterances")

    summary_parser = sub.add_parser("summarize", help="corpus metrics from a results JSONL")
    summary_parser.add_argument("--jsonl", default=str(RESULTS / "hibiki-fleurs-fr-en.jsonl"))
    summary_parser.add_argument("--out", default=str(RESULTS / "hibiki-fleurs-fr-en-summary.json"))

    asr_parser = sub.add_parser("asr-bleu", help="score saved translated speech through a streaming recogniser")
    asr_parser.add_argument("--jsonl", default=str(RESULTS / "hibiki-fleurs-fr-en.jsonl"))
    asr_parser.add_argument("--url", default="http://127.0.0.1:9112")
    asr_parser.add_argument("--out", default=str(RESULTS / "hibiki-fleurs-fr-en-asr-bleu.json"))

    file_parser = sub.add_parser("file", help="translate one audio file (smoke test)")
    add_sidecar(file_parser)
    file_parser.add_argument("--path", required=True)
    file_parser.add_argument("--seconds", type=float, default=0.0)
    file_parser.add_argument("--wav", default="")

    arguments = parser.parse_args()
    if arguments.command == "download":
        download()
    elif arguments.command == "run":
        command_run(arguments)
    elif arguments.command == "summarize":
        command_summarize(arguments)
    elif arguments.command == "asr-bleu":
        command_asr_bleu(arguments)
    elif arguments.command == "file":
        command_file(arguments)


if __name__ == "__main__":
    main()
