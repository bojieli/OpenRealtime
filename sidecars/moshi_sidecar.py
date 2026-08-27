#!/usr/bin/env python3
"""Reference sidecar for Moshi under a native-interaction preset.

Moshi exposes concurrent I/O, a native floor, and native interaction. The
``duplex`` preset selects that bundle, but the capabilities are independent:
they do not make Moshi a mutually exclusive model species, and another
deployment can select external ownership for any capability the sidecar makes
controllable.

What Moshi does not have, and cannot have, is a background reasoner: a single
model has no second model and no shared log to put one on. That is what the
engine supplies, and where its answer splices in is the one genuinely open
question about this binding.

Two paths, in the order the plan puts them:

1. **Inner-monologue text conditioning.** Moshi generates a text stream
   alongside its audio, and the engine's answer is fed into that stream as
   context. The model then decides for itself when to say what it now knows,
   which is the only path that respects a floor it owns.
2. **Explicit hand-off.** The answer is injected and a turn is requested. This
   certainly works, it is the documented fallback, and it is why the binding
   ships whatever the first path turns out to be worth.

Run it through the engine's ``duplex`` binding, or on its own for conformance:

    openrealtime conformance sidecar -- python3 sidecars/moshi_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import queue
import sys
import threading
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log, run  # noqa: E402

DEFAULT_REPOSITORY = "kyutai/moshiko-pytorch-bf16"
#: Moshi's Mimi codec runs at 24 kHz with 80 ms frames.
MODEL_RATE = 24_000
FRAME_SAMPLES = 1_920


class MoshiSidecar(Sidecar):
    """Moshi behind the sidecar protocol."""

    model_name = DEFAULT_REPOSITORY
    output_rate = MODEL_RATE
    capabilities = (
        Capability.FULL_DUPLEX,
        Capability.NATIVE_INTERACTION,
        Capability.NATIVE_VAD,
        Capability.BARGE_IN,
        Capability.TRANSCRIPT,
        Capability.TEXT_INJECTION,
    )

    def __init__(self, input_stream, output_stream, *, repository: str, mock: bool,
                 device: str, injection: str) -> None:
        super().__init__(input_stream, output_stream)
        self.repository = repository
        self.mock = mock
        self.device = device
        self.injection = injection
        self.model = None
        self.mimi = None
        self.state = None
        self.text_tokenizer = None
        self._frames: queue.Queue[np.ndarray] = queue.Queue(maxsize=64)
        self._buffer = bytearray()
        self._speaking = False
        self._stream_thread: threading.Thread | None = None
        self._stop = threading.Event()
        self._pending_text: list[str] = []
        self._text_lock = threading.Lock()

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello) -> None:
        self.model_name = self.repository
        if self.mock:
            log("moshi sidecar running in mock mode; no model is loaded")
            self._stream_thread = threading.Thread(target=self._mock_stream, daemon=True)
            self._stream_thread.start()
            return
        import torch  # noqa: PLC0415
        from huggingface_hub import hf_hub_download  # noqa: PLC0415
        from moshi.models import loaders, LMGen  # noqa: PLC0415

        log(f"loading {self.repository}")
        mimi_weight = hf_hub_download(self.repository, loaders.MIMI_NAME)
        self.mimi = loaders.get_mimi(mimi_weight, device=self.device)
        self.mimi.set_num_codebooks(8)
        moshi_weight = hf_hub_download(self.repository, loaders.MOSHI_NAME)
        self.model = loaders.get_moshi_lm(moshi_weight, device=self.device)
        tokenizer_path = hf_hub_download(self.repository, loaders.TEXT_TOKENIZER_NAME)
        import sentencepiece  # noqa: PLC0415

        self.text_tokenizer = sentencepiece.SentencePieceProcessor(tokenizer_path)
        self.state = LMGen(self.model, temp=0.8, temp_text=0.7)
        self._torch = torch
        self._stream_thread = threading.Thread(target=self._stream, daemon=True)
        self._stream_thread.start()
        log("model ready")

    def on_close(self) -> None:
        self._stop.set()
        if self._stream_thread is not None:
            self._stream_thread.join(timeout=5)
        self.model = None
        self.mimi = None
        self.state = None

    # --- session ------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        """Feed audio continuously.

        A duplex model is always listening, so there is no turn to accumulate
        into: frames go to the model as they arrive, and it decides what to do
        about them.
        """
        self._buffer.extend(pcm16)
        frame_bytes = FRAME_SAMPLES * 2
        while len(self._buffer) >= frame_bytes:
            chunk = bytes(self._buffer[:frame_bytes])
            del self._buffer[:frame_bytes]
            samples = np.frombuffer(chunk, dtype=np.int16).astype(np.float32) / 32768.0
            try:
                self._frames.put_nowait(samples)
            except queue.Full:
                # Dropping the oldest frame is the right failure for realtime
                # audio: a backlog the model works through late is worse than a
                # gap it never hears.
                try:
                    self._frames.get_nowait()
                    self._frames.put_nowait(samples)
                except queue.Empty:
                    pass

    def on_text(self, text: str, role: str) -> None:
        """Take the background reasoner's answer.

        Where it goes depends on the configured injection path, and that choice
        is the research question this binding exists to answer.
        """
        with self._text_lock:
            self._pending_text.append(text)

    def on_respond(self) -> None:
        """Answer an explicit turn request.

        The native-interaction preset gives this model its floor, so the engine
        does not normally ask. When a different ownership composition does -
        the hand-off fallback, or a client driving turns explicitly - pending
        text is spoken rather than left as context.
        """
        with self._text_lock:
            pending = "\n".join(self._pending_text)
            self._pending_text.clear()
        if self.mock:
            self._respond_mock(pending)
            return
        if not pending:
            # Nothing to hand over, and a model that owns its floor does not
            # need permission to speak. Asking again would be the engine
            # taking back the floor it delegated.
            return
        self._speak(pending)

    def _respond_mock(self, pending: str) -> None:
        self.transcript("(mock transcript)", final=True)
        answer = pending or "This is the Moshi sidecar running without a model."
        self.text_delta(answer)
        self.text_done(answer)
        for _ in range(4):
            if self.interrupted():
                return
            self.audio(np.zeros(MODEL_RATE // 10, dtype=np.int16).tobytes())

    # --- streaming ----------------------------------------------------------

    def _stream(self) -> None:
        """Run the model continuously.

        This loop is the difference between duplex and turn-based: it never
        waits to be asked. It consumes every frame, emits whatever the model
        produces, and lets the model decide when that is silence.
        """
        torch = self._torch
        with torch.no_grad(), self.mimi.streaming(1), self.state.streaming(1):
            while not self._stop.is_set():
                try:
                    samples = self._frames.get(timeout=0.5)
                except queue.Empty:
                    continue
                frame = torch.from_numpy(samples).to(self.device)[None, None]
                codes = self.mimi.encode(frame)
                tokens = self.state.step(codes)
                if tokens is None:
                    continue
                self._emit(tokens)

    def _emit(self, tokens) -> None:
        torch = self._torch
        text_token = tokens[0, 0, 0].item()
        if text_token not in (0, 3):
            piece = self.text_tokenizer.id_to_piece(text_token)
            text = piece.replace("▁", " ")
            if text.strip():
                self.text_delta(text)
        audio_tokens = tokens[:, 1:]
        with torch.no_grad():
            waveform = self.mimi.decode(audio_tokens)
        samples = waveform[0, 0].detach().float().cpu().numpy()
        energy = float(np.sqrt(np.mean(np.square(samples)))) if samples.size else 0.0
        # The model's own output energy is its floor signal: it is speaking
        # when it is producing sound, which is the only honest reading
        # available without asking it a question it has no way to answer.
        speaking = energy > 1e-3
        if speaking and not self._speaking:
            self._speaking = True
        elif not speaking and self._speaking:
            self._speaking = False
            self.turn_done()
        if samples.size:
            self.audio((np.clip(samples, -1, 1) * 32767).astype(np.int16).tobytes())

    def _speak(self, text: str) -> None:
        """Condition the model on text it should say.

        Inner-monologue conditioning writes into the text stream the model is
        already generating, so it stays in control of when the words come out.
        Hand-off forces a turn instead. Which one is configured is the F-factor
        this sidecar exposes.
        """
        if self.injection == "handoff":
            self.text_delta(text)
            self.text_done(text)
            return
        with self._text_lock:
            self._pending_text.append(text)

    def _mock_stream(self) -> None:
        while not self._stop.is_set():
            try:
                self._frames.get(timeout=0.2)
            except queue.Empty:
                continue


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", default=DEFAULT_REPOSITORY, help="Moshi weights repository")
    parser.add_argument("--device", default="cuda", help="device to place the model on")
    parser.add_argument(
        "--injection", default="inner-monologue", choices=["inner-monologue", "handoff"],
        help="where a background answer splices in: the model's text stream, or an explicit turn",
    )
    parser.add_argument(
        "--mock", action="store_true",
        help="speak the protocol without loading a model, for plumbing and conformance",
    )
    arguments = parser.parse_args()
    run(
        MoshiSidecar, repository=arguments.repository, mock=arguments.mock,
        device=arguments.device, injection=arguments.injection,
    )


if __name__ == "__main__":
    main()
