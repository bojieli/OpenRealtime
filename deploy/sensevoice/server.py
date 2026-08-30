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
import asyncio, hashlib, io, logging, os, pathlib, threading, time

import numpy as np
import soundfile as sf
from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import JSONResponse

MODEL_ID = os.environ.get("SENSEVOICE_MODEL", "iic/SenseVoiceSmall")
MODEL_PATH = os.environ.get("SENSEVOICE_MODEL_PATH", "").strip()
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
_deployment = {"revision": "", "digest": "", "service_path": "", "service_digest": ""}


def _digest_field(hasher, name: str, value: str):
    name_bytes = name.encode("utf-8")
    value_bytes = value.encode("utf-8")
    hasher.update(str(len(name_bytes)).encode("ascii"))
    hasher.update(b":")
    hasher.update(name_bytes)
    hasher.update(str(len(value_bytes)).encode("ascii"))
    hasher.update(b":")
    hasher.update(value_bytes)


def loaded_model_digest(path: str) -> str:
    """Return the exact cross-language digest the host independently verifies."""
    root = pathlib.Path(path).resolve(strict=True)
    if not root.is_dir():
        raise RuntimeError("SENSEVOICE_MODEL_PATH is not a directory")
    files = []
    for candidate in root.rglob("*"):
        if candidate.is_dir():
            if candidate.name == "__pycache__":
                continue
            continue
        if candidate.name.endswith(".pyc"):
            continue
        if candidate.is_symlink() or not candidate.is_file():
            raise RuntimeError(f"model material is not a regular file: {candidate}")
        files.append(candidate)
    if not files:
        raise RuntimeError("SENSEVOICE_MODEL_PATH is empty")
    files.sort(key=lambda item: item.relative_to(root).as_posix())
    manifest = hashlib.sha256()
    _digest_field(manifest, "format", "openrealtime.loaded-model.v1")
    for candidate in files:
        relative = candidate.relative_to(root).as_posix()
        size = candidate.stat().st_size
        content = hashlib.sha256()
        with candidate.open("rb") as source:
            while True:
                chunk = source.read(1024 * 1024)
                if not chunk:
                    break
                content.update(chunk)
        if candidate.stat().st_size != size:
            raise RuntimeError(f"model material changed while hashing: {candidate}")
        _digest_field(manifest, "file", relative)
        _digest_field(manifest, "size", str(size))
        _digest_field(manifest, "sha256", "sha256:" + content.hexdigest())
    return "sha256:" + manifest.hexdigest()


def service_module_identity():
    """Bind health to the exact source module imported by this process."""
    path = pathlib.Path(__file__).resolve(strict=True)
    if not path.is_file():
        raise RuntimeError("loaded SenseVoice service module is not a regular file")
    content = hashlib.sha256()
    with path.open("rb") as source:
        while True:
            chunk = source.read(1024 * 1024)
            if not chunk:
                break
            content.update(chunk)
    return str(path), "sha256:" + content.hexdigest()


def load():
    global _model
    from funasr import AutoModel
    if not MODEL_PATH:
        raise RuntimeError("SENSEVOICE_MODEL_PATH is required for deployment attestation")
    service_path, service_digest = service_module_identity()
    _deployment["service_path"] = service_path
    _deployment["service_digest"] = service_digest
    digest = loaded_model_digest(MODEL_PATH)
    _deployment["digest"] = digest
    _deployment["revision"] = "content-" + digest.removeprefix("sha256:")[:20]
    started = time.time()
    _model = AutoModel(model=MODEL_PATH, trust_remote_code=False, vad_model=None,
                       device=DEVICE, disable_update=True)
    log.info("loaded %s (%s) on %s in %.1fs", MODEL_ID, _deployment["revision"],
             DEVICE, time.time() - started)


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


def transcribe(samples: np.ndarray, requested: str = "") -> str:
    """Recognise one buffer.

    The requested language is honoured rather than accepted and dropped. The
    endpoint took the field from the first version and always passed "auto" to
    the model, which is fine until somebody is interpreting: auto-detection on
    a short utterance in a second language guesses the first one, and what
    comes back is the right sounds spelled in the wrong language.
    """
    from funasr.utils.postprocess_utils import rich_transcription_postprocess
    # The model is one graph on one device; serialising is what keeps a burst
    # of sessions from interleaving into it.
    with _lock:
        result = _model.generate(input=samples, fs=TARGET_RATE, cache={},
                                 language=requested or "auto", use_itn=True, batch_size_s=300)
    if not result:
        return ""
    return rich_transcription_postprocess(result[0]["text"]).strip()


async def run(samples: np.ndarray, requested: str = "") -> str:
    return await asyncio.wait_for(
        asyncio.to_thread(transcribe, samples, requested), timeout=REQUEST_DEADLINE)


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
        text = await run(samples, (language or "").strip().lower())
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
        "revision": _deployment["revision"], "digest": _deployment["digest"],
        "service_path": _deployment["service_path"],
        "service_digest": _deployment["service_digest"],
        "probe_seconds": round(time.time() - started, 3),
        "requests": served, "failures": _stats["failures"],
        "mean_seconds": round(_stats["elapsed"] / served, 3) if served else 0.0,
        "max_seconds": round(_stats["max_elapsed"], 3),
        "rtf": round(_stats["elapsed"] / _stats["audio_seconds"], 4) if _stats["audio_seconds"] else 0.0,
    })
