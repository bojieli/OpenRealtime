"""Correlate each audio_stalled turn end with upstream round activity and SSE delivery."""
import json, os, re, sys
from pathlib import Path
here = Path(sys.argv[1] if len(sys.argv) > 1 else '.')
ev = [json.loads(l) for l in open(here / 'control.jsonl')]
paths = {e['session']: e.get('stage_timing_log_path') for e in ev if e['event'] == 'session_start'}
def rounds(path):
    if not path: return []
    local = here / 'stage_timing' / os.path.basename(path)
    if not local.exists(): return []
    out = []
    for block in re.split(r'(?=\[round \d+\])', local.read_text()):
        m = re.match(r'\[round (\d+)\]', block)
        if not m: continue
        fields = dict(re.findall(r'^(\w+)=(\S+)', block, re.M))
        fields['round'] = int(m.group(1)); out.append(fields)
    return out
num = lambda v: None if v in (None, 'None') else float(v)
labels = {}
for e in ev:
    if e['event'] == 'session_start': labels[e['session']] = e['t_ms']
order = ['fdb-ui1', 'fdb-ui2', 'fdb-ui3', 'fdb-ui4', 'fdb-ui5', 'fdb-ui6', 'fdb-ui7', 'fdb-ui8',
         'probe-ui1-continuous', 'probe-ui1-finite', 'probe-ui4-continuous', 'probe-ui4-finite', 'probe-long-continuous']
starts = [x['session'] for x in ev if x['event'] == 'session_start']
modes = dict(zip(starts, order)) if len(starts) == len(order) else {}
report = []
for e in ev:
    if not (e['event'] == 'turn_end' and e.get('reason') == 'audio_stalled'): continue
    s, t = e['session'], e['t_ms']
    same = [x for x in ev if x['session'] == s]
    deliveries = [x for x in same if x['event'] == 'audio_delivery' and x['t_ms'] <= t]
    last = deliveries[-1] if deliveries else None
    states = [x for x in same if x['event'] == 'model_state' and x['t_ms'] <= t]
    rs = rounds(paths.get(s))
    window = [r for r in rs if num(r.get('round_started_at_epoch_ms')) and t - 8000 <= num(r['round_started_at_epoch_ms']) <= t]
    after = [r for r in rs if num(r.get('round_started_at_epoch_ms')) and num(r['round_started_at_epoch_ms']) > t]
    report.append({
        'session': s, 'stall_ms': t, 'session_age_s': round((t - labels[s]) / 1000, 1),
        'model_state_at_stall': states[-1].get('to_state') if states else None,
        'mode': modes.get(s),
        'last_delivery_before_ms': t - last['t_ms'] if last else None,
        'last_delivery_round': last.get('round_id') if last else None,
        'deliveries_in_prior_8s': sum(1 for x in deliveries if x['t_ms'] >= t - 8000),
        'rounds_started_prior_8s': len(window),
        'rounds_with_pcm_out_prior_8s': sum(num(r.get('first_pcm_out_sec')) is not None for r in window),
        'rounds_with_t2w_submit_prior_8s': sum(num(r.get('first_t2w_submit_sec')) is not None for r in window),
        'input_seconds_prior_8s': sum(int(r.get('input_samples', 0)) for r in window) / 16000,
        'mean_round_s_prior_8s': round(sum(num(r.get('total_round_sec')) or 0 for r in window) / len(window), 3) if window else None,
        'rounds_with_speaking_token_prior_8s': sum(num(r.get('first_speaking_token_sec')) is not None for r in window),
        'input_samples_prior_8s': sum(int(r.get('input_samples', 0)) for r in window),
        'rounds_after_stall': len(after), 'stage_file': os.path.basename(paths.get(s) or ''),
    })
json.dump(report, open(here / 'stall-correlation.json', 'w'), indent=1)
for r in report: print(r)
