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
import json
import re
import socketserver
import sys
import threading
import time
import uuid
import wave
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import numpy as np  # noqa: E402
from PIL import Image  # noqa: E402

from openrealtime_sidecar import Capability, Sidecar, log, run  # noqa: E402

DEFAULT_MODEL = "Qwen/Qwen3-Omni-30B-A3B-Instruct"
#: The model's audio encoder expects 16 kHz; its speech output is 24 kHz.
MODEL_INPUT_RATE = 16_000
MODEL_OUTPUT_RATE = 24_000


class ModelBackend:
    """One loaded checkpoint shared by sequential or concurrent sessions."""

    def __init__(self, *, model_path: str, device: str, mock: bool) -> None:
        self.model_path = model_path
        self.device = device
        self.mock = mock
        self.model = None
        self.processor = None
        self.lock = threading.Lock()

    def load(self) -> None:
        if self.mock or self.model is not None:
            return
        from transformers import (  # noqa: PLC0415
            AutoProcessor,
            Qwen3OmniMoeForConditionalGeneration,
        )

        log(f"loading shared {self.model_path}")
        self.processor = AutoProcessor.from_pretrained(self.model_path)
        self.model = Qwen3OmniMoeForConditionalGeneration.from_pretrained(
            self.model_path, dtype="auto", device_map=self.device,
        )
        self.model.eval()
        log("shared model ready")


class Qwen3OmniSidecar(Sidecar):
    """Qwen3-Omni behind the sidecar protocol."""

    model_name = DEFAULT_MODEL
    output_rate = MODEL_OUTPUT_RATE
    #: No native voice activity detection and no full duplex: the engine keeps
    #: the floor, which is the point of running this model here.
    capabilities = (
        Capability.TEXT_INJECTION,
        Capability.TOOLS,
        Capability.INTERACTION_ACTS,
        Capability.VISUAL_INPUT,
    )

    def __init__(self, input_stream, output_stream, *, model_path: str, mock: bool,
                 device: str, max_new_tokens: int,
                 backend: ModelBackend | None = None) -> None:
        super().__init__(input_stream, output_stream)
        self.model_path = model_path
        self.mock = mock
        self.device = device
        self.max_new_tokens = max_new_tokens
        self.backend = backend
        self.model = None
        self.processor = None
        self._audio = bytearray()
        self._history: list[dict] = []
        self._images: dict[str, Image.Image] = {}
        self._media_lock = threading.Lock()
        self._generation_lock = backend.lock if backend is not None else threading.Lock()
        self._visual_wakeup = threading.Event()
        self._visual_stop = threading.Event()
        self._visual_worker: threading.Thread | None = None
        self._armed = False
        self._pending_calls: dict[str, str] = {}
        self._last_action_at = 0.0

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
        if self.backend is not None:
            self.backend.load()
            self.processor = self.backend.processor
            self.model = self.backend.model
            self._start_visual_actor()
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
        self._start_visual_actor()

    def on_close(self) -> None:
        self._visual_stop.set()
        self._visual_wakeup.set()
        if self._visual_worker is not None:
            self._visual_worker.join(timeout=5)
        if self.backend is None:
            self.model = None
            self.processor = None

    # --- session ------------------------------------------------------------

    def on_audio(self, pcm16: bytes) -> None:
        # Audio accumulates until the engine says the turn ended. A turn-based
        # model has nothing useful to do with a partial utterance, and
        # pretending otherwise would burn a GPU on every frame.
        # Audio arrives on the protocol reader while a response can be taking
        # the preceding buffer on the worker. Protect the copy-and-clear pair:
        # an append between those two operations would otherwise be erased and
        # the first samples of the next utterance would disappear.
        with self._media_lock:
            self._audio.extend(pcm16)

    def on_image(self, encoded: bytes, *, source: str, mime_type: str,
                 width: int, height: int, timestamp_ms: int) -> None:
        del mime_type, width, height, timestamp_ms
        image = Image.open(io.BytesIO(encoded)).convert("RGB")
        image.load()
        with self._media_lock:
            self._images[source or "screen"] = image
        if self._armed and self.tools:
            self._visual_wakeup.set()

    def on_tools_update(self, tools: list[dict]) -> None:
        super().on_tools_update(tools)
        if self._armed and tools:
            self._visual_wakeup.set()

    def on_text(self, text: str, role: str) -> None:
        # This is the background reasoner's completed answer arriving. It goes
        # into history as context; the engine decides when the model speaks.
        self._history.append({"role": role or "system", "content": [
            {"type": "text", "text": text},
        ]})

    def on_respond(self) -> None:
        audio = self._take_audio()
        self._armed = True
        if self.mock:
            self._respond_mock(audio)
            return
        self._respond_model(audio)

    def on_tool_result(self, message) -> None:
        call_id = str(message.get("call_id", ""))
        name = self._pending_calls.pop(call_id, "tool")
        if message.get("error"):
            result = json.dumps({"error": str(message.get("error"))})
        else:
            output = message.get("output", {})
            result = output if isinstance(output, str) else json.dumps(output)
        self._history.append({"role": "system", "content": [{
            "type": "text", "text": f"The {name} tool returned: {result}",
        }]})
        self._last_action_at = time.monotonic()
        if self.mock:
            return
        self._continue_model()
        self.turn_done()

    # --- generation ---------------------------------------------------------

    def _take_audio(self) -> np.ndarray:
        with self._media_lock:
            raw = bytes(self._audio)
            self._audio.clear()
        if not raw:
            return np.zeros(0, dtype=np.float32)
        samples = np.frombuffer(raw, dtype=np.int16).astype(np.float32) / 32768.0
        return resample(samples, self.input_rate, MODEL_INPUT_RATE)

    def _respond_model(self, audio: np.ndarray) -> None:
        content = self._current_images()
        if audio.size:
            content.append({"type": "audio", "audio": audio})
        if not content:
            content.append({"type": "text", "text": "(the user said nothing)"})
        self._history.append({"role": "user", "content": content})
        text, waveform = self._generate(self._history, self.max_new_tokens, True)
        if self._emit_tool_call(text):
            return
        if text:
            self.text_delta(text)
            self.text_done(text)
            self._history.append({"role": "assistant", "content": [
                {"type": "text", "text": text},
            ]})
        if waveform is not None and waveform.size:
            self._emit_audio(waveform)

    def _continue_model(self) -> None:
        content = self._current_images()
        content.append({
            "type": "text",
            "text": "Continue from the tool result. Speak briefly, or call one next tool if required.",
        })
        temporary = self._history + [{"role": "user", "content": content}]
        text, waveform = self._generate(temporary, self.max_new_tokens, True)
        if self._emit_tool_call(text):
            return
        if text:
            self.text_delta(text)
            self.text_done(text)
            self._history.append({"role": "assistant", "content": [
                {"type": "text", "text": text},
            ]})
        if waveform is not None and waveform.size:
            self._emit_audio(waveform)

    def _generate(self, history: list[dict], max_tokens: int,
                  return_audio: bool) -> tuple[str, np.ndarray | None]:
        tools = self._chat_tools()
        template_arguments = {
            "add_generation_prompt": True,
            "tokenize": False,
        }
        if tools:
            template_arguments["tools"] = tools
        text_prompt = self.processor.apply_chat_template(history, **template_arguments)
        images, audios = collect_media(history)
        processor_arguments = {
            "text": text_prompt,
            "sampling_rate": MODEL_INPUT_RATE,
            "return_tensors": "pt",
            "padding": True,
        }
        if images:
            processor_arguments["images"] = images
        if audios:
            processor_arguments["audio"] = audios
        with self._generation_lock:
            # Qwen3-Omni's processor can combine images and audio in one call.
            # Its official path moves floating multimodal tensors to the model
            # dtype as well as the device; leaving them as float32 is both more
            # expensive and incompatible with some quantized checkpoints.
            inputs = self.processor(**processor_arguments).to(self.model.device)
            inputs = inputs.to(self.model.dtype)
            outputs = self.model.generate(
                **inputs, thinker_max_new_tokens=max_tokens, return_audio=return_audio,
                thinker_return_dict_in_generate=True,
            )
        return split_outputs(outputs, self.processor, inputs)

    def _chat_tools(self) -> list[dict]:
        result = []
        for tool in self.tools:
            result.append({"type": "function", "function": {
                "name": str(tool.get("name", "")),
                "description": str(tool.get("description", "")),
                "parameters": tool.get("parameters") or {"type": "object"},
            }})
        return result

    def _current_images(self) -> list[dict]:
        with self._media_lock:
            images = list(self._images.values())
        return [{"type": "image", "image": image} for image in images]

    def _emit_tool_call(self, text: str) -> bool:
        calls = extract_tool_calls(text)
        if not calls:
            return False
        name, arguments = calls[0]
        allowed = {str(tool.get("name", "")) for tool in self.tools}
        if name not in allowed:
            return False
        call_id = "qwen_" + uuid.uuid4().hex
        self._pending_calls[call_id] = name
        self._history.append({"role": "assistant", "content": [{
            "type": "text", "text": text,
        }]})
        self.send(
            "tool_call", call_id=call_id, name=name,
            arguments=arguments,
        )
        return True

    def _start_visual_actor(self) -> None:
        if self._visual_worker is not None:
            return
        self._visual_worker = threading.Thread(
            target=self._visual_loop, name="qwen-visual-actor", daemon=True,
        )
        self._visual_worker.start()

    def _visual_loop(self) -> None:
        while not self._visual_stop.is_set():
            self._visual_wakeup.wait(timeout=0.5)
            self._visual_wakeup.clear()
            if self._visual_stop.is_set() or not self._armed or not self.tools:
                continue
            if self._pending_calls or time.monotonic() - self._last_action_at < 0.75:
                continue
            content = self._current_images()
            if not content:
                continue
            content.append({"type": "text", "text": (
                "This is a silent visual-control tick. From the current user task and current "
                "screen, call exactly one offered computer tool only if an immediate action is "
                "required now. Otherwise answer only WAIT. Never explain or speak."
            )})
            history = self._history + [{"role": "user", "content": content}]
            try:
                text, _ = self._generate(history, min(self.max_new_tokens, 64), False)
                self._emit_tool_call(text)
            except Exception as failure:  # noqa: BLE001
                log(f"visual actor: {failure}")

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


def collect_media(history: list[dict]) -> tuple[list[Image.Image], list[np.ndarray]]:
    images: list[Image.Image] = []
    audios: list[np.ndarray] = []
    for message in history:
        content = message.get("content", [])
        if not isinstance(content, list):
            continue
        for item in content:
            if item.get("type") == "image" and item.get("image") is not None:
                images.append(item["image"])
            elif item.get("type") == "audio" and item.get("audio") is not None:
                audios.append(item["audio"])
    return images, audios


def extract_tool_calls(text: str) -> list[tuple[str, dict]]:
    """Parse Qwen's documented tool-call envelope without accepting prose."""
    blocks = re.findall(r"<tool_call>\s*(\{.*?\})\s*</tool_call>", text, re.DOTALL)
    if not blocks and text.strip().startswith("{"):
        blocks = [text.strip()]
    calls: list[tuple[str, dict]] = []
    for block in blocks:
        try:
            decoded = json.loads(block)
        except json.JSONDecodeError:
            continue
        name = str(decoded.get("name", "")).strip()
        arguments = decoded.get("arguments", {})
        if isinstance(arguments, str):
            try:
                arguments = json.loads(arguments)
            except json.JSONDecodeError:
                continue
        if name and isinstance(arguments, dict):
            calls.append((name, arguments))
    return calls


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
        "--listen", default="",
        help="persistent TCP address such as 127.0.0.1:9000; loads the model once",
    )
    parser.add_argument(
        "--mock", action="store_true",
        help="speak the protocol without loading a model, for plumbing and conformance",
    )
    arguments = parser.parse_args()
    if arguments.listen:
        host, separator, port = arguments.listen.rpartition(":")
        if not separator or not port.isdigit():
            parser.error("--listen must be host:port")
        backend = ModelBackend(
            model_path=arguments.model, device=arguments.device, mock=arguments.mock,
        )
        backend.load()

        class Handler(socketserver.BaseRequestHandler):
            def handle(self) -> None:
                input_stream = self.request.makefile("rb")
                output_stream = self.request.makefile("wb")
                try:
                    Qwen3OmniSidecar(
                        input_stream, output_stream, model_path=arguments.model,
                        mock=arguments.mock, device=arguments.device,
                        max_new_tokens=arguments.max_new_tokens, backend=backend,
                    ).run()
                finally:
                    input_stream.close()
                    output_stream.close()

        class Server(socketserver.ThreadingTCPServer):
            allow_reuse_address = True
            daemon_threads = True

        with Server((host or "127.0.0.1", int(port)), Handler) as server:
            log(f"qwen3-omni sidecar listening on {host or '127.0.0.1'}:{port}")
            server.serve_forever()
        return

    run(Qwen3OmniSidecar, model_path=arguments.model, mock=arguments.mock,
        device=arguments.device, max_new_tokens=arguments.max_new_tokens)


if __name__ == "__main__":
    main()
