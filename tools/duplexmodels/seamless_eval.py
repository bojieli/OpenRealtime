#!/usr/bin/env python3
"""SeamlessStreaming simultaneous speech-to-text translation (plan cell X0).

A translation task profile, separate from conversational endpoint scoring
(plan section 7.9): it reports quality (BLEU) *and* lag, because ordinary
endpoint latency does not characterise simultaneous translation.

The run uses Meta's own streaming agent pipeline and SimulEval driver
(``streaming_evaluate --task s2tt``, i.e. OnlineFeatureExtractor ->
OfflineWav2VecBertEncoder -> MMA monotonic text decoder, 320 ms source
segments, decision threshold 0.5) on paired FLEURS test utterances. Scoring is
recomputed here from SimulEval's ``instances.log``:

* BLEU with sacrebleu (``zh`` tokenizer for Chinese targets, ``13a`` otherwise);
* lag in source time: SimulEval's AL / LAAL (units = SentencePiece tokens,
  as in the Seamless paper), plus directly readable per-utterance numbers -
  first-emission lag and the source time at which each emitted unit
  was produced (``delays``), and, when ``--computation-aware`` is set, the
  wall-clock-inclusive ``elapsed`` times.

Audio is **not** silence-stripped (``--no-strip-silence``) so lag is measured
from the start of the audio a live system would receive.

License: SeamlessStreaming weights are CC-BY-NC 4.0 (non-commercial); the
seamless_communication code is MIT/CC-BY-NC per file (see its LICENSE).

    V=.runtime/duplex-plan/venvs/seamless-cu128/bin/python
    $V tools/duplexmodels/seamless_eval.py prepare --src en_us --tgt cmn_hans_cn --count 30 \
        --out .runtime/duplex-plan/results/translation/seamless-data/en-cmn
    $V tools/duplexmodels/seamless_eval.py run --data .runtime/duplex-plan/results/translation/seamless-data/en-cmn \
        --tgt-lang cmn --output .runtime/duplex-plan/results/translation/seamless-en-cmn
    $V tools/duplexmodels/seamless_eval.py score --output .runtime/duplex-plan/results/translation/seamless-en-cmn --tgt-lang cmn
"""

from __future__ import annotations

import argparse
import io
import json
import os
import random
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PLAN = ROOT / ".runtime/duplex-plan"
FLEURS = PLAN / "data/fleurs/parquet-data"
HF_SNAPSHOT_GLOB = "models--facebook--seamless-streaming/snapshots/*"


def snapshot() -> Path:
    hub = Path(os.environ.get("HF_HUB_CACHE", Path.home() / ".cache/huggingface/hub"))
    matches = sorted(hub.glob(HF_SNAPSHOT_GLOB))
    if not matches:
        raise SystemExit("facebook/seamless-streaming is not in the HF cache")
    return matches[-1]


def write_cards(directory: Path) -> Path:
    """fairseq2 ``@user`` overrides so the cards resolve to the HF-cache files
    instead of downloading them again."""
    snap = snapshot()
    directory.mkdir(parents=True, exist_ok=True)
    (directory / "seamless_streaming_unity.yaml").write_text(
        "name: seamless_streaming_unity@user\n"
        f"checkpoint: \"file://{snap / 'seamless_streaming_unity.pt'}\"\n"
        f"char_tokenizer: \"file://{snap / 'spm_char_lang38_tc.model'}\"\n")
    (directory / "seamless_streaming_monotonic_decoder.yaml").write_text(
        "name: seamless_streaming_monotonic_decoder@user\n"
        f"checkpoint: \"file://{snap / 'seamless_streaming_monotonic_decoder.pt'}\"\n")
    (directory / "unity_nllb-100.yaml").write_text(
        "name: unity_nllb-100@user\n"
        f"tokenizer: \"file://{snap / 'tokenizer.model'}\"\n")
    (directory / "vocoder_v2.yaml").write_text(
        "name: vocoder_v2@user\n"
        f"checkpoint: \"file://{snap / 'vocoder_v2.pt'}\"\n")
    return directory


# --------------------------------------------------------------------------
# prepare


def prepare(args) -> None:
    import numpy as np
    import pyarrow.parquet as pq
    import soundfile as sf
    from scipy.signal import resample_poly

    src = pq.read_table(FLEURS / args.src / "test-00000-of-00001.parquet").to_pylist()
    tgt = pq.read_table(FLEURS / args.tgt / "test-00000-of-00001.parquet",
                        columns=["id", "raw_transcription", "transcription"]).to_pylist()
    references = {}
    for row in tgt:
        references.setdefault(row["id"], row["raw_transcription"] or row["transcription"])
    seen, rows = set(), []
    for row in src:
        if row["id"] in references and row["id"] not in seen:
            seen.add(row["id"])
            rows.append(row)
    random.Random(args.seed).shuffle(rows)
    out = Path(args.out)
    (out / "audio").mkdir(parents=True, exist_ok=True)
    lines = ["id\taudio\ttgt_text\tsrc_text\tduration_s"]
    for row in rows[: args.count]:
        cell = row["audio"]
        samples, rate = sf.read(io.BytesIO(cell["bytes"]), dtype="float32")
        if samples.ndim > 1:
            samples = samples.mean(axis=1)
        if rate != 16_000:
            from math import gcd

            g = gcd(rate, 16_000)
            samples = resample_poly(samples, 16_000 // g, rate // g).astype("float32")
        name = f"audio/{row['id']}.wav"
        sf.write(out / name, samples, 16_000, subtype="PCM_16")
        clean = lambda s: " ".join(str(s).split()).replace("\t", " ")  # noqa: E731
        lines.append(f"{row['id']}\t{name}\t{clean(references[row['id']])}\t{clean(row['raw_transcription'])}"
                     f"\t{len(samples) / 16_000:.3f}")
    (out / "data.tsv").write_text("\n".join(lines) + "\n")
    print(f"wrote {len(lines) - 1} pairs to {out / 'data.tsv'}")


# --------------------------------------------------------------------------
# run


def run(args) -> None:
    data = Path(args.data)
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    cards = write_cards(PLAN / "venvs" / "seamless-assets")
    env = dict(os.environ, FAIRSEQ2_USER_ASSET_DIR=str(cards), PYTHONWARNINGS="ignore")
    cli = Path(sys.executable).parent / "streaming_evaluate"
    command = [str(cli), "--task", "s2tt", "--data-file", str(data / "data.tsv"),
               "--audio-root-dir", str(data), "--output", str(output), "--tgt-lang", args.tgt_lang,
               "--no-strip-silence", "--device", args.device, "--dtype", args.dtype]
    if args.computation_aware:
        command.append("--computation-aware")
    if args.end_index:
        command += ["--end-index", str(args.end_index)]
    began = time.time()
    print(" ".join(command), flush=True)
    result = subprocess.run(command, env=env, cwd=ROOT)
    (output / "run.json").write_text(json.dumps({
        "command": command, "exit_code": result.returncode, "wall_seconds": round(time.time() - began, 1),
        "device": args.device, "dtype": args.dtype, "snapshot": str(snapshot()),
        "python": sys.executable}, indent=2))
    if result.returncode:
        raise SystemExit(result.returncode)


# --------------------------------------------------------------------------
# score


def score(args) -> dict:
    import numpy as np
    import sacrebleu

    output = Path(args.output)
    instances = [json.loads(line) for line in (output / "instances.log").read_text().splitlines() if line.strip()]
    chinese = args.tgt_lang in ("cmn", "cmn_Hant", "yue", "zho")
    predictions = [i["prediction"].strip() for i in instances]
    references = [i["reference"].strip() for i in instances]
    bleu = sacrebleu.corpus_bleu(predictions, [references], tokenize="zh" if chinese else "13a")
    chrf = sacrebleu.corpus_chrf(predictions, [references])
    per = []
    for inst in instances:
        delays = inst.get("delays") or []
        source_ms = float(inst.get("source_length", 0))
        elapsed = inst.get("elapsed") or []
        per.append({
            "index": inst["index"], "source_ms": source_ms, "units": len(delays),
            "first_emission_source_ms": delays[0] if delays else None,
            "last_emission_source_ms": delays[-1] if delays else None,
            "units_emitted_before_source_end": sum(d < source_ms for d in delays),
            "first_emission_elapsed_ms": elapsed[0] if elapsed else None,
            "end_offset_elapsed_ms": (elapsed[-1] - source_ms) if elapsed else None,
            "prediction": inst["prediction"], "reference": inst["reference"],
        })
    scores = {}
    for name in ("scores", "scores.tsv"):
        path = output / name
        if path.exists():
            scores["simuleval_raw"] = path.read_text()
    first = [p["first_emission_source_ms"] for p in per if p["first_emission_source_ms"] is not None]
    frac = [p["units_emitted_before_source_end"] / p["units"] for p in per if p["units"]]
    summary = {
        "utterances": len(instances), "tgt_lang": args.tgt_lang,
        "bleu": round(bleu.score, 2), "bleu_tokenizer": "zh" if chinese else "13a", "bleu_signature": str(bleu),
        "chrf": round(chrf.score, 2),
        "mean_source_s": round(float(np.mean([p["source_ms"] for p in per])) / 1000, 2),
        "first_emission_source_ms": {"mean": round(float(np.mean(first)), 0),
                                     "p50": round(float(np.percentile(first, 50)), 0),
                                     "p90": round(float(np.percentile(first, 90)), 0)} if first else None,
        "fraction_of_units_emitted_before_source_end": round(float(np.mean(frac)), 3) if frac else None,
    }
    fe = [p["first_emission_elapsed_ms"] for p in per if p["first_emission_elapsed_ms"] is not None]
    eo = [p["end_offset_elapsed_ms"] for p in per if p["end_offset_elapsed_ms"] is not None]
    if fe:
        summary["computation_aware"] = {
            "first_emission_elapsed_ms_mean": round(float(np.mean(fe)), 0),
            "end_offset_elapsed_ms_mean": round(float(np.mean(eo)), 0),
            "end_offset_elapsed_ms_p90": round(float(np.percentile(eo, 90)), 0),
        }
    result = {"summary": summary, **scores, "utterances": per}
    (output / "summary.json").write_text(json.dumps(result, indent=2, ensure_ascii=False))
    print(json.dumps({"summary": summary, **scores}, indent=2, ensure_ascii=False))
    return result


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("prepare")
    p.add_argument("--src", default="en_us")
    p.add_argument("--tgt", default="cmn_hans_cn")
    p.add_argument("--count", type=int, default=30)
    p.add_argument("--seed", type=int, default=7)
    p.add_argument("--out", required=True)
    r = sub.add_parser("run")
    r.add_argument("--data", required=True)
    r.add_argument("--output", required=True)
    r.add_argument("--tgt-lang", required=True, help="3-letter Seamless code, e.g. cmn, eng")
    r.add_argument("--device", default="cuda:0")
    r.add_argument("--dtype", default="fp16")
    r.add_argument("--end-index", type=int, default=0)
    r.add_argument("--computation-aware", action="store_true")
    s = sub.add_parser("score")
    s.add_argument("--output", required=True)
    s.add_argument("--tgt-lang", required=True)
    args = parser.parse_args()
    {"prepare": prepare, "run": run, "score": score}[args.command](args)


if __name__ == "__main__":
    main()
