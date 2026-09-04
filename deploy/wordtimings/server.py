#!/usr/bin/env python3
"""Word timestamps for the agent's own synthesised speech.

OpenRealtime asks this the one question it cannot answer itself: given the
audio it just played, where inside it did each word fall? It already knows what
the words were - it wrote them - so nothing here is asked to decide the wording.
Only the times are wanted, and the runtime reconciles them onto its own text.

The route is the published transcription shape rather than a bespoke one:
multipart POST of a WAV, `response_format=verbose_json`,
`timestamp_granularities[]=word`, and a `words` array of `{word, start, end}` in
seconds. Anything else that answers that request - a hosted API, another local
server - is a drop-in replacement, and the runtime is not told which it got.

Two things are deliberate, and both come from watching a recogniser fail in
production rather than from a design document:

  * /health runs audio through the model rather than returning a constant.
    Reachability is not liveness: the port and the HTTP layer stay up long
    after the thing behind them has stopped answering, and a word-timing
    service that has quietly stopped answering degrades a runtime silently -
    every boundary falls back to an estimate and nothing says so.
  * Inference holds a lock, runs off the event loop, and is bounded by a
    deadline. This sits beside a realtime voice: a call that queues without
    limit turns one slow request into a stall that outlives the utterance it
    was about.
"""
import asyncio, io, logging, os, threading, time

import numpy as np
import soundfile as sf
from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import JSONResponse

MODEL_ID = os.environ.get("WORD_TIMINGS_MODEL", "Systran/faster-whisper-base.en")
DEVICE = os.environ.get("WORD_TIMINGS_DEVICE", "cuda")
COMPUTE_TYPE = os.environ.get("WORD_TIMINGS_COMPUTE_TYPE", "float16")
LANGUAGE = os.environ.get("WORD_TIMINGS_LANGUAGE", "").strip() or None
TARGET_RATE = 16_000
# One utterance. A sentence of synthesised speech is a second or two, and a
# request near this bound means something upstream is sending a recording
# rather than an utterance.
REQUEST_DEADLINE = float(os.environ.get("WORD_TIMINGS_DEADLINE", "10"))
MAX_SECONDS = float(os.environ.get("WORD_TIMINGS_MAX_SECONDS", "120"))

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("wordtimings")

app = FastAPI()
_model = None
_lock = threading.Lock()
_stats = {"requests": 0, "failures": 0, "elapsed": 0.0, "max_elapsed": 0.0,
          "audio_seconds": 0.0, "words": 0}


def load():
    global _model
    from faster_whisper import WhisperModel
    started = time.time()
    _model = WhisperModel(MODEL_ID, device=DEVICE, compute_type=COMPUTE_TYPE)
    log.info("loaded %s on %s in %.1fs", MODEL_ID, DEVICE, time.time() - started)


def decode(payload: bytes):
    """Read the upload as mono float32 at the rate the model wants."""
    samples, rate = sf.read(io.BytesIO(payload), dtype="float32", always_2d=True)
    samples = samples.mean(axis=1)
    if samples.size == 0:
        return samples, 0.0
    seconds = samples.size / float(rate)
    if seconds > MAX_SECONDS:
        raise HTTPException(status_code=413,
                            detail=f"{seconds:.1f}s exceeds the {MAX_SECONDS:.0f}s bound")
    if rate != TARGET_RATE:
        # Linear resampling. What is being resampled is synthesised speech
        # about to be timed, and the artefacts sit far above the band that
        # decides where a word boundary is.
        count = int(round(samples.size * TARGET_RATE / rate))
        if count <= 0:
            return np.zeros(0, dtype="float32"), 0.0
        position = np.linspace(0.0, samples.size - 1.0, count, dtype="float64")
        samples = np.interp(position, np.arange(samples.size), samples).astype("float32")
    return samples, seconds


def transcribe(samples, language):
    """Return the words and their times, in seconds from the start of the clip."""
    if _model is None:
        raise HTTPException(status_code=503, detail="the model is not loaded")
    with _lock:
        segments, _ = _model.transcribe(
            samples, language=language or LANGUAGE, word_timestamps=True,
            # Nothing here is a conversation: it is one utterance the runtime
            # already has the text of. Turning off the condition-on-previous
            # behaviour keeps one clip's timing independent of the last one's.
            condition_on_previous_text=False,
        )
        words, text = [], []
        for segment in segments:
            text.append(segment.text)
            for word in (segment.words or []):
                words.append({
                    "word": word.word.strip(),
                    "start": round(float(word.start), 3),
                    "end": round(float(word.end), 3),
                })
    return "".join(text).strip(), words


async def bounded(function, *arguments):
    try:
        return await asyncio.wait_for(asyncio.to_thread(function, *arguments), REQUEST_DEADLINE)
    except asyncio.TimeoutError:
        raise HTTPException(status_code=504,
                            detail=f"word timing exceeded {REQUEST_DEADLINE:.0f}s")


@app.on_event("startup")
def startup():
    load()


@app.post("/v1/audio/transcriptions")
async def transcriptions(
    file: UploadFile = File(...),
    model: str = Form(default=""),
    language: str = Form(default=""),
    response_format: str = Form(default="json"),
    # FastAPI cannot name a field with brackets as a Python identifier, so the
    # published spelling is bound explicitly. Both spellings are accepted
    # because servers in the field differ on which one they take.
    timestamp_granularities: list[str] = Form(default=[], alias="timestamp_granularities[]"),
    timestamp_granularities_scalar: str = Form(default="", alias="timestamp_granularities"),
):
    payload = await file.read()
    if not payload:
        raise HTTPException(status_code=400, detail="the upload carried no audio")
    started = time.time()
    try:
        samples, seconds = decode(payload)
        text, words = await bounded(transcribe, samples, language.strip())
    except HTTPException:
        _stats["failures"] += 1
        raise
    except Exception as failure:
        _stats["failures"] += 1
        log.exception("word timing failed")
        raise HTTPException(status_code=500, detail=str(failure))
    elapsed = time.time() - started
    _stats["requests"] += 1
    _stats["elapsed"] += elapsed
    _stats["max_elapsed"] = max(_stats["max_elapsed"], elapsed)
    _stats["audio_seconds"] += seconds
    _stats["words"] += len(words)

    granularities = list(timestamp_granularities)
    if timestamp_granularities_scalar:
        granularities.append(timestamp_granularities_scalar)
    body = {"text": text}
    # The caller asked for word times or it did not. Returning them anyway
    # would hide a caller that never asked, and the client reports exactly
    # that misconfiguration by name.
    if response_format == "verbose_json" or "word" in granularities:
        body.update({"task": "transcribe", "language": language or LANGUAGE or "",
                     "duration": round(seconds, 3), "words": words})
    return JSONResponse(body)


@app.get("/health")
async def health():
    """Liveness by inference, not by reachability."""
    tone = (0.1 * np.sin(2.0 * np.pi * 220.0 *
                         np.arange(TARGET_RATE // 2) / TARGET_RATE)).astype("float32")
    started = time.time()
    try:
        await bounded(transcribe, tone, "")
    except HTTPException as failure:
        return JSONResponse(status_code=503,
                            content={"status": "unhealthy", "detail": failure.detail})
    return {
        "status": "ok", "model": MODEL_ID, "device": DEVICE,
        "probe_seconds": round(time.time() - started, 3), "stats": _stats,
    }
