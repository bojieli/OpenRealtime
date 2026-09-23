#!/usr/bin/env python3
"""Derive a declared pre-feedback-gap fixture from a prepared pair.

Inserts silence at each branch's first variant-event source time and shifts
that branch's event times equally. Prefix PCM, feedback PCM and response-window
durations are byte- or value-identical to the source. This is an input-timing
treatment, not a repetition of the source fixture and not a capability score.
"""
import argparse
import copy
import hashlib
import json
from pathlib import Path
import wave


def earliest_onset(pair):
    return min(e['source_start'] for v in pair['variants'] for e in v['events'])


def verify(src):
    """Check that every branch shares identical PCM before its first variant
    event, and retain the result next to the prepared pair."""
    pair = json.loads((src / 'pair.json').read_text())
    # Branches may start their variant at different times (revision family);
    # only audio before the earliest variant onset must be shared.
    onset = earliest_onset(pair)
    prefix, branches = None, []
    for variant in pair['variants']:
        path = src / (variant['id'] + '.input.wav')
        with wave.open(str(path)) as w:
            rate = w.getframerate()
            pcm = w.readframes(w.getnframes())
        head = pcm[:onset * rate // 1_000_000_000 * 2]
        if prefix is None:
            prefix = head
        elif head != prefix:
            raise ValueError(f'{path}: branches do not share an identical pre-feedback prefix')
        branches.append({'variant': variant['id'], 'pcm_sha256': hashlib.sha256(pcm).hexdigest()})
    report = {'shared_prefix_samples': len(prefix) // 2, 'identical_prefix': True, 'branches': branches}
    with (src / 'prefix-verification.json').open('x') as f:
        f.write(json.dumps(report, indent=2) + '\n')
    return report


def derive(src, out, seconds, variants=None):
    gap_ns = int(round(seconds * 1e9))
    if gap_ns <= 0:
        raise ValueError('gap must be positive')
    pair = json.loads((src / 'pair.json').read_text())
    original = copy.deepcopy(pair)
    out.mkdir()
    checks, prefix = [], None
    common = earliest_onset(original)
    known = {v['id'] for v in pair['variants']}
    if variants is not None and not set(variants) <= known:
        raise ValueError(f'unknown variants: {sorted(set(variants) - known)}')
    for variant in pair['variants']:
        old = next(v for v in original['variants'] if v['id'] == variant['id'])
        onset = min(e['source_start'] for e in old['events'])
        # Unselected branches are copied unchanged: a zero-length gap.
        shift = gap_ns if variants is None or variant['id'] in variants else 0
        path = src / (variant['id'] + '.input.wav')
        with wave.open(str(path)) as w:
            if w.getnchannels() != 1 or w.getsampwidth() != 2:
                raise ValueError(f'{path}: expected mono PCM16')
            rate = w.getframerate()
            pcm = w.readframes(w.getnframes())
        cut = onset * rate // 1_000_000_000 * 2
        if cut > len(pcm):
            raise ValueError(f'{path}: feedback onset beyond recording')
        gap = bytes(shift * rate // 1_000_000_000 * 2)
        shifted = pcm[:cut] + gap + pcm[cut:]
        for event in variant['events']:
            for key in ('source_start', 'source_end', 'available_at'):
                event[key] += shift
        if variant['expect'] != old['expect']:
            raise AssertionError('expectation changed')
        shared = shifted[:common * rate // 1_000_000_000 * 2]
        if prefix is None:
            prefix = shared
        elif prefix != shared:
            raise ValueError('branches do not share an identical pre-feedback prefix')
        with wave.open(str(out / (variant['id'] + '.input.wav')), 'wb') as w:
            w.setparams((1, 2, rate, 0, 'NONE', 'not compressed'))
            w.writeframes(shifted)
        checks.append({'variant': variant['id'],
                       'source_wav_sha256': hashlib.sha256(path.read_bytes()).hexdigest(),
                       'inserted_samples': len(gap) // 2, 'insertion_sample': cut // 2, 'rate': rate,
                       'unchanged_pre_feedback_pcm': shifted[:cut] == pcm[:cut],
                       'unchanged_feedback_pcm': shifted[cut + len(gap):] == pcm[cut:],
                       'output_pcm_sha256': hashlib.sha256(shifted).hexdigest()})
    if pair['prefix'] != original['prefix']:
        raise AssertionError('prefix changed')
    (out / 'pair.json').write_text(json.dumps(pair, indent=2) + '\n')
    metadata = json.loads((src / 'recordings.json').read_text())
    scope = 'all branches' if variants is None else 'branches ' + ', '.join(sorted(variants))
    metadata['delivery_treatment'] = (f'{seconds:g} seconds additional pre-feedback silence in {scope}; '
                                      'their timestamps shifted equally; original response-window duration unchanged')
    metadata['source_prepared_pair'] = str(src)
    (out / 'recordings.json').write_text(json.dumps(metadata, indent=2) + '\n')
    audit = {'source_pair_sha256': hashlib.sha256((src / 'pair.json').read_bytes()).hexdigest(),
             'treatment': f'{pair["id"]} gap +{seconds:g} seconds', 'shift_ns': gap_ns,
             'shifted_variants': sorted(variants) if variants is not None else sorted(known),
             'shared_prefix_identical': True, 'branches': checks,
             'tool_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
             'limitations': ['Distinct input-timing treatment, not a repetition of the original fixture.',
                             'Mid-speech opportunity still requires observed assistant output before feedback.',
                             'Uniform word estimates and conservative phrase-end release unchanged.',
                             'No response-window extension relative to feedback; no capability score.']}
    (out / 'preparation-audit.json').write_text(json.dumps(audit, indent=2) + '\n')
    return audit


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=Path)
    parser.add_argument('--out', type=Path)
    parser.add_argument('--seconds', type=float, default=10)
    parser.add_argument('--verify-only', action='store_true', help='check the shared prefix; no gap')
    parser.add_argument('--variants', help='comma-separated variant IDs to shift; default all')
    args = parser.parse_args()
    if args.verify_only:
        print(json.dumps(verify(args.source)))
    elif args.out is None:
        parser.error('--out is required unless --verify-only')
    else:
        derive(args.source, args.out, args.seconds,
               args.variants.split(',') if args.variants else None)
        print(args.out)
