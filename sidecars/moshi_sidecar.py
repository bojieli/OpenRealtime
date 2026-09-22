#!/usr/bin/env python3
"""Reference sidecar for Moshi under a native-interaction preset.

Moshi exposes concurrent I/O, a native floor, and native interaction. The
``duplex`` preset selects that bundle, but the capabilities are independent:
they do not make Moshi a mutually exclusive model species, and another
deployment can select external ownership for any capability the sidecar makes
controllable.

What the released checkpoint (``kyutai/moshiko-pytorch-bf16``) does and does
not expose, as measured on the real weights rather than assumed:

* **Its text stream is its own speech.** Moshi's inner monologue is the text of
  what *Moshi* is saying, aligned with its output audio a frame or so ahead of
  the sound. It is forwarded as ``text_delta``/``text_done`` - the assistant's
  speech - and never as a ``transcript``. Moshi does not transcribe the user,
  so this sidecar does not declare ``transcript``; the engine therefore commits
  no user speech, and the background reasoner has nothing to answer.
* **It has no text context channel.** There is no system prompt, no
  instruction input, and no place to put a background answer other than
  forcing words into the monologue, which would make Moshi recite them - the
  design the engine's hand-off contract explicitly rejects. ``text_injection``
  is therefore not declared, and injected text is refused with a visible,
  non-fatal ``text_unsupported`` error rather than stored and silently dropped.
* **It runs on its own clock.** A full-duplex model must keep stepping when the
  client stops sending audio, or it freezes mid-sentence. The frame loop steps
  once per 80 ms Mimi frame: it consumes input as it arrives, and when input is
  more than ``--jitter-frames`` late it feeds silence, so the model hears the
  gap as silence and finishes what it was saying.
* **Its floor is read from its own output.** Moshi generates audio every frame,
  silent or not. Forwarding silence would make the agent look permanently
  "speaking" to every client and benchmark that counts audio, so only audible
  frames (plus a short decay tail) are forwarded; a turn ends after
  ``--hangover-frames`` quiet frames with ``text_done`` and ``turn_done``.
* **It cannot be told to stop.** ``interrupt`` ends the current turn and mutes
  the model's audio until it next falls quiet; the model itself does not know
  it was cut off. Moshi does yield to an interrupting user on its own.
* ``respond`` cannot force Moshi to speak. It waits for the model's next turn
  (bounded by ``--respond-timeout``, extended while the model is speaking), so
  a turn-based caller such as the conformance suite gets Moshi's own answer.

Two ways to run it:

    # one model load per engine session (the engine spawns this)
    openrealtime serve -binding duplex -sidecar "python3 sidecars/moshi_sidecar.py"

    # one model load shared by sequential sessions (one at a time: batch 1)
    python3 sidecars/moshi_sidecar.py --listen tcp:127.0.0.1:9140
    openrealtime serve -binding duplex -sidecar-address tcp:127.0.0.1:9140

Plumbing and conformance without a model:

    openrealtime conformance sidecar -- python3 sidecars/moshi_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import json
import os
import queue
import socket
import sys
import threading
import time
import traceback
from collections import deque
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log, run  # noqa: E402

DEFAULT_REPOSITORY = "kyutai/moshiko-pytorch-bf16"
#: Moshi's Mimi codec runs at 24 kHz with 80 ms frames.
MODEL_RATE = 24_000
FRAME_SAMPLES = 1_920
FRAME_SECONDS = FRAME_SAMPLES / MODEL_RATE
#: Text-stream tokens that carry no word: end-of-padding and padding.
_SILENT_TEXT_TOKENS = (0, 3)


@dataclass
class TurnOptions:
    """How the sidecar reads Moshi's floor from its output and paces input."""

    #: RMS (full scale 1.0) at which an output frame counts as audible. Moshi's
    #: own silence measured 0.000-0.003 and its speech 0.01-0.15 on this
    #: checkpoint.
    speech_threshold: float = 0.01
    #: Quiet frames still forwarded after speech, so words decay naturally.
    tail_frames: int = 2
    #: Quiet frames that end a turn. Moshi pauses up to ~0.6 s inside one
    #: answer; eight frames (640 ms) keeps those inside the turn.
    hangover_frames: int = 8
    #: How late input may be before the loop feeds silence in its place.
    jitter_frames: int = 2
    #: Input backlog beyond which the oldest frames are dropped. The loop runs
    #: at ~0.4x real time on the reference GPU, so this only trips when the
    #: GPU is starved; a model hearing the past is worse than a gap.
    max_backlog_frames: int = 25
    #: How long ``respond`` waits for Moshi to start a turn.
    respond_timeout: float = 15.0


class LinearResampler:
    """Streaming linear-interpolation resampler for input at another rate."""

    def __init__(self, source_rate: int, target_rate: int) -> None:
        self.step = source_rate / target_rate
        self.position = 0.0
        self.tail = np.zeros(0, dtype=np.float32)

    def __call__(self, samples: np.ndarray) -> np.ndarray:
        data = np.concatenate([self.tail, samples.astype(np.float32)])
        if len(data) < 2 or self.position > len(data) - 1:
            self.tail = data
            return np.zeros(0, dtype=np.float32)
        count = int(np.floor((len(data) - 1 - self.position) / self.step)) + 1
        positions = self.position + np.arange(count) * self.step
        out = np.interp(positions, np.arange(len(data)), data).astype(np.float32)
        following = self.position + count * self.step
        keep = min(int(np.floor(following)), len(data) - 1)
        self.tail = data[keep:]
        self.position = following - keep
        return out


class MoshiModel:
    """The loaded checkpoint: Mimi, the LM, and the text tokenizer.

    Batch size is one, so one session uses it at a time; ``lock`` is held by
    the session that owns it. Streaming state is reset per session, which is
    what the upstream Moshi server does per connection, and CUDA graphs captured
    at warmup stay valid across resets.
    """

    def __init__(self, repository: str, device: str, seed: int, revision: str | None = None) -> None:
        import torch  # noqa: PLC0415
        from moshi.models import LMGen, loaders  # noqa: PLC0415

        self.torch = torch
        self.device = device
        began = time.perf_counter()
        torch.manual_seed(seed)
        if torch.cuda.is_available():
            torch.cuda.manual_seed_all(seed)
        log(f"loading {repository}")
        info = loaders.CheckpointInfo.from_hf_repo(repository, revision=revision)
        if info.model_type != "moshi":
            raise RuntimeError(f"{repository} is a {info.model_type!r} checkpoint, not a Moshi dialogue model")
        self.mimi = info.get_mimi(device=device)
        self.lm = info.get_moshi(device=device, dtype=torch.bfloat16)
        if self.lm.condition_provider is not None and self.lm.condition_provider.conditioners:
            raise RuntimeError(f"{repository} expects conditioning this sidecar does not supply")
        self.tokenizer = info.get_text_tokenizer()
        self.generator = LMGen(self.lm, **info.lm_gen_config)
        if self.mimi.num_codebooks != self.lm.dep_q:
            raise RuntimeError(
                f"Mimi decodes {self.mimi.num_codebooks} codebooks but the LM generates {self.lm.dep_q}")
        self.mimi.streaming_forever(1)
        self.generator.streaming_forever(1)
        self.silence = torch.zeros(1, 1, FRAME_SAMPLES, dtype=torch.float32, device=device)
        self.load_seconds = time.perf_counter() - began
        # step() reads this, so it must exist before the warmup steps below.
        self._first = True
        # CUDA graphs are captured on the first steps; doing it here keeps
        # that multi-second stall out of the first live session.
        began = time.perf_counter()
        for _ in range(4):
            self.step(None)
        if torch.cuda.is_available():
            torch.cuda.synchronize()
        self.warmup_seconds = time.perf_counter() - began
        self.lock = threading.Lock()
        self.reset()
        log(f"moshi loaded in {self.load_seconds:.1f}s, warmed up in {self.warmup_seconds:.1f}s")

    def reset(self) -> None:
        self.mimi.reset_streaming()
        self.generator.reset_streaming()
        self._first = True

    def step(self, samples: np.ndarray | None) -> tuple[str | None, np.ndarray | None]:
        """Run one 80 ms frame: encode input, step the LM, decode output."""
        torch = self.torch
        with torch.no_grad():
            if samples is None:
                chunk = self.silence
            else:
                chunk = torch.from_numpy(samples).to(self.device)[None, None]
            codes = self.mimi.encode(chunk)
            if self._first:
                # As upstream: the first encoded frame carries the encoder's
                # left padding, so the encoder is reset to reapply it.
                self.mimi.reset_streaming()
                self._first = False
            tokens = self.generator.step(codes)
            if tokens is None:
                return None, None
            text_token = int(tokens[0, 0, 0].item())
            piece = None
            if text_token not in _SILENT_TEXT_TOKENS:
                piece = self.tokenizer.id_to_piece(text_token).replace("▁", " ")
            pcm = self.mimi.decode(tokens[:, 1:])[0, 0].float().cpu().numpy()
        return piece, pcm


class MoshiSidecar(Sidecar):
    """Moshi behind the sidecar protocol."""

    model_name = DEFAULT_REPOSITORY
    output_rate = MODEL_RATE
    # native_vad is declared because Moshi owns its floor: it decides from the
    # audio when to speak and when to yield, and the engine must not run a
    # second floor over it. It exposes no user-activity signal, so no
    # speech_started/speech_stopped frames are sent. transcript and
    # text_injection are deliberately absent (see the module docstring).
    capabilities = (
        Capability.FULL_DUPLEX,
        Capability.NATIVE_INTERACTION,
        Capability.NATIVE_VAD,
        Capability.BARGE_IN,
    )

    def __init__(self, input_stream, output_stream, *, repository: str, mock: bool,
                 device: str, seed: int = 42424242, shared: MoshiModel | None = None,
                 options: TurnOptions | None = None, stats_file: str = "", revision: str | None = None) -> None:
        super().__init__(input_stream, output_stream)
        self.repository = repository
        self.revision = revision
        self.mock = mock
        self.device = device
        self.seed = seed
        self.options = options or TurnOptions()
        self.stats_file = stats_file
        self._shared = shared
        self._model: MoshiModel | None = None
        self._holds_model = False
        self._frames: queue.Queue[np.ndarray] = queue.Queue()
        self._pending = np.zeros(0, dtype=np.float32)
        self._resampler: LinearResampler | None = None
        self._stream_thread: threading.Thread | None = None
        self._stop = threading.Event()
        self._closing = threading.Event()
        # Turn state, shared between the frame loop and respond/interrupt.
        self._turn_lock = threading.Lock()
        self._speaking = False
        self._muted = False
        self._quiet_run = 0
        self._idle_run = 0
        self._turn_text: list[str] = []
        self._pending_text: list[str] = []
        self._preroll: bytes = b""
        self._space_carry = ""
        self._respond_event: threading.Event | None = None
        # Frame-loop evidence.
        self._frame_ms: deque[float] = deque(maxlen=20_000)
        self._stats = {"frames": 0, "input_frames": 0, "starved_frames": 0, "dropped_frames": 0,
                       "turns": 0, "interrupts": 0, "audio_frames_sent": 0}
        self._session_began = time.monotonic()

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello) -> None:
        self.model_name = self.repository
        if self.instructions.strip():
            log("moshi has no instruction channel; the session instructions are not given to the model")
        if self.input_rate != MODEL_RATE:
            self._resampler = LinearResampler(self.input_rate, MODEL_RATE)
            log(f"resampling input from {self.input_rate} Hz to {MODEL_RATE} Hz")
        if self.mock:
            log("moshi sidecar running in mock mode; no model is loaded")
            self._stream_thread = threading.Thread(target=self._mock_stream, daemon=True)
            self._stream_thread.start()
            return
        model = self._shared or MoshiModel(self.repository, self.device, self.seed, self.revision)
        # One session at a time: a session that is still closing releases the
        # model within its shutdown, so a short wait absorbs the handover.
        if not model.lock.acquire(timeout=20):
            raise RuntimeError("the Moshi model is serving another session (batch size 1)")
        self._model, self._holds_model = model, True
        model.reset()
        self._stream_thread = threading.Thread(target=self._stream, daemon=True)
        self._stream_thread.start()
        log("model ready")

    def _read_loop(self) -> None:
        try:
            super()._read_loop()
        finally:
            # A respond waiting for Moshi's next turn must not hold the worker
            # past bye: the engine kills a sidecar that does not exit.
            self._closing.set()

    def on_close(self) -> None:
        self._closing.set()
        self._stop.set()
        if self._stream_thread is not None:
            self._stream_thread.join(timeout=5)
        if self._model is not None:
            self._report_stats()
        if self._holds_model and self._model is not None:
            self._holds_model = False
            self._model.lock.release()
        self._model = None

    # --- session ------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        """Queue input for the frame loop, in 80 ms Mimi frames."""
        samples = np.frombuffer(pcm16[: len(pcm16) // 2 * 2], dtype="<i2").astype(np.float32) / 32768.0
        if self._resampler is not None:
            samples = self._resampler(samples)
        self._pending = np.concatenate([self._pending, samples])
        while len(self._pending) >= FRAME_SAMPLES:
            frame = self._pending[:FRAME_SAMPLES].copy()
            self._pending = self._pending[FRAME_SAMPLES:]
            self._frames.put(frame)
            self._stats["input_frames"] += 1
            while self._frames.qsize() > self.options.max_backlog_frames:
                # Dropping the oldest frame is the right failure for realtime
                # audio: a backlog the model works through late is worse than
                # a gap it never hears.
                try:
                    self._frames.get_nowait()
                    self._stats["dropped_frames"] += 1
                except queue.Empty:
                    break

    def on_text(self, text: str, role: str) -> None:
        """Refuse injected text visibly.

        Moshi conditions on nothing but audio and its own monologue. Keeping
        the text as "context" that nothing reads would be a policy wired to
        nothing; saying so lets the operator see the answer was not delivered.
        """
        log(f"moshi cannot read injected {role} text ({len(text)} chars); it was not given to the model")
        self.error("Moshi has no text input: injected text was not given to the model",
                   code="text_unsupported")

    def on_respond(self) -> None:
        """Wait for Moshi's own next turn.

        Nothing can make Moshi speak on request. A respond waits for the turn
        the model takes next (or the rest of the one it is in), so the base
        class's turn_done closes that turn; if the model stays silent past the
        timeout, the turn ends empty, which is what the model chose.
        """
        if self.mock:
            self._respond_mock()
            return
        event = threading.Event()
        with self._turn_lock:
            self._respond_event = event
        deadline = time.monotonic() + self.options.respond_timeout
        while not event.wait(0.05):
            if self._closing.is_set() or self._stop.is_set():
                break
            with self._turn_lock:
                if self._speaking:
                    deadline = max(deadline, time.monotonic() + 1.0)
            if time.monotonic() >= deadline:
                break
        with self._turn_lock:
            if self._respond_event is event:
                # Not consumed by a turn end: give up the wait. Any turn the
                # model starts later ends with its own turn_done.
                self._respond_event = None
                if not event.is_set():
                    log(f"moshi took no turn within {self.options.respond_timeout:.0f}s of respond")

    def _respond_mock(self) -> None:
        answer = "This is the Moshi sidecar running without a model."
        self.text_delta(answer)
        self.text_done(answer)
        for _ in range(4):
            if self.interrupted():
                return
            self.audio(np.zeros(MODEL_RATE // 10, dtype=np.int16).tobytes())

    # --- the frame loop -----------------------------------------------------

    def _stream(self) -> None:
        """Run the model on an 80 ms clock.

        This loop is the difference between duplex and turn-based: it never
        waits to be asked. The clock starts with the first input frame; input
        drives it while it arrives, and silence stands in once input is later
        than the jitter allowance, so the model keeps listening, and keeps
        talking, when the client goes quiet.
        """
        options = self.options
        began: float | None = None
        steps = 0
        try:
            while not self._stop.is_set():
                if began is None:
                    try:
                        samples = self._frames.get(timeout=0.2)
                    except queue.Empty:
                        continue
                    began = time.monotonic()
                else:
                    # Frame i is due at began + i * 80 ms; silence stands in
                    # only when input is later than the jitter allowance.
                    due = int((time.monotonic() - began) / FRAME_SECONDS) + 1 - options.jitter_frames
                    try:
                        samples = self._frames.get_nowait()
                    except queue.Empty:
                        if steps < due:
                            samples = None
                            self._stats["starved_frames"] += 1
                        else:
                            wait = began + (steps + options.jitter_frames) * FRAME_SECONDS - time.monotonic()
                            try:
                                samples = self._frames.get(timeout=max(0.001, wait))
                            except queue.Empty:
                                continue
                if self._interrupted.is_set():
                    self._interrupted.clear()
                    self._on_interrupt()
                started = time.perf_counter()
                piece, pcm = self._model.step(samples)
                self._frame_ms.append((time.perf_counter() - started) * 1000.0)
                steps += 1
                self._stats["frames"] += 1
                if pcm is not None:
                    self._emit(piece, pcm)
                if steps % 750 == 0:
                    self._report_stats(final=False)
        except Exception as failure:  # noqa: BLE001 - reported, not swallowed
            log(traceback.format_exc())
            self.error(f"moshi frame loop: {failure}", code="model_failed", fatal=True)

    def _emit(self, piece: str | None, pcm: np.ndarray) -> None:
        options = self.options
        if piece is not None and not piece.strip():
            # A bare word boundary: carry it onto the next piece, because the
            # engine drops whitespace-only deltas and the terminal text must
            # equal what the deltas carried.
            self._space_carry += piece
            piece = None
        elif piece is not None:
            piece, self._space_carry = self._space_carry + piece, ""
        rms = float(np.sqrt(np.mean(np.square(pcm)))) if pcm.size else 0.0
        loud = rms >= options.speech_threshold
        pcm16 = (np.clip(pcm, -1.0, 1.0) * 32767.0).astype("<i2").tobytes()
        with self._turn_lock:
            if self._muted:
                # Interrupted: stay silent until the model itself falls quiet.
                self._quiet_run = 0 if loud else self._quiet_run + 1
                if self._quiet_run >= options.hangover_frames:
                    self._muted, self._quiet_run = False, 0
                return
            if not self._speaking:
                if piece:
                    # The monologue leads the sound by a frame or so.
                    self._pending_text.append(piece)
                if not loud:
                    self._preroll = pcm16
                    self._idle_run += 1
                    if self._idle_run > options.hangover_frames:
                        # Words the model "thought" but never voiced.
                        self._pending_text.clear()
                    return
                self._speaking, self._quiet_run, self._idle_run = True, 0, 0
                self._stats["turns"] += 1
                words, self._pending_text = self._pending_text, []
                preroll, self._preroll = self._preroll, b""
            else:
                words = [piece] if piece else []
                preroll = b""
                if loud:
                    self._quiet_run = 0
                else:
                    self._quiet_run += 1
            forward = loud or self._quiet_run <= options.tail_frames
            ended = self._quiet_run >= options.hangover_frames
        for word in words:
            self._turn_text.append(word)
            self.text_delta(word)
        if forward:
            if preroll:
                self.audio(preroll)
                self._stats["audio_frames_sent"] += 1
            self.audio(pcm16)
            self._stats["audio_frames_sent"] += 1
        if ended:
            self._end_turn()

    def _end_turn(self, *, mute: bool = False) -> None:
        with self._turn_lock:
            if not self._speaking:
                return
            self._speaking, self._quiet_run = False, 0
            self._muted = mute
            waiter, self._respond_event = self._respond_event, None
            text, self._turn_text = "".join(self._turn_text), []
        if text.strip():
            # Exactly the concatenated deltas: the engine forwards whatever a
            # terminal text adds beyond them, so a trimmed copy would repeat.
            self.text_done(text)
        if waiter is not None:
            # The respond that is waiting returns, and the base class sends
            # this turn's turn_done.
            waiter.set()
        else:
            self.turn_done()

    def _on_interrupt(self) -> None:
        self._stats["interrupts"] += 1
        with self._turn_lock:
            speaking = self._speaking
            waiter = None
            if not speaking:
                waiter, self._respond_event = self._respond_event, None
        if speaking:
            self._end_turn(mute=True)
        elif waiter is not None:
            waiter.set()

    def _report_stats(self, *, final: bool = True) -> None:
        frames = np.array(self._frame_ms, dtype=np.float64)
        summary = dict(self._stats)
        summary["session_seconds"] = round(time.monotonic() - self._session_began, 1)
        if frames.size:
            summary.update({
                "frame_ms_p50": round(float(np.percentile(frames, 50)), 2),
                "frame_ms_p95": round(float(np.percentile(frames, 95)), 2),
                "frame_ms_max": round(float(frames.max()), 2),
                "rtf_p50": round(float(np.percentile(frames, 50)) / (FRAME_SECONDS * 1000), 3),
                "rtf_p95": round(float(np.percentile(frames, 95)) / (FRAME_SECONDS * 1000), 3),
                "frames_over_budget": int((frames > FRAME_SECONDS * 1000).sum()),
            })
        log(("moshi session stats " if final else "moshi frame loop ") + json.dumps(summary))
        if final and self.stats_file:
            try:
                with open(self.stats_file, "a", encoding="utf-8") as handle:
                    handle.write(json.dumps({"model": self.repository, "pid": os.getpid(), **summary}) + "\n")
            except OSError as failure:
                log(f"could not write stats: {failure}")

    def _mock_stream(self) -> None:
        while not self._stop.is_set():
            try:
                self._frames.get(timeout=0.2)
            except queue.Empty:
                continue


def _listen(address: str, factory) -> None:
    """Serve sequential sessions over TCP or a Unix socket with one model load."""
    network, _, target = address.partition(":")
    if network == "tcp":
        host, _, port = target.rpartition(":")
        server = socket.create_server((host or "127.0.0.1", int(port)))
    elif network == "unix":
        if os.path.exists(target):
            os.unlink(target)
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(target)
        server.listen()
    else:
        raise SystemExit(f"--listen must be tcp:host:port or unix:/path, got {address!r}")
    log(f"moshi sidecar listening on {address}")

    def serve(connection: socket.socket) -> None:
        reader, writer = connection.makefile("rb"), connection.makefile("wb")
        try:
            factory(reader, writer).run()
        except Exception:  # noqa: BLE001
            log(traceback.format_exc())
        finally:
            for handle in (reader, writer, connection):
                try:
                    handle.close()
                except OSError:
                    pass

    while True:
        connection, _ = server.accept()
        threading.Thread(target=serve, args=(connection,), daemon=True).start()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repository", default=DEFAULT_REPOSITORY, help="Moshi weights repository")
    parser.add_argument("--revision", default=None, help="pinned Hub model/tokenizer/codec revision")
    parser.add_argument("--device", default="cuda", help="device to place the model on")
    parser.add_argument("--seed", type=int, default=42424242, help="sampling seed (upstream server default)")
    parser.add_argument("--listen", default="",
                        help="serve sequential sessions on tcp:host:port or unix:/path with one model load")
    parser.add_argument("--speech-threshold", type=float, default=TurnOptions.speech_threshold,
                        help="output RMS at which a Moshi frame counts as audible")
    parser.add_argument("--hangover-frames", type=int, default=TurnOptions.hangover_frames,
                        help="quiet 80 ms frames that end a Moshi turn")
    parser.add_argument("--tail-frames", type=int, default=TurnOptions.tail_frames,
                        help="quiet frames still forwarded after speech")
    parser.add_argument("--jitter-frames", type=int, default=TurnOptions.jitter_frames,
                        help="how many frames late input may be before silence stands in")
    parser.add_argument("--respond-timeout", type=float, default=TurnOptions.respond_timeout,
                        help="seconds a respond request waits for Moshi to take a turn")
    parser.add_argument("--stats-file", default=os.environ.get("MOSHI_SIDECAR_STATS", ""),
                        help="append one JSON line of frame-loop evidence per session")
    parser.add_argument(
        "--mock", action="store_true",
        help="speak the protocol without loading a model, for plumbing and conformance",
    )
    arguments = parser.parse_args()
    options = TurnOptions(
        speech_threshold=arguments.speech_threshold, hangover_frames=arguments.hangover_frames,
        tail_frames=arguments.tail_frames, jitter_frames=arguments.jitter_frames,
        respond_timeout=arguments.respond_timeout,
    )
    common = dict(repository=arguments.repository, mock=arguments.mock, device=arguments.device,
                  seed=arguments.seed, options=options, stats_file=arguments.stats_file, revision=arguments.revision)
    if arguments.listen:
        shared = None if arguments.mock else MoshiModel(arguments.repository, arguments.device, arguments.seed, arguments.revision)
        _listen(arguments.listen, lambda reader, writer: MoshiSidecar(reader, writer, shared=shared, **common))
        return
    run(MoshiSidecar, **common)


if __name__ == "__main__":
    main()
