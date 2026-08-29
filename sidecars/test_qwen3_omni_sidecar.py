"""CPU-only contract tests for the Qwen3-Omni adapter glue."""

from __future__ import annotations

import io

import numpy as np

import qwen3_omni_sidecar as qwen


class _Batch(dict):
    def __init__(self) -> None:
        super().__init__({"input_ids": object()})
        self.moves: list[str] = []

    def to(self, target):
        self.moves.append(target)
        return self


class _Processor:
    def __init__(self) -> None:
        self.batch = _Batch()
        self.arguments = None

    def apply_chat_template(self, history, **arguments):
        del history, arguments
        return "prompt"

    def __call__(self, **arguments):
        self.arguments = arguments
        return self.batch


class _Model:
    device = "cuda:0"
    dtype = "bfloat16"

    def __init__(self) -> None:
        self.arguments = None

    def generate(self, **arguments):
        self.arguments = arguments
        return object()


def test_generation_uses_qwen_thinker_limit_and_model_dtype(monkeypatch) -> None:
    sidecar = qwen.Qwen3OmniSidecar(
        io.BytesIO(), io.BytesIO(), model_path="test", mock=False,
        device="cuda", max_new_tokens=192,
    )
    processor = _Processor()
    model = _Model()
    sidecar.processor = processor
    sidecar.model = model
    monkeypatch.setattr(qwen, "split_outputs", lambda outputs, proc, inputs: ("ok", None))

    text, waveform = sidecar._generate([], 64, False)

    assert (text, waveform) == ("ok", None)
    assert processor.batch.moves == ["cuda:0", "bfloat16"]
    assert model.arguments["thinker_max_new_tokens"] == 64
    assert "max_new_tokens" not in model.arguments
    assert model.arguments["return_audio"] is False


def test_take_audio_does_not_reuse_the_previous_turn() -> None:
    sidecar = qwen.Qwen3OmniSidecar(
        io.BytesIO(), io.BytesIO(), model_path="test", mock=True,
        device="cpu", max_new_tokens=16,
    )
    sidecar.input_rate = qwen.MODEL_INPUT_RATE
    samples = np.array([0, 16384, -16384], dtype=np.int16)
    sidecar.on_audio(samples.tobytes())

    first = sidecar._take_audio()
    second = sidecar._take_audio()

    np.testing.assert_allclose(first, [0.0, 0.5, -0.5])
    assert second.size == 0
