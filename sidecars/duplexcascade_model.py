"""Released DuplexCascade checkpoint loader and isolated micro-turn histories.

This backend keeps the upstream full-history generation template. It does not
apply an external floor policy or turn ordinary chat output into control acts.
"""
import importlib.util
import json
from pathlib import Path
import subprocess
import threading
import time


class DuplexCascadeModel:
    def __init__(self, source: Path, snapshot: Path, base: Path, *, slow_tokenizer=False):
        import torch
        from transformers import AutoTokenizer
        from safetensors.torch import load_file
        spec = importlib.util.spec_from_file_location('released_duplexcascade', source / 'model.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        cfg = json.loads((snapshot / 'train_cfg.json').read_text())['model']
        tokenizer = AutoTokenizer.from_pretrained(snapshot / 'tokenizer', local_files_only=True, use_fast=not slow_tokenizer)
        started = time.perf_counter()
        model = module.Model(tokenizer, model_name=str(base), torch_dtype=torch.bfloat16,
                             attn_implementation='sdpa')
        model.enable_lora_adapter(mode=cfg['use_lora'], r=cfg['lora_r'], alpha=cfg['lora_alpha'],
                                  dropout=cfg['lora_dropout'], bias=cfg['lora_bias'])
        # Unlike upstream strict=False, do not silently accept missing checkpoint weights.
        state = load_file(str(snapshot / 'model_state.safetensors'))
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

        self.model, self.tokenizer, self.torch = model, tokenizer, torch
        self.lock = threading.Lock()
        self.metadata = {
            'model_revision': snapshot.name, 'base_revision': base.name,
            'source_revision': subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip(),
            'dtype': 'bfloat16', 'attention': 'sdpa', 'tokenizer_class': type(tokenizer).__name__,
            'strict_checkpoint_load': True, 'renamed_checkpoint_keys': renamed,
            'load_seconds': time.perf_counter()-started,
        }

    def session(self):
        return DuplexCascadeSession(self)


class DuplexCascadeSession:
    def __init__(self, backend):
        self.backend = backend
        bos = backend.tokenizer.bos_token_id
        self.history = [] if bos is None else [bos]

    def step(self, chunk: str, max_new_tokens=64):
        """Generate one micro-turn, preserving special tokens and ordered text.

        The caller schedules 500 ms ticks and admits only newly available words.
        Empty chunks use the released no-voice token. History is committed only
        after successful generation, so exceptions do not leave a partial turn.
        """
        b = self.backend
        t = b.tokenizer
        prompt = '<|im_start|>user\n' + (chunk or '<|no voice|>') + '<|im_end|>\n<|im_start|>assistant\n'
        with b.lock, b.torch.inference_mode():
            history = self.history + t.encode(prompt, add_special_tokens=False)
            inputs = b.torch.tensor([history], device='cuda')
            eos = t.convert_tokens_to_ids('<|im_end|>')
            output = b.model.generate(input_ids=inputs, attention_mask=b.torch.ones_like(inputs),
                                      max_new_tokens=max_new_tokens, do_sample=False,
                                      eos_token_id=eos, pad_token_id=eos)
            generated = output[0, len(history):].tolist()
            self.history = history + generated
            return generated

    def events(self, tokens):
        """Split generation at exact control-token IDs without losing word pieces."""
        tokenizer = self.backend.tokenizer
        controls = {tokenizer.convert_tokens_to_ids(text): text for text in (
            '<|user is talking|>', '<|user finish talking|>', '<|user is thinking|>',
            '<|user interruption|>', '<|user backchannel|>', '<|no voice|>',
            '<|im_end|>')}
        text = []
        for token in tokens:
            if token in controls:
                if text:
                    yield ('text', tokenizer.decode(text))
                    text = []
                yield ('control', controls[token])
            else:
                text.append(token)
        if text:
            yield ('text', tokenizer.decode(text))
