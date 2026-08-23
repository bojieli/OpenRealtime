#!/usr/bin/env python3
"""SenseVoiceSmall behind the OpenAI transcription route.

OpenRealtime's `openai-compatible` recogniser posts a mono 16-bit WAV as
multipart `file` and reads `{"text": ...}` back, so nothing in the engine needs
to know what is serving that route.

The server this replaces answered /api/start in a millisecond while never
answering a chunk again, so two things here are deliberate:

  * /health runs audio through the model rather than returning a constant.
    Reachability is not liveness for a recogniser: the port and the HTTP layer
    stay up long after the thing behind them has stopped answering.
  * Inference holds a lock and runs off the event loop, and every request is
    bounded by a deadline. A recogniser that queues without limit turns one
    slow call into a stall that outlives it.
"""
import asyncio, io, logging, os, threading, time

import numpy as np
import soundfile as sf
from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import JSONResponse

MODEL_ID = os.environ.get("SENSEVOICE_MODEL", "iic/SenseVoiceSmall")
DEVICE = os.environ.get("SENSEVOICE_DEVICE", "cuda:0")
TARGET_RATE = 16_000
# One utterance may not exceed this. SenseVoice runs at roughly a hundredth of
# real time, so anything near it means something upstream is wrong.
REQUEST_DEADLINE = float(os.environ.get("SENSEVOICE_DEADLINE", "20"))
MAX_SECONDS = float(os.environ.get("SENSEVOICE_MAX_SECONDS", "600"))

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("sensevoice")

app = FastAPI()
_model = None
_lock = threading.Lock()
_stats = {"requests": 0, "failures": 0, "elapsed": 0.0, "max_elapsed": 0.0, "audio_seconds": 0.0}


def load():
    global _model
    from funasr import AutoModel
    started = time.time()
    _model = AutoModel(model=MODEL_ID, trust_remote_code=False, vad_model=None,
                       device=DEVICE, disable_update=True)
    log.info("loaded %s on %s in %.1fs", MODEL_ID, DEVICE, time.time() - started)


def decode(raw: bytes):
    """Return mono float32 at 16 kHz. The engine sends WAV; soundfile also
    reads the other containers a curious operator might try by hand."""
    samples, rate = sf.read(io.BytesIO(raw), dtype="float32", always_2d=True)
    samples = samples.mean(axis=1)
    if rate != TARGET_RATE:
        if len(samples) == 0:
            return samples, TARGET_RATE
        count = int(round(len(samples) * TARGET_RATE / rate))
        samples = np.interp(
            np.linspace(0, len(samples) - 1, count, dtype=np.float64),
            np.arange(len(samples), dtype=np.float64), samples,
        ).astype(np.float32)
    return samples, TARGET_RATE


def transcribe(samples: np.ndarray) -> str:
    from funasr.utils.postprocess_utils import rich_transcription_postprocess
    # The model is one graph on one device; serialising is what keeps a burst
    # of sessions from interleaving into it.
    with _lock:
        result = _model.generate(input=samples, fs=TARGET_RATE, cache={},
                                 language="auto", use_itn=True, batch_size_s=300)
    if not result:
        return ""
    return rich_transcription_postprocess(result[0]["text"]).strip()


async def run(samples: np.ndarray) -> str:
    return await asyncio.wait_for(asyncio.to_thread(transcribe, samples), timeout=REQUEST_DEADLINE)


@app.on_event("startup")
def startup():
    load()


@app.post("/v1/audio/transcriptions")
async def transcriptions(file: UploadFile = File(...), model: str = Form(default=MODEL_ID),
                         language: str = Form(default=""), prompt: str = Form(default="")):
    started = time.time()
    raw = await file.read()
    try:
        samples, _ = decode(raw)
    except Exception as error:
        _stats["failures"] += 1
        raise HTTPException(status_code=400, detail=f"cannot decode audio: {error}")

    seconds = len(samples) / TARGET_RATE
    if seconds > MAX_SECONDS:
        _stats["failures"] += 1
        raise HTTPException(status_code=413, detail=f"utterance of {seconds:.0f}s exceeds {MAX_SECONDS:.0f}s")
    # Too short to contain a word. Answering with empty text keeps the caller's
    # turn moving; refusing it would fail a session over trailing silence.
    if seconds < 0.10:
        return JSONResponse({"text": "", "language": ""})

    try:
        text = await run(samples)
    except asyncio.TimeoutError:
        _stats["failures"] += 1
        log.error("deadline exceeded after %.1fs on %.2fs of audio", REQUEST_DEADLINE, seconds)
        raise HTTPException(status_code=504, detail="recognition deadline exceeded")
    except Exception as error:
        _stats["failures"] += 1
        log.exception("recognition failed")
        raise HTTPException(status_code=500, detail=str(error))

    elapsed = time.time() - started
    _stats["requests"] += 1
    _stats["elapsed"] += elapsed
    _stats["max_elapsed"] = max(_stats["max_elapsed"], elapsed)
    _stats["audio_seconds"] += seconds
    log.info("%.2fs audio in %.3fs (rtf %.3f) -> %d chars", seconds, elapsed,
             elapsed / seconds if seconds else 0.0, len(text))
    return JSONResponse({"text": text, "language": language or "auto"})


@app.get("/health")
async def health():
    """Round-trip a tone through the model. A recogniser that has stopped
    answering still accepts connections, so a constant here would report the
    exact failure this endpoint exists to catch as healthy."""
    tone = (0.05 * np.sin(2 * np.pi * 440 * np.arange(TARGET_RATE // 2) / TARGET_RATE)).astype(np.float32)
    started = time.time()
    try:
        await asyncio.wait_for(asyncio.to_thread(transcribe, tone), timeout=REQUEST_DEADLINE)
    except Exception as error:
        return JSONResponse({"status": "unhealthy", "error": str(error)}, status_code=503)
    served = _stats["requests"]
    return JSONResponse({
        "status": "ok", "model": MODEL_ID, "device": DEVICE,
        "probe_seconds": round(time.time() - started, 3),
        "requests": served, "failures": _stats["failures"],
        "mean_seconds": round(_stats["elapsed"] / served, 3) if served else 0.0,
        "max_seconds": round(_stats["max_elapsed"], 3),
        "rtf": round(_stats["elapsed"] / _stats["audio_seconds"], 4) if _stats["audio_seconds"] else 0.0,
    })
