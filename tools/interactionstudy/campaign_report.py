#!/usr/bin/env python3
"""Aggregate a development campaign of per-pair live runs by family.

Recomputes each run's summary from its retained files and reads its causal
audit. Reports per-branch completed-playback screens, pair verdicts and
execution counts. It is descriptive: no pair-clustered uncertainty, semantic
validation or pilot claim.
"""
import argparse
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).parent))
import summarize  # noqa: E402

FAMILIES = {'sc': 'semantic correction', 'st': 'steering', 'pr': 'prosodic', 'si': 'silence',
            'ad': 'addressing', 'pa': 'proactive', 'co': 'concurrent', 'ov': 'revision'}


def pair_rows(run):
    # Always recompute from the retained files: on-disk summaries may predate
    # the current (repeat-aware) pairing rule.
    report = summarize.summarize(run)
    audit_path = run / 'causal-history-audit.json'
    audit = json.loads(audit_path.read_text()) if audit_path.exists() else None
    fixtures = {p['id']: p for p in json.loads((run / 'fixtures.json').read_text())}
    rows = []
    for branch in report['branches']:
        pair = fixtures.get(branch.get('pair_id'))
        variant_id = (branch.get('variant_id') or '').removesuffix('-nofeedback')
        variant = next((v for v in (pair or {}).get('variants', []) if v['id'] == variant_id), None)
        screen = branch.get('lexical_screen') or {}
        segments = []
        result_path = run / branch['directory'] / 'result.json'
        if result_path.exists():
            segments = (json.loads(result_path.read_text()).get('ledger') or {}).get('segments') or []
        onset = min(e['source_start'] for e in variant['events']) if variant else None
        rows.append({'pair': branch.get('pair_id'), 'branch': branch.get('variant_id'),
                     **(opportunity(segments, onset) if onset is not None else {}),
                     'status': branch.get('status'), 'requests': branch.get('requests', 0),
                     'deadline_misses': branch.get('deadline_misses', 0),
                     'completed_segments': branch.get('completed_segments'),
                     'silent_expectation': bool(variant and variant['expect'].get('silent')),
                     'feedback_withheld': branch.get('feedback_withheld'),
                     'screen_passed': screen.get('passed')})
    return {'run': str(run), 'rows': rows, 'pairs': report['paired_discrimination'],
            'audit_violations': None if audit is None else len(audit['violations']),
            'audit_requests': None if audit is None else audit['requests_checked']}


def opportunity(segments, onset):
    """What the assistant had audibly done when the variant's first event began:
    completed sentences, and whether a segment was playing across the onset."""
    heard = [s['text'] for s in segments if s.get('completed_at') is not None and s['completed_at'] <= onset]
    playing = any(s.get('first_played_at') is not None and s['first_played_at'] <= onset
                  and s.get('last_played_at') is not None and s['last_played_at'] > onset for s in segments)
    return {'feedback_onset_ns': onset, 'heard_before_onset': heard, 'speaking_at_onset': playing}


def silent_verdicts(rows):
    """Silent-expectation variants: feedback branch passes by staying quiet; the
    control shares the rule, so a passing control means silence was default."""
    out = []
    for row in rows:
        if row['silent_expectation'] and not row['feedback_withheld']:
            control = next((r for r in rows if r['branch'] == row['branch'] + '-nofeedback'), None)
            out.append({'variant_id': row['branch'], 'feedback_passed': row['screen_passed'],
                        'control_passed': control and control['screen_passed'],
                        'verdict': 'incomplete-pair' if control is None else
                        'non-discriminating' if control['screen_passed'] else
                        'feedback-only-pass' if row['screen_passed'] else 'no-feedback-pass'})
    return out


def build(root):
    runs = sorted(p.parent for p in root.glob('*/fixtures.json'))
    pairs = []
    for run in runs:
        item = pair_rows(run)
        item['silent'] = silent_verdicts(item['rows'])
        pairs.append(item)
    by_family = {}
    for item in pairs:
        for verdict in item['pairs'] + item['silent']:
            pair_id = item['rows'][0]['pair'] if item['rows'] else None
            family = FAMILIES.get((pair_id or '??')[:2], 'unknown')
            counts = by_family.setdefault(family, {})
            counts[verdict['verdict']] = counts.get(verdict['verdict'], 0) + 1
    return {'scope': 'development campaign aggregation; descriptive, no uncertainty or pilot claim',
            'root': str(root), 'runs': len(runs), 'pairs': pairs, 'verdicts_by_family': by_family,
            'incomplete_branches': sum(r['status'] != 'complete' for p in pairs for r in p['rows']),
            'total_requests': sum(r['requests'] for p in pairs for r in p['rows']),
            'total_deadline_misses': sum(r['deadline_misses'] for p in pairs for r in p['rows']),
            'branches_speaking_at_onset': sum(bool(r.get('speaking_at_onset')) for p in pairs for r in p['rows']),
            'branches_with_heard_speech_before_onset': sum(bool(r.get('heard_before_onset')) for p in pairs for r in p['rows'])}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root', type=Path)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    report = build(args.root)
    with args.out.open('x') as f:
        json.dump(report, f, indent=2)
        f.write('\n')
    print(json.dumps(report['verdicts_by_family'], indent=1))
