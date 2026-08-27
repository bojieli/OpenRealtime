#!/usr/bin/env python3
"""Reference sidecar for Qwen3-Omni.

Qwen3-Omni takes audio in and produces text and audio out. It has no
interaction capability of its own: it is a turn-based generator, and it does
not decide when a turn ended. The engine does that, and this sidecar's job is
to be a clean process boundary around the model rather than to have opinions
about timing.

Run it through the engine's ``omni`` binding, or on its own for the sidecar
conformance suite:

    openrealtime conformance sidecar -- python3 sidecars/qwen3_omni_sidecar.py --mock
"""

from __future__ import annotations

import argparse
import io
import sys
import wave
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log, run  # noqa: E402

DEFAULT_MODEL = "Qwen/Qwen3-Omni-30B-A3B-Instruct"
#: The model's audio encoder expects 16 kHz; its speech output is 24 kHz.
MODEL_INPUT_RATE = 16_000
MODEL_OUTPUT_RATE = 24_000


class Qwen3OmniSidecar(Sidecar):
    """Qwen3-Omni behind the sidecar protocol."""

    model_name = DEFAULT_MODEL
    output_rate = MODEL_OUTPUT_RATE
    #: No native voice activity detection and no full duplex: the engine keeps
    #: the floor, which is the point of running this model here.
    capabilities = (
        Capability.TRANSCRIPT,
        Capability.TEXT_INJECTION,
        Capability.INTERACTION_ACTS,
    )

    def __init__(self, input_stream, output_stream, *, model_path: str, mock: bool,
                 device: str, max_new_tokens: int) -> None:
        super().__init__(input_stream, output_stream)
        self.model_path = model_path
        self.mock = mock
        self.device = device
        self.max_new_tokens = max_new_tokens
        self.model = None
        self.processor = None
        self._audio = bytearray()
        self._history: list[dict] = []

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello) -> None:
        self.model_name = self.model_path
        if self.instructions:
            self._history.append({"role": "system", "content": [
                {"type": "text", "text": self.instructions},
            ]})
        if self.mock:
            log("qwen3-omni sidecar running in mock mode; no model is loaded")
            return
        from transformers import (  # noqa: PLC0415 - imported only when a model is wanted
            AutoProcessor,
            Qwen3OmniMoeForConditionalGeneration,
        )

        log(f"loading {self.model_path}")
        self.processor = AutoProcessor.from_pretrained(self.model_path)
        self.model = Qwen3OmniMoeForConditionalGeneration.from_pretrained(
            self.model_path, dtype="auto", device_map=self.device,
        )
        self.model.eval()
        log("model ready")

    def on_close(self) -> None:
        self.model = None
        self.processor = None

    # --- session ------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        # Audio accumulates until the engine says the turn ended. A turn-based
        # model has nothing useful to do with a partial utterance, and
        # pretending otherwise would burn a GPU on every frame.
        self._audio.extend(pcm16)

    def on_text(self, text: str, role: str) -> None:
        # This is the background reasoner's completed answer arriving. It goes
        # into history as context; the engine decides when the model speaks.
        self._history.append({"role": role or "system", "content": [
            {"type": "text", "text": text},
        ]})

    def on_respond(self) -> None:
        audio = self._take_audio()
        if self.mock:
            self._respond_mock(audio)
            return
        self._respond_model(audio)

    # --- generation ---------------------------------------------------------

    def _take_audio(self) -> np.ndarray:
        raw = bytes(self._audio)
        self._audio.clear()
        if not raw:
            return np.zeros(0, dtype=np.float32)
        samples = np.frombuffer(raw, dtype=np.int16).astype(np.float32) / 32768.0
        return resample(samples, self.input_rate, MODEL_INPUT_RATE)

    def _respond_model(self, audio: np.ndarray) -> None:
        content: list[dict] = []
        if audio.size:
            content.append({"type": "audio", "audio": audio})
        if not content:
            content.append({"type": "text", "text": "(the user said nothing)"})
        self._history.append({"role": "user", "content": content})

        text_prompt = self.processor.apply_chat_template(
            self._history, add_generation_prompt=True, tokenize=False,
        )
        inputs = self.processor(
            text=text_prompt, audio=[audio] if audio.size else None,
            sampling_rate=MODEL_INPUT_RATE, return_tensors="pt", padding=True,
        ).to(self.model.device)

        outputs = self.model.generate(
            **inputs, max_new_tokens=self.max_new_tokens,
            return_audio=True, thinker_return_dict_in_generate=True,
        )
        text, waveform = split_outputs(outputs, self.processor, inputs)
        if text:
            self.text_delta(text)
            self.text_done(text)
            self._history.append({"role": "assistant", "content": [
                {"type": "text", "text": text},
            ]})
        if waveform is not None and waveform.size:
            self._emit_audio(waveform)

    def _respond_mock(self, audio: np.ndarray) -> None:
        """Answer without a model.

        Mock mode exists so the conformance suite, the binding tests, and a new
        deployment's plumbing can all be verified without a GPU. It speaks the
        protocol exactly; it just has nothing to say.
        """
        seconds = audio.size / MODEL_INPUT_RATE if audio.size else 0.0
        self.transcript(f"(mock transcript of {seconds:.1f}s of audio)", final=True)
        answer = "This is the Qwen3-Omni sidecar running without a model."
        self.text_delta(answer)
        self.text_done(answer)
        self._emit_audio(silence(MODEL_OUTPUT_RATE, 0.4))

    def _emit_audio(self, waveform: np.ndarray) -> None:
        pcm = to_pcm16(waveform)
        chunk = MODEL_OUTPUT_RATE // 10 * 2  # 100 ms
        for offset in range(0, len(pcm), chunk):
            if self.interrupted():
                return
            self.audio(pcm[offset:offset + chunk])


def resample(samples: np.ndarray, source_rate: int, target_rate: int) -> np.ndarray:
    """Linear resampling.

    Good enough for an encoder front end and cheap enough not to matter. A
    sidecar that needed better would say so; this one does not.
    """
    if source_rate == target_rate or samples.size == 0:
        return samples
    duration = samples.size / source_rate
    target_count = int(duration * target_rate)
    if target_count <= 0:
        return np.zeros(0, dtype=np.float32)
    source_positions = np.linspace(0, samples.size - 1, num=samples.size)
    target_positions = np.linspace(0, samples.size - 1, num=target_count)
    return np.interp(target_positions, source_positions, samples).astype(np.float32)


def to_pcm16(waveform: np.ndarray) -> bytes:
    clipped = np.clip(waveform, -1.0, 1.0)
    return (clipped * 32767.0).astype(np.int16).tobytes()


def silence(rate: int, seconds: float) -> np.ndarray:
    return np.zeros(int(rate * seconds), dtype=np.float32)


def split_outputs(outputs, processor, inputs):
    """Separate generated text from generated speech.

    The model returns either a tuple of (text ids, waveform) or a single
    tensor of ids, depending on whether audio output was produced. Handling
    both here keeps the difference out of the response path.
    """
    waveform = None
    token_ids = outputs
    if isinstance(outputs, tuple):
        token_ids, waveform = outputs[0], outputs[1]
    if hasattr(token_ids, "sequences"):
        token_ids = token_ids.sequences
    prompt_length = inputs["input_ids"].shape[1]
    generated = token_ids[:, prompt_length:]
    text = processor.batch_decode(
        generated, skip_special_tokens=True, clean_up_tokenization_spaces=False,
    )[0].strip()
    if waveform is not None:
        waveform = waveform.reshape(-1).detach().float().cpu().numpy()
    return text, waveform


def wav_bytes(pcm16: bytes, rate: int) -> bytes:
    """Wrap PCM16 in a WAV container, for debugging by ear."""
    buffer = io.BytesIO()
    with wave.open(buffer, "wb") as handle:
        handle.setnchannels(1)
        handle.setsampwidth(2)
        handle.setframerate(rate)
        handle.writeframes(pcm16)
    return buffer.getvalue()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", default=DEFAULT_MODEL, help="model path or hub identifier")
    parser.add_argument("--device", default="cuda", help="device map for the model")
    parser.add_argument("--max-new-tokens", type=int, default=256)
    parser.add_argument(
        "--mock", action="store_true",
        help="speak the protocol without loading a model, for plumbing and conformance",
    )
    arguments = parser.parse_args()
    run(
        Qwen3OmniSidecar, model_path=arguments.model, mock=arguments.mock,
        device=arguments.device, max_new_tokens=arguments.max_new_tokens,
    )


if __name__ == "__main__":
    main()
