#!/usr/bin/env python3
"""Run a development campaign: every pair under every treatment, one session at a time.

Treatments run interleaved within each pair, alternating their order from pair
to pair, so host drift cannot masquerade as a treatment effect. The suite
manifest pins every prepared fixture by hash; each run's outcome, including
failures, is appended to campaign.jsonl with the host load at its start.
"""
import argparse
import glob
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time


def native_active():
    """True while a native study run is executing. Matches the interpreter's
    argv exactly, so shells that merely mention the script never match."""
    for cmdline in glob.glob('/proc/[0-9]*/cmdline'):
        try:
            argv = Path(cmdline).read_bytes().split(b'\0')
        except OSError:
            continue
        if len(argv) > 1 and argv[0].endswith(b'/python') and argv[1].endswith(b'interactionstudy/native_live.py'):
            return True
    return False


def host_load():
    load = os.getloadavg()[0]
    try:
        gpu = subprocess.run(['nvidia-smi', '--query-gpu=utilization.gpu,memory.used',
                              '--format=csv,noheader,nounits'], capture_output=True, text=True, timeout=10).stdout.split(',')
        return {'load1': load, 'gpu_util_percent': int(gpu[0]), 'gpu_memory_used_mib': int(gpu[1])}
    except Exception as exc:  # the load record must never stop the campaign
        return {'load1': load, 'gpu_error': repr(exc)}


def suite(pairs):
    out = []
    for pair_id, prepared in pairs:
        prepared = Path(prepared)
        out.append({'pair': pair_id, 'prepared': str(prepared),
                    'pair_sha256': hashlib.sha256((prepared / 'pair.json').read_bytes()).hexdigest(),
                    'wav_sha256': {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                                   for p in sorted(prepared.glob('*.input.wav'))}})
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True, help='new campaign directory')
    parser.add_argument('--pairs', required=True, help='comma-separated pair=prepared-dir entries')
    parser.add_argument('--treatments', default='v4',
                        help='comma-separated pending-affordance versions, optionally cell-qualified (A1:v4)')
    parser.add_argument('--repeats', type=int, default=1, help="runner repeats per run (the runner's seeded -repeats)")
    parser.add_argument('--cell', default='A2')
    parser.add_argument('--binary', default='.runtime/interactionstudy-bin/interactionstudy')
    parser.add_argument('--timeout', type=int, default=1200)
    args = parser.parse_args()
    args.out.mkdir()
    pairs = [entry.split('=', 1) for entry in args.pairs.split(',')]
    treatments = args.treatments.split(',')
    manifest = {'scope': 'development campaign, not the frozen pilot', 'cell': args.cell, 'repeats': args.repeats,
                'treatments': treatments, 'order': 'interleaved per pair, alternating',
                'binary_sha256': hashlib.sha256(Path(args.binary).read_bytes()).hexdigest(),
                'source_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                'pairs': suite(pairs)}
    (args.out / 'suite.json').write_text(json.dumps(manifest, indent=2) + '\n')
    log = (args.out / 'campaign.jsonl').open('x')
    for index, (pair_id, prepared) in enumerate(pairs):
        order = treatments if index % 2 == 0 else list(reversed(treatments))
        for treatment in order:
            cell, _, affordance = treatment.rpartition(':')
            cell = cell or args.cell
            while native_active():
                time.sleep(30)
            run = args.out / f'{cell}-{affordance}-{pair_id}'
            record = {'pair': pair_id, 'treatment': treatment, 'cell': cell, 'out': str(run), 'began': time.time(), 'host': host_load()}
            with open(str(run) + '.log', 'w') as output:
                try:
                    code = subprocess.run([args.binary, '-live', '-cells', cell, '-pairs', pair_id,
                                           '-repeats', str(args.repeats),
                                           '-prepared-pair', str(Path(prepared) / 'pair.json'),
                                           '-omit-policy-history', '-pending-affordance', affordance,
                                           '-out', str(run)], stdout=output, stderr=subprocess.STDOUT,
                                          timeout=args.timeout).returncode
                except subprocess.TimeoutExpired:
                    code = 'timeout'
            record.update(exit=code, ended=time.time(), native_active_at_end=native_active())
            log.write(json.dumps(record) + '\n')
            log.flush()
    log.write(json.dumps({'campaign': 'finished'}) + '\n')
    log.close()


if __name__ == '__main__':
    main()
