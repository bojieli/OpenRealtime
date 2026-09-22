#!/usr/bin/env python3
"""VibeVoice-Realtime-0.5B served on the duplex-plan synthesis contract.

The model interleaves text and speech in fixed windows: the base LM and the
TTS LM take 5 text tokens, then the TTS LM generates 6 acoustic latents
(7.5 Hz, 0.8 s of audio) by diffusion, then the next 5 text tokens, and so on;
when text runs out it keeps generating speech windows until its EOS head
fires. Upstream's ``generate()`` slices those windows out of a complete
``tts_text_ids`` tensor, and the model card still lists "feed new tokens while
audio is still being generated" as a TODO.

This backend implements that TODO without changing the model's schedule: the
window loop is upstream's, but each 5-token window is taken from a text source
that *blocks* until the window is complete (or text has ended). Because the LMs
are causal and a window is only ever consumed whole, the state at every window
boundary is exactly the state upstream reaches with the complete text; the
only difference is when the window arrives. The KV-cache bookkeeping that
upstream does one window early (it sizes the next window before generating the
current window's speech) is deferred to when the next window is actually fed,
so no extra lookahead is introduced. Declared: ``incremental_text`` at
``token`` granularity (5-token windows). A nonterminal flush is not offered: a
partial window is only something the model sees at the end of text.

Appended text is released to the tokenizer at word boundaries (see
``WordGate``) so the BPE sees the same tokens the complete text would give.

Voice: the bundled embedded prompt ``en-Carter_man`` (upstream's default).
Language: English only (the model card says other languages "may produce
unpredictable results"; there is no Mandarin voice). Attention is SDPA:
flash-attn has no sm_120 build, and upstream warns only FA2 was fully tested.

    python tools/duplexmodels/tts_vibevoice.py --port 9121
"""

from __future__ import annotations

import argparse
import copy
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

log = logging.getLogger("tts_vibevoice")

VOICES = PLAN / "src" / "VibeVoice" / "demo" / "voices" / "streaming_model"
TEXT_WINDOW = 5
SPEECH_WINDOW = 6


class WordGate:
    """Release appended text at whitespace so BPE sees whole words."""

    def __init__(self) -> None:
        self.pending = ""

    def push(self, text: str) -> str:
        self.pending += text
        for index in range(len(self.pending) - 1, 0, -1):
            if self.pending[index].isspace():
                ready, self.pending = self.pending[:index], self.pending[index:]
                return ready
        return ""

    def drain(self) -> str:
        ready, self.pending = self.pending, ""
        return ready


class TextSource:
    """Token windows for the generation loop; blocks while text is still coming."""

    def __init__(self, tokenizer, texts: Optional["queue.Queue[Optional[str]]"] = None,
                 complete: Optional[str] = None, lock: Optional[threading.Lock] = None,
                 cancel: Optional[threading.Event] = None) -> None:
        self.tokenizer = tokenizer
        self.texts = texts
        self.lock = lock
        self.cancel = cancel
        self.tokens: list[int] = []
        self.consumed = 0
        self.ended = False
        self.started = False
        self.gate = WordGate()
        if complete is not None:
            self.tokens = tokenizer.encode(complete.strip() + "\n", add_special_tokens=False)
            self.ended = True

    def _pull(self, block: bool) -> None:
        try:
            item = self.texts.get(block=False)
        except queue.Empty:
            if not block:
                return
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
            self.tokens += self.tokenizer.encode(tail.rstrip() + "\n", add_special_tokens=False)
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
            self.tokens += self.tokenizer.encode(ready, add_special_tokens=False)

    def window(self) -> list[int]:
        """The next text window: 5 tokens, or fewer only once text has ended."""
        while not self.ended and (self.cancel is None or not self.cancel.is_set()):
            if len(self.tokens) - self.consumed >= TEXT_WINDOW:
                break
            self._pull(block=True)
        taken = self.tokens[self.consumed:self.consumed + TEXT_WINDOW]
        self.consumed += len(taken)
        return taken


class VibeVoiceSynthesizer(Synthesizer):
    model = "vibevoice-realtime-0.5b"
    sample_rate = 24_000
    capabilities = SynthesisCapabilities(incremental_text=True, nonterminal_flush=False, input_granularity="token")
    lock = threading.Lock()

    def __init__(self, voice: str = "en-Carter_man", steps: int = 5, cfg_scale: float = 1.5) -> None:
        from huggingface_hub import snapshot_download
        from transformers.cache_utils import DynamicCache
        from transformers.modeling_outputs import BaseModelOutputWithPast
        from vibevoice.modular.modeling_vibevoice_streaming_inference import (
            VibeVoiceStreamingForConditionalGenerationInference,
        )
        from vibevoice.processor.vibevoice_streaming_processor import VibeVoiceStreamingProcessor

        began = time.perf_counter()
        path = snapshot_download("microsoft/VibeVoice-Realtime-0.5B", local_files_only=True)
        self.processor = VibeVoiceStreamingProcessor.from_pretrained(path)
        self.net = VibeVoiceStreamingForConditionalGenerationInference.from_pretrained(
            path, torch_dtype=torch.bfloat16, device_map="cuda", attn_implementation="sdpa")
        self.net.eval()
        scheduler = self.net.model.noise_scheduler
        self.net.model.noise_scheduler = scheduler.from_config(
            scheduler.config, algorithm_type="sde-dpmsolver++", beta_schedule="squaredcos_cap_v2")
        self.net.set_ddpm_inference_steps(num_steps=steps)
        self.steps = steps
        self.cfg_scale = cfg_scale
        self.voice = voice
        with torch.serialization.safe_globals([BaseModelOutputWithPast, DynamicCache]):
            self.prefilled = torch.load(VOICES / f"{voice}.pt", map_location="cuda", weights_only=True)
        self.tokenizer = self.processor.tokenizer
        self.loaded_seconds = time.perf_counter() - began
        self.stats = {"text_windows": 0, "speech_frames": 0, "text_waits": 0}
        log.info("loaded %s in %.1fs (voice=%s steps=%d cfg=%.2f)", self.model, self.loaded_seconds, voice, steps, cfg_scale)

    # -- the window loop (upstream generate(), text fed as it arrives) -------

    @torch.no_grad()
    def _generate(self, source: TextSource, cancel: threading.Event) -> Iterator[np.ndarray]:
        from vibevoice.modular.modeling_vibevoice_streaming_inference import _update_model_kwargs_for_generation
        from vibevoice.modular.modular_vibevoice_tokenizer import VibeVoiceTokenizerStreamingCache

        net, tokenizer = self.net, self.tokenizer
        prefilled = copy.deepcopy(self.prefilled)
        prepared = self.processor.process_input_with_cached_prompt(
            text="x", cached_prompt=prefilled, padding=True, return_tensors="pt", return_attention_mask=True)
        prepared = {k: v.to("cuda") if hasattr(v, "to") else v for k, v in prepared.items()}
        neg_id = tokenizer.convert_tokens_to_ids("<|image_pad|>")
        max_new = net.config.decoder_config.max_position_embeddings - prepared["tts_lm_input_ids"].shape[-1]

        generation_config, model_kwargs, input_ids = net._build_generate_config_model_kwargs(
            {"do_sample": False}, None, tokenizer, return_processors=False,
            input_ids=prepared["input_ids"], attention_mask=prepared["attention_mask"], max_new_tokens=max_new)
        ones = lambda: torch.ones((1, 1), dtype=torch.long, device="cuda")  # noqa: E731
        _, neg_kwargs, _ = net._build_generate_config_model_kwargs(
            None, None, tokenizer, return_processors=False,
            input_ids=torch.full((1, 1), neg_id, dtype=torch.long, device="cuda"), attention_mask=ones(),
            max_new_tokens=max_new)
        tts_config, tts_kwargs, tts_ids = net._build_generate_config_model_kwargs(
            None, None, tokenizer, return_processors=False,
            input_ids=prepared["tts_lm_input_ids"], attention_mask=prepared["tts_lm_attention_mask"],
            max_new_tokens=max_new)
        _, tts_neg_kwargs, tts_neg_ids = net._build_generate_config_model_kwargs(
            None, None, tokenizer, return_processors=False,
            input_ids=torch.full((1, 1), neg_id, dtype=torch.long, device="cuda"), attention_mask=ones(),
            max_new_tokens=max_new)

        acoustic_cache = VibeVoiceTokenizerStreamingCache()
        lm_out = prefilled["lm"]
        tts_out = prefilled["tts_lm"]
        neg_out = prefilled["neg_lm"]
        tts_neg_out = prefilled["neg_tts_lm"]
        neg_kwargs = net._update_model_kwargs_for_generation(neg_out, neg_kwargs, is_encoder_decoder=False)
        tts_neg_kwargs = net._update_model_kwargs_for_generation(tts_neg_out, tts_neg_kwargs, is_encoder_decoder=False)
        # tts_out's kwargs update is pending until we know whether text follows.
        indices = torch.LongTensor([0])
        max_length = tts_config.max_length

        while not cancel.is_set():
            waited = source.consumed == len(source.tokens) and not source.ended
            window = source.window()
            if cancel.is_set():
                return
            if waited:
                self.stats["text_waits"] += 1
            if window:
                count = len(window)
                ids = torch.tensor([window], dtype=torch.long, device="cuda")
                model_kwargs = _update_model_kwargs_for_generation(lm_out, model_kwargs, num_new_tokens=count)
                tts_kwargs = _update_model_kwargs_for_generation(tts_out, tts_kwargs, num_new_tokens=count)
                input_ids = torch.cat([input_ids, ids], dim=-1)
                tts_ids = torch.cat([tts_ids, ids], dim=-1)
                if tts_ids.shape[1] > max_length:
                    log.warning("reached maximum length")
                    return
                lm_out = net.forward_lm(**net.prepare_inputs_for_generation(input_ids, **model_kwargs),
                                        return_dict=True, output_attentions=False, output_hidden_states=False)
                tts_out = net.forward_tts_lm(**net.prepare_inputs_for_generation(tts_ids, **tts_kwargs),
                                             tts_text_masks=torch.ones_like(tts_ids[:, -1:]),
                                             lm_last_hidden_state=lm_out.last_hidden_state,
                                             return_dict=True, output_attentions=False, output_hidden_states=False)
                self.stats["text_windows"] += 1
            tts_kwargs = net._update_model_kwargs_for_generation(tts_out, tts_kwargs, is_encoder_decoder=False)

            for frame in range(SPEECH_WINDOW):
                if cancel.is_set():
                    return
                positive = tts_out.last_hidden_state[indices, -1, :]
                negative = tts_neg_out.last_hidden_state[indices, -1, :]
                latent = net.sample_speech_tokens(positive, negative, cfg_scale=self.cfg_scale).unsqueeze(1)
                scaled = latent / net.model.speech_scaling_factor.to(latent.device) - net.model.speech_bias_factor.to(latent.device)
                audio = net.model.acoustic_tokenizer.decode(scaled.to(net.model.acoustic_tokenizer.device),
                                                            cache=acoustic_cache, sample_indices=indices.to(net.model.acoustic_tokenizer.device),
                                                            use_cache=True, debug=False)
                chunk = audio[0].detach().float().cpu().numpy().reshape(-1)
                peak = float(np.max(np.abs(chunk))) if chunk.size else 0.0
                if peak > 1.0:
                    chunk = chunk / peak
                self.stats["speech_frames"] += 1
                yield chunk

                embed = net.model.acoustic_connector(latent)
                tts_ids = torch.cat([tts_ids, torch.ones_like(tts_ids[:, -1:])], dim=-1)
                if tts_ids.shape[1] > max_length:
                    return
                tts_out = net.forward_tts_lm(**net.prepare_inputs_for_generation(tts_ids, **tts_kwargs),
                                             tts_text_masks=torch.zeros_like(tts_ids[:, -1:]), lm_last_hidden_state=embed,
                                             return_dict=True, output_attentions=False, output_hidden_states=False)
                if frame < SPEECH_WINDOW - 1:
                    tts_kwargs = net._update_model_kwargs_for_generation(tts_out, tts_kwargs, is_encoder_decoder=False)
                # else: the update waits for the next window (see the loop head).
                tts_neg_ids = torch.cat([tts_neg_ids, torch.ones_like(tts_ids[:, -1:])], dim=-1)
                tts_neg_out = net.forward_tts_lm(**net.prepare_inputs_for_generation(tts_neg_ids, **tts_neg_kwargs),
                                                 tts_text_masks=torch.zeros_like(tts_neg_ids[:, -1:]), lm_last_hidden_state=embed,
                                                 return_dict=True, output_attentions=False, output_hidden_states=False)
                tts_neg_kwargs = net._update_model_kwargs_for_generation(tts_neg_out, tts_neg_kwargs, is_encoder_decoder=False)
                if torch.sigmoid(net.tts_eos_classifier(tts_out.last_hidden_state[indices, -1, :]))[0].item() > 0.5:
                    return

    # -- contract ----------------------------------------------------------

    def synthesize(self, text: str, voice: str, cancel: threading.Event) -> Iterator[np.ndarray]:
        text = text.replace("’", "'")
        yield from self._generate(TextSource(self.tokenizer, complete=text), cancel)

    def synthesize_incremental(self, texts: "queue.Queue[Optional[str]]", voice: str,
                               cancel: threading.Event) -> Iterator[np.ndarray]:
        source = TextSource(self.tokenizer, texts=texts, lock=self.lock, cancel=cancel)
        self.lock.acquire()
        try:
            yield from self._generate(source, cancel)
        finally:
            if self.lock.locked():
                self.lock.release()

    def health(self) -> dict:
        return {"voice": f"vibevoice-embedded-{self.voice}", "languages": ["en"], "ddpm_steps": self.steps,
                "cfg_scale": self.cfg_scale, "attention": "sdpa", "loaded_seconds": round(self.loaded_seconds, 1),
                "text_window_tokens": TEXT_WINDOW, "speech_window_frames": SPEECH_WINDOW,
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
    parser.add_argument("--port", type=int, default=9121)
    parser.add_argument("--voice", default="en-Carter_man")
    parser.add_argument("--steps", type=int, default=5)
    parser.add_argument("--cfg-scale", type=float, default=1.5)
    args = parser.parse_args()
    configure_logging()
    synth = VibeVoiceSynthesizer(voice=args.voice, steps=args.steps, cfg_scale=args.cfg_scale)
    cancel = threading.Event()
    for _ in synth.synthesize("Hello there, this is a warm up sentence for the service.", "default", cancel):
        pass
    log.info("ready on :%d", args.port)
    serve(synth, args.host, args.port)


if __name__ == "__main__":
    main()
