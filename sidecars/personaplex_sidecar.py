#!/usr/bin/env python3
"""PersonaPlex with upstream voice/role conditioning and paced Moshi transport.

Requires NVIDIA/personaplex's moshi package on PYTHONPATH. The inherited
transport uses an explicit RMS/hangover floor and mute-until-quiet interruption
policy; these are adapter decisions, not model turn-boundary tokens. Session
instructions condition the initial prompt; mid-session text injection is not
supported. Checkpoints and voice prompts must already be local.
"""
from __future__ import annotations

import argparse
from pathlib import Path
import threading
import time

import numpy as np
from moshi_sidecar import MoshiSidecar, LinearResampler, MODEL_RATE, FRAME_SAMPLES, _listen
from openrealtime_sidecar import log, run

REPOSITORY = "nvidia/personaplex-7b-v1"


class PersonaPlexModel:
    def __init__(self, snapshot: Path, voice: Path, device: str, seed: int):
        import torch
        import sentencepiece
        from moshi.models import loaders, LMGen
        from moshi.offline import seed_all, warmup

        self.torch, self.device = torch, device
        seed_all(seed)
        began = time.perf_counter()
        self.mimi = loaders.get_mimi(str(snapshot / loaders.MIMI_NAME), device)
        self.other_mimi = loaders.get_mimi(str(snapshot / loaders.MIMI_NAME), device)
        self.lm = loaders.get_moshi_lm(str(snapshot / loaders.MOSHI_NAME), device=device)
        self.lm.eval()
        self.tokenizer = sentencepiece.SentencePieceProcessor(str(snapshot / loaders.TEXT_TOKENIZER_NAME))
        self.generator = LMGen(self.lm, audio_silence_frame_cnt=int(.5*self.mimi.frame_rate),
                               sample_rate=self.mimi.sample_rate, device=device,
                               frame_rate=self.mimi.frame_rate, save_voice_prompt_embeddings=False,
                               use_sampling=True, temp=.8, temp_text=.7, top_k=250, top_k_text=25)
        if self.mimi.sample_rate != MODEL_RATE or int(MODEL_RATE/self.mimi.frame_rate) != FRAME_SAMPLES:
            raise ValueError("unexpected PersonaPlex frame rate")
        for module in (self.mimi, self.other_mimi, self.generator):
            module.streaming_forever(1)
        with torch.no_grad():
            warmup(self.mimi, self.other_mimi, self.generator, device, FRAME_SAMPLES)
            self.generator.load_voice_prompt_embeddings(str(voice))
        self.lock = threading.Lock()
        log(f"PersonaPlex loaded and warmed in {time.perf_counter()-began:.1f}s")

    def reset(self, instructions: str):
        from moshi.offline import wrap_with_system_tags
        with self.torch.no_grad():
            self.generator.text_prompt_tokens = self.tokenizer.encode(wrap_with_system_tags(instructions)) if instructions else None
            for module in (self.mimi, self.other_mimi, self.generator):
                module.reset_streaming()
            self.generator.step_system_prompts(self.mimi)
            self.mimi.reset_streaming()

    def step(self, samples):
        with self.torch.no_grad():
            if samples is None:
                samples = np.zeros(FRAME_SAMPLES, dtype=np.float32)
            chunk = self.torch.from_numpy(samples).to(self.device)[None, None]
            codes = self.mimi.encode(chunk)
            tokens = self.generator.step(codes)
            if tokens is None:
                return None, None
            pcm = self.mimi.decode(tokens[:, 1:9])[0, 0].float().cpu().numpy()
            self.other_mimi.decode(tokens[:, 1:9])
            token = int(tokens[0, 0, 0])
            text = None if token in (0, 3) else self.tokenizer.id_to_piece(token).replace("▁", " ")
            return text, pcm


class PersonaPlexSidecar(MoshiSidecar):
    def configure(self, hello):
        self.model_name = REPOSITORY + ("/mock" if self.mock else "")
        if self.input_rate != MODEL_RATE:
            self._resampler = LinearResampler(self.input_rate, MODEL_RATE)
        if self.mock:
            self._stream_thread = threading.Thread(target=self._mock_stream, daemon=True)
        else:
            model = self._shared
            if not model.lock.acquire(timeout=20):
                raise RuntimeError("PersonaPlex is serving another session")
            self._model, self._holds_model = model, True
            model.reset(self.instructions)
            self._stream_thread = threading.Thread(target=self._stream, daemon=True)
        self._stream_thread.start()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", type=Path)
    parser.add_argument("--voice", type=Path)
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--seed", type=int, default=42424242)
    parser.add_argument("--listen", default="")
    parser.add_argument("--stats-file", default="")
    parser.add_argument("--mock", action="store_true")
    args = parser.parse_args()
    if not args.mock and (args.snapshot is None or args.voice is None):
        parser.error("--snapshot and --voice are required for model inference")
    shared = None if args.mock else PersonaPlexModel(args.snapshot, args.voice, args.device, args.seed)
    kwargs = dict(repository=REPOSITORY, mock=args.mock, device=args.device, seed=args.seed,
                  shared=shared, stats_file=args.stats_file)
    if args.listen:
        _listen(args.listen, lambda reader, writer: PersonaPlexSidecar(reader, writer, **kwargs))
    else:
        run(PersonaPlexSidecar, **kwargs)


if __name__ == "__main__":
    main()
