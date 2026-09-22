#!/usr/bin/env python3
"""Probe released DuplexCascade weights with the upstream micro-turn template.

Text-only model feasibility, not ASR/TTS or live agent acceptance. Requires
local checkpoints and the pinned upstream source; records load compatibility
and each generated micro-turn. Input JSON is a list of incremental text chunks;
empty strings stand for silence at the 500 ms model clock.
"""
import argparse
import importlib.util
import json
from pathlib import Path
import subprocess
import time


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source', type=Path, required=True)
    p.add_argument('--snapshot', type=Path, required=True)
    p.add_argument('--base', type=Path, required=True)
    p.add_argument('--chunks', type=Path, required=True)
    p.add_argument('--out', type=Path, required=True)
    p.add_argument('--slow-tokenizer', action='store_true', help='use released vocab/merges when the fast tokenizer format is newer than the runtime')
    a = p.parse_args()
    import torch
    from transformers import AutoTokenizer
    from safetensors.torch import load_file
    spec = importlib.util.spec_from_file_location('released_duplexcascade', a.source / 'model.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    cfg = json.loads((a.snapshot / 'train_cfg.json').read_text())['model']
    tokenizer = AutoTokenizer.from_pretrained(a.snapshot / 'tokenizer', local_files_only=True, use_fast=not a.slow_tokenizer)
    started = time.perf_counter()
    model = module.Model(tokenizer, model_name=str(a.base), torch_dtype=torch.bfloat16,
                         attn_implementation='sdpa')
    model.enable_lora_adapter(mode=cfg['use_lora'], r=cfg['lora_r'], alpha=cfg['lora_alpha'],
                              dropout=cfg['lora_dropout'], bias=cfg['lora_bias'])
    # Unlike upstream strict=False, do not silently accept missing checkpoint weights.
    state = load_file(str(a.snapshot / 'model_state.safetensors'))
    expected = model.state_dict()
    renamed = {}
    for key in list(state):
        if key in expected:
            continue
        prefix, parameter = key.rsplit('.', 1)
        target = prefix + '.base_layer.' + parameter
        if (prefix.endswith(('.q_proj', '.v_proj')) and target in expected
                and state[key].shape == expected[target].shape and target not in state):
            state[target] = state.pop(key)
            renamed[key] = target
    model.load_state_dict(state, strict=True)
    del state, expected
    model.to('cuda').eval()
    report = {'model_revision': a.snapshot.name, 'base_revision': a.base.name,
              'source_revision': subprocess.check_output(['git', '-C', str(a.source), 'rev-parse', 'HEAD'], text=True).strip(),
              'dtype': 'bfloat16', 'attention': 'sdpa', 'tokenizer_class': type(tokenizer).__name__, 'strict_checkpoint_load': True, 'renamed_checkpoint_keys': renamed,
              'scope': 'text-only micro-turn probe, no paced audio or playback',
              'load_seconds': time.perf_counter()-started, 'turns': []}
    history = [] if tokenizer.bos_token_id is None else [tokenizer.bos_token_id]
    for index, chunk in enumerate(json.loads(a.chunks.read_text())):
        history.extend(tokenizer.encode('<|im_start|>user\n' + (chunk or '<|no voice|>') +
                                        '<|im_end|>\n<|im_start|>assistant\n', add_special_tokens=False))
        inputs = torch.tensor([history], device='cuda')
        torch.cuda.synchronize()
        started = time.perf_counter()
        with torch.inference_mode():
            output = model.generate(input_ids=inputs, attention_mask=torch.ones_like(inputs),
                                    max_new_tokens=64, do_sample=False,
                                    eos_token_id=tokenizer.convert_tokens_to_ids('<|im_end|>'),
                                    pad_token_id=tokenizer.convert_tokens_to_ids('<|im_end|>'))
        torch.cuda.synchronize()
        generated = output[0, len(history):].tolist()
        row = {'tick': index, 'input': chunk, 'generated': tokenizer.decode(generated),
               'compute_ms': (time.perf_counter()-started)*1000}
        report['turns'].append(row)
        history.extend(generated)
        print(json.dumps(row), flush=True)
        report['gpu_peak_gib'] = torch.cuda.max_memory_allocated()/2**30
        a.out.parent.mkdir(parents=True, exist_ok=True)
        a.out.write_text(json.dumps(report, indent=2)+'\n')


if __name__ == '__main__':
    main()
