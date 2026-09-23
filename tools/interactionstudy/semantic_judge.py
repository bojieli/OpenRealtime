#!/usr/bin/env python3
"""Local semantic judge over heard assistant speech in each scoring window.

For every non-silent variant, the feedback branch and its muted control are
judged against the same goal, using independent ASR of the window crop (what
was audible), never generated text. A pair counts only when the feedback
branch meets the goal and the control does not. The judge is a local LLM, and
its identity is recorded; if it is the policy backbone, that is a declared
limitation. It is a development scorer, not human rating or a frozen metric.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.request

HERE = Path(__file__).resolve().parent
SYSTEM = ("You evaluate one assistant turn in a live spoken conversation. You receive what the user "
          "said, the goal the assistant's speech should meet in a fixed time window, and a speech "
          "recognition transcript of everything the assistant audibly said in that window. Recognition "
          "may misspell words. Judge only the transcript: an acknowledgement, a promise, a question "
          "or a greeting does not meet a goal that asks for content. Do not credit content the goal "
          "forbids. Reply with JSON only: {\"meets\": true|false, \"quote\": \"words from the transcript "
          "that decide it, or empty\"}.")


def words(events):
    return ' '.join(e['text'] for e in events if e.get('text'))


def goal(pair, variant):
    expect = variant['expect']
    lines = [f"User, before the window: {words(pair['prefix'])}",
             f"User, just before the window opens: {words(variant['events'])}",
             f"Assistant instructions: {pair['instructions']}",
             f"Goal for the assistant's speech in the window: {variant['label']}."]
    if expect.get('require_any_of'):
        groups = '; '.join(' / '.join(g) for g in expect['require_any_of'])
        lines.append(f"Content that would show it (ideas, not exact words): {groups}.")
    if expect.get('forbid'):
        lines.append(f"It must not say: {', '.join(expect['forbid'])}.")
    return '\n'.join(lines)


def ask(url, model, goal_text, transcript):
    body = {'model': model, 'temperature': 0, 'seed': 1729, 'max_tokens': 3000,
            'chat_template_kwargs': {'enable_thinking': True},
            'messages': [{'role': 'system', 'content': SYSTEM},
                         {'role': 'user', 'content': goal_text + '\n\nAssistant transcript in the window: '
                          + json.dumps(transcript or '')}]}
    request = urllib.request.Request(url, json.dumps(body).encode(), {'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=300) as response:
        content = json.load(response)['choices'][0]['message']['content'] or ''
    found = re.findall(r'\{[^{}]*"meets"[^{}]*\}', content)
    if not found:
        raise ValueError('judge returned no verdict: ' + content[-200:])
    verdict = json.loads(found[-1])
    if not isinstance(verdict.get('meets'), bool):
        raise ValueError('judge verdict lacks boolean meets')
    return verdict


def transcript(wav, endpoint):
    out = wav.with_suffix('.asr.json')
    if not out.exists():
        subprocess.run([sys.executable, str(HERE / 'transcribe_recording.py'), str(wav),
                        '--endpoint', endpoint, '--out', str(out)], check=True, capture_output=True)
    data = json.loads(out.read_text())
    if data.get('error'):
        raise RuntimeError(f'{wav}: recognition failed: {data["error"]}')
    return (data.get('final') or {}).get('text', '')


def pair_verdict(feedback_meets, control_meets):
    if control_meets:
        return 'non-discriminating'
    return 'feedback-only-pass' if feedback_meets else 'no-feedback-pass'


def judge_run(run, url, model, asr):
    fixtures = {p['id']: p for p in json.loads((run / 'fixtures.json').read_text())}
    manifest = json.loads((run / 'windows' / 'manifest.json').read_text())
    by_branch = {(m['variant_id'], m['file'].rsplit('-', 1)[-1].removesuffix('.wav')): m for m in manifest}
    rows = []
    for (variant_id, repeat), m in sorted(by_branch.items()):
        if variant_id.endswith('-nofeedback'):
            continue
        pair = fixtures[m['pair_id']]
        variant = next(v for v in pair['variants'] if v['id'] == variant_id)
        if variant['expect'].get('silent'):
            continue
        control = by_branch.get((variant_id + '-nofeedback', repeat))
        row = {'run': str(run), 'pair_id': pair['id'], 'variant_id': variant_id, 'repeat': repeat,
               'window_ns': [m['start_ns'], m['end_ns']]}
        if control is None:
            rows.append({**row, 'verdict': 'incomplete-pair'})
            continue
        text = goal(pair, variant)
        for name, item in (('feedback', m), ('control', control)):
            heard = transcript(run / 'windows' / item['file'], asr)
            row[name] = {'file': item['file'], 'transcript': heard, **ask(url, model, text, heard)}
        row['goal'] = text
        row['verdict'] = pair_verdict(row['feedback']['meets'], row['control']['meets'])
        rows.append(row)
    return rows


# Known-answer checks: the goal and heard transcripts of confirmed cases, plus
# negatives the judge must reject. A judge that fails these is not used.
VALIDATION = [
    ('sc-02', 'corrected', 'The butter in a pan over medium first melt the olive oil in a pan over medium heat', True),
    ('sc-02', 'corrected', '', False),
    ('sc-02', 'corrected', 'Okay, got it.', False),
    ('sc-02', 'corrected', "Butter in a pan over medium heat. Next add finely chopped onions and saute until they're translucent", False),
    ('sc-02', 'corrected', 'Sure, I will change that.', False),
    ('st-02', 'deepen', 'Certainly the feeding schedule for your sourdough starter is crucial. Feed it every twelve hours: discard half, then add fifty grams each of flour and water.', True),
    ('st-02', 'deepen', 'Now let us move on to shaping the loaf and preheating the oven.', False),
    ('st-02', 'skip', 'To bake the loaf, preheat your oven with a dutch oven inside, then score the dough and bake it.', True),
    ('st-02', 'skip', 'Continue feeding your starter daily with equal parts flour and water.', False),
    ('pa-03', 'should-correct', "That's unsafe. Adding more acid to neutralize a spill is incorrect.", True),
    ('pa-03', 'should-correct', 'What lab safety instructions are you reviewing?', False),
]


def validate(pairs, url, model):
    results = []
    for pair_id, variant_id, heard, expected in VALIDATION:
        pair = pairs[pair_id]
        variant = next(v for v in pair['variants'] if v['id'] == variant_id)
        verdict = ask(url, model, goal(pair, variant), heard)
        results.append({'pair_id': pair_id, 'variant_id': variant_id, 'transcript': heard,
                        'expected': expected, **verdict, 'correct': verdict['meets'] == expected})
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root', type=Path, help='campaign directory containing per-pair runs')
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--url', default='http://127.0.0.1:9100/v1/chat/completions')
    parser.add_argument('--model', default='qwen3-8b')
    parser.add_argument('--asr', default='http://127.0.0.1:9110')
    parser.add_argument('--runs', default='*', help='glob of run directories under root')
    args = parser.parse_args()
    args.out.mkdir()
    runs = sorted(p.parent for p in args.root.glob(args.runs + '/fixtures.json'))
    pairs = {}
    for run in runs:
        pairs.update({p['id']: p for p in json.loads((run / 'fixtures.json').read_text())})
    record = {'scope': 'development semantic judge over window ASR; not human rating or frozen scoring',
              'judge_model': args.model, 'judge_url': args.url, 'asr': args.asr,
              'judge_is_policy_backbone': args.model == 'qwen3-8b',
              'source_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), 'runs': [str(r) for r in runs]}
    began = time.monotonic()
    record['validation'] = validate(pairs, args.url, args.model)
    record['validation_passed'] = all(v['correct'] for v in record['validation'])
    (args.out / 'judge.json').write_text(json.dumps(record, indent=2) + '\n')
    if not record['validation_passed']:
        raise SystemExit('judge failed known-answer validation; see judge.json')
    rows = []
    with (args.out / 'judgments.jsonl').open('x') as log:
        for run in runs:
            for row in judge_run(run, args.url, args.model, args.asr):
                rows.append(row)
                log.write(json.dumps(row) + '\n')
                log.flush()
                print(row['pair_id'], row['variant_id'], row['verdict'], flush=True)
    counts = {}
    for row in rows:
        counts[row['verdict']] = counts.get(row['verdict'], 0) + 1
    record.update(verdicts=counts, judged_variants=len(rows), duration_s=time.monotonic() - began)
    (args.out / 'judge.json').write_text(json.dumps(record, indent=2) + '\n')
    print(json.dumps(counts))


if __name__ == '__main__':
    main()
