#!/usr/bin/env python3
"""Qwen3-TTS-12Hz-0.6B-Base served on the duplex-plan synthesis contract.

What the released runtimes do: ``qwen-tts`` (``Qwen3TTSModel``) returns whole
waveforms and its docstring says ``non_streaming_mode=False`` "only simulates
streaming text input ... rather than enabling true streaming input or
streaming generation"; vLLM-Omni's ``/v1/audio/speech/stream`` WebSocket
buffers text to a sentence/clause boundary and issues a request per unit.
Neither accepts text into a running generation.

What the model does: in its "streaming" prompt layout (dual track) the talker
consumes one text token per 12.5 Hz codec frame - the first text token sits
in the prefill next to ``codec_bos`` and token ``k+1`` is added to the input of
decode step ``k``, then ``tts_eos``, then ``tts_pad``. So text can arrive while
codec frames are being generated as long as token ``k+1`` exists before step
``k``. This backend drives the talker step by step (the same forward, code
predictor and sampling settings ``generate()`` uses) and blocks a step only
when its text token has not arrived yet. Declared: ``incremental_text`` at
``token`` granularity. The 12 Hz codec decoder is causal, so audio is decoded
in chunks with 25 frames of left context (upstream's ``chunked_decode``) and
streamed as it is produced; both routes use this path.

Constraint that follows from the prompt layout: only the x-vector voice prompt
is incremental. The ICL voice-clone prompt pairs reference text *and all target
text* with the reference codec frames inside the prefill whenever the text is
shorter than the reference audio, so it needs the complete text first; it is
not offered here.

Voice: x-vector speaker embedding of CosyVoice's bundled
``asset/zero_shot_prompt.wav`` (Mandarin female, 3.48 s) - the same reference
the CosyVoice and Fish services use. Language: ``auto`` (codec "nothink"
prefix), because an incremental context does not know its language up front;
English and Mandarin are both in the checkpoint's language list.

    python tools/duplexmodels/tts_qwen3.py --port 9123
"""

from __future__ import annotations

import argparse
import logging
import os
import queue
import sys
import threading
import time
from pathlib import Path
from typing import Iterator, Optional

os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
PLAN = ROOT / ".runtime" / "duplex-plan"
sys.path.insert(0, str(HERE))

import numpy as np  # noqa: E402
import torch  # noqa: E402

from common import SynthesisCapabilities, Synthesizer, configure_logging, serve_synthesizer  # noqa: E402

log = logging.getLogger("tts_qwen3")

PROMPT_WAV = PLAN / "src" / "CosyVoice" / "asset" / "zero_shot_prompt.wav"
LEFT_CONTEXT = 25  # frames of codec context per decode chunk (upstream chunked_decode)


class WordGate:
    """Release appended text at whitespace (or CJK) so BPE sees whole words."""

    def __init__(self) -> None:
        self.pending = ""

    def push(self, text: str) -> str:
        self.pending += text
        for index in range(len(self.pending) - 1, 0, -1):
            char = self.pending[index]
            if char.isspace():
                ready, self.pending = self.pending[:index], self.pending[index:]
                return ready
            if "　" <= char <= "鿿" or "＀" <= char <= "￯":
                ready, self.pending = self.pending[:index + 1], self.pending[index + 1:]
                return ready
        return ""

    def drain(self) -> str:
        ready, self.pending = self.pending, ""
        return ready


class TextTrack:
    """Text tokens for the talker's text track; blocks while text is coming."""

    def __init__(self, tokenize, texts=None, complete: Optional[str] = None,
                 lock: Optional[threading.Lock] = None, cancel: Optional[threading.Event] = None) -> None:
        self.tokenize = tokenize
        self.texts = texts
        self.lock = lock
        self.cancel = cancel
        self.tokens: list[int] = []
        self.ended = False
        self.started = False
        self.gate = WordGate()
        self.waits = 0
        if complete is not None:
            self.tokens = tokenize(complete.strip())
            self.ended = True

    def _pull(self) -> None:
        try:
            item = self.texts.get(block=False)
        except queue.Empty:
            self.waits += 1
            if self.lock is not None and self.lock.locked():
                self.lock.release()
            try:
                item = self.texts.get()
            finally:
                if self.lock is not None:
                    self.lock.acquire()
        if item is None:
            tail = self.gate.drain()
            if not self.started:
                tail = tail.lstrip()
            if tail.strip():
                self.tokens += self.tokenize(tail.rstrip())
            self.ended = True
            return
        if item == "\x00flush":
            return
        if not self.started:
            item = item.lstrip()
            if not item:
                return
            self.started = True
        ready = self.gate.push(item)
        if ready:
            self.tokens += self.tokenize(ready)

    def need(self, count: int) -> None:
        """Block until ``count`` tokens exist or text has ended."""
        while len(self.tokens) < count and not self.ended:
            if self.cancel is not None and self.cancel.is_set():
                return
            self._pull()


class Qwen3TTSSynthesizer(Synthesizer):
    model = "qwen3-tts-12hz-0.6b-base"
    sample_rate = 24_000
    capabilities = SynthesisCapabilities(incremental_text=True, nonterminal_flush=False, input_granularity="token")
    lock = threading.Lock()

    def __init__(self, first_chunk: int = 3, chunk: int = 8, language: str = "auto",
                 fast_code_predictor: bool = True) -> None:
        from huggingface_hub import snapshot_download
        from qwen_tts import Qwen3TTSModel

        began = time.perf_counter()
        path = snapshot_download("Qwen/Qwen3-TTS-12Hz-0.6B-Base", local_files_only=True)
        self.tts = Qwen3TTSModel.from_pretrained(path, device_map="cuda", dtype=torch.bfloat16,
                                                 attn_implementation="sdpa")
        self.net = self.tts.model
        self.talker = self.net.talker
        self.decoder = self.net.speech_tokenizer.model.decoder
        self.upsample = int(self.decoder.total_upsample)
        self.defaults = dict(self.tts.generate_defaults)
        self.first_chunk, self.chunk = first_chunk, chunk
        self.language = language
        items = self.tts.create_voice_clone_prompt(ref_audio=str(PROMPT_WAV), x_vector_only_mode=True)
        self.speaker = items[0].ref_spk_embedding.to(self.talker.device).to(self.talker.dtype)
        self.text_tokenizer = self.tts.processor.tokenizer
        self._build_constants()
        self.fast_code_predictor = fast_code_predictor
        if fast_code_predictor:
            self._install_fast_code_predictor()
        self.loaded_seconds = time.perf_counter() - began
        self.stats = {"frames": 0, "text_waits": 0}
        log.info("loaded %s in %.1fs", self.model, self.loaded_seconds)

    def _install_fast_code_predictor(self) -> None:
        """Replace ``code_predictor.generate`` with a plain 15-step loop.

        The talker calls HF ``generate()`` once per 80 ms frame to sample the 15
        residual codebooks; on a CPU-saturated host its per-step bookkeeping
        (logits processors, stopping criteria, cache objects) costs more than
        the tiny transformer. This loop makes the same calls - prefill on
        [past_hidden, codebook-0 embedding], then one step per codebook with
        that codebook's embedding and head - and samples the same way
        (temperature, top-k, top-p from the subtalker settings). The talker
        only reads ``.sequences`` from the result.
        """
        from types import SimpleNamespace

        from transformers.cache_utils import DynamicCache

        predictor = self.talker.code_predictor
        groups = self.net.config.talker_config.num_code_groups

        def sample(logits, do_sample, top_k, top_p, temperature):
            if not do_sample:
                return logits.argmax(dim=-1, keepdim=True)
            logits = logits.float() / temperature
            if top_k and top_k > 0:
                threshold = torch.topk(logits, min(top_k, logits.shape[-1])).values[..., -1:]
                logits = logits.masked_fill(logits < threshold, float("-inf"))
            if top_p is not None and top_p < 1.0:
                ordered, indices = torch.sort(logits, descending=True)
                cumulative = ordered.softmax(-1).cumsum(-1)
                remove = cumulative - ordered.softmax(-1) > top_p
                logits = logits.masked_fill(remove.scatter(-1, indices, remove), float("-inf"))
            return torch.multinomial(logits.softmax(-1), 1)

        @torch.no_grad()
        def generate(inputs_embeds, max_new_tokens, do_sample=True, top_p=1.0, top_k=50, temperature=0.9, **_):
            cache = DynamicCache()
            hidden = predictor.small_to_mtp_projection(inputs_embeds)
            length = hidden.shape[1]
            out = predictor.model(inputs_embeds=hidden, past_key_values=cache, use_cache=True,
                                  cache_position=torch.arange(length, device=hidden.device))
            tokens = []
            for step in range(max_new_tokens):
                logits = predictor.lm_head[step](out.last_hidden_state[:, -1])
                token = sample(logits, do_sample, top_k, top_p, temperature)
                tokens.append(token)
                if step == max_new_tokens - 1 or step + 1 >= groups - 1:
                    break
                embed = predictor.small_to_mtp_projection(predictor.model.get_input_embeddings()[step](token))
                out = predictor.model(inputs_embeds=embed, past_key_values=cache, use_cache=True,
                                      cache_position=torch.tensor([length + step], device=hidden.device))
            return SimpleNamespace(sequences=torch.cat(tokens, dim=1))

        predictor.generate = generate

    def _tokenize(self, text: str) -> list[int]:
        return self.text_tokenizer.encode(text, add_special_tokens=False)

    def _project(self, ids: list[int]) -> torch.Tensor:
        tensor = torch.tensor([ids], dtype=torch.long, device=self.talker.device)
        return self.talker.text_projection(self.talker.get_text_embeddings()(tensor))

    @torch.no_grad()
    def _build_constants(self) -> None:
        cfg, tcfg, talker = self.net.config, self.net.config.talker_config, self.talker
        device = talker.device
        self.tts_bos, self.tts_eos, self.tts_pad = self._project(
            [cfg.tts_bos_token_id, cfg.tts_eos_token_id, cfg.tts_pad_token_id]).chunk(3, dim=1)
        embed = talker.get_input_embeddings()
        if self.language == "auto":
            prefix = [tcfg.codec_nothink_id, tcfg.codec_think_bos_id, tcfg.codec_think_eos_id]
        else:
            prefix = [tcfg.codec_think_id, tcfg.codec_think_bos_id, tcfg.codec_language_id[self.language],
                      tcfg.codec_think_eos_id]
        codec = torch.cat([embed(torch.tensor([prefix], device=device)),
                           self.speaker.view(1, 1, -1),
                           embed(torch.tensor([[tcfg.codec_pad_id, tcfg.codec_bos_id]], device=device))], dim=1)
        role_ids = self._tokenize("<|im_start|>assistant\n")
        assert len(role_ids) == 3, role_ids
        role = self._project(role_ids)
        head = torch.cat((self.tts_pad.expand(-1, codec.shape[1] - 2, -1), self.tts_bos), dim=1) + codec[:, :-1]
        self.prefix_embeds = torch.cat([role, head], dim=1)
        self.codec_bos_embed = codec[:, -1:]
        self.eos_id = tcfg.codec_eos_token_id
        vocab = tcfg.vocab_size
        suppress = torch.zeros(vocab, dtype=torch.bool, device=device)
        suppress[vocab - 1024:] = True
        suppress[self.eos_id] = False
        self.suppress = suppress

    def _sample(self, logits: torch.Tensor, history: list[int], step: int) -> int:
        d = self.defaults
        scores = logits.float().clone()
        if history:
            seen = torch.tensor(sorted(set(history)), device=scores.device)
            picked = scores[seen]
            penalty = float(d.get("repetition_penalty", 1.05))
            scores[seen] = torch.where(picked > 0, picked / penalty, picked * penalty)
        scores[self.suppress] = float("-inf")
        if step < 2:  # min_new_tokens=2
            scores[self.eos_id] = float("-inf")
        if not d.get("do_sample", True):
            return int(scores.argmax())
        scores = scores / float(d.get("temperature", 0.9))
        top_k = int(d.get("top_k", 50))
        if top_k > 0:
            threshold = torch.topk(scores, top_k).values[-1]
            scores[scores < threshold] = float("-inf")
        probs = torch.softmax(scores, dim=-1)
        return int(torch.multinomial(probs, 1))

    def _decode(self, frames: list[torch.Tensor], start: int, end: int) -> np.ndarray:
        context = min(LEFT_CONTEXT, start)
        codes = torch.stack(frames[start - context:end], dim=0).transpose(0, 1).unsqueeze(0)  # 1,16,T
        wav = self.decoder(torch.clamp(codes, min=0))
        return wav[0, 0, context * self.upsample:].float().cpu().numpy()

    @torch.no_grad()
    def _generate(self, track: TextTrack, cancel: threading.Event) -> Iterator[np.ndarray]:
        d = self.defaults
        track.need(1)
        if not track.tokens or cancel.is_set():
            return
        talker, device = self.talker, self.talker.device
        prefill = torch.cat([self.prefix_embeds, self._project(track.tokens[:1]) + self.codec_bos_embed], dim=1)
        length = prefill.shape[1]
        attention = torch.ones((1, length), dtype=torch.long, device=device)
        talker.rope_deltas = None
        sub = dict(subtalker_dosample=d.get("subtalker_dosample", True), subtalker_top_k=d.get("subtalker_top_k", 50),
                   subtalker_top_p=d.get("subtalker_top_p", 1.0), subtalker_temperature=d.get("subtalker_temperature", 0.9))
        out = talker(inputs_embeds=prefill, attention_mask=attention, use_cache=True,
                     cache_position=torch.arange(length, device=device), tts_pad_embed=self.tts_pad,
                     trailing_text_hidden=self.tts_pad, **sub)
        past, past_hidden = out.past_key_values, out.past_hidden
        logits = out.logits[0, -1]
        trail: list[torch.Tensor] = []  # text track for decode steps: tokens[1:], then tts_eos
        projected = 1
        eos_added = False
        history: list[int] = []
        frames: list[torch.Tensor] = []
        emitted = 0
        max_new = int(d.get("max_new_tokens", 8192))
        for step in range(max_new):
            if cancel.is_set():
                return
            token = self._sample(logits, history, step)
            if token == self.eos_id:
                break
            history.append(token)
            # Text for this step is tokens[step + 1]; wait for it unless text has ended.
            track.need(step + 2)
            if cancel.is_set():
                return
            if len(track.tokens) > projected:
                trail.append(self._project(track.tokens[projected:]))
                projected = len(track.tokens)
            if track.ended and not eos_added:
                trail.append(self.tts_eos)
                eos_added = True
            trailing = torch.cat(trail, dim=1) if trail else self.tts_pad[:, :0]
            attention = torch.cat([attention, attention.new_ones((1, 1))], dim=1)
            out = talker(input_ids=torch.tensor([[token]], device=device), attention_mask=attention,
                         past_key_values=past, use_cache=True,
                         cache_position=torch.tensor([length + step], device=device),
                         past_hidden=past_hidden, trailing_text_hidden=trailing, tts_pad_embed=self.tts_pad,
                         generation_step=step, **sub)
            past, past_hidden = out.past_key_values, out.past_hidden
            logits = out.logits[0, -1]
            frames.append(out.hidden_states[1][0])
            self.stats["frames"] += 1
            pending = len(frames) - emitted
            if pending >= (self.first_chunk if emitted == 0 else self.chunk):
                yield self._decode(frames, emitted, len(frames))
                emitted = len(frames)
        self.stats["text_waits"] += track.waits
        if len(frames) > emitted and not cancel.is_set():
            yield self._decode(frames, emitted, len(frames))

    # -- contract ----------------------------------------------------------

    def synthesize(self, text: str, voice: str, cancel: threading.Event) -> Iterator[np.ndarray]:
        yield from self._generate(TextTrack(self._tokenize, complete=text), cancel)

    def synthesize_incremental(self, texts: "queue.Queue[Optional[str]]", voice: str,
                               cancel: threading.Event) -> Iterator[np.ndarray]:
        track = TextTrack(self._tokenize, texts=texts, lock=self.lock, cancel=cancel)
        self.lock.acquire()
        try:
            yield from self._generate(track, cancel)
        finally:
            if self.lock.locked():
                self.lock.release()

    def health(self) -> dict:
        return {"voice": "xvector:cosyvoice-asset-zero_shot_prompt (zh female)", "language": self.language,
                # One text token starts the prefill; the codec decoder is
                # causal, so audio goes out after first_chunk frames (80 ms each).
                "first_audio_needs": {"text_tokens": 1, "codec_frames": self.first_chunk,
                                      "audio_s_per_chunk": round(self.first_chunk * 0.08, 3)},
                "languages": ["en", "zh"], "first_chunk_frames": self.first_chunk, "chunk_frames": self.chunk,
                "fast_code_predictor": self.fast_code_predictor,
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
    parser.add_argument("--port", type=int, default=9123)
    parser.add_argument("--first-chunk", type=int, default=3, help="codec frames (80 ms each) before the first audio")
    parser.add_argument("--chunk", type=int, default=8)
    parser.add_argument("--language", default="auto")
    parser.add_argument("--hf-code-predictor", action="store_true",
                        help="use upstream's HF generate() for the 15 residual codebooks")
    args = parser.parse_args()
    configure_logging()
    synth = Qwen3TTSSynthesizer(first_chunk=args.first_chunk, chunk=args.chunk, language=args.language,
                                fast_code_predictor=not args.hf_code_predictor)
    cancel = threading.Event()
    for text in ("Hello there, this is a warm up sentence.", "你好，这是一句预热的话。"):
        for _ in synth.synthesize(text, "default", cancel):
            pass
    log.info("ready on :%d", args.port)
    serve(synth, args.host, args.port)


if __name__ == "__main__":
    main()
