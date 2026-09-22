#!/usr/bin/env python3
"""Fish Audio S2 Pro served on the duplex-plan synthesis contract.

S2 Pro is a dual-AR model (a 4B slow transformer over text + semantic tokens,
a fast transformer over the remaining 9 codebooks) prompted as a chat
conversation: system (reference transcript + reference codes), user (the text
to speak), assistant (voice). The text is part of the prompt *before*
generation starts; nothing in the model or in fish-speech's engine lets text
join a running generation. So this backend declares
``incremental_text=False`` and ``input_granularity=sentence``: on the
incremental route the scaffold buffers appended text to sentence boundaries and
synthesises each sentence as a complete request.

What is streamed is the *output*: fish-speech's engine returns one segment per
text batch (a whole sentence of audio). Here the slow/fast AR loop is driven a
frame at a time (the same ``decode_one_token_ar`` and sampling fish-speech
uses) and the codec re-decodes the accumulated codes every few frames, holding
back the last frames whose samples still depend on right context, so audio
starts before the sentence is finished.

Voice: CosyVoice's bundled ``asset/zero_shot_prompt.wav`` (Mandarin female,
3.48 s) with transcript "希望你以后能够做的比我还好呦。" - the same reference as
the CosyVoice and Qwen3-TTS services. Output is the codec's native 44.1 kHz.
English and Mandarin are both supported by the checkpoint.

Runs in the repository's existing fish-speech 2.0 environment
(``.runtime/fish-env``). Needs the large-model GPU lease (~20 GB).

    python tools/duplexmodels/tts_fish.py --port 9124
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
import threading
import time
from copy import deepcopy
from pathlib import Path
from typing import Iterator

os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
PLAN = ROOT / ".runtime" / "duplex-plan"
FISH = ROOT / ".runtime" / "fish-speech"
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(FISH))

import numpy as np  # noqa: E402
import torch  # noqa: E402

from common import SynthesisCapabilities, Synthesizer, configure_logging, serve_synthesizer  # noqa: E402

log = logging.getLogger("tts_fish")

CHECKPOINT = FISH / "checkpoints" / "s2-pro"
PROMPT_WAV = PLAN / "src" / "CosyVoice" / "asset" / "zero_shot_prompt.wav"
PROMPT_TEXT = "希望你以后能够做的比我还好呦。"


@torch.inference_mode()
def encode_reference_audio(path, codec, device):
    """Upstream encode_audio semantics using SoundFile instead of TorchCodec.

    Only file decoding changes; retain torchaudio resampling, mono averaging,
    codec dtype, and the feature-length slice from the upstream implementation.
    """
    import soundfile as sf
    import torchaudio
    samples, rate = sf.read(str(path), dtype='float32', always_2d=True)
    wav = torch.from_numpy(samples.T.copy())
    if wav.shape[0] > 1:
        wav = wav.mean(dim=0, keepdim=True)
    wav = torchaudio.functional.resample(wav.to(device), rate, codec.sample_rate)[0]
    audios = wav[None, None].to(dtype=next(codec.parameters()).dtype)
    lengths = torch.tensor([len(wav)],device=device,dtype=torch.long)
    indices, feature_lengths = codec.encode(audios,lengths)
    return indices[0, :, :feature_lengths[0]]


class FishS2ProSynthesizer(Synthesizer):
    model = "fish-s2-pro"
    sample_rate = 44_100
    capabilities = SynthesisCapabilities(incremental_text=False, nonterminal_flush=True, input_granularity="sentence")
    lock = threading.Lock()

    def __init__(self, compile_graphs: bool = True, first_chunk: int = 10, chunk: int = 16, holdback: int = 2,
                 max_seq_len: int = 4096, temperature: float = 0.8, top_p: float = 0.8, top_k: int = 30) -> None:
        from fish_speech.models.text2semantic.inference import decode_one_token_ar, load_codec_model
        from fish_speech.models.text2semantic.llama import DualARTransformer

        began = time.perf_counter()
        self.device = "cuda"
        self.precision = torch.bfloat16
        model = DualARTransformer.from_pretrained(str(CHECKPOINT), load_weights=True, max_length=max_seq_len)
        self.net = model.to(device=self.device, dtype=self.precision).eval()
        with torch.device(self.device):
            self.net.setup_caches(max_batch_size=1, max_seq_len=max_seq_len, dtype=self.precision)
        self.prefill_one = decode_one_token_ar
        self.decode_one = decode_one_token_ar
        if compile_graphs:
            self.decode_one = torch.compile(decode_one_token_ar, backend="inductor", mode="default", fullgraph=True)
        self.compiled = compile_graphs
        self.codec = load_codec_model(str(CHECKPOINT / "codec.pth"), self.device, self.precision)
        self.sample_rate = int(self.codec.sample_rate)
        self.prompt_codes = encode_reference_audio(PROMPT_WAV, self.codec, self.device).cpu()
        self.first_chunk, self.chunk, self.holdback = first_chunk, chunk, holdback
        self.sampling = dict(temperature=temperature, top_p=top_p, top_k=top_k)
        self.max_seq_len = max_seq_len
        self._build_conversation()
        self.loaded_seconds = time.perf_counter() - began
        self.stats = {"frames": 0}
        log.info("loaded %s in %.1fs (compile=%s, codec %d Hz)", self.model, self.loaded_seconds,
                 compile_graphs, self.sample_rate)

    def _build_conversation(self) -> None:
        from fish_speech.content_sequence import TextPart, VQPart
        from fish_speech.conversation import Conversation, Message

        # Same system prompt generate_long builds for one reference speaker.
        base = Conversation()
        base.append(Message(role="system", parts=[
            TextPart(text="convert the provided text to speech reference to the following:\n\nText:\n", cal_loss=False),
            TextPart(text=f"<|speaker:0|>{PROMPT_TEXT}", cal_loss=False),
            TextPart(text="\n\nSpeech:\n", cal_loss=False),
            VQPart(codes=self.prompt_codes, cal_loss=False),
        ], cal_loss=False, add_im_start=True, add_im_end=True))
        self.base = base

    def _encode(self, text: str):
        from fish_speech.content_sequence import TextPart
        from fish_speech.conversation import Message

        conversation = deepcopy(self.base)
        conversation.append(Message(role="user", parts=[TextPart(text=text, cal_loss=False)], cal_loss=False,
                                    add_im_start=True, add_im_end=True))
        conversation.append(Message(role="assistant", parts=[], cal_loss=False, modality="voice",
                                    add_im_start=True, add_im_end=False))
        return conversation.encode_for_inference(self.net.tokenizer, num_codebooks=self.net.config.num_codebooks)

    @torch.inference_mode()
    def _decode(self, codes: list[torch.Tensor], upto: int) -> np.ndarray:
        indices = torch.stack(codes, dim=1).unsqueeze(0).long()  # 1, codebooks, T
        audio = self.codec.from_indices(indices)[0, 0]
        per_frame = audio.shape[-1] // indices.shape[-1]
        return audio[: upto * per_frame].float().cpu().numpy(), per_frame

    @torch.inference_mode()
    def synthesize(self, text: str, voice: str, cancel: threading.Event) -> Iterator[np.ndarray]:
        from fish_speech.models.text2semantic.inference import RAS_WIN_SIZE
        from fish_speech.tokenizer import IM_END_TOKEN
        from torch.nn.attention import SDPBackend, sdpa_kernel

        net, device = self.net, self.device
        encoded, audio_masks, audio_parts = self._encode(text.strip())
        encoded = encoded.to(device)
        length = encoded.size(1)
        if length > self.max_seq_len - 1024:
            raise ValueError(f"prompt too long: {length}")
        dtype = next(net.parameters()).dtype
        temperature = torch.tensor(self.sampling["temperature"], device=device, dtype=dtype)
        top_p = torch.tensor(self.sampling["top_p"], device=device, dtype=dtype)
        top_k = self.sampling["top_k"]
        bias = torch.full((1, 1, net.config.vocab_size), float("-inf"), device=device, dtype=dtype)
        bias[0, 0, net.config.semantic_begin_id: net.config.semantic_end_id + 1] = 0.0
        im_end = net.tokenizer.get_token_id(IM_END_TOKEN)
        bias[0, 0, im_end] = 0.0
        width = net.config.num_codebooks + 1

        token = self.prefill_one(net, encoded.view(1, width, -1), torch.arange(0, length, device=device, dtype=torch.long),
                                 temperature, top_p, top_k, bias, audio_masks, audio_parts)
        position = torch.tensor([length], device=device, dtype=torch.int)
        previous = torch.zeros((width, RAS_WIN_SIZE), dtype=torch.int, device=device)
        codes: list[torch.Tensor] = []
        emitted = 0  # frames whose samples have been sent
        sent = 0     # samples sent
        budget = self.max_seq_len - length - 1
        for _ in range(budget):
            if cancel.is_set():
                return
            current = token.view(1, width, -1)
            if int(current[0, 0, -1]) == im_end:
                break
            codes.append(current[0, 1:, -1].clone())
            self.stats["frames"] += 1
            ready = len(codes) - self.holdback
            if ready - emitted >= (self.first_chunk if emitted == 0 else self.chunk):
                audio, _ = self._decode(codes, ready)
                yield audio[sent:]
                sent, emitted = audio.shape[-1], ready
            with sdpa_kernel(SDPBackend.MATH):
                token = self.decode_one(model=net, x=current, input_pos=position, previous_tokens=previous,
                                        temperature=temperature, top_p=top_p, top_k=top_k, semantic_logit_bias=bias,
                                        audio_masks=audio_masks, audio_parts=audio_parts).clone()
            position += 1
            previous = previous.roll(-1, dims=1)
            previous[:, -1] = token.view(width, -1)[:, 0]
        if codes and not cancel.is_set():
            audio, _ = self._decode(codes, len(codes))
            if audio.shape[-1] > sent:
                yield audio[sent:]

    def synthesize_incremental(self, texts, voice: str, cancel: threading.Event) -> Iterator[np.ndarray]:
        """Buffer to sentence boundaries (the declared granularity), stream each sentence.

        The scaffold's default does the same buffering but collects a whole
        sentence's audio before yielding it; this keeps the within-sentence
        output streaming. The lock is held only while a sentence is generated.
        """
        from common import _SENTENCE

        buffered = ""
        while not cancel.is_set():
            item = texts.get()
            flush = item is None or item == "\x00flush"
            if item is not None and item != "\x00flush":
                buffered += item
            pieces = _SENTENCE.split(buffered)
            ready, buffered = (pieces, "") if flush else (pieces[:-1], pieces[-1] if pieces else "")
            for piece in ready:
                if piece.strip() and not cancel.is_set():
                    with self.lock:
                        yield from self.synthesize(piece.strip(), voice, cancel)
            if item is None:
                return

    def health(self) -> dict:
        return {"voice": "cosyvoice-asset-zero_shot_prompt (zh female)", "languages": ["en", "zh"],
                "compiled": self.compiled, "first_chunk_frames": self.first_chunk, "chunk_frames": self.chunk,
                "holdback_frames": self.holdback, "sampling": self.sampling,
                "loaded_seconds": round(self.loaded_seconds, 1),
                "gpu_allocated_gb": round(torch.cuda.memory_allocated() / 2**30, 2),
                "gpu_reserved_gb": round(torch.cuda.memory_reserved() / 2**30, 2), **self.stats}


def serve(synthesizer, host: str, port: int) -> None:
    """``serve_synthesizer`` with the scaffold's annotation defect worked around.

    ``common.py`` has ``from __future__ import annotations`` but imports
    ``Request``/``WebSocket`` inside ``serve_synthesizer``; FastAPI resolves
    the string annotations against common's module globals, fails, and treats
    ``request`` as a required query parameter (every speech request answers
    422). Publishing the names there makes them resolve as intended (the same
    fix as ``asr_kyutai.patch_scaffold_annotations``).
    """
    import common
    import fastapi

    for name in ("Request", "WebSocket"):
        if not hasattr(common, name):
            setattr(common, name, getattr(fastapi, name))
    serve_synthesizer(synthesizer, host, port)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9124)
    parser.add_argument("--no-compile", action="store_true")
    parser.add_argument("--first-chunk", type=int, default=10)
    parser.add_argument("--chunk", type=int, default=16)
    parser.add_argument("--holdback", type=int, default=2)
    args = parser.parse_args()
    configure_logging()
    synth = FishS2ProSynthesizer(compile_graphs=not args.no_compile, first_chunk=args.first_chunk,
                                 chunk=args.chunk, holdback=args.holdback)
    FishS2ProSynthesizer.sample_rate = synth.sample_rate
    cancel = threading.Event()
    for text in ("Hello there, this is a warm up sentence.", "你好，这是一句预热的话。", "One more warm up."):
        began = time.perf_counter()
        for _ in synth.synthesize(text, "default", cancel):
            pass
        log.info("warm-up %.2fs", time.perf_counter() - began)
    log.info("ready on :%d", args.port)
    serve(synth, args.host, args.port)


if __name__ == "__main__":
    main()
