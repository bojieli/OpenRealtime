#!/usr/bin/env python3
"""Probe released DuplexCascade weights with the upstream micro-turn template.

Text-only model feasibility, not ASR/TTS or live agent acceptance. Requires
local checkpoints and the pinned upstream source; records load compatibility
and each generated micro-turn. Input JSON is a list of incremental text chunks;
empty strings stand for silence at the 500 ms model clock.
"""
import argparse
import json
from pathlib import Path
import sys
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
    sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'sidecars'))
    from duplexcascade_model import DuplexCascadeModel
    backend = DuplexCascadeModel(a.source, a.snapshot, a.base, slow_tokenizer=a.slow_tokenizer)
    torch, tokenizer = backend.torch, backend.tokenizer
    session = backend.session()
    report = {**backend.metadata, 'scope': 'text-only micro-turn probe, no paced audio or playback', 'turns': []}
    for index, chunk in enumerate(json.loads(a.chunks.read_text())):
        torch.cuda.synchronize()
        started = time.perf_counter()
        generated = session.step(chunk)
        torch.cuda.synchronize()
        row = {'tick': index, 'input': chunk, 'generated': tokenizer.decode(generated),
               'compute_ms': (time.perf_counter()-started)*1000, 'events': list(session.events(generated))}
        report['turns'].append(row)
        print(json.dumps(row), flush=True)
        report['gpu_peak_gib'] = torch.cuda.max_memory_allocated()/2**30
        a.out.parent.mkdir(parents=True, exist_ok=True)
        a.out.write_text(json.dumps(report, indent=2)+'\n')


if __name__ == '__main__':
    main()
