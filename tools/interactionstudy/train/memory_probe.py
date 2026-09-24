#!/usr/bin/env python3
"""P3 adaptation memory/time probe: BF16 LoRA on the local Qwen3-8B snapshot.

Runs a fixed number of optimizer steps on fixed-seed synthetic token IDs at the
declared context length, microbatch one, with gradient checkpointing. It
measures peak allocated/reserved CUDA memory and seconds per step. It trains on
no dialogue data and makes no claim about learning, quality or a full budget.
"""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--snapshot', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--seq-len', type=int, default=4096)
    parser.add_argument('--steps', type=int, default=8)
    parser.add_argument('--rank', type=int, default=16)
    parser.add_argument('--targets', default='q_proj,k_proj,v_proj,o_proj,gate_proj,up_proj,down_proj')
    parser.add_argument('--seed', type=int, default=1729)
    args = parser.parse_args()
    args.out.mkdir()
    record = {'scope': 'memory/time probe on synthetic token IDs; no training data, learning or quality claim',
              'invocation': {k: str(v) for k, v in vars(args).items()},
              'source_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
              'host': platform.node(), 'status': 'running'}
    path = args.out / 'probe.json'
    write = lambda: path.write_text(json.dumps(record, indent=2) + '\n')
    write()
    try:
        import peft
        import torch
        import transformers
        record['versions'] = {'torch': torch.__version__, 'transformers': transformers.__version__,
                              'peft': peft.__version__, 'cuda': torch.version.cuda}
        record['device'] = torch.cuda.get_device_name(0)
        free, total = torch.cuda.mem_get_info()
        record['cuda_free_before_bytes'], record['cuda_total_bytes'] = free, total
        torch.manual_seed(args.seed)
        began = time.monotonic()
        model = transformers.AutoModelForCausalLM.from_pretrained(
            args.snapshot, torch_dtype=torch.bfloat16, attn_implementation='sdpa').cuda()
        model.gradient_checkpointing_enable(gradient_checkpointing_kwargs={'use_reentrant': False})
        model.config.use_cache = False
        model.enable_input_require_grads()
        model = peft.get_peft_model(model, peft.LoraConfig(
            r=args.rank, lora_alpha=2 * args.rank, lora_dropout=0.0,
            target_modules=args.targets.split(','), task_type='CAUSAL_LM'))
        trainable = sum(p.numel() for p in model.parameters() if p.requires_grad)
        record['load_s'] = time.monotonic() - began
        record['trainable_parameters'] = trainable
        record['total_parameters'] = sum(p.numel() for p in model.parameters())
        record['allocated_after_load_bytes'] = torch.cuda.memory_allocated()
        optimizer = torch.optim.AdamW([p for p in model.parameters() if p.requires_grad], lr=1e-4)
        vocab = model.config.vocab_size
        generator = torch.Generator().manual_seed(args.seed)
        torch.cuda.reset_peak_memory_stats()
        steps = []
        for step in range(args.steps):
            ids = torch.randint(0, vocab, (1, args.seq_len), generator=generator).cuda()
            torch.cuda.synchronize()
            t = time.monotonic()
            loss = model(input_ids=ids, labels=ids).loss
            loss.backward()
            optimizer.step()
            optimizer.zero_grad(set_to_none=True)
            torch.cuda.synchronize()
            steps.append({'step': step, 'seconds': time.monotonic() - t, 'loss': float(loss)})
            record['steps'] = steps
            record['peak_allocated_bytes'] = torch.cuda.max_memory_allocated()
            record['peak_reserved_bytes'] = torch.cuda.max_memory_reserved()
            write()
        warm = [s['seconds'] for s in steps[1:]] or [s['seconds'] for s in steps]
        record['seconds_per_step_excluding_first'] = sum(warm) / len(warm)
        record['tokens_per_second_excluding_first'] = args.seq_len / record['seconds_per_step_excluding_first']
        record['status'] = 'complete'
    except BaseException as exc:
        record.update(status='failed', error=repr(exc))
        # On a shared GPU an out-of-memory failure is ambiguous: record this
        # process's own peak and every process's usage at the moment it failed.
        try:
            import torch
            record['peak_allocated_bytes_at_failure'] = torch.cuda.max_memory_allocated()
            record['peak_reserved_bytes_at_failure'] = torch.cuda.max_memory_reserved()
        except Exception:
            pass
        try:
            import subprocess
            record['gpu_processes_at_failure'] = subprocess.run(
                ['nvidia-smi', '--query-compute-apps=pid,used_memory', '--format=csv,noheader,nounits'],
                capture_output=True, text=True, timeout=10).stdout.split('\n')
        except Exception:
            pass
        raise
    finally:
        write()


if __name__ == '__main__':
    main()
