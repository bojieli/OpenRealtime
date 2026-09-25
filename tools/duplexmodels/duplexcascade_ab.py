"""Compare the faithful and bounded DuplexCascade cells of one A/B.

Scores come from each cell's e2e run directory; clock health comes from the
sidecar traces written during that run's time window. Host load differs
between cells on a shared machine, so the deadline-miss fraction is reported
beside every score: compare scores only where the clocks were comparably
healthy.

Usage::

    python tools/duplexmodels/duplexcascade_ab.py RUN_DIR:TRACE_DIR [RUN_DIR:TRACE_DIR ...]
"""
import json
import statistics
import sys
from datetime import datetime
from pathlib import Path

CATEGORIES = ('user_interruption', 'user_backchannel', 'background_speech', 'talking_to_other')


def _epoch(stamp):
    return datetime.fromisoformat(stamp).timestamp()


def scores(run):
    out = {}
    for category in CATEGORIES:
        path = run / f'fdb-{category}.json'
        if not path.is_file():
            continue
        tasks = json.loads(path.read_text())['tasks']
        applicable = [t for t in tasks if (t.get('notes') or {}).get('applicable') == 'true']
        out[category] = {'passed': sum(bool(t['passed']) for t in applicable), 'applicable': len(applicable),
                         'recordings': len(tasks), 'errors': sum(bool(t.get('error')) for t in tasks),
                         'timeouts': sum('timeout' in (t.get('error') or '') for t in tasks)}
    path = run / 'fdbench.json'
    if path.is_file():
        tasks = json.loads(path.read_text())['tasks']
        total = lambda key: sum(t.get('metrics', {}).get(key, 0) for t in tasks)
        out['fdbench'] = {'passed': sum(bool(t['passed']) for t in tasks), 'conversations': len(tasks),
                          'errors': sum(bool(t.get('error')) for t in tasks),
                          'answered': total('answered'), 'turns': total('turns'),
                          'missed': total('missed_turns'), 'premature': total('premature_turns'),
                          'overrun': total('overrun_turns')}
    return out


def clock(traces, start, end):
    ticks = []
    for path in sorted(traces.glob('session-*.jsonl')):
        if not start <= int(path.stem.split('-')[1]) / 1e9 <= end:
            continue
        ticks += [json.loads(line) for line in path.open()]
    ran = [t for t in ticks if 'compute_and_dispatch_ms' in t]
    queued = [t['queued_audio_s'] for t in ticks if 'queued_audio_s' in t]
    return {'ticks': len(ticks), 'generated': len(ran), 'skipped': len(ticks) - len(ran),
            'deadline_missed': sum(t['deadline_missed'] for t in ran),
            'missed_fraction': round(sum(t['deadline_missed'] for t in ran) / len(ran), 3) if ran else None,
            'compute_p50_ms': round(statistics.median(t['compute_and_dispatch_ms'] for t in ran)) if ran else None,
            'queued_audio_p50_s': round(statistics.median(queued), 2) if queued else None,
            'queued_audio_max_s': round(max(queued), 2) if queued else None,
            'time_at_full_queue': round(sum(q >= 29.9 for q in queued) / len(queued), 3) if queued else None}


def main():
    report = {}
    for argument in sys.argv[1:]:
        run, traces = (Path(part) for part in argument.split(':'))
        meta = json.loads((run / 'run.json').read_text())
        finished = json.loads((run / 'finished.json').read_text()) if (run / 'finished.json').is_file() else {}
        start = _epoch(meta['started'])
        end = _epoch(finished['finished']) if finished else float('inf')
        report[meta['profile']] = {'run': run.name, 'exit_code': finished.get('exit_code'),
                                   'errors': finished.get('errors'),
                                   'load_average': [meta['load_average'], finished.get('load_average_end')],
                                   'scores': scores(run), 'clock': clock(traces, start, end)}
    print(json.dumps(report, indent=1))


if __name__ == '__main__':
    main()
