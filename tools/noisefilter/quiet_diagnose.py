"""Diagnose DeepFilterNet quiet-speech suppression in the service streams.

Replays clean FDB v1.5 user-backchannel recordings through fresh
``DeepFilterStream``/``FIRDeepFilterStream`` states, as the earlier
quiet-clean probe did, and additionally records libDF's per-frame local SNR
(the return value of ``df_process_frame``) and how many output frames are
exactly zero. Variants isolate the conversion from the model:

``legacy``/``fir``      the two service streams at 16 kHz input.
``native48``            the same recording converted offline to 48 kHz with
                        ``resample_poly`` and fed straight to the model.
``fir+dither``          FIR stream with +/-1 LSB TPDF dither added before PCM16
                        rounding, so scaled input never contains digital zeros.

Usage (perception venv)::

    python tools/noisefilter/quiet_diagnose.py --library .runtime/deepfilter-filter/libdf.so \
        --model .runtime/deepfilter-filter/DeepFilterNet3_onnx.tar.gz --out report.json
"""
import argparse
import ctypes
import json
import sys
from pathlib import Path

import numpy as np
import soundfile as sf
from scipy.signal import resample_poly

sys.path.insert(0, str(Path(__file__).resolve().parent))
from server import DeepFilterNet, DeepFilterStream, FIRDeepFilterStream  # noqa: E402

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / ".runtime/full-duplex-bench-v1.5/dataset/user_backchannel"
# The 20 IDs of deploy/duplex/evidence/deepfilternet/20260923-fir/quiet-clean.json.
SAMPLES = ["1", "2", "22", "27", "34", "39", "43", "48", "49", "52",
           "57", "67", "68", "78", "80", "81", "87", "90", "92", "98"]


def power_db(x):
    return float(10 * np.log10(np.mean(np.square(x, dtype=np.float64)) + 1e-12))


class Traced:
    """Wraps a stream so each model frame's local SNR is kept."""

    def __init__(self, stream):
        self.stream, self.lsnr = stream, []
        lib, state = stream.model.lib, stream.state

        def frame():
            self.lsnr.append(float(lib.df_process_frame(state, stream.buffer, stream.buffer)))
        stream.process_frame = frame


def run_stream(model, cls, pcm16):
    traced = Traced(cls(model, 16000))
    out = []
    for i in range(0, len(pcm16), 1600):
        out.append(np.frombuffer(traced.stream.process(pcm16[i:i + 1600].astype("<i2").tobytes()), "<i2"))
    traced.stream.close()
    return np.concatenate(out).astype(np.float64), traced.lsnr


def run_native48(model, x16):
    x48 = resample_poly(x16, 3, 1) / 32768.0
    state = model.state()
    buf = (ctypes.c_float * 480)()
    out, lsnr = [], []
    for i in range(0, len(x48) // 480 * 480, 480):
        buf[:] = x48[i:i + 480].tolist()
        lsnr.append(float(model.lib.df_process_frame(state, buf, buf)))
        out.append(np.asarray(buf, dtype=np.float64))
    model.lib.df_free(state)
    y48 = np.concatenate(out) * 32768.0
    return resample_poly(y48, 1, 3), lsnr


def retention(x, y, delay):
    n = min(len(x), len(y) - delay)
    return power_db(y[delay:delay + n]) - power_db(x[:n])


def summarise(lsnr, y):
    frames = len(y) // 160
    zero = sum(not np.any(y[i * 160:(i + 1) * 160]) for i in range(frames))
    arr = np.asarray(lsnr)
    return {"lsnr_median": float(np.median(arr)), "lsnr_max": float(arr.max()),
            "lsnr_min": float(arr.min()), "frames": frames, "zero_output_frames": int(zero)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--atten-lim-db", type=float, default=100.0)
    parser.add_argument("--levels", default="0,-30")
    parser.add_argument("--variants", default="legacy,fir,native48,fir+dither")
    parser.add_argument("--file", default="input.wav", help="fixture file (input.wav or clean_input.wav)")
    parser.add_argument("--out", required=True)
    args = parser.parse_args()
    model = DeepFilterNet(args.library, args.model, atten_lim_db=args.atten_lim_db, pool=0)
    rng = np.random.default_rng(0)
    records = []
    for sample in SAMPLES:
        source, rate = sf.read(FIXTURES / sample / args.file, dtype="float64")
        assert rate == 16000
        source = source * 32768.0
        tail = np.zeros(1600)
        for level in [float(v) for v in args.levels.split(",")]:
            scaled = source * 10 ** (level / 20)
            pcm = np.clip(np.rint(scaled), -32768, 32767)
            reference = np.concatenate([pcm, tail])
            for variant in args.variants.split(","):
                if variant == "legacy":
                    y, lsnr = run_stream(model, DeepFilterStream, reference)
                    delay = 640
                elif variant == "fir":
                    y, lsnr = run_stream(model, FIRDeepFilterStream, reference)
                    delay = 672
                elif variant == "fir+dither":
                    dithered = np.clip(np.rint(scaled + rng.uniform(-.5, .5, len(scaled))
                                               + rng.uniform(-.5, .5, len(scaled))), -32768, 32767)
                    y, lsnr = run_stream(model, FIRDeepFilterStream, np.concatenate([dithered, tail]))
                    delay = 672
                elif variant == "native48":
                    y, lsnr = run_native48(model, reference)
                    delay = 480  # model's 30 ms (1440 samples at 48 kHz)
                else:
                    raise SystemExit(f"unknown variant {variant}")
                record = {"sample": sample, "level_db": level, "variant": variant,
                          "input_power_dbfs": power_db(pcm / 32768.0),
                          "input_zero_fraction": float(np.mean(pcm == 0)),
                          "retention_db": retention(reference, y, delay), **summarise(lsnr, y)}
                records.append(record)
                print(json.dumps(record), flush=True)
    summary = {}
    for r in records:
        key = f"{r['variant']}|{r['level_db']:g}"
        s = summary.setdefault(key, {"clips": 0, "over_60db_loss": 0, "mean_retention_db": 0.0})
        s["clips"] += 1
        s["over_60db_loss"] += r["retention_db"] < -60
        s["mean_retention_db"] += r["retention_db"] / len(SAMPLES)
    Path(args.out).write_text(json.dumps({"atten_lim_db": args.atten_lim_db, "file": args.file, "summary": summary,
                                          "records": records}, indent=1))
    print(json.dumps(summary, indent=1))


if __name__ == "__main__":
    main()
