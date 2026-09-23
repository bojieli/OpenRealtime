#!/usr/bin/env python3
"""Offline fixed-state probe of identical-text revision after speak; not a live score.

Selects, from retained playback-state v2 live runs, every request made while
the segment started by the immediately preceding speak/continue was still
opening. Replays each exact request under truthful state renderings that differ
only in how the opening segment is described.
"""
import argparse
import hashlib
import json
from pathlib import Path
import time
import urllib.request

OPENING = 'synthesis context opening; no audio played; requested text: '
SILENT = 'You are SILENT.'
CONDITIONS = ('original', 'age', 'starting', 'age+starting')


def render(content, condition, age_s):
    if content.count(OPENING) != 1 or content.count(SILENT) != 1:
        raise ValueError('unexpected opening-state rendering')
    if 'age' in condition:
        content = content.replace(OPENING, OPENING.replace(
            'no audio played;', f'requested {age_s:.1f} s ago; no audio played yet;'))
    if 'starting' in condition:
        content = content.replace(SILENT, 'You are SILENT; your pending segment is starting '
                                  'and will play unless you revise or yield it.')
    return content


def select(runs):
    for root in runs:
        for path in sorted(root.glob('*/actions.jsonl')):
            rows = [json.loads(line) for line in path.read_text().splitlines()]
            for index in range(1, len(rows)):
                prev, row = rows[index - 1], rows[index]
                if prev['decision'].get('action', {}).get('act') not in ('speak', 'continue'):
                    continue
                if prev['execution_status'] != 'synthesis-started':
                    continue
                if OPENING not in row['decision']['request']['messages'][1]['content']:
                    continue
                started = prev['model_admission_ns'] + prev['decision']['duration_ns']
                yield path, index, row, max(0, row['model_admission_ns'] - started) / 1e9


def run(runs, out, url):
    out.mkdir()
    source = Path(__file__)
    (out / 'probe-source.py').write_bytes(source.read_bytes())
    selected = list(select(runs))
    (out / 'manifest.json').write_text(json.dumps({
        'scope': 'offline fixed-state intervention; no live adaptation or latency claim',
        'selection': 'first request after a speak/continue whose segment is still opening',
        'conditions': CONDITIONS, 'source_runs': [str(r) for r in runs],
        'source_sha256': hashlib.sha256(source.read_bytes()).hexdigest(),
        'states': len(selected), 'capability_score': None}, indent=2) + '\n')
    tally = {c: {} for c in CONDITIONS}
    with (out / 'responses.jsonl').open('x') as log:
        for path, index, row, age in selected:
            pending = row['self_at_admission']['Pending']
            for condition in CONDITIONS:
                body = json.loads(json.dumps(row['decision']['request']))
                body['messages'][1]['content'] = render(body['messages'][1]['content'], condition, age)
                record = {'source': str(path), 'row_index': index, 'turn_id': row['turn_id'],
                          'condition': condition, 'segment_age_s': age,
                          'source_sha256': hashlib.sha256(path.read_bytes()).hexdigest(), 'request': body}
                began = time.monotonic()
                try:
                    req = urllib.request.Request(url, json.dumps(body).encode(), {'Content-Type': 'application/json'})
                    with urllib.request.urlopen(req, timeout=60) as response:
                        record['response'] = json.load(response)
                    action = json.loads(record['response']['choices'][0]['message']['content'])
                    same = action['act'] == 'revise' and json.dumps(action['text']) in pending
                    outcome = 'revise-identical' if same else action['act']
                except Exception as exc:
                    record['error'] = str(exc)
                    outcome = 'error'
                record['outcome'] = outcome
                record['wall_s'] = time.monotonic() - began
                log.write(json.dumps(record) + '\n')
                log.flush()
                tally[condition][outcome] = tally[condition].get(outcome, 0) + 1
                print(condition, path.parent.name, index, outcome, flush=True)
    (out / 'tally.json').write_text(json.dumps(tally, indent=2) + '\n')
    print(json.dumps(tally, indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('runs', nargs='+', type=Path)
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--url', default='http://127.0.0.1:9100/v1/chat/completions')
    args = parser.parse_args()
    run(args.runs, args.out, args.url)
