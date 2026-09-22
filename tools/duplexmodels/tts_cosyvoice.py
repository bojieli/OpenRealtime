#!/usr/bin/env python3
"""Fun-CosyVoice3-0.5B-2512 served on the duplex-plan synthesis contract.

Complete text (``/v1/audio/speech``) runs the upstream non-streaming-input LLM
per normalised sentence and streams the flow/vocoder in 25-token hops, as
``CosyVoice3Model.tts(stream=True)`` does.

Incremental text (``/v1/tts/stream``) uses CosyVoice's *bistream* LLM input
(``Qwen2LM.inference_bistream``): the speech-token LM interleaves 5 text tokens
with 15 speech tokens and, when it emits its fill token, blocks inside the
generation until more text arrives. That is genuine text-during-generation
support, so this backend declares ``incremental_text`` at ``token``
granularity. How early it can speak is bounded by the model and the voice
prompt, and is reported rather than hidden:

* The LM consumes text in groups of 5 tokens, each followed by 15 speech
  tokens; the flow decodes in 25-token chunks plus 3 tokens of lookahead.
* ``--lm-prompt zero_shot`` (upstream zero-shot): the LM first interleaves the
  prompt's 87 speech tokens with text at 5:15, and the prompt transcript
  (~10 tokens) cannot cover them, so ~20 *new* text tokens are spent pairing
  with the prompt before the first new speech token. With upstream's first
  flow hop of 41 tokens (25 + 13 alignment pad + 3), measured: no audio before
  ~35 text tokens (~28 English words) had arrived.
* ``--lm-prompt none`` (default; CosyVoice3's instruct2 layout: system prompt
  only): the LM starts after 5 text tokens. Timbre still comes from the flow
  prompt and speaker embedding of the same prompt wav. Upstream's bistream loop
  mishandles this layout for CosyVoice3 (see ``bistream``), so the loop is
  reimplemented here with that one test corrected.
* ``--no-align-flow-prompt`` keeps upstream's first-hop padding; by default the
  flow prompt drops its oldest 12 tokens (0.48 s) so it is a whole number of
  25-token chunks and the first hop needs 28 speech tokens instead of 41.

With the defaults the first audio needs ~10 text tokens.

Upstream quirks fixed here rather than inherited: ``CosyVoice2Model.tts``
grows ``self.token_hop_len`` on the *instance*, so every request after the
first starts with a 100-token hop; and its 100 ms polling loop cannot be
cancelled. This backend drives the same LM/flow/vocoder calls with a
per-request hop and a cancel check between tokens.

Voice: the bundled ``asset/zero_shot_prompt.wav`` (Mandarin female, 3.48 s,
transcript "希望你以后能够做的比我还好呦。"). English and Mandarin are both
supported by the checkpoint.

    python tools/duplexmodels/tts_cosyvoice.py --port 9120
"""

from __future__ import annotations

import argparse
import logging
import os
import queue
import re
import sys
import threading
import time
import uuid
from pathlib import Path
from typing import Iterator, Optional

os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
PLAN = ROOT / ".runtime" / "duplex-plan"
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(PLAN / "src" / "Matcha-TTS"))
sys.path.insert(0, str(PLAN / "src" / "CosyVoice"))

import numpy as np  # noqa: E402
import soundfile  # noqa: E402
import torch  # noqa: E402
import torchaudio  # noqa: E402

from common import SynthesisCapabilities, Synthesizer, configure_logging, serve_synthesizer  # noqa: E402

log = logging.getLogger("tts_cosyvoice")


def _load_audio(path, *args, **kwargs):
    # torchaudio >= 2.9 routes load() through torchcodec (FFmpeg); the prompt
    # is a plain wav, so read it with soundfile as CosyVoice's backend= intends.
    data, rate = soundfile.read(str(path), dtype="float32", always_2d=True)
    return torch.from_numpy(np.ascontiguousarray(data.T)), rate


torchaudio.load = _load_audio

PROMPT_WAV = PLAN / "src" / "CosyVoice" / "asset" / "zero_shot_prompt.wav"
PROMPT_TEXT = "希望你以后能够做的比我还好呦。"
SYSTEM = "You are a helpful assistant.<|endofprompt|>"
_CJK = re.compile(r"[　-〿㐀-鿿＀-￯]")


def model_dir() -> str:
    from huggingface_hub import snapshot_download
    return snapshot_download("FunAudioLLM/Fun-CosyVoice3-0.5B-2512", local_files_only=True)


@torch.inference_mode()
def bistream(llm, text, prompt_text, prompt_speech_token, sampling: int = 25):
    """``Qwen2LM.inference_bistream`` for CosyVoice3 with one fix.

    Upstream starts decoding without consuming any text when the LM has no
    prompt speech, because its "append text first" test is
    ``lm_input.size(1) == 1`` - true for CosyVoice2 (``[sos]``) but never for
    CosyVoice3, whose mandatory system prompt precedes the text. The LM then
    emits 15 speech tokens conditioned on no text before its first fill
    token. Here the test is "no speech token decoded yet and no prompt speech
    to pair", which is what the CosyVoice2 branch means. Everything else
    (5:15 interleave, fill tokens, final decode) is upstream's.
    """
    device = prompt_text.device
    sos_emb = llm.speech_embedding.weight[llm.sos].reshape(1, 1, -1)
    task_id_emb = llm.speech_embedding.weight[llm.task_id].reshape(1, 1, -1)
    embed_text = llm.llm.model.model.embed_tokens
    if prompt_speech_token.shape[1] != 0:
        prompt_speech_emb = llm.speech_embedding(prompt_speech_token)
    else:
        prompt_speech_emb = torch.zeros(1, 0, llm.llm_input_size, dtype=prompt_text.dtype, device=device)
    no_prompt_speech = prompt_speech_emb.size(1) == 0
    eop_index = prompt_text.flatten().tolist().index(151646)  # <|endofprompt|>
    lm_input = torch.concat([sos_emb, embed_text(prompt_text[:, :eop_index + 1])], dim=1)
    text_cache = embed_text(prompt_text[:, eop_index + 1:])
    ratio_text, ratio_speech = llm.mix_ratio
    out_tokens: list[int] = []
    cache = None
    next_fill_index = (int(prompt_speech_token.shape[1] / ratio_speech) + 1) * ratio_speech - prompt_speech_token.shape[1]

    def step(inputs):
        nonlocal cache
        seq_len = inputs.shape[1] if cache is None else inputs.shape[1] + cache[0][0].size(2)
        y_pred, cache = llm.llm.forward_one_step(
            inputs, masks=torch.tril(torch.ones((1, seq_len, seq_len), device=device)).to(torch.bool), cache=cache)
        return llm.llm_decoder(y_pred[:, -1]).log_softmax(dim=-1)

    for this_text in text:
        text_cache = torch.concat([text_cache, embed_text(this_text)], dim=1)
        while prompt_speech_emb.size(1) != 0 and text_cache.size(1) >= ratio_text:
            lm_input = torch.concat([lm_input, text_cache[:, :ratio_text], prompt_speech_emb[:, :ratio_speech]], dim=1)
            text_cache, prompt_speech_emb = text_cache[:, ratio_text:], prompt_speech_emb[:, ratio_speech:]
        if prompt_speech_emb.size(1) != 0:
            continue
        need_text = (out_tokens and out_tokens[-1] == llm.fill_token) or (not out_tokens and no_prompt_speech)
        if need_text:
            if text_cache.size(1) < ratio_text:
                continue
            chunk = text_cache[:, :ratio_text]
            lm_input = chunk if out_tokens else torch.concat([lm_input, chunk], dim=1)
            text_cache = text_cache[:, ratio_text:]
        while True:
            logp = step(lm_input)
            if next_fill_index != -1 and len(out_tokens) == next_fill_index:
                top_ids = llm.fill_token
                next_fill_index += ratio_speech + 1
            else:
                top_ids = llm.sampling_ids(logp.squeeze(dim=0), out_tokens, sampling, ignore_eos=True)
            if top_ids == llm.fill_token:
                next_fill_index = len(out_tokens) + ratio_speech + 1
            out_tokens.append(top_ids)
            if top_ids >= llm.speech_token_size:
                if top_ids == llm.fill_token:
                    break
                raise ValueError(f"should not get token {top_ids}")
            yield top_ids
            lm_input = llm.speech_embedding.weight[top_ids].reshape(1, 1, -1)

    lm_input = torch.concat([lm_input, text_cache, task_id_emb], dim=1)
    while True:
        logp = step(lm_input)
        top_ids = llm.sampling_ids(logp.squeeze(dim=0), out_tokens, sampling, ignore_eos=False)
        out_tokens.append(top_ids)
        if top_ids >= llm.speech_token_size:
            if top_ids == llm.eos_token:
                break
            raise ValueError(f"should not get token {top_ids}")
        yield top_ids
        lm_input = llm.speech_embedding.weight[top_ids].reshape(1, 1, -1)


class WordGate:
    """Release appended text at word boundaries so BPE sees whole words.

    A Latin word split across two appends ("hel" + "lo") would otherwise be
    tokenised as two fragments the LM never saw in training. CJK characters
    and punctuation are released at once: they are already token boundaries.
    """

    def __init__(self) -> None:
        self.pending = ""

    def push(self, text: str) -> str:
        self.pending += text
        for index in range(len(self.pending) - 1, 0, -1):
            char = self.pending[index]
            if char.isspace():
                # Keep the space with the word that follows it: BPE merges a
                # leading space into the next token (" world"), never a
                # trailing one.
                ready, self.pending = self.pending[:index], self.pending[index:]
                return ready
            if _CJK.match(char):
                ready, self.pending = self.pending[:index + 1], self.pending[index + 1:]
                return ready
        return ""

    def drain(self) -> str:
        ready, self.pending = self.pending, ""
        return ready


class CosyVoice3Synthesizer(Synthesizer):
    model = "fun-cosyvoice3-0.5b"
    sample_rate = 24_000
    capabilities = SynthesisCapabilities(incremental_text=True, nonterminal_flush=False, input_granularity="token")
    lock = threading.Lock()

    def __init__(self, fp16: bool = False, lm_prompt: str = "none", align_flow_prompt: bool = True) -> None:
        from cosyvoice.cli.cosyvoice import CosyVoice3

        began = time.perf_counter()
        self.cosy = CosyVoice3(model_dir(), fp16=fp16)
        self.fp16 = fp16
        self.core = self.cosy.model  # CosyVoice3Model
        self.frontend = self.cosy.frontend
        prompt = self.frontend.frontend_zero_shot("", SYSTEM + PROMPT_TEXT, str(PROMPT_WAV), self.sample_rate, "")
        prompt.pop("text"), prompt.pop("text_len")
        self.lm_prompt = lm_prompt
        if lm_prompt == "none":
            # instruct2-style LM prompt: system prompt only, no prompt speech.
            # Timbre still comes from the flow prompt and speaker embedding.
            prompt["prompt_text"], _ = self.frontend._extract_text_token(SYSTEM)
            prompt["llm_prompt_speech_token"] = torch.zeros(1, 0, dtype=torch.int32, device=self.cosy.model.device)
        self.align_flow_prompt = align_flow_prompt
        if align_flow_prompt:
            # Drop the oldest prompt frames so the flow prompt is a whole number
            # of 25-token chunks: the first hop then needs 28 speech tokens
            # instead of 41 (upstream pads the first hop to re-align chunks).
            tokens = prompt["flow_prompt_speech_token"]
            keep = (tokens.shape[1] // 25) * 25
            cut = tokens.shape[1] - keep
            prompt["flow_prompt_speech_token"] = tokens[:, cut:]
            prompt["prompt_speech_feat"] = prompt["prompt_speech_feat"][:, 2 * cut:]
        self.prompt = prompt
        self.device = self.core.device
        self.loaded_seconds = time.perf_counter() - began
        self.stats = {"llm_tokens": 0, "flow_calls": 0}
        log.info("loaded %s in %.1fs (fp16=%s)", self.model, self.loaded_seconds, fp16)

    # -- model drivers -----------------------------------------------------

    def _llm_tokens(self, text, cancel: threading.Event, tokens: list, cond: threading.Condition,
                    done: threading.Event, errors: list, hold_lock: bool, gate: Optional[threading.Lock]) -> None:
        core, prompt = self.core, self.prompt
        llm = core.llm
        silent, max_silent = 0, 5
        held = False
        try:
            if hold_lock:
                self.lock.acquire()
                held = True
            with core.llm_context, torch.cuda.amp.autocast(self.fp16):
                prompt_text = prompt["prompt_text"].to(self.device)
                speech = prompt["llm_prompt_speech_token"].to(self.device)
                common = dict(prompt_text=prompt_text,
                              prompt_text_len=torch.tensor([prompt_text.shape[1]], dtype=torch.int32, device=self.device),
                              prompt_speech_token=speech,
                              prompt_speech_token_len=torch.tensor([speech.shape[1]], dtype=torch.int32, device=self.device),
                              embedding=prompt["llm_embedding"].to(self.device))
                if isinstance(text, torch.Tensor):
                    generator = llm.inference(text=text.to(self.device),
                                              text_len=torch.tensor([text.shape[1]], dtype=torch.int32, device=self.device),
                                              uuid=uuid.uuid4().hex, **common)
                else:
                    generator = bistream(llm, text, prompt_text, speech)
                for token in generator:
                    if cancel.is_set():
                        break
                    if token in core.silent_tokens:
                        silent += 1
                        if silent > max_silent:
                            continue
                    else:
                        silent = 0
                    with cond:
                        tokens.append(token)
                        cond.notify_all()
                generator.close()
        except Exception as error:  # noqa: BLE001
            log.exception("llm failed")
            errors.append(error)
        finally:
            if held and self.lock.locked():
                try:
                    self.lock.release()
                except RuntimeError:
                    pass
            done.set()
            with cond:
                cond.notify_all()

    def _speak(self, text, cancel: threading.Event, hold_lock: bool) -> Iterator[np.ndarray]:
        """Run the LM in a thread and stream flow+vocoder hops from its tokens."""
        core, prompt = self.core, self.prompt
        tokens: list[int] = []
        cond = threading.Condition()
        done = threading.Event()
        errors: list = []
        worker = threading.Thread(target=self._llm_tokens,
                                  args=(text, cancel, tokens, cond, done, errors, hold_lock, None), daemon=True)
        worker.start()
        key = uuid.uuid4().hex
        core.hift_cache_dict[key] = None
        flow_prompt = prompt["flow_prompt_speech_token"]
        hop = core.token_hop_len  # 25, the flow's static chunk
        pad = int(np.ceil(flow_prompt.shape[1] / hop) * hop - flow_prompt.shape[1])
        lookahead = core.flow.pre_lookahead_len
        offset = 0
        this_hop_base = hop
        try:
            while not cancel.is_set():
                this_hop = this_hop_base + pad if offset == 0 else this_hop_base
                need = offset + this_hop + lookahead
                with cond:
                    cond.wait_for(lambda: len(tokens) >= need or done.is_set() or cancel.is_set(), timeout=0.5)
                    available = list(tokens[:need]) if len(tokens) >= need else None
                if cancel.is_set():
                    break
                if available is not None:
                    speech = core.token2wav(token=torch.tensor(available).unsqueeze(0),
                                            prompt_token=flow_prompt, prompt_feat=prompt["prompt_speech_feat"],
                                            embedding=prompt["flow_embedding"], token_offset=offset, uuid=key,
                                            stream=True, finalize=False)
                    self.stats["flow_calls"] += 1
                    offset += this_hop
                    this_hop_base = min(core.token_max_hop_len, this_hop_base * core.stream_scale_factor)
                    yield speech.squeeze(0).float().cpu().numpy()
                    continue
                if done.is_set():
                    break
            worker.join()
            if errors:
                raise errors[0]
            if cancel.is_set():
                return
            with cond:
                final = list(tokens)
            self.stats["llm_tokens"] += len(final)
            if final:
                # Always finalise, as upstream does: it also flushes the
                # vocoder's cached tail from the last streamed hop.
                speech = core.token2wav(token=torch.tensor(final, dtype=torch.int32).unsqueeze(0),
                                        prompt_token=flow_prompt, prompt_feat=prompt["prompt_speech_feat"],
                                        embedding=prompt["flow_embedding"], token_offset=offset, uuid=key,
                                        finalize=True)
                yield speech.squeeze(0).float().cpu().numpy()
        finally:
            cancel_local = cancel.is_set()
            if cancel_local:
                worker.join(timeout=5)
            core.hift_cache_dict.pop(key, None)

    # -- contract ----------------------------------------------------------

    def synthesize(self, text: str, voice: str, cancel: threading.Event) -> Iterator[np.ndarray]:
        # The scaffold already holds self.lock around complete-text requests.
        for sentence in self.frontend.text_normalize(text, split=True, text_frontend=True):
            if cancel.is_set():
                return
            tokens, _ = self.frontend._extract_text_token(sentence)
            yield from self._speak(tokens, cancel, hold_lock=False)

    def synthesize_incremental(self, texts: "queue.Queue[Optional[str]]", voice: str,
                               cancel: threading.Event) -> Iterator[np.ndarray]:
        gate = WordGate()
        tokenizer = self.frontend.tokenizer
        allowed = self.frontend.allowed_special
        lock = self.lock
        started = [False]

        def token_stream():
            # Runs on the LM thread, which holds self.lock while computing and
            # releases it while it waits here for more text.
            while not cancel.is_set():
                try:
                    item = texts.get_nowait()
                except queue.Empty:
                    if lock.locked():
                        lock.release()
                    item = texts.get()
                    lock.acquire()
                if item is None:
                    tail = gate.drain()
                    if tail.strip():
                        for token in tokenizer.encode(tail, allowed_special=allowed):
                            yield torch.tensor([[token]], dtype=torch.int32, device=self.device)
                    return
                if item == "\x00flush":
                    continue  # nonterminal flush is not supported by the bistream LM
                if not started[0]:
                    item = item.lstrip()
                    if not item:
                        continue
                    started[0] = True
                ready = gate.push(item)
                if ready:
                    for token in tokenizer.encode(ready, allowed_special=allowed):
                        yield torch.tensor([[token]], dtype=torch.int32, device=self.device)

        yield from self._speak(token_stream(), cancel, hold_lock=True)

    def health(self) -> dict:
        return {"voice": "cosyvoice-asset-zero_shot_prompt (zh female)", "lm_prompt": self.lm_prompt, "flow_prompt_aligned": self.align_flow_prompt,
                "flow_prompt_speech_tokens": int(self.prompt["flow_prompt_speech_token"].shape[1]),
                "lm_prompt_speech_tokens": int(self.prompt["llm_prompt_speech_token"].shape[1]),
                "languages": ["en", "zh"], "fp16": self.fp16, "loaded_seconds": round(self.loaded_seconds, 1),
                "gpu_allocated_gb": round(torch.cuda.memory_allocated() / 2**30, 2),
                "gpu_reserved_gb": round(torch.cuda.memory_reserved() / 2**30, 2), **self.stats}


def warm(synth: CosyVoice3Synthesizer) -> None:
    cancel = threading.Event()
    for text in ("Hello there, this is a warm up sentence.", "你好，这是一句预热的话。"):
        with synth.lock:
            for _ in synth.synthesize(text, "default", cancel):
                pass


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
    parser.add_argument("--port", type=int, default=9120)
    parser.add_argument("--fp16", action="store_true")
    parser.add_argument("--lm-prompt", choices=["none", "zero_shot"], default="none",
                        help="none: LM sees only the system prompt (lead 5 text tokens); zero_shot: LM also "
                             "continues the prompt's transcript + speech tokens (upstream zero-shot; ~20-token lead)")
    parser.add_argument("--no-align-flow-prompt", action="store_true")
    args = parser.parse_args()
    configure_logging()
    synth = CosyVoice3Synthesizer(fp16=args.fp16, lm_prompt=args.lm_prompt,
                                  align_flow_prompt=not args.no_align_flow_prompt)
    # CosyVoice configures the root logger at DEBUG and logs every bistream
    # step ("not enough text token to decode, wait for more") at INFO.
    logging.getLogger().setLevel(logging.WARNING)
    for name in ("tts_cosyvoice", "duplexmodels"):
        logging.getLogger(name).setLevel(logging.INFO)
    warm(synth)
    log.info("ready on :%d", args.port)
    serve(synth, args.host, args.port)


if __name__ == "__main__":
    main()
