#!/usr/bin/env python3
"""Reference sidecar for MiniCPM-o 4.5.

MiniCPM-o has a genuinely streaming interface: audio is prefilled a chunk at a
time, and each generation step returns either "still listening" or a piece of
speech. That is closer to duplex than a turn-based generator, but the model
still does not decide when a conversation's turn has ended in the sense a
tool-using agent needs, so the engine keeps the floor and this sidecar reports
what the model decided rather than acting on it.

Run it through the engine's ``omni`` binding, or on its own for conformance:

    openrealtime conformance sidecar -- python3 sidecars/minicpm_o_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import sys
import threading
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log, run  # noqa: E402

DEFAULT_MODEL = "openbmb/MiniCPM-o-4_5"
#: The model prefills audio at 16 kHz and emits speech at 24 kHz.
MODEL_INPUT_RATE = 16_000
MODEL_OUTPUT_RATE = 24_000
#: The streaming interface expects roughly one second of audio per prefill.
PREFILL_SECONDS = 1.0


class MiniCPMOSidecar(Sidecar):
    """MiniCPM-o 4.5 behind the sidecar protocol."""

    model_name = DEFAULT_MODEL
    output_rate = MODEL_OUTPUT_RATE
    capabilities = (
        Capability.TRANSCRIPT,
        Capability.TEXT_INJECTION,
        Capability.INTERACTION_ACTS,
        # The model reports listen-versus-speak per chunk, which is a native
        # activity signal even though the engine does not act on it here.
        Capability.NATIVE_VAD,
    )

    def __init__(self, input_stream, output_stream, *, model_path: str, mock: bool,
                 device: str, max_chunks: int) -> None:
        super().__init__(input_stream, output_stream)
        self.model_path = model_path
        self.mock = mock
        self.device = device
        self.max_chunks = max_chunks
        self.model = None
        self.tokenizer = None
        self._pending = bytearray()
        self._prefill_lock = threading.Lock()
        self._prefilled_seconds = 0.0

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello) -> None:
        self.model_name = self.model_path
        if self.mock:
            log("minicpm-o sidecar running in mock mode; no model is loaded")
            return
        import torch  # noqa: PLC0415
        from transformers import AutoModel, AutoTokenizer  # noqa: PLC0415

        log(f"loading {self.model_path}")
        self.tokenizer = AutoTokenizer.from_pretrained(self.model_path, trust_remote_code=True)
        self.model = AutoModel.from_pretrained(
            self.model_path, trust_remote_code=True,
            attn_implementation="sdpa", torch_dtype=torch.bfloat16,
            init_vision=False, init_audio=True, init_tts=True,
        ).eval().to(self.device)
        self.model.init_tts()
        self.model.reset_session()
        if self.instructions:
            self._prefill_text(self.instructions)
        log("model ready")

    def on_close(self) -> None:
        self.model = None
        self.tokenizer = None

    # --- session ------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        """Prefill audio as it arrives, a chunk at a time.

        Prefilling while the user is still speaking is the whole reason this
        model's interface is shaped this way: by the time the engine asks for a
        turn, most of the work is done.
        """
        self._pending.extend(pcm16)
        chunk_bytes = int(self.input_rate * PREFILL_SECONDS) * 2
        while len(self._pending) >= chunk_bytes:
            chunk = bytes(self._pending[:chunk_bytes])
            del self._pending[:chunk_bytes]
            self._prefill_audio(chunk)

    def on_text(self, text: str, role: str) -> None:
        # The background reasoner's answer becomes context. The engine then
        # asks for a turn, and the model says it.
        if self.mock or self.model is None:
            return
        self._prefill_text(text)

    def on_respond(self) -> None:
        # Anything not yet prefilled is prefilled now: the turn is over, so
        # there is no later chunk to wait for.
        if self._pending:
            chunk = bytes(self._pending)
            self._pending.clear()
            self._prefill_audio(chunk)
        if self.mock:
            self._respond_mock()
            return
        self._respond_model()

    # --- model --------------------------------------------------------------

    def _prefill_audio(self, pcm16: bytes) -> None:
        if self.mock or self.model is None or not pcm16:
            self._prefilled_seconds += len(pcm16) / 2 / max(self.input_rate, 1)
            return
        samples = np.frombuffer(pcm16, dtype=np.int16).astype(np.float32) / 32768.0
        waveform = resample(samples, self.input_rate, MODEL_INPUT_RATE)
        with self._prefill_lock:
            self.model.streaming_prefill(audio_waveform=waveform)
        self._prefilled_seconds += samples.size / max(self.input_rate, 1)

    def _prefill_text(self, text: str) -> None:
        if self.mock or self.model is None or not text.strip():
            return
        with self._prefill_lock:
            self.model.streaming_prefill(text_list=[text])

    def _respond_model(self) -> None:
        spoken: list[str] = []
        for _ in range(self.max_chunks):
            if self.interrupted():
                return
            with self._prefill_lock:
                result = self.model.streaming_generate()
            if result.get("is_listen"):
                # The model chose to keep listening. The engine asked for a
                # turn, so this is reported and the turn ends rather than
                # spinning: whose decision wins is the floor policy's business,
                # and the engine holds the floor.
                self.send("log", text="model chose to keep listening")
                break
            text = str(result.get("text") or "")
            if text:
                spoken.append(text)
                self.text_delta(text)
            waveform = result.get("audio_waveform")
            if waveform is not None and len(waveform):
                self.audio(to_pcm16(np.asarray(waveform, dtype=np.float32)))
            if result.get("end_of_turn"):
                break
        if spoken:
            self.text_done("".join(spoken))

    def _respond_mock(self) -> None:
        self.transcript(
            f"(mock transcript of {self._prefilled_seconds:.1f}s of audio)", final=True,
        )
        self._prefilled_seconds = 0.0
        answer = "This is the MiniCPM-o sidecar running without a model."
        self.text_delta(answer)
        self.text_done(answer)
        for _ in range(4):
            if self.interrupted():
                return
            self.audio(to_pcm16(np.zeros(MODEL_OUTPUT_RATE // 10, dtype=np.float32)))


def resample(samples: np.ndarray, source_rate: int, target_rate: int) -> np.ndarray:
    if source_rate == target_rate or samples.size == 0:
        return samples
    target_count = int(samples.size / source_rate * target_rate)
    if target_count <= 0:
        return np.zeros(0, dtype=np.float32)
    source_positions = np.linspace(0, samples.size - 1, num=samples.size)
    target_positions = np.linspace(0, samples.size - 1, num=target_count)
    return np.interp(target_positions, source_positions, samples).astype(np.float32)


def to_pcm16(waveform: np.ndarray) -> bytes:
    return (np.clip(waveform, -1.0, 1.0) * 32767.0).astype(np.int16).tobytes()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", default=DEFAULT_MODEL, help="model path or hub identifier")
    parser.add_argument("--device", default="cuda", help="device to place the model on")
    parser.add_argument(
        "--max-chunks", type=int, default=64,
        help="how many generation chunks one turn may take before it is closed",
    )
    parser.add_argument(
        "--mock", action="store_true",
        help="speak the protocol without loading a model, for plumbing and conformance",
    )
    arguments = parser.parse_args()
    run(
        MiniCPMOSidecar, model_path=arguments.model, mock=arguments.mock,
        device=arguments.device, max_chunks=arguments.max_chunks,
    )


if __name__ == "__main__":
    main()
